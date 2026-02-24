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

// ListToolsTool lists available Tool CRDs.
type ListToolsTool struct{}

func (t *ListToolsTool) Name() string { return "list_tools" }

func (t *ListToolsTool) Description() string {
	return "List available tools and built-in tools with their descriptions."
}

func (t *ListToolsTool) Parameters() json.RawMessage {
	return mustMarshalSchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"namespace": map[string]any{"type": "string", "description": "Namespace"},
		},
	})
}

func (t *ListToolsTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	tc := GetToolContext(ctx)
	if tc == nil {
		return ChatToolErrorResult("internal_error", "missing tool context", "")
	}

	var a map[string]any
	if err := json.Unmarshal(args, &a); err != nil {
		return ChatToolErrorResult("invalid_arguments", fmt.Sprintf("failed to parse arguments: %v", err), "Ensure arguments are valid JSON")
	}

	namespace := chatGetStringArgDefault(a, "namespace", tc.Namespace)

	toolList := &corev1alpha1.ToolList{}
	if err := tc.Client.List(ctx, toolList, client.InNamespace(namespace)); err != nil {
		return classifyChatK8sErr(err)
	}

	tools := make([]map[string]any, 0, len(toolList.Items))
	for _, tool := range toolList.Items {
		tools = append(tools, map[string]any{
			"name":        tool.Name,
			"description": tool.Spec.Description,
			"builtin":     false,
		})
	}

	return ChatToolSuccess(tools)
}
