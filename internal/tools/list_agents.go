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

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
)

// ListAgentsTool lists available Agent CRDs.
type ListAgentsTool struct{}

func (t *ListAgentsTool) Name() string { return "list_agents" }

func (t *ListAgentsTool) Description() string {
	return "List available agents with their model, tools, and runtime configuration. Use before creating a task with agentRef to find the right agent."
}

func (t *ListAgentsTool) Parameters() json.RawMessage {
	return mustMarshalSchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"namespace": map[string]any{"type": "string", "description": "Namespace"},
		},
	})
}

func (t *ListAgentsTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	tc := GetToolContext(ctx)
	if tc == nil {
		return ChatToolErrorResult("internal_error", "missing tool context", "")
	}

	var a map[string]any
	if err := json.Unmarshal(args, &a); err != nil {
		return ChatToolErrorResult("invalid_arguments", fmt.Sprintf("failed to parse arguments: %v", err), "Ensure arguments are valid JSON")
	}

	namespace := chatGetStringArgDefault(a, "namespace", tc.Namespace)

	agentList := &corev1alpha1.AgentList{}
	if err := tc.Client.List(ctx, agentList, client.InNamespace(namespace)); err != nil {
		return classifyChatK8sErr(err)
	}

	agents := make([]map[string]any, 0, len(agentList.Items))
	for _, agent := range agentList.Items {
		info := map[string]any{
			"name": agent.Name,
		}

		if agent.Spec.Model != nil {
			info["model"] = fmt.Sprintf("%s/%s", agent.Spec.Model.Provider, agent.Spec.Model.Name)
		}

		if len(agent.Spec.Tools) > 0 {
			toolNames := make([]string, 0, len(agent.Spec.Tools))
			for _, tr := range agent.Spec.Tools {
				toolNames = append(toolNames, tr.Name)
			}
			info["tools"] = toolNames
		}

		if agent.Spec.Runtime != nil {
			info["runtime"] = string(agent.Spec.Runtime.Type)
		}

		if desc, ok := agent.Annotations["description"]; ok {
			info["description"] = desc
		}

		agents = append(agents, info)
	}

	return ChatToolSuccess(agents)
}
