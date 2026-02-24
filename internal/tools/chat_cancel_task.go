/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
)

// ChatCancelTaskTool cancels and deletes a task (chat version).
type ChatCancelTaskTool struct{}

func (t *ChatCancelTaskTool) Name() string { return "cancel_task" }

func (t *ChatCancelTaskTool) Description() string {
	return "Cancel and delete a task. Use when a task is stuck, no longer needed, or the user requests cancellation."
}

func (t *ChatCancelTaskTool) Parameters() json.RawMessage {
	return mustMarshalSchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":      map[string]any{"type": "string", "description": "Task name"},
			"namespace": map[string]any{"type": "string", "description": "Namespace"},
		},
		"required": []string{"name"},
	})
}

func (t *ChatCancelTaskTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	tc := GetToolContext(ctx)
	if tc == nil {
		return ChatToolErrorResult("internal_error", "missing tool context", "")
	}

	var a map[string]any
	if err := json.Unmarshal(args, &a); err != nil {
		return ChatToolErrorResult("invalid_arguments", fmt.Sprintf("failed to parse arguments: %v", err), "Ensure arguments are valid JSON")
	}

	name := chatGetStringArg(a, "name")
	if name == "" {
		return ChatToolErrorResult("invalid_arguments", "name is required", "Provide the task name")
	}
	namespace := chatGetStringArgDefault(a, "namespace", tc.Namespace)

	task := &corev1alpha1.Task{}
	if err := tc.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, task); err != nil {
		return classifyChatK8sErr(err)
	}

	if err := tc.Client.Delete(ctx, task); err != nil {
		return classifyChatK8sErr(err)
	}

	return ChatToolSuccess(map[string]any{
		"name":    task.Name,
		"message": "Task cancelled and deleted",
	})
}
