/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apitypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

const (
	msgTaskCreated           = "Task created"
	errTypeAlreadyExists     = "already_exists"
	testGitCredentialsSecret = "git-credentials"
)

func TestCreateAgentTaskTool_Name(t *testing.T) {
	tool := &CreateAgentTaskTool{}
	if got := tool.Name(); got != createAgentTaskToolName {
		t.Errorf("Name() = %v, want %v", got, createAgentTaskToolName)
	}
}

func TestCreateAgentTaskTool_Description(t *testing.T) {
	tool := &CreateAgentTaskTool{}
	if got := tool.Description(); got == "" {
		t.Error("Description() returned empty string")
	}
}

func TestCreateAgentTaskTool_Parameters(t *testing.T) {
	tool := &CreateAgentTaskTool{}
	params := tool.Parameters()
	if params == nil {
		t.Fatal("Parameters() returned nil")
	}
	var schema map[string]any
	if err := json.Unmarshal(params, &schema); err != nil {
		t.Fatalf("Parameters() returned invalid JSON: %v", err)
	}
	if schema[jsonSchemaTypeField] != typeObject {
		t.Error("Parameters schema should have type: object")
	}
	props, ok := schema[jsonSchemaPropertiesField].(map[string]any)
	if !ok {
		t.Fatal("missing properties")
	}
	for _, key := range []string{nameField, promptField, agentRefField, namespaceField, timeoutField, "maxTurns", workspaceField, scheduleField} {
		if _, ok := props[key]; !ok {
			t.Errorf("missing %s property", key)
		}
	}
}

func newCreateAgentTaskToolCtx(fc client.Client) context.Context {
	taskCounter := 0
	tc := &ToolContext{
		Client:    fc,
		Namespace: defaultNamespace,
		GenerateTaskName: func() string {
			taskCounter++
			return testAgentTaskGeneratedName
		},
		TaskLabels: func() map[string]string {
			return map[string]string{managedByLabelValue: trueStr}
		},
		CheckTaskLimit: func() *ChatToolError { return nil },
		IncrementTasks: func() { taskCounter++ },
	}
	return WithToolContext(context.Background(), tc)
}

func TestCreateAgentTaskTool_Execute(t *testing.T) {
	tests := []struct {
		name        string
		args        json.RawMessage
		objects     []client.Object
		checkResult func(t *testing.T, result string)
	}{
		{
			name: "happy path",
			args: json.RawMessage(`{"name":"my-agent-task","prompt":"Fix the bug","agentRef":"copilot-agent"}`),
			checkResult: func(t *testing.T, result string) {
				var r ChatToolResult
				if err := json.Unmarshal([]byte(result), &r); err != nil {
					t.Fatalf("failed to parse result: %v", err)
				}
				if !r.Success {
					t.Errorf("expected success, got error: %s", r.Error)
				}
				data := r.Data.(map[string]any)
				if data[nameField] != testAgentTaskGeneratedName {
					t.Errorf("name = %v, want agent-task-1", data[nameField])
				}
				if data[namespaceField] != defaultNamespace {
					t.Errorf("namespace = %v, want default", data[namespaceField])
				}
				if data[phaseField] != taskPhasePendingString {
					t.Errorf("phase = %v, want Pending", data[phaseField])
				}
			},
		},
		{
			name: "with workspace and maxTurns",
			args: json.RawMessage(`{
				"name":"ws-task",
				"prompt":"Refactor module",
				"agentRef":"claude-agent",
				"maxTurns": 10,
				"workspace": {
					"gitRepo": "https://github.com/example/repo",
					"branch": "main",
					"pushBranch": "feature/refactor",
					"subPath": "src"
				}
			}`),
			checkResult: func(t *testing.T, result string) {
				var r ChatToolResult
				if err := json.Unmarshal([]byte(result), &r); err != nil {
					t.Fatalf("failed to parse result: %v", err)
				}
				if !r.Success {
					t.Errorf("expected success, got error: %s", r.Error)
				}
			},
		},
		{
			name: "with schedule",
			args: json.RawMessage(`{"name":"sched-task","prompt":"Run nightly","agentRef":"agent","schedule":"0 0 * * *"}`),
			checkResult: func(t *testing.T, result string) {
				var r ChatToolResult
				if err := json.Unmarshal([]byte(result), &r); err != nil {
					t.Fatalf("failed to parse result: %v", err)
				}
				if !r.Success {
					t.Errorf("expected success, got error: %s", r.Error)
				}
				data := r.Data.(map[string]any)
				msg := data[messageField].(string)
				if msg == msgTaskCreated {
					t.Error("expected scheduled message, got one-time message")
				}
			},
		},
		{
			name: "missing prompt",
			args: json.RawMessage(`{"name":"t","agentRef":"a"}`),
			checkResult: func(t *testing.T, result string) {
				var r ChatToolResult
				if err := json.Unmarshal([]byte(result), &r); err != nil {
					t.Fatalf("failed to parse result: %v", err)
				}
				if r.Success {
					t.Error("expected failure for missing prompt")
				}
				if r.ErrorType != errTypeInvalidArgs {
					t.Errorf("errorType = %v, want invalid_arguments", r.ErrorType)
				}
			},
		},
		{
			name: "missing agentRef",
			args: json.RawMessage(`{"name":"t","prompt":"do it"}`),
			checkResult: func(t *testing.T, result string) {
				var r ChatToolResult
				if err := json.Unmarshal([]byte(result), &r); err != nil {
					t.Fatalf("failed to parse result: %v", err)
				}
				if r.Success {
					t.Error("expected failure for missing agentRef")
				}
				if r.ErrorType != errTypeInvalidArgs {
					t.Errorf("errorType = %v, want invalid_arguments", r.ErrorType)
				}
			},
		},
		{
			name: invalidJSONArgsCaseName,
			args: json.RawMessage(`{bad`),
			checkResult: func(t *testing.T, result string) {
				var r ChatToolResult
				if err := json.Unmarshal([]byte(result), &r); err != nil {
					t.Fatalf("failed to parse result: %v", err)
				}
				if r.Success {
					t.Error("expected failure for invalid JSON")
				}
				if r.ErrorType != errTypeInvalidArgs {
					t.Errorf("errorType = %v, want invalid_arguments", r.ErrorType)
				}
			},
		},
		{
			name: invalidTimeoutCaseName,
			args: json.RawMessage(`{"name":"t","prompt":"p","agentRef":"a","timeout":"bad"}`),
			checkResult: func(t *testing.T, result string) {
				var r ChatToolResult
				if err := json.Unmarshal([]byte(result), &r); err != nil {
					t.Fatalf("failed to parse result: %v", err)
				}
				if r.Success {
					t.Error("expected failure for invalid timeout")
				}
				if r.ErrorType != errTypeInvalidArgs {
					t.Errorf("errorType = %v, want invalid_arguments", r.ErrorType)
				}
			},
		},
		{
			name: k8sAlreadyExistsErrorCaseName,
			args: json.RawMessage(`{"name":"existing","prompt":"do it","agentRef":"a"}`),
			objects: []client.Object{
				&corev1alpha1.Task{
					ObjectMeta: metav1.ObjectMeta{
						Name:      testAgentTaskGeneratedName,
						Namespace: defaultNamespace,
					},
					Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
				},
			},
			checkResult: func(t *testing.T, result string) {
				var r ChatToolResult
				if err := json.Unmarshal([]byte(result), &r); err != nil {
					t.Fatalf("failed to parse result: %v", err)
				}
				if r.Success {
					t.Error("expected failure for already exists")
				}
				if r.ErrorType != errTypeAlreadyExists {
					t.Errorf("errorType = %v, want already_exists", r.ErrorType)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := newFakeClient(tt.objects...)
			ctx := newCreateAgentTaskToolCtx(fc)
			tool := &CreateAgentTaskTool{}

			result, err := tool.Execute(ctx, tt.args)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tt.checkResult != nil {
				tt.checkResult(t, result)
			}
		})
	}
}

func TestCreateAgentTaskTool_Execute_PreservesExplicitGitSecretRef(t *testing.T) {
	fc := newFakeClient(
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "my-secret", Namespace: defaultNamespace}},
	)
	ctx := newCreateAgentTaskToolCtx(fc)
	tool := &CreateAgentTaskTool{}

	result, err := tool.Execute(ctx, json.RawMessage(`{
		"prompt":"Refactor repo",
		"agentRef":"claude-agent",
		"workspace":{
			"gitRepo":"https://github.com/example/repo",
			"gitSecretRef":"my-secret"
		}
	}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var r ChatToolResult
	if err := json.Unmarshal([]byte(result), &r); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if !r.Success {
		t.Fatalf("expected success, got error: %s", r.Error)
	}

	task := &corev1alpha1.Task{}
	if err := fc.Get(context.Background(), apitypes.NamespacedName{Name: testAgentTaskGeneratedName, Namespace: defaultNamespace}, task); err != nil {
		t.Fatalf("failed to get task: %v", err)
	}
	if task.Spec.AgentRuntime == nil || task.Spec.AgentRuntime.Workspace == nil {
		t.Fatal("expected workspace to be set")
	}
	if task.Spec.AgentRuntime.Workspace.GitSecretRef == nil {
		t.Fatal("expected gitSecretRef to be preserved")
	}
	if task.Spec.AgentRuntime.Workspace.GitSecretRef.Name != "my-secret" {
		t.Errorf("gitSecretRef = %q, want %q", task.Spec.AgentRuntime.Workspace.GitSecretRef.Name, "my-secret")
	}
}

func TestCreateAgentTaskTool_Execute_AutoDiscoversGitSecretRefWhenOmitted(t *testing.T) {
	fc := newFakeClient(
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: testGitCredentialsSecret, Namespace: defaultNamespace}},
	)
	ctx := newCreateAgentTaskToolCtx(fc)
	tool := &CreateAgentTaskTool{}

	result, err := tool.Execute(ctx, json.RawMessage(`{
		"prompt":"Refactor repo",
		"agentRef":"claude-agent",
		"workspace":{
			"gitRepo":"https://github.com/example/repo"
		}
	}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var r ChatToolResult
	if err := json.Unmarshal([]byte(result), &r); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if !r.Success {
		t.Fatalf("expected success, got error: %s", r.Error)
	}

	task := &corev1alpha1.Task{}
	if err := fc.Get(context.Background(), apitypes.NamespacedName{Name: testAgentTaskGeneratedName, Namespace: defaultNamespace}, task); err != nil {
		t.Fatalf("failed to get task: %v", err)
	}
	if task.Spec.AgentRuntime == nil || task.Spec.AgentRuntime.Workspace == nil {
		t.Fatal("expected workspace to be set")
	}
	if task.Spec.AgentRuntime.Workspace.GitSecretRef == nil {
		t.Fatal("expected gitSecretRef to be auto-discovered")
	}
	if task.Spec.AgentRuntime.Workspace.GitSecretRef.Name != testGitCredentialsSecret {
		t.Fatalf("gitSecretRef = %q, want %q", task.Spec.AgentRuntime.Workspace.GitSecretRef.Name, testGitCredentialsSecret)
	}
}

func TestCreateAgentTaskTool_Execute_UsesCopilotAgentSecretForGitCredentials(t *testing.T) {
	fc := newFakeClient(
		&corev1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "copilot-agent", Namespace: defaultNamespace},
			Spec: corev1alpha1.AgentSpec{
				Runtime:   &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeCopilot},
				SecretRef: &corev1.LocalObjectReference{Name: testCustomCopilotSecretName},
			},
		},
	)
	ctx := newCreateAgentTaskToolCtx(fc)
	tool := &CreateAgentTaskTool{}

	result, err := tool.Execute(ctx, json.RawMessage(`{
		"prompt":"Refactor repo",
		"agentRef":"copilot-agent",
		"workspace":{
			"gitRepo":"https://github.com/example/repo"
		}
	}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var r ChatToolResult
	if err := json.Unmarshal([]byte(result), &r); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if !r.Success {
		t.Fatalf("expected success, got error: %s", r.Error)
	}

	task := &corev1alpha1.Task{}
	if err := fc.Get(context.Background(), apitypes.NamespacedName{Name: testAgentTaskGeneratedName, Namespace: defaultNamespace}, task); err != nil {
		t.Fatalf("failed to get task: %v", err)
	}
	if task.Spec.AgentRuntime == nil || task.Spec.AgentRuntime.Workspace == nil || task.Spec.AgentRuntime.Workspace.GitSecretRef == nil {
		t.Fatal("expected gitSecretRef to be populated from the copilot agent")
	}
	if task.Spec.AgentRuntime.Workspace.GitSecretRef.Name != testCustomCopilotSecretName {
		t.Fatalf("gitSecretRef = %q, want %q", task.Spec.AgentRuntime.Workspace.GitSecretRef.Name, testCustomCopilotSecretName)
	}
}

func TestCreateAgentTaskTool_Execute_MissingContext(t *testing.T) {
	tool := &CreateAgentTaskTool{}
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"name":"t","prompt":"p","agentRef":"a"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var r ChatToolResult
	if err := json.Unmarshal([]byte(result), &r); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if r.Success {
		t.Error("expected failure for missing context")
	}
	if r.ErrorType != errTypeInternalError {
		t.Errorf("errorType = %v, want internal_error", r.ErrorType)
	}
}
