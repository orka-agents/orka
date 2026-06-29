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

func (t *ListToolsTool) Name() string { return listToolsToolName }

func (t *ListToolsTool) Description() string {
	return "List available tools and built-in tools with their descriptions."
}

func (t *ListToolsTool) Parameters() json.RawMessage {
	return mustMarshalSchema(map[string]any{jsonSchemaTypeField: jsonSchemaTypeObject, jsonSchemaPropertiesField: map[string]any{namespaceField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: namespaceDescription}}})
}

func (t *ListToolsTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	tc := GetToolContext(ctx)
	if tc == nil {
		return ChatToolErrorResult(internalErrorType, "missing tool context", "")
	}

	var a map[string]any
	if err := json.Unmarshal(args, &a); err != nil {
		return ChatToolErrorResult("invalid_arguments", fmt.Sprintf("failed to parse arguments: %v", err), "Ensure arguments are valid JSON")
	}

	namespace := chatGetStringArgDefault(a, namespaceField, tc.Namespace)
	if result, ok := checkChatNamespaceScope(tc, namespace); !ok {
		return result, nil
	}

	toolList := &corev1alpha1.ToolList{}
	if err := tc.Client.List(ctx, toolList, client.InNamespace(namespace)); err != nil {
		return classifyChatK8sErr(err)
	}

	tools := make([]map[string]any, 0, len(toolList.Items))
	for _, tool := range toolList.Items {
		tools = append(tools, map[string]any{nameField: tool.Name, jsonSchemaDescriptionField: tool.Spec.Description,
			"builtin": false,
		})
	}

	return ChatToolSuccess(tools)
}
