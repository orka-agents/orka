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
	apitypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/labels"
)

const testCancelTaskName = "cancel_task"

func TestCancelTaskTool_Name(t *testing.T) {
	tool := NewCancelTaskTool(newFakeClient())
	if got := tool.Name(); got != testCancelTaskName {
		t.Errorf("Name() = %v, want %v", got, testCancelTaskName)
	}
}

func TestCancelTaskTool_Description(t *testing.T) {
	tool := NewCancelTaskTool(newFakeClient())
	if got := tool.Description(); got == "" {
		t.Error("Description() returned empty string")
	}
}

func TestCancelTaskTool_Parameters(t *testing.T) {
	tool := NewCancelTaskTool(newFakeClient())
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
	if _, ok := props["task_name"]; !ok {
		t.Error("missing task_name property")
	}
	if _, ok := props["namespace"]; !ok {
		t.Error("missing namespace property")
	}
	if _, ok := props["reason"]; !ok {
		t.Error("missing reason property")
	}
}

func TestCancelTaskTool_Execute(t *testing.T) {
	tests := []struct {
		name       string
		args       CancelTaskArgs
		childTask  *corev1alpha1.Task
		envVars    map[string]string
		wantErr    bool
		wantStatus string
	}{
		{
			name: "cancel running child task",
			args: CancelTaskArgs{
				TaskName:  "child-task-1",
				Namespace: "default",
				Reason:    "wrong approach",
			},
			childTask: &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "child-task-1",
					Namespace: "default",
					Labels: map[string]string{
						labels.LabelParentTask: parentTaskName,
					},
				},
				Spec: corev1alpha1.TaskSpec{
					Type: corev1alpha1.TaskTypeAI,
				},
				Status: corev1alpha1.TaskStatus{
					Phase: corev1alpha1.TaskPhaseRunning,
				},
			},
			envVars:    map[string]string{"ORKA_TASK_NAME": parentTaskName, "ORKA_TASK_NAMESPACE": "default"},
			wantStatus: "cancelled",
		},
		{
			name: "cancel pending child task",
			args: CancelTaskArgs{
				TaskName:  "child-task-2",
				Namespace: "default",
			},
			childTask: &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "child-task-2",
					Namespace: "default",
					Labels: map[string]string{
						labels.LabelParentTask: parentTaskName,
					},
				},
				Spec: corev1alpha1.TaskSpec{
					Type: corev1alpha1.TaskTypeAI,
				},
				Status: corev1alpha1.TaskStatus{
					Phase: corev1alpha1.TaskPhasePending,
				},
			},
			envVars:    map[string]string{"ORKA_TASK_NAME": parentTaskName, "ORKA_TASK_NAMESPACE": "default"},
			wantStatus: "cancelled",
		},
		{
			name: "cannot cancel already succeeded task",
			args: CancelTaskArgs{
				TaskName:  "child-task-3",
				Namespace: "default",
			},
			childTask: &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "child-task-3",
					Namespace: "default",
					Labels: map[string]string{
						labels.LabelParentTask: parentTaskName,
					},
				},
				Spec: corev1alpha1.TaskSpec{
					Type: corev1alpha1.TaskTypeAI,
				},
				Status: corev1alpha1.TaskStatus{
					Phase: corev1alpha1.TaskPhaseSucceeded,
				},
			},
			envVars:    map[string]string{"ORKA_TASK_NAME": parentTaskName, "ORKA_TASK_NAMESPACE": "default"},
			wantStatus: "task is already in Succeeded phase, cannot cancel",
		},
		{
			name: "cannot cancel task from different parent",
			args: CancelTaskArgs{
				TaskName:  "other-child",
				Namespace: "default",
			},
			childTask: &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "other-child",
					Namespace: "default",
					Labels: map[string]string{
						labels.LabelParentTask: "other-parent",
					},
				},
				Spec: corev1alpha1.TaskSpec{
					Type: corev1alpha1.TaskTypeAI,
				},
				Status: corev1alpha1.TaskStatus{
					Phase: corev1alpha1.TaskPhaseRunning,
				},
			},
			envVars: map[string]string{"ORKA_TASK_NAME": parentTaskName, "ORKA_TASK_NAMESPACE": "default"},
			wantErr: true,
		},
		{
			name: "cancel without ORKA_TASK_NAME skips parent check",
			args: CancelTaskArgs{
				TaskName:  "child-task-no-parent",
				Namespace: "default",
			},
			childTask: &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "child-task-no-parent",
					Namespace: "default",
				},
				Spec: corev1alpha1.TaskSpec{
					Type: corev1alpha1.TaskTypeAI,
				},
				Status: corev1alpha1.TaskStatus{
					Phase: corev1alpha1.TaskPhaseRunning,
				},
			},
			envVars:    map[string]string{},
			wantStatus: "cancelled",
		},
		{
			name: "missing task name",
			args: CancelTaskArgs{
				Namespace: "default",
			},
			envVars: map[string]string{"ORKA_TASK_NAME": parentTaskName},
			wantErr: true,
		},
		{
			name: "task not found",
			args: CancelTaskArgs{
				TaskName:  "nonexistent",
				Namespace: "default",
			},
			envVars: map[string]string{"ORKA_TASK_NAME": parentTaskName},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.envVars {
				t.Setenv(k, v)
			}

			var objs []client.Object
			if tt.childTask != nil {
				objs = append(objs, tt.childTask)
			}

			fc := newFakeClient(objs...)
			tool := NewCancelTaskTool(fc)

			argsJSON, _ := json.Marshal(tt.args)
			result, err := tool.Execute(context.Background(), argsJSON)

			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			var res CancelTaskResult
			if err := json.Unmarshal([]byte(result), &res); err != nil {
				t.Fatalf("failed to parse result: %v", err)
			}

			if res.Status != tt.wantStatus {
				t.Errorf("status = %q, want %q", res.Status, tt.wantStatus)
			}

			// Verify task was updated if cancelled
			if tt.wantStatus == "cancelled" && tt.childTask != nil {
				updated := &corev1alpha1.Task{}
				err := fc.Get(context.Background(), apitypes.NamespacedName{
					Name:      tt.childTask.Name,
					Namespace: tt.childTask.Namespace,
				}, updated)
				if err != nil {
					t.Fatalf("failed to get updated task: %v", err)
				}
				if updated.Status.Phase != corev1alpha1.TaskPhaseCancelled {
					t.Errorf("task phase = %v, want Cancelled", updated.Status.Phase)
				}
			}
		})
	}
}
