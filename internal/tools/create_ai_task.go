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

// CreateAITaskTool creates an AI-type Task CR.
type CreateAITaskTool struct{}

func (t *CreateAITaskTool) Name() string { return "create_ai_task" }

func (t *CreateAITaskTool) Description() string {
	return "Create an AI/LLM-powered task. Use when the user needs LLM reasoning, code review, content generation, or analysis. Do NOT use for running shell commands or CLI runtimes."
}

func (t *CreateAITaskTool) Parameters() json.RawMessage {
	return mustMarshalSchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":        map[string]any{"type": "string", "description": "Task name"},
			"prompt":      map[string]any{"type": "string", "description": "The prompt/instruction for the AI task"},
			"agentRef":    map[string]any{"type": "string", "description": "Optional Agent name to use"},
			"providerRef": map[string]any{"type": "string", "description": "Optional Provider CRD reference name. Omit to let the controller resolve the task from the referenced Agent or model settings."},
			"namespace":   map[string]any{"type": "string", "description": "Namespace"},
			"timeout":     map[string]any{"type": "string", "description": "Timeout duration, e.g. \"5m\""},
			"priority":    map[string]any{"type": "integer", "description": "Priority 0-1000"},
			"sessionRef":  map[string]any{"type": "string", "description": "Session name for conversation continuity; creates the session if missing and appends the transcript on completion"},
			"schedule":    map[string]any{"type": "string", "description": "Cron schedule for recurring tasks (e.g., '0 */6 * * *' for every 6 hours, '0 9 * * 1-5' for weekdays at 9am, '*/5 * * * *' for every 5 minutes). Leave empty for one-time tasks."},
		},
		"required": []string{"name", "prompt"},
	})
}

func (t *CreateAITaskTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
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

	prompt := chatGetStringArg(a, "prompt")
	if prompt == "" {
		return ChatToolErrorResult("invalid_arguments", "prompt is required", "Provide a prompt for the AI task")
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
			Type:   corev1alpha1.TaskTypeAI,
			Prompt: prompt,
		},
	}

	if agentRef := chatGetStringArg(a, "agentRef"); agentRef != "" {
		task.Spec.AgentRef = &corev1alpha1.AgentReference{Name: agentRef}
	}

	if providerName := chatGetStringArg(a, "providerRef"); providerName != "" {
		if task.Spec.AI == nil {
			task.Spec.AI = &corev1alpha1.AISpec{}
		}
		task.Spec.AI.ProviderRef = &corev1alpha1.ProviderReference{Name: providerName}
	}

	if d, errResult, ok := parseTimeoutArg(a); !ok {
		return errResult, nil
	} else if d > 0 {
		task.Spec.Timeout = &metav1.Duration{Duration: d}
	}

	if _, ok := a["priority"]; ok {
		p := int32(chatGetIntArg(a, "priority", 500))
		task.Spec.Priority = &p
	}

	if sessionRef := chatGetStringArg(a, "sessionRef"); sessionRef != "" {
		task.Spec.SessionRef = &corev1alpha1.SessionReference{
			Name:   sessionRef,
			Create: true,
			Append: true,
		}
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

func mustMarshalSchema(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
