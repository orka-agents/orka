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
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/labels"
)

// DelegateTaskTool implements multi-agent task delegation
type DelegateTaskTool struct {
	k8sClient client.Client
}

// WorkspaceArgs specifies a git workspace for agent runtime tasks
type WorkspaceArgs struct {
	GitRepo      string `json:"gitRepo,omitempty"`
	Branch       string `json:"branch,omitempty"`
	Ref          string `json:"ref,omitempty"`
	GitSecretRef string `json:"gitSecretRef,omitempty"`
	PushBranch   string `json:"pushBranch,omitempty"`
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

	// PriorTask references a previously completed task whose diff should be
	// applied to the workspace before this task begins. Optional.
	PriorTask string `json:"prior_task,omitempty"`

	// Feedback provides review feedback to include in the task prompt.
	// Used with prior_task for iterative code review workflows. Optional.
	Feedback string `json:"feedback,omitempty"`

	// AutoRetry enables automatic re-creation of this child task if it fails.
	// When enabled, wait_for_tasks will automatically re-delegate failed tasks
	// with the error context as feedback. Optional.
	AutoRetry bool `json:"auto_retry,omitempty"`

	// MaxRetries is the maximum number of auto-retry attempts (default: 2).
	// Only used when auto_retry is true.
	MaxRetries *int `json:"max_retries,omitempty"`
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
	return "Delegate a task to another agent. Creates a child Task CR that will be picked up by the specified agent. Supports iterative workflows via prior_task (applies previous diff) and feedback (prepends review feedback to prompt)."
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
					},
					"gitSecretRef": {
						"type": "string",
						"description": "Name of the Kubernetes Secret containing git credentials (must have a 'token' key)"
					},
					"pushBranch": {
						"type": "string",
						"description": "Remote branch name to push changes to after the agent completes. When set, changes are committed and pushed automatically."
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
			},
			"prior_task": {
				"type": "string",
				"description": "Name of a previously completed task whose diff should be applied to the workspace before this task starts. Used for iterative workflows."
			},
			"feedback": {
				"type": "string",
				"description": "Review feedback to prepend to the task prompt. Used with prior_task for iterative code review workflows."
			},
			"auto_retry": {
				"type": "boolean",
				"description": "Enable automatic re-creation of this task if it fails. wait_for_tasks will re-delegate with error context."
			},
			"max_retries": {
				"type": "integer",
				"description": "Maximum number of auto-retry attempts (default: 2). Only used when auto_retry is true."
			}
		},
		"required": ["agent", "prompt"]
	}`)
}

// delegationContext holds validated delegation parameters.
type delegationContext struct {
	args            DelegateTaskArgs
	parentName      string
	parentNamespace string
	currentDepth    int
	namespace       string
	parentTask      *corev1alpha1.Task
	targetAgent     *corev1alpha1.Agent
	priority        *int32
}

// parseDelegateArgs parses and validates the delegation arguments and environment.
func (t *DelegateTaskTool) parseDelegateArgs(ctx context.Context, args json.RawMessage) (*delegationContext, error) {
	parentName := os.Getenv("ORKA_TASK_NAME")
	parentNamespace := os.Getenv("ORKA_TASK_NAMESPACE")
	depthStr := os.Getenv("ORKA_COORDINATION_DEPTH")
	allowedAgents := os.Getenv("ORKA_COORDINATION_ALLOWED_AGENTS")
	maxDepthStr := os.Getenv("ORKA_COORDINATION_MAX_DEPTH")

	var delegateArgs DelegateTaskArgs
	if err := json.Unmarshal(args, &delegateArgs); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	if delegateArgs.Agent == "" {
		return nil, fmt.Errorf("agent is required")
	}
	if delegateArgs.Prompt == "" {
		return nil, fmt.Errorf("prompt is required")
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
			return nil, fmt.Errorf("agent %q is not in the allowed agents list", delegateArgs.Agent)
		}
	}

	// Validate depth
	currentDepth := 0
	if depthStr != "" {
		var err error
		currentDepth, err = strconv.Atoi(depthStr)
		if err != nil {
			return nil, fmt.Errorf("invalid coordination depth %q: %w", depthStr, err)
		}
	}

	maxDepth := 3
	if maxDepthStr != "" {
		var err error
		maxDepth, err = strconv.Atoi(maxDepthStr)
		if err != nil {
			return nil, fmt.Errorf("invalid max coordination depth %q: %w", maxDepthStr, err)
		}
	}

	if currentDepth+1 > maxDepth {
		return nil, fmt.Errorf("coordination depth exceeded: current depth %d, max depth %d", currentDepth, maxDepth)
	}

	// Determine namespace
	ns := delegateArgs.Namespace
	if ns == "" {
		ns = parentNamespace
	}
	if ns == "" {
		ns = defaultNamespace
	}

	// Fetch parent Task for owner reference
	parentTask := &corev1alpha1.Task{}
	if parentName != "" {
		if err := t.k8sClient.Get(ctx, types.NamespacedName{Name: parentName, Namespace: ns}, parentTask); err != nil {
			return nil, fmt.Errorf("failed to get parent task: %w", err)
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
		return nil, fmt.Errorf("failed to get agent %q: %w", delegateArgs.Agent, err)
	}

	return &delegationContext{
		args:            delegateArgs,
		parentName:      parentName,
		parentNamespace: parentNamespace,
		currentDepth:    currentDepth,
		namespace:       ns,
		parentTask:      parentTask,
		targetAgent:     targetAgent,
		priority:        priority,
	}, nil
}

// buildDelegatedTask creates the child Task object from a validated delegation context.
func (t *DelegateTaskTool) buildDelegatedTask(ctx context.Context, dc *delegationContext) *corev1alpha1.Task {
	// Auto-detect task type based on agent configuration
	taskType := corev1alpha1.TaskTypeAI
	if dc.targetAgent.Spec.Runtime != nil {
		taskType = corev1alpha1.TaskTypeAgent
	}

	childTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: dc.parentName + "-child-",
			Namespace:    dc.namespace,
			Labels: map[string]string{
				labels.LabelParentTask:     dc.parentName,
				labels.LabelCoordinator:    trueStr,
				labels.LabelDelegatedAgent: dc.args.Agent,
			},
			Annotations: map[string]string{
				labels.AnnotationCoordinationDepth: strconv.Itoa(dc.currentDepth + 1),
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type: taskType,
			AgentRef: &corev1alpha1.AgentReference{
				Name: dc.args.Agent,
			},
			Prompt:   dc.args.Prompt,
			Priority: dc.priority,
		},
	}

	// Store auto-retry config as annotations
	if dc.args.AutoRetry {
		childTask.Annotations[labels.AnnotationAutoRetry] = trueStr
		maxRetries := 2
		if dc.args.MaxRetries != nil && *dc.args.MaxRetries >= 0 {
			maxRetries = *dc.args.MaxRetries
		}
		childTask.Annotations[labels.AnnotationMaxRetries] = strconv.Itoa(maxRetries)
		childTask.Annotations[labels.AnnotationRetryCount] = "0"
		childTask.Annotations[labels.AnnotationOriginalPrompt] = dc.args.Prompt
	}

	// Set agent runtime config for agent-type tasks
	if taskType == corev1alpha1.TaskTypeAgent {
		t.applyAgentRuntimeConfig(ctx, childTask, dc)
	}

	// Prepend feedback to prompt if provided
	if dc.args.Feedback != "" {
		childTask.Spec.Prompt = fmt.Sprintf("FEEDBACK FROM REVIEW:\n%s\n\nTASK:\n%s", dc.args.Feedback, childTask.Spec.Prompt)
	}

	// Handle prior task reference for iterative workflows
	if dc.args.PriorTask != "" {
		t.applyPriorTaskConfig(ctx, childTask, dc)
	}

	// Set owner reference if parent task exists
	if dc.parentTask.UID != "" {
		blockOwnerDeletion := true
		isController := true
		childTask.OwnerReferences = []metav1.OwnerReference{
			{
				APIVersion:         corev1alpha1.GroupVersion.String(),
				Kind:               "Task",
				Name:               dc.parentTask.Name,
				UID:                dc.parentTask.UID,
				Controller:         &isController,
				BlockOwnerDeletion: &blockOwnerDeletion,
			},
		}
	}

	return childTask
}

// applyAgentRuntimeConfig sets agent runtime configuration on the child task.
func (t *DelegateTaskTool) applyAgentRuntimeConfig(ctx context.Context, childTask *corev1alpha1.Task, dc *delegationContext) {
	childTask.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{}

	if dc.args.Workspace != nil {
		childTask.Spec.AgentRuntime.Workspace = &corev1alpha1.WorkspaceConfig{
			GitRepo:    dc.args.Workspace.GitRepo,
			Branch:     dc.args.Workspace.Branch,
			Ref:        dc.args.Workspace.Ref,
			PushBranch: dc.args.Workspace.PushBranch,
		}
		if dc.args.Workspace.GitSecretRef != "" {
			childTask.Spec.AgentRuntime.Workspace.GitSecretRef = &corev1.LocalObjectReference{
				Name: dc.args.Workspace.GitSecretRef,
			}
		} else if dc.args.Workspace.GitRepo != "" {
			// Auto-detect git credentials secret in the namespace
			if secretName := t.findGitSecret(ctx, dc.namespace); secretName != "" {
				childTask.Spec.AgentRuntime.Workspace.GitSecretRef = &corev1.LocalObjectReference{
					Name: secretName,
				}
			}
		}
	}

	if dc.args.MaxTurns != nil {
		childTask.Spec.AgentRuntime.MaxTurns = dc.args.MaxTurns
	}
	if dc.args.AllowBash != nil {
		childTask.Spec.AgentRuntime.AllowBash = dc.args.AllowBash
	}
}

// findGitSecret looks for a git credentials secret in the namespace.
// It checks well-known names first, then looks for secrets with a "token" key.
func (t *DelegateTaskTool) findGitSecret(ctx context.Context, namespace string) string {
	// Check well-known secret names
	for _, name := range []string{"github-credentials", "git-credentials", "github-token", "git-token"} {
		secret := &corev1.Secret{}
		if err := t.k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, secret); err == nil {
			if _, hasToken := secret.Data["token"]; hasToken {
				return name
			}
			if _, hasPassword := secret.Data["password"]; hasPassword {
				return name
			}
		}
	}
	return ""
}

// applyPriorTaskConfig sets prior task reference and copies workspace config from the prior task.
func (t *DelegateTaskTool) applyPriorTaskConfig(ctx context.Context, childTask *corev1alpha1.Task, dc *delegationContext) {
	childTask.Spec.PriorTaskRef = &corev1alpha1.PriorTaskReference{
		Name:      dc.args.PriorTask,
		Namespace: dc.namespace,
	}

	priorTask := &corev1alpha1.Task{}
	if err := t.k8sClient.Get(ctx, types.NamespacedName{Name: dc.args.PriorTask, Namespace: dc.namespace}, priorTask); err == nil {
		// Copy workspace from prior task if not explicitly provided
		if dc.args.Workspace == nil {
			if priorTask.Spec.AgentRuntime != nil && priorTask.Spec.AgentRuntime.Workspace != nil {
				if childTask.Spec.AgentRuntime == nil {
					childTask.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{}
				}
				childTask.Spec.AgentRuntime.Workspace = priorTask.Spec.AgentRuntime.Workspace.DeepCopy()
			}
		}

		// Increment iteration count
		iteration := 1
		if iterStr, ok := priorTask.Labels[labels.LabelIteration]; ok {
			if iter, err := strconv.Atoi(iterStr); err == nil {
				iteration = iter + 1
			}
		}
		childTask.Labels[labels.LabelIteration] = strconv.Itoa(iteration)

		// Copy or generate iteration group
		if group, ok := priorTask.Labels[labels.LabelIterationGroup]; ok {
			childTask.Labels[labels.LabelIterationGroup] = group
		} else {
			childTask.Labels[labels.LabelIterationGroup] = string(priorTask.UID)
		}
	}
}

// Execute delegates a task to another agent
func (t *DelegateTaskTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	dc, err := t.parseDelegateArgs(ctx, args)
	if err != nil {
		return "", err
	}

	childTask := t.buildDelegatedTask(ctx, dc)

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
