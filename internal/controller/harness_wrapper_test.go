package controller

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/labels"
	"github.com/sozercan/orka/internal/store"
	"github.com/sozercan/orka/workers/harness/cliwrapper"
)

func TestHarnessWrapperTaskRunsThroughTurnRunner(t *testing.T) {
	t.Setenv(harnessWrapperFeatureGateEnv, "true")
	cfg := cliwrapper.DefaultConfig()
	cfg.AllowUnauthenticated = true
	server, err := cliwrapper.NewServer(cfg, cliwrapper.NewFakeAdapter(cliwrapper.FakeBehaviorSuccess))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.Handler())
	defer srv.Close()
	t.Setenv(harnessWrapperEndpointEnv, srv.URL)

	task, agent := harnessWrapperTaskAndAgent()
	r := newUnitReconciler(newTestScheme(), task, agent)
	updated := runHarnessWrapperTaskToCompletion(t, r, task)
	if updated.Status.Phase != corev1alpha1.TaskPhaseSucceeded {
		t.Fatalf("phase = %s, want Succeeded (message=%s)", updated.Status.Phase, updated.Status.Message)
	}
	if updated.Status.JobName != "" {
		t.Fatalf("JobName = %q, want no legacy job", updated.Status.JobName)
	}
	if updated.Status.ResultRef == nil || !updated.Status.ResultRef.Available {
		t.Fatalf("ResultRef = %#v, want available result reference", updated.Status.ResultRef)
	}
	result, err := r.ResultStore.GetResult(context.Background(), task.Namespace, task.Name)
	if err != nil {
		t.Fatalf("GetResult: %v", err)
	}
	if string(result) != "ok" {
		t.Fatalf("result = %q, want ok", string(result))
	}
	eventsList, err := r.ExecutionEventStore.ListExecutionEvents(context.Background(), store.ExecutionEventFilter{Namespace: task.Namespace, StreamID: task.Name})
	if err != nil {
		t.Fatalf("ListExecutionEvents: %v", err)
	}
	if !hasExecutionEventType(eventsList, events.ExecutionEventTypeAgentRuntimeCompleted) {
		t.Fatalf("events = %#v, want harness mapped runtime completed", eventsList)
	}
}

func TestHarnessWrapperControllerSendsBearerToken(t *testing.T) {
	t.Setenv(harnessWrapperFeatureGateEnv, "true")
	t.Setenv(harnessWrapperAuthValueEnv, "controller-auth-value")
	cfg := cliwrapper.DefaultConfig()
	cfg.AuthValue = "controller-auth-value"
	server, err := cliwrapper.NewServer(cfg, cliwrapper.NewFakeAdapter(cliwrapper.FakeBehaviorSuccess))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.Handler())
	defer srv.Close()
	t.Setenv(harnessWrapperEndpointEnv, srv.URL)

	task, agent := harnessWrapperTaskAndAgent()
	r := newUnitReconciler(newTestScheme(), task, agent)
	updated := runHarnessWrapperTaskToCompletion(t, r, task)
	if updated.Status.Phase != corev1alpha1.TaskPhaseSucceeded {
		t.Fatalf("phase = %s, want Succeeded", updated.Status.Phase)
	}
}

func TestHarnessWrapperStartTurnUsesComputedAttemptForTurnID(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	task.Status.Attempts = 1
	r := newUnitReconciler(newTestScheme(), task, agent)
	request := r.harnessWrapperStartTurnRequest(task, agent, time.Now(), 2)
	if !strings.HasPrefix(string(request.TurnID), "harness-task-") || !strings.HasSuffix(string(request.TurnID), "-2") {
		t.Fatalf("TurnID = %q, want namespaced/UID-scoped attempt 2 turn ID", request.TurnID)
	}
}

func TestHarnessWrapperRunningTaskFinishesWhenFeatureGateDisabledAfterStart(t *testing.T) {
	t.Setenv(harnessWrapperFeatureGateEnv, "true")
	cfg := cliwrapper.DefaultConfig()
	cfg.AllowUnauthenticated = true
	server, err := cliwrapper.NewServer(cfg, cliwrapper.NewFakeAdapter(cliwrapper.FakeBehaviorSuccess))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.Handler())
	defer srv.Close()
	t.Setenv(harnessWrapperEndpointEnv, srv.URL)

	task, agent := harnessWrapperTaskAndAgent()
	r := newUnitReconciler(newTestScheme(), task, agent)
	if _, err := r.handlePending(context.Background(), task); err != nil {
		t.Fatalf("handlePending: %v", err)
	}
	t.Setenv(harnessWrapperFeatureGateEnv, "false")
	var running corev1alpha1.Task
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &running); err != nil {
		t.Fatalf("get running task: %v", err)
	}
	if _, err := r.handleRunning(context.Background(), &running); err != nil {
		t.Fatalf("handleRunning: %v", err)
	}
	var updated corev1alpha1.Task
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &updated); err != nil {
		t.Fatalf("get completed task: %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhaseSucceeded {
		t.Fatalf("phase = %s, want Succeeded", updated.Status.Phase)
	}
}

func TestHarnessWrapperFeatureGateDisabledUsesLegacyJobPath(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	r := newUnitReconciler(newTestScheme(), task, agent)
	if _, err := r.handlePending(context.Background(), task); err != nil {
		t.Fatalf("handlePending: %v", err)
	}
	var updated corev1alpha1.Task
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &updated); err != nil {
		t.Fatalf("get task: %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhaseRunning {
		t.Fatalf("phase = %s, want Running legacy job", updated.Status.Phase)
	}
	if updated.Status.JobName == "" {
		t.Fatal("JobName empty, want legacy job created when feature gate disabled")
	}
}

func TestHarnessWrapperMissingEndpointFailsWhenEnabled(t *testing.T) {
	t.Setenv(harnessWrapperFeatureGateEnv, "true")
	task, agent := harnessWrapperTaskAndAgent()
	r := newUnitReconciler(newTestScheme(), task, agent)
	if _, err := r.handlePending(context.Background(), task); err != nil {
		t.Fatalf("handlePending: %v", err)
	}
	var updated corev1alpha1.Task
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &updated); err != nil {
		t.Fatalf("get task: %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhaseFailed {
		t.Fatalf("phase = %s, want Failed", updated.Status.Phase)
	}
	if updated.Status.JobName != "" {
		t.Fatalf("JobName = %q, want no job", updated.Status.JobName)
	}
}

func runHarnessWrapperTaskToCompletion(t *testing.T, r *TaskReconciler, task *corev1alpha1.Task) corev1alpha1.Task {
	t.Helper()
	if _, err := r.handlePending(context.Background(), task); err != nil {
		t.Fatalf("handlePending: %v", err)
	}
	var running corev1alpha1.Task
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &running); err != nil {
		t.Fatalf("get running task: %v", err)
	}
	if running.Status.Phase != corev1alpha1.TaskPhaseRunning {
		t.Fatalf("phase after pending = %s, want Running", running.Status.Phase)
	}
	if _, err := r.handleRunning(context.Background(), &running); err != nil {
		t.Fatalf("handleRunning: %v", err)
	}
	var updated corev1alpha1.Task
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &updated); err != nil {
		t.Fatalf("get completed task: %v", err)
	}
	return updated
}

func harnessWrapperTaskAndAgent() (*corev1alpha1.Task, *corev1alpha1.Agent) {
	annotations := map[string]string{
		labels.AnnotationHarnessWrapper:        "true",
		labels.AnnotationHarnessWrapperRuntime: "generic",
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "harness-task", Namespace: "default", UID: types.UID("uid-harness-task"), Annotations: annotations},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAgent,
			AgentRef: &corev1alpha1.AgentReference{Name: "harness-agent"},
			Prompt:   "hello harness",
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
	}
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "harness-agent", Namespace: "default"},
		Spec:       corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeCodex}},
	}
	return task, agent
}

func hasExecutionEventType(eventsList []store.ExecutionEvent, typ string) bool {
	for _, event := range eventsList {
		if event.Type == typ {
			return true
		}
	}
	return false
}
