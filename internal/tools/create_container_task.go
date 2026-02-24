/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
)

// CreateContainerTaskTool creates a container-type Task CR.
type CreateContainerTaskTool struct{}

func (t *CreateContainerTaskTool) Name() string { return "create_container_task" }

func (t *CreateContainerTaskTool) Description() string {
	return "Create a container task to run commands. Use when the user needs to run a shell command, build code, or execute a container image. Do NOT use for LLM reasoning."
}

func (t *CreateContainerTaskTool) Parameters() json.RawMessage {
	return mustMarshalSchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":      map[string]any{"type": "string", "description": "Task name"},
			"image":     map[string]any{"type": "string", "description": "Container image to run. Leave empty to use the default worker image which includes common tools (kubectl, sh) and writes results to a ConfigMap. Only set a custom image if you need a specific runtime not in the default worker."},
			"command":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Command to execute"},
			"args":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Arguments to the command"},
			"namespace": map[string]any{"type": "string", "description": "Namespace"},
			"timeout":   map[string]any{"type": "string", "description": "Timeout duration, e.g. \"5m\""},
			"priority":  map[string]any{"type": "integer", "description": "Priority 0-1000"},
			"schedule":  map[string]any{"type": "string", "description": "Cron schedule for recurring tasks (e.g., '0 */6 * * *' for every 6 hours, '0 9 * * 1-5' for weekdays at 9am, '*/5 * * * *' for every 5 minutes). Leave empty for one-time tasks."},
		},
		"required": []string{"name"},
	})
}

func (t *CreateContainerTaskTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	tc := GetToolContext(ctx)
	if tc == nil {
		return ChatToolErrorResult("internal_error", "missing tool context", "")
	}

	var a map[string]any
	if err := json.Unmarshal(args, &a); err != nil {
		return ChatToolErrorResult("invalid_arguments", fmt.Sprintf("failed to parse arguments: %v", err), "Ensure arguments are valid JSON")
	}

	if limitErr := tc.CheckTaskLimit(); limitErr != nil {
		return ChatToolErrorResult(limitErr.Type, limitErr.Message, limitErr.Suggestion)
	}

	namespace := chatGetStringArgDefault(a, "namespace", tc.Namespace)
	if r, ok := checkChatNamespaceScope(tc, namespace); !ok {
		return r, nil
	}

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tc.GenerateTaskName(),
			Namespace: namespace,
			Labels:    tc.TaskLabels(),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:    corev1alpha1.TaskTypeContainer,
			Image:   chatGetStringArg(a, "image"),
			Command: chatGetStringSliceArg(a, "command"),
			Args:    chatGetStringSliceArg(a, "args"),
		},
	}

	if d, errResult, ok := parseDurationArg(a, "timeout"); !ok {
		return errResult, nil
	} else if d > 0 {
		task.Spec.Timeout = &metav1.Duration{Duration: d}
	}

	if _, ok := a["priority"]; ok {
		p := int32(chatGetIntArg(a, "priority", 500))
		task.Spec.Priority = &p
	}

	schedule := chatGetStringArg(a, "schedule")
	if schedule != "" {
		task.Spec.Schedule = schedule
	}

	if err := tc.Client.Create(ctx, task); err != nil {
		return classifyChatK8sErr(err)
	}

	tc.IncrementTasks()
	return ChatToolSuccess(map[string]any{
		"name":      task.Name,
		"namespace": task.Namespace,
		"phase":     "Pending",
		"message":   taskCreatedMsg(schedule),
	})
}
