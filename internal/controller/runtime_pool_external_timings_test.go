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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

	// A milestone from another sequence reports nothing.
	w.Annotations[externalWorkloadPublishedAnnotation] = formatExternalRuntimeMilestone(1, published)
	if phases := externalRuntimeStartPhases(pool, w, published.Add(time.Hour)); phases != nil {
		t.Fatalf("stale milestone phases = %v", phases)
	}
}

func TestExternalRuntimeStopPhases(t *testing.T) {
	detached := time.Unix(2000, 0).UTC()
	w := &workspacev1alpha1.ExecutionWorkspace{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
			acpWorkspaceLastDetachedAnnotation:     detached.Format(time.RFC3339Nano),
			externalRetirementAuthorizedAnnotation: formatExternalRuntimeMilestone(2, detached.Add(4*time.Second)),
		}},
		Spec: workspacev1alpha1.ExecutionWorkspaceSpec{Retirement: &workspacev1alpha1.WorkloadRetirement{Sequence: 2, Action: workspacev1alpha1.WorkloadRetirementSuspend}},
	}
	got := phaseDurations(externalRuntimeStopPhases(w, detached.Add(10*time.Second)))
	if len(got) != 3 || got[externalRuntimePhaseDrain] != 4*time.Second || got[externalRuntimePhaseProviderRetirement] != 6*time.Second ||
		got[externalRuntimePhaseStopTotal] != 10*time.Second {
		t.Fatalf("suspend phases = %v", got)
	}

	// A Stop is not started by a detach: only the provider's part is known.
	w.Spec.Retirement.Action = workspacev1alpha1.WorkloadRetirementStop
	got = phaseDurations(externalRuntimeStopPhases(w, detached.Add(10*time.Second)))
	if len(got) != 1 || got[externalRuntimePhaseProviderRetirement] != 6*time.Second {
		t.Fatalf("stop phases = %v", got)
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
