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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestWaitForTaskTool_Name(t *testing.T) {
	tool := &WaitForTaskTool{}
	if got := tool.Name(); got != waitForTaskToolName {
		t.Errorf("Name() = %q, want %q", got, waitForTaskToolName)
	}
}

func TestWaitForTaskTool_Description(t *testing.T) {
	tool := &WaitForTaskTool{}
	if got := tool.Description(); got == "" {
		t.Error("Description() returned empty string")
	}
}

func TestWaitForTaskTool_Parameters(t *testing.T) {
	tool := &WaitForTaskTool{}
	params := tool.Parameters()
	if params == nil {
		t.Fatal("Parameters() returned nil")
	}
	var schema map[string]any
	if err := json.Unmarshal(params, &schema); err != nil {
		t.Fatalf("Parameters() returned invalid JSON: %v", err)
	}
	props, ok := schema[jsonSchemaPropertiesField].(map[string]any)
	if !ok {
		t.Fatal("missing properties")
	}
	if _, ok := props[nameField]; !ok {
		t.Error("missing name property")
	}
	if _, ok := props[timeoutField]; !ok {
		t.Error("missing timeout property")
	}
}

func TestWaitForTaskTool_Execute(t *testing.T) {
	now := metav1.Now()

	tests := []struct {
		name       string
		args       string
		task       *corev1alpha1.Task
		wantPhase  string
		wantErrStr string
	}{
		{
			name: "task already succeeded",
			args: `{"name": "done-task"}`,
			task: &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "done-task",
					Namespace: defaultNamespace,
				},
				Status: corev1alpha1.TaskStatus{
					Phase:          corev1alpha1.TaskPhaseSucceeded,
					StartTime:      &now,
					CompletionTime: &now,
				},
			},
			wantPhase: taskPhaseSucceededString,
		},
		{
			name: "task already failed",
			args: `{"name": "failed-task"}`,
			task: &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "failed-task",
					Namespace: defaultNamespace,
				},
				Status: corev1alpha1.TaskStatus{
					Phase:          corev1alpha1.TaskPhaseFailed,
					Message:        "something went wrong",
					StartTime:      &now,
					CompletionTime: &now,
				},
			},
			wantPhase: taskPhaseFailedString,
		},
		{
			name: "task still running — returns timeout result",
			args: `{"name": "running-task", "timeout": 1}`,
			task: &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testRunningTaskName,
					Namespace: defaultNamespace,
				},
				Status: corev1alpha1.TaskStatus{
					Phase:     corev1alpha1.TaskPhaseRunning,
					StartTime: &now,
				},
			},
			wantPhase: taskPhaseRunningString,
		},
		{
			name: testCustomNamespaceCaseName,
			args: `{"name": "ns-task", "namespace": "prod"}`,
			task: &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "ns-task",
					Namespace: testProdNamespace,
				},
				Status: corev1alpha1.TaskStatus{
					Phase: corev1alpha1.TaskPhaseSucceeded,
				},
			},
			wantPhase: taskPhaseSucceededString,
		},
		{
			name:       "missing name argument",
			args:       `{}`,
			wantErrStr: "name is required",
		},
		{
			name:       invalidJSONArgsCaseName,
			args:       `{bad json}`,
			wantErrStr: failedToParseArgumentsMessage,
		},
		{
			name:       taskNotFoundCaseName,
			args:       `{"name": "nonexistent"}`,
			wantErrStr: errTypeNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool := &WaitForTaskTool{}

			fc := newFakeClient()
			if tt.task != nil {
				fc = newFakeClient(tt.task)
			}

			tc := &ToolContext{
				Client:    fc,
				Namespace: defaultNamespace,
			}
			ctx := WithToolContext(context.Background(), tc)

			result, err := tool.Execute(ctx, json.RawMessage(tt.args))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tt.wantErrStr != "" {
				if !strings.Contains(result, tt.wantErrStr) {
					t.Errorf("result = %q, want to contain %q", result, tt.wantErrStr)
				}
				return
			}

			var res ChatToolResult
			if err := json.Unmarshal([]byte(result), &res); err != nil {
				t.Fatalf("failed to parse result: %v", err)
			}
			if !res.Success {
				t.Errorf("expected success=true, got false; result: %s", result)
			}

			data, ok := res.Data.(map[string]any)
			if !ok {
				t.Fatalf("expected data map, got %T", res.Data)
			}
			if phase, _ := data[phaseField].(string); phase != tt.wantPhase {
				t.Errorf("phase = %q, want %q", phase, tt.wantPhase)
			}
		})
	}
}

func TestWaitForTaskTool_Execute_MissingToolContext(t *testing.T) {
	tool := &WaitForTaskTool{}
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"name": "test"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "missing tool context") {
		t.Errorf("expected missing tool context error, got %q", result)
	}
}

func TestWaitForTaskTool_Execute_ContextCancelled(t *testing.T) {
	now := metav1.Now()
	tool := &WaitForTaskTool{}

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testRunningTaskName,
			Namespace: defaultNamespace,
		},
		Status: corev1alpha1.TaskStatus{
			Phase:     corev1alpha1.TaskPhaseRunning,
			StartTime: &now,
		},
	}

	tc := &ToolContext{
		Client:    newFakeClient(task),
		Namespace: defaultNamespace,
	}

	ctx, cancel := context.WithCancel(context.Background())
	ctx = WithToolContext(ctx, tc)
	cancel() // cancel immediately

	result, err := tool.Execute(ctx, json.RawMessage(`{"name": "running-task", "timeout": 60}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should return timeout/still-running result
	if !strings.Contains(result, "still running") {
		t.Errorf("expected still running message, got %q", result)
	}
}
