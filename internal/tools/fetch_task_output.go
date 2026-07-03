/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

// FetchTaskOutputTool fetches the result/output of a completed task.
type FetchTaskOutputTool struct{}

func (t *FetchTaskOutputTool) Name() string { return fetchTaskOutputToolName }

func (t *FetchTaskOutputTool) Description() string {
	return "Get the result/output of a completed task from its ConfigMap. Returns the task result truncated to 2K characters if large. Do NOT use to check if a task is still running — use check_task_progress for that."
}

func (t *FetchTaskOutputTool) Parameters() json.RawMessage {
	return mustMarshalSchema(map[string]any{jsonSchemaTypeField: jsonSchemaTypeObject, jsonSchemaPropertiesField: map[string]any{nameField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: taskNameDescription}, namespaceField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: namespaceDescription}}, jsonSchemaRequiredField: []string{nameField}})
}

func (t *FetchTaskOutputTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
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
		return ChatToolErrorResult("invalid_arguments", "name is required", "Provide the task name")
	}
	namespace := chatGetStringArgDefault(a, namespaceField, tc.Namespace)

	task := &corev1alpha1.Task{}
	if err := tc.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, task); err != nil {
		return classifyChatK8sErr(err)
	}

	if task.Status.ResultRef == nil || !task.Status.ResultRef.Available {
		return ChatToolErrorResult(errTypeNotFound, "task has no result yet", "Wait for the task to complete, then try again")
	}

	if tc.ResultStore == nil {
		return ChatToolErrorResult(internalErrorType, "result store not configured", "")
	}

	data, err := tc.ResultStore.GetResult(ctx, namespace, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ChatToolErrorResult(errTypeNotFound, "result not found in store", "The result may have been deleted")
		}
		return ChatToolErrorResult(internalErrorType, fmt.Sprintf("failed to get result: %v", err), "")
	}

	result := string(data)
	const maxLen = 2048
	if len(result) > maxLen {
		result = result[:maxLen] + fmt.Sprintf(" [truncated, full output: %d chars]", len(data))
	}

	return ChatToolSuccess(map[string]any{nameField: task.Name, phaseField: string(task.Status.Phase), "output": result})
}
