/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"container/list"

	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

type acpSettledDeliveryRecovery struct {
	resourceVersion string
	epoch           int64
}

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

const acpSettledDeliveryRecoveryCacheSize = 16384

type acpSettledDeliveryRecoveryCache struct {
	limit   int
	entries map[types.UID]*list.Element
	order   list.List
}

type acpSettledDeliveryRecoveryEntry struct {
	uid     types.UID
	verdict acpSettledDeliveryRecovery
}

func (c *acpSettledDeliveryRecoveryCache) size() int {
	if c.limit > 0 {
		return c.limit
	}
	return acpSettledDeliveryRecoveryCacheSize
}

func (c *acpSettledDeliveryRecoveryCache) len() int {
	return len(c.entries)
}

func (c *acpSettledDeliveryRecoveryCache) get(uid types.UID) (acpSettledDeliveryRecovery, bool) {
	element, ok := c.entries[uid]
	if !ok {
		return acpSettledDeliveryRecovery{}, false
	}
	c.order.MoveToFront(element)
	return element.Value.(*acpSettledDeliveryRecoveryEntry).verdict, true
}

func (c *acpSettledDeliveryRecoveryCache) put(uid types.UID, verdict acpSettledDeliveryRecovery) {
	if c.entries == nil {
		c.entries = make(map[types.UID]*list.Element)
	}
	if element, ok := c.entries[uid]; ok {
		element.Value.(*acpSettledDeliveryRecoveryEntry).verdict = verdict
		c.order.MoveToFront(element)
		return
	}
	for len(c.entries) >= c.size() {
		c.remove(c.order.Back().Value.(*acpSettledDeliveryRecoveryEntry).uid)
	}
	c.entries[uid] = c.order.PushFront(&acpSettledDeliveryRecoveryEntry{uid: uid, verdict: verdict})
}

func (c *acpSettledDeliveryRecoveryCache) remove(uid types.UID) {
	if element, ok := c.entries[uid]; ok {
		c.order.Remove(element)
		delete(c.entries, uid)
	}
}

func (d *ACPDispatcher) deliveryRecoverySettled(task *corev1alpha1.Task, epoch int64) bool {
	if task == nil || task.UID == "" || task.ResourceVersion == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	settled, ok := d.settledDeliveryRecoveries.get(task.UID)
	return ok && settled.resourceVersion == task.ResourceVersion && settled.epoch == epoch
}

func (d *ACPDispatcher) rememberDeliveryRecoverySettled(task *corev1alpha1.Task, epoch int64) {
	if task == nil || task.UID == "" || task.ResourceVersion == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.settledDeliveryRecoveries.put(task.UID, acpSettledDeliveryRecovery{resourceVersion: task.ResourceVersion, epoch: epoch})
}

func (d *ACPDispatcher) pruneSettledDeliveryRecoveries(tasks []corev1alpha1.Task) {
	live := make(map[types.UID]struct{}, len(tasks))
	for i := range tasks {
		live[tasks[i].UID] = struct{}{}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for uid := range d.settledDeliveryRecoveries.entries {
		if _, ok := live[uid]; !ok {
			d.settledDeliveryRecoveries.remove(uid)
		}
	}
}
