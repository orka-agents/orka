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

// WorkspaceProvider selects the execution workspace backend.
// +kubebuilder:validation:Enum=agent-sandbox;substrate
type WorkspaceProvider string

const (
	// WorkspaceProviderAgentSandbox uses the Kubernetes SIG agent-sandbox backend.
	WorkspaceProviderAgentSandbox WorkspaceProvider = "agent-sandbox"
	// WorkspaceProviderSubstrate uses the Agent Substrate actor backend.
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

// ExecutionWorkspaceSpec defines an optional durable execution workspace request.
type ExecutionWorkspaceSpec struct {
	// Enabled requests use of a durable workspace for the task execution.
	// +kubebuilder:default=false
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// Provider selects the workspace backend. When omitted, the controller
	// resolves the configured default workspace provider; the built-in
	// compatibility default is agent-sandbox.
	// +optional
	Provider WorkspaceProvider `json:"provider,omitempty"`

	// TemplateRef references the workspace template to instantiate or reuse.
	// The template name is required when enabled is true unless the controller
	// is configured with a provider-specific default template.
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

	// Boot asks providers that support it to boot the workspace workload from scratch
	// instead of resuming from the provider's default snapshot. Currently supported
	// by the Substrate provider.
	// +optional
	Boot bool `json:"boot,omitempty"`

	// PoolRef references an operator-managed Substrate actor pool for placement,
	// density tracking, and oversubscription policy.
	// +optional
	PoolRef *SubstrateActorPoolReference `json:"poolRef,omitempty"`

	// Snapshot configures explicit provider snapshot restore/checkpoint behavior.
	// Non-empty settings are currently rejected until provider checkpoint/restore
	// support is available through Orka.
	// +optional
	Snapshot *ExecutionWorkspaceSnapshotSpec `json:"snapshot,omitempty"`

	// Hibernation configures process lifetime inside the workspace. Resident
	// mode is currently rejected until the worker protocol can report per-turn
	// completion separately from resident process lifetime.
	// +optional
	Hibernation *ExecutionWorkspaceHibernationSpec `json:"hibernation,omitempty"`
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

// ExecutionWorkspaceSnapshotSpec selects explicit provider snapshots.
type ExecutionWorkspaceSnapshotSpec struct {
	// RestoreURI is a provider-native snapshot URI prefix to restore before the
	// workspace command runs.
	// +optional
	RestoreURI string `json:"restoreURI,omitempty"`

	// CheckpointURI is a provider-native snapshot URI prefix to write when the
	// workspace is retained or released.
	// +optional
	CheckpointURI string `json:"checkpointURI,omitempty"`

	// CheckpointOnRelease requests a checkpoint when cleanup releases or retains
	// the workspace. CheckpointURI must be set when this is true.
	// +optional
	CheckpointOnRelease bool `json:"checkpointOnRelease,omitempty"`
}

// ExecutionWorkspaceProcessMode controls process lifetime inside a workspace.
// +kubebuilder:validation:Enum=fresh;resident
type ExecutionWorkspaceProcessMode string

const (
	// ExecutionWorkspaceProcessModeFresh starts a fresh command for each task.
	ExecutionWorkspaceProcessModeFresh ExecutionWorkspaceProcessMode = "fresh"
	// ExecutionWorkspaceProcessModeResident reuses a long-lived process when the
	// runtime command supports a stdin-driven resident protocol.
	ExecutionWorkspaceProcessModeResident ExecutionWorkspaceProcessMode = "resident"
)

// ExecutionWorkspaceHibernationSpec configures resident process reuse.
type ExecutionWorkspaceHibernationSpec struct {
	// ProcessMode controls whether each turn starts fresh or reuses a resident
	// process. Defaults to fresh.
	// +optional
	ProcessMode ExecutionWorkspaceProcessMode `json:"processMode,omitempty"`

	// ResidentKey selects the process slot for resident mode. When omitted, the
	// worker derives a stable key from namespace, template, and reuse key.
	// +optional
	ResidentKey string `json:"residentKey,omitempty"`
}
