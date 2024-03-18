/*
Copyright the Velero contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/clock"

	internalVolume "github.com/vmware-tanzu/velero/internal/volume"
	velerov1api "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	"github.com/vmware-tanzu/velero/pkg/metrics"
	"github.com/vmware-tanzu/velero/pkg/persistence"
	"github.com/vmware-tanzu/velero/pkg/plugin/clientmgmt"
	"github.com/vmware-tanzu/velero/pkg/restore"
	kubeutil "github.com/vmware-tanzu/velero/pkg/util/kube"
	"github.com/vmware-tanzu/velero/pkg/util/results"
)

const (
	PVPatchMaximumDuration = 10 * time.Minute
)

type restoreFinalizerReconciler struct {
	client.Client
	namespace         string
	logger            logrus.FieldLogger
	newPluginManager  func(logger logrus.FieldLogger) clientmgmt.Manager
	backupStoreGetter persistence.ObjectBackupStoreGetter
	metrics           *metrics.ServerMetrics
	clock             clock.WithTickerAndDelayedExecution
	crClient          client.Client
}

func NewRestoreFinalizerReconciler(
	logger logrus.FieldLogger,
	namespace string,
	client client.Client,
	newPluginManager func(logrus.FieldLogger) clientmgmt.Manager,
	backupStoreGetter persistence.ObjectBackupStoreGetter,
	metrics *metrics.ServerMetrics,
	crClient client.Client,
) *restoreFinalizerReconciler {
	return &restoreFinalizerReconciler{
		Client:            client,
		logger:            logger,
		namespace:         namespace,
		newPluginManager:  newPluginManager,
		backupStoreGetter: backupStoreGetter,
		metrics:           metrics,
		clock:             &clock.RealClock{},
		crClient:          crClient,
	}
}

func (r *restoreFinalizerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&velerov1api.Restore{}).
		Complete(r)
}

// +kubebuilder:rbac:groups=velero.io,resources=restores,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=velero.io,resources=restores/status,verbs=get
func (r *restoreFinalizerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.logger.WithField("restore finalizer", req.String())
	log.Debug("restoreFinalizerReconciler getting restore")

	original := &velerov1api.Restore{}
	if err := r.Get(ctx, req.NamespacedName, original); err != nil {
		if apierrors.IsNotFound(err) {
			log.WithError(err).Error("restore not found")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, errors.Wrapf(err, "error getting restore %s", req.String())
	}
	restore := original.DeepCopy()
	log.Debugf("restore: %s", restore.Name)

	log = r.logger.WithFields(
		logrus.Fields{
			"restore": req.String(),
		},
	)

	switch restore.Status.Phase {
	case velerov1api.RestorePhaseFinalizing, velerov1api.RestorePhaseFinalizingPartiallyFailed:
	default:
		log.Debug("Restore is not awaiting finalization, skipping")
		return ctrl.Result{}, nil
	}

	info, err := fetchBackupInfoInternal(r.Client, r.namespace, restore.Spec.BackupName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.WithError(err).Error("not found backup, skip")
			if err2 := r.finishProcessing(velerov1api.RestorePhasePartiallyFailed, restore, original); err2 != nil {
				log.WithError(err2).Error("error updating restore's final status")
				return ctrl.Result{}, errors.Wrap(err2, "error updating restore's final status")
			}
			return ctrl.Result{}, nil
		}
		log.WithError(err).Error("error getting backup info")
		return ctrl.Result{}, errors.Wrap(err, "error getting backup info")
	}

	pluginManager := r.newPluginManager(r.logger)
	defer pluginManager.CleanupClients()
	backupStore, err := r.backupStoreGetter.Get(info.location, pluginManager, r.logger)
	if err != nil {
		log.WithError(err).Error("error getting backup store")
		return ctrl.Result{}, errors.Wrap(err, "error getting backup store")
	}

	volumeInfo, err := backupStore.GetBackupVolumeInfos(restore.Spec.BackupName)
	if err != nil {
		log.WithError(err).Errorf("error getting volumeInfo for backup %s", restore.Spec.BackupName)
		return ctrl.Result{}, errors.Wrap(err, "error getting volumeInfo")
	}

	restoredResourceList, err := backupStore.GetRestoredResourceList(restore.Name)
	if err != nil {
		log.WithError(err).Error("error getting restoredResourceList")
		return ctrl.Result{}, errors.Wrap(err, "error getting restoredResourceList")
	}

	restoredPVCList := getRestoredPVCFromRestoredResourceList(restoredResourceList)

	finalizerCtx := &finalizerContext{
		logger:          log,
		restore:         restore,
		crClient:        r.crClient,
		volumeInfo:      volumeInfo,
		restoredPVCList: restoredPVCList,
	}
	warnings, errs := finalizerCtx.execute()

	warningCnt := len(warnings.Velero) + len(warnings.Cluster)
	for _, w := range warnings.Namespaces {
		warningCnt += len(w)
	}
	errCnt := len(errs.Velero) + len(errs.Cluster)
	for _, e := range errs.Namespaces {
		errCnt += len(e)
	}
	restore.Status.Warnings += warningCnt
	restore.Status.Errors += errCnt

	if !errs.IsEmpty() {
		restore.Status.Phase = velerov1api.RestorePhaseFinalizingPartiallyFailed
	}

	if warningCnt > 0 || errCnt > 0 {
		err := r.updateResults(backupStore, restore, &warnings, &errs)
		if err != nil {
			log.WithError(err).Error("error updating results")
			return ctrl.Result{}, errors.Wrap(err, "error updating results")
		}
	}

	finalPhase := velerov1api.RestorePhaseCompleted
	if restore.Status.Phase == velerov1api.RestorePhaseFinalizingPartiallyFailed {
		finalPhase = velerov1api.RestorePhasePartiallyFailed
	}
	log.Infof("Marking restore %s", finalPhase)

	if err := r.finishProcessing(finalPhase, restore, original); err != nil {
		log.WithError(err).Error("error updating restore's final status")
		return ctrl.Result{}, errors.Wrap(err, "error updating restore's final status")
	}

	return ctrl.Result{}, nil
}

func (r *restoreFinalizerReconciler) updateResults(backupStore persistence.BackupStore, restore *velerov1api.Restore, newWarnings *results.Result, newErrs *results.Result) error {
	originResults, err := backupStore.GetRestoreResults(restore.Name)
	if err != nil {
		return errors.Wrap(err, "error getting restore results")
	}
	warnings := originResults["warnings"]
	errs := originResults["errors"]
	warnings.Merge(newWarnings)
	errs.Merge(newErrs)

	m := map[string]results.Result{
		"warnings": warnings,
		"errors":   errs,
	}
	if err := putResults(restore, m, backupStore); err != nil {
		return errors.Wrap(err, "error putting restore results")
	}

	return nil
}

func (r *restoreFinalizerReconciler) finishProcessing(restorePhase velerov1api.RestorePhase, restore *velerov1api.Restore, original *velerov1api.Restore) error {
	if restorePhase == velerov1api.RestorePhasePartiallyFailed {
		restore.Status.Phase = velerov1api.RestorePhasePartiallyFailed
		r.metrics.RegisterRestorePartialFailure(restore.Spec.ScheduleName)
	} else {
		restore.Status.Phase = velerov1api.RestorePhaseCompleted
		r.metrics.RegisterRestoreSuccess(restore.Spec.ScheduleName)
	}
	restore.Status.CompletionTimestamp = &metav1.Time{Time: r.clock.Now()}

	return kubeutil.PatchResource(original, restore, r.Client)
}

// finalizerContext includes all the dependencies required by finalization tasks and
// a function execute() to orderly implement task logic.
type finalizerContext struct {
	logger          logrus.FieldLogger
	restore         *velerov1api.Restore
	crClient        client.Client
	volumeInfo      []*internalVolume.VolumeInfo
	restoredPVCList map[string]struct{}
}

func (ctx *finalizerContext) execute() (results.Result, results.Result) { //nolint:unparam //temporarily ignore the lint report: result 0 is always nil (unparam)
	warnings, errs := results.Result{}, results.Result{}

	// implement finalization tasks
	pdpErrs := ctx.patchDynamicPVWithVolumeInfo()
	errs.Merge(&pdpErrs)

	return warnings, errs
}

// patchDynamicPV patches newly dynamically provisioned PV using volume info
// in order to restore custom settings that would otherwise be lost during dynamic PV recreation.
func (ctx *finalizerContext) patchDynamicPVWithVolumeInfo() (errs results.Result) {
	ctx.logger.Info("patching newly dynamically provisioned PV starts")

	var pvWaitGroup sync.WaitGroup
	var resultLock sync.Mutex
	maxConcurrency := 3
	semaphore := make(chan struct{}, maxConcurrency)

	for _, volumeItem := range ctx.volumeInfo {
		if (volumeItem.BackupMethod == internalVolume.PodVolumeBackup || volumeItem.BackupMethod == internalVolume.CSISnapshot) && volumeItem.PVInfo != nil {
			// Determine restored PVC namespace
			restoredNamespace := volumeItem.PVCNamespace
			if remapped, ok := ctx.restore.Spec.NamespaceMapping[restoredNamespace]; ok {
				restoredNamespace = remapped
			}

			// Check if PVC was restored in previous phase
			pvcKey := fmt.Sprintf("%s/%s", restoredNamespace, volumeItem.PVCName)
			if _, restored := ctx.restoredPVCList[pvcKey]; !restored {
				continue
			}

			pvWaitGroup.Add(1)
			go func(volInfo internalVolume.VolumeInfo, restoredNamespace string) {
				defer pvWaitGroup.Done()

				semaphore <- struct{}{}

				log := ctx.logger.WithField("PVC", volInfo.PVCName).WithField("PVCNamespace", restoredNamespace)
				log.Debug("patching dynamic PV is in progress")

				err := wait.PollUntilContextTimeout(context.Background(), 10*time.Second, PVPatchMaximumDuration, true, func(context.Context) (bool, error) {
					// wait for PVC to be bound
					pvc := &v1.PersistentVolumeClaim{}
					err := ctx.crClient.Get(context.Background(), client.ObjectKey{Name: volInfo.PVCName, Namespace: restoredNamespace}, pvc)
					if apierrors.IsNotFound(err) {
						log.Debug("error not finding PVC")
						return false, nil
					}
					if err != nil {
						return false, err
					}

					if pvc.Status.Phase != v1.ClaimBound || pvc.Spec.VolumeName == "" {
						log.Debugf("PVC: %s not ready", pvc.Name)
						return false, nil
					}

					// wait for PV to be bound
					pvName := pvc.Spec.VolumeName
					pv := &v1.PersistentVolume{}
					err = ctx.crClient.Get(context.Background(), client.ObjectKey{Name: pvName}, pv)
					if apierrors.IsNotFound(err) {
						log.Debugf("error not finding PV: %s", pvName)
						return false, nil
					}
					if err != nil {
						return false, err
					}

					if pv.Spec.ClaimRef == nil || pv.Status.Phase != v1.VolumeBound {
						log.Debugf("PV: %s not ready", pvName)
						return false, nil
					}

					// validate PV
					if pv.Spec.ClaimRef.Name != pvc.Name || pv.Spec.ClaimRef.Namespace != restoredNamespace {
						return false, fmt.Errorf("PV was bound by unexpected PVC, unexpected PVC: %s/%s, expected PVC: %s/%s",
							pv.Spec.ClaimRef.Namespace, pv.Spec.ClaimRef.Name, restoredNamespace, pvc.Name)
					}

					// patch PV's reclaim policy and label using the corresponding data stored in volume info
					if needPatch(pv, volInfo.PVInfo) {
						updatedPV := pv.DeepCopy()
						updatedPV.Labels = volInfo.PVInfo.Labels
						updatedPV.Spec.PersistentVolumeReclaimPolicy = v1.PersistentVolumeReclaimPolicy(volInfo.PVInfo.ReclaimPolicy)
						if err := kubeutil.PatchResource(pv, updatedPV, ctx.crClient); err != nil {
							return false, err
						}
						log.Infof("newly dynamically provisioned PV:%s has been patched using volume info", pvName)
					}

					return true, nil
				})

				if err != nil {
					err = fmt.Errorf("fail to patch dynamic PV, err: %s, PVC: %s, PV: %s", err, volInfo.PVCName, volInfo.PVName)
					ctx.logger.WithError(errors.WithStack((err))).Error("err patching dynamic PV using volume info")
					resultLock.Lock()
					defer resultLock.Unlock()
					errs.Add(restoredNamespace, err)
				}

				<-semaphore
			}(*volumeItem, restoredNamespace)
		}
	}

	pvWaitGroup.Wait()
	ctx.logger.Info("patching newly dynamically provisioned PV ends")

	return errs
}

func getRestoredPVCFromRestoredResourceList(restoredResourceList map[string][]string) map[string]struct{} {
	pvcKey := "v1/PersistentVolumeClaim"
	pvcList := make(map[string]struct{})

	for _, pvc := range restoredResourceList[pvcKey] {
		// the format of pvc string in restoredResourceList is like: "namespace/pvcName(status)"
		// extract the substring before "(created)" if the status in rightmost Parenthesis is "created"
		r := regexp.MustCompile(`\(([^)]+)\)`)
		matches := r.FindAllStringSubmatch(pvc, -1)
		if len(matches) > 0 && matches[len(matches)-1][1] == restore.ItemRestoreResultCreated {
			pvcList[pvc[:len(pvc)-len("(created)")]] = struct{}{}
		}
	}

	return pvcList
}

func needPatch(newPV *v1.PersistentVolume, pvInfo *internalVolume.PVInfo) bool {
	if newPV.Spec.PersistentVolumeReclaimPolicy != v1.PersistentVolumeReclaimPolicy(pvInfo.ReclaimPolicy) {
		return true
	}

	newPVLabels, pvLabels := newPV.Labels, pvInfo.Labels
	for k, v := range pvLabels {
		if _, ok := newPVLabels[k]; !ok {
			return true
		}
		if newPVLabels[k] != v {
			return true
		}
	}

	return false
}
