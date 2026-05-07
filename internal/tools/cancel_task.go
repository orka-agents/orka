/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/labels"
)

// CancelTaskTool implements task cancellation for coordination workflows
type CancelTaskTool struct {
	k8sClient client.Client
}

// CancelTaskArgs are the arguments for the cancel_task tool
type CancelTaskArgs struct {
	TaskName  string `json:"task_name"`
	Namespace string `json:"namespace,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// CancelTaskResult represents the cancellation result
type CancelTaskResult struct {
	TaskName string `json:"taskName"`
	Status   string `json:"status"`
}

// NewCancelTaskTool creates a new cancel task tool
func NewCancelTaskTool(k8sClient client.Client) *CancelTaskTool {
	return &CancelTaskTool{
		k8sClient: k8sClient,
	}
}

// Name returns the tool name
func (t *CancelTaskTool) Name() string {
	return cancelTaskToolName
}

// Description returns the tool description
func (t *CancelTaskTool) Description() string {
	return "Cancel a running child task. The task's Job will be terminated and the task will be marked as Cancelled. " +
		"Use this to stop a task that is going down the wrong path, taking too long, or is no longer needed. " +
		"You can only cancel tasks that are children of the current task."
}

// Parameters returns the JSON Schema for the tool parameters
func (t *CancelTaskTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"task_name": {
				"type": "string",
				"description": "Name of the child task to cancel"
			},
			"namespace": {
				"type": "string",
				"description": "Namespace of the task (defaults to current namespace)"
			},
			"reason": {
				"type": "string",
				"description": "Reason for cancellation (optional, included in task status message)"
			}
		},
		"required": ["task_name"]
	}`)
}

// Execute cancels a running child task
func (t *CancelTaskTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a CancelTaskArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("failed to parse arguments: %w", err)
	}

	if a.TaskName == "" {
		return "", fmt.Errorf("task_name is required")
	}

	parentTaskName := os.Getenv(envOrkaTaskName)

	namespace := a.Namespace
	if namespace == "" {
		namespace = os.Getenv(envOrkaTaskNamespace)
	}
	if namespace == "" {
		namespace = defaultNamespace
	}

	// Get the target task
	task := &corev1alpha1.Task{}
	if err := t.k8sClient.Get(ctx, types.NamespacedName{
		Name:      a.TaskName,
		Namespace: namespace,
	}, task); err != nil {
		return "", fmt.Errorf("failed to get task %q: %w", a.TaskName, err)
	}

	// Verify this is a child of the current task (only when running inside a task context)
	if parentTaskName != "" {
		parentLabel := labels.ParentTaskName(task.Labels, task.Annotations)
		if parentLabel != parentTaskName {
			return "", fmt.Errorf("task %q is not a child of the current task %q", a.TaskName, parentTaskName)
		}
	}

	// Can only cancel Pending or Running tasks
	if task.Status.Phase != corev1alpha1.TaskPhasePending && task.Status.Phase != corev1alpha1.TaskPhaseRunning {
		result := CancelTaskResult{
			TaskName: a.TaskName,
			Status:   fmt.Sprintf("task is already in %s phase, cannot cancel", task.Status.Phase),
		}
		data, _ := json.Marshal(result)
		return string(data), nil
	}

	// Set the task phase to Cancelled
	now := metav1.Now()
	task.Status.Phase = corev1alpha1.TaskPhaseCancelled
	task.Status.CompletionTime = &now
	message := "cancelled by parent task"
	if a.Reason != "" {
		message = fmt.Sprintf("cancelled by parent task: %s", a.Reason)
	}
	task.Status.Message = message

	if err := t.k8sClient.Status().Update(ctx, task); err != nil {
		return "", fmt.Errorf("failed to cancel task %q: %w", a.TaskName, err)
	}

	result := CancelTaskResult{
		TaskName: a.TaskName,
		Status:   cancelledStatusString,
	}
	data, _ := json.Marshal(result)
	return string(data), nil
}
