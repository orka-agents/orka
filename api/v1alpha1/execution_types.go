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

	// Workspace selects an ExecutionWorkspaceClass for an ACP agent Task.
	// Omit workspace for ordinary execution. Workspace dispatch requires the
	// workspace provider API, ACP workspace dispatch, and an installed provider.
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

// WorkspaceProvider identifies a registered execution workspace controller.
// Resolution against an ExecutionWorkspaceProvider validates installed support.
// +kubebuilder:validation:MinLength=1
// +kubebuilder:validation:MaxLength=253
type WorkspaceProvider string

const (
	// WorkspaceProviderAgentSandbox is the legacy snapshot identity; new bindings use registered controller names.
	WorkspaceProviderAgentSandbox WorkspaceProvider = "agent-sandbox"
	// WorkspaceProviderSubstrate identifies retained Substrate MCP Tools and legacy snapshots.
	WorkspaceProviderSubstrate WorkspaceProvider = "substrate"
)

// ExecutionWorkspacePhase is Orka's provider-neutral workspace lifecycle phase.
// +kubebuilder:validation:Enum=Pending;Ready;Released;Retained;Deleted;Failed
type ExecutionWorkspacePhase string

const (
	ExecutionWorkspacePhasePending  ExecutionWorkspacePhase = "Pending"
	ExecutionWorkspacePhaseReady    ExecutionWorkspacePhase = "Ready"
	ExecutionWorkspacePhaseReleased ExecutionWorkspacePhase = "Released"
	ExecutionWorkspacePhaseRetained ExecutionWorkspacePhase = "Retained"
	ExecutionWorkspacePhaseDeleted  ExecutionWorkspacePhase = "Deleted"
	ExecutionWorkspacePhaseFailed   ExecutionWorkspacePhase = "Failed"
)

// ExecutionWorkspaceReason explains provider-neutral workspace lifecycle transitions.
// +kubebuilder:validation:Enum=WorkspacePending;WorkspaceClaimed;WorkspaceReady;WorkspaceReleased;WorkspaceRetained;WorkspaceDeleted;WorkspaceValidationFailed;WorkspaceAttachmentLocked;WorkspaceClaimFailed;WorkspaceReadinessFailed;WorkspaceHandoffFailed;WorkspaceCommandFailed;WorkspaceSecretScrubFailed;WorkspaceCleanupFailed;WorkspaceStatusUpdateFailed
type ExecutionWorkspaceReason string

const (
	ExecutionWorkspaceReasonPending            ExecutionWorkspaceReason = "WorkspacePending"
	ExecutionWorkspaceReasonClaimed            ExecutionWorkspaceReason = "WorkspaceClaimed"
	ExecutionWorkspaceReasonReady              ExecutionWorkspaceReason = "WorkspaceReady"
	ExecutionWorkspaceReasonReleased           ExecutionWorkspaceReason = "WorkspaceReleased"
	ExecutionWorkspaceReasonRetained           ExecutionWorkspaceReason = "WorkspaceRetained"
	ExecutionWorkspaceReasonDeleted            ExecutionWorkspaceReason = "WorkspaceDeleted"
	ExecutionWorkspaceReasonValidationFailed   ExecutionWorkspaceReason = "WorkspaceValidationFailed"
	ExecutionWorkspaceReasonAttachmentLocked   ExecutionWorkspaceReason = "WorkspaceAttachmentLocked"
	ExecutionWorkspaceReasonClaimFailed        ExecutionWorkspaceReason = "WorkspaceClaimFailed"
	ExecutionWorkspaceReasonReadinessFailed    ExecutionWorkspaceReason = "WorkspaceReadinessFailed"
	ExecutionWorkspaceReasonHandoffFailed      ExecutionWorkspaceReason = "WorkspaceHandoffFailed"
	ExecutionWorkspaceReasonCommandFailed      ExecutionWorkspaceReason = "WorkspaceCommandFailed"
	ExecutionWorkspaceReasonSecretScrubFailed  ExecutionWorkspaceReason = "WorkspaceSecretScrubFailed"
	ExecutionWorkspaceReasonCleanupFailed      ExecutionWorkspaceReason = "WorkspaceCleanupFailed"
	ExecutionWorkspaceReasonStatusUpdateFailed ExecutionWorkspaceReason = "WorkspaceStatusUpdateFailed"
)

// WorkspaceClassReference references an ExecutionWorkspaceClass in the Task namespace.
type WorkspaceClassReference struct {
	// Name is the class name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// WorkspaceOnDetachPolicy is a Task-requested class-permitted detach action.
// +kubebuilder:validation:Enum=Suspend;Delete
type WorkspaceOnDetachPolicy string

const (
	WorkspaceOnDetachSuspend WorkspaceOnDetachPolicy = "Suspend"
	WorkspaceOnDetachDelete  WorkspaceOnDetachPolicy = "Delete"
)

// ExecutionWorkspaceSpec selects a durable workspace through an administrator-managed class.
type ExecutionWorkspaceSpec struct {
	// ClassRef selects an immutable ExecutionWorkspaceClass in the Task namespace.
	// +required
	ClassRef *WorkspaceClassReference `json:"classRef"`

	// RestoreFrom seeds a new workspace from a retained Data checkpoint in the
	// Task namespace. This is a cold start with fresh credentials. It does not
	// replay a previous Task or restore process memory. Every continuation of
	// the new Session must keep this immutable origin reference.
	// +optional
	RestoreFrom *WorkspaceCheckpointReference `json:"restoreFrom,omitempty"`

	// ReusePolicy controls whether the workspace is fresh or session-scoped.
	// Defaults to none when omitted.
	// +kubebuilder:default=none
	// +optional
	ReusePolicy WorkspaceReusePolicy `json:"reusePolicy,omitempty"`

	// WorkspaceSlot names one independently reusable workspace within a Session.
	// +kubebuilder:default=default
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +optional
	WorkspaceSlot string `json:"workspaceSlot,omitempty"`

	// OnDetach requests an action allowed by the selected class.
	// +optional
	OnDetach WorkspaceOnDetachPolicy `json:"onDetach,omitempty"`
}

// WorkspaceCheckpointReference pins a namespaced ExecutionWorkspaceCheckpoint
// and its immutable Data artifact. Native snapshot locations are never accepted.
type WorkspaceCheckpointReference struct {
	// Name identifies a checkpoint in the Task namespace.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// UID prevents restoring through a replaced checkpoint name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	UID string `json:"uid"`
	// Digest pins the artifact accepted by the caller.
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	Digest string `json:"digest"`
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

// SubstrateActorPoolReference references a SubstrateActorPool.
type SubstrateActorPoolReference struct {
	// Name is the pool name.
	// +optional
	Name string `json:"name,omitempty"`

	// Namespace is the pool namespace. It defaults to the Task namespace.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}
