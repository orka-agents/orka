/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/tracing"
	"github.com/orka-agents/orka/internal/tracing/testutil"
	"github.com/orka-agents/orka/internal/workerenv"
)

func TestCreatePRMonitorTool_Metadata(t *testing.T) {
	tool := &CreatePRMonitorTool{}

	if tool.Name() != createPRMonitorToolName {
		t.Errorf("Name() = %q, want %q", tool.Name(), createPRMonitorToolName)
	}
	if tool.Description() == "" {
		t.Fatal("Description() returned empty string")
	}

	var schema map[string]any
	if err := json.Unmarshal(tool.Parameters(), &schema); err != nil {
		t.Fatalf("failed to unmarshal schema: %v", err)
	}
	if schema[jsonSchemaTypeField] != jsonSchemaTypeObject {
		t.Errorf("schema type = %v, want %q", schema[jsonSchemaTypeField], jsonSchemaTypeObject)
	}

	props, ok := schema[jsonSchemaPropertiesField].(map[string]any)
	if !ok {
		t.Fatalf("schema properties = %T, want map[string]any", schema[jsonSchemaPropertiesField])
	}
	for _, field := range []string{
		nameField,
		namespaceField,
		repoURLField,
		scheduleField,
		agentRefField,
		providerRefField,
		"gitSecretRef",
		perPageField,
		"review_event",
		promptField,
	} {
		if _, ok := props[field]; !ok {
			t.Errorf("schema missing property %q", field)
		}
	}

	required, ok := schema[jsonSchemaRequiredField].([]any)
	if !ok {
		t.Fatalf("schema required = %T, want []any", schema[jsonSchemaRequiredField])
	}
	for _, field := range []string{nameField, repoURLField, scheduleField, agentRefField} {
		if !containsAnyString(required, field) {
			t.Errorf("schema required = %v, want %q", required, field)
		}
	}
}

func TestCreatePRMonitorTool_ExecuteMissingToolContext(t *testing.T) {
	tool := &CreatePRMonitorTool{}

	resultJSON, err := tool.Execute(context.Background(), mustJSON(t, map[string]any{}))
	if err != nil {
		t.Fatalf("Execute() returned error: %v", err)
	}

	var result ChatToolResult
	if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}
	if result.Success {
		t.Fatal("expected failure")
	}
	if result.ErrorType != internalErrorType {
		t.Errorf("ErrorType = %q, want %q", result.ErrorType, internalErrorType)
	}
}

func TestCreatePRMonitorTool_ExecuteCreatesScheduledAITask(t *testing.T) {
	fc := newFakeClient(
		&corev1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "reviewer", Namespace: defaultNamespace},
			Spec: corev1alpha1.AgentSpec{
				Coordination: &corev1alpha1.CoordinationConfig{Enabled: true},
			},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: testGitCredentialsSecret, Namespace: defaultNamespace},
			Data:       map[string][]byte{tokenKey: []byte(testGitHubToken)},
		},
	)
	ctx := newCreatePRMonitorToolContext(fc)
	tool := &CreatePRMonitorTool{}

	resultJSON, err := tool.Execute(ctx, mustJSON(t, map[string]any{
		nameField:        "daily-pr-monitor",
		repoURLField:     "https://github.com/orka-agents/orka",
		scheduleField:    "*/15 * * * *",
		agentRefField:    "reviewer",
		providerRefField: "default-provider",
		"gitSecretRef":   testGitCredentialsSecret,
		perPageField:     100,
		"review_event":   "comment",
		promptField:      "Focus on regressions.",
	}))
	if err != nil {
		t.Fatalf("Execute() returned error: %v", err)
	}

	var result ChatToolResult
	if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success, got error: %s", result.Error)
	}

	var task corev1alpha1.Task
	if err := fc.Get(context.Background(), types.NamespacedName{Name: "pr-monitor-task", Namespace: defaultNamespace}, &task); err != nil {
		t.Fatalf("failed to get created task: %v", err)
	}
	if task.Spec.Type != corev1alpha1.TaskTypeAI {
		t.Errorf("task type = %q, want %q", task.Spec.Type, corev1alpha1.TaskTypeAI)
	}
	if task.Spec.Schedule != "*/15 * * * *" {
		t.Errorf("schedule = %q", task.Spec.Schedule)
	}
	if task.Annotations[labels.AnnotationPRMonitorName] != "daily-pr-monitor" {
		t.Errorf("monitor annotation = %q", task.Annotations[labels.AnnotationPRMonitorName])
	}
	if task.Annotations[labels.AnnotationDisableCoordinationToolInject] != trueStr {
		t.Errorf("disable coordination tool injection annotation = %q", task.Annotations[labels.AnnotationDisableCoordinationToolInject])
	}
	if task.Spec.AgentRef == nil || task.Spec.AgentRef.Name != "reviewer" {
		t.Fatalf("AgentRef = %#v, want reviewer", task.Spec.AgentRef)
	}
	if task.Spec.AI == nil {
		t.Fatal("AI spec is nil")
	}
	if task.Spec.AI.ProviderRef == nil || task.Spec.AI.ProviderRef.Name != "default-provider" {
		t.Fatalf("ProviderRef = %#v, want default-provider", task.Spec.AI.ProviderRef)
	}
	if task.Spec.Workspace == nil {
		t.Fatal("Workspace is nil")
	}
	if task.Spec.Workspace.GitRepo != "https://github.com/orka-agents/orka" {
		t.Errorf("workspace.gitRepo = %q", task.Spec.Workspace.GitRepo)
	}
	if task.Spec.Workspace.GitSecretRef == nil || task.Spec.Workspace.GitSecretRef.Name != testGitCredentialsSecret {
		t.Fatalf("workspace.gitSecretRef = %#v, want %s", task.Spec.Workspace.GitSecretRef, testGitCredentialsSecret)
	}
	if !hasEnvVar(task.Spec.Env, workerenv.GitRepo, "https://github.com/orka-agents/orka") {
		t.Errorf("env missing %s=https://github.com/orka-agents/orka: %#v", workerenv.GitRepo, task.Spec.Env)
	}
	for _, tool := range prMonitorRequiredTools {
		if !containsString(task.Spec.AI.Tools, tool) {
			t.Errorf("AI tools = %v, want %q", task.Spec.AI.Tools, tool)
		}
	}
	if !strings.Contains(task.Spec.Prompt, "list_pull_requests") {
		t.Errorf("prompt missing list_pull_requests: %s", task.Spec.Prompt)
	}
	if !strings.Contains(task.Spec.Prompt, "repo_url \"https://github.com/orka-agents/orka\"") {
		t.Errorf("prompt missing repo URL: %s", task.Spec.Prompt)
	}
	if !strings.Contains(task.Spec.Prompt, "Focus on regressions.") {
		t.Errorf("prompt missing extra instructions: %s", task.Spec.Prompt)
	}
}

func TestCreatePRMonitorTool_ExecuteStampsTraceContext(t *testing.T) {
	if _, err := tracing.Init("test", false); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	_ = testutil.NewSpanHarness(t)
	_, gitSecret := githubRepoTaskWithSecret("https://github.com/orka-agents/orka")
	fc := newFakeClient(
		&corev1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "reviewer", Namespace: defaultNamespace},
			Spec: corev1alpha1.AgentSpec{
				Coordination: &corev1alpha1.CoordinationConfig{Enabled: true},
			},
		},
		gitSecret,
	)
	baseCtx := newCreatePRMonitorToolContext(fc)
	parentCtx, parentSpan := tracing.Tracer("test").Start(context.Background(), "chat-tool")
	defer parentSpan.End()
	ctx := WithToolContext(parentCtx, GetToolContext(baseCtx))

	_, err := (&CreatePRMonitorTool{}).Execute(ctx, mustJSON(t, map[string]any{
		nameField:      "trace-pr-monitor",
		repoURLField:   "https://github.com/orka-agents/orka",
		scheduleField:  "*/15 * * * *",
		agentRefField:  "reviewer",
		"gitSecretRef": gitSecret.Name,
	}))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var task corev1alpha1.Task
	if err := fc.Get(context.Background(), types.NamespacedName{Name: "pr-monitor-task", Namespace: defaultNamespace}, &task); err != nil {
		t.Fatalf("failed to get created task: %v", err)
	}
	if task.Annotations[labels.AnnotationTraceParent] == "" {
		t.Fatalf("missing %s annotation", labels.AnnotationTraceParent)
	}
}

func TestCreatePRMonitorTool_ExecuteAuthorizesTaskCreate(t *testing.T) {
	fc := newFakeClient(
		&corev1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "reviewer", Namespace: defaultNamespace},
			Spec: corev1alpha1.AgentSpec{
				Coordination: &corev1alpha1.CoordinationConfig{Enabled: true},
			},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: testGitCredentialsSecret, Namespace: defaultNamespace},
			Data:       map[string][]byte{tokenKey: []byte(testGitHubToken)},
		},
	)
	authorized := false
	authorizedWithSecret := false
	ctx := newCreatePRMonitorToolContextWithAuthorize(fc, func(_ context.Context, task *corev1alpha1.Task) *ChatToolError {
		authorized = true
		authorizedWithSecret = task.Spec.Workspace != nil &&
			task.Spec.Workspace.GitSecretRef != nil &&
			task.Spec.Workspace.GitSecretRef.Name == testGitCredentialsSecret
		if task.Annotations == nil {
			task.Annotations = map[string]string{}
		}
		task.Annotations["test.authorization"] = "stamped"
		return nil
	})

	resultJSON, err := (&CreatePRMonitorTool{}).Execute(ctx, mustJSON(t, map[string]any{
		nameField:     "daily-pr-monitor",
		repoURLField:  testSozercanAynaRepoURL,
		scheduleField: "*/15 * * * *",
		agentRefField: "reviewer",
	}))
	if err != nil {
		t.Fatalf("Execute() returned error: %v", err)
	}
	var result ChatToolResult
	if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success, got error: %s", result.Error)
	}
	if !authorized {
		t.Fatal("AuthorizeTaskCreate was not called")
	}
	if !authorizedWithSecret {
		t.Fatal("AuthorizeTaskCreate did not see the resolved gitSecretRef")
	}

	var task corev1alpha1.Task
	if err := fc.Get(context.Background(), types.NamespacedName{Name: "pr-monitor-task", Namespace: defaultNamespace}, &task); err != nil {
		t.Fatalf("failed to get created task: %v", err)
	}
	if task.Annotations["test.authorization"] != "stamped" {
		t.Fatalf("authorization stamp = %q, want stamped", task.Annotations["test.authorization"])
	}
}

func TestCreatePRMonitorTool_ExecuteRejectsUnauthorizedTaskCreate(t *testing.T) {
	fc := newFakeClient(
		&corev1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "reviewer", Namespace: defaultNamespace},
			Spec: corev1alpha1.AgentSpec{
				Coordination: &corev1alpha1.CoordinationConfig{Enabled: true},
			},
		},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: testGitCredentialsSecret, Namespace: defaultNamespace}},
	)
	ctx := newCreatePRMonitorToolContextWithAuthorize(fc, func(context.Context, *corev1alpha1.Task) *ChatToolError {
		return &ChatToolError{Type: "authorization_failed", Message: "task blocked by context token"}
	})

	resultJSON, err := (&CreatePRMonitorTool{}).Execute(ctx, mustJSON(t, map[string]any{
		nameField:     "daily-pr-monitor",
		repoURLField:  testSozercanAynaRepoURL,
		scheduleField: "*/15 * * * *",
		agentRefField: "reviewer",
	}))
	if err != nil {
		t.Fatalf("Execute() returned error: %v", err)
	}
	var result ChatToolResult
	if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}
	if result.Success {
		t.Fatalf("expected failure, got success: %s", resultJSON)
	}
	if result.ErrorType != "authorization_failed" || !strings.Contains(result.Error, "blocked") {
		t.Fatalf("result = %#v, want authorization_failed", result)
	}

	var task corev1alpha1.Task
	err = fc.Get(context.Background(), types.NamespacedName{Name: "pr-monitor-task", Namespace: defaultNamespace}, &task)
	if err == nil {
		t.Fatal("task was created despite authorization failure")
	}
}

func TestCreatePRMonitorTool_ExecuteAuthorizesBeforeExplicitGitSecretLookup(t *testing.T) {
	fc := newFakeClient(&corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "reviewer", Namespace: defaultNamespace},
		Spec: corev1alpha1.AgentSpec{
			Coordination: &corev1alpha1.CoordinationConfig{Enabled: true},
		},
	})
	ctx := newCreatePRMonitorToolContextWithAuthorize(fc, func(context.Context, *corev1alpha1.Task) *ChatToolError {
		return &ChatToolError{Type: "authorization_failed", Message: "task blocked by context token"}
	})

	resultJSON, err := (&CreatePRMonitorTool{}).Execute(ctx, mustJSON(t, map[string]any{
		nameField:      "daily-pr-monitor",
		repoURLField:   testSozercanAynaRepoURL,
		scheduleField:  "*/15 * * * *",
		agentRefField:  "reviewer",
		"gitSecretRef": "missing-secret",
	}))
	if err != nil {
		t.Fatalf("Execute() returned error: %v", err)
	}
	var result ChatToolResult
	if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}
	if result.Success {
		t.Fatalf("expected failure, got success: %s", resultJSON)
	}
	if result.ErrorType != "authorization_failed" || strings.Contains(result.Error, "missing-secret") {
		t.Fatalf("result = %#v, want authorization failure without secret existence details", result)
	}

	var task corev1alpha1.Task
	err = fc.Get(context.Background(), types.NamespacedName{Name: "pr-monitor-task", Namespace: defaultNamespace}, &task)
	if err == nil {
		t.Fatal("task was created despite authorization failure")
	}
}

func TestCreatePRMonitorTool_ExecuteRejectsMissingGitCredentials(t *testing.T) {
	fc := newFakeClient(&corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "reviewer", Namespace: defaultNamespace},
		Spec: corev1alpha1.AgentSpec{
			Coordination: &corev1alpha1.CoordinationConfig{Enabled: true},
		},
	})

	result := executeCreatePRMonitorForFailure(t, fc, map[string]any{
		nameField:     "daily-pr-monitor",
		repoURLField:  testSozercanAynaRepoURL,
		scheduleField: "*/15 * * * *",
		agentRefField: "reviewer",
	})
	if result.ErrorType != errTypeInvalidArgs || !strings.Contains(result.Error, "gitSecretRef is required") {
		t.Fatalf("result = %#v, want missing git credentials invalid_arguments", result)
	}

	var task corev1alpha1.Task
	err := fc.Get(context.Background(), types.NamespacedName{Name: "pr-monitor-task", Namespace: defaultNamespace}, &task)
	if err == nil {
		t.Fatal("task was created despite missing git credentials")
	}
}

func TestCreatePRMonitorTool_ExecuteRejectsMissingExplicitGitSecret(t *testing.T) {
	fc := newFakeClient(&corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "reviewer", Namespace: defaultNamespace},
		Spec: corev1alpha1.AgentSpec{
			Coordination: &corev1alpha1.CoordinationConfig{Enabled: true},
		},
	})

	resultJSON, err := (&CreatePRMonitorTool{}).Execute(newCreatePRMonitorToolContext(fc), mustJSON(t, map[string]any{
		nameField:      "daily-pr-monitor",
		repoURLField:   testSozercanAynaRepoURL,
		scheduleField:  "*/15 * * * *",
		agentRefField:  "reviewer",
		"gitSecretRef": "missing-secret",
	}))
	if err != nil {
		t.Fatalf("Execute() returned error: %v", err)
	}
	var result ChatToolResult
	if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}
	if result.Success {
		t.Fatalf("expected missing gitSecretRef failure, got success: %s", resultJSON)
	}
	if result.ErrorType != errTypeInvalidArgs || !strings.Contains(result.Error, "missing-secret") {
		t.Fatalf("result = %#v, want missing explicit gitSecretRef invalid_arguments", result)
	}

	var task corev1alpha1.Task
	err = fc.Get(context.Background(), types.NamespacedName{Name: "pr-monitor-task", Namespace: defaultNamespace}, &task)
	if err == nil {
		t.Fatal("task was created despite missing explicit gitSecretRef")
	}
}

func TestCreatePRMonitorTool_ExecuteMissingAgentRef(t *testing.T) {
	result := executeCreatePRMonitorForFailure(t, newFakeClient(), map[string]any{
		nameField:     "daily-pr-monitor",
		scheduleField: "*/15 * * * *",
	})
	if result.ErrorType != errTypeInvalidArgs || !strings.Contains(result.Error, "agent_ref is required") {
		t.Fatalf("result = %#v, want missing agent_ref invalid_arguments", result)
	}
}

func TestCreatePRMonitorTool_ExecuteMissingRepoURL(t *testing.T) {
	result := executeCreatePRMonitorForFailure(t, newFakeClient(), map[string]any{
		nameField:     "daily-pr-monitor",
		scheduleField: "*/15 * * * *",
		agentRefField: "reviewer",
	})
	if result.ErrorType != errTypeInvalidArgs || !strings.Contains(result.Error, "repo_url is required") {
		t.Fatalf("result = %#v, want missing repo_url invalid_arguments", result)
	}
}

func TestCreatePRMonitorTool_ExecuteRejectsNonRepositoryRepoURL(t *testing.T) {
	for _, repoURL := range []string{
		"https://github.com/orka-agents/orka/pull/124",
		"https://github.com/orka-agents/orka/issues/124",
		"https://github.com/orka-agents/orka/tree/main",
	} {
		t.Run(repoURL, func(t *testing.T) {
			result := executeCreatePRMonitorForFailure(t, newFakeClient(), map[string]any{
				nameField:     "daily-pr-monitor",
				repoURLField:  repoURL,
				scheduleField: "*/15 * * * *",
				agentRefField: "reviewer",
			})
			if result.ErrorType != errTypeInvalidArgs || !strings.Contains(result.Error, "invalid repo_url") {
				t.Fatalf("result = %#v, want invalid repo_url", result)
			}
		})
	}
}

func TestCreatePRMonitorTool_ExecuteAgentNotFound(t *testing.T) {
	result := executeCreatePRMonitorForFailure(t, newFakeClient(), map[string]any{
		nameField:     "daily-pr-monitor",
		repoURLField:  testSozercanAynaRepoURL,
		scheduleField: "*/15 * * * *",
		agentRefField: "missing-agent",
	})
	if result.ErrorType != errTypeInvalidArgs || !strings.Contains(result.Error, "not found") {
		t.Fatalf("result = %#v, want agent not found invalid_arguments", result)
	}
}

func TestCreatePRMonitorTool_ExecuteAgentCoordinationDisabled(t *testing.T) {
	fc := newFakeClient(&corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "reviewer", Namespace: defaultNamespace},
		Spec: corev1alpha1.AgentSpec{
			Coordination: &corev1alpha1.CoordinationConfig{Enabled: false},
		},
	})
	result := executeCreatePRMonitorForFailure(t, fc, map[string]any{
		nameField:     "daily-pr-monitor",
		repoURLField:  testSozercanAynaRepoURL,
		scheduleField: "*/15 * * * *",
		agentRefField: "reviewer",
	})
	if result.ErrorType != errTypeInvalidArgs || !strings.Contains(result.Error, "must have coordination enabled") {
		t.Fatalf("result = %#v, want coordination disabled invalid_arguments", result)
	}
}

func TestCreatePRMonitorTool_ExecuteRuntimeAgentRejected(t *testing.T) {
	fc := newFakeClient(&corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "runtime-reviewer", Namespace: defaultNamespace},
		Spec: corev1alpha1.AgentSpec{
			Runtime:      &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeCodex},
			Coordination: &corev1alpha1.CoordinationConfig{Enabled: true},
		},
	})
	result := executeCreatePRMonitorForFailure(t, fc, map[string]any{
		nameField:     "daily-pr-monitor",
		repoURLField:  testSozercanAynaRepoURL,
		scheduleField: "*/15 * * * *",
		agentRefField: "runtime-reviewer",
	})
	if result.ErrorType != errTypeInvalidArgs || !strings.Contains(result.Error, "runtime Agent") {
		t.Fatalf("result = %#v, want runtime agent invalid_arguments", result)
	}
}

func TestCreatePRMonitorTool_ExecuteAutonomousAgentRejected(t *testing.T) {
	fc := newFakeClient(&corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "autonomous-reviewer", Namespace: defaultNamespace},
		Spec: corev1alpha1.AgentSpec{
			Coordination: &corev1alpha1.CoordinationConfig{Enabled: true, Autonomous: true},
		},
	})
	result := executeCreatePRMonitorForFailure(t, fc, map[string]any{
		nameField:     "daily-pr-monitor",
		repoURLField:  testSozercanAynaRepoURL,
		scheduleField: "*/15 * * * *",
		agentRefField: "autonomous-reviewer",
	})
	if result.ErrorType != errTypeInvalidArgs || !strings.Contains(result.Error, "autonomous") {
		t.Fatalf("result = %#v, want autonomous agent invalid_arguments", result)
	}
}

func TestCreatePRMonitorTool_ExecuteInvalidReviewEventRejected(t *testing.T) {
	fc := newFakeClient(&corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "reviewer", Namespace: defaultNamespace},
		Spec: corev1alpha1.AgentSpec{
			Coordination: &corev1alpha1.CoordinationConfig{Enabled: true},
		},
	})
	result := executeCreatePRMonitorForFailure(t, fc, map[string]any{
		nameField:      "daily-pr-monitor",
		repoURLField:   testSozercanAynaRepoURL,
		scheduleField:  "*/15 * * * *",
		agentRefField:  "reviewer",
		"review_event": "INVALID",
	})
	if result.ErrorType != errTypeInvalidArgs || !strings.Contains(result.Error, "invalid review_event") {
		t.Fatalf("result = %#v, want invalid review event", result)
	}
}

func executeCreatePRMonitorForFailure(t *testing.T, c client.Client, args map[string]any) ChatToolResult {
	t.Helper()
	ctx := newCreatePRMonitorToolContext(c)
	resultJSON, err := (&CreatePRMonitorTool{}).Execute(ctx, mustJSON(t, args))
	if err != nil {
		t.Fatalf("Execute() returned error: %v", err)
	}
	var result ChatToolResult
	if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}
	if result.Success {
		t.Fatalf("expected failure, got success: %s", resultJSON)
	}
	return result
}

func newCreatePRMonitorToolContext(c client.Client) context.Context {
	return newCreatePRMonitorToolContextWithAuthorize(c, nil)
}

func newCreatePRMonitorToolContextWithAuthorize(c client.Client, authorize func(context.Context, *corev1alpha1.Task) *ChatToolError) context.Context {
	return WithToolContext(context.Background(), &ToolContext{
		Client:              c,
		Namespace:           defaultNamespace,
		AuthorizeTaskCreate: authorize,
		GenerateTaskName: func() string {
			return "pr-monitor-task"
		},
		TaskLabels: func() map[string]string {
			return map[string]string{managedByLabelValue: trueStr}
		},
		CheckTaskLimit: func() *ChatToolError { return nil },
		IncrementTasks: func() {},
	})
}

func mustJSON(t *testing.T, v map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("failed to marshal JSON: %v", err)
	}
	return b
}

func hasEnvVar(values []corev1.EnvVar, name, want string) bool {
	for _, value := range values {
		if value.Name == name && value.Value == want {
			return true
		}
	}
	return false
}

func containsString(values []string, want string) bool {
	return slices.Contains(values, want)
}

func containsAnyString(values []any, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
