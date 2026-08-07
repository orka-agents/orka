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
	attempts, err := r.HarnessV1Attempts.ListHarnessV1AttemptsByTask(
		ctx, task.Namespace, string(task.UID),
	)
	if err != nil {
		return false, fmt.Errorf("list harness v1 attempts before Task deletion: %w", err)
	}
	reclamationBindingDigest := binding.BindingDigest
	allTerminal := true
	for i := range attempts {
		attempt := &attempts[i]
		if attempt.Namespace != task.Namespace || attempt.TaskName != task.Name ||
			attempt.TaskUID != string(task.UID) {
			return false, fmt.Errorf("harness v1 attempt identity changed before Task deletion")
		}
		if attempt.BindingDigest != reclamationBindingDigest {
			return false, errors.New("harness v1 attempt binding changed before Task deletion")
		}
		if !store.IsTerminalHarnessV1AttemptState(attempt.State) {
			allTerminal = false
		}
	}
	if !allTerminal {
		return false, nil
	}
	// A crash may occur after terminal attempt persistence but before ordinary
	// dispatcher settlement retires the legacy runtime_sessions row. Perform
	// the same idempotent settlement used by dispatcher and adjudicated cleanup
	// before the attempt aggregate can be reclaimed.
	runtimeSessions := HarnessV1Dispatcher{Attempts: r.HarnessV1Attempts}
	for i := range attempts {
		if err := runtimeSessions.settleHarnessV1RuntimeSessionRecord(ctx, task, &attempts[i]); err != nil {
			if errors.Is(err, store.ErrNotReady) {
				return false, nil
			}
			return false, fmt.Errorf("settle harness v1 runtime session before Task deletion: %w", err)
		}
	}
	fence, err := r.ControllerEpochManager.CurrentFence(ctx)
	if err != nil {
		return false, err
	}
	_, err = r.HarnessV1Attempts.ReclaimHarnessV1Attempts(ctx, store.ReclaimHarnessV1AttemptsRequest{
		Namespace:       task.Namespace,
		TaskUID:         string(task.UID),
		BindingDigest:   reclamationBindingDigest,
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
