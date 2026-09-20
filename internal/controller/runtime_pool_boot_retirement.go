package controller

import (
	"context"
	"errors"
	"fmt"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func loadRuntimePoolBootRetirement(ctx context.Context, effects store.ExternalEffectStore, w runtimePoolBootWitness) (bool, error) {
	var proof agentRuntimeBootRetirement
	effect, err := readAgentRuntimeRecoveryEffect(ctx, effects, w.identity(runtimePoolBootRetirementKind), &proof)
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrNotReady) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	digest, err := runtimePoolBootWitnessDigest(w)
	if err != nil {
		return false, err
	}
	if proof.SchemaVersion != 1 || proof.WitnessDigest != digest || effect.RequestDigest != digest {
		return false, fmt.Errorf("%w: native RuntimePool retirement witness digest changed", store.ErrConflict)
	}
	switch proof.Kind {
	case runtimePoolBootDrainedProof:
		if proof.ContainerTermination == nil && proof.DrainedStatus != nil &&
			harnessv2.CompareFence(w.Fence, proof.DrainedStatus.Fence, false) == harnessv2.FenceMatch && upgradeDrainSupervisorIsQuiescent(*proof.DrainedStatus) {
			return true, nil
		}
	case runtimePoolBootTerminatedProof:
		if proof.DrainedStatus == nil && validWitnessContainerTermination(w.physicalWitness(), proof.ContainerTermination) && nativeRuntimePoolProvider(w.Provider) {
			return true, nil
		}
	}
	return false, fmt.Errorf("%w: native RuntimePool retirement lacks exact positive evidence", store.ErrConflict)
}

func (r *RuntimePoolReconciler) persistRuntimePoolBootRetirement(ctx context.Context, w runtimePoolBootWitness, proof agentRuntimeBootRetirement, fence store.ControllerEpochFence) error {
	if w.Fence.ControllerEpoch > uint64(fence.Epoch) {
		return fmt.Errorf("%w: native RuntimePool boot is newer than the retirement owner", store.ErrConflict)
	}
	if retired, err := loadRuntimePoolBootRetirement(ctx, r.ControlStore, w); err != nil || retired {
		return err
	}
	digest, err := runtimePoolBootWitnessDigest(w)
	if err != nil {
		return err
	}
	proof.SchemaVersion, proof.WitnessDigest = 1, digest
	return persistAgentRuntimeRecoveryEffect(ctx, r.ControlStore, fence, w.identity(runtimePoolBootRetirementKind), digest, proof)
}

// verifiedNativeRuntimePoolRetirement is a READ-ONLY gate for the dispatcher.
// Missing evidence returns false, never an absence-based cleanup receipt. The
// caller must retain its existing frozen-turn checks and epoch-guarded writes.
// It works after the pool/Pod is gone and never consults a replacement instance.
func verifiedNativeRuntimePoolRetirement(ctx context.Context, effects store.ExternalEffectStore, task *corev1alpha1.Task, taskUID types.UID) (bool, error) {
	if task == nil || task.Status.Execution == nil || task.Status.Execution.RuntimePoolUID == "" ||
		task.Status.Execution.AgentRuntimeUID != "" || task.Spec.Execution != nil && task.Spec.Execution.Workspace != nil {
		return false, nil
	}
	e := task.Status.Execution
	identity := agentRuntimeRecoveryIdentity(runtimePoolBootWitnessKind, types.UID(e.RuntimePoolUID), task.Namespace, e.RuntimeInstanceID)
	w, err := loadRuntimePoolBootWitness(ctx, effects, identity)
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrNotReady) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := runtimePoolTaskWitnessMatches(task, taskUID, w); err != nil {
		return false, err
	}
	return loadRuntimePoolBootRetirement(ctx, effects, w)
}

func (r *RuntimePoolReconciler) recordRetiredRuntimePoolTasks(ctx context.Context, w runtimePoolBootWitness, fence store.ControllerEpochFence) error {
	var tasks corev1alpha1.TaskList
	if err := r.APIReader.List(ctx, &tasks, client.InNamespace(w.PoolNamespace)); err != nil {
		return err
	}
	for i := range tasks.Items {
		task := &tasks.Items[i]
		e := task.Status.Execution
		if e == nil || e.RuntimePoolUID != string(w.PoolUID) || e.RuntimeInstanceID != string(w.Fence.RuntimeInstanceID) || e.RuntimeSessionUID == "" {
			continue
		}
		uid := acpTaskControlUID(task)
		// Keep each epoch interlock short. A namespace's historical Task list
		// must not turn a single mutation guard into an unbounded cleanup loop.
		if err := agentRuntimeRecoveryGuard(ctx, r.ControlStore, fence, func(writeCtx context.Context) error {
			retired, err := loadRuntimePoolBootRetirement(writeCtx, r.ControlStore, w)
			if err != nil || !retired {
				return errors.Join(fmt.Errorf("%w: native RuntimePool boot is not retired", store.ErrNotReady), err)
			}
			latest := &corev1alpha1.Task{}
			if err := r.APIReader.Get(writeCtx, client.ObjectKeyFromObject(task), latest); err != nil {
				return err
			}
			if err := runtimePoolTaskWitnessMatches(latest, uid, w); err != nil {
				return err
			}
			return persistTaskScopedRuntimeSessionCleanupReceipt(writeCtx, r.Client, task, uid, e.RuntimeInstanceID, e.RuntimeSessionUID, e.RuntimeSessionGeneration)
		}); err != nil {
			return err
		}
	}
	return nil
}

// recordNativeRuntimePoolDrain adds durable boot evidence only for enrolled
// native instances. The legacy/provider authenticated receipt path is unchanged.
func (r *RuntimePoolReconciler) recordNativeRuntimePoolDrain(ctx context.Context, pool *corev1alpha1.RuntimePool, active *corev1alpha1.RuntimePoolActiveInstanceStatus, status harnessv2.StatusResponse) error {
	if pool.Spec.ExecutionWorkspace != nil {
		return r.recordDrainedRuntimePoolTaskCleanup(ctx, pool, active, status)
	}
	if active == nil {
		return fmt.Errorf("%w: native RuntimePool drain lacks active identity", store.ErrConflict)
	}
	pod := &corev1.Pod{}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: active.PodNamespace, Name: active.PodName}, pod); err != nil {
		return err
	}
	if string(pod.UID) != active.PodUID {
		return fmt.Errorf("%w: native drain Pod UID changed", store.ErrConflict)
	}
	if pod.Annotations[runtimePoolBootEnrollmentAnnotation] == "" {
		return r.recordDrainedRuntimePoolTaskCleanup(ctx, pool, active, status)
	}
	fence, err := r.runtimePoolRetirementFence(ctx)
	if err != nil {
		return err
	}
	w, err := loadRuntimePoolBootWitness(ctx, r.ControlStore, agentRuntimeRecoveryIdentity(runtimePoolBootWitnessKind, pool.UID, pool.Namespace, active.RuntimeInstanceID))
	if err != nil {
		return err
	}
	if !runtimePoolRetirementFenceMatches(pool, active, status) || harnessv2.CompareFence(w.Fence, status.Fence, false) != harnessv2.FenceMatch ||
		!runtimePoolWitnessRunning(w, pod) || pod.Status.PodIP != w.PodAddress {
		return fmt.Errorf("%w: authenticated drain does not match the enrolled native runtime", store.ErrConflict)
	}
	retired, err := loadRuntimePoolBootRetirement(ctx, r.ControlStore, w)
	if err != nil {
		return err
	}
	if !retired {
		if err := r.requireRuntimePoolBootRetention(ctx, w); err != nil {
			return err
		}
	}
	// Retain only structured proof, not status reason/message payloads.
	safeStatus := harnessv2.StatusResponse{Protocol: status.Protocol, Fence: status.Fence, Lifecycle: status.Lifecycle,
		Drain: harnessv2.DrainStatus{Requested: true, AcceptingNewSessions: false}}
	if err := r.persistRuntimePoolBootRetirement(ctx, w, agentRuntimeBootRetirement{Kind: runtimePoolBootDrainedProof, DrainedStatus: &safeStatus}, fence); err != nil {
		return err
	}
	if err := r.recordRetiredRuntimePoolTasks(ctx, w, fence); err != nil {
		return err
	}
	witnesses, err := r.runtimePoolBootRecords(ctx, pool, runtimePoolBootWitnessKind)
	if err != nil {
		return err
	}
	return r.releaseRuntimePoolBootPod(ctx, w, witnesses, fence)
}

func (r *RuntimePoolReconciler) reconcileNativeRuntimePoolRetirement(ctx context.Context, pool *corev1alpha1.RuntimePool) error {
	if pool.Spec.ExecutionWorkspace != nil {
		return nil
	}
	preparations, err := r.runtimePoolBootRecords(ctx, pool, runtimePoolBootPreparationKind)
	if err != nil {
		return err
	}
	witnesses, err := r.runtimePoolBootRecords(ctx, pool, runtimePoolBootWitnessKind)
	if err != nil {
		return err
	}
	if len(preparations) == 0 && len(witnesses) == 0 {
		return r.rejectUnwitnessedRuntimePoolRetention(ctx, pool, nil)
	}
	fence, err := r.runtimePoolRetirementFence(ctx)
	if err != nil {
		return err
	}
	if err := r.resumeRuntimePoolBootPreparations(ctx, pool, preparations, fence); err != nil {
		return err
	}
	witnesses, err = r.runtimePoolBootRecords(ctx, pool, runtimePoolBootWitnessKind)
	if err != nil {
		return err
	}
	for _, w := range witnesses {
		retired, err := loadRuntimePoolBootRetirement(ctx, r.ControlStore, w)
		if err != nil {
			return err
		}
		if !retired {
			pod := &corev1.Pod{}
			if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: w.PodNamespace, Name: w.PodName}, pod); err != nil {
				return fmt.Errorf("%w: original native runtime Pod unavailable without retirement proof: %w", store.ErrNotReady, err)
			}
			if err := r.requireRuntimePoolBootRetention(ctx, w); err != nil {
				return err
			}
			terminal, err := runtimePoolWitnessPod(w, pod)
			if err != nil {
				return err
			}
			if terminal == nil {
				continue
			}
			// Kubernetes termination Message/Reason may contain application text.
			safeTerminal := &corev1.ContainerStateTerminated{ContainerID: terminal.ContainerID, StartedAt: terminal.StartedAt, FinishedAt: terminal.FinishedAt}
			if err := r.persistRuntimePoolBootRetirement(ctx, w, agentRuntimeBootRetirement{Kind: runtimePoolBootTerminatedProof, ContainerTermination: safeTerminal}, fence); err != nil {
				return err
			}
		}
		if err := r.recordRetiredRuntimePoolTasks(ctx, w, fence); err != nil {
			return err
		}
		if err := r.releaseRuntimePoolBootPod(ctx, w, witnesses, fence); err != nil {
			return err
		}
	}
	return r.rejectUnwitnessedRuntimePoolRetention(ctx, pool, preparations)
}

func (r *RuntimePoolReconciler) rejectUnwitnessedRuntimePoolRetention(ctx context.Context, pool *corev1alpha1.RuntimePool, preparations []runtimePoolBootWitness) error {
	cfg, err := r.runtimePoolConfigForDeletion(pool)
	if err != nil {
		return err
	}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	var pods corev1.PodList
	if err := reader.List(ctx, &pods, client.InNamespace(cfg.namespace), client.MatchingLabels{runtimePoolUIDLabel: string(pool.UID)}); err != nil {
		return err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !controllerutil.ContainsFinalizer(pod, runtimePoolBootPodFinalizer) {
			continue
		}
		found := false
		for _, w := range preparations {
			digest, err := runtimePoolBootWitnessDigest(w)
			if err != nil {
				return err
			}
			if pod.UID == w.PodUID && pod.Namespace == w.PodNamespace && pod.Name == w.PodName && pod.Annotations[runtimePoolBootRetentionAnnotation] == digest {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%w: retained native runtime Pod has no exact durable preparation", store.ErrConflict)
		}
	}
	return nil
}

func (r *RuntimePoolReconciler) releaseRuntimePoolBootPod(ctx context.Context, w runtimePoolBootWitness, witnesses []runtimePoolBootWitness, fence store.ControllerEpochFence) error {
	return agentRuntimeRecoveryGuard(ctx, r.ControlStore, fence, func(writeCtx context.Context) error {
		if retired, err := loadRuntimePoolBootRetirement(writeCtx, r.ControlStore, w); err != nil || !retired {
			return errors.Join(fmt.Errorf("%w: native Pod release requires exact boot retirement", store.ErrNotReady), err)
		}
		pod := &corev1.Pod{}
		err := r.APIReader.Get(writeCtx, client.ObjectKey{Namespace: w.PodNamespace, Name: w.PodName}, pod)
		if apierrors.IsNotFound(err) {
			return nil
		} // Positive proof was verified above.
		if err != nil {
			return err
		}
		if pod.UID != w.PodUID {
			return fmt.Errorf("%w: retired native Pod UID changed", store.ErrConflict)
		}
		owner := pod.Annotations[runtimePoolBootRetentionAnnotation]
		if !controllerutil.ContainsFinalizer(pod, runtimePoolBootPodFinalizer) {
			if owner != "" {
				return fmt.Errorf("%w: native Pod lost its retention barrier", store.ErrConflict)
			}
			return nil
		}
		digest, err := runtimePoolBootWitnessDigest(w)
		if err != nil {
			return err
		}
		if owner != digest {
			// This historical boot already released retention. A later committed
			// boot may own the same Pod; only its own replay may release it.
			for _, other := range witnesses {
				if other.PodUID != pod.UID || other.PodNamespace != pod.Namespace || other.PodName != pod.Name {
					continue
				}
				otherDigest, err := runtimePoolBootWitnessDigest(other)
				if err != nil {
					return err
				}
				if owner == otherDigest {
					return nil
				}
			}
			return fmt.Errorf("%w: native Pod retention identity changed", store.ErrConflict)
		}
		if _, err := runtimePoolWitnessPod(w, pod); err != nil {
			return err
		}
		for _, other := range witnesses {
			if other.PodUID != w.PodUID || other.PodNamespace != w.PodNamespace {
				continue
			}
			if retired, err := loadRuntimePoolBootRetirement(writeCtx, r.ControlStore, other); err != nil || !retired {
				return errors.Join(fmt.Errorf("%w: retained native Pod has an unretired boot", store.ErrNotReady), err)
			}
		}
		base := pod.DeepCopy()
		controllerutil.RemoveFinalizer(pod, runtimePoolBootPodFinalizer)
		delete(pod.Annotations, runtimePoolBootRetentionAnnotation)
		return r.Patch(writeCtx, pod, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	})
}

// Guard only new enrolled native owners; provider and legacy finalization keep
// their existing path. The optimistic owner PATCH fences delayed enrollment.
func (r *RuntimePoolReconciler) removeRuntimePoolFinalizer(ctx context.Context, pool *corev1alpha1.RuntimePool) error {
	remove := func(writeCtx context.Context) error {
		base := pool.DeepCopy()
		controllerutil.RemoveFinalizer(pool, runtimePoolFinalizer)
		return client.IgnoreNotFound(r.Patch(writeCtx, pool, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})))
	}
	if pool.Spec.ExecutionWorkspace != nil {
		return remove(ctx)
	}
	preparations, err := r.runtimePoolBootRecords(ctx, pool, runtimePoolBootPreparationKind)
	if err != nil {
		return err
	}
	if len(preparations) == 0 && pool.Annotations[runtimePoolBootOwnerVersionAnnotation] == "" {
		return remove(ctx)
	}
	fence, err := r.runtimePoolRetirementFence(ctx)
	if err != nil {
		return err
	}
	return agentRuntimeRecoveryGuard(ctx, r.ControlStore, fence, func(writeCtx context.Context) error {
		witnesses, err := r.runtimePoolBootRecords(writeCtx, pool, runtimePoolBootWitnessKind)
		if err != nil {
			return err
		}
		for _, w := range witnesses {
			if retired, err := loadRuntimePoolBootRetirement(writeCtx, r.ControlStore, w); err != nil || !retired {
				return errors.Join(fmt.Errorf("%w: RuntimePool owner still has an unretired boot", store.ErrNotReady), err)
			}
		}
		return remove(writeCtx)
	})
}

func (r *RuntimePoolReconciler) failRuntimePoolBootReconcile(ctx context.Context, pool *corev1alpha1.RuntimePool, cause error) (ctrl.Result, error) {
	fence, err := r.runtimePoolRetirementFence(ctx)
	if err != nil {
		return ctrl.Result{}, errors.Join(cause, err)
	}
	err = agentRuntimeRecoveryGuard(ctx, r.ControlStore, fence, func(writeCtx context.Context) error {
		status := pool.DeepCopy().Status
		status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDegraded
		status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
		status.Message = "native runtime boot enrollment or retirement evidence is unavailable; admission is closed"
		r.setRuntimePoolCondition(pool, &status, corev1alpha1.RuntimePoolConditionAdmissionReady, metav1.ConditionFalse, corev1alpha1.RuntimePoolReasonAdmissionClosed, status.Message)
		_, patchErr := r.finishRuntimePoolStatus(writeCtx, pool, status, runtimePoolRequeue)
		return patchErr
	})
	return ctrl.Result{}, errors.Join(cause, err)
}

func (r *RuntimePoolReconciler) finishEnrolledRuntimePoolServing(ctx context.Context, pool *corev1alpha1.RuntimePool, status corev1alpha1.RuntimePoolStatus, enrolled *runtimePoolBootWitness) (ctrl.Result, error) {
	if enrolled == nil {
		return r.finishRuntimePoolStatus(ctx, pool, status, runtimePoolRequeue)
	}
	fence, err := r.runtimePoolRetirementFence(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	var result ctrl.Result
	err = agentRuntimeRecoveryGuard(ctx, r.ControlStore, fence, func(writeCtx context.Context) error {
		current := &corev1alpha1.RuntimePool{}
		if err := r.APIReader.Get(writeCtx, client.ObjectKeyFromObject(pool), current); err != nil {
			return err
		}
		if current.UID != pool.UID || current.Generation != pool.Generation || !current.DeletionTimestamp.IsZero() || current.Spec.ExecutionWorkspace != nil || current.Spec.Runtime.Profile.Digest != pool.Spec.Runtime.Profile.Digest {
			return fmt.Errorf("%w: native pool authority changed before admission", store.ErrConflict)
		}
		active := status.ActiveInstance
		if active == nil || active.ControllerEpoch != fence.Epoch {
			return fmt.Errorf("%w: enrolled native runtime admission fence changed", store.ErrConflict)
		}
		w, err := loadRuntimePoolBootWitness(writeCtx, r.ControlStore, agentRuntimeRecoveryIdentity(runtimePoolBootWitnessKind, pool.UID, pool.Namespace, active.RuntimeInstanceID))
		if err != nil {
			return err
		}
		if retired, err := loadRuntimePoolBootRetirement(writeCtx, r.ControlStore, w); err != nil || retired {
			return errors.Join(fmt.Errorf("%w: retired native boot cannot serve", store.ErrConflict), err)
		}
		if err := r.requireRuntimePoolBootRetention(writeCtx, w); err != nil {
			return err
		}
		pod := &corev1.Pod{}
		if err := r.APIReader.Get(writeCtx, client.ObjectKey{Namespace: w.PodNamespace, Name: w.PodName}, pod); err != nil {
			return err
		}
		if !pod.DeletionTimestamp.IsZero() || !runtimePoolWitnessRunning(w, pod) || pod.Status.PodIP != w.PodAddress {
			return fmt.Errorf("%w: enrolled native container changed before admission", store.ErrConflict)
		}
		result, err = r.finishRuntimePoolStatus(writeCtx, pool, status, runtimePoolRequeue)
		return err
	})
	return result, err
}
