/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// PromptAttemptExecutionState is the durable prompt execution state.
// +kubebuilder:validation:Enum=Queued;Reserved;SessionStarting;Planned;Submitting;SubmittedUnknown;Accepted;Running;Settling;Succeeded;Failed;Cancelled;OutcomeUnknown
type PromptAttemptExecutionState string

// PromptAttemptDeliveryState is the durable delivery state for one prompt.
// +kubebuilder:validation:Enum=NotRequested;Validating;Preparing;Prepared;Publishing;Verifying;VerifiedExact;DeliveredSuperseded;ReadValidated;NoChange;CancelledBeforePublish;ReadOnlyWorkspaceModified;DeliveryConflict;CredentialBlocked;PublicationOutcomeUnknown
type PromptAttemptDeliveryState string

// PromptCredentialBinding freezes one role-specific Secret identity without
// storing credential material.
type PromptCredentialBinding struct {
	// +kubebuilder:validation:Enum=SourceRead;TargetRead;TargetWrite;Forge
	Role string `json:"role"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Namespace string `json:"namespace"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	SecretName string `json:"secretName"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	SecretKey string `json:"secretKey"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	SecretUID string `json:"secretUid"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	ResourceVersion string `json:"resourceVersion"`
}

// PromptAttemptSpec is the immutable identity and request binding for one
// Task prompt attempt.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="prompt attempt spec is immutable"
type PromptAttemptSpec struct {
	// ID is the canonical DurableControlStore prompt-attempt ID.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	ID string `json:"id"`

	// TaskUID is the immutable Kubernetes UID of the Task.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	TaskUID string `json:"taskUid"`

	// Attempt is the one-based Task attempt number.
	// +kubebuilder:validation:Minimum=1
	Attempt int64 `json:"attempt"`

	// PromptID is the immutable prompt identity within the attempt.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	PromptID string `json:"promptId"`

	// RequestDigest binds the prompt identity to exact canonical input.
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	RequestDigest string `json:"requestDigest"`

	// CredentialBindings is the immutable, role-separated Secret identity set.
	// +optional
	// +listType=map
	// +listMapKey=role
	// +kubebuilder:validation:MaxItems=4
	CredentialBindings []PromptCredentialBinding `json:"credentialBindings,omitempty"`
}

// PromptAttemptStatus holds the exact execution and delivery state machines.
// +kubebuilder:validation:XValidation:rule="!has(self.executionState) || self.executionState != 'OutcomeUnknown' || (has(self.outcomeMarker) && size(self.outcomeMarker) > 0)",message="OutcomeUnknown requires an explicit outcome marker"
type PromptAttemptStatus struct {
	// SessionUID is immutable after first binding.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	SessionUID string `json:"sessionUid,omitempty"`

	// SessionLeaseGeneration is immutable after first binding.
	// +optional
	// +kubebuilder:validation:Minimum=1
	SessionLeaseGeneration int64 `json:"sessionLeaseGeneration,omitempty"`

	// RuntimeInstanceID is immutable after first binding.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	RuntimeInstanceID string `json:"runtimeInstanceId,omitempty"`

	// +optional
	ExecutionState PromptAttemptExecutionState `json:"executionState,omitempty"`

	// +optional
	DeliveryState PromptAttemptDeliveryState `json:"deliveryState,omitempty"`

	// +optional
	// +kubebuilder:validation:MaxLength=16384
	TerminalReason string `json:"terminalReason,omitempty"`

	// +optional
	// +kubebuilder:validation:MaxLength=16384
	OutcomeMarker string `json:"outcomeMarker,omitempty"`

	ControlRecordMutationStatus `json:",inline"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=pattempt
// +kubebuilder:printcolumn:name="Execution",type=string,JSONPath=`.status.executionState`
// +kubebuilder:printcolumn:name="Delivery",type=string,JSONPath=`.status.deliveryState`
// +kubebuilder:printcolumn:name="Attempt",type=integer,JSONPath=`.spec.attempt`
// +kubebuilder:printcolumn:name="Version",type=integer,JSONPath=`.status.version`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PromptAttempt is the Kubernetes-authoritative prompt execution and delivery
// control record.
type PromptAttempt struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PromptAttemptSpec   `json:"spec"`
	Status PromptAttemptStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PromptAttemptList contains a list of PromptAttempt.
type PromptAttemptList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PromptAttempt `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PromptAttempt{}, &PromptAttemptList{})
}
