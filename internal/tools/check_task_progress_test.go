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
	errTypeInternalError = "internal_error"
)

func TestCheckTaskProgressTool_Name(t *testing.T) {
	tool := &CheckTaskProgressTool{}
	if got := tool.Name(); got != "check_task_progress" {
		t.Errorf("Name() = %v, want %v", got, "check_task_progress")
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
	if schema["type"] != typeObject {
		t.Error("Parameters schema should have type: object")
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("missing properties")
	}
	for _, key := range []string{"name", "namespace"} {
		if _, ok := props[key]; !ok {
			t.Errorf("missing %s property", key)
		}
	}
}

func TestCheckTaskProgressTool_Execute(t *testing.T) {
	newToolCtx := func(fc client.Client) context.Context {
		tc := &ToolContext{
			Client:    fc,
			Namespace: "default",
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
						Name:      "my-task",
						Namespace: "default",
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
				if data["name"] != "my-task" {
					t.Errorf("name = %v, want my-task", data["name"])
				}
				if data["phase"] != "Running" {
					t.Errorf("phase = %v, want Running", data["phase"])
				}
				if data["message"] != "Processing" {
					t.Errorf("message = %v, want Processing", data["message"])
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
						Namespace: "default",
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
				if data["phase"] != "Succeeded" {
					t.Errorf("phase = %v, want Succeeded", data["phase"])
				}
				conditions, ok := data["conditions"].([]any)
				if !ok {
					t.Fatal("expected conditions to be a slice")
				}
				if len(conditions) != 1 {
					t.Fatalf("expected 1 condition, got %d", len(conditions))
				}
				cond := conditions[0].(map[string]any)
				if cond["type"] != "Ready" {
					t.Errorf("condition type = %v, want Ready", cond["type"])
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
						Namespace: "prod",
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
				if data["namespace"] != "prod" {
					t.Errorf("namespace = %v, want prod", data["namespace"])
				}
			},
		},
		{
			name: "missing name",
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
			name: "invalid JSON args",
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
			name: "task not found",
			args: json.RawMessage(`{"name":"nonexistent"}`),
			checkResult: func(t *testing.T, result string) {
				var r ChatToolResult
				if err := json.Unmarshal([]byte(result), &r); err != nil {
					t.Fatalf("failed to parse result: %v", err)
				}
				if r.Success {
					t.Error("expected failure for not found")
				}
				if r.ErrorType != "not_found" {
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
