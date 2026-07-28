/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"fmt"

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
	return mustMarshalSchema(map[string]any{jsonSchemaTypeField: jsonSchemaTypeObject, jsonSchemaPropertiesField: map[string]any{nameField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: agentNameDescription}, namespaceField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: namespaceDescription}, systemPromptField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "System prompt for the agent"}, toolsField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeArray, itemsField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString}, jsonSchemaDescriptionField: "Tool names to attach"}, modelField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeObject, jsonSchemaPropertiesField: map[string]any{
		"provider": map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Model provider (e.g. anthropic, openai)"}, nameField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Model name"}, "temperature": map[string]any{jsonSchemaTypeField: "number", jsonSchemaDescriptionField: "Sampling temperature"},
	},
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

	// Update specified fields
	if modelValue, ok := a[modelField]; ok {
		if err := applyAgentModelUpdate(agent, modelValue); err != nil {
			return ChatToolErrorResult(
				"invalid_arguments",
				err.Error(),
				"Provide model as an object with string provider/name fields and a numeric temperature",
			)
		}
	}

	if systemPrompt := chatGetStringArg(a, systemPromptField); systemPrompt != "" {
		agent.Spec.SystemPrompt = &corev1alpha1.PromptSource{
			Inline: systemPrompt,
		}
	}

	if toolNames := chatGetStringSliceArg(a, toolsField); len(toolNames) > 0 {
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
	if result, ok := authorizeAgentUpdate(ctx, tc, agent); !ok {
		return result, nil
	}

	if err := tc.Client.Update(ctx, agent); err != nil {
		return classifyChatK8sErr(err)
	}

	return ChatToolSuccess(map[string]any{nameField: agent.Name, messageField: "Agent updated"})
}

func applyAgentModelUpdate(agent *corev1alpha1.Agent, value any) error {
	switch model := value.(type) {
	case map[string]any:
		var provider *string
		if value, ok := model["provider"]; ok {
			providerValue, ok := value.(string)
			if !ok {
				return fmt.Errorf("model.provider must be a string")
			}
			provider = &providerValue
		}

		var name *string
		if value, ok := model[nameField]; ok {
			nameValue, ok := value.(string)
			if !ok {
				return fmt.Errorf("model.name must be a string")
			}
			name = &nameValue
		}

		var temperature *float64
		if value, ok := model["temperature"]; ok {
			temperatureValue, ok := value.(float64)
			if !ok {
				return fmt.Errorf("model.temperature must be a number")
			}
			temperature = &temperatureValue
		}

		if provider == nil && name == nil && temperature == nil {
			return nil
		}
		if agent.Spec.Model == nil {
			agent.Spec.Model = &corev1alpha1.ModelConfig{}
		}
		if provider != nil {
			agent.Spec.Model.Provider = *provider
		}
		if name != nil {
			agent.Spec.Model.Name = *name
		}
		if temperature != nil {
			agent.Spec.Model.Temperature = temperature
		}
		return nil

	case string:
		if model == "" {
			return nil
		}
		provider, name := splitModelString(model)
		if agent.Spec.Model == nil {
			agent.Spec.Model = &corev1alpha1.ModelConfig{}
		}
		if provider != "" {
			agent.Spec.Model.Provider = provider
		}
		agent.Spec.Model.Name = name
		return nil

	default:
		return fmt.Errorf("model must be an object or legacy provider/name string")
	}
}
