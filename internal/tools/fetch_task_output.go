/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/store"
)

// FetchTaskOutputTool fetches the result/output of a completed task.
type FetchTaskOutputTool struct{}

func (t *FetchTaskOutputTool) Name() string { return "fetch_task_output" }

func (t *FetchTaskOutputTool) Description() string {
	return "Get the result/output of a completed task from its ConfigMap. Returns the task result truncated to 2K characters if large. Do NOT use to check if a task is still running — use check_task_progress for that."
}

func (t *FetchTaskOutputTool) Parameters() json.RawMessage {
	return mustMarshalSchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":      map[string]any{"type": "string", "description": "Task name"},
			"namespace": map[string]any{"type": "string", "description": "Namespace"},
		},
		"required": []string{"name"},
	})
}

func (t *FetchTaskOutputTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
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

	if task.Status.ResultRef == nil || !task.Status.ResultRef.Available {
		return ChatToolErrorResult("not_found", "task has no result yet", "Wait for the task to complete, then try again")
	}

	if tc.ResultStore == nil {
		return ChatToolErrorResult("internal_error", "result store not configured", "")
	}

	data, err := tc.ResultStore.GetResult(ctx, namespace, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ChatToolErrorResult("not_found", "result not found in store", "The result may have been deleted")
		}
		return ChatToolErrorResult("internal_error", fmt.Sprintf("failed to get result: %v", err), "")
	}

	result := string(data)
	const maxLen = 2048
	if len(result) > maxLen {
		result = result[:maxLen] + fmt.Sprintf(" [truncated, full output: %d chars]", len(data))
	}

	return ChatToolSuccess(map[string]any{
		"name":   task.Name,
		"phase":  string(task.Status.Phase),
		"output": result,
	})
}
