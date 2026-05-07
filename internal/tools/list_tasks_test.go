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

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
)

func TestListTasksTool_Name(t *testing.T) {
	tool := &ListTasksTool{}
	if got := tool.Name(); got != listTasksToolName {
		t.Errorf("Name() = %v, want %v", got, listTasksToolName)
	}
}

func TestListTasksTool_Description(t *testing.T) {
	tool := &ListTasksTool{}
	if got := tool.Description(); got == "" {
		t.Error("Description() returned empty string")
	}
}

func TestListTasksTool_Parameters(t *testing.T) {
	tool := &ListTasksTool{}
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
	for _, key := range []string{namespaceField, statusField, limitField} {
		if _, ok := props[key]; !ok {
			t.Errorf("missing %s property", key)
		}
	}
}

func TestListTasksTool_Execute(t *testing.T) {
	tasks := []corev1alpha1.Task{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "task-1", Namespace: defaultNamespace},
			Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
			Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "task-2", Namespace: defaultNamespace},
			Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
			Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "task-3", Namespace: defaultNamespace},
			Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
			Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
		},
	}

	tests := []struct {
		name      string
		args      map[string]any
		wantCount int
		wantErr   bool
	}{
		{
			name:      "list all tasks",
			args:      map[string]any{},
			wantCount: 3,
		},
		{
			name:      "filter by status Running",
			args:      map[string]any{statusField: taskPhaseRunningString},
			wantCount: 2,
		},
		{
			name:      "filter by status Succeeded",
			args:      map[string]any{statusField: taskPhaseSucceededString},
			wantCount: 1,
		},
		{
			name:      "filter by status no match",
			args:      map[string]any{statusField: taskPhaseFailedString},
			wantCount: 0,
		},
		{
			name:      "limit results",
			args:      map[string]any{limitField: 1},
			wantCount: 1,
		},
		{
			name:      "case insensitive status filter",
			args:      map[string]any{statusField: "running"},
			wantCount: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := newFakeClient(&tasks[0], &tasks[1], &tasks[2])
			tc := &ToolContext{Client: fc, Namespace: defaultNamespace}
			ctx := WithToolContext(context.Background(), tc)

			argsJSON, _ := json.Marshal(tt.args)
			tool := &ListTasksTool{}
			result, err := tool.Execute(ctx, argsJSON)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			var res ChatToolResult
			if err := json.Unmarshal([]byte(result), &res); err != nil {
				t.Fatalf("failed to parse result: %v", err)
			}
			if !res.Success {
				t.Fatalf("expected success, got error: %s", res.Error)
			}

			data, ok := res.Data.([]any)
			if !ok {
				t.Fatalf("expected data to be array, got %T", res.Data)
			}
			if len(data) != tt.wantCount {
				t.Errorf("got %d tasks, want %d", len(data), tt.wantCount)
			}
		})
	}
}

func TestListTasksTool_Execute_MissingToolContext(t *testing.T) {
	tool := &ListTasksTool{}
	result, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var res ChatToolResult
	if err := json.Unmarshal([]byte(result), &res); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if res.Success {
		t.Error("expected failure for missing tool context")
	}
}

func TestListTasksTool_Execute_InvalidJSON(t *testing.T) {
	fc := newFakeClient()
	tc := &ToolContext{Client: fc, Namespace: defaultNamespace}
	ctx := WithToolContext(context.Background(), tc)

	tool := &ListTasksTool{}
	result, err := tool.Execute(ctx, json.RawMessage(invalidJSONText))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var res ChatToolResult
	if err := json.Unmarshal([]byte(result), &res); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if res.Success {
		t.Error("expected failure for invalid JSON")
	}
	if res.ErrorType != "invalid_arguments" {
		t.Errorf("expected errorType 'invalid_arguments', got %q", res.ErrorType)
	}
}

func TestListTasksTool_Execute_EmptyList(t *testing.T) {
	fc := newFakeClient()
	tc := &ToolContext{Client: fc, Namespace: defaultNamespace}
	ctx := WithToolContext(context.Background(), tc)

	tool := &ListTasksTool{}
	result, err := tool.Execute(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var res ChatToolResult
	if err := json.Unmarshal([]byte(result), &res); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected success, got error: %s", res.Error)
	}
	data, ok := res.Data.([]any)
	if !ok {
		t.Fatalf("expected data to be array, got %T", res.Data)
	}
	if len(data) != 0 {
		t.Errorf("expected 0 tasks, got %d", len(data))
	}
}
