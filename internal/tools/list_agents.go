/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// Agents that use a runtimeRef are listed with runtime "external" and the
// referenced AgentRuntime name.
const (
	externalRuntimeLabel = "external"
	runtimeRefField      = "runtimeRef"
)

// ListAgentsTool lists available Agent CRDs.
type ListAgentsTool struct{}

func (t *ListAgentsTool) Name() string { return listAgentsToolName }

func (t *ListAgentsTool) Description() string {
	return "List available agents with their model, tools, and runtime configuration. Use before creating a task with agentRef to find the right agent."
}

func (t *ListAgentsTool) Parameters() json.RawMessage {
	return mustMarshalSchema(map[string]any{jsonSchemaTypeField: jsonSchemaTypeObject, jsonSchemaPropertiesField: map[string]any{namespaceField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: namespaceDescription}}})
}

func (t *ListAgentsTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	tc := GetToolContext(ctx)
	if tc == nil {
		return ChatToolErrorResult(internalErrorType, "missing tool context", "")
	}

	var a map[string]any
	if err := json.Unmarshal(args, &a); err != nil {
		return ChatToolErrorResult("invalid_arguments", fmt.Sprintf("failed to parse arguments: %v", err), "Ensure arguments are valid JSON")
	}

	namespace := chatGetStringArgDefault(a, namespaceField, tc.Namespace)

	agentList := &corev1alpha1.AgentList{}
	if err := tc.Client.List(ctx, agentList, client.InNamespace(namespace)); err != nil {
		return classifyChatK8sErr(err)
	}

	agents := make([]map[string]any, 0, len(agentList.Items))
	for _, agent := range agentList.Items {
		info := map[string]any{nameField: agent.Name}

		// A referenced Provider is authoritative over model.provider.
		provider := ""
		if agent.Spec.ProviderRef != nil {
			provider = agent.Spec.ProviderRef.Name
			info[providerRefField] = provider
		} else if agent.Spec.Model != nil {
			provider = agent.Spec.Model.Provider
		}
		if agent.Spec.Model != nil && agent.Spec.Model.Name != "" {
			model := agent.Spec.Model.Name
			if provider != "" {
				model = provider + "/" + model
			}
			info[modelField] = model
		}

		if len(agent.Spec.Tools) > 0 {
			toolNames := make([]string, 0, len(agent.Spec.Tools))
			for _, tr := range agent.Spec.Tools {
				toolNames = append(toolNames, tr.Name)
			}
			info[toolsField] = toolNames
		}

		if runtime := agent.Spec.Runtime; runtime != nil {
			// A runtimeRef names an admin-registered AgentRuntime instead of a built-in type.
			if runtime.RuntimeRef != nil {
				info[runtimeField] = externalRuntimeLabel
				info[runtimeRefField] = runtime.RuntimeRef.Name
			} else {
				info[runtimeField] = string(runtime.Type)
			}
		}

		if desc, ok := agent.Annotations[jsonSchemaDescriptionField]; ok {
			info[jsonSchemaDescriptionField] = desc
		}

		agents = append(agents, info)
	}

	return ChatToolSuccess(agents)
}
