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
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func reservedWriteTestTask(poolName, poolUID string) *corev1alpha1.Task {
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "reserving", UID: "reserving-uid"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending, Execution: &corev1alpha1.TaskExecutionStatus{
			State: corev1alpha1.TaskExecutionStateQueued, Attempt: 1, PromptID: "prompt-1", RequestDigest: "sha256:request",
			RuntimePoolName: poolName, RuntimePoolUID: poolUID,
		}},
	}
}

// The Reserved write checks the pool binding on the uncached read its
// optimistic patch is based on: it writes when the attempt is still bound to
// the pool and writes nothing when the binding moved.
func TestPatchReservedExecutionForPoolChecksTheBindingItWritesOn(t *testing.T) {
	pool := &corev1alpha1.RuntimePool{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "pool", UID: "pool-uid"}}
	reserve := func(status *corev1alpha1.TaskExecutionStatus) { status.State = corev1alpha1.TaskExecutionStateReserved }

	stored := reservedWriteTestTask(pool.Name, string(pool.UID))
	r := newUnitReconciler(newTestScheme(), stored)
	dispatcher := &ACPDispatcher{Client: r.Client, APIReader: r.Client}
	inHand := stored.DeepCopy()
	bound, err := dispatcher.patchReservedExecutionForPool(context.Background(), inHand, pool, reserve)
	if err != nil || !bound || inHand.Status.Execution.State != corev1alpha1.TaskExecutionStateReserved {
		t.Fatalf("bound=%t err=%v state=%s, want the Reserved write", bound, err, inHand.Status.Execution.State)
	}

	rebound := reservedWriteTestTask("replacement", "replacement-uid")
	r = newUnitReconciler(newTestScheme(), rebound)
	dispatcher = &ACPDispatcher{Client: r.Client, APIReader: r.Client}
	inHand = reservedWriteTestTask(pool.Name, string(pool.UID))
	bound, err = dispatcher.patchReservedExecutionForPool(context.Background(), inHand, pool, reserve)
	if err != nil || bound {
		t.Fatalf("bound=%t err=%v, want no write for a Task rebound to another pool", bound, err)
	}
	current := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(rebound), current); err != nil {
		t.Fatal(err)
	}
	if current.Status.Execution.State != corev1alpha1.TaskExecutionStateQueued {
		t.Fatalf("rebound Task state = %s, want it untouched", current.Status.Execution.State)
	}
}

// A scan reports how long after the first pending wakeup it started.
func TestDispatcherWakeRecordsTheFirstPendingWakeup(t *testing.T) {
	d := &ACPDispatcher{}
	d.Wake()
	first := d.wokenAt.Load()
	time.Sleep(time.Millisecond)
	d.Wake()
	if first == 0 || d.wokenAt.Load() != first {
		t.Fatalf("wokenAt = %d after a second wakeup, want the first (%d) kept", d.wokenAt.Load(), first)
	}
	if woken := d.wokenAt.Swap(0); woken != first {
		t.Fatalf("scan consumed %d, want %d", woken, first)
	}
}

func TestMarkACPStepUsesTheContextTimer(t *testing.T) {
	markACPStep(context.Background(), "ignored") // no timer: nothing to mark
	steps := newACPDispatchScanTimer(time.Now, false)
	ctx := withACPSteps(context.Background(), steps)
	markACPStep(ctx, "sessionLease")
	if len(steps.values) != 2 || steps.values[0] != "sessionLeaseMs" {
		t.Fatalf("steps = %v, want the marked step", steps.values)
	}
}

// Admission is timed only for a pool that is not yet admitting, and is
// reported once, when its stored status serves and accepts.
func TestRuntimeAdmissionTimingsRecordOnlyRealAdmissions(t *testing.T) {
	clock := &steppingClock{now: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)}
	timings := &acpRuntimeAdmissionTimings{now: clock.Now, records: map[acpRuntimeAdmissionKey]*acpRuntimeAdmissionRecord{}}
	pool := &corev1alpha1.RuntimePool{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "pool", UID: "pool-uid"}}
	pool.Status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStarting
	r := newUnitReconciler(newTestScheme(), pool)

	timings.startupObserved(pool, 2)
	clock.Advance(300 * time.Millisecond)
	timings.credentialsSeeded(pool, 2)
	clock.Advance(200 * time.Millisecond)
	timings.served(context.Background(), r.Client, pool, 2) // still starting: nothing reported
	if len(timings.records) != 1 {
		t.Fatalf("records = %d, want the admission still open while the pool starts", len(timings.records))
	}
	setRuntimePoolAdmitting(t, r.Client, pool)
	timings.served(context.Background(), r.Client, pool, 2)
	if len(timings.records) != 0 {
		t.Fatal("serving did not report and close the admission")
	}
	// Steady-state reconciles of an admitting pool record nothing.
	timings.startupObserved(pool, 2)
	if len(timings.records) != 0 {
		t.Fatal("a pool already serving and accepting opened an admission record")
	}
}

func setRuntimePoolAdmitting(t *testing.T, c client.Client, pool *corev1alpha1.RuntimePool) {
	t.Helper()
	stored := &corev1alpha1.RuntimePool{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pool), stored); err != nil {
		t.Fatal(err)
	}
	stored.Status.Lifecycle = corev1alpha1.RuntimePoolLifecycleServing
	stored.Status.AdmissionState = corev1alpha1.RuntimePoolAdmissionAccepting
	// This fake registers no RuntimePool status subresource, so Update
	// stores the status.
	if err := c.Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	pool.Status.Lifecycle = stored.Status.Lifecycle
	pool.Status.AdmissionState = stored.Status.AdmissionState
}

// The pass sets its own copy of the pool to Serving before writing the
// status. When that write fails or conflicts, the stored pool still starts:
// the admission stays open, and the pass that lands the write reports it
// once, with the retry's delay included.
func TestRuntimeAdmissionTimingsWaitForThePersistedStatus(t *testing.T) {
	clock := &steppingClock{now: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)}
	timings := &acpRuntimeAdmissionTimings{now: clock.Now, records: map[acpRuntimeAdmissionKey]*acpRuntimeAdmissionRecord{}}
	pool := &corev1alpha1.RuntimePool{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "pool", UID: "pool-uid"}}
	pool.Status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStarting
	r := newUnitReconciler(newTestScheme(), pool)
	timings.startupObserved(pool, 3)

	// The failed write left only the pass's copy serving.
	local := pool.DeepCopy()
	local.Status.Lifecycle = corev1alpha1.RuntimePoolLifecycleServing
	local.Status.AdmissionState = corev1alpha1.RuntimePoolAdmissionAccepting
	timings.served(context.Background(), r.Client, local, 3)
	if len(timings.records) != 1 {
		t.Fatal("a status write that never landed closed the admission")
	}

	clock.Advance(time.Second)
	setRuntimePoolAdmitting(t, r.Client, pool)
	timings.served(context.Background(), r.Client, pool, 3)
	if len(timings.records) != 0 {
		t.Fatal("the pass that landed the status did not report the admission")
	}
	timings.served(context.Background(), r.Client, pool, 3)
	if len(timings.records) != 0 {
		t.Fatal("a later pass reopened the admission")
	}
}

// A runtime admitted once is admitted again in the same workload sequence
// when the pool closes and reopens, such as after an attachment changes the
// workspace generation. The record marks that admission a readmission and
// keeps the lifecycle the pool reopened from; a first start is not one.
func TestRuntimeAdmissionTimingsMarkReadmissions(t *testing.T) {
	timings := &acpRuntimeAdmissionTimings{now: time.Now, records: map[acpRuntimeAdmissionKey]*acpRuntimeAdmissionRecord{}}
	pool := &corev1alpha1.RuntimePool{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "pool", UID: "pool-uid"}}
	pool.Status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStarting
	timings.startupObserved(pool, 1)
	first := timings.records[acpRuntimeAdmissionKey{pool: pool.UID, sequence: 1}]
	if first == nil || first.readmission {
		t.Fatalf("first start record = %+v, want an admission that is not a readmission", first)
	}

	reopened := pool.DeepCopy()
	reopened.Status.ActiveInstance = &corev1alpha1.RuntimePoolActiveInstanceStatus{RuntimeInstanceID: "workspace:actor.boot"}
	reopened.Status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDegraded
	timings.startupObserved(reopened, 2)
	again := timings.records[acpRuntimeAdmissionKey{pool: pool.UID, sequence: 2}]
	if again == nil || !again.readmission || again.reopenedFrom != corev1alpha1.RuntimePoolLifecycleDegraded {
		t.Fatalf("reopen record = %+v, want a readmission from Degraded", again)
	}
}
