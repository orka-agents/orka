/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/sozercan/mercan/api/v1alpha1"
)

// DelegateTaskTool implements multi-agent task delegation
type DelegateTaskTool struct {
	k8sClient client.Client
}

// WorkspaceArgs specifies a git workspace for agent runtime tasks
type WorkspaceArgs struct {
	GitRepo string `json:"gitRepo,omitempty"`
	Branch  string `json:"branch,omitempty"`
	Ref     string `json:"ref,omitempty"`
}

// DelegateTaskArgs are the arguments for the delegate_task tool
type DelegateTaskArgs struct {
	Agent     string         `json:"agent"`
	Prompt    string         `json:"prompt"`
	Namespace string         `json:"namespace,omitempty"`
	Priority  *int32         `json:"priority,omitempty"`
	Workspace *WorkspaceArgs `json:"workspace,omitempty"`
	MaxTurns  *int32         `json:"maxTurns,omitempty"`
	AllowBash *bool          `json:"allowBash,omitempty"`
}

// DelegateTaskResult represents the delegation result
type DelegateTaskResult struct {
	TaskName string `json:"taskName"`
	Status   string `json:"status"`
}

// NewDelegateTaskTool creates a new delegate task tool
func NewDelegateTaskTool(k8sClient client.Client) *DelegateTaskTool {
	return &DelegateTaskTool{
		k8sClient: k8sClient,
	}
}

// Name returns the tool name
func (t *DelegateTaskTool) Name() string {
	return "delegate_task"
}

// Description returns the tool description
func (t *DelegateTaskTool) Description() string {
	return "Delegate a task to another agent. Creates a child Task CR that will be picked up by the specified agent."
}

// Parameters returns the JSON Schema for parameters
func (t *DelegateTaskTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"agent": {
				"type": "string",
				"description": "Name of the agent to delegate to"
			},
			"prompt": {
				"type": "string",
				"description": "The task prompt for the agent"
			},
			"namespace": {
				"type": "string",
				"description": "Namespace (defaults to current)"
			},
			"priority": {
				"type": "integer",
				"description": "Priority 0-1000 (defaults to parent priority)"
			},
			"workspace": {
				"type": "object",
				"description": "Git workspace configuration for agent runtime tasks",
				"properties": {
					"gitRepo": {
						"type": "string",
						"description": "Git repository URL"
					},
					"branch": {
						"type": "string",
						"description": "Git branch name"
					},
					"ref": {
						"type": "string",
						"description": "Git ref (commit SHA or tag)"
					}
				}
			},
			"maxTurns": {
				"type": "integer",
				"description": "Maximum number of turns for the agent"
			},
			"allowBash": {
				"type": "boolean",
				"description": "Whether to allow bash execution in the agent"
			}
		},
		"required": ["agent", "prompt"]
	}`)
}

// Execute delegates a task to another agent
func (t *DelegateTaskTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	// Read environment variables
	parentName := os.Getenv("MERCAN_TASK_NAME")
	parentNamespace := os.Getenv("MERCAN_TASK_NAMESPACE")
	depthStr := os.Getenv("MERCAN_COORDINATION_DEPTH")
	allowedAgents := os.Getenv("MERCAN_COORDINATION_ALLOWED_AGENTS")
	maxDepthStr := os.Getenv("MERCAN_COORDINATION_MAX_DEPTH")

	// Parse args
	var delegateArgs DelegateTaskArgs
	if err := json.Unmarshal(args, &delegateArgs); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	if delegateArgs.Agent == "" {
		return "", fmt.Errorf("agent is required")
	}
	if delegateArgs.Prompt == "" {
		return "", fmt.Errorf("prompt is required")
	}

	// Validate agent is allowed
	if allowedAgents != "" {
		allowed := strings.Split(allowedAgents, ",")
		found := false
		for _, a := range allowed {
			if strings.TrimSpace(a) == delegateArgs.Agent {
				found = true
				break
			}
		}
		if !found {
			return "", fmt.Errorf("agent %q is not in the allowed agents list", delegateArgs.Agent)
		}
	}

	// Validate depth
	currentDepth := 0
	if depthStr != "" {
		var err error
		currentDepth, err = strconv.Atoi(depthStr)
		if err != nil {
			return "", fmt.Errorf("invalid coordination depth %q: %w", depthStr, err)
		}
	}

	maxDepth := 3 // default max depth
	if maxDepthStr != "" {
		var err error
		maxDepth, err = strconv.Atoi(maxDepthStr)
		if err != nil {
			return "", fmt.Errorf("invalid max coordination depth %q: %w", maxDepthStr, err)
		}
	}

	if currentDepth+1 > maxDepth {
		return "", fmt.Errorf("coordination depth exceeded: current depth %d, max depth %d", currentDepth, maxDepth)
	}

	// Determine namespace
	ns := delegateArgs.Namespace
	if ns == "" {
		ns = parentNamespace
	}
	if ns == "" {
		ns = "default"
	}

	// Fetch parent Task for owner reference
	parentTask := &corev1alpha1.Task{}
	if parentName != "" {
		if err := t.k8sClient.Get(ctx, types.NamespacedName{Name: parentName, Namespace: ns}, parentTask); err != nil {
			return "", fmt.Errorf("failed to get parent task: %w", err)
		}
	}

	// Determine priority
	var priority *int32
	if delegateArgs.Priority != nil {
		priority = delegateArgs.Priority
	} else if parentTask.Spec.Priority != nil {
		priority = parentTask.Spec.Priority
	}

	// Look up the target Agent to determine task type
	targetAgent := &corev1alpha1.Agent{}
	if err := t.k8sClient.Get(ctx, types.NamespacedName{
		Name: delegateArgs.Agent, Namespace: ns,
	}, targetAgent); err != nil {
		return "", fmt.Errorf("failed to get agent %q: %w", delegateArgs.Agent, err)
	}

	// Auto-detect task type based on agent configuration
	taskType := corev1alpha1.TaskTypeAI
	if targetAgent.Spec.Runtime != nil {
		taskType = corev1alpha1.TaskTypeAgent
	}

	// Build child Task
	childTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: parentName + "-child-",
			Namespace:    ns,
			Labels: map[string]string{
				"mercan.ai/parent-task":     parentName,
				"mercan.ai/coordinator":     "true",
				"mercan.ai/delegated-agent": delegateArgs.Agent,
			},
			Annotations: map[string]string{
				"mercan.ai/coordination-depth": strconv.Itoa(currentDepth + 1),
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type: taskType,
			AgentRef: &corev1alpha1.AgentReference{
				Name: delegateArgs.Agent,
			},
			Prompt:   delegateArgs.Prompt,
			Priority: priority,
		},
	}

	// Set agent runtime config for agent-type tasks
	if taskType == corev1alpha1.TaskTypeAgent {
		childTask.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{}

		if delegateArgs.Workspace != nil {
			childTask.Spec.AgentRuntime.Workspace = &corev1alpha1.WorkspaceConfig{
				GitRepo: delegateArgs.Workspace.GitRepo,
				Branch:  delegateArgs.Workspace.Branch,
				Ref:     delegateArgs.Workspace.Ref,
			}
		}

		if delegateArgs.MaxTurns != nil {
			childTask.Spec.AgentRuntime.MaxTurns = delegateArgs.MaxTurns
		}

		if delegateArgs.AllowBash != nil {
			childTask.Spec.AgentRuntime.AllowBash = delegateArgs.AllowBash
		}
	}

	// Set owner reference if parent task exists
	if parentTask.UID != "" {
		blockOwnerDeletion := true
		isController := true
		childTask.OwnerReferences = []metav1.OwnerReference{
			{
				APIVersion:         corev1alpha1.GroupVersion.String(),
				Kind:               "Task",
				Name:               parentTask.Name,
				UID:                parentTask.UID,
				Controller:         &isController,
				BlockOwnerDeletion: &blockOwnerDeletion,
			},
		}
	}

	// Create the child task
	if err := t.k8sClient.Create(ctx, childTask); err != nil {
		return "", fmt.Errorf("failed to create child task: %w", err)
	}

	result := DelegateTaskResult{
		TaskName: childTask.Name,
		Status:   "created",
	}

	output, err := json.Marshal(result)
	if err != nil {
		return "", err
	}

	return string(output), nil
}

// Ensure DelegateTaskTool implements Tool
var _ Tool = (*DelegateTaskTool)(nil)
