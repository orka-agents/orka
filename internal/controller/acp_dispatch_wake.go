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

const acpDispatchWakeMinInterval = 200 * time.Millisecond

func (d *ACPDispatcher) Wake() {
	d.wokenAt.CompareAndSwap(0, time.Now().UnixNano())
	select {
	case d.wakeChannel() <- struct{}{}:
	default:
	}
}

func (d *ACPDispatcher) wakeChannel() chan struct{} {
	d.wakeOnce.Do(func() { d.wake = make(chan struct{}, 1) })
	return d.wake
}

func (d *ACPDispatcher) wakeForAdmission() {
	d.admissionWake.Store(true)
	d.Wake()
}

func (d *ACPDispatcher) takeAdmissionWake() bool {
	return d.admissionWake.Swap(false)
}

func waitForNextDispatchScan(ctx context.Context, tick <-chan time.Time, wake <-chan struct{}, lastScan time.Time, immediate func() bool) bool {
	admission := func() bool { return immediate != nil && immediate() }
	select {
	case <-ctx.Done():
		return false
	case <-tick:
		admission()
		return true
	case <-wake:
	}
	for {
		admitted := admission()
		if admitted || acpDispatchWakeMinInterval-time.Since(lastScan) <= 0 {
			return true
		}
		timer := time.NewTimer(acpDispatchWakeMinInterval - time.Since(lastScan))
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-tick:
			timer.Stop()
			admission()
			return true
		case <-timer.C:
			admission()
			return true
		case <-wake:
			timer.Stop()
		}
	}
}

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
			switch {
			case acpRuntimePoolAdmissionOpened(oldPool, newPool):
				d.wakeForAdmission()
			case acpRuntimePoolOpenedForDispatch(oldPool, newPool):
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

func acpRuntimePoolOpenedForDispatch(oldPool, newPool *corev1alpha1.RuntimePool) bool {
	if !runtimePoolServingForDispatch(newPool) {
		return false
	}
	if acpRuntimePoolAdmissionOpened(oldPool, newPool) {
		return true
	}
	if newPool.Status.AdmissionState == corev1alpha1.RuntimePoolAdmissionAccepting &&
		oldPool.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionAccepting {
		return true
	}
	previous, current := oldPool.Status.Capacity, newPool.Status.Capacity
	return current.RunningPrompts < previous.RunningPrompts ||
		current.ReservedPrompts < previous.ReservedPrompts ||
		current.ResidentSessions < previous.ResidentSessions ||
		current.ReservedSessions < previous.ReservedSessions ||
		len(current.Reservations) < len(previous.Reservations)
}

func acpRuntimePoolAdmissionOpened(oldPool, newPool *corev1alpha1.RuntimePool) bool {
	if !runtimePoolServingForDispatch(newPool) {
		return false
	}
	if !runtimePoolServingForDispatch(oldPool) {
		return true
	}
	if newPool.Status.AdmissionState == corev1alpha1.RuntimePoolAdmissionAccepting &&
		oldPool.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionAccepting &&
		(oldPool.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed || !runtimePoolAtCapacity(oldPool.Status.Capacity)) {
		return true
	}
	return !reflect.DeepEqual(activeInstanceIdentity(oldPool.Status.ActiveInstance), activeInstanceIdentity(newPool.Status.ActiveInstance))
}

func runtimePoolServingForDispatch(pool *corev1alpha1.RuntimePool) bool {
	return pool != nil && pool.DeletionTimestamp.IsZero() &&
		pool.Status.Lifecycle == corev1alpha1.RuntimePoolLifecycleServing &&
		pool.Status.ActiveInstance != nil && strings.TrimSpace(pool.Status.ActiveInstance.RuntimeInstanceID) != ""
}
