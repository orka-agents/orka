/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

// acpSettledDeliveryRecovery is the Task version and controller epoch at
// which delivery recovery last found nothing to recover from settled durable
// state.
type acpSettledDeliveryRecovery struct {
	resourceVersion string
	epoch           int64
}

// acpDeliveryRecoveryVerdictSettled reports whether a "nothing to recover"
// verdict for this Task can only change when the Task itself changes. Every
// durable input the verdict read must be final. A terminal execution state
// never transitions. Delivery is final in a terminal state other than
// NotRequested, which may still move to Validating: a Succeeded attempt's
// verdict reads delivery directly, so it needs final delivery; a Failed,
// Cancelled, or OutcomeUnknown attempt's verdict reads delivery only for
// pending runtime cleanup, so complete cleanup also suffices. The Task's phase
// must be settled, where a finalized or missing Session turn is already
// memoized as final. Deleting Tasks are never cached, because terminal
// recovery also owns their pending runtime cleanup.
func acpDeliveryRecoveryVerdictSettled(task *corev1alpha1.Task, attempt *store.PromptAttempt, cleanupPending bool) bool {
	if task == nil || attempt == nil || !task.DeletionTimestamp.IsZero() || !task.Status.Phase.IsTerminal() {
		return false
	}
	deliveryFinal := attempt.DeliveryState != store.PromptDeliveryNotRequested &&
		store.IsTerminalPromptDeliveryState(attempt.DeliveryState)
	switch attempt.ExecutionState {
	case store.PromptExecutionSucceeded:
		return deliveryFinal
	case store.PromptExecutionFailed, store.PromptExecutionCancelled, store.PromptExecutionOutcomeUnknown:
		return deliveryFinal || !cleanupPending
	default:
		return false
	}
}

// deliveryRecoverySettled reports whether delivery recovery already found
// nothing to recover for this exact Task version under this controller epoch.
// Any Task write, including a projection or cleanup receipt, changes its
// resourceVersion and makes the next scan read the store again.
func (d *ACPDispatcher) deliveryRecoverySettled(task *corev1alpha1.Task, epoch int64) bool {
	if task == nil || task.UID == "" || task.ResourceVersion == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	settled, ok := d.settledDeliveryRecoveries[task.UID]
	return ok && settled.resourceVersion == task.ResourceVersion && settled.epoch == epoch
}

func (d *ACPDispatcher) rememberDeliveryRecoverySettled(task *corev1alpha1.Task, epoch int64) {
	if task == nil || task.UID == "" || task.ResourceVersion == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.settledDeliveryRecoveries == nil {
		d.settledDeliveryRecoveries = make(map[types.UID]acpSettledDeliveryRecovery)
	}
	d.settledDeliveryRecoveries[task.UID] = acpSettledDeliveryRecovery{resourceVersion: task.ResourceVersion, epoch: epoch}
}

// pruneSettledDeliveryRecoveries forgets Tasks no longer listed.
func (d *ACPDispatcher) pruneSettledDeliveryRecoveries(tasks []corev1alpha1.Task) {
	live := make(map[types.UID]struct{}, len(tasks))
	for i := range tasks {
		live[tasks[i].UID] = struct{}{}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for uid := range d.settledDeliveryRecoveries {
		if _, ok := live[uid]; !ok {
			delete(d.settledDeliveryRecoveries, uid)
		}
	}
}
