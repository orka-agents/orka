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

// DeleteToolTool deletes a Tool CRD.
type DeleteToolTool struct{}

func (t *DeleteToolTool) Name() string { return deleteToolToolName }

func (t *DeleteToolTool) Description() string {
	return "Delete a tool."
}

func (t *DeleteToolTool) Parameters() json.RawMessage {
	return mustMarshalSchema(map[string]any{jsonSchemaTypeField: jsonSchemaTypeObject, jsonSchemaPropertiesField: map[string]any{nameField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Tool name"}, namespaceField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: namespaceDescription}}, jsonSchemaRequiredField: []string{nameField}})
}

func (t *DeleteToolTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
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
		return ChatToolErrorResult("invalid_arguments", "name is required", "Provide the tool name")
	}

	namespace := chatGetStringArgDefault(a, namespaceField, tc.Namespace)

	tool := &corev1alpha1.Tool{}
	if err := tc.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, tool); err != nil {
		return classifyChatK8sErr(err)
	}

	if err := tc.Client.Delete(ctx, tool); err != nil {
		return classifyChatK8sErr(err)
	}

	return ChatToolSuccess(map[string]any{nameField: tool.Name, messageField: "Tool deleted"})
}
