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

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
)

// CheckTaskProgressTool gets the current phase, duration, and status conditions of a task.
type CheckTaskProgressTool struct{}

func (t *CheckTaskProgressTool) Name() string { return "check_task_progress" }

func (t *CheckTaskProgressTool) Description() string {
	return "Get the current phase, duration, and status conditions of a task. Do NOT use to get the output/result — use fetch_task_output for that."
}

func (t *CheckTaskProgressTool) Parameters() json.RawMessage {
	return mustMarshalSchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":      map[string]any{"type": "string", "description": "Task name"},
			"namespace": map[string]any{"type": "string", "description": "Namespace"},
		},
		"required": []string{"name"},
	})
}

func (t *CheckTaskProgressTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
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

	data := map[string]any{
		"name":      task.Name,
		"namespace": task.Namespace,
		"phase":     string(task.Status.Phase),
		"message":   task.Status.Message,
	}

	if task.Status.StartTime != nil {
		duration := time.Since(task.Status.StartTime.Time)
		data["duration"] = duration.Round(time.Second).String()
	}

	if len(task.Status.Conditions) > 0 {
		conditions := make([]map[string]string, 0, len(task.Status.Conditions))
		for _, c := range task.Status.Conditions {
			conditions = append(conditions, map[string]string{
				"type":    c.Type,
				"status":  string(c.Status),
				"reason":  c.Reason,
				"message": c.Message,
			})
		}
		data["conditions"] = conditions
	}

	return ChatToolSuccess(data)
}
