/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

const testAITaskName = "ai-task-1"

func TestCreateAITaskTool_Name(t *testing.T) {
	tool := &CreateAITaskTool{}
	if got := tool.Name(); got != createAITaskToolName {
		t.Errorf("Name() = %v, want %v", got, createAITaskToolName)
	}
}

func TestCreateAITaskTool_Description(t *testing.T) {
	tool := &CreateAITaskTool{}
	if got := tool.Description(); got == "" {
		t.Error("Description() returned empty string")
	}
}

func TestCreateAITaskTool_Parameters(t *testing.T) {
	tool := &CreateAITaskTool{}
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
	for _, key := range []string{nameField, promptField, agentRefField, providerRefField, namespaceField, timeoutField, priorityField, "sessionRef", scheduleField} {
		if _, ok := props[key]; !ok {
			t.Errorf("missing %s property", key)
		}
	}
}

func TestCreateAITaskTool_Execute(t *testing.T) {
	taskCounter := 0
	newToolCtx := func(fc client.Client) context.Context {
		taskCounter = 0
		tc := &ToolContext{
			Client:    fc,
			Namespace: defaultNamespace,
			GenerateTaskName: func() string {
				taskCounter++
				return testAITaskName
			},
			TaskLabels: func() map[string]string {
				return map[string]string{managedByLabelValue: trueStr}
			},
			CheckTaskLimit: func() *ChatToolError { return nil },
			IncrementTasks: func() { taskCounter++ },
		}
		return WithToolContext(context.Background(), tc)
	}

	tests := []struct {
		name        string
		args        json.RawMessage
		objects     []client.Object
		wantSuccess bool
		wantErr     bool
		checkResult func(t *testing.T, result string)
	}{
		{
			name:        "happy path with prompt",
			args:        json.RawMessage(`{"name":"my-ai-task","prompt":"Summarize this document"}`),
			wantSuccess: true,
			checkResult: func(t *testing.T, result string) {
				var r ChatToolResult
				if err := json.Unmarshal([]byte(result), &r); err != nil {
					t.Fatalf("failed to parse result: %v", err)
				}
				if !r.Success {
					t.Errorf("expected success, got error: %s", r.Error)
				}
				data, ok := r.Data.(map[string]any)
				if !ok {
					t.Fatal("data is not a map")
				}
				if data[nameField] != testAITaskName {
					t.Errorf("name = %v, want %s", data[nameField], testAITaskName)
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
			name:        "with optional fields",
			args:        json.RawMessage(`{"name":"my-ai-task","prompt":"Analyze code","agentRef":"my-agent","providerRef":"openai","timeout":"10m","priority":100,"sessionRef":"sess-1","schedule":"0 */6 * * *"}`),
			wantSuccess: true,
			checkResult: func(t *testing.T, result string) {
				var r ChatToolResult
				if err := json.Unmarshal([]byte(result), &r); err != nil {
					t.Fatalf("failed to parse result: %v", err)
				}
				if !r.Success {
					t.Errorf("expected success, got error: %s", r.Error)
				}
				data := r.Data.(map[string]any)
				if msg, ok := data[messageField].(string); !ok || msg == "" {
					t.Error("expected non-empty message for scheduled task")
				}
			},
		},
		{
			name: "missing prompt",
			args: json.RawMessage(`{"name":"my-ai-task"}`),
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
			name:    invalidJSONArgsCaseName,
			args:    json.RawMessage(invalidJSONText),
			wantErr: false,
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
			args: json.RawMessage(`{"name":"my-ai-task","prompt":"do it","timeout":"notaduration"}`),
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
			args: json.RawMessage(`{"name":"existing-task","prompt":"do something"}`),
			objects: []client.Object{
				&corev1alpha1.Task{
					ObjectMeta: metav1.ObjectMeta{
						Name:      testAITaskName,
						Namespace: defaultNamespace,
					},
					Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
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
			ctx := newToolCtx(fc)
			tool := &CreateAITaskTool{}

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

func TestCreateAITaskTool_Execute_OmittedProviderRefLeavesNil(t *testing.T) {
	fc := newFakeClient()
	taskCounter := 0
	ctx := WithToolContext(context.Background(), &ToolContext{
		Client:    fc,
		Namespace: defaultNamespace,
		GenerateTaskName: func() string {
			taskCounter++
			return testAITaskName
		},
		TaskLabels: func() map[string]string {
			return map[string]string{managedByLabelValue: trueStr}
		},
		CheckTaskLimit: func() *ChatToolError { return nil },
		IncrementTasks: func() { taskCounter++ },
	})

	tool := &CreateAITaskTool{}
	result, err := tool.Execute(ctx, json.RawMessage(`{"prompt":"Analyze code"}`))
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

	var created corev1alpha1.Task
	if err := fc.Get(context.Background(), client.ObjectKey{
		Name:      testAITaskName,
		Namespace: defaultNamespace,
	}, &created); err != nil {
		t.Fatalf("failed to get created task: %v", err)
	}

	if created.Spec.AI != nil && created.Spec.AI.ProviderRef != nil {
		t.Fatalf("AI.providerRef = %#v, want nil when providerRef argument is omitted", created.Spec.AI.ProviderRef)
	}
}

func TestCreateAITaskTool_Execute_ExplicitProviderRefPreserved(t *testing.T) {
	fc := newFakeClient()
	taskCounter := 0
	ctx := WithToolContext(context.Background(), &ToolContext{
		Client:    fc,
		Namespace: defaultNamespace,
		GenerateTaskName: func() string {
			taskCounter++
			return testAITaskName
		},
		TaskLabels: func() map[string]string {
			return map[string]string{managedByLabelValue: trueStr}
		},
		CheckTaskLimit: func() *ChatToolError { return nil },
		IncrementTasks: func() { taskCounter++ },
	})

	tool := &CreateAITaskTool{}
	result, err := tool.Execute(ctx, json.RawMessage(`{"prompt":"Analyze code","providerRef":"openai"}`))
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

	var created corev1alpha1.Task
	if err := fc.Get(context.Background(), client.ObjectKey{
		Name:      testAITaskName,
		Namespace: defaultNamespace,
	}, &created); err != nil {
		t.Fatalf("failed to get created task: %v", err)
	}

	if created.Spec.AI == nil {
		t.Fatal("AI spec is nil, want providerRef")
	}
	if created.Spec.AI.ProviderRef == nil {
		t.Fatal("AI.providerRef is nil, want openai")
	}
	if created.Spec.AI.ProviderRef.Name != providerOpenAI {
		t.Fatalf("AI.providerRef.name = %q, want openai", created.Spec.AI.ProviderRef.Name)
	}
}

func TestCreateAITaskTool_Execute_SessionRefCreatesAndAppends(t *testing.T) {
	fc := newFakeClient()
	taskCounter := 0
	ctx := WithToolContext(context.Background(), &ToolContext{
		Client:    fc,
		Namespace: defaultNamespace,
		GenerateTaskName: func() string {
			taskCounter++
			return testAITaskName
		},
		TaskLabels: func() map[string]string {
			return map[string]string{managedByLabelValue: trueStr}
		},
		CheckTaskLimit: func() *ChatToolError { return nil },
		IncrementTasks: func() { taskCounter++ },
	})

	tool := &CreateAITaskTool{}
	result, err := tool.Execute(ctx, json.RawMessage(`{"prompt":"Analyze code","sessionRef":"sess-1"}`))
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

	var created corev1alpha1.Task
	if err := fc.Get(context.Background(), client.ObjectKey{
		Name:      testAITaskName,
		Namespace: defaultNamespace,
	}, &created); err != nil {
		t.Fatalf("failed to get created task: %v", err)
	}

	if created.Spec.SessionRef == nil {
		t.Fatal("SessionRef is nil")
	}
	if created.Spec.SessionRef.Name != "sess-1" {
		t.Errorf("SessionRef.Name = %q, want sess-1", created.Spec.SessionRef.Name)
	}
	if !created.Spec.SessionRef.Create {
		t.Error("SessionRef.Create = false, want true")
	}
	if !created.Spec.SessionRef.Append {
		t.Error("SessionRef.Append = false, want true")
	}
}

func TestCreateAITaskTool_Execute_MissingContext(t *testing.T) {
	tool := &CreateAITaskTool{}
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"name":"t","prompt":"p"}`))
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
	if r.ErrorType != internalErrorType {
		t.Errorf("errorType = %v, want internal_error", r.ErrorType)
	}
}
