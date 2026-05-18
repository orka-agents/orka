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

// CreateAgentTaskTool creates an agent-runtime Task CR.
type CreateAgentTaskTool struct{}

func (t *CreateAgentTaskTool) Name() string { return createAgentTaskToolName }

func (t *CreateAgentTaskTool) Description() string {
	return "Create a task using an external CLI runtime (Copilot, Claude Code, Codex) for code changes in a git repo. Do NOT use for simple container commands or direct LLM reasoning."
}

func (t *CreateAgentTaskTool) Parameters() json.RawMessage {
	return mustMarshalSchema(map[string]any{jsonSchemaTypeField: jsonSchemaTypeObject, jsonSchemaPropertiesField: map[string]any{nameField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: taskNameDescription}, promptField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "The prompt/instruction for the agent"}, agentRefField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Agent name with runtime configured"}, namespaceField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: namespaceDescription}, timeoutField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: timeoutDescription}, "maxTurns": map[string]any{jsonSchemaTypeField: jsonSchemaTypeInteger, jsonSchemaDescriptionField: "Maximum agent loop iterations"}, workspaceField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeObject, jsonSchemaPropertiesField: map[string]any{
		"gitRepo":      map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Git repository URL"},
		"branch":       map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Git branch to clone from (must exist). Omit to use the default branch."},
		"pushBranch":   map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Branch name to push changes to (will be created if it doesn't exist). Use this for new feature branches."},
		"gitSecretRef": map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Optional secret name containing git credentials. Omit to auto-discover git credentials or reuse the Copilot agent secret when available."},
		"subPath":      map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Sub-path within the repo"},
	},
	}, scheduleField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: cronScheduleDescription},
	}, jsonSchemaRequiredField: []string{nameField, promptField, agentRefField},
	})
}

func (t *CreateAgentTaskTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	tc := GetToolContext(ctx)
	if tc == nil {
		return ChatToolErrorResult(internalErrorType, "missing tool context", "")
	}

	var a map[string]any
	if err := json.Unmarshal(args, &a); err != nil {
		return ChatToolErrorResult("invalid_arguments", fmt.Sprintf("failed to parse arguments: %v", err), "Ensure arguments are valid JSON")
	}

	if limitErr := tc.CheckTaskLimit(); limitErr != nil {
		return ChatToolErrorResult(limitErr.Type, limitErr.Message, limitErr.Suggestion)
	}

	prompt := chatGetStringArg(a, promptField)
	if prompt == "" {
		return ChatToolErrorResult("invalid_arguments", "prompt is required", "Provide a prompt for the agent task")
	}

	agentRef := chatGetStringArg(a, agentRefField)
	if agentRef == "" {
		return ChatToolErrorResult("invalid_arguments", "agentRef is required", "Provide an agent reference for the agent task")
	}

	namespace := chatGetStringArgDefault(a, namespaceField, tc.Namespace)
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

	if d, errResult, ok := parseTimeoutArg(a); !ok {
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

	if ws, ok := a[workspaceField]; ok {
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
			agent, err := loadAgent(ctx, tc.Client, namespace, agentRef)
			if err != nil {
				result, _ := ChatToolErrorResult(internalErrorType, err.Error(), "")
				return result, nil
			}
			secretRef, err := resolveWorkspaceGitSecretRef(ctx, tc.Client, namespace, agent, chatGetStringArg(wsMap, "gitSecretRef"))
			if err != nil {
				result, _ := ChatToolErrorResult(internalErrorType, err.Error(), "")
				return result, nil
			}
			wsCfg.GitSecretRef = secretRef
			agentRuntime.Workspace = wsCfg
		}
	}

	task.Spec.AgentRuntime = agentRuntime

	schedule := chatGetStringArg(a, scheduleField)
	if schedule != "" {
		task.Spec.Schedule = schedule
	}

	if result, ok := authorizeTaskCreate(ctx, tc, task); !ok {
		return result, nil
	}
	if err := tc.Client.Create(ctx, task); err != nil {
		return classifyChatK8sErr(err)
	}

	tc.IncrementTasks()
	return ChatToolSuccess(map[string]any{nameField: task.Name, namespaceField: task.Namespace, phaseField: taskPhasePendingString, messageField: taskCreatedMsg(schedule)})
}
