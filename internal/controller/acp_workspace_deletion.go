package controller

import (
	"context"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

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
	retired, err := r.retireACPTaskArtifactReferences(ctx, task)
	if err != nil || !retired {
		return retired, err
	}
	return r.reconcileACPClassWorkspaceSettlement(ctx, task)
}
