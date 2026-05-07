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

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/store"
)

// fakeResultStore implements the ResultStore interface for testing.
type fakeResultStore struct {
	data map[string][]byte
}

func (f *fakeResultStore) GetResult(_ context.Context, namespace, taskName string) ([]byte, error) {
	key := namespace + "/" + taskName
	d, ok := f.data[key]
	if !ok {
		return nil, store.ErrNotFound
	}
	return d, nil
}

func TestFetchTaskOutputTool_Name(t *testing.T) {
	tool := &FetchTaskOutputTool{}
	if got := tool.Name(); got != fetchTaskOutputToolName {
		t.Errorf("Name() = %q, want %q", got, fetchTaskOutputToolName)
	}
}

func TestFetchTaskOutputTool_Description(t *testing.T) {
	tool := &FetchTaskOutputTool{}
	if got := tool.Description(); got == "" {
		t.Error("Description() returned empty string")
	}
}

func TestFetchTaskOutputTool_Parameters(t *testing.T) {
	tool := &FetchTaskOutputTool{}
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
	if _, ok := props[namespaceField]; !ok {
		t.Error("missing namespace property")
	}
}

func TestFetchTaskOutputTool_Execute(t *testing.T) {
	tests := []struct {
		name       string
		args       string
		task       *corev1alpha1.Task
		storeData  map[string][]byte
		wantErr    bool
		wantOutput string
		wantErrStr string
	}{
		{
			name: "happy path — result available",
			args: `{"name": "my-task"}`,
			task: &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testMyTaskName,
					Namespace: defaultNamespace,
				},
				Status: corev1alpha1.TaskStatus{
					Phase:     corev1alpha1.TaskPhaseSucceeded,
					ResultRef: &corev1alpha1.ResultReference{Available: true},
				},
			},
			storeData:  map[string][]byte{"default/my-task": []byte("task output here")},
			wantOutput: "task output here",
		},
		{
			name: "result truncated when large",
			args: `{"name": "big-task", "namespace": "prod"}`,
			task: &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "big-task",
					Namespace: testProdNamespace,
				},
				Status: corev1alpha1.TaskStatus{
					Phase:     corev1alpha1.TaskPhaseSucceeded,
					ResultRef: &corev1alpha1.ResultReference{Available: true},
				},
			},
			storeData:  map[string][]byte{"prod/big-task": make([]byte, 3000)},
			wantOutput: "truncated",
		},
		{
			name:       "missing name argument",
			args:       `{"namespace": "` + defaultNamespace + `"}`,
			wantErrStr: "name is required",
		},
		{
			name:       invalidJSONArgsCaseName,
			args:       invalidJSONText,
			wantErrStr: failedToParseArgumentsMessage,
		},
		{
			name:       taskNotFoundCaseName,
			args:       `{"name": "nonexistent"}`,
			wantErrStr: errTypeNotFound,
		},
		{
			name: "result not yet available",
			args: `{"name": "running-task"}`,
			task: &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testRunningTaskName,
					Namespace: defaultNamespace,
				},
				Status: corev1alpha1.TaskStatus{
					Phase: corev1alpha1.TaskPhaseRunning,
				},
			},
			wantErrStr: "no result yet",
		},
		{
			name: "result ref available but store returns not found",
			args: `{"name": "deleted-result"}`,
			task: &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "deleted-result",
					Namespace: defaultNamespace,
				},
				Status: corev1alpha1.TaskStatus{
					Phase:     corev1alpha1.TaskPhaseSucceeded,
					ResultRef: &corev1alpha1.ResultReference{Available: true},
				},
			},
			storeData:  map[string][]byte{},
			wantErrStr: "result not found",
		},
		{
			name: "missing tool context",
			args: `{"name": "test"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool := &FetchTaskOutputTool{}

			// Special case: test missing tool context
			if tt.name == "missing tool context" {
				result, err := tool.Execute(context.Background(), json.RawMessage(tt.args))
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if !strings.Contains(result, "missing tool context") {
					t.Errorf("expected missing tool context error, got %q", result)
				}
				return
			}

			var objs []interface{ GetName() string }
			fc := newFakeClient()
			if tt.task != nil {
				fc = newFakeClient(tt.task)
			}

			_ = objs
			rs := &fakeResultStore{data: tt.storeData}
			tc := &ToolContext{
				Client:      fc,
				Namespace:   defaultNamespace,
				ResultStore: rs,
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

			if tt.wantOutput != "" {
				if !strings.Contains(result, tt.wantOutput) {
					t.Errorf("result = %q, want to contain %q", result, tt.wantOutput)
				}
			}

			// Verify success structure
			var res ChatToolResult
			if err := json.Unmarshal([]byte(result), &res); err != nil {
				t.Fatalf("failed to parse result: %v", err)
			}
			if !res.Success {
				t.Errorf("expected success=true, got false")
			}
		})
	}
}

func TestFetchTaskOutputTool_Execute_NilResultStore(t *testing.T) {
	tool := &FetchTaskOutputTool{}

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testMyTaskName,
			Namespace: defaultNamespace,
		},
		Status: corev1alpha1.TaskStatus{
			Phase:     corev1alpha1.TaskPhaseSucceeded,
			ResultRef: &corev1alpha1.ResultReference{Available: true},
		},
	}

	tc := &ToolContext{
		Client:      newFakeClient(task),
		Namespace:   defaultNamespace,
		ResultStore: nil,
	}
	ctx := WithToolContext(context.Background(), tc)

	result, err := tool.Execute(ctx, json.RawMessage(`{"name": "my-task"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "result store not configured") {
		t.Errorf("expected result store error, got %q", result)
	}
}
