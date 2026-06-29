package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/harness"
	"github.com/sozercan/orka/internal/harness/harnesstest"
	"github.com/sozercan/orka/internal/labels"
	"github.com/sozercan/orka/internal/store"
)

func allowInsecureLoopbackHarnessEndpoint(t *testing.T, r *TaskReconciler, endpoint string) {
	t.Helper()
	r.HarnessEndpointAllowInsecureLoopback = true
	t.Setenv(harnessWrapperEndpointEnv, endpoint)
}

func TestHarnessProviderRejectsUnhealthyHarness(t *testing.T) {
	server := harnesstest.NewFakeHarnessServer(harnesstest.FakeHarnessConfig{Behavior: harnesstest.BehaviorUnhealthy})
	defer server.Close()
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "harness-unhealthy", Namespace: "default", Annotations: map[string]string{labels.AnnotationHarnessEndpoint: server.URL()}},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, Prompt: "hello"},
		Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
	}
	r := newUnitReconciler(scheme, task)
	allowInsecureLoopbackHarnessEndpoint(t, r, task.Annotations[labels.AnnotationHarnessEndpoint])
	result, err := r.handlePending(context.Background(), task)
	if err != nil {
		t.Fatalf("handlePending() error = %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Fatalf("result=%#v, want requeue for ambiguous StartTurn failure", result)
	}
	updated := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: task.Name}, updated); err != nil {
		t.Fatalf("Get task: %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhaseFailed || !strings.Contains(updated.Status.Message, "harness is not ready") {
		t.Fatalf("status=%#v, want failed not-ready harness", updated.Status)
	}
}

func TestHarnessProviderTaskRunsTurnAndPersistsEvents(t *testing.T) {
	server := harnesstest.NewFakeHarnessServer(harnesstest.FakeHarnessConfig{Behavior: harnesstest.BehaviorSuccess})
	defer server.Close()
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "harness-task",
			Namespace: "default",
			Annotations: map[string]string{
				labels.AnnotationHarnessEndpoint: server.URL(),
			},
		},
		Spec:   corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, Prompt: "hello"},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
	}
	r := newUnitReconciler(scheme, task)
	allowInsecureLoopbackHarnessEndpoint(t, r, task.Annotations[labels.AnnotationHarnessEndpoint])

	if _, err := r.handlePending(context.Background(), task); err != nil {
		t.Fatalf("handlePending() error = %v", err)
	}
	updated := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "harness-task"}, updated); err != nil {
		t.Fatalf("Get updated task: %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhaseSucceeded || updated.Status.JobName != "" {
		t.Fatalf("status = %#v, want succeeded without Job", updated.Status)
	}
	result, err := r.ResultStore.GetResult(context.Background(), "default", "harness-task")
	if err != nil || string(result) != "ok" {
		t.Fatalf("result = %q err=%v, want ok", result, err)
	}
	listed, err := r.ExecutionEventStore.ListExecutionEvents(context.Background(), store.ExecutionEventFilter{
		Namespace: "default", StreamID: "harness-task", Limit: 20,
	})
	if err != nil {
		t.Fatalf("ListExecutionEvents: %v", err)
	}
	if !hasHarnessProviderExecutionEventType(listed, events.ExecutionEventTypeAgentRuntimeCompleted) ||
		!hasHarnessProviderExecutionEventType(listed, events.ExecutionEventTypeTaskSucceeded) {
		t.Fatalf("events = %#v, want harness runtime completion and task success", listed)
	}
}

func TestHarnessProviderSkipsCompletedResultWhenFailedTerminalPresent(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "harness-conflict", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
		Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
	}
	r := newUnitReconciler(newTestScheme(), task)
	_, err := r.handleHarnessTurnRunResult(context.Background(), task, nil, "turn-conflict", harness.TurnRunResult{
		Completed: &harness.TurnCompleted{Result: "stale success"},
		Failed:    &harness.TurnFailed{Reason: "failed", Message: "authoritative failure"},
	}, fmt.Errorf("stream ended after failed terminal"))
	if err != nil {
		t.Fatalf("handleHarnessTurnRunResult: %v", err)
	}
	if result, err := r.ResultStore.GetResult(context.Background(), task.Namespace, task.Name); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetResult = %q err=%v, want no stale completed result", result, err)
	}
	updated := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, updated); err != nil {
		t.Fatalf("Get updated task: %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhaseFailed {
		t.Fatalf("phase = %s, want Failed", updated.Status.Phase)
	}
}

func TestHarnessProviderDoesNotOverwriteFetchedFailedOutputRef(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "harness-failed-ref", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
		Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
	}
	r := newUnitReconciler(newTestScheme(), task)
	if err := r.ResultStore.SaveResult(context.Background(), task.Namespace, task.Name, []byte("full diagnostics")); err != nil {
		t.Fatalf("SaveResult: %v", err)
	}
	_, err := r.completeHarnessTaskFromTerminal(context.Background(), task, nil, &harness.TurnFailed{Reason: "failed", Message: "failed", Result: "preview", OutputRef: "ref"}, false)
	if err != nil {
		t.Fatalf("completeHarnessTaskFromTerminal: %v", err)
	}
	result, err := r.ResultStore.GetResult(context.Background(), task.Namespace, task.Name)
	if err != nil {
		t.Fatalf("GetResult: %v", err)
	}
	if string(result) != "full diagnostics" {
		t.Fatalf("result = %q, want existing fetched diagnostics preserved", result)
	}
}

func TestHarnessProviderTaskFailureMarksTaskFailed(t *testing.T) {
	server := harnesstest.NewFakeHarnessServer(harnesstest.FakeHarnessConfig{Behavior: harnesstest.BehaviorFailure})
	defer server.Close()
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "harness-fail-task",
			Namespace: "default",
			Annotations: map[string]string{
				labels.AnnotationHarnessEndpoint: server.URL(),
			},
		},
		Spec:   corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, Prompt: "hello"},
		Status: harnessStartedPendingStatus(time.Now().UTC()),
	}
	r := newUnitReconciler(scheme, task)
	allowInsecureLoopbackHarnessEndpoint(t, r, task.Annotations[labels.AnnotationHarnessEndpoint])

	if _, err := r.handlePending(context.Background(), task); err != nil {
		t.Fatalf("handlePending() error = %v", err)
	}
	updated := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "harness-fail-task"}, updated); err != nil {
		t.Fatalf("Get updated task: %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhaseFailed || !strings.Contains(updated.Status.Message, "harness turn failed") {
		t.Fatalf("status = %#v, want failed harness turn", updated.Status)
	}
}

func TestMarkHarnessTaskTurnIdentityRepairsStatusForPersistedIdentity(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "harness-identity-repair", Namespace: "default", Annotations: map[string]string{
			labels.AnnotationHarnessRuntimeSession: "runtime-a",
			labels.AnnotationHarnessTurn:           "turn-a",
			labels.AnnotationHarnessCorrelation:    "corr-a",
		}},
		Spec:   corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
	}
	r := newUnitReconciler(newTestScheme(), task)
	started, err := r.markHarnessTaskTurnIdentity(context.Background(), task, "runtime-a", "turn-a", "corr-a")
	if err != nil {
		t.Fatalf("markHarnessTaskTurnIdentity: %v", err)
	}
	if !started {
		t.Fatal("markHarnessTaskTurnIdentity returned started=false for persisted identity")
	}
	updated := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, updated); err != nil {
		t.Fatalf("Get updated task: %v", err)
	}
	if updated.Status.StartTime == nil || updated.Status.Attempts != 1 {
		t.Fatalf("status start=%v attempts=%d, want repaired start time and one attempt", updated.Status.StartTime, updated.Status.Attempts)
	}
	condition := meta.FindStatusCondition(updated.Status.Conditions, ConditionTypeJobCreated)
	if condition == nil || condition.Status != metav1.ConditionTrue || condition.Reason != "HarnessProviderStarting" {
		t.Fatalf("condition=%#v, want HarnessProviderStarting", condition)
	}
}

func TestHarnessProviderEndpointRequiresApprovedInNamespaceService(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "default"}}
	approved := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "harness",
			Namespace: "default",
			Annotations: map[string]string{
				labels.AnnotationHarnessProvider: "kubernetes-service",
			},
		},
		Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "harness"}},
	}
	external := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "external", Namespace: "default", Annotations: map[string]string{
			labels.AnnotationHarnessProvider: "kubernetes-service",
		}},
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeExternalName, ExternalName: "example.com"},
	}
	unapproved := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "plain", Namespace: "default"}}
	r := newUnitReconciler(scheme, task, approved, external, unapproved)

	if err := r.validateHarnessServiceEndpoint(context.Background(), task, "http://harness.default.svc:8080"); err != nil {
		t.Fatalf("approved service validation error = %v", err)
	}
	if err := r.validateHarnessServiceEndpoint(context.Background(), task, "http://external.default.svc:8080"); err == nil || !strings.Contains(err.Error(), "ExternalName") {
		t.Fatalf("external validation error = %v, want ExternalName rejection", err)
	}
	if err := r.validateHarnessServiceEndpoint(context.Background(), task, "http://plain.default.svc:8080"); err == nil || !strings.Contains(err.Error(), "selector") {
		t.Fatalf("plain validation error = %v, want selector rejection", err)
	}
	if err := r.validateHarnessServiceEndpoint(context.Background(), task, "ftp://harness.default.svc:8080"); err == nil || !strings.Contains(err.Error(), "scheme") {
		t.Fatalf("scheme validation error = %v, want scheme rejection", err)
	}
	if err := r.validateHarnessServiceEndpoint(context.Background(), task, "http://user:pass@harness.default.svc:8080"); err == nil || !strings.Contains(err.Error(), "user info") {
		t.Fatalf("userinfo validation error = %v, want user info rejection", err)
	}
	if err := r.validateHarnessServiceEndpoint(context.Background(), task, "http://harness.default.svc:8080?token=secret"); err == nil || !strings.Contains(err.Error(), "query") {
		t.Fatalf("query validation error = %v, want query rejection", err)
	}
	if err := r.validateHarnessServiceEndpoint(context.Background(), task, "http://harness.default.svc:8080#secret"); err == nil || !strings.Contains(err.Error(), "fragment") {
		t.Fatalf("fragment validation error = %v, want fragment rejection", err)
	}
	allowInsecureLoopbackHarnessEndpoint(t, r, "http://127.0.0.1:8080")
	if err := r.validateHarnessServiceEndpoint(context.Background(), task, "http://plain.default.svc:8080"); err == nil || !strings.Contains(err.Error(), "selector") {
		t.Fatalf("plain validation with loopback test flag error = %v, want selector rejection", err)
	}
	if err := r.validateHarnessServiceEndpoint(context.Background(), task, "http://127.0.0.1:8080"); err != nil {
		t.Fatalf("loopback test endpoint rejected: %v", err)
	}
	if _, err := harnessServiceNameFromHost("harness.other.svc", "default"); err == nil {
		t.Fatal("harnessServiceNameFromHost accepted cross-namespace host")
	}
}

func TestHarnessCapabilitiesSupportToolMode(t *testing.T) {
	if !harnessCapabilitiesSupportToolMode(&harness.CapabilitiesResponse{ToolExecutionModes: []harness.ToolExecutionMode{harness.ToolExecutionModeObserved}}, harness.ToolExecutionModeObserved) {
		t.Fatal("observed mode was not accepted")
	}
	if harnessCapabilitiesSupportToolMode(&harness.CapabilitiesResponse{ToolExecutionModes: []harness.ToolExecutionMode{harness.ToolExecutionModeObserved}}, harness.ToolExecutionModeBrokered) {
		t.Fatal("brokered mode accepted despite missing capability")
	}
	if !harnessCapabilitiesSupportToolMode(&harness.CapabilitiesResponse{ToolExecutionModes: []harness.ToolExecutionMode{harness.ToolExecutionModeObserved, harness.ToolExecutionModeBrokered}}, harness.ToolExecutionModeBrokered) {
		t.Fatal("brokered mode was not accepted when advertised")
	}
}

func TestHarnessProviderRejectsBrokeredAgentTasksUntilProviderIdentitySupported(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "brokered-agent", Namespace: "default", Annotations: map[string]string{
			labels.AnnotationHarnessBrokeredTools: "list_tools",
		}},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
	}
	r := newUnitReconciler(newTestScheme(), task)
	_, rejected, err := r.rejectUnsupportedServiceHarnessBrokeredTools(context.Background(), task)
	if err != nil {
		t.Fatalf("rejectUnsupportedServiceHarnessBrokeredTools: %v", err)
	}
	if !rejected {
		t.Fatal("brokered agent task was not rejected")
	}
}

func TestHarnessProviderRejectsBrokeredToolsUntilProviderIdentitySupported(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "brokered-service", Namespace: "default", Annotations: map[string]string{
			labels.AnnotationHarnessEndpoint:      "http://harness.default.svc:8080",
			labels.AnnotationHarnessProvider:      string(harness.ProviderKindKubernetesService),
			labels.AnnotationHarnessBrokeredTools: "list_tools",
		}},
		Spec:   corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, Prompt: "hello"},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
	}
	r := newUnitReconciler(newTestScheme(), task)
	if err := r.RuntimeSessionStore.CreateRuntimeSession(context.Background(), &harness.RuntimeSession{
		ID:    "runtime-brokered-reject",
		Owner: harness.RuntimeSessionOwner{Namespace: task.Namespace, SessionName: task.Name, ActiveTask: task.Name, Provider: harness.ProviderKindKubernetesService},
		State: harness.RuntimeSessionStateTurnRunning, CleanupPolicy: harness.RuntimeCleanupPolicyDelete,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateRuntimeSession: %v", err)
	}
	task.Annotations[labels.AnnotationHarnessRuntimeSession] = "runtime-brokered-reject"
	if _, err := r.runHarnessTask(context.Background(), task); err != nil {
		t.Fatalf("runHarnessTask: %v", err)
	}
	updated := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, updated); err != nil {
		t.Fatalf("Get updated task: %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhaseFailed || !strings.Contains(updated.Status.Message, "brokered tool execution") {
		t.Fatalf("status = %#v, want fail-closed brokered tool rejection", updated.Status)
	}
	runtimeSession, err := r.RuntimeSessionStore.GetRuntimeSession(context.Background(), task.Namespace, "runtime-brokered-reject")
	if err != nil {
		t.Fatalf("GetRuntimeSession: %v", err)
	}
	if runtimeSession.State != harness.RuntimeSessionStateUnhealthy {
		t.Fatalf("runtime state = %s, want unhealthy", runtimeSession.State)
	}
}

func TestHarnessProviderRecoversTerminalBeforeBrokeredServiceRejection(t *testing.T) {
	runtimeID := harness.RuntimeSessionID("runtime-brokered-terminal")
	turnID := harness.HarnessTurnID("turn-brokered-terminal")
	correlationID := "corr-brokered-terminal"
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "brokered-terminal", Namespace: "default", Annotations: map[string]string{
			labels.AnnotationHarnessEndpoint:       "http://harness.default.svc:8080",
			labels.AnnotationHarnessBrokeredTools:  "list_tools",
			labels.AnnotationHarnessRuntimeSession: string(runtimeID),
			labels.AnnotationHarnessTurn:           string(turnID),
			labels.AnnotationHarnessCorrelation:    correlationID,
		}},
		Spec:   corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, Prompt: "hello"},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
	}
	r := newUnitReconciler(newTestScheme(), task)
	frame := harness.HarnessEventFrame{
		Version:          harness.ProtocolVersion,
		Type:             harness.FrameTurnCompleted,
		RuntimeSessionID: runtimeID,
		TurnID:           turnID,
		CorrelationID:    correlationID,
		Seq:              1,
		Severity:         events.ExecutionEventSeverityInfo,
		Summary:          "turn completed",
		Completed:        &harness.TurnCompleted{Result: "ok", FinalEventSeq: 1},
	}
	mapped, err := harness.MapFrameToExecutionEvent(frame, harness.EventMapContext{Namespace: task.Namespace, TaskName: task.Name})
	if err != nil {
		t.Fatalf("MapFrameToExecutionEvent: %v", err)
	}
	if _, err := r.ExecutionEventStore.AppendExecutionEvent(context.Background(), mapped); err != nil {
		t.Fatalf("AppendExecutionEvent: %v", err)
	}
	if _, err := r.runHarnessTask(context.Background(), task); err != nil {
		t.Fatalf("runHarnessTask: %v", err)
	}
	updated := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, updated); err != nil {
		t.Fatalf("Get updated task: %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhaseSucceeded {
		t.Fatalf("phase = %s, want recovered Succeeded before brokered rejection", updated.Status.Phase)
	}
}

func TestHarnessProviderUsesBrokeredToolModeForAnnotatedServiceHarness(t *testing.T) {
	if got := harnessToolExecutionMode(&corev1alpha1.Task{}); got != harness.ToolExecutionModeObserved {
		t.Fatalf("mode without annotation = %s, want observed", got)
	}
	got := harnessToolExecutionMode(&corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
		labels.AnnotationHarnessBrokeredTools: "web_fetch",
	}}})
	if got != harness.ToolExecutionModeBrokered {
		t.Fatalf("mode with brokered tools = %s, want brokered", got)
	}
}

func TestHarnessClientForRecoveredTerminalSkipsRetryableFailureOutputRef(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "retry-output-ref", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, RetryPolicy: &corev1alpha1.RetryPolicy{MaxRetries: 1}},
		Status:     corev1alpha1.TaskStatus{Attempts: 1},
	}
	r := newUnitReconciler(newTestScheme(), task)
	client, _, handled, err := r.harnessClientForRecoveredTerminal(context.Background(), task, nil, &harness.TurnFailed{Retryable: true, OutputRef: "ref", Result: "preview"})
	if err != nil || handled || client != nil {
		t.Fatalf("client=%#v handled=%v err=%v, want retryable failure to skip outputRef client", client, handled, err)
	}
}

func TestHarnessClientForRecoveredTerminalSkipsSavedCompletedOutputRef(t *testing.T) {
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "saved-completed", Namespace: "default"}}
	r := newUnitReconciler(newTestScheme(), task)
	if err := r.ResultStore.SaveResult(context.Background(), task.Namespace, task.Name, []byte("full result")); err != nil {
		t.Fatalf("SaveResult: %v", err)
	}
	client, _, handled, err := r.harnessClientForRecoveredTerminal(context.Background(), task, &harness.TurnCompleted{Result: "preview", ResultTruncated: true, OutputRef: "ref"}, nil)
	if err != nil || handled || client != nil {
		t.Fatalf("client=%#v handled=%v err=%v, want saved completed output to skip recovery client", client, handled, err)
	}
}

func TestHarnessTerminalNeedsClientForOutputRefs(t *testing.T) {
	if harnessTerminalNeedsClient(&harness.TurnCompleted{Result: "ok", OutputRef: "ref"}) {
		t.Fatal("small completed inline result with outputRef should not require recovery client")
	}
	if !harnessTerminalNeedsClient(&harness.TurnCompleted{Result: "preview", ResultTruncated: true, OutputRef: "ref"}) {
		t.Fatal("truncated completed outputRef should require recovery client")
	}
}

func TestHarnessProviderTaskDeletionMarksRuntimeUnhealthy(t *testing.T) {
	scheme := newTestScheme()
	deletedAt := metav1.Now()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "harness-delete", Namespace: "default", Finalizers: []string{labels.TaskFinalizer}, DeletionTimestamp: &deletedAt,
			Annotations: map[string]string{
				labels.AnnotationHarnessEndpoint:       "http://missing.default.svc:8080",
				labels.AnnotationHarnessRuntimeSession: "runtime-delete",
				labels.AnnotationHarnessTurn:           "turn-delete",
				labels.AnnotationHarnessCorrelation:    "corr-delete",
			},
		},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, SessionRef: &corev1alpha1.SessionReference{Name: "session-delete"}},
	}
	r := newUnitReconciler(scheme, task)
	if err := r.RuntimeSessionStore.CreateRuntimeSession(context.Background(), &harness.RuntimeSession{
		ID:    "runtime-delete",
		Owner: harness.RuntimeSessionOwner{Namespace: "default", SessionName: "session-delete", ActiveTask: task.Name, Provider: harness.ProviderKindKubernetesService},
		State: harness.RuntimeSessionStateTurnRunning, CleanupPolicy: harness.RuntimeCleanupPolicyDelete,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateRuntimeSession: %v", err)
	}
	if _, err := r.handleDeletion(context.Background(), task); err != nil {
		t.Fatalf("handleDeletion: %v", err)
	}
	session, err := r.RuntimeSessionStore.GetRuntimeSession(context.Background(), "default", "runtime-delete")
	if err != nil {
		t.Fatalf("GetRuntimeSession: %v", err)
	}
	if session.State != harness.RuntimeSessionStateUnhealthy || session.Owner.ActiveTask != "" {
		t.Fatalf("session=%#v, want unhealthy inactive runtime after task deletion", session)
	}
}

func TestHarnessProviderTaskDeletionSkipsFinalizedRuntimeSession(t *testing.T) {
	scheme := newTestScheme()
	deletedAt := metav1.Now()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "harness-delete-retained", Namespace: "default", Finalizers: []string{labels.TaskFinalizer}, DeletionTimestamp: &deletedAt,
			Annotations: map[string]string{labels.AnnotationHarnessRuntimeSession: "runtime-retained"},
		},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, SessionRef: &corev1alpha1.SessionReference{Name: "session-delete"}},
	}
	r := newUnitReconciler(scheme, task)
	if err := r.RuntimeSessionStore.CreateRuntimeSession(context.Background(), &harness.RuntimeSession{
		ID:    "runtime-retained",
		Owner: harness.RuntimeSessionOwner{Namespace: "default", SessionName: "session-delete", Provider: harness.ProviderKindKubernetesService},
		State: harness.RuntimeSessionStateRetained, CleanupPolicy: harness.RuntimeCleanupPolicyRetain,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateRuntimeSession: %v", err)
	}
	if _, err := r.handleDeletion(context.Background(), task); err != nil {
		t.Fatalf("handleDeletion: %v", err)
	}
	session, err := r.RuntimeSessionStore.GetRuntimeSession(context.Background(), "default", "runtime-retained")
	if err != nil {
		t.Fatalf("GetRuntimeSession: %v", err)
	}
	if session.State != harness.RuntimeSessionStateRetained {
		t.Fatalf("session=%#v, want retained runtime left finalized", session)
	}
}

func TestHarnessProviderPendingInProgressRequeuesOrTimesOutWithoutRestartingTurn(t *testing.T) {
	scheme := newTestScheme()
	startedAt := time.Now().UTC()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "harness-running",
			Namespace: "default",
			Annotations: map[string]string{
				labels.AnnotationHarnessEndpoint:       "http://harness.default.svc:8080",
				labels.AnnotationHarnessRuntimeSession: "runtime-a",
				labels.AnnotationHarnessTurn:           "turn-a",
				labels.AnnotationHarnessCorrelation:    "corr-a",
				labels.AnnotationHarnessTurnStartedAt:  startedAt.Format(time.RFC3339Nano),
			},
		},
		Spec:   corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
		Status: harnessStartedPendingStatus(startedAt),
	}
	r := newUnitReconciler(scheme, task)
	result, err := r.handlePending(context.Background(), task)
	if err != nil {
		t.Fatalf("handlePending() error = %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Fatalf("handlePending() result = %#v, want requeue while harness turn may still be active", result)
	}

	if err := r.RuntimeSessionStore.CreateRuntimeSession(context.Background(), &harness.RuntimeSession{
		ID: "runtime-a",
		Owner: harness.RuntimeSessionOwner{
			Namespace: "default", SessionName: "harness-running", ActiveTask: task.Name, Provider: harness.ProviderKindKubernetesService,
		},
		State: harness.RuntimeSessionStateTurnRunning, CleanupPolicy: harness.RuntimeCleanupPolicyDelete,
		CreatedAt: startedAt, UpdatedAt: startedAt,
	}); err != nil {
		t.Fatalf("CreateRuntimeSession: %v", err)
	}
	task.Annotations[labels.AnnotationHarnessTurnStartedAt] = time.Now().Add(-defaultHarnessTurnTimeout - 2*time.Minute).UTC().Format(time.RFC3339Nano)
	_, err = r.handlePending(context.Background(), task)
	if err != nil {
		t.Fatalf("handlePending(timeout) error = %v", err)
	}
	updated := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "harness-running"}, updated); err != nil {
		t.Fatalf("get task: %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhaseFailed {
		t.Fatalf("updated phase = %s, want Failed", updated.Status.Phase)
	}
	runtimeSession, err := r.RuntimeSessionStore.GetRuntimeSession(context.Background(), "default", "runtime-a")
	if err != nil {
		t.Fatalf("GetRuntimeSession: %v", err)
	}
	if runtimeSession.State != harness.RuntimeSessionStateUnhealthy || runtimeSession.Owner.ActiveTask != "" {
		t.Fatalf("runtimeSession = %#v, want unhealthy inactive after timeout", runtimeSession)
	}
}

func TestHarnessProviderPendingInProgressResumesPersistedTurn(t *testing.T) {
	server := harnesstest.NewFakeHarnessServer(harnesstest.FakeHarnessConfig{Behavior: harnesstest.BehaviorSuccess})
	defer server.Close()
	runtimeID := harness.RuntimeSessionID("runtime-resume")
	turnID := harness.HarnessTurnID("turn-resume")
	correlationID := "corr-resume"
	client, err := harness.NewClient(server.URL())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := client.StartTurn(context.Background(), harness.StartTurnRequest{
		Version:          harness.ProtocolVersion,
		Namespace:        "default",
		TaskName:         "harness-resume",
		SessionName:      "harness-resume",
		RuntimeSessionID: runtimeID,
		TurnID:           turnID,
		CorrelationID:    correlationID,
		Deadline:         time.Now().Add(time.Minute),
		AuthIdentity:     harness.AuthIdentity{Subject: "test"},
	}); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "harness-resume",
			Namespace: "default",
			Annotations: map[string]string{
				labels.AnnotationHarnessEndpoint:       server.URL(),
				labels.AnnotationHarnessRuntimeSession: string(runtimeID),
				labels.AnnotationHarnessTurn:           string(turnID),
				labels.AnnotationHarnessCorrelation:    correlationID,
				labels.AnnotationHarnessTurnStartedAt:  time.Now().UTC().Format(time.RFC3339Nano),
			},
		},
		Spec:   corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
		Status: harnessStartedPendingStatus(time.Now().UTC()),
	}
	r := newUnitReconciler(scheme, task)
	allowInsecureLoopbackHarnessEndpoint(t, r, task.Annotations[labels.AnnotationHarnessEndpoint])
	if _, err := r.handlePending(context.Background(), task); err != nil {
		t.Fatalf("handlePending(resume) error type=%T value=%#v", err, err)
	}
	updated := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "harness-resume"}, updated); err != nil {
		t.Fatalf("get updated task: %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhaseSucceeded {
		t.Fatalf("phase = %s, want Succeeded", updated.Status.Phase)
	}
}

func TestHarnessProviderRunningResumeRejectsStaleFrames(t *testing.T) {
	runtimeID := harness.RuntimeSessionID("runtime-stale")
	turnID := harness.HarnessTurnID("turn-stale")
	correlationID := "corr-stale"
	startedAt := time.Now().UTC()
	seenAfterSeq := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != harness.TurnsPath+"/"+string(turnID)+"/events" {
			harness.WriteError(w, http.StatusNotFound, "not found")
			return
		}
		seenAfterSeq = r.URL.Query().Get("afterSeq")
		w.Header().Set("Content-Type", "text/event-stream")
		stale := harness.HarnessEventFrame{
			Version:          harness.ProtocolVersion,
			Type:             harness.FrameRuntimeOutput,
			RuntimeSessionID: runtimeID,
			TurnID:           turnID,
			CorrelationID:    correlationID,
			Seq:              2,
			CreatedAt:        startedAt.Add(2 * time.Millisecond),
			Severity:         events.ExecutionEventSeverityInfo,
			Summary:          "stale output",
			ContentText:      "duplicate",
		}
		completed := harness.HarnessEventFrame{
			Version:          harness.ProtocolVersion,
			Type:             harness.FrameTurnCompleted,
			RuntimeSessionID: runtimeID,
			TurnID:           turnID,
			CorrelationID:    correlationID,
			Seq:              3,
			CreatedAt:        startedAt.Add(3 * time.Millisecond),
			Severity:         events.ExecutionEventSeverityInfo,
			Summary:          "turn completed",
			Completed:        &harness.TurnCompleted{Result: "ok", FinalEventSeq: 3},
		}
		_ = harness.WriteSSEFrame(w, stale)
		_ = harness.WriteSSEFrame(w, completed)
		_ = harness.WriteSSEDone(w)
	}))
	defer server.Close()

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "harness-stale",
			Namespace: "default",
			Annotations: map[string]string{
				labels.AnnotationHarnessEndpoint:       server.URL,
				labels.AnnotationHarnessRuntimeSession: string(runtimeID),
				labels.AnnotationHarnessTurn:           string(turnID),
				labels.AnnotationHarnessCorrelation:    correlationID,
				labels.AnnotationHarnessTurnStartedAt:  startedAt.Format(time.RFC3339Nano),
			},
		},
		Spec:   corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
		Status: harnessStartedPendingStatus(startedAt),
	}
	r := newUnitReconciler(newTestScheme(), task)
	allowInsecureLoopbackHarnessEndpoint(t, r, task.Annotations[labels.AnnotationHarnessEndpoint])
	if err := r.RuntimeSessionStore.CreateRuntimeSession(context.Background(), &harness.RuntimeSession{
		ID: runtimeID,
		Owner: harness.RuntimeSessionOwner{
			Namespace: "default", SessionName: task.Name, ActiveTask: task.Name, Provider: harness.ProviderKindKubernetesService,
		},
		State: harness.RuntimeSessionStateTurnRunning, CleanupPolicy: harness.RuntimeCleanupPolicyDelete,
		CreatedAt: startedAt, UpdatedAt: startedAt,
	}); err != nil {
		t.Fatalf("CreateRuntimeSession: %v", err)
	}
	seed := harness.HarnessEventFrame{
		Version:          harness.ProtocolVersion,
		Type:             harness.FrameRuntimeOutput,
		RuntimeSessionID: runtimeID,
		TurnID:           turnID,
		CorrelationID:    correlationID,
		Seq:              2,
		CreatedAt:        startedAt.Add(2 * time.Millisecond),
		Severity:         events.ExecutionEventSeverityInfo,
		Summary:          "already persisted",
		ContentText:      "persisted",
	}
	mapped, err := harness.MapFrameToExecutionEvent(seed, harness.EventMapContext{
		Namespace: task.Namespace, TaskName: task.Name, SessionName: task.Name,
	})
	if err != nil {
		t.Fatalf("MapFrameToExecutionEvent(seed): %v", err)
	}
	if _, err := r.ExecutionEventStore.AppendExecutionEvent(context.Background(), mapped); err != nil {
		t.Fatalf("AppendExecutionEvent(seed): %v", err)
	}

	if _, err := r.handlePending(context.Background(), task); err != nil {
		t.Fatalf("handlePending(stale resume) error = %v", err)
	}
	if seenAfterSeq != "2" {
		t.Fatalf("afterSeq = %q, want 2", seenAfterSeq)
	}
	updated := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, updated); err != nil {
		t.Fatalf("get updated task: %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhaseFailed {
		t.Fatalf("phase = %s, want Failed for stale resumed frame", updated.Status.Phase)
	}
	if !strings.Contains(updated.Status.Message, "non-monotonic harness frame seq 2 after 2") {
		t.Fatalf("message = %q, want non-monotonic stale-frame failure", updated.Status.Message)
	}
	listed, err := r.ExecutionEventStore.ListExecutionEvents(context.Background(), store.ExecutionEventFilter{Namespace: task.Namespace, StreamID: task.Name})
	if err != nil {
		t.Fatalf("ListExecutionEvents: %v", err)
	}
	seq2Frames := 0
	terminalFrames := 0
	for _, event := range listed {
		seq, completed, failed, cancelled := harnessFrameStateFromEvent(event, runtimeID, turnID)
		if seq == 2 {
			seq2Frames++
		}
		if completed != nil || failed != nil || cancelled {
			terminalFrames++
		}
	}
	if seq2Frames != 1 {
		t.Fatalf("seq2Frames = %d, want only the previously persisted frame", seq2Frames)
	}
	if terminalFrames != 0 {
		t.Fatalf("terminalFrames = %d, want stale stream rejected before terminal append", terminalFrames)
	}
}

func TestHarnessFrameStateRecognizesAgentRuntimeCancelled(t *testing.T) {
	content, _ := json.Marshal(map[string]any{
		"harness": map[string]any{"runtimeSessionID": "runtime-a", "turnID": "turn-a", "seq": 7},
	})
	seq, completed, failed, cancelled := harnessFrameStateFromEvent(store.ExecutionEvent{
		Type:    events.ExecutionEventTypeAgentRuntimeCancelled,
		Content: content,
	}, "runtime-a", "turn-a")
	if seq != 7 || completed != nil || failed != nil || !cancelled {
		t.Fatalf("seq=%d completed=%#v failed=%#v cancelled=%v, want cancelled terminal", seq, completed, failed, cancelled)
	}
}

func TestDeterministicHarnessResumeErrorClassification(t *testing.T) {
	if !isDeterministicHarnessResumeError(context.Background(), fmt.Errorf("harness frame identity does not match persisted task turn")) {
		t.Fatal("deterministic callback error was not classified as deterministic")
	}
	if isDeterministicHarnessResumeError(context.Background(), fmt.Errorf("temporary event store append failure")) {
		t.Fatal("generic append/store error classified as deterministic")
	}
	if isDeterministicHarnessResumeError(context.Background(), harness.ClientError{Op: "stream_frames", StatusCode: http.StatusNotFound, Message: "not found"}) {
		t.Fatal("client/status error classified as deterministic")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if isDeterministicHarnessResumeError(ctx, fmt.Errorf("context cancelled path")) {
		t.Fatal("context cancellation classified as deterministic")
	}
}

func TestDeterministicHarnessResumeErrorClassifiesMalformedClientFrame(t *testing.T) {
	if !isDeterministicHarnessResumeError(context.Background(), harness.ClientError{Op: "stream_frames", Message: "decode harness frame: invalid JSON"}) {
		t.Fatal("malformed frame client error was not classified as deterministic")
	}
	if isDeterministicHarnessResumeError(context.Background(), harness.ClientError{Op: "stream_frames", Message: "decode harness frame: unexpected EOF"}) {
		t.Fatal("transient truncated frame stream classified as deterministic")
	}
	if isDeterministicHarnessResumeError(context.Background(), harness.ClientError{Op: "stream_frames", StatusCode: http.StatusServiceUnavailable, Message: "unavailable"}) {
		t.Fatal("HTTP client error classified as deterministic")
	}
}

func TestHarnessTerminalMissingMarksRuntimeUnhealthy(t *testing.T) {
	scheme := newTestScheme()
	runtimeID := harness.RuntimeSessionID("runtime-terminal-missing")
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "harness-terminal-missing", Namespace: "default", Annotations: map[string]string{
			labels.AnnotationHarnessRuntimeSession: string(runtimeID),
		}},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
	}
	r := newUnitReconciler(scheme, task)
	if err := r.RuntimeSessionStore.CreateRuntimeSession(context.Background(), &harness.RuntimeSession{
		ID: runtimeID,
		Owner: harness.RuntimeSessionOwner{
			Namespace: "default", SessionName: task.Name, ActiveTask: task.Name, Provider: harness.ProviderKindKubernetesService,
		},
		State: harness.RuntimeSessionStateTurnRunning, CleanupPolicy: harness.RuntimeCleanupPolicyDelete,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateRuntimeSession: %v", err)
	}
	_, err := r.handleHarnessTurnRunResult(context.Background(), task, nil, "turn-terminal-missing", harness.TurnRunResult{Accepted: &harness.StartTurnResponse{Accepted: true}}, fmt.Errorf("harness turn ended without terminal frame"))
	if err != nil {
		t.Fatalf("handleHarnessTurnRunResult: %v", err)
	}
	updatedRuntime, err := r.RuntimeSessionStore.GetRuntimeSession(context.Background(), task.Namespace, runtimeID)
	if err != nil {
		t.Fatalf("GetRuntimeSession: %v", err)
	}
	if updatedRuntime.State != harness.RuntimeSessionStateUnhealthy || updatedRuntime.Owner.ActiveTask != "" {
		t.Fatalf("runtime = %#v, want unhealthy inactive", updatedRuntime)
	}
	updatedTask := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, updatedTask); err != nil {
		t.Fatalf("get task: %v", err)
	}
	if updatedTask.Status.Phase != corev1alpha1.TaskPhaseFailed {
		t.Fatalf("phase = %s, want Failed", updatedTask.Status.Phase)
	}
}

func TestHarnessTurnIdentityTimedOutUsesConditionTimestamp(t *testing.T) {
	old := metav1.NewTime(time.Now().UTC().Add(-2 * defaultHarnessTurnTimeout))
	task := &corev1alpha1.Task{Status: corev1alpha1.TaskStatus{Conditions: []metav1.Condition{{
		Type: ConditionTypeJobCreated, Status: metav1.ConditionTrue, Reason: "HarnessProviderStarting", LastTransitionTime: old,
	}}}}
	if !harnessTurnIdentityTimedOut(task) {
		t.Fatal("harnessTurnIdentityTimedOut() = false, want true from condition timestamp fallback")
	}
	task.Annotations = map[string]string{labels.AnnotationHarnessTurnIdentityStartedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if harnessTurnIdentityTimedOut(task) {
		t.Fatal("fresh turn identity timestamp was classified as timed out")
	}
}

func TestHarnessRuntimeOwnerSessionNamePreservesLegacyAndPrefixedNoSessionKeys(t *testing.T) {
	const prefixedTaskSessionName = "task/task-a"
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "task-a"}}
	if got := harnessRuntimeOwnerSessionName(task, ""); got != prefixedTaskSessionName {
		t.Fatalf("new owner session key = %q, want %s", got, prefixedTaskSessionName)
	}
	task.Status.Conditions = []metav1.Condition{{Type: ConditionTypeJobCreated, Status: metav1.ConditionTrue, Reason: "HarnessProviderStarting"}}
	task.Annotations = map[string]string{
		labels.AnnotationHarnessRuntimeSession: "task-task-a-12345678",
		labels.AnnotationHarnessTurn:           "turn-a",
		labels.AnnotationHarnessCorrelation:    "corr-a",
	}
	if got := harnessRuntimeOwnerSessionName(task, ""); got != prefixedTaskSessionName {
		t.Fatalf("prefixed annotated owner session key = %q, want %s", got, prefixedTaskSessionName)
	}
	task.Annotations[labels.AnnotationHarnessRuntimeSession] = "task-a-12345678"
	if got := harnessRuntimeOwnerSessionName(task, ""); got != "task-a" {
		t.Fatalf("legacy annotated owner session key = %q, want task-a", got)
	}
	delete(task.Annotations, labels.AnnotationHarnessTurn)
	if got := harnessRuntimeOwnerSessionName(task, ""); got != prefixedTaskSessionName {
		t.Fatalf("unowned annotated owner session key = %q, want %s", got, prefixedTaskSessionName)
	}
	const explicitSessionName = "session-a"
	if got := harnessRuntimeOwnerSessionName(task, explicitSessionName); got != explicitSessionName {
		t.Fatalf("explicit session owner key = %q, want %s", got, explicitSessionName)
	}
}

func TestHarnessProviderAcceptedTurnMarksTaskRunning(t *testing.T) {
	scheme := newTestScheme()
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "harness-running-accepted", Namespace: "default"}, Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending}}
	r := newUnitReconciler(scheme, task)
	acceptedAt := time.Date(2026, 6, 21, 10, 0, 0, 0, time.UTC)
	if err := r.markHarnessTaskTurnAccepted(context.Background(), task, acceptedAt); err != nil {
		t.Fatalf("markHarnessTaskTurnAccepted: %v", err)
	}
	updated := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, updated); err != nil {
		t.Fatalf("get task: %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhaseRunning {
		t.Fatalf("phase = %s, want Running", updated.Status.Phase)
	}
	condition := meta.FindStatusCondition(updated.Status.Conditions, ConditionTypeJobCreated)
	if condition == nil || condition.Reason != "HarnessProviderStarted" {
		t.Fatalf("condition = %#v, want HarnessProviderStarted", condition)
	}
}

func TestCancelHarnessProviderTurnCancelsServiceHarness(t *testing.T) {
	server := harnesstest.NewFakeHarnessServer(harnesstest.FakeHarnessConfig{Behavior: harnesstest.BehaviorLongRunning})
	defer server.Close()
	runtimeID := harness.RuntimeSessionID("runtime-cancel")
	turnID := harness.HarnessTurnID("turn-cancel")
	correlationID := "corr-cancel"
	client, err := harness.NewClient(server.URL())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := client.StartTurn(context.Background(), harness.StartTurnRequest{
		Version:          harness.ProtocolVersion,
		Namespace:        "default",
		TaskName:         "harness-cancel",
		SessionName:      "harness-cancel",
		RuntimeSessionID: runtimeID,
		TurnID:           turnID,
		CorrelationID:    correlationID,
		Deadline:         time.Now().Add(time.Minute),
		AuthIdentity:     harness.AuthIdentity{Subject: "test"},
	}); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "harness-cancel", Namespace: "default", Annotations: map[string]string{
			labels.AnnotationHarnessEndpoint:       server.URL(),
			labels.AnnotationHarnessRuntimeSession: string(runtimeID),
			labels.AnnotationHarnessTurn:           string(turnID),
			labels.AnnotationHarnessCorrelation:    correlationID,
			labels.AnnotationHarnessTurnStartedAt:  time.Now().UTC().Format(time.RFC3339Nano),
		}},
		Status: corev1alpha1.TaskStatus{Conditions: []metav1.Condition{{Type: ConditionTypeJobCreated, Status: metav1.ConditionTrue, Reason: "HarnessProviderStarted"}}},
	}
	r := newUnitReconciler(newTestScheme(), task)
	allowInsecureLoopbackHarnessEndpoint(t, r, task.Annotations[labels.AnnotationHarnessEndpoint])
	if err := r.cancelHarnessProviderTurn(context.Background(), task, "test cancel"); err != nil {
		t.Fatalf("cancelHarnessProviderTurn: %v", err)
	}
	cancelled := false
	if err := client.StreamFrames(context.Background(), turnID, 0, func(frame harness.HarnessEventFrame) error {
		if frame.Type == harness.FrameTurnCancelled {
			cancelled = true
		}
		return nil
	}); err != nil {
		t.Fatalf("StreamFrames: %v", err)
	}
	if !cancelled {
		t.Fatal("cancelled frame not observed after provider cancel")
	}
}

func TestHarnessProviderDoesNotStampSyntheticSessionOnEvents(t *testing.T) {
	server := harnesstest.NewFakeHarnessServer(harnesstest.FakeHarnessConfig{Behavior: harnesstest.BehaviorSuccess})
	defer server.Close()
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "harness-nosession", Namespace: "default", Annotations: map[string]string{
			labels.AnnotationHarnessEndpoint: server.URL(),
		}},
		Spec:   corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, Prompt: "hello"},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
	}
	r := newUnitReconciler(scheme, task)
	allowInsecureLoopbackHarnessEndpoint(t, r, task.Annotations[labels.AnnotationHarnessEndpoint])
	if _, err := r.handlePending(context.Background(), task); err != nil {
		t.Fatalf("handlePending() error = %v", err)
	}
	requests := server.StartTurnRequests()
	if len(requests) != 1 || requests[0].SessionName != task.Name {
		t.Fatalf("StartTurnRequests = %#v, want protocol session fallback to task name", requests)
	}
	listed, err := r.ExecutionEventStore.ListExecutionEvents(context.Background(), store.ExecutionEventFilter{Namespace: task.Namespace, StreamID: task.Name})
	if err != nil {
		t.Fatalf("ListExecutionEvents: %v", err)
	}
	if len(listed) == 0 {
		t.Fatal("no harness events persisted")
	}
	for _, event := range listed {
		if event.SessionName != "" {
			t.Fatalf("event %s SessionName = %q, want empty without real SessionRef", event.Type, event.SessionName)
		}
	}
}

func TestHarnessProviderRunningTaskResumesPersistedTurn(t *testing.T) {
	server := harnesstest.NewFakeHarnessServer(harnesstest.FakeHarnessConfig{Behavior: harnesstest.BehaviorSuccess})
	defer server.Close()
	runtimeID := harness.RuntimeSessionID("runtime-running-resume")
	turnID := harness.HarnessTurnID("turn-running-resume")
	correlationID := "corr-running-resume"
	client, err := harness.NewClient(server.URL())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := client.StartTurn(context.Background(), harness.StartTurnRequest{
		Version:          harness.ProtocolVersion,
		Namespace:        "default",
		TaskName:         "harness-running-resume",
		SessionName:      "harness-running-resume",
		RuntimeSessionID: runtimeID,
		TurnID:           turnID,
		CorrelationID:    correlationID,
		Deadline:         time.Now().Add(time.Minute),
		AuthIdentity:     harness.AuthIdentity{Subject: "test"},
	}); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "harness-running-resume", Namespace: "default", Finalizers: []string{labels.TaskFinalizer}, Annotations: map[string]string{
			labels.AnnotationHarnessEndpoint:       server.URL(),
			labels.AnnotationHarnessRuntimeSession: string(runtimeID),
			labels.AnnotationHarnessTurn:           string(turnID),
			labels.AnnotationHarnessCorrelation:    correlationID,
			labels.AnnotationHarnessTurnStartedAt:  time.Now().UTC().Format(time.RFC3339Nano),
		}},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, Prompt: "hello"},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning, Conditions: []metav1.Condition{{
			Type: ConditionTypeJobCreated, Status: metav1.ConditionTrue, Reason: "HarnessProviderStarted",
		}}},
	}
	r := newUnitReconciler(scheme, task)
	allowInsecureLoopbackHarnessEndpoint(t, r, task.Annotations[labels.AnnotationHarnessEndpoint])
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: task.Namespace, Name: task.Name}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	updated := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, updated); err != nil {
		t.Fatalf("get task: %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhaseSucceeded {
		t.Fatalf("phase = %s, want Succeeded after running harness resume", updated.Status.Phase)
	}
}

func TestHarnessProviderRetainsAndReusesRuntimeSession(t *testing.T) {
	server := harnesstest.NewFakeHarnessServer(harnesstest.FakeHarnessConfig{Behavior: harnesstest.BehaviorSuccess})
	defer server.Close()
	scheme := newTestScheme()
	first := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "harness-reuse-1", Namespace: "default", Annotations: map[string]string{
			labels.AnnotationHarnessEndpoint:    server.URL(),
			labels.AnnotationHarnessReusePolicy: "retain",
		}},
		Spec:   corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, Prompt: "one", SessionRef: &corev1alpha1.SessionReference{Name: "session-reuse", Create: true}},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
	}
	second := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "harness-reuse-2", Namespace: "default", Annotations: map[string]string{
			labels.AnnotationHarnessEndpoint:    server.URL(),
			labels.AnnotationHarnessReusePolicy: "retain",
		}},
		Spec:   corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, Prompt: "two", SessionRef: &corev1alpha1.SessionReference{Name: "session-reuse", Create: true}},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
	}
	r := newUnitReconciler(scheme, first, second)
	allowInsecureLoopbackHarnessEndpoint(t, r, first.Annotations[labels.AnnotationHarnessEndpoint])
	if _, err := r.handlePending(context.Background(), first); err != nil {
		t.Fatalf("first handlePending: %v", err)
	}
	if _, err := r.handlePending(context.Background(), second); err != nil {
		t.Fatalf("second handlePending: %v", err)
	}
	requests := server.StartTurnRequests()
	if len(requests) != 2 {
		t.Fatalf("start turn requests = %d, want 2", len(requests))
	}
	if requests[0].RuntimeSessionID != requests[1].RuntimeSessionID {
		t.Fatalf("runtime session ids = %q/%q, want reuse", requests[0].RuntimeSessionID, requests[1].RuntimeSessionID)
	}
	sessions, _, err := r.RuntimeSessionStore.ListRuntimeSessions(context.Background(), harness.RuntimeSessionFilter{Namespace: "default", SessionName: "session-reuse", IncludeDeleted: true})
	if err != nil {
		t.Fatalf("ListRuntimeSessions: %v", err)
	}
	if len(sessions) != 1 || sessions[0].State != harness.RuntimeSessionStateRetained {
		t.Fatalf("runtime sessions = %#v, want retained reused runtime", sessions)
	}
}

func TestHarnessProviderPendingIdentityWithoutAcceptanceStartsTurnOnce(t *testing.T) {
	server := harnesstest.NewFakeHarnessServer(harnesstest.FakeHarnessConfig{Behavior: harnesstest.BehaviorSuccess})
	defer server.Close()
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "harness-preaccepted", Namespace: "default", Annotations: map[string]string{
			labels.AnnotationHarnessEndpoint:       server.URL(),
			labels.AnnotationHarnessRuntimeSession: "runtime-preaccepted",
			labels.AnnotationHarnessTurn:           "turn-preaccepted",
			labels.AnnotationHarnessCorrelation:    "corr-preaccepted",
		}},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, Prompt: "hello", SessionRef: &corev1alpha1.SessionReference{Name: "session-preaccepted", Create: true}},
		Status: corev1alpha1.TaskStatus{
			Phase:    corev1alpha1.TaskPhasePending,
			Attempts: 1,
			Conditions: []metav1.Condition{{
				Type: ConditionTypeJobCreated, Status: metav1.ConditionTrue, Reason: "HarnessProviderStarting",
			}},
		},
	}
	r := newUnitReconciler(scheme, task)
	allowInsecureLoopbackHarnessEndpoint(t, r, task.Annotations[labels.AnnotationHarnessEndpoint])
	if err := r.RuntimeSessionStore.CreateRuntimeSession(context.Background(), &harness.RuntimeSession{
		ID: "runtime-preaccepted",
		Owner: harness.RuntimeSessionOwner{
			Namespace: "default", SessionName: "session-preaccepted", ActiveTask: task.Name, Provider: harness.ProviderKindKubernetesService,
		},
		State: harness.RuntimeSessionStateTurnRunning, CleanupPolicy: harness.RuntimeCleanupPolicyDelete,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateRuntimeSession: %v", err)
	}
	if _, err := r.handlePending(context.Background(), task); err != nil {
		t.Fatalf("handlePending() error = %v", err)
	}
	requests := server.StartTurnRequests()
	if len(requests) != 1 || requests[0].RuntimeSessionID != "runtime-preaccepted" || requests[0].TurnID != "turn-preaccepted" {
		t.Fatalf("start requests=%#v, want one start using pre-persisted identity", requests)
	}
}

func TestHarnessProviderAcceptedIdentityResumesWithoutDuplicateStart(t *testing.T) {
	server := harnesstest.NewFakeHarnessServer(harnesstest.FakeHarnessConfig{Behavior: harnesstest.BehaviorSuccess})
	defer server.Close()
	runtimeID := harness.RuntimeSessionID("runtime-accepted-before-marker")
	turnID := harness.HarnessTurnID("turn-accepted-before-marker")
	correlationID := "corr-accepted-before-marker"
	client, err := harness.NewClient(server.URL())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := client.StartTurn(context.Background(), harness.StartTurnRequest{
		Version:          harness.ProtocolVersion,
		Namespace:        "default",
		TaskName:         "harness-accepted-before-marker",
		SessionName:      "session-accepted-before-marker",
		RuntimeSessionID: runtimeID,
		TurnID:           turnID,
		CorrelationID:    correlationID,
		Deadline:         time.Now().Add(time.Minute),
		AuthIdentity:     harness.AuthIdentity{Subject: "test"},
	}); err != nil {
		t.Fatalf("StartTurn(seed): %v", err)
	}
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "harness-accepted-before-marker", Namespace: "default", Annotations: map[string]string{
			labels.AnnotationHarnessEndpoint:       server.URL(),
			labels.AnnotationHarnessRuntimeSession: string(runtimeID),
			labels.AnnotationHarnessTurn:           string(turnID),
			labels.AnnotationHarnessCorrelation:    correlationID,
			labels.AnnotationHarnessTurnStartedAt:  time.Now().UTC().Format(time.RFC3339Nano),
		}},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, Prompt: "hello", SessionRef: &corev1alpha1.SessionReference{Name: "session-accepted-before-marker", Create: true}},
		Status: corev1alpha1.TaskStatus{
			Phase:    corev1alpha1.TaskPhasePending,
			Attempts: 1,
			Conditions: []metav1.Condition{{
				Type: ConditionTypeJobCreated, Status: metav1.ConditionTrue, Reason: "HarnessProviderStarting",
			}},
		},
	}
	r := newUnitReconciler(scheme, task)
	allowInsecureLoopbackHarnessEndpoint(t, r, task.Annotations[labels.AnnotationHarnessEndpoint])
	if err := r.RuntimeSessionStore.CreateRuntimeSession(context.Background(), &harness.RuntimeSession{
		ID: runtimeID,
		Owner: harness.RuntimeSessionOwner{
			Namespace: "default", SessionName: "session-accepted-before-marker", ActiveTask: task.Name, Provider: harness.ProviderKindKubernetesService,
		},
		State: harness.RuntimeSessionStateTurnRunning, CleanupPolicy: harness.RuntimeCleanupPolicyDelete,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateRuntimeSession: %v", err)
	}
	if _, err := r.handlePending(context.Background(), task); err != nil {
		t.Fatalf("handlePending() error = %v", err)
	}
	requests := server.StartTurnRequests()
	if len(requests) != 1 {
		t.Fatalf("StartTurnRequests = %d, want no duplicate StartTurn", len(requests))
	}
	updated := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: task.Name}, updated); err != nil {
		t.Fatalf("Get task: %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhaseSucceeded {
		t.Fatalf("phase = %s, want Succeeded", updated.Status.Phase)
	}
}

func TestHarnessProviderAmbiguousStartTurnFailureRequeuesForResume(t *testing.T) {
	server := harnesstest.NewFakeHarnessServer(harnesstest.FakeHarnessConfig{Behavior: harnesstest.BehaviorStartTurnError})
	defer server.Close()
	scheme := newTestScheme()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "harness-start-fail", Namespace: "default", Annotations: map[string]string{
			labels.AnnotationHarnessEndpoint:    server.URL(),
			labels.AnnotationHarnessReusePolicy: "retain",
		}},
		Spec:   corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, Prompt: "hello", SessionRef: &corev1alpha1.SessionReference{Name: "session-start-fail", Create: true}},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
	}
	r := newUnitReconciler(scheme, task)
	allowInsecureLoopbackHarnessEndpoint(t, r, task.Annotations[labels.AnnotationHarnessEndpoint])
	if _, err := r.handlePending(context.Background(), task); err != nil {
		t.Fatalf("handlePending() error = %v", err)
	}
	sessions, _, err := r.RuntimeSessionStore.ListRuntimeSessions(context.Background(), harness.RuntimeSessionFilter{Namespace: "default", SessionName: "session-start-fail", IncludeDeleted: true})
	if err != nil {
		t.Fatalf("ListRuntimeSessions: %v", err)
	}
	if len(sessions) != 1 || sessions[0].State != harness.RuntimeSessionStateTurnRunning || sessions[0].Owner.ActiveTask != task.Name {
		t.Fatalf("runtime sessions=%#v, want active runtime retained for resume after ambiguous StartTurn failure", sessions)
	}
}

func TestHarnessProviderRetainedRuntimeCarriesResidentStateAcrossTurns(t *testing.T) {
	server := harnesstest.NewFakeHarnessServer(harnesstest.FakeHarnessConfig{Behavior: harnesstest.BehaviorResidentState})
	defer server.Close()
	scheme := newTestScheme()
	first := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "harness-resident-1", Namespace: "default", Annotations: map[string]string{
			labels.AnnotationHarnessEndpoint:    server.URL(),
			labels.AnnotationHarnessReusePolicy: "retain",
		}},
		Spec:   corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, Prompt: "write state.txt hello-resident", SessionRef: &corev1alpha1.SessionReference{Name: "session-resident", Create: true}},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
	}
	second := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "harness-resident-2", Namespace: "default", Annotations: map[string]string{
			labels.AnnotationHarnessEndpoint:    server.URL(),
			labels.AnnotationHarnessReusePolicy: "retain",
		}},
		Spec:   corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, Prompt: "read state.txt", SessionRef: &corev1alpha1.SessionReference{Name: "session-resident", Create: true}},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
	}
	r := newUnitReconciler(scheme, first, second)
	allowInsecureLoopbackHarnessEndpoint(t, r, first.Annotations[labels.AnnotationHarnessEndpoint])
	if _, err := r.handlePending(context.Background(), first); err != nil {
		t.Fatalf("first handlePending: %v", err)
	}
	if _, err := r.handlePending(context.Background(), second); err != nil {
		t.Fatalf("second handlePending: %v", err)
	}
	firstResult, err := r.ResultStore.GetResult(context.Background(), "default", first.Name)
	if err != nil || !strings.Contains(string(firstResult), "wrote state.txt") {
		t.Fatalf("first result=%q err=%v, want write acknowledgement", firstResult, err)
	}
	secondResult, err := r.ResultStore.GetResult(context.Background(), "default", second.Name)
	if err != nil || !strings.Contains(string(secondResult), "hello-resident") {
		t.Fatalf("second result=%q err=%v, want retained runtime state", secondResult, err)
	}
	requests := server.StartTurnRequests()
	if len(requests) != 2 || requests[0].RuntimeSessionID != requests[1].RuntimeSessionID {
		t.Fatalf("start turn requests=%#v, want same retained runtime session", requests)
	}
}

func TestHarnessGeneratedIDsIgnoreUserAnnotationsBeforeControllerOwnership(t *testing.T) {
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
		Namespace: "default", Name: "task-a", UID: types.UID("uid-a"),
		Annotations: map[string]string{
			labels.AnnotationHarnessRuntimeSession: "user-runtime",
			labels.AnnotationHarnessTurn:           "user-turn",
			labels.AnnotationHarnessCorrelation:    "user-corr",
		},
	}}
	if got := harnessRuntimeSessionID(task, "session-a"); got == "user-runtime" {
		t.Fatal("harnessRuntimeSessionID trusted user annotation before controller ownership")
	}
	if got := harnessTurnID(task); got == "user-turn" {
		t.Fatal("harnessTurnID trusted user annotation before controller ownership")
	}
	if got := harnessCorrelationID(task); got == "user-corr" {
		t.Fatal("harnessCorrelationID trusted user annotation before controller ownership")
	}
}

func TestHarnessTurnIDIncludesTaskUID(t *testing.T) {
	left := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "same-name", UID: types.UID("uid-a")}}
	right := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "same-name", UID: types.UID("uid-b")}}
	if harnessTurnID(left) == harnessTurnID(right) {
		t.Fatalf("turn IDs collide for distinct task UIDs: %q", harnessTurnID(left))
	}
}

func TestHarnessTaskHasControllerStartedRequiresControllerStatusCondition(t *testing.T) {
	startedAt := time.Now().UTC().Format(time.RFC3339Nano)
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
		labels.AnnotationHarnessRuntimeSession: "runtime-a",
		labels.AnnotationHarnessTurn:           "turn-a",
		labels.AnnotationHarnessCorrelation:    "corr-a",
		labels.AnnotationHarnessTurnStartedAt:  startedAt,
	}}}
	if harnessTaskHasControllerStarted(task) {
		t.Fatal("harnessTaskHasControllerStarted trusted annotations without controller status condition")
	}
	task.Status.Conditions = []metav1.Condition{{Type: ConditionTypeJobCreated, Status: metav1.ConditionTrue, Reason: "HarnessProviderStarting"}}
	if !harnessTaskHasControllerStarted(task) {
		t.Fatal("harnessTaskHasControllerStarted rejected controller-owned harness status")
	}
}

func harnessStartedPendingStatus(startedAt time.Time) corev1alpha1.TaskStatus {
	start := metav1.NewTime(startedAt)
	return corev1alpha1.TaskStatus{
		Phase:     corev1alpha1.TaskPhasePending,
		Attempts:  1,
		StartTime: &start,
		Conditions: []metav1.Condition{{
			Type:   ConditionTypeJobCreated,
			Status: metav1.ConditionTrue,
			Reason: "HarnessProviderStarted",
		}},
	}
}

func hasHarnessProviderExecutionEventType(listed []store.ExecutionEvent, typ string) bool {
	for _, event := range listed {
		if event.Type == typ {
			return true
		}
	}
	return false
}
