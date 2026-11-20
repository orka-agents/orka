package controller

import (
	"strings"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// runtimePoolStartFailedTask is a standalone Task the dispatcher planned on the
// pool's active instance whose RuntimeSession creation then failed. Planning
// records the instance and RuntimeSession identity; the supervisor boot is
// recorded only after the RuntimeSession is created, so it can be empty.
func runtimePoolStartFailedTask(t *testing.T, pool *corev1alpha1.RuntimePool, name, bootID string) *corev1alpha1.Task {
	t.Helper()
	task := runtimePoolRetirementTask(t, pool, name)
	task.Spec.SessionRef = nil
	task.Status.Phase = corev1alpha1.TaskPhaseFailed
	e := task.Status.Execution
	e.State, e.Outcome = corev1alpha1.TaskExecutionStateFailed, corev1alpha1.TaskExecutionOutcomeFailed
	e.Reason, e.Message = "RuntimeSessionStartFailed", "runtime session failed to start"
	e.RuntimeSessionUID = name + "-runtime-session"
	e.RuntimeSessionSupervisorBootID = bootID
	e.RuntimeSessionProfileDigest = ""
	return task
}

func (f *nativePoolBootFixture) startFailedTask(t *testing.T, name, bootID string) *corev1alpha1.Task {
	t.Helper()
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	task := runtimePoolStartFailedTask(t, &pool, name, bootID)
	if err := f.r.Create(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	if err := f.r.Delete(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	return task
}

func (f *nativePoolBootFixture) assertRetiredTaskCleanup(t *testing.T, task *corev1alpha1.Task) {
	t.Helper()
	proofTask := task.DeepCopy()
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(task), task); err != nil {
		t.Fatal(err)
	}
	if !runtimeSessionCleanupCompleteForUID(task, task.UID) {
		t.Fatal("retired boot did not record cleanup for the Task whose RuntimeSession never started")
	}
	// The deleting Task's own cleanup path uses the read-only gate, which must
	// not depend on the receipt the pool already wrote.
	if retired, err := verifiedNativeRuntimePoolRetirement(t.Context(), f.control, proofTask, proofTask.UID); err != nil || !retired {
		t.Fatalf("read-only retirement gate for the deleted Task: %t %v", retired, err)
	}
	dispatcher := &ACPDispatcher{Client: f.r.Client, APIReader: f.r.APIReader, Store: f.control, Epochs: f.r.Epochs}
	if ready, err := dispatcher.reconcileRecoveredRuntimeSession(t.Context(), proofTask, proofTask.UID, true, nil); err != nil || !ready {
		t.Fatalf("deleted Task runtime cleanup: ready %t, err %v", ready, err)
	}
	pod := &corev1.Pod{}
	err := f.r.Get(t.Context(), client.ObjectKeyFromObject(&f.pod), pod)
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	if err == nil && controllerutil.ContainsFinalizer(pod, runtimePoolBootPodFinalizer) {
		t.Fatal("retired boot Pod kept its retention finalizer")
	}
}

func TestRuntimePoolBootScaleDownRetiresTaskWithoutSupervisorBoot(t *testing.T) {
	f := newNativePoolBootFixture(t, false)
	f.serve(t)
	task := f.startFailedTask(t, "start-failed-turn", "")
	current := runtimePoolTestGetPool(t, f.r, f.pool)
	current.Spec.DesiredReplicas = 0
	if err := f.r.Update(t.Context(), &current); err != nil {
		t.Fatal(err)
	}
	runtimePoolReconcile(t, f.r, f.pool)
	f.assertNoReceipt(t, task)
	f.supervisor.probe = runtimePoolValidProbe(f.pool, &f.pod, "native-boot", true)
	runtimePoolReconcile(t, f.r, f.pool)
	runtimePoolReconcile(t, f.r, f.pool)
	f.assertRetiredTaskCleanup(t, task)
	if err := f.r.Delete(t.Context(), &f.pod); err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	runtimePoolReconcile(t, f.r, f.pool)
	stopped := runtimePoolTestGetPool(t, f.r, f.pool)
	if stopped.Status.ActiveInstance != nil || stopped.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleStopped {
		t.Fatalf("pool after retiring the boot = %s, active %v", stopped.Status.Lifecycle, stopped.Status.ActiveInstance)
	}
}

func TestRuntimePoolBootTerminationRetiresTaskWithoutSupervisorBoot(t *testing.T) {
	f := newNativePoolBootFixture(t, false)
	f.serve(t)
	task := f.startFailedTask(t, "start-failed-turn", "")
	if err := f.r.Delete(t.Context(), &f.pod); err != nil {
		t.Fatal(err)
	}
	f.terminate(t, false)
	runtimePoolReconcile(t, f.r, f.pool)
	f.assertRetiredTaskCleanup(t, task)
}

func TestRuntimePoolBootRetirementStillRejectsAnotherSupervisorBoot(t *testing.T) {
	f := newNativePoolBootFixture(t, false)
	f.serve(t)
	task := f.startFailedTask(t, "other-boot-turn", "another-boot")
	current := runtimePoolTestGetPool(t, f.r, f.pool)
	status := runtimePoolValidProbe(f.pool, &f.pod, "native-boot", true).Status
	err := f.r.recordNativeRuntimePoolDrain(t.Context(), &current, current.Status.ActiveInstance, status)
	if err == nil || !strings.Contains(err.Error(), "does not match the exact retired RuntimePool boot") {
		t.Fatalf("Task frozen to another supervisor boot was accepted: %v", err)
	}
	f.assertNoReceipt(t, task)
	if retired, err := verifiedNativeRuntimePoolRetirement(t.Context(), f.control, task, task.UID); err == nil || retired {
		t.Fatalf("read-only gate accepted another supervisor boot: %t %v", retired, err)
	}
	pod := &corev1.Pod{}
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(&f.pod), pod); err != nil || !controllerutil.ContainsFinalizer(pod, runtimePoolBootPodFinalizer) {
		t.Fatalf("boot mismatch released the retained Pod: %v", err)
	}
}

func TestRuntimePoolDrainCleanupAcceptsTaskWithoutSupervisorBoot(t *testing.T) {
	pool := runtimePoolTestObject(1)
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolTestReconciler(t, runtimePoolTestScheme(t), supervisor, pool)
	_, pod := runtimePoolTestStartServing(t, r, pool, supervisor, "retiring-pod", "retiring-pod-uid", "10.0.0.81", "retiring-boot")
	current := runtimePoolTestGetPool(t, r, pool)
	unbooted := runtimePoolStartFailedTask(t, &current, "start-failed-turn", "")
	otherBoot := runtimePoolStartFailedTask(t, &current, "other-boot-turn", "another-boot")
	for _, task := range []*corev1alpha1.Task{unbooted, otherBoot} {
		if err := r.Create(t.Context(), task); err != nil {
			t.Fatal(err)
		}
	}
	status := runtimePoolValidProbe(pool, &pod, "retiring-boot", true).Status
	err := r.recordDrainedRuntimePoolTaskCleanup(t.Context(), &current, current.Status.ActiveInstance, status)
	if err == nil || !strings.Contains(err.Error(), "other-boot-turn lacks exact RuntimePool retirement authority") {
		t.Fatalf("Task frozen to another supervisor boot was accepted: %v", err)
	}
	// Remove the mismatched Task so the drain can record the remaining one.
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(otherBoot), otherBoot); err != nil {
		t.Fatal(err)
	}
	otherBoot.Finalizers = nil
	if err := r.Update(t.Context(), otherBoot); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(t.Context(), otherBoot); err != nil {
		t.Fatal(err)
	}
	if err := r.recordDrainedRuntimePoolTaskCleanup(t.Context(), &current, current.Status.ActiveInstance, status); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(unbooted), unbooted); err != nil {
		t.Fatal(err)
	}
	if !runtimeSessionCleanupCompleteForUID(unbooted, unbooted.UID) {
		t.Fatal("quiescent drain did not record cleanup for the Task whose RuntimeSession never started")
	}
}

func TestRuntimePoolTaskBootMatches(t *testing.T) {
	const podUID, bootID = "pod-uid", "boot-1"
	instanceID := runtimePoolRuntimeInstanceID(podUID, bootID)
	for _, tc := range []struct {
		name                   string
		taskInstance, taskBoot string
		instance, boot         string
		want                   bool
	}{
		{name: "recorded boot", taskInstance: instanceID, taskBoot: bootID, instance: instanceID, boot: bootID, want: true},
		{name: "no recorded boot", taskInstance: instanceID, instance: instanceID, boot: bootID, want: true},
		{name: "other recorded boot", taskInstance: instanceID, taskBoot: "boot-2", instance: instanceID, boot: bootID},
		{name: "other instance", taskInstance: runtimePoolRuntimeInstanceID(podUID, "boot-2"), instance: instanceID, boot: bootID},
		{name: "no recorded boot, instance not derived from boot", taskInstance: "pod-uid.boot-2", instance: "pod-uid.boot-2", boot: bootID},
		{name: "no recorded boot, no retired boot", taskInstance: "pod-uid.", instance: "pod-uid.", boot: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &corev1alpha1.TaskExecutionStatus{RuntimeInstanceID: tc.taskInstance, RuntimeSessionSupervisorBootID: tc.taskBoot}
			if got := runtimePoolTaskBootMatches(e, podUID, tc.instance, harnessv2.SupervisorBootID(tc.boot)); got != tc.want {
				t.Fatalf("runtimePoolTaskBootMatches = %t, want %t", got, tc.want)
			}
		})
	}
	if runtimePoolTaskBootMatches(nil, podUID, instanceID, bootID) {
		t.Fatal("missing execution status matched a boot")
	}
}
