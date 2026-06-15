package controller

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/labels"
	"github.com/sozercan/orka/internal/store"
	"github.com/sozercan/orka/internal/workerenv"
	"github.com/sozercan/orka/workers/harness/cliwrapper"
)

func TestHarnessWrapperTaskRunsThroughTurnRunner(t *testing.T) {
	cfg := cliwrapper.DefaultConfig()
	cfg.AllowUnauthenticated = true
	server, err := cliwrapper.NewServer(cfg, &cliwrapper.FakeAdapter{Behavior: cliwrapper.FakeBehaviorSuccess, RuntimeName: "codex"})
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
		t.Fatalf("JobName = %q, want no worker job", updated.Status.JobName)
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
	t.Setenv(harnessWrapperAuthValueEnv, "controller-auth-value")
	cfg := cliwrapper.DefaultConfig()
	cfg.AuthValue = "controller-auth-value"
	server, err := cliwrapper.NewServer(cfg, &cliwrapper.FakeAdapter{Behavior: cliwrapper.FakeBehaviorSuccess, RuntimeName: "codex"})
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
	request, err := r.harnessWrapperStartTurnRequest(context.Background(), task, agent, time.Now(), 2)
	if err != nil {
		t.Fatalf("harnessWrapperStartTurnRequest: %v", err)
	}
	if !strings.HasPrefix(string(request.TurnID), "harness-task-") || !strings.HasSuffix(string(request.TurnID), "-2") {
		t.Fatalf("TurnID = %q, want namespaced/UID-scoped attempt 2 turn ID", request.TurnID)
	}
}

func TestHarnessRuntimeRunningTaskFinishesAfterStart(t *testing.T) {
	cfg := cliwrapper.DefaultConfig()
	cfg.AllowUnauthenticated = true
	server, err := cliwrapper.NewServer(cfg, &cliwrapper.FakeAdapter{Behavior: cliwrapper.FakeBehaviorSuccess, RuntimeName: "codex"})
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

func TestHarnessRuntimeMissingEndpointFailsAgentTask(t *testing.T) {
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
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "harness-task", Namespace: "default", UID: types.UID("uid-harness-task")},
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

func TestHarnessWrapperTurnRequestCarriesAgentRuntimeSecretEnv(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	agent.Spec.SecretRef = &corev1.LocalObjectReference{Name: "agent-runtime-secret"}
	task.Spec.SecretRef = &corev1alpha1.SecretReference{Name: "task-runtime-secret"}
	agentSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-runtime-secret", Namespace: agent.Namespace},
		Data: map[string][]byte{
			workerenv.OpenAIAPIKey: []byte("runtime-openai-key"),
			workerenv.GitHubToken:  []byte("runtime-github-token"),
		},
	}
	taskSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "task-runtime-secret", Namespace: task.Namespace},
		Data: map[string][]byte{
			workerenv.AnthropicAPIKey: []byte("runtime-anthropic-key"),
		},
	}
	r := newUnitReconciler(newTestScheme(), task, agent, agentSecret, taskSecret)
	request, err := r.harnessWrapperStartTurnRequest(context.Background(), task, agent, time.Now(), 1)
	if err != nil {
		t.Fatalf("harnessWrapperStartTurnRequest: %v", err)
	}
	env := map[string]string{}
	for _, item := range request.Input.Env {
		env[item.Name] = item.Value
	}
	if env[workerenv.OpenAIAPIKey] != "runtime-openai-key" {
		t.Fatalf("%s = %q, want runtime credential", workerenv.OpenAIAPIKey, env[workerenv.OpenAIAPIKey])
	}
	if env[workerenv.GitHubToken] != "runtime-github-token" {
		t.Fatalf("%s = %q, want runtime credential", workerenv.GitHubToken, env[workerenv.GitHubToken])
	}
	if env[workerenv.AnthropicAPIKey] != "runtime-anthropic-key" {
		t.Fatalf("%s = %q, want task runtime credential", workerenv.AnthropicAPIKey, env[workerenv.AnthropicAPIKey])
	}
}

func TestHarnessWrapperTurnRequestFiltersReadOnlyRuntimeSecretEnv(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	task.Annotations = map[string]string{labels.AnnotationAgentReadOnly: scheduledRunLabelValue}
	agent.Spec.Runtime.Type = corev1alpha1.AgentRuntimeClaude
	agent.Spec.SecretRef = &corev1.LocalObjectReference{Name: "agent-runtime-secret"}
	agentSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-runtime-secret", Namespace: agent.Namespace},
		Data: map[string][]byte{
			workerenv.AnthropicAPIKey: []byte("runtime-anthropic-key"),
			workerenv.GitHubToken:     []byte("runtime-github-token"),
		},
	}
	task.Spec.SecretRef = &corev1alpha1.SecretReference{Name: "task-runtime-secret"}
	taskSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "task-runtime-secret", Namespace: task.Namespace},
		Data:       map[string][]byte{workerenv.OpenAIAPIKey: []byte("task-openai-key")},
	}
	r := newUnitReconciler(newTestScheme(), task, agent, agentSecret, taskSecret)
	request, err := r.harnessWrapperStartTurnRequest(context.Background(), task, agent, time.Now(), 1)
	if err != nil {
		t.Fatalf("harnessWrapperStartTurnRequest: %v", err)
	}
	env := map[string]string{}
	for _, item := range request.Input.Env {
		env[item.Name] = item.Value
	}
	if env[workerenv.AnthropicAPIKey] != "runtime-anthropic-key" {
		t.Fatalf("%s = %q, want runtime credential", workerenv.AnthropicAPIKey, env[workerenv.AnthropicAPIKey])
	}
	if _, ok := env[workerenv.GitHubToken]; ok {
		t.Fatalf("%s should be filtered for read-only harness turns", workerenv.GitHubToken)
	}
	if env[workerenv.OpenAIAPIKey] == "task-openai-key" {
		t.Fatalf("task secret credentials should not be sent to read-only harness turns")
	}
}

func TestHarnessWrapperTurnRequestRejectsCrossNamespaceTaskSecret(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	task.Spec.SecretRef = &corev1alpha1.SecretReference{Name: "task-runtime-secret", Namespace: "other"}
	r := newUnitReconciler(newTestScheme(), task, agent)
	_, err := r.harnessWrapperStartTurnRequest(context.Background(), task, agent, time.Now(), 1)
	if err == nil || !strings.Contains(err.Error(), "does not match task namespace") {
		t.Fatalf("harnessWrapperStartTurnRequest() error = %v, want namespace rejection", err)
	}
}

func TestHarnessWrapperPlannedTurnMustMatchTaskIdentity(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	task.Annotations = map[string]string{
		harnessWrapperTurnIDAnnotation:  string(harnessWrapperTurnID(task, 1)),
		harnessWrapperRuntimeAnnotation: task.Namespace + ":" + harnessWrapperSessionName(task) + ":" + string(agent.Spec.Runtime.Type),
		harnessWrapperCorrelationIDAnno: string(task.UID),
	}
	if !harnessWrapperPlannedTurnMatchesTask(task, agent, 1) {
		t.Fatal("expected planned turn to match task identity")
	}
	task.Annotations[harnessWrapperTurnIDAnnotation] = "other-turn"
	if harnessWrapperPlannedTurnMatchesTask(task, agent, 1) {
		t.Fatal("expected copied turn id to be rejected")
	}
}

func TestHarnessWrapperTurnRequestCarriesSafeEnvAndWorkspaceMetadata(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	task.Spec.Env = []corev1.EnvVar{{Name: workerenv.PRBaseSHA, Value: "base-sha"}, {Name: "ORKA_SECURITY_STAGE", Value: "review"}}
	task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{Workspace: &corev1alpha1.WorkspaceConfig{
		GitRepo:      "https://github.com/sozercan/orka",
		Branch:       "main",
		SubPath:      "docs",
		ForkRepo:     "https://github.com/sozercan/orka-fork",
		PRBaseBranch: "contract",
		PushBranch:   "agent/test-branch",
	}}
	r := newUnitReconciler(newTestScheme(), task, agent)
	r.JobBuilder = &JobBuilder{ControllerURL: "http://orka-api.test:8080"}
	request, err := r.harnessWrapperStartTurnRequest(context.Background(), task, agent, time.Now(), 1)
	if err != nil {
		t.Fatalf("harnessWrapperStartTurnRequest: %v", err)
	}
	for key, want := range map[string]string{
		"gitRepo":          "https://github.com/sozercan/orka",
		"gitBranch":        "main",
		"workspaceSubPath": "docs",
		"forkRepo":         "https://github.com/sozercan/orka-fork",
		"prBaseBranch":     "contract",
		"pushBranch":       "agent/test-branch",
		"prBaseSHA":        "base-sha",
	} {
		if got := request.Metadata[key]; got != want {
			t.Fatalf("metadata[%s] = %q, want %q", key, got, want)
		}
	}
	env := map[string]string{}
	for _, item := range request.Input.Env {
		env[item.Name] = item.Value
	}
	for key, want := range map[string]string{
		workerenv.ControllerURL:  "http://orka-api.test:8080",
		workerenv.ResultEndpoint: "http://orka-api.test:8080/internal/v1/results/default/harness-task",
		workerenv.PRBaseSHA:      "base-sha",
		"ORKA_SECURITY_STAGE":    "review",
	} {
		if got := env[key]; got != want {
			t.Fatalf("env[%s] = %q, want %q", key, got, want)
		}
	}
}

func TestValidateHarnessWrapperTaskEnvRejectsSecretAndValueFrom(t *testing.T) {
	if err := validateHarnessWrapperTaskEnv([]corev1.EnvVar{{Name: "ORKA_SECURITY_STAGE", Value: "review"}}); err != nil {
		t.Fatalf("safe env rejected: %v", err)
	}
	if err := validateHarnessWrapperTaskEnv([]corev1.EnvVar{{Name: "API_TOKEN", Value: "secret"}}); err == nil {
		t.Fatal("expected secret-shaped env name to be rejected")
	}
	if err := validateHarnessWrapperTaskEnv([]corev1.EnvVar{{Name: "SAFE", ValueFrom: &corev1.EnvVarSource{}}}); err == nil {
		t.Fatal("expected valueFrom env to be rejected")
	}
}

func TestHarnessWrapperCapabilitiesReadErrorRetryable(t *testing.T) {
	if !harnessWrapperCapabilitiesErrorIsRetryable(fmt.Errorf("read harness runtime capabilities: boom")) {
		t.Fatal("expected capabilities read error to be retryable")
	}
	if harnessWrapperCapabilitiesErrorIsRetryable(fmt.Errorf("harness runtime \"multi\" does not match task runtime \"codex\"")) {
		t.Fatal("expected runtime mismatch to remain terminal")
	}
}
