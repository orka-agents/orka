/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	execevents "github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
)

const workerUpstreamSummary = "provider_upstream_error: completion failed: 429 Too Many Requests rate limited by upstream"

func workerFailureTask(name string) *corev1alpha1.Task {
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
		Status: corev1alpha1.TaskStatus{
			Phase:   corev1alpha1.TaskPhaseRunning,
			JobName: name + "-job",
		},
	}
}

func terminatedWorkerPod(task *corev1alpha1.Task, exitCode int32, reason string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      task.Name + "-pod",
			Namespace: task.Namespace,
			Labels: map[string]string{
				labels.LabelTask:               labels.SelectorValue(task.Name),
				"batch.kubernetes.io/job-name": task.Status.JobName,
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "worker",
			Resources: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
			},
		}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "worker",
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: exitCode,
					Reason:   reason,
				}},
			}},
		},
	}
}

func appendWorkerFailedEvent(t *testing.T, r *TaskReconciler, task *corev1alpha1.Task, summary string) {
	t.Helper()
	appendWorkerFailedEventAt(t, r, task, summary, time.Time{})
}

func appendWorkerFailedEventAt(
	t *testing.T, r *TaskReconciler, task *corev1alpha1.Task, summary string, createdAt time.Time,
) {
	t.Helper()
	if _, err := r.ExecutionEventStore.AppendExecutionEvent(context.Background(), &store.ExecutionEvent{
		Namespace:  task.Namespace,
		StreamType: store.ExecutionEventStreamTypeTask,
		StreamID:   task.Name,
		TaskName:   task.Name,
		Type:       execevents.ExecutionEventTypeWorkerFailed,
		Severity:   execevents.ExecutionEventSeverityError,
		Summary:    summary,
		CreatedAt:  createdAt,
	}); err != nil {
		t.Fatalf("AppendExecutionEvent() error = %v", err)
	}
}

func TestDiagnoseFailedJobPrefersWorkerReportedProviderFailure(t *testing.T) {
	task := workerFailureTask("provider-down")
	r := newUnitReconciler(newTestScheme(), task, terminatedWorkerPod(task, 1, "Error"))
	appendWorkerFailedEvent(t, r, task, workerUpstreamSummary)

	got := r.diagnoseFailedJob(context.Background(), task)
	want := "job failed: " + workerUpstreamSummary
	if got != want {
		t.Fatalf("diagnoseFailedJob() = %q, want %q", got, want)
	}
}

func TestDiagnoseFailedJobReportsUnreachableProviderDetail(t *testing.T) {
	task := workerFailureTask("provider-refused")
	r := newUnitReconciler(newTestScheme(), task, terminatedWorkerPod(task, 1, "Error"))
	appendWorkerFailedEvent(t, r, task,
		`provider_upstream_error: completion failed: Post "http://relay.default.svc/v1/responses": `+
			`dial tcp 10.96.0.7:80: connect: connection refused`)

	got := r.diagnoseFailedJob(context.Background(), task)
	for _, want := range []string{"job failed: provider_upstream_error:", "connection refused"} {
		if !strings.Contains(got, want) {
			t.Fatalf("diagnoseFailedJob() = %q, want it to contain %q", got, want)
		}
	}
	if strings.Contains(got, "exited with code") {
		t.Fatalf("diagnoseFailedJob() = %q, want the worker detail instead of the exit code", got)
	}
}

func TestDiagnoseFailedJobUsesLatestWorkerFailureReport(t *testing.T) {
	task := workerFailureTask("retried-task")
	r := newUnitReconciler(newTestScheme(), task, terminatedWorkerPod(task, 1, "Error"))
	appendWorkerFailedEvent(t, r, task, "first attempt failed")
	appendWorkerFailedEvent(t, r, task, workerUpstreamSummary)

	got := r.diagnoseFailedJob(context.Background(), task)
	want := "job failed: " + workerUpstreamSummary
	if got != want {
		t.Fatalf("diagnoseFailedJob() = %q, want the final attempt's report %q", got, want)
	}
}

func TestDiagnoseFailedJobPrefersOOMKillOverWorkerReport(t *testing.T) {
	task := workerFailureTask("oom-task")
	r := newUnitReconciler(newTestScheme(), task, terminatedWorkerPod(task, 137, "OOMKilled"))
	appendWorkerFailedEvent(t, r, task, workerUpstreamSummary)

	got := r.diagnoseFailedJob(context.Background(), task)
	if !strings.Contains(got, "OOMKilled") || !strings.Contains(got, "512Mi") {
		t.Fatalf("diagnoseFailedJob() = %q, want the OOM remedy", got)
	}
}

func TestDiagnoseFailedJobFallsBackToExitCodeWithoutWorkerReport(t *testing.T) {
	task := workerFailureTask("no-report")
	r := newUnitReconciler(newTestScheme(), task, terminatedWorkerPod(task, 1, "Error"))

	got := r.diagnoseFailedJob(context.Background(), task)
	want := "job failed: container exited with code 1 (reason=Error)"
	if got != want {
		t.Fatalf("diagnoseFailedJob() = %q, want %q", got, want)
	}
}

func TestDiagnoseFailedJobIgnoresBlankWorkerReport(t *testing.T) {
	task := workerFailureTask("blank-report")
	r := newUnitReconciler(newTestScheme(), task, terminatedWorkerPod(task, 1, "Error"))
	appendWorkerFailedEvent(t, r, task, "   ")

	got := r.diagnoseFailedJob(context.Background(), task)
	want := "job failed: container exited with code 1 (reason=Error)"
	if got != want {
		t.Fatalf("diagnoseFailedJob() = %q, want %q", got, want)
	}
}

func TestDiagnoseFailedJobIgnoresOtherTaskWorkerReport(t *testing.T) {
	task := workerFailureTask("target-task")
	other := workerFailureTask("other-task")
	r := newUnitReconciler(newTestScheme(), task, terminatedWorkerPod(task, 1, "Error"))
	appendWorkerFailedEvent(t, r, other, workerUpstreamSummary)

	got := r.diagnoseFailedJob(context.Background(), task)
	want := "job failed: container exited with code 1 (reason=Error)"
	if got != want {
		t.Fatalf("diagnoseFailedJob() = %q, want %q", got, want)
	}
}

func TestDiagnoseFailedJobReportsWorkerDetailWithoutPodSignal(t *testing.T) {
	task := workerFailureTask("gc-pod")
	r := newUnitReconciler(newTestScheme(), task)
	appendWorkerFailedEvent(t, r, task, workerUpstreamSummary)

	got := r.diagnoseFailedJob(context.Background(), task)
	want := "job failed: " + workerUpstreamSummary
	if got != want {
		t.Fatalf("diagnoseFailedJob() = %q, want %q", got, want)
	}
}

func TestDiagnoseFailedJobToleratesEventLookupFailure(t *testing.T) {
	task := workerFailureTask("event-store-down")
	r := newUnitReconciler(newTestScheme(), task, terminatedWorkerPod(task, 1, "Error"))
	r.ExecutionEventStore = failingExecutionEventStore{err: errors.New("store unavailable")}

	got := r.diagnoseFailedJob(context.Background(), task)
	want := "job failed: container exited with code 1 (reason=Error)"
	if got != want {
		t.Fatalf("diagnoseFailedJob() = %q, want %q", got, want)
	}
}

func TestDiagnoseFailedJobToleratesMissingEventStore(t *testing.T) {
	task := workerFailureTask("no-event-store")
	r := newUnitReconciler(newTestScheme(), task, terminatedWorkerPod(task, 1, "Error"))
	r.ExecutionEventStore = nil

	got := r.diagnoseFailedJob(context.Background(), task)
	want := "job failed: container exited with code 1 (reason=Error)"
	if got != want {
		t.Fatalf("diagnoseFailedJob() = %q, want %q", got, want)
	}
}

func TestDiagnoseFailedJobIgnoresPreviousAttemptWorkerReport(t *testing.T) {
	task := workerFailureTask("stale-report")
	startedAt := metav1.NewTime(time.Now())
	task.Status.StartTime = &startedAt
	task.Status.Attempts = 2
	r := newUnitReconciler(newTestScheme(), task, terminatedWorkerPod(task, 1, "Error"))
	appendWorkerFailedEventAt(t, r, task, workerUpstreamSummary, startedAt.Add(-5*time.Minute))

	got := r.diagnoseFailedJob(context.Background(), task)
	want := "job failed: container exited with code 1 (reason=Error)"
	if got != want {
		t.Fatalf("diagnoseFailedJob() = %q, want the previous attempt's report ignored (%q)", got, want)
	}
}

func TestDiagnoseFailedJobUsesCurrentAttemptWorkerReport(t *testing.T) {
	task := workerFailureTask("current-report")
	startedAt := metav1.NewTime(time.Now())
	task.Status.StartTime = &startedAt
	task.Status.Attempts = 2
	r := newUnitReconciler(newTestScheme(), task, terminatedWorkerPod(task, 1, "Error"))
	appendWorkerFailedEventAt(t, r, task, "first attempt failed", startedAt.Add(-5*time.Minute))
	appendWorkerFailedEventAt(t, r, task, workerUpstreamSummary, startedAt.Add(time.Second))

	got := r.diagnoseFailedJob(context.Background(), task)
	want := "job failed: " + workerUpstreamSummary
	if got != want {
		t.Fatalf("diagnoseFailedJob() = %q, want %q", got, want)
	}
}

func TestWorkerFailureDetailRedactsCredentialShape(t *testing.T) {
	got := workerFailureDetail(`provider_upstream_error: 401 Unauthorized api_key=sk-livesecretvalue0123456789`)
	if strings.Contains(got, "sk-livesecretvalue0123456789") {
		t.Fatalf("workerFailureDetail() = %q, want the credential redacted", got)
	}
	if !strings.Contains(got, "provider_upstream_error") {
		t.Fatalf("workerFailureDetail() = %q, want the failure code preserved", got)
	}
}

func TestWorkerFailureDetailRedactsCredentialSplitByControlRunes(t *testing.T) {
	got := workerFailureDetail("provider_upstream_error: pass\nword=hunter2supersecretvalue")
	if strings.Contains(got, "hunter2supersecretvalue") {
		t.Fatalf("workerFailureDetail() = %q, want the split credential redacted", got)
	}
}

func TestWorkerFailureDetailStripsControlRunes(t *testing.T) {
	got := workerFailureDetail("provider_upstream_error:\x1b[31m failed\x00")
	if strings.ContainsAny(got, "\x1b\x00") {
		t.Fatalf("workerFailureDetail() = %q, want control runes stripped", got)
	}
}

func TestWorkerFailureDetailBoundsLongSummary(t *testing.T) {
	got := workerFailureDetail(strings.Repeat("é", 4096))
	if len(got) > workerFailureDetailLimit {
		t.Fatalf("workerFailureDetail() length = %d, want <= %d", len(got), workerFailureDetailLimit)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("workerFailureDetail() = %q, want valid UTF-8", got)
	}
}

func TestWorkerFailureDetailIgnoresEmptySummary(t *testing.T) {
	if got := workerFailureDetail("\t \n"); got != "" {
		t.Fatalf("workerFailureDetail() = %q, want empty", got)
	}
}
