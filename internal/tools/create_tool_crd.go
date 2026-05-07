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

func (t *CreateToolCRDTool) Name() string { return createToolCRDToolName }

func (t *CreateToolCRDTool) Description() string {
	return "Create a tool with an HTTP endpoint."
}

func (t *CreateToolCRDTool) Parameters() json.RawMessage {
	return mustMarshalSchema(map[string]any{jsonSchemaTypeField: jsonSchemaTypeObject, jsonSchemaPropertiesField: map[string]any{nameField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Tool name"}, namespaceField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: namespaceDescription}, jsonSchemaDescriptionField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Tool description"}, urlField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "HTTP endpoint URL"}, methodField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "HTTP method (default POST)"}}, jsonSchemaRequiredField: []string{nameField, jsonSchemaDescriptionField, urlField}})
}

func (t *CreateToolCRDTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
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
		return ChatToolErrorResult("invalid_arguments", "name is required", "Provide a name for the tool")
	}

	description := chatGetStringArg(a, jsonSchemaDescriptionField)
	if description == "" {
		return ChatToolErrorResult("invalid_arguments", "description is required", "Provide a description for the tool")
	}

	url := chatGetStringArg(a, urlField)
	if url == "" {
		return ChatToolErrorResult("invalid_arguments", "url is required", "Provide the HTTP endpoint URL")
	}

	namespace := chatGetStringArgDefault(a, namespaceField, tc.Namespace)
	if r, ok := checkChatNamespaceScope(tc, namespace); !ok {
		return r, nil
	}

	method := chatGetStringArgDefault(a, methodField, httpMethodPostString)

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

	return ChatToolSuccess(map[string]any{nameField: tool.Name, namespaceField: tool.Namespace, messageField: "Tool created"})
}
