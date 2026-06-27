package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/harness/harnesstest"
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
	secret := attachHarnessWrapperRuntimeSecret(task, agent)
	r := newUnitReconciler(newTestScheme(), task, agent, secret)
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
	t.Setenv(harnessWrapperAuthValueEnv, "x")
	cfg := cliwrapper.DefaultConfig()
	cfg.AuthValue = "x"
	server, err := cliwrapper.NewServer(cfg, &cliwrapper.FakeAdapter{Behavior: cliwrapper.FakeBehaviorSuccess, RuntimeName: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.Handler())
	defer srv.Close()
	t.Setenv(harnessWrapperEndpointEnv, srv.URL)

	task, agent := harnessWrapperTaskAndAgent()
	secret := attachHarnessWrapperRuntimeSecret(task, agent)
	r := newUnitReconciler(newTestScheme(), task, agent, secret)
	updated := runHarnessWrapperTaskToCompletion(t, r, task)
	if updated.Status.Phase != corev1alpha1.TaskPhaseSucceeded {
		t.Fatalf("phase = %s, want Succeeded", updated.Status.Phase)
	}
}

func TestHarnessWrapperTaskRunsAgainstRuntimeRefAgentRuntime(t *testing.T) {
	server := harnesstest.NewFakeHarnessServer(harnesstest.FakeHarnessConfig{RuntimeName: "fibey-agentkit"})
	defer server.Close()

	task, agent := harnessWrapperTaskAndAgent()
	agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "fibey-agentkit"}}
	runtime, token := harnessWrapperReadyAgentRuntime(task.Namespace, server.URL())
	r := newUnitReconciler(newTestScheme(), task, agent, runtime, token)

	updated := runHarnessWrapperTaskToCompletion(t, r, task)
	if updated.Status.Phase != corev1alpha1.TaskPhaseSucceeded {
		t.Fatalf("phase = %s, want Succeeded (message=%s)", updated.Status.Phase, updated.Status.Message)
	}
	if got := updated.Annotations[harnessWrapperRuntimeRefAnno]; got != "fibey-agentkit" {
		t.Fatalf("runtimeRef annotation = %q, want fibey-agentkit", got)
	}
	if got := updated.Annotations[harnessWrapperContractAnno]; got != "orka.harness.v1" {
		t.Fatalf("contract annotation = %q, want orka.harness.v1", got)
	}
}

func TestHarnessWrapperRuntimeRefUsesObservedRuntimeName(t *testing.T) {
	server := harnesstest.NewFakeHarnessServer(harnesstest.FakeHarnessConfig{RuntimeName: "agentkit-fibey-runtime"})
	defer server.Close()

	task, agent := harnessWrapperTaskAndAgent()
	agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "fibey-agentkit"}}
	runtime, token := harnessWrapperReadyAgentRuntime(task.Namespace, server.URL())
	runtime.Status.ObservedCapabilities.RuntimeName = "agentkit-fibey-runtime"
	r := newUnitReconciler(newTestScheme(), task, agent, runtime, token)

	updated := runHarnessWrapperTaskToCompletion(t, r, task)
	if updated.Status.Phase != corev1alpha1.TaskPhaseSucceeded {
		t.Fatalf("phase = %s, want Succeeded (message=%s)", updated.Status.Phase, updated.Status.Message)
	}
	if updated.Status.HarnessRuntime == nil || updated.Status.HarnessRuntime.RuntimeName != "agentkit-fibey-runtime" {
		t.Fatalf("HarnessRuntime = %#v, want observed runtime name", updated.Status.HarnessRuntime)
	}
}

func TestHarnessWrapperRuntimeRefFreezesEndpointForRunningTurn(t *testing.T) {
	server := harnesstest.NewFakeHarnessServer(harnesstest.FakeHarnessConfig{RuntimeName: "fibey-agentkit"})
	defer server.Close()

	task, agent := harnessWrapperTaskAndAgent()
	agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "fibey-agentkit"}}
	runtime, token := harnessWrapperReadyAgentRuntime(task.Namespace, server.URL())
	r := newUnitReconciler(newTestScheme(), task, agent, runtime, token)
	if _, err := r.handlePending(context.Background(), task); err != nil {
		t.Fatalf("handlePending: %v", err)
	}

	var running corev1alpha1.Task
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &running); err != nil {
		t.Fatalf("get running task: %v", err)
	}
	var changed corev1alpha1.AgentRuntime
	if err := r.Get(context.Background(), types.NamespacedName{Name: "fibey-agentkit", Namespace: task.Namespace}, &changed); err != nil {
		t.Fatalf("get runtime: %v", err)
	}
	changed.Spec.Deployment.Endpoint = "http://127.0.0.1:1"
	changed.Generation = 2
	changed.Status.Ready = false
	changed.Status.ObservedGeneration = 1
	if err := r.Update(context.Background(), &changed); err != nil {
		t.Fatalf("update runtime spec: %v", err)
	}
	if err := r.Status().Update(context.Background(), &changed); err != nil {
		t.Fatalf("update runtime status: %v", err)
	}

	if _, err := r.handleRunning(context.Background(), &running); err != nil {
		t.Fatalf("handleRunning: %v", err)
	}
	var updated corev1alpha1.Task
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &updated); err != nil {
		t.Fatalf("get completed task: %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhaseSucceeded {
		t.Fatalf("phase = %s, want Succeeded using frozen endpoint (message=%s)", updated.Status.Phase, updated.Status.Message)
	}
}

func TestHarnessWrapperRuntimeRefNotReadyWaitsBeforeStartTurn(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "should not be called", http.StatusInternalServerError)
	}))
	defer server.Close()

	task, agent := harnessWrapperTaskAndAgent()
	agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "fibey-agentkit"}}
	runtime, token := harnessWrapperReadyAgentRuntime(task.Namespace, server.URL)
	runtime.Status.Ready = false
	runtime.Status.Message = "probe failed"
	r := newUnitReconciler(newTestScheme(), task, agent, runtime, token)

	result, err := r.handlePending(context.Background(), task)
	if err != nil {
		t.Fatalf("handlePending: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("RequeueAfter = %s, want dependency wait", result.RequeueAfter)
	}
	var updated corev1alpha1.Task
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &updated); err != nil {
		t.Fatalf("get task: %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhasePending {
		t.Fatalf("phase = %s, want Pending", updated.Status.Phase)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("server requests = %d, want 0 before StartTurn", got)
	}
}

func TestHarnessWrapperRuntimeRefStaleGenerationWaitsBeforeStartTurn(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "fibey-agentkit"}}
	runtime, token := harnessWrapperReadyAgentRuntime(task.Namespace, "http://127.0.0.1:1")
	runtime.Generation = 2
	runtime.Status.ObservedGeneration = 1
	r := newUnitReconciler(newTestScheme(), task, agent, runtime, token)

	result, err := r.handlePending(context.Background(), task)
	if err != nil {
		t.Fatalf("handlePending: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("RequeueAfter = %s, want dependency wait", result.RequeueAfter)
	}
	var updated corev1alpha1.Task
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &updated); err != nil {
		t.Fatalf("get task: %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhasePending {
		t.Fatalf("phase = %s, want Pending", updated.Status.Phase)
	}
}

func TestHarnessWrapperBuiltInRuntimeIgnoresRuntimeRefAnnotation(t *testing.T) {
	t.Setenv(harnessWrapperEndpointEnv, "http://wrapper.example.invalid")
	task, agent := harnessWrapperTaskAndAgent()
	task.Annotations = map[string]string{harnessWrapperRuntimeRefAnno: "fibey-agentkit"}
	runtime, token := harnessWrapperReadyAgentRuntime(task.Namespace, "http://custom.example.invalid")
	r := newUnitReconciler(newTestScheme(), task, agent, runtime, token)

	target, err := r.resolveHarnessRuntimeTarget(context.Background(), task, agent)
	if err != nil {
		t.Fatalf("resolveHarnessRuntimeTarget: %v", err)
	}
	if target.RuntimeRefName != "" {
		t.Fatalf("RuntimeRefName = %q, want built-in wrapper target", target.RuntimeRefName)
	}
	if target.Endpoint != "http://wrapper.example.invalid" {
		t.Fatalf("Endpoint = %q, want shared wrapper endpoint", target.Endpoint)
	}
	if target.RuntimeName != string(corev1alpha1.AgentRuntimeCodex) {
		t.Fatalf("RuntimeName = %q, want codex", target.RuntimeName)
	}
}

func TestHarnessWrapperRuntimeRefMissingAgentRuntimeFailsClearly(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "missing-runtime"}}
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
	if !strings.Contains(updated.Status.Message, `AgentRuntime "missing-runtime" not found`) {
		t.Fatalf("message = %q, want missing AgentRuntime context", updated.Status.Message)
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
	secret := attachHarnessWrapperRuntimeSecret(task, agent)
	r := newUnitReconciler(newTestScheme(), task, agent, secret)
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
	secret := attachHarnessWrapperRuntimeSecret(task, agent)
	r := newUnitReconciler(newTestScheme(), task, agent, secret)
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

func attachHarnessWrapperRuntimeSecret(task *corev1alpha1.Task, agent *corev1alpha1.Agent) *corev1.Secret {
	agent.Spec.SecretRef = &corev1.LocalObjectReference{Name: "harness-runtime-secret"}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "harness-runtime-secret", Namespace: task.Namespace},
		Data: map[string][]byte{
			workerenv.OpenAIAPIKey: []byte("test-runtime-key"),
		},
	}
}

func harnessWrapperReadyAgentRuntime(namespace, endpoint string) (*corev1alpha1.AgentRuntime, *corev1.Secret) {
	const name = "fibey-agentkit"
	runtime := &corev1alpha1.AgentRuntime{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Generation: 1},
		Spec: corev1alpha1.AgentRuntimeRegistrySpec{
			ContractVersion: corev1alpha1.AgentRuntimeContractHarnessV1,
			Deployment: corev1alpha1.AgentRuntimeDeploymentSpec{
				Mode:     corev1alpha1.AgentRuntimeDeploymentModeExternalEndpoint,
				Endpoint: endpoint,
			},
			ClientAuth: corev1alpha1.AgentRuntimeClientAuth{BearerAuthRef: corev1alpha1.AgentRuntimeBearerAuthReference{
				Name: name + "-token",
				Key:  "token",
			}},
		},
		Status: corev1alpha1.AgentRuntimeStatus{
			Ready:              true,
			ObservedGeneration: 1,
			ObservedCapabilities: &corev1alpha1.AgentRuntimeObservedCapabilities{
				ProtocolVersion:         "orka.harness.v1",
				RuntimeName:             name,
				ToolExecutionModes:      []corev1alpha1.AgentRuntimeToolExecutionMode{corev1alpha1.AgentRuntimeToolExecutionModeObserved},
				SupportsCancel:          true,
				MaxConcurrentTurns:      1,
				SupportsRuntimeSessions: true,
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-token", Namespace: namespace, Labels: map[string]string{agentRuntimeAuthUseLabel: scheduledRunLabelValue, agentRuntimeAuthRefNameLabel: name}},
		Data:       map[string][]byte{"token": []byte("x")},
	}
	return runtime, secret
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

func TestPlannedHarnessWrapperStartTurnRequestRebuildsFullInput(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	task.Spec.Env = []corev1.EnvVar{{Name: workerenv.CodexCLIPath, Value: "/bin/codex-test"}}
	agent.Spec.SecretRef = &corev1.LocalObjectReference{Name: "agent-runtime-secret"}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-runtime-secret", Namespace: task.Namespace},
		Data: map[string][]byte{
			workerenv.OpenAIAPIKey: []byte("runtime-openai-key"),
		},
	}
	r := newUnitReconciler(newTestScheme(), task, agent, secret)
	planned, err := r.harnessWrapperStartTurnRequest(context.Background(), task, agent, time.Now(), 1)
	if err != nil {
		t.Fatalf("harnessWrapperStartTurnRequest: %v", err)
	}
	task.Annotations = map[string]string{
		harnessWrapperTurnIDAnnotation:  string(planned.TurnID),
		harnessWrapperRuntimeAnnotation: string(planned.RuntimeSessionID),
		harnessWrapperCorrelationIDAnno: planned.CorrelationID,
		harnessWrapperMetadataAnno:      `{"runtime":"codex","wrapper":"cli"}`,
		harnessWrapperLastFrameSeqAnno:  "0",
		harnessWrapperStartedAnno:       scheduledRunLabelValue,
	}

	replayed, err := r.plannedHarnessWrapperStartTurnRequest(context.Background(), task, agent, time.Now())
	if err != nil {
		t.Fatalf("plannedHarnessWrapperStartTurnRequest: %v", err)
	}
	if replayed.TurnID != planned.TurnID || replayed.RuntimeSessionID != planned.RuntimeSessionID || replayed.CorrelationID != planned.CorrelationID {
		t.Fatalf("replayed identity = (%q,%q,%q), want planned (%q,%q,%q)", replayed.TurnID, replayed.RuntimeSessionID, replayed.CorrelationID, planned.TurnID, planned.RuntimeSessionID, planned.CorrelationID)
	}
	env := map[string]string{}
	for _, item := range replayed.Input.Env {
		env[item.Name] = item.Value
	}
	if env[workerenv.CodexCLIPath] != "/bin/codex-test" {
		t.Fatalf("%s = %q, want task env", workerenv.CodexCLIPath, env[workerenv.CodexCLIPath])
	}
	if env[workerenv.OpenAIAPIKey] != "runtime-openai-key" {
		t.Fatalf("%s = %q, want runtime secret env", workerenv.OpenAIAPIKey, env[workerenv.OpenAIAPIKey])
	}
}

func TestHarnessWrapperTurnRequestUsesTaskNamespaceForCrossNamespaceAgentSecret(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	agent.Namespace = "shared-agents"
	agent.Spec.SecretRef = &corev1.LocalObjectReference{Name: "runtime-secret"}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "runtime-secret", Namespace: task.Namespace},
		Data:       map[string][]byte{workerenv.OpenAIAPIKey: []byte("task-local-key")},
	}
	r := newUnitReconciler(newTestScheme(), task, agent, secret)
	request, err := r.harnessWrapperStartTurnRequest(context.Background(), task, agent, time.Now(), 1)
	if err != nil {
		t.Fatalf("harnessWrapperStartTurnRequest: %v", err)
	}
	env := map[string]string{}
	for _, item := range request.Input.Env {
		env[item.Name] = item.Value
	}
	if env[workerenv.OpenAIAPIKey] != "task-local-key" {
		t.Fatalf("%s = %q, want task-local secret", workerenv.OpenAIAPIKey, env[workerenv.OpenAIAPIKey])
	}
}

func TestHarnessWrapperTurnRequestAllowsRuntimeSecretProviderBaseURL(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	agent.Spec.SecretRef = &corev1.LocalObjectReference{Name: "runtime-secret"}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "runtime-secret", Namespace: task.Namespace},
		Data: map[string][]byte{
			workerenv.OpenAIAPIKey:  []byte("runtime-key"),
			workerenv.OpenAIBaseURL: []byte("https://proxy.example.invalid/v1"),
		},
	}
	r := newUnitReconciler(newTestScheme(), task, agent, secret)
	request, err := r.harnessWrapperStartTurnRequest(context.Background(), task, agent, time.Now(), 1)
	if err != nil {
		t.Fatalf("harnessWrapperStartTurnRequest: %v", err)
	}
	env := map[string]string{}
	for _, item := range request.Input.Env {
		env[item.Name] = item.Value
	}
	if env[workerenv.OpenAIBaseURL] != "https://proxy.example.invalid/v1" {
		t.Fatalf("%s = %q, want proxy base URL", workerenv.OpenAIBaseURL, env[workerenv.OpenAIBaseURL])
	}
}

func TestHarnessWrapperSecretEnvSkipsFileStyleKeys(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	agent.Spec.SecretRef = &corev1.LocalObjectReference{Name: "runtime-secret"}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "runtime-secret", Namespace: task.Namespace},
		Data: map[string][]byte{
			workerenv.OpenAIAPIKey: []byte("runtime-key"),
			".npmrc":               []byte("registry=https://example.invalid"),
			"config.json":          []byte("{}"),
		},
	}
	r := newUnitReconciler(newTestScheme(), task, agent, secret)
	request, err := r.harnessWrapperStartTurnRequest(context.Background(), task, agent, time.Now(), 1)
	if err != nil {
		t.Fatalf("harnessWrapperStartTurnRequest: %v", err)
	}
	env := map[string]string{}
	for _, item := range request.Input.Env {
		env[item.Name] = item.Value
	}
	if env[workerenv.OpenAIAPIKey] != "runtime-key" {
		t.Fatalf("%s = %q, want runtime key", workerenv.OpenAIAPIKey, env[workerenv.OpenAIAPIKey])
	}
	if _, ok := env[".npmrc"]; ok {
		t.Fatal("file-style key .npmrc was projected as env")
	}
	if _, ok := env["config.json"]; ok {
		t.Fatal("file-style key config.json was projected as env")
	}
}

func TestHarnessWrapperTurnRequestRejectsWrapperPrivateSecretEnv(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	task.Spec.SecretRef = &corev1alpha1.SecretReference{Name: "task-runtime-secret"}
	taskSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "task-runtime-secret", Namespace: task.Namespace},
		Data: map[string][]byte{
			"ORKA_HARNESS_WRAPPER_CHILD_UID": []byte("0"),
		},
	}
	r := newUnitReconciler(newTestScheme(), task, agent, taskSecret)
	_, err := r.harnessWrapperStartTurnRequest(context.Background(), task, agent, time.Now(), 1)
	if err == nil || !strings.Contains(err.Error(), "reserved for wrapper configuration") {
		t.Fatalf("harnessWrapperStartTurnRequest() error = %v, want wrapper-private Secret env rejection", err)
	}
}

func TestHarnessWrapperTurnRequestRejectsControllerUploadSecretEnv(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	task.Spec.SecretRef = &corev1alpha1.SecretReference{Name: "task-runtime-secret"}
	taskSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "task-runtime-secret", Namespace: task.Namespace},
		Data: map[string][]byte{
			workerenv.ControllerURL: []byte("https://attacker.example.invalid"),
			"HTTPS_PROXY":           []byte("https://proxy.example.invalid"),
			"ORKA_ARTIFACTS_DIR":    []byte("/tmp/evil-artifacts"),
		},
	}
	r := newUnitReconciler(newTestScheme(), task, agent, taskSecret)
	_, err := r.harnessWrapperStartTurnRequest(context.Background(), task, agent, time.Now(), 1)
	if err == nil || !strings.Contains(err.Error(), "reserved for controller-managed runtime configuration") {
		t.Fatalf("harnessWrapperStartTurnRequest() error = %v, want controller upload env rejection", err)
	}
}

func TestHarnessWrapperTurnRequestPrependsSkillsToSystemPrompt(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	agent.Spec.SystemPrompt = &corev1alpha1.PromptSource{Inline: "Base instructions"}
	agent.Spec.Skills = []corev1alpha1.SkillReference{{Name: "agent-skill"}}
	task.Spec.AI = &corev1alpha1.AISpec{Skills: []corev1alpha1.SkillReference{{
		ConfigMapRef: &corev1alpha1.ConfigMapKeySelector{Name: "task-skills", Key: "review"},
	}}}
	skill := &corev1alpha1.Skill{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-skill", Namespace: task.Namespace},
		Spec: corev1alpha1.SkillSpec{Content: corev1alpha1.SkillContent{
			Inline: "Use the agent skill.",
		}},
	}
	skillCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "task-skills", Namespace: task.Namespace},
		Data:       map[string]string{"review": "Use the task skill."},
	}
	r := newUnitReconciler(newTestScheme(), task, agent, skill, skillCM)
	request, err := r.harnessWrapperStartTurnRequest(context.Background(), task, agent, time.Now(), 1)
	if err != nil {
		t.Fatalf("harnessWrapperStartTurnRequest: %v", err)
	}
	want := "Use the agent skill.\n\nUse the task skill.\n\nBase instructions"
	if request.Metadata["systemPrompt"] != want {
		t.Fatalf("systemPrompt = %q, want %q", request.Metadata["systemPrompt"], want)
	}
}

func TestHarnessWrapperTurnRequestFiltersReadOnlyRuntimeSecretEnv(t *testing.T) {
	const readOnlyWorkspaceGitCredential = "test"
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
		Data:       map[string][]byte{"token": []byte(readOnlyWorkspaceGitCredential)},
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
	if env[workerenv.GitHubToken] == "runtime-github-token" {
		t.Fatalf("read-only Claude runtime should not receive unrelated %s", workerenv.GitHubToken)
	}
	if env[workerenv.GitToken] != readOnlyWorkspaceGitCredential {
		t.Fatalf("%s = %q, want workspace git credential for read-only prep", workerenv.GitToken, env[workerenv.GitToken])
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
	if env[workerenv.GitToken] != readOnlyWorkspaceGitCredential {
		t.Fatalf("workspace git credentials not preserved for read-only prep: %#v", env)
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

func TestExistingHarnessFrameKeysIndexesStoredHarnessIdentity(t *testing.T) {
	task, _ := harnessWrapperTaskAndAgent()
	eventStore := store.NewFakeExecutionEventStore()
	_, err := eventStore.AppendExecutionEvent(context.Background(), &store.ExecutionEvent{
		Namespace:  task.Namespace,
		StreamType: store.ExecutionEventStreamTypeTask,
		StreamID:   task.Name,
		Type:       events.ExecutionEventTypeAgentRuntimeStarted,
		Content:    []byte(`{"harness":{"runtimeSessionID":"runtime-1","turnID":"turn-1","correlationID":"corr-1","seq":7}}`),
	})
	if err != nil {
		t.Fatalf("AppendExecutionEvent: %v", err)
	}
	r := &TaskReconciler{ExecutionEventStore: eventStore}
	keys, err := r.existingHarnessFrameKeys(context.Background(), task)
	if err != nil {
		t.Fatalf("existingHarnessFrameKeys: %v", err)
	}
	key := strings.Join([]string{"runtime-1", "turn-1", "corr-1", "7"}, "\x00")
	if _, ok := keys[key]; !ok {
		t.Fatalf("existing frame key missing from %#v", keys)
	}
}

// The controller-side cross-restart idempotency backstop: if frames for a turn ID
// are already persisted, the turn already ran and must be recovered rather than
// re-issued (which would duplicate side effects after a wrapper restart).
func TestHarnessWrapperTurnHasPersistedFrames(t *testing.T) {
	task, _ := harnessWrapperTaskAndAgent()
	eventStore := store.NewFakeExecutionEventStore()
	if _, err := eventStore.AppendExecutionEvent(context.Background(), &store.ExecutionEvent{
		Namespace:  task.Namespace,
		StreamType: store.ExecutionEventStreamTypeTask,
		StreamID:   task.Name,
		Type:       events.ExecutionEventTypeAgentRuntimeStarted,
		Content:    []byte(`{"harness":{"runtimeSessionID":"runtime-1","turnID":"turn-abc","correlationID":"corr-1","seq":1}}`),
	}); err != nil {
		t.Fatalf("AppendExecutionEvent: %v", err)
	}
	r := &TaskReconciler{ExecutionEventStore: eventStore}

	has, err := r.harnessWrapperTurnHasPersistedFrames(context.Background(), task, "turn-abc")
	if err != nil {
		t.Fatalf("harnessWrapperTurnHasPersistedFrames: %v", err)
	}
	if !has {
		t.Fatal("expected persisted frames for turn-abc to be detected")
	}

	has, err = r.harnessWrapperTurnHasPersistedFrames(context.Background(), task, "turn-other")
	if err != nil {
		t.Fatalf("harnessWrapperTurnHasPersistedFrames(other): %v", err)
	}
	if has {
		t.Fatal("unexpected match for a different turn ID")
	}

	emptyStore := store.NewFakeExecutionEventStore()
	has, err = (&TaskReconciler{ExecutionEventStore: emptyStore}).harnessWrapperTurnHasPersistedFrames(context.Background(), task, "turn-abc")
	if err != nil {
		t.Fatalf("harnessWrapperTurnHasPersistedFrames(empty): %v", err)
	}
	if has {
		t.Fatal("unexpected match against an empty store")
	}
}

// The persisted execution-event SessionName for a harness task must be EMPTY
// when the task has no real SessionRef, so a SessionRef-less task named "foo"
// cannot collide its events into a real Session "foo". The protocol-level
// harnessWrapperSessionName still falls back to the task name (a non-empty
// identifier is required on the wire), but that value must NOT be the one
// persisted as the event session key.
func TestHarnessEventSessionNameEmptyWithoutRealSessionRef(t *testing.T) {
	r := &TaskReconciler{}
	task, _ := harnessWrapperTaskAndAgent()
	task.Spec.SessionRef = nil

	if got := r.executionEventSessionName(context.Background(), task); got != "" {
		t.Fatalf("executionEventSessionName for SessionRef-less task = %q, want empty (no collision into a real Session)", got)
	}
	// The protocol identifier helper still returns a non-empty value (the task name).
	if got := harnessWrapperSessionName(task); got != task.Name {
		t.Fatalf("harnessWrapperSessionName = %q, want task name %q for the protocol request", got, task.Name)
	}
}

func TestExistingHarnessFrameKeysPagesPastNonHarnessEvents(t *testing.T) {
	task, _ := harnessWrapperTaskAndAgent()
	eventStore := store.NewFakeExecutionEventStore()
	for i := range store.MaxExecutionEventLimit {
		if _, err := eventStore.AppendExecutionEvent(context.Background(), &store.ExecutionEvent{
			Namespace:  task.Namespace,
			StreamType: store.ExecutionEventStreamTypeTask,
			StreamID:   task.Name,
			TaskName:   task.Name,
			Type:       events.ExecutionEventTypeModelMessage,
			Summary:    fmt.Sprintf("non-harness event %d", i),
		}); err != nil {
			t.Fatalf("AppendExecutionEvent(non-harness %d): %v", i, err)
		}
	}
	if _, err := eventStore.AppendExecutionEvent(context.Background(), &store.ExecutionEvent{
		Namespace:  task.Namespace,
		StreamType: store.ExecutionEventStreamTypeTask,
		StreamID:   task.Name,
		TaskName:   task.Name,
		Type:       events.ExecutionEventTypeAgentRuntimeCompleted,
		Content:    []byte(`{"harness":{"runtimeSessionID":"runtime-page","turnID":"turn-page","correlationID":"corr-page","seq":1001}}`),
	}); err != nil {
		t.Fatalf("AppendExecutionEvent(harness): %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	keys, err := (&TaskReconciler{ExecutionEventStore: eventStore}).existingHarnessFrameKeys(ctx, task)
	if err != nil {
		t.Fatalf("existingHarnessFrameKeys: %v", err)
	}
	key := strings.Join([]string{"runtime-page", "turn-page", "corr-page", "1001"}, "\x00")
	if _, ok := keys[key]; !ok {
		t.Fatalf("paged harness frame key missing from %#v", keys)
	}
}

func TestHarnessWrapperTurnMetadataCarriesTaskTimeout(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	task.Spec.Timeout = &metav1.Duration{Duration: 45 * time.Minute}
	r := newUnitReconciler(newTestScheme(), task, agent)
	request, err := r.harnessWrapperStartTurnRequest(context.Background(), task, agent, time.Now(), 1)
	if err != nil {
		t.Fatalf("harnessWrapperStartTurnRequest: %v", err)
	}
	if request.Metadata["timeoutSeconds"] != "2700" {
		t.Fatalf("metadata timeoutSeconds = %q, want 2700", request.Metadata["timeoutSeconds"])
	}
}

func TestHarnessWrapperTurnRequestResolvesTaskValueFromEnv(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	task.Spec.Env = []corev1.EnvVar{
		{
			Name: "CONFIG_VALUE",
			ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "task-config"},
				Key:                  "setting",
			}},
		},
		{
			Name:      "TASK_NAME",
			ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}},
		},
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "task-config", Namespace: task.Namespace},
		Data:       map[string]string{"setting": "from-config"},
	}
	r := newUnitReconciler(newTestScheme(), task, agent, cm)
	request, err := r.harnessWrapperStartTurnRequest(context.Background(), task, agent, time.Now(), 1)
	if err != nil {
		t.Fatalf("harnessWrapperStartTurnRequest: %v", err)
	}
	env := map[string]string{}
	for _, item := range request.Input.Env {
		env[item.Name] = item.Value
	}
	if env["CONFIG_VALUE"] != "from-config" {
		t.Fatalf("CONFIG_VALUE = %q, want from-config", env["CONFIG_VALUE"])
	}
	if env["TASK_NAME"] != task.Name {
		t.Fatalf("TASK_NAME = %q, want %q", env["TASK_NAME"], task.Name)
	}
}

func TestHarnessWrapperTurnRequestOmitsOptionalMissingValueFromEnv(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	optional := true
	task.Spec.Env = []corev1.EnvVar{
		{
			Name: "OPTIONAL_CONFIG",
			ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "missing-config"},
				Key:                  "setting",
				Optional:             &optional,
			}},
		},
	}
	r := newUnitReconciler(newTestScheme(), task, agent)
	request, err := r.harnessWrapperStartTurnRequest(context.Background(), task, agent, time.Now(), 1)
	if err != nil {
		t.Fatalf("harnessWrapperStartTurnRequest: %v", err)
	}
	for _, item := range request.Input.Env {
		if item.Name == "OPTIONAL_CONFIG" {
			t.Fatalf("OPTIONAL_CONFIG was emitted with value %q, want omitted", item.Value)
		}
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
	agent.Spec.SecretRef = &corev1.LocalObjectReference{Name: "agent-runtime-secret"}
	agentSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-runtime-secret", Namespace: task.Namespace},
		Data: map[string][]byte{
			workerenv.GitToken: []byte("agent-token-value"),
		},
	}
	r := newUnitReconciler(newTestScheme(), task, agent, gitSecret, agentSecret)
	request, err := r.harnessWrapperStartTurnRequest(context.Background(), task, agent, time.Now(), 1)
	if err != nil {
		t.Fatalf("harnessWrapperStartTurnRequest: %v", err)
	}
	env := map[string]string{}
	for _, item := range request.Input.Env {
		env[item.Name] = item.Value
	}
	if env[workerenv.GitToken] != "git-token-value" {
		t.Fatalf("git token env missing or wrong: %#v", env)
	}
	if env[workerenv.GitHubToken] == "git-token-value" {
		t.Fatalf("workspace git token should not overwrite runtime %s: %#v", workerenv.GitHubToken, env)
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

func TestValidateHarnessWrapperTaskEnvRejectsSecretAndUnsupportedValueFrom(t *testing.T) {
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
	if harnessWrapperCapabilitiesErrorIsRetryable(fmt.Errorf("read harness runtime capabilities: get failed (404): not found")) {
		t.Fatal("expected permanent capabilities 404 to remain terminal")
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

func TestHarnessWrapperStartTurnErrorClassification(t *testing.T) {
	if !harnessWrapperStartTurnErrorIsRetryable(fmt.Errorf("post failed: connection refused")) {
		t.Fatal("expected transport start error to be retryable")
	}
	if harnessWrapperStartTurnErrorIsRetryable(fmt.Errorf("start_turn failed (401): unauthorized")) {
		t.Fatal("expected auth start error to remain terminal")
	}
}

func TestHarnessWrapperTurnMetadataDefaultsMaxTurns(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	r := newUnitReconciler(newTestScheme(), task, agent)
	request, err := r.harnessWrapperStartTurnRequest(context.Background(), task, agent, time.Now(), 1)
	if err != nil {
		t.Fatalf("harnessWrapperStartTurnRequest: %v", err)
	}
	if request.Metadata["maxTurns"] != "50" {
		t.Fatalf("metadata maxTurns = %q, want 50", request.Metadata["maxTurns"])
	}
}
