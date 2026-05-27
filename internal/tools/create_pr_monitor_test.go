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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
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
	for _, field := range []string{nameField, scheduleField, agentRefField} {
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
	fc := newFakeClient(&corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "reviewer", Namespace: defaultNamespace},
		Spec: corev1alpha1.AgentSpec{
			Coordination: &corev1alpha1.CoordinationConfig{Enabled: true},
		},
	})
	ctx := newCreatePRMonitorToolContext(fc)
	tool := &CreatePRMonitorTool{}

	resultJSON, err := tool.Execute(ctx, mustJSON(t, map[string]any{
		nameField:        "daily-pr-monitor",
		repoURLField:     "https://github.com/sozercan/orka",
		scheduleField:    "*/15 * * * *",
		agentRefField:    "reviewer",
		providerRefField: "default-provider",
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
	if task.Spec.AgentRef == nil || task.Spec.AgentRef.Name != "reviewer" {
		t.Fatalf("AgentRef = %#v, want reviewer", task.Spec.AgentRef)
	}
	if task.Spec.AI == nil {
		t.Fatal("AI spec is nil")
	}
	if task.Spec.AI.ProviderRef == nil || task.Spec.AI.ProviderRef.Name != "default-provider" {
		t.Fatalf("ProviderRef = %#v, want default-provider", task.Spec.AI.ProviderRef)
	}
	for _, tool := range prMonitorRequiredTools {
		if !containsString(task.Spec.AI.Tools, tool) {
			t.Errorf("AI tools = %v, want %q", task.Spec.AI.Tools, tool)
		}
	}
	if !strings.Contains(task.Spec.Prompt, "list_pull_requests") {
		t.Errorf("prompt missing list_pull_requests: %s", task.Spec.Prompt)
	}
	if !strings.Contains(task.Spec.Prompt, "repo_url \"https://github.com/sozercan/orka\"") {
		t.Errorf("prompt missing repo URL: %s", task.Spec.Prompt)
	}
	if !strings.Contains(task.Spec.Prompt, "Focus on regressions.") {
		t.Errorf("prompt missing extra instructions: %s", task.Spec.Prompt)
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

func TestCreatePRMonitorTool_ExecuteAgentNotFound(t *testing.T) {
	result := executeCreatePRMonitorForFailure(t, newFakeClient(), map[string]any{
		nameField:     "daily-pr-monitor",
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
		scheduleField: "*/15 * * * *",
		agentRefField: "reviewer",
	})
	if result.ErrorType != errTypeInvalidArgs || !strings.Contains(result.Error, "must have coordination enabled") {
		t.Fatalf("result = %#v, want coordination disabled invalid_arguments", result)
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
	return WithToolContext(context.Background(), &ToolContext{
		Client:    c,
		Namespace: defaultNamespace,
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
