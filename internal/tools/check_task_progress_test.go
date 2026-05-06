/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
)

const (
	errTypeInvalidArgs   = "invalid_arguments"
	errTypeInternalError = internalErrorType
)

func TestCheckTaskProgressTool_Name(t *testing.T) {
	tool := &CheckTaskProgressTool{}
	if got := tool.Name(); got != checkTaskProgressToolName {
		t.Errorf("Name() = %v, want %v", got, checkTaskProgressToolName)
	}
}

func TestCheckTaskProgressTool_Description(t *testing.T) {
	tool := &CheckTaskProgressTool{}
	if got := tool.Description(); got == "" {
		t.Error("Description() returned empty string")
	}
}

func TestCheckTaskProgressTool_Parameters(t *testing.T) {
	tool := &CheckTaskProgressTool{}
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
	for _, key := range []string{nameField, namespaceField} {
		if _, ok := props[key]; !ok {
			t.Errorf("missing %s property", key)
		}
	}
}

func TestCheckTaskProgressTool_Execute(t *testing.T) {
	newToolCtx := func(fc client.Client) context.Context {
		tc := &ToolContext{
			Client:    fc,
			Namespace: defaultNamespace,
		}
		return WithToolContext(context.Background(), tc)
	}

	startTime := metav1.NewTime(time.Now().Add(-5 * time.Minute))

	tests := []struct {
		name        string
		args        json.RawMessage
		objects     []client.Object
		checkResult func(t *testing.T, result string)
	}{
		{
			name: "happy path - running task",
			args: json.RawMessage(`{"name":"my-task"}`),
			objects: []client.Object{
				&corev1alpha1.Task{
					ObjectMeta: metav1.ObjectMeta{
						Name:      testMyTaskName,
						Namespace: defaultNamespace,
					},
					Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
					Status: corev1alpha1.TaskStatus{
						Phase:     corev1alpha1.TaskPhaseRunning,
						Message:   "Processing",
						StartTime: &startTime,
					},
				},
			},
			checkResult: func(t *testing.T, result string) {
				var r ChatToolResult
				if err := json.Unmarshal([]byte(result), &r); err != nil {
					t.Fatalf("failed to parse result: %v", err)
				}
				if !r.Success {
					t.Errorf("expected success, got error: %s", r.Error)
				}
				data := r.Data.(map[string]any)
				if data[nameField] != testMyTaskName {
					t.Errorf("name = %v, want my-task", data[nameField])
				}
				if data[phaseField] != taskPhaseRunningString {
					t.Errorf("phase = %v, want Running", data[phaseField])
				}
				if data[messageField] != "Processing" {
					t.Errorf("message = %v, want Processing", data[messageField])
				}
				if _, ok := data["duration"]; !ok {
					t.Error("expected duration to be set for running task with start time")
				}
			},
		},
		{
			name: "task with conditions",
			args: json.RawMessage(`{"name":"cond-task"}`),
			objects: []client.Object{
				&corev1alpha1.Task{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "cond-task",
						Namespace: defaultNamespace,
					},
					Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
					Status: corev1alpha1.TaskStatus{
						Phase: corev1alpha1.TaskPhaseSucceeded,
						Conditions: []metav1.Condition{
							{
								Type:               "Ready",
								Status:             metav1.ConditionTrue,
								Reason:             "TaskCompleted",
								Message:            "Task completed successfully",
								LastTransitionTime: metav1.Now(),
							},
						},
					},
				},
			},
			checkResult: func(t *testing.T, result string) {
				var r ChatToolResult
				if err := json.Unmarshal([]byte(result), &r); err != nil {
					t.Fatalf("failed to parse result: %v", err)
				}
				if !r.Success {
					t.Errorf("expected success, got error: %s", r.Error)
				}
				data := r.Data.(map[string]any)
				if data[phaseField] != taskPhaseSucceededString {
					t.Errorf("phase = %v, want Succeeded", data[phaseField])
				}
				conditions, ok := data["conditions"].([]any)
				if !ok {
					t.Fatal("expected conditions to be a slice")
				}
				if len(conditions) != 1 {
					t.Fatalf("expected 1 condition, got %d", len(conditions))
				}
				cond := conditions[0].(map[string]any)
				if cond[jsonSchemaTypeField] != "Ready" {
					t.Errorf("condition type = %v, want Ready", cond[jsonSchemaTypeField])
				}
			},
		},
		{
			name: "task in explicit namespace",
			args: json.RawMessage(`{"name":"ns-task","namespace":"prod"}`),
			objects: []client.Object{
				&corev1alpha1.Task{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "ns-task",
						Namespace: testProdNamespace,
					},
					Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
					Status: corev1alpha1.TaskStatus{
						Phase: corev1alpha1.TaskPhasePending,
					},
				},
			},
			checkResult: func(t *testing.T, result string) {
				var r ChatToolResult
				if err := json.Unmarshal([]byte(result), &r); err != nil {
					t.Fatalf("failed to parse result: %v", err)
				}
				if !r.Success {
					t.Errorf("expected success, got error: %s", r.Error)
				}
				data := r.Data.(map[string]any)
				if data[namespaceField] != testProdNamespace {
					t.Errorf("namespace = %v, want prod", data[namespaceField])
				}
			},
		},
		{
			name: missingNameCaseName,
			args: json.RawMessage(`{}`),
			checkResult: func(t *testing.T, result string) {
				var r ChatToolResult
				if err := json.Unmarshal([]byte(result), &r); err != nil {
					t.Fatalf("failed to parse result: %v", err)
				}
				if r.Success {
					t.Error("expected failure for missing name")
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
			name: taskNotFoundCaseName,
			args: json.RawMessage(`{"name":"nonexistent"}`),
			checkResult: func(t *testing.T, result string) {
				var r ChatToolResult
				if err := json.Unmarshal([]byte(result), &r); err != nil {
					t.Fatalf("failed to parse result: %v", err)
				}
				if r.Success {
					t.Error("expected failure for not found")
				}
				if r.ErrorType != errTypeNotFound {
					t.Errorf("errorType = %v, want not_found", r.ErrorType)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := newFakeClient(tt.objects...)
			ctx := newToolCtx(fc)
			tool := &CheckTaskProgressTool{}

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

func TestCheckTaskProgressTool_Execute_MissingContext(t *testing.T) {
	tool := &CheckTaskProgressTool{}
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"name":"t"}`))
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
