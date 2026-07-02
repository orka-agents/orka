/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// WaitForTaskTool waits for a single task to complete.
type WaitForTaskTool struct{}

func (t *WaitForTaskTool) Name() string { return waitForTaskToolName }

func (t *WaitForTaskTool) Description() string {
	return "Wait for a task to complete. Each call waits up to the specified timeout. If the task isn't done, you can call again or do other work. Use after creating a task."
}

func (t *WaitForTaskTool) Parameters() json.RawMessage {
	return mustMarshalSchema(map[string]any{jsonSchemaTypeField: jsonSchemaTypeObject, jsonSchemaPropertiesField: map[string]any{nameField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: taskNameDescription}, namespaceField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: namespaceDescription}, timeoutField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeInteger, jsonSchemaDescriptionField: "Seconds to wait (max 60, default 30)"}}, jsonSchemaRequiredField: []string{nameField}})
}

func (t *WaitForTaskTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	tc := GetToolContext(ctx)
	if tc == nil {
		return ChatToolErrorResult(internalErrorType, "missing tool context", "")
	}

	var a map[string]any
	if err := json.Unmarshal(args, &a); err != nil {
		return ChatToolErrorResult("invalid_arguments", fmt.Sprintf("failed to parse arguments: %v", err), "Ensure arguments are valid JSON")
	}

	name := chatGetStringArg(a, nameField)
	if name == "" {
		return ChatToolErrorResult("invalid_arguments", "name is required", "Provide the task name")
	}
	namespace := chatGetStringArgDefault(a, namespaceField, tc.Namespace)
	timeout := min(chatGetIntArg(a, timeoutField, 30), 60)

	task := &corev1alpha1.Task{}
	if err := tc.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, task); err != nil {
		return classifyChatK8sErr(err)
	}

	if task.Status.Phase == corev1alpha1.TaskPhaseSucceeded || task.Status.Phase == corev1alpha1.TaskPhaseFailed {
		return chatTaskStatusResult(task)
	}

	deadline := time.Now().Add(time.Duration(timeout) * time.Second)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return chatTaskTimeoutResult(task)
		case <-ticker.C:
			if time.Now().After(deadline) {
				_ = tc.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, task)
				return chatTaskTimeoutResult(task)
			}

			if err := tc.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, task); err != nil {
				return classifyChatK8sErr(err)
			}

			if task.Status.Phase == corev1alpha1.TaskPhaseSucceeded || task.Status.Phase == corev1alpha1.TaskPhaseFailed {
				return chatTaskStatusResult(task)
			}
		}
	}
}

func chatTaskStatusResult(task *corev1alpha1.Task) (string, error) {
	data := map[string]any{nameField: task.Name, phaseField: string(task.Status.Phase), messageField: task.Status.Message}
	if task.Status.StartTime != nil {
		elapsed := time.Since(task.Status.StartTime.Time)
		if task.Status.CompletionTime != nil {
			elapsed = task.Status.CompletionTime.Sub(task.Status.StartTime.Time)
		}
		data["elapsed"] = elapsed.Round(time.Second).String()
	}
	return ChatToolSuccess(data)
}

func chatTaskTimeoutResult(task *corev1alpha1.Task) (string, error) {
	data := map[string]any{nameField: task.Name, phaseField: string(task.Status.Phase), messageField: "Task is still running. Call wait_for_task again to continue waiting, or do other work in the meantime."}
	if task.Status.StartTime != nil {
		data["elapsed"] = time.Since(task.Status.StartTime.Time).Round(time.Second).String()
	}
	return ChatToolSuccess(data)
}
