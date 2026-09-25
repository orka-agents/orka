package controller

import (
	"context"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// prepareACPClassWorkspaceDeletion releases a settled Session Task's workspace
// before final Task reclamation. DataOnly suspension retires its runtime and
// records the receipt that Session archival needs; waiting for archival first
// would leave the cancelled Task's Actor attached indefinitely.
func (r *TaskReconciler) prepareACPClassWorkspaceDeletion(ctx context.Context, task *corev1alpha1.Task) (bool, error) {
	if task == nil || task.Spec.Type != corev1alpha1.TaskTypeAgent || task.Spec.SessionRef == nil ||
		task.DeletionTimestamp.IsZero() || task.Status.Execution == nil || r.DurableControlStore == nil ||
		strings.TrimSpace(task.Labels[acpExecutionWorkspaceLinkLabel]) == "" ||
		task.Annotations[acpTaskWorkspaceSettledAnnotation] != "" {
		return true, nil
	}
	ready, err := r.acpTaskCleanupReady(ctx, task, false)
	if err != nil || !ready {
		return ready, err
	}
	// Capabilities must retire before detach can destroy their serving runtime.
	// Keep the Task finalizer and all durable attempt/Session records until the
	// normal deletion path observes runtime retirement and archival receipts.
	retired, err := r.retireACPTaskArtifactReferences(ctx, task)
	if err != nil || !retired {
		return retired, err
	}
	return r.reconcileACPClassWorkspaceSettlement(ctx, task)
}
