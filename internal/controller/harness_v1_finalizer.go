/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"fmt"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

// harnessV1TaskDeletionReady atomically re-proves every route-specific v1
// terminal and Session/outbox barrier before removing the attempt aggregates.
// Reclamation is the finalization proof: an active attempt or incomplete
// Session projection retains the Task finalizer for dispatcher recovery.
func (r *TaskReconciler) harnessV1TaskDeletionReady(
	ctx context.Context,
	task *corev1alpha1.Task,
) (bool, error) {
	if !taskManagedByHarnessV1(task) {
		return true, nil
	}
	binding := task.Status.AgentExecutionBinding
	if task.UID == "" || binding == nil {
		return false, fmt.Errorf("harness v1 Task identity and binding are required for deletion")
	}
	if r.HarnessV1Attempts == nil {
		return false, fmt.Errorf("harness v1 attempt store is required for deletion")
	}
	if r.ControllerEpochManager == nil {
		return false, fmt.Errorf("controller epoch manager is required for harness v1 deletion")
	}
	fence, err := r.ControllerEpochManager.CurrentFence(ctx)
	if err != nil {
		return false, err
	}
	_, err = r.HarnessV1Attempts.ReclaimHarnessV1Attempts(ctx, store.ReclaimHarnessV1AttemptsRequest{
		Namespace:       task.Namespace,
		TaskUID:         string(task.UID),
		BindingDigest:   binding.BindingDigest,
		SessionRequired: task.Spec.SessionRef != nil,
		Fence:           fence,
	})
	if errors.Is(err, store.ErrNotReady) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reclaim harness v1 attempts before Task deletion: %w", err)
	}
	return true, nil
}
