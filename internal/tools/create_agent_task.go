/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/tracing"
)

// CreateAgentTaskTool creates an agent-runtime Task CR.
type CreateAgentTaskTool struct{}

func (t *CreateAgentTaskTool) Name() string { return createAgentTaskToolName }

func (t *CreateAgentTaskTool) Description() string {
	return "Create a task using an external CLI runtime (Copilot, Claude Code, Codex, OpenCode) for code changes in a git repo. Do NOT use for simple container commands or direct LLM reasoning."
}

func (t *CreateAgentTaskTool) Parameters() json.RawMessage {
	return mustMarshalSchema(map[string]any{jsonSchemaTypeField: jsonSchemaTypeObject, jsonSchemaPropertiesField: map[string]any{nameField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: taskNameDescription}, promptField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "The prompt/instruction for the agent"}, agentRefField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Agent name with runtime configured"}, namespaceField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: namespaceDescription}, timeoutField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: timeoutDescription}, "maxTurns": map[string]any{jsonSchemaTypeField: jsonSchemaTypeInteger, jsonSchemaDescriptionField: "Maximum agent loop iterations"}, workspaceField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeObject, jsonSchemaPropertiesField: map[string]any{
		"intent":                       map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaEnumField: []string{"read", "write"}, jsonSchemaDescriptionField: "Workspace intent. Defaults to read; publication fields require write."},
		"gitRepo":                      map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Source Git repository URL"},
		"branch":                       map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Source branch to clone from (must exist). Omit with ref to resolve and freeze the repository's advertised default branch."},
		"ref":                          map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Exact source commit, tag, or ref"},
		"readCredentialRef":            map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Optional Secret name for clone/read credentials. Omit to auto-discover a read credential when available."},
		"publicationGitRepo":           map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Publication repository URL for write Tasks"},
		"publicationReadCredentialRef": map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Optional Secret name for target-repository preflight and verification credentials"},
		"publicationCredentialRef":     map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Secret name for target-repository write credentials. Required for write intent."},
		"forgeCredentialRef":           map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Optional Secret name for forge API credentials used to reconcile pull requests"},
		"pushBranch":                   map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Publication branch name"},
		"prBaseBranch":                 map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Pull request base branch"},
		"createPR":                     map[string]any{jsonSchemaTypeField: jsonSchemaTypeBoolean, jsonSchemaDescriptionField: "Reconcile a pull request after publication"},
		"subPath":                      map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Sub-path within the repo"},
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

	if turns, errResult, ok := parseMaxTurnsArg(a); !ok {
		return errResult, nil
	} else if turns != nil {
		agentRuntime = &corev1alpha1.AgentRuntimeSpec{MaxTurns: turns}
	}

	if ws, ok := a[workspaceField]; ok {
		wsMap, ok := ws.(map[string]any)
		if !ok {
			return ChatToolErrorResult("invalid_arguments", "workspace must be an object", "Provide workspace as a JSON object or omit it")
		}
		wsCfg := &corev1alpha1.WorkspaceConfig{Intent: corev1alpha1.WorkspaceIntentRead}
		if intent := strings.ToLower(strings.TrimSpace(chatGetStringArg(wsMap, "intent"))); intent != "" {
			switch corev1alpha1.WorkspaceIntent(intent) {
			case corev1alpha1.WorkspaceIntentRead, corev1alpha1.WorkspaceIntentWrite:
				wsCfg.Intent = corev1alpha1.WorkspaceIntent(intent)
			default:
				return ChatToolErrorResult("invalid_arguments", "workspace.intent must be read or write", "Use read for inspection or write for publication")
			}
		}
		wsCfg.GitRepo = chatGetStringArg(wsMap, "gitRepo")
		wsCfg.Branch = chatGetStringArg(wsMap, "branch")
		wsCfg.Ref = chatGetStringArg(wsMap, "ref")
		wsCfg.SubPath = chatGetStringArg(wsMap, "subPath")
		wsCfg.PublicationGitRepo = chatGetStringArg(wsMap, "publicationGitRepo")
		wsCfg.PushBranch = chatGetStringArg(wsMap, "pushBranch")
		wsCfg.PRBaseBranch = chatGetStringArg(wsMap, "prBaseBranch")
		createPR, errResult, ok := parseCreatePRArg(wsMap)
		if !ok {
			return errResult, nil
		}
		wsCfg.CreatePR = createPR
		if workspaceRequestsPublication(wsCfg) {
			wsCfg.Intent = corev1alpha1.WorkspaceIntentWrite
		}
		publicationCredential := strings.TrimSpace(chatGetStringArg(wsMap, "publicationCredentialRef"))
		if wsCfg.Intent == corev1alpha1.WorkspaceIntentWrite && publicationCredential == "" {
			return ChatToolErrorResult("invalid_arguments", "workspace.publicationCredentialRef is required for write intent", "Provide a dedicated target-repository write credential")
		}
		agent, err := loadAgent(ctx, tc.Client, namespace, agentRef)
		if err != nil {
			result, _ := ChatToolErrorResult(internalErrorType, err.Error(), "")
			return result, nil
		}
		readRef, err := resolveWorkspaceCredentialRef(ctx, tc.Client, namespace, agent, chatGetStringArg(wsMap, "readCredentialRef"))
		if err != nil {
			result, _ := ChatToolErrorResult(internalErrorType, err.Error(), "")
			return result, nil
		}
		wsCfg.ReadCredentialRef = readRef
		if publicationReadCredential := strings.TrimSpace(chatGetStringArg(wsMap, "publicationReadCredentialRef")); publicationReadCredential != "" {
			wsCfg.PublicationReadCredentialRef = &corev1alpha1.WorkspaceCredentialReference{Name: publicationReadCredential}
		}
		if publicationCredential != "" {
			wsCfg.PublicationCredentialRef = &corev1alpha1.WorkspaceCredentialReference{Name: publicationCredential}
		}
		if forgeCredential := strings.TrimSpace(chatGetStringArg(wsMap, "forgeCredentialRef")); forgeCredential != "" {
			wsCfg.ForgeCredentialRef = &corev1alpha1.WorkspaceCredentialReference{Name: forgeCredential}
		}
		task.Spec.Workspace = wsCfg
	}

	task.Spec.AgentRuntime = agentRuntime

	schedule := chatGetStringArg(a, scheduleField)
	if schedule != "" {
		task.Spec.Schedule = schedule
	}

	if result, ok := authorizeTaskCreate(ctx, tc, task); !ok {
		return result, nil
	}
	tracing.StampTaskTraceContext(ctx, task)
	if err := tc.Client.Create(ctx, task); err != nil {
		return classifyChatK8sErr(err)
	}

	tc.IncrementTasks()
	return ChatToolSuccess(map[string]any{nameField: task.Name, namespaceField: task.Namespace, phaseField: taskPhasePendingString, messageField: taskCreatedMsg(schedule)})
}
