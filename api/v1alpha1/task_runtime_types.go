/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// WorkspaceIntent declares whether an agent workspace is immutable read-only
// input or may produce a validated publication artifact. Agent Tasks that omit
// intent are interpreted as read by controller logic; container Task behavior
// is unchanged when intent is omitted.
// +kubebuilder:validation:Enum=read;write
type WorkspaceIntent string

const (
	WorkspaceIntentRead  WorkspaceIntent = "read"
	WorkspaceIntentWrite WorkspaceIntent = "write"
)

// WorkspaceCredentialReference references a Secret in the Task namespace. The
// controller freezes the Secret resourceVersion when it reserves an attempt;
// Secret contents are never copied to Task status.
type WorkspaceCredentialReference struct {
	// Name is the name of the Secret in the Task namespace.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`

	// Key is the Secret data key containing one bearer token or one complete
	// Authorization header. It defaults to "token" when omitted.
	// +optional
	// +kubebuilder:default=token
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9._-]+$`
	Key string `json:"key,omitempty"`
}

// RepositoryIdentity is the canonical identity derived from a credential-free
// repository URL. It is used for ownership, BranchClaim, and publication
// reconciliation decisions without persisting credentials.
type RepositoryIdentity struct {
	// Provider identifies the source-control provider or forge.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`
	Provider string `json:"provider"`

	// ID is the canonical credential-free URL identity and must match the
	// corresponding repository URL after normalization. For GitHub, use
	// "github.com/owner/repo"; GitHub GraphQL node IDs are not accepted.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	ID string `json:"id"`
}

// TaskExecutionState is the durable execution state of one Task attempt.
// +kubebuilder:validation:Enum=Queued;Reserved;SessionStarting;Planned;Submitting;SubmittedUnknown;Accepted;Running;Settling;Succeeded;Failed;Cancelled;OutcomeUnknown
type TaskExecutionState string

const (
	TaskExecutionStateQueued           TaskExecutionState = "Queued"
	TaskExecutionStateReserved         TaskExecutionState = "Reserved"
	TaskExecutionStateSessionStarting  TaskExecutionState = "SessionStarting"
	TaskExecutionStatePlanned          TaskExecutionState = "Planned"
	TaskExecutionStateSubmitting       TaskExecutionState = "Submitting"
	TaskExecutionStateSubmittedUnknown TaskExecutionState = "SubmittedUnknown"
	TaskExecutionStateAccepted         TaskExecutionState = "Accepted"
	TaskExecutionStateRunning          TaskExecutionState = "Running"
	TaskExecutionStateSettling         TaskExecutionState = "Settling"
	TaskExecutionStateSucceeded        TaskExecutionState = "Succeeded"
	TaskExecutionStateFailed           TaskExecutionState = "Failed"
	TaskExecutionStateCancelled        TaskExecutionState = "Cancelled"
	TaskExecutionStateOutcomeUnknown   TaskExecutionState = "OutcomeUnknown"
)

// TaskExecutionOutcome is the terminal classification of one Task attempt.
// OutcomeUnknown is terminal and must never be treated as generic retryable
// failure by controller logic.
// +kubebuilder:validation:Enum=Succeeded;Failed;Cancelled;OutcomeUnknown
type TaskExecutionOutcome string

const (
	TaskExecutionOutcomeSucceeded      TaskExecutionOutcome = "Succeeded"
	TaskExecutionOutcomeFailed         TaskExecutionOutcome = "Failed"
	TaskExecutionOutcomeCancelled      TaskExecutionOutcome = "Cancelled"
	TaskExecutionOutcomeOutcomeUnknown TaskExecutionOutcome = "OutcomeUnknown"
)

// TaskExecutionReason is a bounded machine-readable execution reason.
// +kubebuilder:validation:MaxLength=128
// +kubebuilder:validation:Pattern=`^[A-Za-z][A-Za-z0-9._-]{0,127}$`
type TaskExecutionReason string

const (
	// TaskExecutionReasonAtCapacity is a scheduling reason, not a terminal Task phase.
	TaskExecutionReasonAtCapacity TaskExecutionReason = "AtCapacity"
	// TaskExecutionReasonRuntimeLost classifies accepted or running work lost with
	// an unprovable terminal result; its outcome must be OutcomeUnknown.
	TaskExecutionReasonRuntimeLost TaskExecutionReason = "RuntimeLost"
)

// TaskExecutionStatus is the structured execution lifecycle for the current
// Task attempt. The existing top-level Task phase remains the compatibility
// projection and is not replaced by this status.
// +kubebuilder:validation:XValidation:rule="!has(self.outcome) || (has(self.state) && self.state == self.outcome)",message="execution outcome requires the matching terminal state"
// +kubebuilder:validation:XValidation:rule="!has(self.state) || !(self.state in ['Succeeded', 'Failed', 'Cancelled', 'OutcomeUnknown']) || has(self.outcome)",message="terminal execution state requires an outcome"
type TaskExecutionStatus struct {
	// State is the current durable execution state.
	// +optional
	State TaskExecutionState `json:"state,omitempty"`

	// Outcome is set only after execution reaches a terminal classification.
	// +optional
	Outcome TaskExecutionOutcome `json:"outcome,omitempty"`

	// Reason is a stable machine-readable explanation for State or Outcome.
	// +optional
	Reason TaskExecutionReason `json:"reason,omitempty"`

	// Attempt is the one-based Task execution attempt represented by this status.
	// +kubebuilder:validation:Minimum=1
	// +optional
	Attempt int32 `json:"attempt,omitempty"`

	// PromptID is the durable prompt identity used for submission and settlement.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	PromptID string `json:"promptID,omitempty"`

	// RuntimePoolName is the namespaced logical pool selected for this attempt.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	RuntimePoolName string `json:"runtimePoolName,omitempty"`

	// RuntimePoolUID is the immutable pool UID fenced into runtime requests.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	RuntimePoolUID string `json:"runtimePoolUID,omitempty"`

	// AgentRuntimeName is the namespaced external orka.harness.v2 registration
	// selected for this attempt. It is mutually exclusive with RuntimePoolName.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	AgentRuntimeName string `json:"agentRuntimeName,omitempty"`

	// AgentRuntimeUID is the immutable external AgentRuntime UID fenced into the
	// attempt selection.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	AgentRuntimeUID string `json:"agentRuntimeUID,omitempty"`

	// RuntimeInstanceID is the exact selected supervisor Pod UID plus boot identity.
	// +kubebuilder:validation:MaxLength=512
	// +optional
	RuntimeInstanceID string `json:"runtimeInstanceID,omitempty"`

	// RuntimeSessionUID is the stable controller-owned Session execution identity.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	RuntimeSessionUID string `json:"runtimeSessionUID,omitempty"`

	// RuntimeSessionGeneration is the monotonic profile/session generation.
	// +kubebuilder:validation:Minimum=0
	// +optional
	RuntimeSessionGeneration int64 `json:"runtimeSessionGeneration,omitempty"`

	// RuntimeSessionSupervisorBootID freezes the supervisor boot that owns a
	// pending or reusable Session generation.
	// +kubebuilder:validation:MaxLength=512
	// +optional
	RuntimeSessionSupervisorBootID string `json:"runtimeSessionSupervisorBootID,omitempty"`

	// RuntimeSessionProfileDigest freezes the immutable runtime behavior bound to
	// a pending or reusable Session generation.
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	// +optional
	RuntimeSessionProfileDigest string `json:"runtimeSessionProfileDigest,omitempty"`

	// RuntimeSessionMCPDigest binds the complete non-secret effective MCP policy
	// and descriptor configuration to a pending or reusable Session generation.
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	// +optional
	RuntimeSessionMCPDigest string `json:"runtimeSessionMCPDigest,omitempty"`

	// RuntimeSessionWorkspaceDigest binds a reusable Session generation to the
	// exact repository, source ref, verified baseline, intent, and relative root.
	// It contains no credential material.
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	// +optional
	RuntimeSessionWorkspaceDigest string `json:"runtimeSessionWorkspaceDigest,omitempty"`

	// RuntimeSessionRecreationPending records that the exact generation is being
	// created or replaced and must be reconciled before a different request may
	// reuse that identity.
	// +optional
	RuntimeSessionRecreationPending bool `json:"runtimeSessionRecreationPending,omitempty"`

	// RuntimeSessionCleanupDigest is the controller-owned proof that the exact
	// RuntimeSession requiring retirement was deleted or its immutable runtime
	// instance was replaced. Users may read but cannot mutate the Task status subresource.
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	// +optional
	RuntimeSessionCleanupDigest string `json:"runtimeSessionCleanupDigest,omitempty"`

	// RequestDigest is the canonical immutable prompt request digest.
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	// +optional
	RequestDigest string `json:"requestDigest,omitempty"`

	// ControllerEpoch is the durable controller epoch fencing this attempt.
	// +kubebuilder:validation:Minimum=0
	// +optional
	ControllerEpoch int64 `json:"controllerEpoch,omitempty"`

	// ReadCredentialResourceVersion freezes the read credential Secret version
	// selected at reservation without exposing credential material.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	ReadCredentialResourceVersion string `json:"readCredentialResourceVersion,omitempty"`

	// PublicationReadCredentialResourceVersion freezes the target-read Secret
	// version used for preflight and independent verification.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	PublicationReadCredentialResourceVersion string `json:"publicationReadCredentialResourceVersion,omitempty"`

	// PublicationCredentialResourceVersion freezes the target-write Secret
	// version selected at reservation without exposing credential material.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	PublicationCredentialResourceVersion string `json:"publicationCredentialResourceVersion,omitempty"`

	// ForgeCredentialResourceVersion freezes the forge-only Secret version used
	// for pull request reconciliation.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	ForgeCredentialResourceVersion string `json:"forgeCredentialResourceVersion,omitempty"`

	// Message contains bounded, sanitized execution context.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	Message string `json:"message,omitempty"`

	// LastTransitionTime is the last durable execution-state transition time.
	// +optional
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
}

// TaskDeliveryState is the durable validation and publication lifecycle.
// +kubebuilder:validation:Enum=NotRequested;Validating;Preparing;Prepared;Publishing;Verifying;VerifiedExact;DeliveredSuperseded;ReadValidated;NoChange;CancelledBeforePublish;ReadOnlyWorkspaceModified;DeliveryConflict;CredentialBlocked;PublicationOutcomeUnknown
type TaskDeliveryState string

const (
	TaskDeliveryStateNotRequested              TaskDeliveryState = "NotRequested"
	TaskDeliveryStateValidating                TaskDeliveryState = "Validating"
	TaskDeliveryStatePreparing                 TaskDeliveryState = "Preparing"
	TaskDeliveryStatePrepared                  TaskDeliveryState = "Prepared"
	TaskDeliveryStatePublishing                TaskDeliveryState = "Publishing"
	TaskDeliveryStateVerifying                 TaskDeliveryState = "Verifying"
	TaskDeliveryStateVerifiedExact             TaskDeliveryState = "VerifiedExact"
	TaskDeliveryStateDeliveredSuperseded       TaskDeliveryState = "DeliveredSuperseded"
	TaskDeliveryStateReadValidated             TaskDeliveryState = "ReadValidated"
	TaskDeliveryStateNoChange                  TaskDeliveryState = "NoChange"
	TaskDeliveryStateCancelledBeforePublish    TaskDeliveryState = "CancelledBeforePublish"
	TaskDeliveryStateReadOnlyWorkspaceModified TaskDeliveryState = "ReadOnlyWorkspaceModified"
	TaskDeliveryStateDeliveryConflict          TaskDeliveryState = "DeliveryConflict"
	TaskDeliveryStateCredentialBlocked         TaskDeliveryState = "CredentialBlocked"
	TaskDeliveryStatePublicationOutcomeUnknown TaskDeliveryState = "PublicationOutcomeUnknown"
)

// TaskDeliveryOutcome is the terminal delivery classification.
// +kubebuilder:validation:Enum=NotRequested;VerifiedExact;DeliveredSuperseded;ReadValidated;NoChange;CancelledBeforePublish;ReadOnlyWorkspaceModified;DeliveryConflict;CredentialBlocked;PublicationOutcomeUnknown
type TaskDeliveryOutcome string

const (
	TaskDeliveryOutcomeNotRequested              TaskDeliveryOutcome = "NotRequested"
	TaskDeliveryOutcomeVerifiedExact             TaskDeliveryOutcome = "VerifiedExact"
	TaskDeliveryOutcomeDeliveredSuperseded       TaskDeliveryOutcome = "DeliveredSuperseded"
	TaskDeliveryOutcomeReadValidated             TaskDeliveryOutcome = "ReadValidated"
	TaskDeliveryOutcomeNoChange                  TaskDeliveryOutcome = "NoChange"
	TaskDeliveryOutcomeCancelledBeforePublish    TaskDeliveryOutcome = "CancelledBeforePublish"
	TaskDeliveryOutcomeReadOnlyWorkspaceModified TaskDeliveryOutcome = "ReadOnlyWorkspaceModified"
	TaskDeliveryOutcomeDeliveryConflict          TaskDeliveryOutcome = "DeliveryConflict"
	TaskDeliveryOutcomeCredentialBlocked         TaskDeliveryOutcome = "CredentialBlocked"
	TaskDeliveryOutcomePublicationOutcomeUnknown TaskDeliveryOutcome = "PublicationOutcomeUnknown"
)

// TaskDeliveryReason is a bounded machine-readable delivery reason.
// +kubebuilder:validation:MaxLength=128
// +kubebuilder:validation:Pattern=`^[A-Za-z][A-Za-z0-9._-]{0,127}$`
type TaskDeliveryReason string

const (
	// TaskDeliveryReasonCancellationRequestedAfterPublish records that publication
	// won the durable CAS and must continue reconciliation despite cancellation.
	TaskDeliveryReasonCancellationRequestedAfterPublish TaskDeliveryReason = "CancellationRequestedAfterPublish"
)

// TaskPullRequestReceipt is the bounded, non-secret receipt for an explicitly
// requested pull request reconciliation.
type TaskPullRequestReceipt struct {
	// ID is the provider's durable pull request identifier.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	ID string `json:"id"`

	// Number is the provider's numeric pull request number when available.
	// +kubebuilder:validation:Minimum=1
	// +optional
	Number int64 `json:"number,omitempty"`

	// URL is the canonical user-facing pull request URL. It must not contain credentials.
	// +kubebuilder:validation:MaxLength=2048
	// +optional
	URL string `json:"url,omitempty"`

	// State is the provider-observed pull request state.
	// +kubebuilder:validation:MaxLength=64
	// +optional
	State string `json:"state,omitempty"`

	// BaseBranch is the reconciled pull request base branch.
	// +kubebuilder:validation:MaxLength=255
	// +optional
	BaseBranch string `json:"baseBranch,omitempty"`

	// HeadBranch is the reconciled pull request head branch.
	// +kubebuilder:validation:MaxLength=255
	// +optional
	HeadBranch string `json:"headBranch,omitempty"`

	// HeadSHA is the exact observed pull request head commit.
	// +kubebuilder:validation:Pattern=`^([a-f0-9]{40}|[a-f0-9]{64})$`
	// +optional
	HeadSHA string `json:"headSHA,omitempty"`
}

// TaskDeliveryStatus is the structured validation/publication status and its
// durable, non-secret receipt.
// +kubebuilder:validation:XValidation:rule="!has(self.outcome) || (has(self.state) && self.state == self.outcome)",message="delivery outcome requires the matching terminal state"
// +kubebuilder:validation:XValidation:rule="!has(self.state) || !(self.state in ['NotRequested', 'VerifiedExact', 'DeliveredSuperseded', 'ReadValidated', 'NoChange', 'CancelledBeforePublish', 'ReadOnlyWorkspaceModified', 'DeliveryConflict', 'CredentialBlocked', 'PublicationOutcomeUnknown']) || has(self.outcome)",message="terminal delivery state requires an outcome"
type TaskDeliveryStatus struct {
	// State is the current durable delivery state.
	// +optional
	State TaskDeliveryState `json:"state,omitempty"`

	// Outcome is set only after delivery reaches a terminal classification.
	// +optional
	Outcome TaskDeliveryOutcome `json:"outcome,omitempty"`

	// Reason is a stable machine-readable explanation for State or Outcome.
	// +optional
	Reason TaskDeliveryReason `json:"reason,omitempty"`

	// PublicationID is the durable identity reused for reconciliation of the same artifact.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9._:-]{0,252}$`
	// +optional
	PublicationID string `json:"publicationID,omitempty"`

	// SourceRepository is the canonical repository from which the workspace baseline was created.
	// +optional
	SourceRepository *RepositoryIdentity `json:"sourceRepository,omitempty"`

	// PublicationRepository is the canonical repository whose branch was reconciled.
	// +optional
	PublicationRepository *RepositoryIdentity `json:"publicationRepository,omitempty"`

	// Branch is the publication branch without a refs/heads/ prefix.
	// +kubebuilder:validation:MaxLength=255
	// +optional
	Branch string `json:"branch,omitempty"`

	// StartingSHA is the verified source baseline before prompt execution.
	// +kubebuilder:validation:Pattern=`^([a-f0-9]{40}|[a-f0-9]{64})$`
	// +optional
	StartingSHA string `json:"startingSHA,omitempty"`

	// RemoteBeforeSHA is the exact publication ref observed before the CAS push.
	// Nil means not yet observed; a pointer to the empty string records explicit
	// absence; a non-empty value records the observed object ID.
	// +kubebuilder:validation:Pattern=`^(|[a-f0-9]{40}|[a-f0-9]{64})$`
	// +optional
	RemoteBeforeSHA *string `json:"remoteBeforeSHA,omitempty"`

	// TreeSHA is the deterministic clean-room tree written by the publisher.
	// +kubebuilder:validation:Pattern=`^([a-f0-9]{40}|[a-f0-9]{64})$`
	// +optional
	TreeSHA string `json:"treeSHA,omitempty"`

	// ExpectedCommitSHA is the exact Orka-owned commit prepared for publication.
	// +kubebuilder:validation:Pattern=`^([a-f0-9]{40}|[a-f0-9]{64})$`
	// +optional
	ExpectedCommitSHA string `json:"expectedCommitSHA,omitempty"`

	// VerifiedRemoteSHA is the independently observed remote branch head.
	// +kubebuilder:validation:Pattern=`^([a-f0-9]{40}|[a-f0-9]{64})$`
	// +optional
	VerifiedRemoteSHA string `json:"verifiedRemoteSHA,omitempty"`

	// SupersedingRemoteSHA is the verified descendant that superseded ExpectedCommitSHA.
	// +kubebuilder:validation:Pattern=`^([a-f0-9]{40}|[a-f0-9]{64})$`
	// +optional
	SupersedingRemoteSHA string `json:"supersedingRemoteSHA,omitempty"`

	// ArtifactDigest is the durable content-addressed workspace delta digest.
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	// +optional
	ArtifactDigest string `json:"artifactDigest,omitempty"`

	// PRReceipt is present only when createPR was explicitly requested and reconciled.
	// +optional
	PRReceipt *TaskPullRequestReceipt `json:"prReceipt,omitempty"`

	// Message contains bounded, sanitized delivery context.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	Message string `json:"message,omitempty"`

	// LastTransitionTime is the last durable delivery-state transition time.
	// +optional
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
}
