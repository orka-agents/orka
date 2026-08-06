/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// AgentExecutionAdjudicationAction is a one-way operator resolution. Actions
// never authorize prompt replay, publication replay, or protocol mutation.
// +kubebuilder:validation:Enum=ConfirmV1Outcome;ConfirmV2Outcome;CleanupV1;CleanupV2;CleanupBoth;MarkNoExecution;AbandonOutcomeUnknown;BootstrapNewLineage
type AgentExecutionAdjudicationAction string

const (
	// AgentExecutionAdjudicationConfirmV1Outcome confirms a proven terminal v1 outcome.
	AgentExecutionAdjudicationConfirmV1Outcome AgentExecutionAdjudicationAction = "ConfirmV1Outcome"
	// AgentExecutionAdjudicationConfirmV2Outcome confirms a proven terminal v2 outcome.
	AgentExecutionAdjudicationConfirmV2Outcome AgentExecutionAdjudicationAction = "ConfirmV2Outcome"
	// AgentExecutionAdjudicationCleanupV1 authorizes cleanup of the discovered v1 lineage.
	AgentExecutionAdjudicationCleanupV1 AgentExecutionAdjudicationAction = "CleanupV1"
	// AgentExecutionAdjudicationCleanupV2 authorizes cleanup of the discovered v2 lineage.
	AgentExecutionAdjudicationCleanupV2 AgentExecutionAdjudicationAction = "CleanupV2"
	// AgentExecutionAdjudicationCleanupBoth authorizes cleanup of both discovered lineages.
	AgentExecutionAdjudicationCleanupBoth AgentExecutionAdjudicationAction = "CleanupBoth"
	// AgentExecutionAdjudicationMarkNoExecution confirms UnboundNoExecution from
	// independently verified evidence.
	AgentExecutionAdjudicationMarkNoExecution AgentExecutionAdjudicationAction = "MarkNoExecution"
	// AgentExecutionAdjudicationAbandonOutcomeUnknown permanently abandons an
	// unprovable effect as OutcomeUnknown.
	AgentExecutionAdjudicationAbandonOutcomeUnknown AgentExecutionAdjudicationAction = "AbandonOutcomeUnknown"
	// AgentExecutionAdjudicationBootstrapNewLineage bootstraps a new Session
	// lineage from a reconciled canonical transcript.
	AgentExecutionAdjudicationBootstrapNewLineage AgentExecutionAdjudicationAction = "BootstrapNewLineage"
)

// AgentExecutionAdjudicationState is the controller-owned application state.
// +kubebuilder:validation:Enum=Pending;Applying;Applied;Rejected;Superseded
type AgentExecutionAdjudicationState string

const (
	AgentExecutionAdjudicationPending    AgentExecutionAdjudicationState = "Pending"
	AgentExecutionAdjudicationApplying   AgentExecutionAdjudicationState = "Applying"
	AgentExecutionAdjudicationApplied    AgentExecutionAdjudicationState = "Applied"
	AgentExecutionAdjudicationRejected   AgentExecutionAdjudicationState = "Rejected"
	AgentExecutionAdjudicationSuperseded AgentExecutionAdjudicationState = "Superseded"
)

// AgentExecutionSubjectReference identifies an exact adjudication subject.
type AgentExecutionSubjectReference struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// +kubebuilder:validation:Required
	UID types.UID `json:"uid"`
}

// AgentExecutionExpectedSubjectState fences adjudication application to the
// exact observed subject and evidence closure.
type AgentExecutionExpectedSubjectState struct {
	// SubjectResourceVersion is the subject resourceVersion the requester
	// observed when creating the adjudication.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	SubjectResourceVersion string `json:"subjectResourceVersion"`

	// SubjectDomainVersion is the durable-store domain version of the subject,
	// when one exists.
	// +optional
	// +kubebuilder:validation:Minimum=0
	SubjectDomainVersion int64 `json:"subjectDomainVersion,omitempty"`

	// EvidenceClosureWatermark digests the complete evidence set observed for
	// the subject. Newly discovered evidence supersedes the adjudication.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	EvidenceClosureWatermark string `json:"evidenceClosureWatermark"`
}

// AgentExecutionAdjudicationSpec is the immutable, admin-authored resolution
// request. Original quarantine and blocked-state evidence is never edited;
// resolution is represented by this separate record plus an immutable
// subject-side resolution reference.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="adjudication spec is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.quarantineDigest) || has(self.blockedStateDigest)",message="adjudication requires quarantine or blocked-state evidence digests"
type AgentExecutionAdjudicationSpec struct {
	// +kubebuilder:validation:Required
	TaskRef AgentExecutionSubjectReference `json:"taskRef"`

	// +optional
	SessionRef *AgentExecutionSubjectReference `json:"sessionRef,omitempty"`

	// +kubebuilder:validation:Required
	ExpectedState AgentExecutionExpectedSubjectState `json:"expectedState"`

	// QuarantineDigest matches the immutable quarantine record being resolved.
	// +optional
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	QuarantineDigest string `json:"quarantineDigest,omitempty"`

	// BlockedStateDigest matches the blocked Session state being resolved.
	// +optional
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	BlockedStateDigest string `json:"blockedStateDigest,omitempty"`

	// +kubebuilder:validation:Required
	Action AgentExecutionAdjudicationAction `json:"action"`

	// EvidenceDigests reference independently verified receipts and evidence.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +listType=set
	// +kubebuilder:validation:items:Pattern=`^sha256:[a-f0-9]{64}$`
	EvidenceDigests []string `json:"evidenceDigests"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=4096
	Justification string `json:"justification"`

	// RequestedBy is the verified requester identity. Admission rejects a value
	// that does not match the authenticated caller.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	RequestedBy string `json:"requestedBy"`

	// ExpiresAt bounds how long an unapplied adjudication stays actionable.
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`
}

// AgentExecutionAdjudicationStatus is the controller-owned, idempotent
// application record.
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.state) || !(oldSelf.state in ['Applied','Rejected','Superseded']) || (has(self.state) && self.state == oldSelf.state)",message="terminal adjudication state is immutable"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.operationId) || (has(self.operationId) && self.operationId == oldSelf.operationId)",message="operationId is immutable once recorded"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.operationDigest) || (has(self.operationDigest) && self.operationDigest == oldSelf.operationDigest)",message="operationDigest is immutable once recorded"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.resolutionRefDigest) || (has(self.resolutionRefDigest) && self.resolutionRefDigest == oldSelf.resolutionRefDigest)",message="resolutionRefDigest is immutable once recorded"
type AgentExecutionAdjudicationStatus struct {
	// +optional
	State AgentExecutionAdjudicationState `json:"state,omitempty"`

	// OperationID identifies the exactly-once controller application operation.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	OperationID string `json:"operationId,omitempty"`

	// +optional
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	OperationDigest string `json:"operationDigest,omitempty"`

	// ResultingSubjectResourceVersion is the subject resourceVersion produced
	// by applying the decision.
	// +optional
	ResultingSubjectResourceVersion string `json:"resultingSubjectResourceVersion,omitempty"`

	// ResolutionRefDigest matches the immutable subject-side resolution
	// reference appended by application.
	// +optional
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	ResolutionRefDigest string `json:"resolutionRefDigest,omitempty"`

	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`

	// Message explains Rejected or Superseded outcomes.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=aexadj
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=`.spec.action`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
// +kubebuilder:printcolumn:name="Task",type=string,JSONPath=`.spec.taskRef.name`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AgentExecutionAdjudication is the namespaced, admin-authored, fail-closed
// resolution record for quarantined Tasks and blocked Sessions.
type AgentExecutionAdjudication struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentExecutionAdjudicationSpec   `json:"spec"`
	Status AgentExecutionAdjudicationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentExecutionAdjudicationList contains a list of AgentExecutionAdjudication.
type AgentExecutionAdjudicationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentExecutionAdjudication `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentExecutionAdjudication{}, &AgentExecutionAdjudicationList{})
}
