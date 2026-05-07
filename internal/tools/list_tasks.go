/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
)

// ListTasksTool lists Task CRDs with optional status filter.
type ListTasksTool struct{}

func (t *ListTasksTool) Name() string { return listTasksToolName }

func (t *ListTasksTool) Description() string {
	return "List tasks with optional status filter. Use to check what tasks exist or monitor multiple tasks."
}

func (t *ListTasksTool) Parameters() json.RawMessage {
	return mustMarshalSchema(map[string]any{jsonSchemaTypeField: jsonSchemaTypeObject, jsonSchemaPropertiesField: map[string]any{namespaceField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: namespaceDescription}, statusField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Filter by status: Pending, Running, Succeeded, Failed"}, limitField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeInteger, jsonSchemaDescriptionField: "Max results to return (default 20)"}}})
}

func (t *ListTasksTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	tc := GetToolContext(ctx)
	if tc == nil {
		return ChatToolErrorResult(internalErrorType, "missing tool context", "")
	}

	var a map[string]any
	if err := json.Unmarshal(args, &a); err != nil {
		return ChatToolErrorResult("invalid_arguments", fmt.Sprintf("failed to parse arguments: %v", err), "Ensure arguments are valid JSON")
	}

	namespace := chatGetStringArgDefault(a, namespaceField, tc.Namespace)
	statusFilter := chatGetStringArg(a, statusField)
	limit := chatGetIntArg(a, limitField, 20)

	taskList := &corev1alpha1.TaskList{}
	if err := tc.Client.List(ctx, taskList, client.InNamespace(namespace)); err != nil {
		return classifyChatK8sErr(err)
	}

	tasks := make([]map[string]any, 0)
	for _, task := range taskList.Items {
		if statusFilter != "" && !strings.EqualFold(string(task.Status.Phase), statusFilter) {
			continue
		}

		age := time.Since(task.CreationTimestamp.Time).Round(time.Second).String()

		tasks = append(tasks, map[string]any{nameField: task.Name, phaseField: string(task.Status.Phase), jsonSchemaTypeField: string(task.Spec.Type), "age": age})

		if len(tasks) >= limit {
			break
		}
	}

	return ChatToolSuccess(tasks)
}
