/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/labels"
)

// DeleteAgentTool implements agent cleanup for dynamically created agents
type DeleteAgentTool struct {
	k8sClient client.Client
}

// DeleteAgentArgs are the arguments for the delete_agent tool
type DeleteAgentArgs struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

// DeleteAgentResult represents the deletion result
type DeleteAgentResult struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// NewDeleteAgentTool creates a new delete agent tool
func NewDeleteAgentTool(k8sClient client.Client) *DeleteAgentTool {
	return &DeleteAgentTool{
		k8sClient: k8sClient,
	}
}

const deleteAgentToolName = "delete_agent"

// Name returns the tool name
func (t *DeleteAgentTool) Name() string {
	return deleteAgentToolName
}

// Description returns the tool description
func (t *DeleteAgentTool) Description() string {
	return "Delete a dynamically created agent. Removes the Agent CR from the cluster."
}

// Parameters returns the JSON Schema for parameters
func (t *DeleteAgentTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {
				"type": "string",
				"description": "Name of the agent to delete"
			},
			"namespace": {
				"type": "string",
				"description": "Namespace (defaults to current task namespace)"
			}
		},
		"required": ["name"]
	}`)
}

// Execute deletes an agent CR
func (t *DeleteAgentTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a DeleteAgentArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("failed to parse args: %w", err)
	}

	if a.Name == "" {
		return "", fmt.Errorf("agent name is required")
	}

	namespace := a.Namespace
	if namespace == "" {
		namespace = os.Getenv(envOrkaTaskNamespace)
	}
	if namespace == "" {
		namespace = defaultNamespace
	}

	// Get the agent to ensure it exists
	var agent corev1alpha1.Agent
	if err := t.k8sClient.Get(ctx, types.NamespacedName{
		Name:      a.Name,
		Namespace: namespace,
	}, &agent); err != nil {
		return "", fmt.Errorf("failed to get agent %s/%s: %w", namespace, a.Name, err)
	}

	// Verify ownership - only allow deleting agents created by the current task
	currentTask := os.Getenv(envOrkaTaskName)
	if currentTask != "" {
		ownerLabel := agent.Labels[labels.LabelCreatedBy]
		if ownerLabel != "" && ownerLabel != currentTask {
			return "", fmt.Errorf("cannot delete agent %s/%s: not owned by current task (owner: %q, current: %q)", namespace, a.Name, ownerLabel, currentTask)
		}
	}

	// Delete the agent
	if err := t.k8sClient.Delete(ctx, &agent); err != nil {
		return "", fmt.Errorf("failed to delete agent %s/%s: %w", namespace, a.Name, err)
	}

	result := DeleteAgentResult{
		Name:   a.Name,
		Status: deletedStatusString,
	}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("failed to marshal result: %w", err)
	}
	return string(resultJSON), nil
}
