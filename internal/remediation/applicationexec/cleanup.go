package applicationexec

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

type volumeReceipt struct {
	Namespace string    `json:"namespace"`
	ClaimName string    `json:"claimName"`
	ClaimUID  types.UID `json:"claimUID"`
	Name      string    `json:"name"`
	UID       types.UID `json:"uid"`
	Released  bool      `json:"released,omitempty"`
}

func (b *Backend) recordVolumes(ctx context.Context, r *run, pod *corev1.Pod) error {
	for _, mount := range pod.Spec.Volumes {
		if mount.PersistentVolumeClaim == nil {
			continue
		}
		claim := &corev1.PersistentVolumeClaim{}
		if err := b.targetReader.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: mount.PersistentVolumeClaim.ClaimName}, claim); err != nil {
			return safeError(contract.ErrUnavailable, err)
		}
		owned := false
		for _, entry := range r.state.Effects {
			if entry.Kind == "PersistentVolumeClaim" && entry.Namespace == claim.Namespace && entry.Name == claim.Name &&
				entry.UID == claim.UID && entry.UID != "" && !entry.Deleted {
				owned = true
			}
		}
		if !owned || claim.Spec.VolumeName == "" || claim.Status.Phase != corev1.ClaimBound || !claim.DeletionTimestamp.IsZero() {
			return contract.ErrConflict
		}
		volume := &corev1.PersistentVolume{}
		if err := b.targetReader.Get(ctx, client.ObjectKey{Name: claim.Spec.VolumeName}, volume); err != nil {
			return safeError(contract.ErrUnavailable, err)
		}
		if volume.UID == "" || volume.Spec.ClaimRef == nil || volume.Spec.ClaimRef.UID != claim.UID ||
			volume.Spec.ClaimRef.Name != claim.Name || volume.Spec.ClaimRef.Namespace != claim.Namespace ||
			volume.Spec.StorageClassName != b.config.StorageClass || !volume.DeletionTimestamp.IsZero() {
			return ErrIsolation
		}
		receipt := volumeReceipt{Namespace: claim.Namespace, ClaimName: claim.Name, ClaimUID: claim.UID, Name: volume.Name, UID: volume.UID}
		if err := b.rememberVolume(ctx, r, receipt); err != nil {
			return err
		}
		if volume.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimDelete {
			return ErrIsolation
		}
	}
	return nil
}

func (b *Backend) rememberVolume(ctx context.Context, r *run, receipt volumeReceipt) error {
	for _, previous := range r.state.Volumes {
		if previous.ClaimUID == receipt.ClaimUID || previous.Name == receipt.Name {
			if previous != receipt {
				return contract.ErrConflict
			}
			return nil
		}
	}
	r.state.Volumes = append(r.state.Volumes, receipt)
	return b.save(ctx, r)
}

func (b *Backend) capturePendingVolumes(ctx context.Context, r *run) error {
	claims := make(map[types.UID]effect)
	for _, entry := range r.state.Effects {
		if entry.Kind == "PersistentVolumeClaim" && entry.UID != "" {
			claims[entry.UID] = entry
		}
	}
	if len(claims) == 0 {
		return nil
	}
	volumes := &corev1.PersistentVolumeList{}
	if err := b.targetReader.List(ctx, volumes, client.Limit(64)); err != nil {
		return safeError(contract.ErrUnavailable, err)
	}
	if volumes.Continue != "" {
		return ErrIsolation
	}
	for _, volume := range volumes.Items {
		if volume.Spec.ClaimRef == nil {
			continue
		}
		claim, owned := claims[volume.Spec.ClaimRef.UID]
		if !owned {
			continue
		}
		if volume.UID == "" || volume.Spec.ClaimRef.Name != claim.Name || volume.Spec.ClaimRef.Namespace != claim.Namespace {
			return contract.ErrConflict
		}
		receipt := volumeReceipt{Namespace: claim.Namespace, ClaimName: claim.Name, ClaimUID: claim.UID, Name: volume.Name, UID: volume.UID}
		if err := b.rememberVolume(ctx, r, receipt); err != nil {
			return err
		}
		if volume.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimDelete {
			return ErrIsolation
		}
	}
	return nil
}

func (b *Backend) Cancel(ctx context.Context, receipt contract.ExecutionReceipt) error {
	if b == nil || !b.config.Enabled {
		return contract.ErrDisabled
	}
	ctx, cancel := context.WithTimeout(ctx, b.config.CleanupTimeout)
	defer cancel()
	if err := b.lockEffects(ctx); err != nil {
		return err
	}
	defer func() { <-b.effectGate }()
	r, err := b.boundRun(ctx, receipt)
	if err != nil {
		return err
	}
	if err := b.checkTask(ctx, r, true); err != nil {
		return err
	}
	if r.state.Settled {
		return b.finishTasks(ctx, r)
	}
	if !r.state.Cancelled {
		r.state.Cancelled = true
		if err := b.save(ctx, r); err != nil {
			return err
		}
	}
	for {
		done, err := b.cleanup(ctx, r)
		if done || err != nil && !errors.Is(err, errReceiptsPending) {
			return err
		}
		if pauseErr := b.pause(ctx); pauseErr != nil {
			return errors.Join(err, pauseErr)
		}
		if err := b.fresh(ctx, r); err != nil {
			return err
		}
	}
}

func (b *Backend) cleanupCluster(ctx context.Context, r *run) error {
	for _, cluster := range []struct {
		reader client.Reader
		uid    string
	}{{b.controlReader, r.state.ControlUID}, {b.targetReader, r.state.TargetUID}} {
		ns := &corev1.Namespace{}
		if err := cluster.reader.Get(ctx, client.ObjectKey{Name: metav1.NamespaceSystem}, ns); err != nil {
			return safeError(contract.ErrUnavailable, err)
		}
		if string(ns.UID) != cluster.uid || !ns.DeletionTimestamp.IsZero() {
			return contract.ErrConflict
		}
	}
	return nil
}

func (b *Backend) cleanupNamespace(ctx context.Context, r *run, name string) (bool, error) {
	if name == metav1.NamespaceSystem {
		return true, nil
	}
	var uid types.UID
	for _, entry := range r.state.Effects {
		if entry.Kind == namespaceKind && entry.Name == name {
			uid = entry.UID
		}
	}
	if uid == "" {
		return false, ErrUncertain
	}
	ns := &corev1.Namespace{}
	err := b.targetReader.Get(ctx, client.ObjectKey{Name: name}, ns)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, safeError(contract.ErrUnavailable, err)
	}
	if ns.UID != uid {
		return false, contract.ErrConflict
	}
	return true, nil
}

func (b *Backend) deleteEffect(ctx context.Context, r *run, index int) (bool, error) {
	entry := r.state.Effects[index]
	if entry.Deleted {
		return true, nil
	}
	if entry.Rejected {
		r.state.Effects[index].Deleted = true
		return true, b.save(ctx, r)
	}
	if entry.UID == "" {
		return false, ErrUncertain
	}
	if entry.Namespace != "" {
		if _, err := b.cleanupNamespace(ctx, r, entry.Namespace); err != nil {
			return false, err
		}
	}
	object, err := b.objectFor(entry)
	if err != nil {
		return false, err
	}
	err = b.targetReader.Get(ctx, client.ObjectKey{Namespace: entry.Namespace, Name: entry.Name}, object)
	if apierrors.IsNotFound(err) {
		r.state.Effects[index].Deleted = true
		return true, b.save(ctx, r)
	}
	if err != nil {
		return false, safeError(contract.ErrUnavailable, err)
	}
	if object.GetUID() != entry.UID {
		return false, contract.ErrConflict
	}
	if object.GetDeletionTimestamp().IsZero() {
		if err := b.target.Delete(ctx, object, client.Preconditions{UID: &entry.UID},
			client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil && !apierrors.IsNotFound(err) {
			return false, safeError(contract.ErrUnavailable, err)
		}
	}
	return false, nil
}

func (b *Backend) settlePods(ctx context.Context, r *run) (bool, error) {
	allGone := true
	owners := make(map[types.UID]effect)
	namespaces := make(map[string]bool)
	for _, entry := range r.state.Effects {
		if (entry.Kind == jobKind || entry.Kind == replicaSetKind && entry.GeneratedBy != "") && entry.UID != "" {
			owners[entry.UID] = entry
			namespaces[entry.Namespace] = true
		}
	}
	for namespace := range namespaces {
		present, err := b.cleanupNamespace(ctx, r, namespace)
		if err != nil {
			return false, err
		}
		if !present {
			continue
		}
		pods := &corev1.PodList{}
		if err := b.targetReader.List(ctx, pods, client.InNamespace(namespace), client.Limit(64)); err != nil {
			return false, safeError(contract.ErrUnavailable, err)
		}
		if pods.Continue != "" {
			return false, ErrIsolation
		}
		for i := range pods.Items {
			allGone = false
			if err := b.settlePod(ctx, r, &pods.Items[i], owners); err != nil {
				return false, err
			}
		}
	}
	return allGone, nil
}

func (b *Backend) settlePod(ctx context.Context, r *run, pod *corev1.Pod, owners map[types.UID]effect) error {
	namespace := pod.Namespace
	var owner effect
	for _, reference := range pod.OwnerReferences {
		if known, ok := owners[reference.UID]; ok && known.Namespace == namespace {
			owner = known
		}
	}
	if owner.UID == "" {
		// No candidate has Pod-create authority. An unexplained Pod in
		// a run-owned namespace is a boundary failure, not ours to adopt.
		return contract.ErrConflict
	}
	if owner.Kind == jobKind && !ownedByJob(pod, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: owner.Name, UID: owner.UID}}) ||
		owner.Kind == replicaSetKind && !exactAppsOwner(pod, replicaSetKind, owner.Name, owner.UID) {
		return contract.ErrConflict
	}
	if pod.UID == "" {
		return contract.ErrConflict
	}
	key := "cleanup-pod/" + string(pod.UID)
	entry := r.effect(key)
	for index := range r.state.Effects {
		known := &r.state.Effects[index]
		if known.Kind == podKind && known.Namespace == namespace && known.Name == pod.Name {
			entry, key = known, known.Key
		}
	}
	if entry == nil {
		if len(r.state.Effects) >= maxEffects {
			return contract.ErrInvalid
		}
		content, _ := json.Marshal([]string{namespace, pod.Name, string(pod.UID), string(owner.UID)})
		r.state.Effects = append(r.state.Effects, effect{Key: key, APIVersion: "v1", Kind: podKind,
			Namespace: namespace, Name: pod.Name, UID: pod.UID, Digest: pv.Digest(content), Attempted: true})
		if err := b.save(ctx, r); err != nil {
			return err
		}
	} else if entry.UID != pod.UID || entry.Name != pod.Name || entry.Namespace != namespace || entry.Deleted {
		return contract.ErrConflict
	}
	if pod.DeletionTimestamp.IsZero() {
		uid := pod.UID
		if err := b.target.Delete(ctx, pod, client.Preconditions{UID: &uid},
			client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil && !apierrors.IsNotFound(err) {
			return safeError(contract.ErrUnavailable, err)
		}
	}
	return nil
}

func (b *Backend) settleVolumes(ctx context.Context, r *run) (bool, error) {
	allGone := true
	for i := range r.state.Volumes {
		receipt := r.state.Volumes[i]
		if receipt.Released {
			continue
		}
		volume := &corev1.PersistentVolume{}
		err := b.targetReader.Get(ctx, client.ObjectKey{Name: receipt.Name}, volume)
		if apierrors.IsNotFound(err) {
			r.state.Volumes[i].Released = true
			if err := b.save(ctx, r); err != nil {
				return false, err
			}
			continue
		}
		if err != nil {
			return false, safeError(contract.ErrUnavailable, err)
		}
		if volume.UID != receipt.UID || volume.Spec.ClaimRef == nil || volume.Spec.ClaimRef.UID != receipt.ClaimUID ||
			volume.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimDelete {
			return false, contract.ErrConflict
		}
		// The configured provisioner, not a model or this backend, owns PV
		// destruction. Keep the target claim until its exact UID is absent.
		allGone = false
	}
	return allGone, nil
}

func (b *Backend) cleanup(ctx context.Context, r *run) (bool, error) {
	if !r.state.Cancelled && !r.state.Finished && r.state.Failed == "" {
		return false, contract.ErrConflict
	}
	if err := b.cleanupCluster(ctx, r); err != nil {
		return false, err
	}
	if err := b.checkTask(ctx, r, true); err != nil {
		return false, err
	}
	if err := b.cancelRecord(ctx, r); err != nil {
		return false, err
	}
	if err := b.collectStoppingReceipts(ctx, r); err != nil {
		return false, err
	}
	dataGone, dataErr := b.finalizeSandboxData(ctx, r)
	if dataErr != nil && !errors.Is(dataErr, errDataFinalizationBlocked) && !errors.Is(dataErr, contract.ErrConflict) {
		return false, dataErr
	}
	if !dataGone && dataErr == nil {
		return false, nil
	}
	executionGone, executionErr := b.stopExecution(ctx, r)
	grantsGone, grantsErr := b.revokeExecutionGrants(ctx, r)
	if executionErr != nil || grantsErr != nil {
		return false, errors.Join(dataErr, executionErr, grantsErr)
	}
	if !executionGone || !grantsGone {
		return false, nil
	}
	if !dataGone {
		return false, dataErr
	}
	// A PVC can bind while its Pod is still pending. Capture those controller-
	// created PV identities even when no container was ever observed running.
	if err := b.capturePendingVolumes(ctx, r); err != nil {
		return false, err
	}
	resourcesGone, err := b.deleteRemainingResources(ctx, r)
	if err != nil || !resourcesGone {
		return false, err
	}
	namespacesGone, err := b.deleteEffectKind(ctx, r, namespaceKind)
	if err != nil || !namespacesGone {
		return false, err
	}
	volumesGone, err := b.settleVolumes(ctx, r)
	if err != nil || !volumesGone {
		return false, err
	}
	for i := range r.state.Effects {
		if r.state.Effects[i].Key == "target-claim" {
			done, err := b.deleteEffect(ctx, r, i)
			if err != nil || !done {
				return false, err
			}
		}
	}
	if err := b.finishTasks(ctx, r); err != nil {
		return false, err
	}
	r.state.Settled = true
	return true, b.save(ctx, r)
}

func (b *Backend) stopExecution(ctx context.Context, r *run) (bool, error) {
	if err := b.captureReplicaCleanup(ctx, r); err != nil {
		return false, err
	}
	jobsGone, err := b.deleteEffectKind(ctx, r, jobKind)
	if err != nil {
		return false, err
	}
	replicasGone, err := b.stopReplicaControllers(ctx, r)
	if err != nil {
		return false, err
	}
	podsGone, err := b.settlePods(ctx, r)
	return jobsGone && replicasGone && podsGone, err
}

func (b *Backend) revokeExecutionGrants(ctx context.Context, r *run) (bool, error) {
	// Bindings grant authority to names, not immutable service-account UIDs.
	// Revoke only recorded grants, even when unrelated data prevents settlement.
	gone := true
	var failures []error
	for _, kind := range []string{"ClusterRoleBinding", "RoleBinding", clusterRoleKind, roleKind} {
		deleted, err := b.deleteEffectKind(ctx, r, kind)
		gone = gone && deleted
		if err != nil {
			failures = append(failures, err)
		}
	}
	return gone, errors.Join(failures...)
}

func (b *Backend) deleteEffectKind(ctx context.Context, r *run, kind string) (bool, error) {
	gone := true
	for index := range r.state.Effects {
		if r.state.Effects[index].Kind != kind {
			continue
		}
		done, err := b.deleteEffect(ctx, r, index)
		if err != nil {
			return false, err
		}
		gone = gone && done
	}
	return gone, nil
}

func (b *Backend) deleteRemainingResources(ctx context.Context, r *run) (bool, error) {
	gone := true
	for index, entry := range slices.Backward(r.state.Effects) {
		if entry.Kind == jobKind || entry.Kind == namespaceKind || entry.Kind == clusterRoleKind ||
			entry.Kind == "ClusterRoleBinding" || entry.Key == "target-claim" {
			continue
		}
		done, err := b.deleteEffect(ctx, r, index)
		if err != nil {
			return false, err
		}
		gone = gone && done
	}
	return gone, nil
}

// ReconcileCleanup is suitable for a leader-elected periodic sweep in addition
// to Task deletion reconciliation. It covers parent removal, forced Task
// removal, and an expired process even when no caller is currently resuming it.
func (b *Backend) ReconcileCleanup(ctx context.Context) error {
	if b == nil || !b.config.Enabled {
		return contract.ErrDisabled
	}
	runs, err := activeRuns(ctx, b.controlReader, b.config.Namespace)
	if err != nil {
		return err
	}
	var failures []error
	for _, r := range runs {
		if r.state.Task.UID == "" {
			if err := b.sweepRootReservation(ctx, r); err != nil {
				failures = append(failures, err)
			}
			continue
		}
		if r.state.Finished && !r.state.Cancelled {
			if err := b.lockEffects(ctx); err != nil {
				return err
			}
			current, err := b.boundRun(ctx, r.receipt())
			if err == nil {
				_, err = b.cleanup(ctx, current)
			}
			<-b.effectGate
			if err != nil {
				failures = append(failures, err)
			}
			continue
		}
		parentErr := b.parent(ctx, r)
		taskErr := b.checkTask(ctx, r, false)
		stop := r.state.Cancelled || r.state.Failed != "" || anyTaskRejected(r) || !r.object.DeletionTimestamp.IsZero() ||
			!time.Now().Before(r.state.Deadline) || errors.Is(parentErr, contract.ErrConflict) ||
			apierrors.IsNotFound(parentErr) || apierrors.IsNotFound(taskErr)
		if stop {
			if err := b.Cancel(ctx, r.receipt()); err != nil {
				failures = append(failures, err)
			}
		} else if parentErr != nil || taskErr != nil && !r.state.Finished {
			failures = append(failures, errors.Join(parentErr, taskErr))
		}
	}
	return errors.Join(failures...)
}

type cleanupRunnable struct{ backend *Backend }

func (r cleanupRunnable) NeedLeaderElection() bool { return true }

func (r cleanupRunnable) Start(ctx context.Context) error {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		if err := r.backend.ReconcileCleanup(ctx); err != nil && ctx.Err() == nil {
			// Detailed API errors may contain submitted objects. The durable
			// ledger retains safe failure classes and exact cleanup identities.
			log.FromContext(ctx).Error(contract.ErrUnavailable, "application cleanup remains unsettled")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (b *Backend) CleanupRunnable() manager.Runnable { return cleanupRunnable{backend: b} }
