package controller

import (
	"context"
	"fmt"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const nativeSubstrateDeletionSettlementGrace = 25 * time.Second

// Authenticated quiescence can precede the controller consuming the final
// receipt. Retain that exact runtime briefly, without extending prompt deadlines
// or making controller availability an unbounded prerequisite for deletion.
// The caller must still prove authenticated quiescence and retirement authority;
// expiry releases only this additional controller-status wait.
func (r *RuntimePoolReconciler) nativeSubstrateDeletionTasksSettled(ctx context.Context, pool *corev1alpha1.RuntimePool, active *corev1alpha1.RuntimePoolActiveInstanceStatus, startedAt time.Time) (bool, error) {
	if !r.now().Before(startedAt.Add(nativeSubstrateDeletionSettlementGrace)) {
		return true, nil
	}
	var tasks corev1alpha1.TaskList
	if err := uncachedReader(r.APIReader, r.Client).List(ctx, &tasks, client.InNamespace(pool.Namespace)); err != nil {
		return false, fmt.Errorf("list Tasks before native RuntimePool deletion settlement: %w", err)
	}
	for i := range tasks.Items {
		task := &tasks.Items[i]
		execution := task.Status.Execution
		if execution == nil || execution.RuntimePoolName != pool.Name || execution.RuntimePoolUID != string(pool.UID) ||
			execution.RuntimeInstanceID != active.RuntimeInstanceID || execution.RuntimeSessionSupervisorBootID != active.BootID {
			continue
		}
		// Match workspace demand release: success also waits for delivery,
		// whereas cancellation and other terminal execution states are settled.
		if !taskExecutionStateTerminal(execution.State) || execution.State == corev1alpha1.TaskExecutionStateSucceeded &&
			(task.Status.Delivery == nil || !store.IsTerminalPromptDeliveryState(store.PromptDeliveryState(task.Status.Delivery.State))) {
			return false, nil
		}
	}
	return true, nil
}
