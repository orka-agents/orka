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

	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
)

// UpdateAgentTool updates an existing Agent CRD.
type UpdateAgentTool struct{}

func (t *UpdateAgentTool) Name() string { return "update_agent" }

func (t *UpdateAgentTool) Description() string {
	return "Update an existing agent."
}

func (t *UpdateAgentTool) Parameters() json.RawMessage {
	return mustMarshalSchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":         map[string]any{"type": "string", "description": "Agent name"},
			"namespace":    map[string]any{"type": "string", "description": "Namespace"},
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
		},
		"required": []string{"name"},
	})
}

func (t *UpdateAgentTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
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
		return ChatToolErrorResult("invalid_arguments", "name is required", "Provide the agent name")
	}

	namespace := chatGetStringArgDefault(a, "namespace", tc.Namespace)

	agent := &corev1alpha1.Agent{}
	if err := tc.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, agent); err != nil {
		return classifyChatK8sErr(err)
	}

	// Update specified fields
	if modelProvider := chatGetStringArg(a, "model"); modelProvider != "" {
		parts := strings.SplitN(modelProvider, "/", 2)
		if agent.Spec.Model == nil {
			agent.Spec.Model = &corev1alpha1.ModelConfig{}
		}
		if len(parts) == 2 {
			agent.Spec.Model.Provider = parts[0]
			agent.Spec.Model.Name = parts[1]
		} else {
			agent.Spec.Model.Name = modelProvider
		}
	}

	if systemPrompt := chatGetStringArg(a, "systemPrompt"); systemPrompt != "" {
		agent.Spec.SystemPrompt = &corev1alpha1.PromptSource{
			Inline: systemPrompt,
		}
	}

	if toolNames := chatGetStringSliceArg(a, "tools"); len(toolNames) > 0 {
		agent.Spec.Tools = nil
		for _, tn := range toolNames {
			agent.Spec.Tools = append(agent.Spec.Tools, corev1alpha1.ToolReference{Name: tn})
		}
	}

	// Re-fetch before update to avoid conflicts
	latest := &corev1alpha1.Agent{}
	if err := tc.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, latest); err != nil {
		return classifyChatK8sErr(err)
	}
	agent.ResourceVersion = latest.ResourceVersion

	if err := tc.Client.Update(ctx, agent); err != nil {
		return classifyChatK8sErr(err)
	}

	return ChatToolSuccess(map[string]any{
		"name":    agent.Name,
		"message": "Agent updated",
	})
}
