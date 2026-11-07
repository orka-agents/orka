// Copyright (c) 2026. MIT License - see LICENSE file for details.

package controller

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	workspaceprovider "github.com/orka-agents/orka-workspace/sdk"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func suspendableNativeWorkspace(t *testing.T) func(*externalRuntimePoolFixture) {
	return func(f *externalRuntimePoolFixture) {
		t.Helper()
		workspace := f.currentWorkspace(t)
		workspace.Spec.Lifecycle.AllowedOnDetach = append(workspace.Spec.Lifecycle.AllowedOnDetach, workspacev1alpha1.WorkspaceOnDetachSuspend)
		workspace.Annotations[acpWorkspaceSuspendModeAnnotation] = "DataOnly"
		if err := f.r.Update(t.Context(), workspace); err != nil {
			t.Fatal(err)
		}
	}
}

// terminateNativeWorker models the kubelet's graceful termination of the
// exact worker: a finalizer keeps the Pod observable with its deletion time.
func terminateNativeWorker(t *testing.T, f *externalRuntimePoolFixture, worker corev1.Pod) {
	t.Helper()
	current := &corev1.Pod{}
	if err := f.r.APIReader.Get(t.Context(), client.ObjectKeyFromObject(&worker), current); err != nil {
		t.Fatal(err)
	}
	current.Finalizers = append(current.Finalizers, "test.orka.ai/observe-termination")
	if err := f.r.Update(t.Context(), current); err != nil {
		t.Fatal(err)
	}
	if err := f.r.Delete(t.Context(), current, client.GracePeriodSeconds(3600)); err != nil {
		t.Fatal(err)
	}
}

func evictionRecord(t *testing.T, f *externalRuntimePoolFixture) *externalRuntimeEviction {
	t.Helper()
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	record, err := externalRuntimeEvictionRecord(&pool)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func evictionCondition(t *testing.T, f *externalRuntimePoolFixture) *metav1.Condition {
	t.Helper()
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	return meta.FindStatusCondition(pool.Status.Conditions, corev1alpha1.RuntimePoolConditionWorkerEviction)
}

func quiescentEvictionProbe(f *externalRuntimePoolFixture, pod *corev1.Pod, reason string) RuntimePoolProbeResult {
	probe := runtimePoolValidProbe(f.pool, pod, "native-recycle-boot", true)
	probe.Status.Drain.Reason = reason
	return probe
}

// withActivePrompt models one accepted prompt still running on the supervisor.
func withActivePrompt(probe RuntimePoolProbeResult) RuntimePoolProbeResult {
	const sessionUID = harnessv2.RuntimeSessionUID("evicted-session-uid")
	probe.Status.Sessions = []harnessv2.RuntimeSessionStatus{{RuntimeSessionID: "evicted-session", RuntimeSessionUID: sessionUID, Generation: 1,
		State: harnessv2.RuntimeSessionStatePromptRunning, ActivePromptID: "accepted-prompt", LastTransitionAt: runtimePoolTestNow}}
	probe.Status.ActivePrompts = []harnessv2.ActivePromptStatus{{RuntimeSessionUID: sessionUID, SessionGeneration: 1, TaskUID: "task-uid", TaskAttempt: 1,
		PromptID: "accepted-prompt", LeaseExpiresAt: runtimePoolTestNow.Add(time.Minute), FrameSequence: 3, StartedAt: runtimePoolTestNow.Add(-time.Minute)}}
	probe.Status.Pressure.ResidentSessions, probe.Status.Pressure.ActivePrompts = 1, 1
	if err := probe.Status.Validate(); err != nil {
		panic(err)
	}
	return probe
}

func TestExternalNativeWorkerTerminationDrainsBeforeSuspendOnly(t *testing.T) {
	f, w, pod, worker := nativeRecycleReviewFixtureWith(t, suspendableNativeWorkspace(t))
	if !externalWorkspaceSuspendable(w) {
		t.Fatal("fixture workspace is not data-only suspendable")
	}
	terminateNativeWorker(t, f, worker)

	runtimePoolReconcile(t, f.r, f.pool)
	record := evictionRecord(t, f)
	if record == nil || !record.matches(f.currentWorkspace(t)) || record.Source != externalEvictionSourceWorker || !record.Suspendable {
		t.Fatalf("worker termination was not recorded for the exact instance: %#v", record)
	}
	if want := record.SignaledAt.Add(harnessv2.WorkerEvictionSuspendWindow); !record.Deadline.Equal(want) {
		t.Fatalf("eviction deadline = %s, want the worker shutdown budget %s", record.Deadline, want)
	}
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	if pool.Annotations[externalRuntimeRetirementRequestedAnnotation] != booleanTrueValue || pool.Status.AdmissionState == corev1alpha1.RuntimePoolAdmissionAccepting ||
		pool.Status.ActiveInstance == nil || pool.Status.ActiveInstance.PodUID != string(pod.UID) {
		t.Fatalf("eviction did not close admission while preserving the exact instance: %#v", pool.Status)
	}
	if condition := evictionCondition(t, f); condition == nil || condition.Status != metav1.ConditionTrue || condition.Reason != corev1alpha1.RuntimePoolReasonEvictionDraining {
		t.Fatalf("eviction condition = %#v", condition)
	}

	// The supervisor has not drained yet: Core requests a drain that announces
	// the planned Suspend, and authorizes nothing.
	for range 3 {
		runtimePoolReconcile(t, f.r, f.pool)
	}
	if f.supervisor.drainCalls == 0 || f.supervisor.drainReason != harnessv2.DrainReasonWorkspaceSuspend {
		t.Fatalf("eviction drain calls=%d reason=%q, want a workspace_suspend drain", f.supervisor.drainCalls, f.supervisor.drainReason)
	}
	if retirement := f.currentWorkspace(t).Spec.Retirement; retirement != nil {
		t.Fatalf("retirement authorized before drain settlement: %#v", retirement)
	}

	// An accepted prompt is still running: it must settle, never be replayed
	// or captured mid-flight.
	f.supervisor.probe = withActivePrompt(quiescentEvictionProbe(f, &pod, harnessv2.DrainReasonWorkerEviction))
	runtimePoolReconcile(t, f.r, f.pool)
	if retirement := f.currentWorkspace(t).Spec.Retirement; retirement != nil {
		t.Fatalf("Suspend authorized while an accepted prompt was running: %#v", retirement)
	}
	if message := runtimePoolTestGetPool(t, f.r, f.pool).Status.Message; message != "waiting for runtime drain settlement" {
		t.Fatalf("busy evicted runtime was not held at drain settlement: %q", message)
	}

	f.supervisor.probe = quiescentEvictionProbe(f, &pod, harnessv2.DrainReasonWorkerEviction)
	runtimePoolReconcile(t, f.r, f.pool)
	current := f.currentWorkspace(t)
	retirement := current.Spec.Retirement
	if retirement == nil || retirement.Action != workspacev1alpha1.WorkloadRetirementSuspend || retirement.Identity != current.Status.Allocation.Identity || retirement.Sequence != current.Spec.Workload.Sequence {
		t.Fatalf("quiescent evicted runtime did not earn an exact Suspend: %#v", retirement)
	}
	if current.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady {
		t.Fatal("eviction changed the workspace's desired state")
	}
}

func TestExternalNativeEvictionNeverAuthorizesSuspendAfterDeadline(t *testing.T) {
	f, _, pod, worker := nativeRecycleReviewFixtureWith(t, suspendableNativeWorkspace(t))
	terminateNativeWorker(t, f, worker)
	runtimePoolReconcile(t, f.r, f.pool)
	record := evictionRecord(t, f)
	if record == nil {
		t.Fatal("eviction was not recorded")
	}
	f.r.Now = func() time.Time { return record.Deadline }
	f.supervisor.probe = quiescentEvictionProbe(f, &pod, harnessv2.DrainReasonWorkerEviction)
	for range 4 {
		runtimePoolReconcile(t, f.r, f.pool)
	}
	if retirement := f.currentWorkspace(t).Spec.Retirement; retirement != nil {
		t.Fatalf("Suspend authorized at the eviction deadline: %#v", retirement)
	}
	if condition := evictionCondition(t, f); condition == nil || condition.Reason != corev1alpha1.RuntimePoolReasonEvictionDeadlineExceeded {
		t.Fatalf("deadline condition = %#v", condition)
	}

	// The provider later reports the killed instance as failed: Core retires
	// it with Stop, never Suspend, and the failure is terminal.
	w := f.currentWorkspace(t)
	w.Status.State = workspacev1alpha1.ExecutionWorkspaceStateFailed
	if err := f.r.Status().Update(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		runtimePoolReconcile(t, f.r, f.pool)
	}
	if retirement := f.currentWorkspace(t).Spec.Retirement; retirement == nil || retirement.Action != workspacev1alpha1.WorkloadRetirementStop {
		t.Fatalf("failed evicted instance retirement = %#v, want Stop", retirement)
	}
}

func TestExternalNativeEvictionOfNonSuspendableWorkspaceKeepsLostInstanceSemantics(t *testing.T) {
	f, _, _, worker := nativeRecycleReviewFixture(t)
	terminateNativeWorker(t, f, worker)
	runtimePoolReconcile(t, f.r, f.pool)
	record := evictionRecord(t, f)
	if record == nil || record.Suspendable {
		t.Fatalf("non-suspendable eviction record = %#v", record)
	}
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	if pool.Annotations[externalRuntimeRetirementRequestedAnnotation] != "" || pool.Status.AdmissionState == corev1alpha1.RuntimePoolAdmissionAccepting {
		t.Fatalf("non-suspendable eviction requested retirement or kept admission open: %#v", pool.Status)
	}
	if condition := evictionCondition(t, f); condition == nil || condition.Reason != corev1alpha1.RuntimePoolReasonEvictionNotSuspendable {
		t.Fatalf("non-suspendable eviction condition = %#v", condition)
	}
	if f.currentWorkspace(t).Spec.Retirement != nil {
		t.Fatal("non-suspendable eviction authorized retirement")
	}
}

func TestExternalNativeSupervisorEvictionSignalStartsEviction(t *testing.T) {
	f, _, pod, _ := nativeRecycleReviewFixtureWith(t, suspendableNativeWorkspace(t))
	probe := runtimePoolValidProbe(f.pool, &pod, "native-recycle-boot", true)
	signaledAt := runtimePoolTestNow.Add(-2 * time.Minute)
	probe.Status.Drain.Reason = harnessv2.DrainReasonWorkerEviction
	probe.Status.Drain.RequestedAt = signaledAt
	f.supervisor.probe = withActivePrompt(probe)
	runtimePoolReconcile(t, f.r, f.pool)
	record := evictionRecord(t, f)
	if record == nil || record.Source != externalEvictionSourceSupervisor || !record.SignaledAt.Equal(signaledAt) ||
		!record.Deadline.Equal(signaledAt.Add(harnessv2.WorkerEvictionSuspendWindow)) {
		t.Fatalf("supervisor eviction record = %#v", record)
	}
	if pool := runtimePoolTestGetPool(t, f.r, f.pool); pool.Annotations[externalRuntimeRetirementRequestedAnnotation] != booleanTrueValue {
		t.Fatal("supervisor eviction did not request exact retirement")
	}

	// A supervisor clock ahead of Core never extends the window.
	f2, _, pod2, _ := nativeRecycleReviewFixtureWith(t, suspendableNativeWorkspace(t))
	future := runtimePoolValidProbe(f2.pool, &pod2, "native-recycle-boot", true)
	future.Status.Drain.Reason = harnessv2.DrainReasonWorkerEviction
	future.Status.Drain.RequestedAt = runtimePoolTestNow.Add(time.Hour)
	f2.supervisor.probe = future
	runtimePoolReconcile(t, f2.r, f2.pool)
	if record := evictionRecord(t, f2); record == nil || !record.Deadline.Equal(runtimePoolTestNow.Add(harnessv2.WorkerEvictionSuspendWindow)) {
		t.Fatalf("future supervisor signal extended the eviction window: %#v", record)
	}
}

func TestExternalNativeEvictionKeepsEarliestSignalAndShortestWindow(t *testing.T) {
	f, w, _, _ := nativeRecycleReviewFixtureWith(t, suspendableNativeWorkspace(t))
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	early := runtimePoolTestNow.Add(-10 * time.Minute)
	if _, err := f.r.beginExternalRuntimeEviction(t.Context(), &pool, w, externalEvictionSourceSupervisor, early, early.Add(harnessv2.WorkerEvictionSuspendWindow)); err != nil {
		t.Fatal(err)
	}
	pool = runtimePoolTestGetPool(t, f.r, f.pool)
	spot := runtimePoolTestNow.Add(15 * time.Second)
	if _, err := f.r.beginExternalRuntimeEviction(t.Context(), &pool, w, externalEvictionSourceWorker, runtimePoolTestNow, spot); err != nil {
		t.Fatal(err)
	}
	record := evictionRecord(t, f)
	if record == nil || !record.SignaledAt.Equal(early) || !record.Deadline.Equal(spot) || record.Source != externalEvictionSourceSupervisor {
		t.Fatalf("merged eviction record = %#v", record)
	}
}

func TestExternalRuntimeEvictionRecordBindsExactInstance(t *testing.T) {
	f, w, _, _ := nativeRecycleReviewFixtureWith(t, suspendableNativeWorkspace(t))
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	stale := externalRuntimeEviction{WorkspaceUID: w.UID, Sequence: w.Status.Allocation.Sequence + 1, Identity: w.Status.Allocation.Identity,
		Source: externalEvictionSourceWorker, SignaledAt: runtimePoolTestNow.Add(-time.Hour), Deadline: runtimePoolTestNow.Add(-time.Minute), Suspendable: true}
	encoded, err := json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	pool.Annotations[externalRuntimeEvictionAnnotation] = string(encoded)
	if f.r.externalEvictionDeadlineExceeded(&pool, w) {
		t.Fatal("an eviction record for another instance gated this instance")
	}
	pool.Annotations[externalRuntimeEvictionAnnotation] = "{not json"
	if !f.r.externalEvictionDeadlineExceeded(&pool, w) {
		t.Fatal("a malformed eviction record did not fail closed")
	}
}

func TestExternalWorkerEvictionSignal(t *testing.T) {
	now := runtimePoolTestNow
	deleting := metav1.NewTime(now.Add(time.Hour))
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &deleting, DeletionGracePeriodSeconds: ptr.To[int64](3600)}}
	signaledAt, deadline, evicting := externalWorkerEvictionSignal(pod, now.Add(time.Minute))
	if !evicting || !signaledAt.Equal(now) || !deadline.Equal(now.Add(harnessv2.WorkerEvictionSuspendWindow)) {
		t.Fatalf("pod deletion signal = %s, %s, %t", signaledAt, deadline, evicting)
	}
	spot := metav1.NewTime(now.Add(15 * time.Second))
	pod = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &spot, DeletionGracePeriodSeconds: ptr.To[int64](15)}}
	if _, deadline, evicting := externalWorkerEvictionSignal(pod, now); !evicting || !deadline.Equal(spot.Time) {
		t.Fatalf("short grace deadline = %s, want the Pod deadline %s", deadline, spot.Time)
	}
	pod = &corev1.Pod{Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: podConditionDisruptionTarget, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(now)}}}}
	if signaledAt, deadline, evicting := externalWorkerEvictionSignal(pod, now.Add(time.Minute)); !evicting || !signaledAt.Equal(now) || !deadline.Equal(now.Add(harnessv2.WorkerEvictionSuspendWindow)) {
		t.Fatalf("disruption target signal = %s, %s, %t", signaledAt, deadline, evicting)
	}
	if _, _, evicting := externalWorkerEvictionSignal(&corev1.Pod{}, now); evicting {
		t.Fatal("a running worker was reported as evicting")
	}
}

// Admission never reaches a terminating worker; settlement of accepted work
// and exact retirement still can.
func TestExternalTerminatingWorkerIsReachableOnlyForCleanup(t *testing.T) {
	f, w, _, worker := nativeRecycleReviewFixtureWith(t, suspendableNativeWorkspace(t))
	terminateNativeWorker(t, f, worker)
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	_, err := f.r.attestExternalWorkspaceStartup(t.Context(), w.Spec.Workload, w.Status.Allocation.Startup)
	var eviction *externalWorkerEvictionError
	if !errors.As(err, &eviction) || !errors.Is(err, workspaceprovider.ErrStaleIdentity) {
		t.Fatalf("admission attestation of a terminating worker = %v", err)
	}
	if _, err := runtimePoolWorkspaceStartupPod(t.Context(), f.r.APIReader, &pool); err == nil {
		t.Fatal("admission endpoint resolved to a terminating worker")
	}
	if _, err := runtimePoolWorkspaceCleanupPod(t.Context(), f.r.APIReader, &pool); err != nil {
		t.Fatalf("cleanup endpoint could not reach the terminating worker: %v", err)
	}
	if _, err := f.r.attestExternalRetiringWorkspaceStartup(t.Context(), w.Spec.Workload, w.Status.Allocation.Startup); err != nil {
		t.Fatalf("retirement attestation rejected the exact terminating worker: %v", err)
	}
}

func TestExternalEvictionSettlesAsSuspendedOnlyWithRetainedData(t *testing.T) {
	for _, test := range []struct {
		name     string
		retained bool
		failed   bool
		want     string
	}{
		{name: "suspended", retained: true, want: corev1alpha1.RuntimePoolReasonEvictionSuspended},
		{name: "no retained data", want: corev1alpha1.RuntimePoolReasonEvictionDeadlineExceeded},
		{name: "failed", retained: true, failed: true, want: corev1alpha1.RuntimePoolReasonEvictionDeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, w, _, _ := nativeRecycleReviewFixtureWith(t, suspendableNativeWorkspace(t))
			pool := runtimePoolTestGetPool(t, f.r, f.pool)
			if _, err := f.r.beginExternalRuntimeEviction(t.Context(), &pool, w, externalEvictionSourceWorker, runtimePoolTestNow, runtimePoolTestNow.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			w.Status.Allocation.State = workspacev1alpha1.AllocationStopped
			w.Status.Allocation.Startup = nil
			if test.retained {
				w.Status.Allocation.RetainedData = &workspacev1alpha1.RetainedDataReference{ID: "retained", SourceInstance: w.Status.Allocation.Identity, ProofSHA256: "sha256:0000000000000000000000000000000000000000000000000000000000000000"}
			}
			if test.failed {
				w.Status.State = workspacev1alpha1.ExecutionWorkspaceStateFailed
			}
			pool = runtimePoolTestGetPool(t, f.r, f.pool)
			if _, err := f.r.externalPoolStopped(t.Context(), &pool, w); err != nil {
				t.Fatal(err)
			}
			pool = runtimePoolTestGetPool(t, f.r, f.pool)
			if pool.Annotations[externalRuntimeEvictionAnnotation] != "" || pool.Annotations[externalRuntimeRetirementRequestedAnnotation] != "" {
				t.Fatal("stopped pool kept its eviction or retirement record")
			}
			condition := meta.FindStatusCondition(pool.Status.Conditions, corev1alpha1.RuntimePoolConditionWorkerEviction)
			if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != test.want {
				t.Fatalf("settled eviction condition = %#v, want %s", condition, test.want)
			}
		})
	}
}

// startRetirementDrain begins an exact retirement for a reason other than
// eviction and returns once Core has requested the authenticated drain.
func startRetirementDrain(t *testing.T, f *externalRuntimePoolFixture) {
	t.Helper()
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	pool.Annotations[externalRuntimeRetirementRequestedAnnotation] = booleanTrueValue
	if err := f.r.Update(t.Context(), &pool); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		runtimePoolReconcile(t, f.r, f.pool)
	}
	if f.supervisor.drainCalls == 0 || f.currentWorkspace(t).Spec.Retirement != nil {
		t.Fatal("retirement did not reach the authenticated drain")
	}
}

// Termination that begins while a retirement drain is already running still
// records its window, and Suspend is refused once that window has passed.
func TestExternalEvictionDuringRetirementDrainIsRecordedAndGated(t *testing.T) {
	f, _, pod, _ := nativeRecycleReviewFixtureWith(t, suspendableNativeWorkspace(t))
	startRetirementDrain(t, f)
	if record := evictionRecord(t, f); record != nil {
		t.Fatalf("eviction recorded before any signal: %#v", record)
	}
	late := quiescentEvictionProbe(f, &pod, harnessv2.DrainReasonWorkerEviction)
	late.Status.Drain.RequestedAt = runtimePoolTestNow.Add(-harnessv2.WorkerEvictionSuspendWindow - time.Minute)
	f.supervisor.probe = late
	runtimePoolReconcile(t, f.r, f.pool)
	record := evictionRecord(t, f)
	if record == nil || record.Source != externalEvictionSourceSupervisor || !record.Deadline.Equal(late.Status.Drain.RequestedAt.Add(harnessv2.WorkerEvictionSuspendWindow)) {
		t.Fatalf("eviction during retirement drain = %#v", record)
	}
	for range 2 {
		runtimePoolReconcile(t, f.r, f.pool)
	}
	if retirement := f.currentWorkspace(t).Spec.Retirement; retirement != nil {
		t.Fatalf("Suspend authorized after an eviction window observed during the drain: %#v", retirement)
	}
	if condition := evictionCondition(t, f); condition == nil || condition.Reason != corev1alpha1.RuntimePoolReasonEvictionDeadlineExceeded {
		t.Fatalf("deadline condition = %#v", condition)
	}
}

// The authenticated probe can outlast a short window; the deadline is
// rechecked immediately before Suspend is authorized.
func TestExternalEvictionDeadlineIsRecheckedAfterTheDrainProbe(t *testing.T) {
	f, w, pod, _ := nativeRecycleReviewFixtureWith(t, suspendableNativeWorkspace(t))
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	deadline := runtimePoolTestNow.Add(time.Minute)
	if _, err := f.r.beginExternalRuntimeEviction(t.Context(), &pool, w, externalEvictionSourceWorker, runtimePoolTestNow, deadline); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		runtimePoolReconcile(t, f.r, f.pool)
	}
	f.supervisor.probe = quiescentEvictionProbe(f, &pod, harnessv2.DrainReasonWorkspaceSuspend)
	f.supervisor.afterProbe = func() { f.r.Now = func() time.Time { return deadline } }
	runtimePoolReconcile(t, f.r, f.pool)
	if retirement := f.currentWorkspace(t).Spec.Retirement; retirement != nil {
		t.Fatalf("Suspend authorized after the deadline passed during the probe: %#v", retirement)
	}
	if condition := evictionCondition(t, f); condition == nil || condition.Reason != corev1alpha1.RuntimePoolReasonEvictionDeadlineExceeded {
		t.Fatalf("deadline condition = %#v", condition)
	}
}

// Termination first observed during a retirement drain that will suspend is
// reported as draining, replacing an earlier instance's settled result, so the
// suspension it ends in is reported as EvictionSuspended.
func TestExternalEvictionDuringSuspendRetirementReportsDrainingAndSettles(t *testing.T) {
	f, _, pod, _ := nativeRecycleReviewFixtureWith(t, suspendableNativeWorkspace(t))
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	f.r.setRuntimePoolCondition(&pool, &pool.Status, corev1alpha1.RuntimePoolConditionWorkerEviction, metav1.ConditionFalse,
		corev1alpha1.RuntimePoolReasonEvictionDeadlineExceeded, "an earlier instance was lost")
	if err := f.r.Status().Update(t.Context(), &pool); err != nil {
		t.Fatal(err)
	}
	startRetirementDrain(t, f)
	probe := quiescentEvictionProbe(f, &pod, harnessv2.DrainReasonWorkerEviction)
	probe.Status.Drain.RequestedAt = runtimePoolTestNow
	f.supervisor.probe = probe
	runtimePoolReconcile(t, f.r, f.pool)
	if record := evictionRecord(t, f); record == nil || record.Source != externalEvictionSourceSupervisor {
		t.Fatalf("eviction during retirement drain = %#v", record)
	}
	if condition := evictionCondition(t, f); condition == nil || condition.Status != metav1.ConditionTrue || condition.Reason != corev1alpha1.RuntimePoolReasonEvictionDraining {
		t.Fatalf("eviction observed during a Suspend retirement condition = %#v, want EvictionDraining", condition)
	}
	for range 2 {
		runtimePoolReconcile(t, f.r, f.pool)
	}
	if condition := evictionCondition(t, f); condition == nil || condition.Status != metav1.ConditionTrue || condition.Reason != corev1alpha1.RuntimePoolReasonEvictionDraining {
		t.Fatalf("later drain status writes replaced the eviction condition: %#v", condition)
	}
	w := f.currentWorkspace(t)
	if retirement := w.Spec.Retirement; retirement == nil || retirement.Action != workspacev1alpha1.WorkloadRetirementSuspend {
		t.Fatalf("quiescent retirement drain did not earn Suspend: %#v", retirement)
	}
	w.Status.Allocation.State = workspacev1alpha1.AllocationStopped
	w.Status.Allocation.Startup = nil
	w.Status.Allocation.RetainedData = &workspacev1alpha1.RetainedDataReference{ID: "retained", SourceInstance: w.Status.Allocation.Identity, ProofSHA256: "sha256:0000000000000000000000000000000000000000000000000000000000000000"}
	pool = runtimePoolTestGetPool(t, f.r, f.pool)
	if _, err := f.r.externalPoolStopped(t.Context(), &pool, w); err != nil {
		t.Fatal(err)
	}
	if condition := evictionCondition(t, f); condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != corev1alpha1.RuntimePoolReasonEvictionSuspended {
		t.Fatalf("settled eviction condition = %#v, want EvictionSuspended", condition)
	}
}
