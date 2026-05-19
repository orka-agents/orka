/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import corev1 "k8s.io/api/core/v1"

// ExecutionSpec defines worker pod runtime and placement controls.
type ExecutionSpec struct {
	// RuntimeClassName routes worker pods through a specific RuntimeClass.
	// +optional
	RuntimeClassName string `json:"runtimeClassName,omitempty"`

	// NodeSelector constrains worker pods to nodes with matching labels.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations allows worker pods to schedule onto tainted nodes.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// Affinity defines Kubernetes affinity and anti-affinity rules for worker pods.
	// +optional
	Affinity *corev1.Affinity `json:"affinity,omitempty"`

	// Workspace requests an upstream agent-sandbox execution workspace for agent Tasks.
	// When enabled, the Task controller validates the request and propagates the
	// resolved sandbox settings to the agent worker Job. The worker wrapper then
	// claims the sandbox workspace and runs the configured agent runtime inside it.
	// +optional
	Workspace *ExecutionWorkspaceSpec `json:"workspace,omitempty"`
}

// WorkspaceReusePolicy controls how execution workspaces are reused between tasks.
// +kubebuilder:validation:Enum=none;session
type WorkspaceReusePolicy string

const (
	// WorkspaceReusePolicyNone creates a fresh workspace per task.
	WorkspaceReusePolicyNone WorkspaceReusePolicy = "none"
	// WorkspaceReusePolicySession reuses a workspace for tasks in the same session.
	WorkspaceReusePolicySession WorkspaceReusePolicy = "session"
)

// WorkspaceCleanupPolicy controls what happens to an execution workspace after use.
// +kubebuilder:validation:Enum=delete;retain
type WorkspaceCleanupPolicy string

const (
	// WorkspaceCleanupPolicyDelete deletes the workspace after task completion.
	WorkspaceCleanupPolicyDelete WorkspaceCleanupPolicy = "delete"
	// WorkspaceCleanupPolicyRetain retains the workspace after task completion.
	WorkspaceCleanupPolicyRetain WorkspaceCleanupPolicy = "retain"
)

// ExecutionWorkspaceSpec defines an optional durable execution workspace request.
type ExecutionWorkspaceSpec struct {
	// Enabled requests use of a durable workspace for the task execution.
	// +kubebuilder:default=false
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// TemplateRef references the workspace template to instantiate or reuse.
	// The template name is required when enabled is true unless the controller
	// is configured with a default agent sandbox template.
	// +optional
	TemplateRef *WorkspaceTemplateReference `json:"templateRef,omitempty"`

	// ReusePolicy controls whether the workspace is fresh or session-scoped.
	// Defaults to none when omitted.
	// +kubebuilder:default=none
	// +optional
	ReusePolicy WorkspaceReusePolicy `json:"reusePolicy,omitempty"`

	// CleanupPolicy controls whether the workspace is deleted or retained after use.
	// Defaults to delete when omitted.
	// +optional
	CleanupPolicy WorkspaceCleanupPolicy `json:"cleanupPolicy,omitempty"`
}

// WorkspaceTemplateReference references an execution workspace template.
type WorkspaceTemplateReference struct {
	// Name is the name of the workspace template.
	// +optional
	Name string `json:"name,omitempty"`

	// Namespace is the namespace of the workspace template and claim.
	// It defaults to the Task namespace, or the controller namespace when configured.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}
