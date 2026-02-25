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

// CreateToolCRDTool creates a Tool CRD with an HTTP endpoint.
type CreateToolCRDTool struct{}

func (t *CreateToolCRDTool) Name() string { return "create_tool" }

func (t *CreateToolCRDTool) Description() string {
	return "Create a tool with an HTTP endpoint."
}

func (t *CreateToolCRDTool) Parameters() json.RawMessage {
	return mustMarshalSchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":        map[string]any{"type": "string", "description": "Tool name"},
			"namespace":   map[string]any{"type": "string", "description": "Namespace"},
			"description": map[string]any{"type": "string", "description": "Tool description"},
			"url":         map[string]any{"type": "string", "description": "HTTP endpoint URL"},
			"method":      map[string]any{"type": "string", "description": "HTTP method (default POST)"},
		},
		"required": []string{"name", "description", "url"},
	})
}

func (t *CreateToolCRDTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
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
		return ChatToolErrorResult("invalid_arguments", "name is required", "Provide a name for the tool")
	}

	description := chatGetStringArg(a, "description")
	if description == "" {
		return ChatToolErrorResult("invalid_arguments", "description is required", "Provide a description for the tool")
	}

	url := chatGetStringArg(a, "url")
	if url == "" {
		return ChatToolErrorResult("invalid_arguments", "url is required", "Provide the HTTP endpoint URL")
	}

	namespace := chatGetStringArgDefault(a, "namespace", tc.Namespace)
	if r, ok := checkChatNamespaceScope(tc, namespace); !ok {
		return r, nil
	}

	method := chatGetStringArgDefault(a, "method", "POST")

	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: corev1alpha1.ToolSpec{
			Description: description,
			HTTP: corev1alpha1.HTTPExecution{
				URL:    url,
				Method: method,
			},
		},
	}

	if err := tc.Client.Create(ctx, tool); err != nil {
		return classifyChatK8sErr(err)
	}

	return ChatToolSuccess(map[string]any{
		"name":      tool.Name,
		"namespace": tool.Namespace,
		"message":   "Tool created",
	})
}
