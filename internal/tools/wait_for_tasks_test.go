/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tools

import (
	"context"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/sozercan/mercan/api/v1alpha1"
)

func TestWaitForTasksTool_Name(t *testing.T) {
	tool := NewWaitForTasksTool(nil)
	if got := tool.Name(); got != "wait_for_tasks" {
		t.Errorf("Name() = %v, want %v", got, "wait_for_tasks")
	}
}

func TestWaitForTasksTool_Description(t *testing.T) {
	tool := NewWaitForTasksTool(nil)
	if desc := tool.Description(); desc == "" {
		t.Error("Description() returned empty string")
	}
}

func TestWaitForTasksTool_Parameters(t *testing.T) {
	tool := NewWaitForTasksTool(nil)
	params := tool.Parameters()
	if params == nil {
		t.Fatal("Parameters() returned nil")
	}

	var schema map[string]any
	if err := json.Unmarshal(params, &schema); err != nil {
		t.Errorf("Parameters() returned invalid JSON: %v", err)
	}
	if schema["type"] != "object" {
		t.Error("Parameters schema should have type: object")
	}
}

func TestWaitForTasksTool_Execute(t *testing.T) {
	tests := []struct {
		name          string
		tasks         []corev1alpha1.Task
		configMaps    []corev1.ConfigMap
		args          WaitForTasksArgs
		wantCompleted bool
		wantResults   []TaskResultInfo
		wantErr       bool
	}{
		{
			name: "all tasks succeeded",
			tasks: []corev1alpha1.Task{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-a", Namespace: "test-ns"},
					Spec: corev1alpha1.TaskSpec{
						Type:     corev1alpha1.TaskTypeAI,
						AgentRef: &corev1alpha1.AgentReference{Name: "agent-a"},
					},
					Status: corev1alpha1.TaskStatus{
						Phase: corev1alpha1.TaskPhaseSucceeded,
						ResultRef: &corev1alpha1.ResultReference{
							ConfigMapName: "result-task-a",
							Key:           "result",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-b", Namespace: "test-ns"},
					Spec: corev1alpha1.TaskSpec{
						Type:     corev1alpha1.TaskTypeAI,
						AgentRef: &corev1alpha1.AgentReference{Name: "agent-b"},
					},
					Status: corev1alpha1.TaskStatus{
						Phase: corev1alpha1.TaskPhaseSucceeded,
						ResultRef: &corev1alpha1.ResultReference{
							ConfigMapName: "result-task-b",
							Key:           "result",
						},
					},
				},
			},
			configMaps: []corev1.ConfigMap{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "result-task-a", Namespace: "test-ns"},
					Data:       map[string]string{"result": "result from task-a"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "result-task-b", Namespace: "test-ns"},
					Data:       map[string]string{"result": "result from task-b"},
				},
			},
			args:          WaitForTasksArgs{Tasks: []string{"task-a", "task-b"}, Timeout: "1s"},
			wantCompleted: true,
			wantResults: []TaskResultInfo{
				{Task: "task-a", Agent: "agent-a", Phase: "Succeeded", Result: "result from task-a"},
				{Task: "task-b", Agent: "agent-b", Phase: "Succeeded", Result: "result from task-b"},
			},
		},
		{
			name: "mixed results",
			tasks: []corev1alpha1.Task{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-ok", Namespace: "test-ns"},
					Spec: corev1alpha1.TaskSpec{
						Type:     corev1alpha1.TaskTypeAI,
						AgentRef: &corev1alpha1.AgentReference{Name: "agent-ok"},
					},
					Status: corev1alpha1.TaskStatus{
						Phase: corev1alpha1.TaskPhaseSucceeded,
						ResultRef: &corev1alpha1.ResultReference{
							ConfigMapName: "result-task-ok",
							Key:           "result",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-fail", Namespace: "test-ns"},
					Spec: corev1alpha1.TaskSpec{
						Type:     corev1alpha1.TaskTypeAI,
						AgentRef: &corev1alpha1.AgentReference{Name: "agent-fail"},
					},
					Status: corev1alpha1.TaskStatus{
						Phase:   corev1alpha1.TaskPhaseFailed,
						Message: "error: out of memory",
					},
				},
			},
			configMaps: []corev1.ConfigMap{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "result-task-ok", Namespace: "test-ns"},
					Data:       map[string]string{"result": "success output"},
				},
			},
			args:          WaitForTasksArgs{Tasks: []string{"task-ok", "task-fail"}, Timeout: "1s"},
			wantCompleted: true,
			wantResults: []TaskResultInfo{
				{Task: "task-ok", Agent: "agent-ok", Phase: "Succeeded", Result: "success output"},
				{Task: "task-fail", Agent: "agent-fail", Phase: "Failed", Result: "error: out of memory"},
			},
		},
		{
			name: "timeout with pending tasks",
			tasks: []corev1alpha1.Task{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-pending", Namespace: "test-ns"},
					Spec: corev1alpha1.TaskSpec{
						Type: corev1alpha1.TaskTypeAI,
					},
					Status: corev1alpha1.TaskStatus{
						Phase: corev1alpha1.TaskPhaseRunning,
					},
				},
			},
			args:          WaitForTasksArgs{Tasks: []string{"task-pending"}, Timeout: "100ms"},
			wantCompleted: false,
			wantResults: []TaskResultInfo{
				{Task: "task-pending", Phase: "Running"},
			},
		},
		{
			name:          "missing task",
			tasks:         []corev1alpha1.Task{},
			args:          WaitForTasksArgs{Tasks: []string{"nonexistent"}, Timeout: "100ms"},
			wantCompleted: true,
			wantResults: []TaskResultInfo{
				{Task: "nonexistent", Phase: "Error"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("MERCAN_TASK_NAMESPACE", "test-ns")

			scheme := newTestScheme()
			objs := make([]client.Object, 0, len(tt.tasks)+len(tt.configMaps))
			for i := range tt.tasks {
				objs = append(objs, &tt.tasks[i])
			}
			for i := range tt.configMaps {
				objs = append(objs, &tt.configMaps[i])
			}

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(objs...).
				WithStatusSubresource(&corev1alpha1.Task{}).
				Build()

			tool := NewWaitForTasksTool(fakeClient)

			argsJSON, err := json.Marshal(tt.args)
			if err != nil {
				t.Fatalf("failed to marshal args: %v", err)
			}

			result, err := tool.Execute(context.Background(), argsJSON)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Execute() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}

			var got WaitForTasksResult
			if err := json.Unmarshal([]byte(result), &got); err != nil {
				t.Fatalf("failed to unmarshal result: %v", err)
			}

			if got.Completed != tt.wantCompleted {
				t.Errorf("Completed = %v, want %v", got.Completed, tt.wantCompleted)
			}

			if len(got.Results) != len(tt.wantResults) {
				t.Fatalf("Results length = %d, want %d", len(got.Results), len(tt.wantResults))
			}

			for i, want := range tt.wantResults {
				gotR := got.Results[i]
				if gotR.Task != want.Task {
					t.Errorf("Results[%d].Task = %q, want %q", i, gotR.Task, want.Task)
				}
				if gotR.Agent != want.Agent {
					t.Errorf("Results[%d].Agent = %q, want %q", i, gotR.Agent, want.Agent)
				}
				if gotR.Phase != want.Phase {
					t.Errorf("Results[%d].Phase = %q, want %q", i, gotR.Phase, want.Phase)
				}
				if want.Result != "" && gotR.Result != want.Result {
					t.Errorf("Results[%d].Result = %q, want %q", i, gotR.Result, want.Result)
				}
				// For error cases, just check result is non-empty
				if want.Phase == "Error" && gotR.Result == "" {
					t.Errorf("Results[%d].Result should contain error info", i)
				}
			}
		})
	}
}

func TestWaitForTasksTool_Execute_EmptyTasks(t *testing.T) {
	tool := NewWaitForTasksTool(nil)
	args := json.RawMessage(`{"tasks": []}`)
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Error("Execute() expected error for empty tasks")
	}
}

func TestWaitForTasksTool_Execute_InvalidJSON(t *testing.T) {
	tool := NewWaitForTasksTool(nil)
	args := json.RawMessage(`{invalid}`)
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Error("Execute() expected error for invalid JSON")
	}
}

func TestWaitForTasksTool_Execute_InvalidTimeout(t *testing.T) {
	tool := NewWaitForTasksTool(nil)
	args := json.RawMessage(`{"tasks": ["t1"], "timeout": "not-a-duration"}`)
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Error("Execute() expected error for invalid timeout")
	}
}

func TestWaitForTasksTool_Execute_MissingNamespace(t *testing.T) {
	t.Setenv("MERCAN_TASK_NAMESPACE", "")
	tool := NewWaitForTasksTool(nil)
	args := json.RawMessage(`{"tasks": ["t1"]}`)
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Error("Execute() expected error for missing namespace")
	}
}
