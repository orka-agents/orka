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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/labels"
)

// ChatCreateAgentTool creates an Agent CRD from the chat context.
type ChatCreateAgentTool struct{}

func (t *ChatCreateAgentTool) Name() string { return "create_agent" }

func (t *ChatCreateAgentTool) Description() string {
	return "Create an agent with model, tools, and optional runtime and coordination configuration. Enable coordination to allow this agent to delegate tasks to other agents. Provide initialPrompt to also create and start a Task immediately."
}

func (t *ChatCreateAgentTool) Parameters() json.RawMessage {
	return mustMarshalSchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":         map[string]any{"type": "string", "description": "Agent name"},
			"namespace":    map[string]any{"type": "string", "description": "Namespace"},
			"providerRef":  map[string]any{"type": "string", "description": "Provider name (defaults to 'default')"},
			"systemPrompt": map[string]any{"type": "string", "description": "System prompt for the agent"},
			"tools":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Tool names to attach"},
			"model": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"provider":    map[string]any{"type": "string", "description": "Model provider (e.g. anthropic, openai)"},
					"name":        map[string]any{"type": "string", "description": "Model name"},
					"temperature": map[string]any{"type": "number", "description": "Sampling temperature"},
				},
			},
			"runtime": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"type":            map[string]any{"type": "string", "description": "Runtime type: copilot, claude, or codex"},
					"defaultMaxTurns": map[string]any{"type": "integer", "description": "Default max agent loop iterations"},
					"secretRef":       map[string]any{"type": "string", "description": "Optional secret name containing runtime credentials. Omit to auto-discover the standard secret for this runtime."},
				},
			},
			"initialPrompt": map[string]any{
				"type":        "string",
				"description": "When provided, automatically create and start a Task using this agent with this prompt. One tool call = agent + task. Leave empty to only create the agent config without running it.",
			},
			"coordination": map[string]any{
				"type":        "object",
				"description": "Enable multi-agent coordination so this agent can delegate tasks to other agents via delegate_task/wait_for_tasks tools",
				"properties": map[string]any{
					"enabled":               map[string]any{"type": "boolean", "description": "Enable coordination (delegate_task/wait_for_tasks tools)"},
					"maxConcurrentChildren": map[string]any{"type": "integer", "description": "Max concurrent child tasks (default 5)"},
					"maxDepth":              map[string]any{"type": "integer", "description": "Max delegation depth (default 3)"},
					"allowedAgents": map[string]any{
						"type":        "array",
						"description": "List of agent names this agent can delegate to. If empty, can delegate to any agent.",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"name":      map[string]any{"type": "string", "description": "Agent name"},
								"namespace": map[string]any{"type": "string", "description": "Agent namespace (defaults to same namespace)"},
							},
							"required": []string{"name"},
						},
					},
				},
			},
		},
		"required": []string{"name"},
	})
}

func (t *ChatCreateAgentTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
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
		return ChatToolErrorResult("invalid_arguments", "name is required", "Provide a name for the agent")
	}

	namespace := chatGetStringArgDefault(a, "namespace", tc.Namespace)
	if r, ok := checkChatNamespaceScope(tc, namespace); !ok {
		return r, nil
	}

	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				labels.LabelCreatedBy: "chat",
			},
		},
		Spec: corev1alpha1.AgentSpec{
			TTLAfterLastTask: &metav1.Duration{Duration: time.Hour},
		},
	}

	// Model configuration
	if modelObj, ok := a["model"]; ok {
		switch m := modelObj.(type) {
		case map[string]any:
			agent.Spec.Model = &corev1alpha1.ModelConfig{
				Name:     chatGetStringArg(m, "name"),
				Provider: chatGetStringArg(m, "provider"),
			}
			if temp, ok := m["temperature"]; ok {
				if tempF, ok := temp.(float64); ok {
					agent.Spec.Model.Temperature = &tempF
				}
			}
		case string:
			provider, modelName := splitModelString(m)
			if provider != "" {
				agent.Spec.Model = &corev1alpha1.ModelConfig{Provider: provider, Name: modelName}
			} else {
				agent.Spec.Model = &corev1alpha1.ModelConfig{Name: modelName}
			}
		}
	}

	// Set providerRef
	providerRefName := chatGetStringArgDefault(a, "providerRef", "default")
	agent.Spec.ProviderRef = &corev1alpha1.ProviderReference{Name: providerRefName}

	if agent.Spec.Model != nil && agent.Spec.Model.Provider != "" {
		agent.Spec.Model.Provider = ""
	}

	if systemPrompt := chatGetStringArg(a, "systemPrompt"); systemPrompt != "" {
		agent.Spec.SystemPrompt = &corev1alpha1.PromptSource{
			Inline: systemPrompt,
		}
	}

	if toolNames := chatGetStringSliceArg(a, "tools"); len(toolNames) > 0 {
		for _, tn := range toolNames {
			agent.Spec.Tools = append(agent.Spec.Tools, corev1alpha1.ToolReference{Name: tn})
		}
	}

	if errResult, ok := parseRuntimeConfig(ctx, tc.Client, namespace, a, agent); !ok {
		return errResult, nil
	}
	parseCoordinationConfig(a, agent)

	if err := tc.Client.Create(ctx, agent); err != nil {
		return classifyChatK8sErr(err)
	}

	// Handle initialPrompt: create a task for the agent
	if initialPrompt := chatGetStringArg(a, "initialPrompt"); initialPrompt != "" {
		return t.handleInitialPrompt(ctx, tc, agent, namespace, initialPrompt)
	}

	return ChatToolSuccess(map[string]any{
		"name":      agent.Name,
		"namespace": agent.Namespace,
		"message":   "Agent created",
	})
}

func (t *ChatCreateAgentTool) handleInitialPrompt(ctx context.Context, tc *ToolContext, agent *corev1alpha1.Agent, namespace, initialPrompt string) (string, error) {
	if limitErr := tc.CheckTaskLimit(); limitErr != nil {
		return ChatToolSuccess(map[string]any{
			"name":      agent.Name,
			"namespace": agent.Namespace,
			"message":   "Agent created, but task creation skipped: task limit reached",
		})
	}

	taskType := corev1alpha1.TaskTypeAI
	if agent.Spec.Runtime != nil {
		taskType = corev1alpha1.TaskTypeAgent
	}

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tc.GenerateTaskName(),
			Namespace: namespace,
			Labels:    tc.TaskLabels(),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   taskType,
			Prompt: initialPrompt,
			AgentRef: &corev1alpha1.AgentReference{
				Name: agent.Name,
			},
		},
	}

	if agent.Spec.ProviderRef != nil && taskType == corev1alpha1.TaskTypeAI {
		if task.Spec.AI == nil {
			task.Spec.AI = &corev1alpha1.AISpec{}
		}
		task.Spec.AI.ProviderRef = agent.Spec.ProviderRef
	}

	if err := tc.Client.Create(ctx, task); err != nil {
		return ChatToolSuccess(map[string]any{
			"agentName":      agent.Name,
			"agentNamespace": agent.Namespace,
			"message":        fmt.Sprintf("Agent created, but task creation failed: %v", err),
		})
	}

	tc.IncrementTasks()
	return ChatToolSuccess(map[string]any{
		"agentName":      agent.Name,
		"agentNamespace": agent.Namespace,
		"taskName":       task.Name,
		"taskNamespace":  task.Namespace,
		"message":        "Agent created and task started",
	})
}

// parseRuntimeConfig extracts runtime configuration from chat args into the agent spec.
func parseRuntimeConfig(ctx context.Context, k8sClient client.Reader, namespace string, a map[string]any, agent *corev1alpha1.Agent) (string, bool) {
	if coord, ok := a["coordination"].(map[string]any); ok {
		if enabled, ok := coord["enabled"].(bool); ok && enabled {
			return "", true
		}
	}
	rt, ok := a["runtime"]
	if !ok {
		return "", true
	}
	rtMap, ok := rt.(map[string]any)
	if !ok {
		return "", true
	}
	runtimeType := chatGetStringArg(rtMap, "type")
	if runtimeType == "" {
		return "", true
	}
	agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{
		Type: corev1alpha1.AgentRuntimeType(runtimeType),
	}
	secretRef, err := resolveRuntimeSecretRef(ctx, k8sClient, namespace, agent.Spec.Runtime.Type, chatGetRuntimeSecretRefArg(a))
	if err != nil {
		result, _ := ChatToolErrorResult("invalid_arguments", err.Error(), "Create one of the supported runtime credential secrets or choose an AI agent without runtime")
		return result, false
	}
	agent.Spec.SecretRef = secretRef
	agent.Spec.ProviderRef = nil
	return "", true
}

func chatGetRuntimeSecretRefArg(a map[string]any) string {
	rt, ok := a["runtime"].(map[string]any)
	if !ok {
		return ""
	}
	return chatGetStringArg(rt, "secretRef")
}

// parseCoordinationConfig extracts coordination configuration from chat args into the agent spec.
func parseCoordinationConfig(a map[string]any, agent *corev1alpha1.Agent) {
	coord, ok := a["coordination"]
	if !ok {
		return
	}
	coordMap, ok := coord.(map[string]any)
	if !ok {
		return
	}
	coordCfg := &corev1alpha1.CoordinationConfig{}
	if enabled, ok := coordMap["enabled"].(bool); ok {
		coordCfg.Enabled = enabled
	}
	if maxCC, ok := coordMap["maxConcurrentChildren"].(float64); ok {
		coordCfg.MaxConcurrentChildren = int32(maxCC)
	}
	if maxD, ok := coordMap["maxDepth"].(float64); ok {
		coordCfg.MaxDepth = int32(maxD)
	}
	if allowed, ok := coordMap["allowedAgents"].([]any); ok {
		for _, item := range allowed {
			aMap, ok := item.(map[string]any)
			if !ok {
				continue
			}
			aa := corev1alpha1.AllowedAgent{
				Name:      chatGetStringArg(aMap, "name"),
				Namespace: chatGetStringArg(aMap, "namespace"),
			}
			if aa.Name != "" {
				coordCfg.AllowedAgents = append(coordCfg.AllowedAgents, aa)
			}
		}
	}
	agent.Spec.Coordination = coordCfg
	if coordCfg.Enabled {
		agent.Spec.Runtime = nil
		agent.Spec.SecretRef = nil
	}
}
