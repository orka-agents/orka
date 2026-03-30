/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
)

// CreateAgentTaskTool creates an agent-runtime Task CR.
type CreateAgentTaskTool struct{}

func (t *CreateAgentTaskTool) Name() string { return "create_agent_task" }

func (t *CreateAgentTaskTool) Description() string {
	return "Create a task using an external CLI runtime (Copilot, Claude Code) for code changes in a git repo. Do NOT use for simple container commands or direct LLM reasoning."
}

func (t *CreateAgentTaskTool) Parameters() json.RawMessage {
	return mustMarshalSchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":      map[string]any{"type": "string", "description": "Task name"},
			"prompt":    map[string]any{"type": "string", "description": "The prompt/instruction for the agent"},
			"agentRef":  map[string]any{"type": "string", "description": "Agent name with runtime configured"},
			"namespace": map[string]any{"type": "string", "description": "Namespace"},
			"timeout":   map[string]any{"type": "string", "description": "Timeout duration, e.g. \"5m\""},
			"maxTurns":  map[string]any{"type": "integer", "description": "Maximum agent loop iterations"},
			"workspace": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"gitRepo":      map[string]any{"type": "string", "description": "Git repository URL"},
					"branch":       map[string]any{"type": "string", "description": "Git branch to clone from (must exist). Omit to use the default branch."},
					"pushBranch":   map[string]any{"type": "string", "description": "Branch name to push changes to (will be created if it doesn't exist). Use this for new feature branches."},
					"gitSecretRef": map[string]any{"type": "string", "description": "Secret name containing git credentials. Explicit only; no automatic discovery is performed."},
					"subPath":      map[string]any{"type": "string", "description": "Sub-path within the repo"},
				},
			},
			"schedule": map[string]any{"type": "string", "description": "Cron schedule for recurring tasks (e.g., '0 */6 * * *' for every 6 hours, '0 9 * * 1-5' for weekdays at 9am, '*/5 * * * *' for every 5 minutes). Leave empty for one-time tasks."},
		},
		"required": []string{"name", "prompt", "agentRef"},
	})
}

func (t *CreateAgentTaskTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
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
		return ChatToolErrorResult("invalid_arguments", "prompt is required", "Provide a prompt for the agent task")
	}

	agentRef := chatGetStringArg(a, "agentRef")
	if agentRef == "" {
		return ChatToolErrorResult("invalid_arguments", "agentRef is required", "Provide an agent reference for the agent task")
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
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: prompt,
			AgentRef: &corev1alpha1.AgentReference{
				Name: agentRef,
			},
		},
	}

	if d, errResult, ok := parseDurationArg(a, "timeout"); !ok {
		return errResult, nil
	} else if d > 0 {
		task.Spec.Timeout = &metav1.Duration{Duration: d}
	}

	var agentRuntime *corev1alpha1.AgentRuntimeSpec

	if maxTurns, ok := a["maxTurns"]; ok {
		if agentRuntime == nil {
			agentRuntime = &corev1alpha1.AgentRuntimeSpec{}
		}
		mt := int32(maxTurns.(float64))
		agentRuntime.MaxTurns = &mt
	}

	if ws, ok := a["workspace"]; ok {
		if wsMap, ok := ws.(map[string]any); ok {
			if agentRuntime == nil {
				agentRuntime = &corev1alpha1.AgentRuntimeSpec{}
			}
			wsCfg := &corev1alpha1.WorkspaceConfig{}
			if gitRepo := chatGetStringArg(wsMap, "gitRepo"); gitRepo != "" {
				wsCfg.GitRepo = gitRepo
			}
			if branch := chatGetStringArg(wsMap, "branch"); branch != "" {
				wsCfg.Branch = branch
			}
			if subPath := chatGetStringArg(wsMap, "subPath"); subPath != "" {
				wsCfg.SubPath = subPath
			}
			if pushBranch := chatGetStringArg(wsMap, "pushBranch"); pushBranch != "" {
				wsCfg.PushBranch = pushBranch
			}
			if gitSecretRef := chatGetStringArg(wsMap, "gitSecretRef"); gitSecretRef != "" {
				wsCfg.GitSecretRef = &corev1.LocalObjectReference{Name: gitSecretRef}
			}
			agentRuntime.Workspace = wsCfg
		}
	}

	task.Spec.AgentRuntime = agentRuntime

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
