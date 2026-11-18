/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	orkametrics "github.com/orka-agents/orka/internal/metrics"
)

func TestExternalRuntimeMilestoneRoundTripIsBoundToItsSequence(t *testing.T) {
	at := time.Date(2026, 11, 17, 10, 0, 0, 123456789, time.UTC)
	raw := formatExternalRuntimeMilestone(3, at)
	if got, ok := parseExternalRuntimeMilestone(raw, 3); !ok || !got.Equal(at) {
		t.Fatalf("parse(%q) = %s, %v", raw, got, ok)
	}
	for _, test := range []struct {
		raw      string
		sequence int64
	}{
		{raw, 4}, {"", 3}, {"3", 3}, {"x " + at.Format(time.RFC3339Nano), 3}, {"3 yesterday", 3},
	} {
		if _, ok := parseExternalRuntimeMilestone(test.raw, test.sequence); ok {
			t.Fatalf("parse(%q, %d) accepted", test.raw, test.sequence)
		}
	}
}

func TestExternalRuntimeStartPath(t *testing.T) {
	for want, request := range map[string]workspacev1alpha1.WorkloadRequest{
		externalRuntimePathCold:        {Sequence: 1},
		externalRuntimePathRestore:     {Sequence: 1, RestoreFrom: &workspacev1alpha1.WorkloadCheckpointReference{}},
		externalRuntimePathResume:      {Sequence: 2, RetainedData: &workspacev1alpha1.RetainedDataReference{}},
		externalRuntimePathReplacement: {Sequence: 2},
	} {
		if got := externalRuntimeStartPath(&request); got != want {
			t.Fatalf("path(%+v) = %s, want %s", request, got, want)
		}
	}
}

func phaseDurations(phases []externalRuntimePhase) map[string]time.Duration {
	durations := map[string]time.Duration{}
	for _, phase := range phases {
		durations[phase.name] = phase.duration
	}
	return durations
}

func TestExternalRuntimeStartPhases(t *testing.T) {
	published := time.Unix(1000, 0).UTC()
	pool := &corev1alpha1.RuntimePool{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(published.Add(-2 * time.Second))}}
	w := &workspacev1alpha1.ExecutionWorkspace{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{externalWorkloadPublishedAnnotation: formatExternalRuntimeMilestone(1, published)}},
		Spec:       workspacev1alpha1.ExecutionWorkspaceSpec{Workload: &workspacev1alpha1.WorkloadRequest{Sequence: 1}},
		Status: workspacev1alpha1.ExecutionWorkspaceStatus{Conditions: []metav1.Condition{{
			Type: string(workspacev1alpha1.ConditionWorkspaceDataPlaneReady), Status: metav1.ConditionTrue, LastTransitionTime: metav1.NewTime(published.Add(5 * time.Second)),
		}}},
	}
	got := phaseDurations(externalRuntimeStartPhases(pool, w, published.Add(8*time.Second)))
	want := map[string]time.Duration{
		externalRuntimePhaseProvision: 2 * time.Second, externalRuntimePhaseProviderMaterialization: 5 * time.Second,
		externalRuntimePhaseBootstrapAndProbe: 3 * time.Second, externalRuntimePhaseStartTotal: 8 * time.Second,
	}
	if len(got) != len(want) {
		t.Fatalf("phases = %v, want %v", got, want)
	}
	for phase, duration := range want {
		if got[phase] != duration {
			t.Fatalf("phases = %v, want %v", got, want)
		}
	}

	// A later sequence has no provision phase, and a DataPlaneReady report
	// from before this sequence was published does not split the start.
	w.Spec.Workload.Sequence = 2
	w.Annotations[externalWorkloadPublishedAnnotation] = formatExternalRuntimeMilestone(2, published.Add(time.Minute))
	got = phaseDurations(externalRuntimeStartPhases(pool, w, published.Add(time.Minute+4*time.Second)))
	if len(got) != 1 || got[externalRuntimePhaseStartTotal] != 4*time.Second {
		t.Fatalf("later sequence phases = %v", got)
	}

	// A condition time keeps whole seconds, so readiness in the publication's
	// own second still splits the start, while readiness from an earlier second
	// predates the sequence.
	subsecond := published.Add(time.Minute + 700*time.Millisecond)
	w.Annotations[externalWorkloadPublishedAnnotation] = formatExternalRuntimeMilestone(2, subsecond)
	w.Status.Conditions[0].LastTransitionTime = metav1.NewTime(subsecond.Add(200 * time.Millisecond))
	raw, err := json.Marshal(w.Status.Conditions[0])
	if err != nil {
		t.Fatal(err)
	}
	var stored metav1.Condition
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	w.Status.Conditions[0] = stored
	got = phaseDurations(externalRuntimeStartPhases(pool, w, subsecond.Add(3*time.Second)))
	if len(got) != 3 || got[externalRuntimePhaseProviderMaterialization] != 0 || got[externalRuntimePhaseBootstrapAndProbe] != 3*time.Second ||
		got[externalRuntimePhaseStartTotal] != 3*time.Second {
		t.Fatalf("second-precision readiness phases = %v", got)
	}
	w.Status.Conditions[0].LastTransitionTime = metav1.NewTime(subsecond.Truncate(time.Second).Add(-time.Second))
	got = phaseDurations(externalRuntimeStartPhases(pool, w, subsecond.Add(3*time.Second)))
	if len(got) != 1 || got[externalRuntimePhaseStartTotal] != 3*time.Second {
		t.Fatalf("earlier-second readiness phases = %v", got)
	}

	// A milestone from another sequence reports nothing.
	w.Annotations[externalWorkloadPublishedAnnotation] = formatExternalRuntimeMilestone(1, published)
	if phases := externalRuntimeStartPhases(pool, w, published.Add(time.Hour)); phases != nil {
		t.Fatalf("stale milestone phases = %v", phases)
	}
}

func TestExternalRuntimeStopPhases(t *testing.T) {
	requested := time.Unix(2000, 0).UTC()
	w := &workspacev1alpha1.ExecutionWorkspace{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
			externalRuntimeStopRequestedAnnotation: formatExternalRuntimeMilestone(2, requested),
			externalRetirementAuthorizedAnnotation: formatExternalRuntimeMilestone(2, requested.Add(4*time.Second)),
			// Cold Suspend settlement stamps the detach after the drain ended.
			acpWorkspaceLastDetachedAnnotation: requested.Add(5 * time.Second).Format(time.RFC3339Nano),
		}},
		Spec: workspacev1alpha1.ExecutionWorkspaceSpec{Retirement: &workspacev1alpha1.WorkloadRetirement{Sequence: 2}},
	}
	for _, action := range []workspacev1alpha1.WorkloadRetirementAction{
		workspacev1alpha1.WorkloadRetirementSuspend, workspacev1alpha1.WorkloadRetirementStop, workspacev1alpha1.WorkloadRetirementDelete,
	} {
		w.Spec.Retirement.Action = action
		got := phaseDurations(externalRuntimeStopPhases(w, requested.Add(10*time.Second)))
		if len(got) != 3 || got[externalRuntimePhaseDrain] != 4*time.Second || got[externalRuntimePhaseProviderRetirement] != 6*time.Second ||
			got[externalRuntimePhaseStopTotal] != 10*time.Second {
			t.Fatalf("%s phases = %v", action, got)
		}
	}

	// A detach before the stop began, as with a warm window, is not its start.
	w.Annotations[acpWorkspaceLastDetachedAnnotation] = requested.Add(-time.Minute).Format(time.RFC3339Nano)
	if got := phaseDurations(externalRuntimeStopPhases(w, requested.Add(10*time.Second))); got[externalRuntimePhaseDrain] != 4*time.Second {
		t.Fatalf("warm-window phases = %v", got)
	}

	// Without a start for this sequence only the provider's part is known.
	for _, raw := range []string{"", formatExternalRuntimeMilestone(1, requested), formatExternalRuntimeMilestone(2, requested.Add(5*time.Second))} {
		w.Annotations[externalRuntimeStopRequestedAnnotation] = raw
		got := phaseDurations(externalRuntimeStopPhases(w, requested.Add(10*time.Second)))
		if len(got) != 1 || got[externalRuntimePhaseProviderRetirement] != 6*time.Second {
			t.Fatalf("phases with stop start %q = %v", raw, got)
		}
	}
}

func TestExternalRuntimeTimingReportsObserveRetirementStart(t *testing.T) {
	var reports externalRuntimeTimingReports
	at := time.Unix(3000, 0).UTC()
	// A pool this process first sees already retiring has no known start.
	reports.observeRetirement("restarted", 1, true, at)
	reports.observeRetirement("restarted", 1, true, at.Add(time.Second))
	if _, ok := reports.retirementRequested("restarted", 1); ok {
		t.Fatal("retirement under way before the first look was given a start")
	}

	reports.observeRetirement("pool", 1, false, at)
	reports.observeRetirement("pool", 1, true, at.Add(time.Second))
	reports.observeRetirement("pool", 1, true, at.Add(2*time.Second))
	if got, ok := reports.retirementRequested("pool", 1); !ok || !got.Equal(at.Add(time.Second)) {
		t.Fatalf("retirement start = %s, %v, want the first observation", got, ok)
	}
	if _, ok := reports.retirementRequested("pool", 2); ok {
		t.Fatal("another sequence shares the retirement start")
	}
	// A retirement that is cancelled before authorization starts afresh.
	reports.observeRetirement("pool", 1, false, at.Add(3*time.Second))
	if _, ok := reports.retirementRequested("pool", 1); ok {
		t.Fatal("cancelled retirement kept its start")
	}
	reports.observeRetirement("pool", 1, true, at.Add(4*time.Second))
	if got, ok := reports.retirementRequested("pool", 1); !ok || !got.Equal(at.Add(4*time.Second)) {
		t.Fatalf("later retirement start = %s, %v", got, ok)
	}
	// A new sequence retiring without a pass in between gets its own start.
	reports.observeRetirement("pool", 2, true, at.Add(5*time.Second))
	if got, ok := reports.retirementRequested("pool", 2); !ok || !got.Equal(at.Add(5*time.Second)) {
		t.Fatalf("next sequence start = %s, %v", got, ok)
	}
}

func TestStampExternalRuntimeStopRequestedOnlyOnFirstAuthorization(t *testing.T) {
	r := &RuntimePoolReconciler{}
	requested := time.Unix(4000, 0).UTC()
	r.externalTimings.observeRetirement("pool", 3, false, requested)
	r.externalTimings.observeRetirement("pool", 3, true, requested)
	w := &workspacev1alpha1.ExecutionWorkspace{Spec: workspacev1alpha1.ExecutionWorkspaceSpec{
		Retirement: &workspacev1alpha1.WorkloadRetirement{Sequence: 3, Action: workspacev1alpha1.WorkloadRetirementSuspend},
	}}
	r.stampExternalRuntimeStopRequested(w, nil, "pool", requested.Add(2*time.Second), false)
	stamped := formatExternalRuntimeMilestone(3, requested)
	if w.Annotations[externalRuntimeStopRequestedAnnotation] != stamped {
		t.Fatalf("stop start = %q", w.Annotations[externalRuntimeStopRequestedAnnotation])
	}
	// Deletion superseding a Suspend that has not stopped the instance keeps
	// the start of the one stop.
	previous := w.Spec.Retirement.DeepCopy()
	w.Spec.Retirement.Action = workspacev1alpha1.WorkloadRetirementDelete
	r.externalTimings.observeRetirement("pool", 3, false, requested.Add(time.Minute))
	r.stampExternalRuntimeStopRequested(w, previous, "pool", requested.Add(time.Hour), false)
	if w.Annotations[externalRuntimeStopRequestedAnnotation] != stamped {
		t.Fatalf("unfinished re-authorization stop start = %q", w.Annotations[externalRuntimeStopRequestedAnnotation])
	}
	// A Delete that follows a finished Suspend of the same sequence did not drain.
	r.stampExternalRuntimeStopRequested(w, previous, "pool", requested.Add(time.Hour), true)
	if raw, present := w.Annotations[externalRuntimeStopRequestedAnnotation]; present {
		t.Fatalf("re-authorization after the stop kept stop start %q", raw)
	}
	// An unknown start leaves no stale one behind.
	w.Annotations[externalRuntimeStopRequestedAnnotation] = stamped
	w.Spec.Retirement.Sequence = 4
	r.stampExternalRuntimeStopRequested(w, nil, "pool", requested.Add(time.Hour), false)
	if raw, present := w.Annotations[externalRuntimeStopRequestedAnnotation]; present {
		t.Fatalf("unknown start kept %q", raw)
	}
}

func TestPhaseLogKey(t *testing.T) {
	for phase, want := range map[string]string{
		externalRuntimePhaseProviderMaterialization: "providerMaterializationMs",
		externalRuntimePhaseStartTotal:              "startTotalMs",
		externalRuntimePhaseDrain:                   "drainMs",
	} {
		if got := phaseLogKey(phase); got != want {
			t.Fatalf("phaseLogKey(%q) = %q, want %q", phase, got, want)
		}
	}
}

func runtimePhaseSamples(t *testing.T, path, phase string) uint64 {
	t.Helper()
	metric := &dto.Metric{}
	observer := orkametrics.ACPWorkspaceRuntimePhaseDuration.WithLabelValues(path, phase)
	if err := observer.(prometheus.Metric).Write(metric); err != nil {
		t.Fatal(err)
	}
	return metric.GetHistogram().GetSampleCount()
}

type runtimeTimingClock struct{ now time.Time }

func (c *runtimeTimingClock) Now() time.Time { return c.now }

// A real start and stop of an execution-workspace runtime stamp their
// milestones in the writes Core already makes, and record each phase once.
func TestExternalRuntimePoolRecordsStartAndStopPhases(t *testing.T) {
	f := newExternalRuntimePoolFixture(t)
	clock := &runtimeTimingClock{now: time.Now().UTC()}
	f.r.Now = clock.Now
	startTotal := runtimePhaseSamples(t, externalRuntimePathCold, externalRuntimePhaseStartTotal)
	materialization := runtimePhaseSamples(t, externalRuntimePathCold, externalRuntimePhaseProviderMaterialization)
	retirement := runtimePhaseSamples(t, "stop", externalRuntimePhaseProviderRetirement)
	drain := runtimePhaseSamples(t, "stop", externalRuntimePhaseDrain)
	stopTotal := runtimePhaseSamples(t, "stop", externalRuntimePhaseStopTotal)

	workspace, pod := f.materialize(t)
	published, ok := parseExternalRuntimeMilestone(workspace.Annotations[externalWorkloadPublishedAnnotation], 1)
	if !ok || !published.Equal(clock.now) {
		t.Fatalf("published milestone = %q", workspace.Annotations[externalWorkloadPublishedAnnotation])
	}
	clock.now = clock.now.Add(5 * time.Second)
	workspace.Status.Conditions = append(workspace.Status.Conditions, metav1.Condition{
		Type: string(workspacev1alpha1.ConditionWorkspaceDataPlaneReady), Status: metav1.ConditionTrue, Reason: "Ready",
		ObservedGeneration: workspace.Generation, LastTransitionTime: metav1.NewTime(clock.now),
	})
	if err := f.r.Status().Update(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	clock.now = clock.now.Add(2 * time.Second)
	runtimePoolReconcile(t, f.r, f.pool)
	if status := runtimePoolTestGetPool(t, f.r, f.pool).Status; status.AdmissionState != corev1alpha1.RuntimePoolAdmissionAccepting {
		t.Fatalf("runtime did not serve: %s", status.Message)
	}
	runtimePoolReconcile(t, f.r, f.pool)
	if got := runtimePhaseSamples(t, externalRuntimePathCold, externalRuntimePhaseStartTotal) - startTotal; got != 1 {
		t.Fatalf("start_total samples = %d, want one per started instance", got)
	}
	if got := runtimePhaseSamples(t, externalRuntimePathCold, externalRuntimePhaseProviderMaterialization) - materialization; got != 1 {
		t.Fatalf("provider_materialization samples = %d", got)
	}

	// Drain and stop the instance.
	current := runtimePoolTestGetPool(t, f.r, f.pool)
	current.Spec.DesiredReplicas = 0
	if err := f.r.Update(context.Background(), &current); err != nil {
		t.Fatal(err)
	}
	runtimePoolReconcile(t, f.r, f.pool)
	runtimePoolReconcile(t, f.r, f.pool)
	f.supervisor.probe = runtimePoolValidProbe(f.pool, &pod, "external-boot", true)
	clock.now = clock.now.Add(3 * time.Second)
	runtimePoolReconcile(t, f.r, f.pool)
	workspace = f.currentWorkspace(t)
	if workspace.Spec.Retirement == nil {
		t.Fatal("drained runtime was not authorized to retire")
	}
	if authorized, ok := parseExternalRuntimeMilestone(workspace.Annotations[externalRetirementAuthorizedAnnotation], workspace.Spec.Retirement.Sequence); !ok || !authorized.Equal(clock.now) {
		t.Fatalf("retirement milestone = %q", workspace.Annotations[externalRetirementAuthorizedAnnotation])
	}
	if requested, ok := parseExternalRuntimeMilestone(workspace.Annotations[externalRuntimeStopRequestedAnnotation], workspace.Spec.Retirement.Sequence); !ok || !requested.Equal(clock.now.Add(-3*time.Second)) {
		t.Fatalf("stop-requested milestone = %q", workspace.Annotations[externalRuntimeStopRequestedAnnotation])
	}
	workspace.Status.Allocation.State = workspacev1alpha1.AllocationStopped
	workspace.Status.Allocation.Startup = nil
	if err := f.r.Status().Update(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	if err := f.r.Delete(context.Background(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: pod.Namespace, Name: pod.Name}}); err != nil {
		t.Fatal(err)
	}
	clock.now = clock.now.Add(6 * time.Second)
	runtimePoolReconcile(t, f.r, f.pool)
	runtimePoolReconcile(t, f.r, f.pool)
	if status := runtimePoolTestGetPool(t, f.r, f.pool).Status; status.Lifecycle != corev1alpha1.RuntimePoolLifecycleStopped {
		t.Fatalf("runtime did not stop: %s/%s", status.Lifecycle, status.Message)
	}
	if got := runtimePhaseSamples(t, "stop", externalRuntimePhaseProviderRetirement) - retirement; got != 1 {
		t.Fatalf("provider_retirement samples = %d, want one per stop", got)
	}
	if got := runtimePhaseSamples(t, "stop", externalRuntimePhaseDrain) - drain; got != 1 {
		t.Fatalf("drain samples = %d, want one per stop", got)
	}
	if got := runtimePhaseSamples(t, "stop", externalRuntimePhaseStopTotal) - stopTotal; got != 1 {
		t.Fatalf("stop_total samples = %d, want one per stop", got)
	}
}

func runtimePoolReconcileContext(t *testing.T, ctx context.Context, r *RuntimePoolReconciler, pool *corev1alpha1.RuntimePool) {
	t.Helper()
	runtimePoolTestSyncBootEpoch(t, r)
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

func runtimeStoppedLines(lines []map[string]any) []map[string]any {
	var stopped []map[string]any
	for _, line := range lines {
		if line["msg"] == externalRuntimeStoppedLogMessage {
			stopped = append(stopped, line)
		}
	}
	return stopped
}

// suspendFixtureToAuthorization serves a Suspend-capable runtime, scales its
// pool to zero as a cleared attachment does, and drains it until Core
// authorizes the Suspend. restart, when set, runs after the given number of
// retiring passes to model a controller restart.
func suspendFixtureToAuthorization(t *testing.T, f *externalRuntimePoolFixture, clock *runtimeTimingClock, ctx context.Context, restartAfter int) (*workspacev1alpha1.ExecutionWorkspace, corev1.Pod) {
	t.Helper()
	workspace := f.currentWorkspace(t)
	workspace.Spec.Lifecycle.AllowedOnDetach = append(workspace.Spec.Lifecycle.AllowedOnDetach, workspacev1alpha1.WorkspaceOnDetachSuspend)
	workspace.Annotations[acpWorkspaceSuspendModeAnnotation] = "DataOnly"
	if err := f.r.Update(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	_, pod := f.serve(t)
	current := runtimePoolTestGetPool(t, f.r, f.pool)
	current.Spec.DesiredReplicas = 0
	if err := f.r.Update(ctx, &current); err != nil {
		t.Fatal(err)
	}
	for pass := range 2 {
		if pass == restartAfter {
			f.r.externalTimings = externalRuntimeTimingReports{}
		}
		runtimePoolReconcileContext(t, ctx, f.r, f.pool)
		clock.now = clock.now.Add(time.Second)
	}
	f.supervisor.probe = runtimePoolValidProbe(f.pool, &pod, "external-boot", true)
	clock.now = clock.now.Add(time.Second)
	runtimePoolReconcileContext(t, ctx, f.r, f.pool)
	workspace = f.currentWorkspace(t)
	if workspace.Spec.Retirement == nil || workspace.Spec.Retirement.Action != workspacev1alpha1.WorkloadRetirementSuspend {
		t.Fatalf("drained runtime was not authorized to suspend: %#v", workspace.Spec.Retirement)
	}
	return workspace, pod
}

// settleSuspendedFixture models settlement suspending the workspace after the
// authorization, as the cold Suspend path does, and the provider's stop.
func settleSuspendedFixture(t *testing.T, f *externalRuntimePoolFixture, clock *runtimeTimingClock, ctx context.Context, pod corev1.Pod) {
	t.Helper()
	clock.now = clock.now.Add(time.Second)
	workspace := f.currentWorkspace(t)
	workspace.Annotations[acpWorkspaceLastDetachedAnnotation] = clock.now.Format(time.RFC3339Nano)
	workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
	if err := f.r.Update(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	workspace = f.currentWorkspace(t)
	workspace.Status.Allocation.State = workspacev1alpha1.AllocationStopped
	workspace.Status.Allocation.Startup = nil
	workspace.Status.ObservedGeneration = workspace.Generation
	if err := f.r.Status().Update(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	if err := f.r.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: pod.Namespace, Name: pod.Name}}); err != nil {
		t.Fatal(err)
	}
	clock.now = clock.now.Add(2 * time.Second)
}

func stopSuspendedFixture(t *testing.T, f *externalRuntimePoolFixture, clock *runtimeTimingClock, ctx context.Context, pod corev1.Pod) {
	t.Helper()
	settleSuspendedFixture(t, f, clock, ctx, pod)
	for range 3 {
		runtimePoolReconcileContext(t, ctx, f.r, f.pool)
	}
	if status := runtimePoolTestGetPool(t, f.r, f.pool).Status; status.Lifecycle != corev1alpha1.RuntimePoolLifecycleStopped {
		t.Fatalf("runtime did not stop: %s/%s", status.Lifecycle, status.Message)
	}
}

// A Stopped write that fails after Core proved the stop is retried later; the
// provider's retirement still ends at the first proof.
func TestExternalRuntimePoolStopEndsAtFirstProofDespiteFailedWrite(t *testing.T) {
	ctx, lines := captureRuntimeTimingLines(t)
	f := newExternalRuntimePoolFixture(t)
	clock := &runtimeTimingClock{now: time.Now().UTC()}
	f.r.Now = clock.Now
	_, pod := suspendFixtureToAuthorization(t, f, clock, ctx, -1)
	settleSuspendedFixture(t, f, clock, ctx, pod)
	original := f.r.Client
	failed := false
	f.r.Client = interceptor.NewClient(f.r.Client.(client.WithWatch), interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, delegate client.Client, subResource string, object client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if pool, ok := object.(*corev1alpha1.RuntimePool); ok && !failed && pool.Status.Lifecycle == corev1alpha1.RuntimePoolLifecycleStopped {
				failed = true
				return apierrors.NewServiceUnavailable("injected")
			}
			return delegate.SubResource(subResource).Patch(ctx, object, patch, opts...)
		},
	})
	runtimePoolTestSyncBootEpoch(t, f.r)
	if _, err := f.r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.pool)}); err == nil || !failed {
		t.Fatalf("Stopped write did not fail: %v", err)
	}
	f.r.Client = original
	clock.now = clock.now.Add(5 * time.Second)
	for range 3 {
		runtimePoolReconcileContext(t, ctx, f.r, f.pool)
	}
	stopped := runtimeStoppedLines(*lines)
	if len(stopped) != 1 || stopped[0]["providerRetirementMs"] != float64(3000) || stopped[0]["stopTotalMs"] != float64(6000) {
		t.Fatalf("stop lines = %v", stopped)
	}
}

// On the cold Suspend path settlement stamps last-detached-at after Core
// authorized the Suspend. The drain is still measured, from the pool's first
// retiring pass, and survives a restart after the authorization.
func TestExternalRuntimePoolMeasuresColdSuspendDrain(t *testing.T) {
	ctx, lines := captureRuntimeTimingLines(t)
	f := newExternalRuntimePoolFixture(t)
	clock := &runtimeTimingClock{now: time.Now().UTC()}
	f.r.Now = clock.Now
	stopRequested := clock.now
	workspace, pod := suspendFixtureToAuthorization(t, f, clock, ctx, -1)
	sequence := workspace.Spec.Retirement.Sequence
	if requested, ok := parseExternalRuntimeMilestone(workspace.Annotations[externalRuntimeStopRequestedAnnotation], sequence); !ok || !requested.Equal(stopRequested) {
		t.Fatalf("stop-requested milestone = %q, want %s", workspace.Annotations[externalRuntimeStopRequestedAnnotation], stopRequested)
	}
	// A restart after the authorization keeps the persisted start.
	f.r.externalTimings = externalRuntimeTimingReports{}
	stopSuspendedFixture(t, f, clock, ctx, pod)
	workspace = f.currentWorkspace(t)
	authorized, _ := parseExternalRuntimeMilestone(workspace.Annotations[externalRetirementAuthorizedAnnotation], sequence)
	detached, err := time.Parse(time.RFC3339Nano, workspace.Annotations[acpWorkspaceLastDetachedAnnotation])
	if err != nil || !detached.After(authorized) {
		t.Fatalf("last-detached-at %s does not follow the authorization %s", detached, authorized)
	}
	stopped := runtimeStoppedLines(*lines)
	if len(stopped) != 1 {
		t.Fatalf("stop lines = %v", *lines)
	}
	line := stopped[0]
	if line["action"] != "suspend" || line["drainMs"] != float64(3000) || line["providerRetirementMs"] != float64(3000) || line["stopTotalMs"] != float64(6000) {
		t.Fatalf("stop line = %v", line)
	}
}

// A controller that restarts while a retirement is under way does not know
// when it began, so it reports only the provider's part of that stop.
func TestExternalRuntimePoolOmitsDrainAfterRestartDuringRetirement(t *testing.T) {
	ctx, lines := captureRuntimeTimingLines(t)
	f := newExternalRuntimePoolFixture(t)
	clock := &runtimeTimingClock{now: time.Now().UTC()}
	f.r.Now = clock.Now
	workspace, pod := suspendFixtureToAuthorization(t, f, clock, ctx, 1)
	if raw, present := workspace.Annotations[externalRuntimeStopRequestedAnnotation]; present {
		t.Fatalf("restarted controller stamped stop start %q", raw)
	}
	stopSuspendedFixture(t, f, clock, ctx, pod)
	stopped := runtimeStoppedLines(*lines)
	if len(stopped) != 1 {
		t.Fatalf("stop lines = %v", *lines)
	}
	if _, present := stopped[0]["drainMs"]; present {
		t.Fatalf("stop line after restart = %v", stopped[0])
	}
	if _, present := stopped[0]["stopTotalMs"]; present || stopped[0]["providerRetirementMs"] != float64(3000) {
		t.Fatalf("stop line after restart = %v", stopped[0])
	}
}

// The start and stop lines name only the pool, the path or action, and the
// sequence, and are written once per transition.
func TestObserveExternalRuntimeTransitionLogsOncePerTransition(t *testing.T) {
	f := newExternalRuntimePoolFixture(t)
	clock := &runtimeTimingClock{now: time.Now().UTC()}
	f.r.Now = clock.Now
	f.materialize(t)
	pool := runtimePoolTestGetPool(t, f.r, f.pool)

	var lines []map[string]any
	ctx := logf.IntoContext(context.Background(), funcr.NewJSON(func(obj string) {
		values := map[string]any{}
		if err := json.Unmarshal([]byte(obj), &values); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, values)
	}, funcr.Options{}))
	clock.now = clock.now.Add(9 * time.Second)
	closed := corev1alpha1.RuntimePoolStatus{AdmissionState: corev1alpha1.RuntimePoolAdmissionClosed}
	accepting := corev1alpha1.RuntimePoolStatus{AdmissionState: corev1alpha1.RuntimePoolAdmissionAccepting,
		ActiveInstance: &corev1alpha1.RuntimePoolActiveInstanceStatus{RuntimeInstanceID: "instance-1"}}
	f.r.observeExternalRuntimeTransition(ctx, &pool, closed, accepting)
	// Re-admitting the same instance, for example after a degraded probe, is
	// not a new start, and neither is a pass that stays admitted.
	degraded := *closed.DeepCopy()
	degraded.ActiveInstance = accepting.ActiveInstance.DeepCopy()
	f.r.observeExternalRuntimeTransition(ctx, &pool, degraded, accepting)
	f.r.observeExternalRuntimeTransition(ctx, &pool, accepting, accepting)
	if len(lines) != 1 {
		t.Fatalf("lines = %v", lines)
	}
	line := lines[0]
	if line["msg"] != externalRuntimeStartedLogMessage || line["path"] != externalRuntimePathCold || line["sequence"] != float64(1) ||
		line["runtimePool"] != pool.Name || line["startTotalMs"] != float64(9000) {
		t.Fatalf("start line = %v", line)
	}
	for key := range line {
		switch key {
		case "level", "logger", "msg", "runtimePool", "path", "sequence", "provisionMs", "startTotalMs":
		default:
			t.Fatalf("start line carries unexpected field %q: %v", key, line)
		}
	}
}

func captureRuntimeTimingLines(t *testing.T) (context.Context, *[]map[string]any) {
	t.Helper()
	var lines []map[string]any
	ctx := logf.IntoContext(context.Background(), funcr.NewJSON(func(obj string) {
		values := map[string]any{}
		if err := json.Unmarshal([]byte(obj), &values); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, values)
	}, funcr.Options{}))
	return ctx, &lines
}

// A probe can record an instance while its supervisor still boots. Its first
// admission is its start; a later readmission of the same instance is not.
func TestObserveExternalRuntimeTransitionReportsBootedInstanceOnFirstAdmission(t *testing.T) {
	f := newExternalRuntimePoolFixture(t)
	f.materialize(t)
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	ctx, lines := captureRuntimeTimingLines(t)
	instance := &corev1alpha1.RuntimePoolActiveInstanceStatus{RuntimeInstanceID: "instance-1"}
	booting := corev1alpha1.RuntimePoolStatus{Lifecycle: corev1alpha1.RuntimePoolLifecycleStarting, AdmissionState: corev1alpha1.RuntimePoolAdmissionClosed, ActiveInstance: instance}
	accepting := corev1alpha1.RuntimePoolStatus{Lifecycle: corev1alpha1.RuntimePoolLifecycleServing, AdmissionState: corev1alpha1.RuntimePoolAdmissionAccepting, ActiveInstance: instance}
	atCapacity := corev1alpha1.RuntimePoolStatus{Lifecycle: corev1alpha1.RuntimePoolLifecycleServing, AdmissionState: corev1alpha1.RuntimePoolAdmissionClosed, ActiveInstance: instance}
	f.r.observeExternalRuntimeTransition(ctx, &pool, booting, accepting)
	f.r.observeExternalRuntimeTransition(ctx, &pool, accepting, atCapacity)
	f.r.observeExternalRuntimeTransition(ctx, &pool, atCapacity, accepting)
	if len(*lines) != 1 || (*lines)[0]["msg"] != externalRuntimeStartedLogMessage {
		t.Fatalf("lines = %v, want one start", *lines)
	}
}

// The reconcile's own workspace is reported even when a second read would
// fail after the transition persisted.
func TestObserveExternalRuntimeTransitionUsesReconciledWorkspace(t *testing.T) {
	f := newExternalRuntimePoolFixture(t)
	workspace, _ := f.materialize(t)
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	if err := f.r.Delete(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	ctx, lines := captureRuntimeTimingLines(t)
	accepting := corev1alpha1.RuntimePoolStatus{AdmissionState: corev1alpha1.RuntimePoolAdmissionAccepting,
		ActiveInstance: &corev1alpha1.RuntimePoolActiveInstanceStatus{RuntimeInstanceID: "instance-1"}}
	f.r.observeExternalRuntimeTransition(ctx, &pool, corev1alpha1.RuntimePoolStatus{}, accepting)
	if len(*lines) != 0 {
		t.Fatalf("lines without a workspace = %v", *lines)
	}
	f.r.observeExternalRuntimeTransition(withExternalRuntimeTimingWorkspace(ctx, workspace), &pool, corev1alpha1.RuntimePoolStatus{}, accepting)
	if len(*lines) != 1 || (*lines)[0]["msg"] != externalRuntimeStartedLogMessage {
		t.Fatalf("lines = %v, want one start", *lines)
	}
}

// A pool that leaves Stopped after a transient failure and records Stopped
// again for the same retirement does not report that stop twice.
func TestObserveExternalRuntimeTransitionReportsEachRetirementOnce(t *testing.T) {
	f := newExternalRuntimePoolFixture(t)
	clock := &runtimeTimingClock{now: time.Now().UTC()}
	f.r.Now = clock.Now
	workspace, _ := f.materialize(t)
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	workspace.Spec.Retirement = &workspacev1alpha1.WorkloadRetirement{Sequence: workspace.Spec.Workload.Sequence, Action: workspacev1alpha1.WorkloadRetirementStop}
	setExternalRuntimeMilestone(workspace, externalRetirementAuthorizedAnnotation, workspace.Spec.Retirement.Sequence, clock.now)
	clock.now = clock.now.Add(4 * time.Second)
	ctx, lines := captureRuntimeTimingLines(t)
	ctx = withExternalRuntimeTimingWorkspace(ctx, workspace)
	stopping := corev1alpha1.RuntimePoolStatus{Lifecycle: corev1alpha1.RuntimePoolLifecycleStopping}
	degraded := corev1alpha1.RuntimePoolStatus{Lifecycle: corev1alpha1.RuntimePoolLifecycleDegraded}
	stopped := corev1alpha1.RuntimePoolStatus{Lifecycle: corev1alpha1.RuntimePoolLifecycleStopped}
	f.r.observeExternalRuntimeTransition(ctx, &pool, stopping, stopped)
	f.r.observeExternalRuntimeTransition(ctx, &pool, degraded, stopped)
	if len(*lines) != 1 || (*lines)[0]["msg"] != externalRuntimeStoppedLogMessage || (*lines)[0]["providerRetirementMs"] != float64(4000) {
		t.Fatalf("lines = %v, want one stop", *lines)
	}
	// A different retirement of the same sequence is a new stop.
	workspace.Spec.Retirement.Action = workspacev1alpha1.WorkloadRetirementDelete
	f.r.observeExternalRuntimeTransition(ctx, &pool, degraded, stopped)
	if len(*lines) != 2 || (*lines)[1]["action"] != "delete" {
		t.Fatalf("lines = %v, want a delete stop", *lines)
	}
}

// Deleting a running pool never records Stopped; releasing its finalizer
// after the proven termination reports the stop.
func TestExternalRuntimePoolDeletionRecordsStop(t *testing.T) {
	ctx := context.Background()
	f := newExternalRuntimePoolFixture(t)
	clock := &runtimeTimingClock{now: time.Now().UTC()}
	f.r.Now = clock.Now
	retirement := runtimePhaseSamples(t, "delete", externalRuntimePhaseProviderRetirement)
	_, pod := f.serve(t)
	current := runtimePoolTestGetPool(t, f.r, f.pool)
	if err := f.r.Delete(ctx, &current); err != nil {
		t.Fatal(err)
	}
	runtimePoolReconcile(t, f.r, f.pool)
	runtimePoolReconcile(t, f.r, f.pool)
	f.supervisor.probe = runtimePoolValidProbe(f.pool, &pod, "external-boot", true)
	runtimePoolReconcile(t, f.r, f.pool)
	workspace := f.currentWorkspace(t)
	if workspace.Spec.Retirement == nil || workspace.Spec.Retirement.Action != workspacev1alpha1.WorkloadRetirementDelete {
		t.Fatalf("deleted pool did not authorize deletion: %#v", workspace.Spec.Retirement)
	}
	workspace.Status.Allocation.Startup = nil
	workspace.Status.Allocation.State = workspacev1alpha1.AllocationDeleted
	workspace.Status.Allocation.Disposition = &workspacev1alpha1.ExecutionWorkspaceDisposition{Compute: workspacev1alpha1.DispositionDeleted, AccessCredentials: workspacev1alpha1.DispositionRevoked,
		EphemeralSecrets: workspacev1alpha1.DispositionDeleted, WorkspaceData: workspacev1alpha1.DispositionDeleted, PersistentVolumes: workspacev1alpha1.DispositionDeleted,
		Checkpoints: workspacev1alpha1.DispositionDeleted, ProviderResources: workspacev1alpha1.DispositionDeleted}
	workspace.Status.ObservedGeneration = workspace.Generation
	if err := f.r.Status().Update(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	if err := f.r.Delete(ctx, &pod); err != nil {
		t.Fatal(err)
	}
	clock.now = clock.now.Add(5 * time.Second)
	for range 6 {
		if _, err := f.r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.pool)}); err != nil {
			t.Fatal(err)
		}
		if err := f.r.Get(ctx, client.ObjectKeyFromObject(f.pool), &corev1alpha1.RuntimePool{}); apierrors.IsNotFound(err) {
			break
		}
	}
	if err := f.r.Get(ctx, client.ObjectKeyFromObject(f.pool), &corev1alpha1.RuntimePool{}); !apierrors.IsNotFound(err) {
		t.Fatalf("deleted pool was not finalized: %v", err)
	}
	if got := runtimePhaseSamples(t, "delete", externalRuntimePhaseProviderRetirement) - retirement; got != 1 {
		t.Fatalf("delete provider_retirement samples = %d, want one", got)
	}
}

// When Core itself requests the retirement, here to retire an old runtime
// fence, the drain starts at that request rather than at the next pass.
func TestExternalRuntimePoolStartsDrainWhereCoreRequestsRetirement(t *testing.T) {
	f := newExternalRuntimePoolFixture(t)
	clock := &runtimeTimingClock{now: time.Now().UTC()}
	f.r.Now = clock.Now
	workspace, _ := f.serve(t)
	requested := clock.now.Add(time.Second)
	clock.now = requested
	f.r.ControllerEpoch++
	runtimePoolReconcile(t, f.r, f.pool)
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	if pool.Annotations[externalRuntimeRetirementRequestedAnnotation] != booleanTrueValue {
		t.Fatalf("old runtime fence was not retired: %s", pool.Status.Message)
	}
	clock.now = clock.now.Add(2 * time.Second)
	runtimePoolReconcile(t, f.r, f.pool)
	if got, ok := f.r.externalTimings.retirementRequested(pool.UID, workspace.Spec.Workload.Sequence); !ok || !got.Equal(requested) {
		t.Fatalf("retirement start = %s, %v, want %s", got, ok, requested)
	}
}
