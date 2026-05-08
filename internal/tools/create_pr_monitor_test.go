/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	apitypes "k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
)

const prMonitorGeneratedTaskName = "pr-monitor-task"

func TestCreatePRMonitorTool_Metadata(t *testing.T) {
	tool := &CreatePRMonitorTool{}

	if tool.Name() != createPRMonitorToolName {
		t.Errorf("Name() = %q, want %q", tool.Name(), createPRMonitorToolName)
	}
	if !strings.Contains(tool.Description(), "pull request monitor") {
		t.Errorf("Description() = %q, want pull request monitor", tool.Description())
	}

	params := tool.Parameters()
	if len(params) == 0 {
		t.Fatal("Parameters() returned empty JSON")
	}

	var schema map[string]any
	if err := json.Unmarshal(params, &schema); err != nil {
		t.Fatalf("failed to parse parameters schema: %v", err)
	}
	if schema[jsonSchemaTypeField] != jsonSchemaTypeObject {
		t.Errorf("schema type = %v, want %q", schema[jsonSchemaTypeField], jsonSchemaTypeObject)
	}

	props, ok := schema[jsonSchemaPropertiesField].(map[string]any)
	if !ok {
		t.Fatal("schema missing properties")
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
			t.Errorf("schema missing %q property", field)
		}
	}

	required, ok := schema[jsonSchemaRequiredField].([]any)
	if !ok {
		t.Fatalf("schema required = %T, want []any", schema[jsonSchemaRequiredField])
	}
	requiredSet := make(map[string]bool, len(required))
	for _, field := range required {
		fieldName, ok := field.(string)
		if !ok {
			t.Fatalf("required entry = %T, want string", field)
		}
		requiredSet[fieldName] = true
	}
	for _, field := range []string{nameField, scheduleField} {
		if !requiredSet[field] {
			t.Errorf("required missing %q", field)
		}
	}
}

func TestCreatePRMonitorTool_ExecuteMissingToolContext(t *testing.T) {
	tool := &CreatePRMonitorTool{}

	resultText, err := tool.Execute(context.Background(), json.RawMessage(`{"name":"monitor","schedule":"*/15 * * * *"}`))
	if err != nil {
		t.Fatalf("Execute() returned error: %v", err)
	}

	var result ChatToolResult
	if err := json.Unmarshal([]byte(resultText), &result); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if result.Success {
		t.Fatal("Success = true, want false")
	}
	if result.ErrorType != internalErrorType {
		t.Errorf("ErrorType = %q, want %q", result.ErrorType, internalErrorType)
	}
}

func TestCreatePRMonitorTool_ExecuteCreatesScheduledAITask(t *testing.T) {
	fc := newFakeClient()
	increments := 0
	tc := &ToolContext{
		Client:    fc,
		Namespace: defaultNamespace,
		GenerateTaskName: func() string {
			return prMonitorGeneratedTaskName
		},
		TaskLabels: func() map[string]string {
			return map[string]string{managedByLabelValue: trueStr}
		},
		CheckTaskLimit: func() *ChatToolError { return nil },
		IncrementTasks: func() {
			increments++
		},
	}
	ctx := WithToolContext(context.Background(), tc)

	tool := &CreatePRMonitorTool{}
	args, _ := json.Marshal(map[string]any{
		nameField:        "daily-pr-monitor",
		repoURLField:     testSozercanAynaRepoURL,
		scheduleField:    "*/15 * * * *",
		perPageField:     100,
		promptField:      "Focus on security-sensitive changes.",
		agentRefField:    "reviewer",
		providerRefField: "openai-provider",
	})

	resultText, err := tool.Execute(ctx, args)
	if err != nil {
		t.Fatalf("Execute() returned error: %v", err)
	}

	var result ChatToolResult
	if err := json.Unmarshal([]byte(resultText), &result); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if !result.Success {
		t.Fatalf("Success = false, result = %+v", result)
	}

	var task corev1alpha1.Task
	if err := fc.Get(
		context.Background(),
		apitypes.NamespacedName{Name: prMonitorGeneratedTaskName, Namespace: defaultNamespace},
		&task,
	); err != nil {
		t.Fatalf("created task not found: %v", err)
	}

	if task.Spec.Type != corev1alpha1.TaskTypeAI {
		t.Errorf("task type = %q, want %q", task.Spec.Type, corev1alpha1.TaskTypeAI)
	}
	if task.Spec.Schedule != "*/15 * * * *" {
		t.Errorf("schedule = %q, want */15 * * * *", task.Spec.Schedule)
	}
	if task.Labels[managedByLabelValue] != trueStr {
		t.Errorf("managed label = %q, want %q", task.Labels[managedByLabelValue], trueStr)
	}
	if task.Spec.AgentRef == nil || task.Spec.AgentRef.Name != "reviewer" {
		t.Fatalf("AgentRef = %#v, want reviewer", task.Spec.AgentRef)
	}
	if task.Spec.AI == nil || task.Spec.AI.ProviderRef == nil || task.Spec.AI.ProviderRef.Name != "openai-provider" {
		t.Fatalf("ProviderRef = %#v, want openai-provider", task.Spec.AI)
	}

	prompt := task.Spec.Prompt
	for _, want := range []string{
		"list_pull_requests",
		"check_pr_review_marker",
		"check_pull_request_ci",
		"review_pull_request",
		"post_review_comment",
		testSozercanAynaRepoURL,
		"per_page 100",
		"Do not review PRs with failing CI",
		"Include the marker returned by check_pr_review_marker",
		"Focus on security-sensitive changes.",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, prompt)
		}
	}

	if increments != 1 {
		t.Errorf("IncrementTasks called %d times, want 1", increments)
	}
}
