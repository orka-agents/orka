/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestACPDispatcherWakeCoalesces(t *testing.T) {
	d := &ACPDispatcher{}
	d.Wake()
	d.Wake() // never blocks while one wakeup is pending
	select {
	case <-d.wakeChannel():
	default:
		t.Fatal("Wake did not signal the dispatcher")
	}
	select {
	case <-d.wakeChannel():
		t.Fatal("pending wakeups did not coalesce")
	default:
	}
}

func TestWaitForNextDispatchScan(t *testing.T) {
	ctx := context.Background()
	wake := make(chan struct{}, 1)

	// A wakeup long after the last scan starts the next scan immediately.
	wake <- struct{}{}
	start := time.Now()
	if !waitForNextDispatchScan(ctx, nil, wake, start.Add(-time.Second)) {
		t.Fatal("wakeup did not start a scan")
	}
	if elapsed := time.Since(start); elapsed >= acpDispatchWakeMinInterval {
		t.Fatalf("stale-scan wakeup waited %s", elapsed)
	}

	// A wakeup right after a scan waits out the minimum interval.
	wake <- struct{}{}
	start = time.Now()
	if !waitForNextDispatchScan(ctx, nil, wake, start) {
		t.Fatal("wakeup did not start a scan")
	}
	if elapsed := time.Since(start); elapsed < acpDispatchWakeMinInterval-10*time.Millisecond {
		t.Fatalf("back-to-back wakeup scanned after %s, want at least %s", elapsed, acpDispatchWakeMinInterval)
	}

	// The periodic tick still drives scans without wakeups.
	tick := make(chan time.Time, 1)
	tick <- time.Now()
	if !waitForNextDispatchScan(ctx, tick, wake, time.Now()) {
		t.Fatal("tick did not start a scan")
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if waitForNextDispatchScan(cancelled, nil, wake, time.Now()) {
		t.Fatal("a cancelled dispatcher kept scanning")
	}
}

func TestACPTaskEnteredDispatchQueue(t *testing.T) {
	queued := &corev1alpha1.Task{Status: corev1alpha1.TaskStatus{Execution: &corev1alpha1.TaskExecutionStatus{
		State: corev1alpha1.TaskExecutionStateQueued, Attempt: 1, RuntimePoolName: "pool", RuntimePoolUID: "pool-uid",
	}}}
	pending := &corev1alpha1.Task{}
	rebound := queued.DeepCopy()
	rebound.Status.Execution.RuntimePoolUID = "replacement-uid"
	reserved := queued.DeepCopy()
	reserved.Status.Execution.State = corev1alpha1.TaskExecutionStateReserved
	relabelled := queued.DeepCopy()
	relabelled.Labels = map[string]string{"unrelated": "change"}

	for _, tc := range []struct {
		name     string
		old, new *corev1alpha1.Task
		want     bool
	}{
		{name: "newly queued", old: pending, new: queued, want: true},
		{name: "rebound to a replacement pool", old: queued, new: rebound, want: true},
		{name: "requeued after reservation", old: reserved, new: queued, want: true},
		{name: "unrelated update of a queued Task", old: queued, new: relabelled},
		{name: "reserved", old: queued, new: reserved},
		{name: "not executing", old: pending, new: pending},
	} {
		if got := acpTaskEnteredDispatchQueue(tc.old, tc.new); got != tc.want {
			t.Errorf("%s: acpTaskEnteredDispatchQueue = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestACPRuntimePoolOpenedForDispatch(t *testing.T) {
	serving := &corev1alpha1.RuntimePool{
		ObjectMeta: metav1.ObjectMeta{Generation: 2},
		Status: corev1alpha1.RuntimePoolStatus{
			ObservedGeneration: 2, Lifecycle: corev1alpha1.RuntimePoolLifecycleServing,
			AdmissionState: corev1alpha1.RuntimePoolAdmissionAccepting,
			ActiveInstance: &corev1alpha1.RuntimePoolActiveInstanceStatus{RuntimeInstanceID: "instance-1", ControllerEpoch: 1},
			Capacity:       corev1alpha1.RuntimePoolCapacityStatus{RunningPrompts: 1, ReservedPrompts: 1},
		},
	}
	starting := serving.DeepCopy()
	starting.Status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStarting
	starting.Status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
	// A Serving pool closed to new sessions still admits a continuation's
	// prompt-only reservation.
	closed := serving.DeepCopy()
	closed.Status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
	closedReleased := closed.DeepCopy()
	closedReleased.Status.Capacity.RunningPrompts = 0
	released := serving.DeepCopy()
	released.Status.Capacity.RunningPrompts = 0
	busier := serving.DeepCopy()
	busier.Status.Capacity.ReservedPrompts = 2
	rebound := serving.DeepCopy()
	rebound.Status.ActiveInstance.ControllerEpoch = 2
	probed := serving.DeepCopy()
	observed := metav1.Now()
	probed.Status.ActiveInstance.LastObservedTime = &observed
	noInstance := serving.DeepCopy()
	noInstance.Status.ActiveInstance = nil
	deleting := serving.DeepCopy()
	now := metav1.Now()
	deleting.DeletionTimestamp = &now

	for _, tc := range []struct {
		name     string
		old, new *corev1alpha1.RuntimePool
		want     bool
	}{
		{name: "started serving and accepting", old: starting, new: serving, want: true},
		{name: "started serving closed to new sessions", old: starting, new: closed, want: true},
		{name: "started accepting sessions", old: closed, new: serving, want: true},
		{name: "released capacity", old: serving, new: released, want: true},
		{name: "closed pool released prompt capacity", old: closed, new: closedReleased, want: true},
		{name: "active instance rebound to the current epoch", old: serving, new: rebound, want: true},
		{name: "health probe timestamp", old: serving, new: probed},
		{name: "took more capacity", old: serving, new: busier},
		{name: "stopped serving", old: serving, new: starting},
		{name: "stopped accepting", old: serving, new: closed},
		{name: "no active instance", old: starting, new: noInstance},
		{name: "deleting", old: starting, new: deleting},
		{name: "unchanged", old: serving, new: serving.DeepCopy()},
	} {
		if got := acpRuntimePoolOpenedForDispatch(tc.old, tc.new); got != tc.want {
			t.Errorf("%s: acpRuntimePoolOpenedForDispatch = %v, want %v", tc.name, got, tc.want)
		}
	}
}
