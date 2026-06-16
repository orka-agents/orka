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
	task.Spec.Env = []corev1.EnvVar{
		{Name: workerenv.AgentReadOnly, Value: "false"},
		{Name: workerenv.ResultStdout, Value: "false"},
		{Name: workerenv.AllowBash, Value: scheduledRunLabelValue},
		{Name: workerenv.AllowedTools, Value: "Bash,Write"},
	}
	agent.Spec.Runtime.Type = corev1alpha1.AgentRuntimeClaude
	agent.Spec.SecretRef = &corev1.LocalObjectReference{Name: "agent-runtime-secret"}
	task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{Workspace: &corev1alpha1.WorkspaceConfig{
		GitRepo:      "https://github.com/sozercan/orka",
		GitSecretRef: &corev1.LocalObjectReference{Name: "git-credentials"},
	}}
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
	gitSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "git-credentials", Namespace: task.Namespace},
		Data:       map[string][]byte{"token": []byte("git-token")},
	}
	r := newUnitReconciler(newTestScheme(), task, agent, agentSecret, taskSecret, gitSecret)
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
	if env[workerenv.AllowBash] != "" || env[workerenv.AllowedTools] == "Bash,Write" {
		t.Fatalf("read-only task env should not override runtime permissions: %#v", env)
	}
	if env[workerenv.AgentReadOnly] != scheduledRunLabelValue || env[workerenv.ResultStdout] != scheduledRunLabelValue {
		t.Fatalf("read-only control env not forced: %#v", env)
	}
	if env[workerenv.GitToken] != "" || env[workerenv.GitHubToken] != "" {
		t.Fatalf("workspace git credentials should not be sent to read-only harness turns")
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
		harnessWrapperRuntimeAnnotation: string(harnessWrapperRuntimeSessionID(task, string(agent.Spec.Runtime.Type))),
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

func TestHarnessWrapperTurnRequestCarriesWorkspaceGitSecretEnv(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{Workspace: &corev1alpha1.WorkspaceConfig{
		GitRepo:      "https://github.com/sozercan/orka",
		GitSecretRef: &corev1.LocalObjectReference{Name: "git-credentials"},
	}}
	gitSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "git-credentials", Namespace: task.Namespace},
		Data: map[string][]byte{
			"token":    []byte("git-token-value"),
			"username": []byte("git-user"),
		},
	}
	r := newUnitReconciler(newTestScheme(), task, agent, gitSecret)
	request, err := r.harnessWrapperStartTurnRequest(context.Background(), task, agent, time.Now(), 1)
	if err != nil {
		t.Fatalf("harnessWrapperStartTurnRequest: %v", err)
	}
	env := map[string]string{}
	for _, item := range request.Input.Env {
		env[item.Name] = item.Value
	}
	if env[workerenv.GitToken] != "git-token-value" || env[workerenv.GitHubToken] != "git-token-value" {
		t.Fatalf("git token env missing or wrong: %#v", env)
	}
	if env[workerenv.GitUsername] != "git-user" {
		t.Fatalf("git username env = %q, want git-user", env[workerenv.GitUsername])
	}
	if env[workerenv.GitAskpass] == "" {
		t.Fatalf("%s missing from harness turn env", workerenv.GitAskpass)
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

func TestHarnessWrapperStreamMissingTurnErrorClassification(t *testing.T) {
	for _, message := range []string{"stream_frames failed (404): turn not found", "stream_frames failed (410): gone"} {
		if !harnessWrapperStreamErrorIsMissingTurn(fmt.Errorf("%s", message)) {
			t.Fatalf("%q should be classified as missing turn", message)
		}
	}
	if harnessWrapperStreamErrorIsMissingTurn(fmt.Errorf("stream_frames failed (401): unauthorized")) {
		t.Fatal("unauthorized stream error should not be classified as missing turn")
	}
}

func TestHarnessWrapperRuntimeSessionIDUsesUIDWithoutExplicitSession(t *testing.T) {
	task, _ := harnessWrapperTaskAndAgent()
	got := string(harnessWrapperRuntimeSessionID(task, string(corev1alpha1.AgentRuntimeClaude)))
	if !strings.Contains(got, string(task.UID)) {
		t.Fatalf("runtime session id = %q, want task UID", got)
	}
	task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "shared-session"}
	got = string(harnessWrapperRuntimeSessionID(task, string(corev1alpha1.AgentRuntimeClaude)))
	if strings.Contains(got, string(task.UID)) || !strings.Contains(got, "shared-session") {
		t.Fatalf("runtime session id = %q, want explicit shared session without UID", got)
	}
}

func TestCancelHarnessWrapperPlannedMissingTurnIsIgnored(t *testing.T) {
	cfg := cliwrapper.DefaultConfig()
	cfg.AllowUnauthenticated = true
	server, err := cliwrapper.NewServer(cfg, &cliwrapper.FakeAdapter{Behavior: cliwrapper.FakeBehaviorSuccess})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.Handler())
	defer srv.Close()
	t.Setenv(harnessWrapperEndpointEnv, srv.URL)
	task, agent := harnessWrapperTaskAndAgent()
	request, err := (&TaskReconciler{}).harnessWrapperStartTurnRequest(context.Background(), task, agent, time.Now(), 1)
	if err != nil {
		t.Fatal(err)
	}
	task.Annotations = map[string]string{
		harnessWrapperTurnIDAnnotation:  string(request.TurnID),
		harnessWrapperRuntimeAnnotation: string(request.RuntimeSessionID),
		harnessWrapperCorrelationIDAnno: request.CorrelationID,
		harnessWrapperStartedAnno:       "false",
	}
	r := newUnitReconciler(newTestScheme(), task, agent)
	if err := r.cancelHarnessWrapperTurn(context.Background(), task, "test"); err != nil {
		t.Fatalf("cancelHarnessWrapperTurn() error = %v, want nil for missing planned turn", err)
	}
}

func TestHarnessWrapperTurnAnnotationsMustMatchCurrentAttempt(t *testing.T) {
	task, _ := harnessWrapperTaskAndAgent()
	task.Status.Attempts = 2
	task.Annotations = map[string]string{
		harnessWrapperTurnIDAnnotation:  string(harnessWrapperTurnID(task, 2)),
		harnessWrapperRuntimeAnnotation: "attacker-controlled",
		harnessWrapperCorrelationIDAnno: string(task.UID),
	}
	if !harnessWrapperTurnAnnotationsMatchTaskAttempt(task, 2) {
		t.Fatal("expected current task/attempt turn annotations to match")
	}
	task.Annotations[harnessWrapperTurnIDAnnotation] = string(harnessWrapperTurnID(task, 1))
	if harnessWrapperTurnAnnotationsMatchTaskAttempt(task, 2) {
		t.Fatal("expected stale/copied turn id to be rejected")
	}
}

func TestCancelHarnessWrapperStartedMissingTurnIsIgnored(t *testing.T) {
	cfg := cliwrapper.DefaultConfig()
	cfg.AllowUnauthenticated = true
	server, err := cliwrapper.NewServer(cfg, &cliwrapper.FakeAdapter{Behavior: cliwrapper.FakeBehaviorSuccess})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.Handler())
	defer srv.Close()
	t.Setenv(harnessWrapperEndpointEnv, srv.URL)
	task, agent := harnessWrapperTaskAndAgent()
	task.Status.Attempts = 1
	request, err := (&TaskReconciler{}).harnessWrapperStartTurnRequest(context.Background(), task, agent, time.Now(), 1)
	if err != nil {
		t.Fatal(err)
	}
	task.Annotations = map[string]string{
		harnessWrapperTurnIDAnnotation:  string(request.TurnID),
		harnessWrapperRuntimeAnnotation: string(request.RuntimeSessionID),
		harnessWrapperCorrelationIDAnno: request.CorrelationID,
		harnessWrapperStartedAnno:       scheduledRunLabelValue,
	}
	r := newUnitReconciler(newTestScheme(), task, agent)
	if err := r.cancelHarnessWrapperTurn(context.Background(), task, "test"); err != nil {
		t.Fatalf("cancelHarnessWrapperTurn() error = %v, want nil for missing started turn", err)
	}
}
