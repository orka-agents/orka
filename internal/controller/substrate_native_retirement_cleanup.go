package controller

import (
	"context"
	"fmt"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func nativeSubstrateFailedPoolInactive(pool *corev1alpha1.RuntimePool) bool {
	return runtimePoolIsSubstrateBacked(pool) && pool.UID != "" &&
		pool.Spec.DesiredReplicas == 0 && pool.Status.CurrentReplicas == 0 && pool.Status.ActiveInstance == nil &&
		pool.Status.Lifecycle == corev1alpha1.RuntimePoolLifecycleDegraded &&
		pool.Status.AdmissionState == corev1alpha1.RuntimePoolAdmissionClosed && pool.Status.ObservedGeneration == pool.Generation
}

func (r *RuntimePoolReconciler) verifyFailedNativeSubstrateCleanup(ctx context.Context, pool *corev1alpha1.RuntimePool) error {
	if !nativeSubstrateFailedPoolInactive(pool) {
		return fmt.Errorf("failed native workspace has not observed closed admission and zero replicas at the current generation")
	}
	return r.verifyNativeSubstrateFailedRetirement(ctx, pool)
}

func (r *RuntimePoolReconciler) verifyNativeSubstrateFailedRetirement(ctx context.Context, pool *corev1alpha1.RuntimePool) error {
	if pool.Annotations[substrateNativeJournalAnnotation] != substrateNativeJournalRequired {
		return fmt.Errorf("failed native workspace has no required lifecycle journal")
	}
	_, record, err := r.readNativeSubstrateState(ctx, pool)
	if err != nil {
		return fmt.Errorf("read failed native workspace cleanup proof: %w", err)
	}
	// Attempt is cleared only after proving workload absence and deleting the
	// Actor. A terminal failure never starts another attempt for this pool UID.
	if record == nil || record.Phase != substrateNativeFailed || record.Failure == "" || record.Attempt != nil || record.AfterStop != "" {
		return fmt.Errorf("failed native workspace cleanup is incomplete")
	}
	return nil
}

// recordFailedNativeSubstrateTaskCleanup preserves per-Task retirement proof
// after Actor loss, when authenticated supervisor drain is no longer possible.
// The exact terminal pool journal proves all its runtime incarnations absent,
// including failures cleaned before this controller started. Task and binding
// identities still fence each receipt; failure outcomes are never changed.
func (r *RuntimePoolReconciler) recordFailedNativeSubstrateTaskCleanup(ctx context.Context, pool *corev1alpha1.RuntimePool) error {
	if pool.DeletionTimestamp.IsZero() && !nativeSubstrateFailedPoolInactive(pool) {
		return fmt.Errorf("failed native RuntimePool has not closed admission for retirement")
	}
	if err := r.verifyNativeSubstrateFailedRetirement(ctx, pool); err != nil {
		return err
	}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	var tasks corev1alpha1.TaskList
	if err := reader.List(ctx, &tasks, client.InNamespace(pool.Namespace)); err != nil {
		return fmt.Errorf("list Tasks after failed native RuntimePool retirement: %w", err)
	}
	for i := range tasks.Items {
		task := &tasks.Items[i]
		execution := task.Status.Execution
		if execution == nil || execution.RuntimePoolName != pool.Name || execution.RuntimePoolUID != string(pool.UID) || execution.RuntimeSessionUID == "" {
			continue
		}
		taskUID := acpTaskControlUID(task)
		if runtimeSessionCleanupCompleteForUID(task, taskUID) {
			continue
		}
		binding := executionBinding(task, corev1alpha1.AgentRuntimeContractHarnessV2)
		profile := pool.Spec.Runtime.Profile.Digest
		sessionProfileMatches := execution.RuntimeSessionProfileDigest == profile ||
			(task.Spec.SessionRef == nil && execution.RuntimeSessionProfileDigest == "")
		if binding == nil || binding.Backend != corev1alpha1.AgentExecutionBackendRuntimePool || binding.Task.UID != taskUID ||
			binding.RuntimeProfileDigest != profile || !sessionProfileMatches ||
			strings.TrimSpace(execution.RuntimeInstanceID) == "" || strings.TrimSpace(execution.RuntimeSessionSupervisorBootID) == "" ||
			execution.RuntimeSessionGeneration < 1 || execution.AgentRuntimeName != "" || execution.AgentRuntimeUID != "" {
			return fmt.Errorf("%w: Task %s/%s lacks exact failed native RuntimePool retirement authority", store.ErrConflict, task.Namespace, task.Name)
		}
		digest, err := canonicalAgentExecutionBindingDigest(*binding)
		if err != nil || digest != binding.BindingDigest {
			return fmt.Errorf("%w: Task %s/%s RuntimePool binding failed integrity verification", store.ErrConflict, task.Namespace, task.Name)
		}
		if err := persistTaskScopedRuntimeSessionCleanupReceipt(ctx, r.Client, task, taskUID,
			execution.RuntimeInstanceID, execution.RuntimeSessionUID, execution.RuntimeSessionGeneration); err != nil {
			return fmt.Errorf("record failed native RuntimePool retirement proof for Task %s/%s: %w", task.Namespace, task.Name, err)
		}
	}
	return nil
}
