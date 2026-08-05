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

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// UpdateAgentTool updates an existing Agent CRD.
type UpdateAgentTool struct{}

func (t *UpdateAgentTool) Name() string { return updateAgentToolName }

func (t *UpdateAgentTool) Description() string {
	return "Update an existing agent."
}

func (t *UpdateAgentTool) Parameters() json.RawMessage {
	return mustMarshalSchema(map[string]any{jsonSchemaTypeField: jsonSchemaTypeObject, jsonSchemaPropertiesField: map[string]any{nameField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: agentNameDescription}, namespaceField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: namespaceDescription}, systemPromptField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "System prompt for the agent. OpenCode runtime Agents do not support Agent system prompts; use Task prompts instead."}, toolsField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeArray, itemsField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString}, jsonSchemaDescriptionField: "Tool names to attach"}, modelField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeObject, jsonSchemaPropertiesField: map[string]any{
		"provider": map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Model provider (e.g. anthropic, openai). For OpenCode this is normalized into model.name."},
		nameField:  map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Model name. OpenCode accepts a provider/model ID or a bare model name when the existing provider is retained."},
		"temperature": map[string]any{jsonSchemaTypeField: "number", "minimum": 0, "maximum": 2,
			jsonSchemaDescriptionField: "Sampling temperature. OpenCode only accepts the legacy default 0.7."},
		"contextWindow": map[string]any{jsonSchemaTypeField: jsonSchemaTypeInteger, "minimum": 1, jsonSchemaDescriptionField: "Reviewed model context capacity; required for OpenCode"},
		"maxTokens":     map[string]any{jsonSchemaTypeField: jsonSchemaTypeInteger, "minimum": 1, jsonSchemaDescriptionField: "Reviewed maximum output tokens; required for OpenCode"},
	},
		jsonSchemaDescriptionField: "Partial model update. Omitted fields preserve their existing values.",
	},
	}, jsonSchemaRequiredField: []string{nameField},
	})
}

func (t *UpdateAgentTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	tc := GetToolContext(ctx)
	if tc == nil {
		return ChatToolErrorResult(internalErrorType, "missing tool context", "")
	}

	var a map[string]any
	if err := json.Unmarshal(args, &a); err != nil {
		return ChatToolErrorResult("invalid_arguments", fmt.Sprintf("failed to parse arguments: %v", err), "Ensure arguments are valid JSON")
	}

	name := chatGetStringArg(a, nameField)
	if name == "" {
		return ChatToolErrorResult("invalid_arguments", "name is required", "Provide the agent name")
	}

	namespace := chatGetStringArgDefault(a, namespaceField, tc.Namespace)

	agent := &corev1alpha1.Agent{}
	if err := tc.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, agent); err != nil {
		return classifyChatK8sErr(err)
	}

	requestedSystemPrompt := chatGetStringArg(a, systemPromptField)
	if requestedSystemPrompt != "" && isOpenCodeAgent(agent) {
		return ChatToolErrorResult(
			"invalid_arguments",
			"opencode runtime does not support systemPrompt",
			"Omit systemPrompt for OpenCode Agents and use Task prompts for instructions.",
		)
	}

	if rawModel, supplied := a[modelField]; supplied {
		modelUpdate, err := parseAgentModelUpdate(rawModel)
		if err != nil {
			return ChatToolErrorResult(
				"invalid_arguments",
				err.Error(),
				"Provide model as an object with valid provider, name, temperature, contextWindow, and maxTokens fields.",
			)
		}
		if err := applyAgentModelUpdate(agent, modelUpdate); err != nil {
			return ChatToolErrorResult(
				"invalid_arguments",
				err.Error(),
				"Use a consistent provider/model ID and reviewed model limits.",
			)
		}
	}

	if requestedSystemPrompt != "" {
		agent.Spec.SystemPrompt = &corev1alpha1.PromptSource{
			Inline: requestedSystemPrompt,
		}
	}

	if toolNames := chatGetStringSliceArg(a, toolsField); len(toolNames) > 0 {
		agent.Spec.Tools = nil
		for _, tn := range toolNames {
			agent.Spec.Tools = append(agent.Spec.Tools, corev1alpha1.ToolReference{Name: tn})
		}
	}

	if isOpenCodeAgent(agent) {
		if agent.Spec.SystemPrompt != nil &&
			(strings.TrimSpace(agent.Spec.SystemPrompt.Inline) != "" || agent.Spec.SystemPrompt.ConfigMapRef != nil) {
			return ChatToolErrorResult(
				"invalid_arguments",
				"opencode runtime does not support systemPrompt",
				"Remove spec.systemPrompt and use Task prompts for OpenCode instructions.",
			)
		}
		if result, ok := normalizeChatOpenCodeModel(agent); !ok {
			return result, nil
		}
	}

	// Re-fetch before update to avoid conflicts
	latest := &corev1alpha1.Agent{}
	if err := tc.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, latest); err != nil {
		return classifyChatK8sErr(err)
	}
	agent.ResourceVersion = latest.ResourceVersion
	if result, ok := authorizeAgentUpdate(ctx, tc, agent); !ok {
		return result, nil
	}

	if err := tc.Client.Update(ctx, agent); err != nil {
		return classifyChatK8sErr(err)
	}

	return ChatToolSuccess(map[string]any{nameField: agent.Name, messageField: "Agent updated"})
}
