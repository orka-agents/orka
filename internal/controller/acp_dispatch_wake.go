/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// acpDispatchWakeMinInterval bounds how often wakeups can start a dispatch
// scan. Each scan lists every Task and reads durable state, so a burst of
// events coalesces into at most one extra scan per interval; the periodic
// ticker is unchanged.
const acpDispatchWakeMinInterval = 200 * time.Millisecond

// Wake asks the dispatcher to scan before its next tick. It never blocks, and
// wakeups that arrive while one is pending coalesce. A wakeup only moves a
// scan earlier: reservation still re-reads the Task, the epoch fence, and the
// RuntimePool admission state.
func (d *ACPDispatcher) Wake() {
	select {
	case d.wakeChannel() <- struct{}{}:
	default:
	}
}

func (d *ACPDispatcher) wakeChannel() chan struct{} {
	d.wakeOnce.Do(func() { d.wake = make(chan struct{}, 1) })
	return d.wake
}

// waitForNextDispatchScan blocks until the ticker fires, or a wakeup arrives
// and at least acpDispatchWakeMinInterval has passed since lastScan. It
// reports false when ctx ends.
func waitForNextDispatchScan(ctx context.Context, tick <-chan time.Time, wake <-chan struct{}, lastScan time.Time) bool {
	select {
	case <-ctx.Done():
		return false
	case <-tick:
		return true
	case <-wake:
	}
	remaining := acpDispatchWakeMinInterval - time.Since(lastScan)
	if remaining <= 0 {
		return true
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-tick:
		return true
	case <-timer.C:
		return true
	}
}

// registerDispatchWakeHandlers wakes the dispatcher when a Task becomes
// queued for dispatch and when a RuntimePool starts serving or accepting, or
// frees capacity. Without informers, or if registration fails, the dispatcher keeps
// its periodic scan only. The returned function removes the handlers.
func (d *ACPDispatcher) registerDispatchWakeHandlers(ctx context.Context) func() {
	if d.Informers == nil {
		return func() {}
	}
	log := logf.FromContext(ctx)
	var removers []func()
	register := func(object client.Object, handler toolscache.ResourceEventHandler) {
		informer, err := d.Informers.GetInformer(ctx, object)
		if err != nil {
			log.Error(err, "ACP dispatch wakeups unavailable; dispatch keeps its periodic scan", "kind", fmt.Sprintf("%T", object))
			return
		}
		registration, err := informer.AddEventHandler(handler)
		if err != nil {
			log.Error(err, "ACP dispatch wakeups unavailable; dispatch keeps its periodic scan", "kind", fmt.Sprintf("%T", object))
			return
		}
		removers = append(removers, func() { _ = informer.RemoveEventHandler(registration) })
	}
	register(&corev1alpha1.Task{}, toolscache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObject, newObject any) {
			oldTask, _ := oldObject.(*corev1alpha1.Task)
			newTask, _ := newObject.(*corev1alpha1.Task)
			if acpTaskEnteredDispatchQueue(oldTask, newTask) {
				d.Wake()
			}
		},
	})
	register(&corev1alpha1.RuntimePool{}, toolscache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObject, newObject any) {
			oldPool, _ := oldObject.(*corev1alpha1.RuntimePool)
			newPool, _ := newObject.(*corev1alpha1.RuntimePool)
			if acpRuntimePoolOpenedForDispatch(oldPool, newPool) {
				d.Wake()
			}
		},
	})
	return func() {
		for _, remove := range removers {
			remove()
		}
	}
}

// acpTaskEnteredDispatchQueue reports a Task update that the next dispatch
// scan can act on: it newly reached Queued, or was rebound to another target.
func acpTaskEnteredDispatchQueue(oldTask, newTask *corev1alpha1.Task) bool {
	if newTask == nil || newTask.Status.Execution == nil ||
		newTask.Status.Execution.State != corev1alpha1.TaskExecutionStateQueued {
		return false
	}
	if oldTask == nil || oldTask.Status.Execution == nil {
		return true
	}
	previous, current := oldTask.Status.Execution, newTask.Status.Execution
	return previous.State != current.State || previous.Attempt != current.Attempt ||
		previous.RuntimePoolUID != current.RuntimePoolUID || previous.AgentRuntimeUID != current.AgentRuntimeUID
}

// acpRuntimePoolOpenedForDispatch reports a RuntimePool update after which a
// waiting Task may now be reservable, mirroring runtimePoolReservationAdmission:
// prompt slots need a Serving pool with an active instance, and only a new
// resident session also needs Accepting. It wakes when the pool starts
// serving, starts accepting sessions, changes its active instance, or frees
// prompt or session capacity while serving.
func acpRuntimePoolOpenedForDispatch(oldPool, newPool *corev1alpha1.RuntimePool) bool {
	if !runtimePoolServingForDispatch(newPool) {
		return false
	}
	if !runtimePoolServingForDispatch(oldPool) {
		return true
	}
	if newPool.Status.AdmissionState == corev1alpha1.RuntimePoolAdmissionAccepting &&
		oldPool.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionAccepting {
		return true
	}
	if !reflect.DeepEqual(activeInstanceIdentity(oldPool.Status.ActiveInstance), activeInstanceIdentity(newPool.Status.ActiveInstance)) {
		return true
	}
	previous, current := oldPool.Status.Capacity, newPool.Status.Capacity
	return current.RunningPrompts < previous.RunningPrompts ||
		current.ReservedPrompts < previous.ReservedPrompts ||
		current.ResidentSessions < previous.ResidentSessions ||
		current.ReservedSessions < previous.ReservedSessions ||
		len(current.Reservations) < len(previous.Reservations)
}

func runtimePoolServingForDispatch(pool *corev1alpha1.RuntimePool) bool {
	return pool != nil && pool.DeletionTimestamp.IsZero() &&
		pool.Status.Lifecycle == corev1alpha1.RuntimePoolLifecycleServing &&
		pool.Status.ActiveInstance != nil && strings.TrimSpace(pool.Status.ActiveInstance.RuntimeInstanceID) != ""
}
