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

// ChatDeleteAgentTool deletes an Agent CRD (chat version).
type ChatDeleteAgentTool struct{}

func (t *ChatDeleteAgentTool) Name() string { return deleteAgentToolName }

func (t *ChatDeleteAgentTool) Description() string {
	return "Delete an agent."
}

func (t *ChatDeleteAgentTool) Parameters() json.RawMessage {
	return mustMarshalSchema(map[string]any{jsonSchemaTypeField: jsonSchemaTypeObject, jsonSchemaPropertiesField: map[string]any{nameField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: agentNameDescription}, namespaceField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: namespaceDescription}}, jsonSchemaRequiredField: []string{nameField}})
}

func (t *ChatDeleteAgentTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
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
	if result, ok := authorizeAgentDelete(ctx, tc, agent); !ok {
		return result, nil
	}

	if err := tc.Client.Delete(ctx, agent); err != nil {
		return classifyChatK8sErr(err)
	}

	return ChatToolSuccess(map[string]any{nameField: agent.Name, messageField: "Agent deleted"})
}
