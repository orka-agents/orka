/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// RuntimeSessionControlLifecycle is the durable RuntimeSession lifecycle.
// +kubebuilder:validation:Enum=Creating;Idle;PromptRunning;Validating;PreparingPublication;PublicationPrepared;Publishing;Verifying;Finalizing;Cancelling;Poisoned;Deleting;Deleted
type RuntimeSessionControlLifecycle string

// RuntimeSessionControlAvailability gates the Session mutation lease.
// +kubebuilder:validation:Enum=Available;ReconciliationBlocked
type RuntimeSessionControlAvailability string

// RuntimeSessionLineageStatus is the Kubernetes-authoritative, append-once
// protocol/runtime identity for one conversation Session. Generation is
// independent from mutation-lease and ACP RuntimeSession generations.
type RuntimeSessionLineageStatus struct {
	// NamespaceUID prevents a same-name recreated namespace from attaching to
	// durable state owned by the previous namespace identity.
	NamespaceUID types.UID `json:"namespaceUID"`

	// SessionUID repeats the immutable control identity at the lineage fence.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	SessionUID string `json:"sessionUid"`

	ContractVersion AgentRuntimeContractVersion `json:"contractVersion"`

	// +kubebuilder:validation:Minimum=1
	Generation int64 `json:"generation"`

	// RuntimeIdentity is the built-in runtime type or AgentRuntime UID.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	RuntimeIdentity string `json:"runtimeIdentity"`

	// ConfigDigest freezes the configuration/execution-snapshot identity used
	// when the lineage was established.
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	ConfigDigest string `json:"configDigest"`

	EstablishedAt metav1.Time `json:"establishedAt"`
}

// RuntimeSessionControlSpec contains immutable session identity, ownership, and
// profile bindings. Profile changes create a new session generation in status;
// they do not mutate this immutable record identity.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="runtime session control spec is immutable"
type RuntimeSessionControlSpec struct {
	// SessionName is the immutable user-visible Session key within the object
	// namespace. The Kubernetes object name is a digest-derived storage key and
	// must not be treated as the Session name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	SessionName string `json:"sessionName"`

	// SessionUID is the immutable Orka Session identity.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	SessionUID string `json:"sessionUid"`

	// RequestDigest binds creation to exact canonical input.
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	RequestDigest string `json:"requestDigest"`

	// Owner identifies the immutable Task or durable Session owner.
	Owner ControlRecordOwner `json:"owner"`

	// RuntimePoolRef is the controller-owned logical pool name when known.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	RuntimePoolRef string `json:"runtimePoolRef,omitempty"`

	// RuntimePoolUID fences the pool object across delete/recreate.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	RuntimePoolUID string `json:"runtimePoolUid,omitempty"`

	// RuntimeProfileDigest binds the session to immutable runtime behavior.
	// +optional
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	RuntimeProfileDigest string `json:"runtimeProfileDigest,omitempty"`

	// ProfileDigestSchemaVersion identifies how RuntimeProfileDigest was built.
	// +optional
	// +kubebuilder:validation:MaxLength=64
	ProfileDigestSchemaVersion string `json:"profileDigestSchemaVersion,omitempty"`
}

// RuntimeSessionMutationLeaseStatus mirrors the namespaced Kubernetes Lease
// that serializes mutation for one immutable SessionUID.
type RuntimeSessionMutationLeaseStatus struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	LeaseName string `json:"leaseName"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	LeaseResourceVersion string `json:"leaseResourceVersion"`

	// +kubebuilder:validation:Minimum=1
	Generation int64 `json:"generation"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	TaskUID string `json:"taskUid"`

	// +kubebuilder:validation:Minimum=1
	Attempt int64 `json:"attempt"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	PromptID string `json:"promptId"`

	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	RequestDigest string `json:"requestDigest"`

	AcquiredAt metav1.Time `json:"acquiredAt"`

	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`
}

// RuntimeSessionResidentRuntimeState is the lifecycle of an ADR 0059 handoff
// record.
// +kubebuilder:validation:Enum=Resident;Claimed;Retiring
type RuntimeSessionResidentRuntimeState string

// RuntimeSessionResidentRuntimeClaim names the turn whose lease acquisition
// claimed a resident runtime.
type RuntimeSessionResidentRuntimeClaim struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	TaskUID string `json:"taskUid"`

	// +kubebuilder:validation:Minimum=1
	Attempt int64 `json:"attempt"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	PromptID string `json:"promptId"`

	// +kubebuilder:validation:Minimum=1
	LeaseGeneration int64 `json:"leaseGeneration"`
}

// RuntimeSessionResidentRuntimeStatus is the ADR 0059 handoff record for the
// one live ACP RuntimeSession a Session keeps across a turn boundary. It holds
// identities and digests only, never a credential.
type RuntimeSessionResidentRuntimeStatus struct {
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	HandoffID string `json:"handoffId"`

	// +kubebuilder:validation:MaxLength=1024
	SourceTaskUID string `json:"sourceTaskUid"`
	// +kubebuilder:validation:Minimum=1
	SourceAttempt int64 `json:"sourceAttempt"`
	// +kubebuilder:validation:MaxLength=1024
	SourcePromptAttemptID string `json:"sourcePromptAttemptId"`
	// +kubebuilder:validation:MaxLength=1024
	SourceTurnID string `json:"sourceTurnId"`
	// +kubebuilder:validation:Minimum=1
	SourceLeaseGeneration int64 `json:"sourceLeaseGeneration"`

	// +kubebuilder:validation:MaxLength=253
	ControllerEpochName string `json:"controllerEpochName"`
	// +kubebuilder:validation:Minimum=1
	ControllerEpoch int64 `json:"controllerEpoch"`

	// +kubebuilder:validation:MaxLength=253
	WorkspaceName string `json:"workspaceName"`
	// +kubebuilder:validation:MaxLength=1024
	WorkspaceUID string `json:"workspaceUid"`
	// +kubebuilder:validation:Minimum=1
	WorkspaceAttachmentEpoch int64 `json:"workspaceAttachmentEpoch"`

	// +kubebuilder:validation:MaxLength=253
	RuntimePoolName string `json:"runtimePoolName"`
	// +kubebuilder:validation:MaxLength=1024
	RuntimePoolUID string `json:"runtimePoolUid"`
	// +kubebuilder:validation:Minimum=1
	RuntimePoolGeneration int64 `json:"runtimePoolGeneration"`

	// +kubebuilder:validation:MaxLength=1024
	RuntimeInstanceID string `json:"runtimeInstanceId"`
	// +kubebuilder:validation:MaxLength=1024
	SupervisorBootID string `json:"supervisorBootId"`
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	ServedRecordDigest string `json:"servedRecordDigest"`

	// +kubebuilder:validation:MaxLength=1024
	RuntimeSessionUID string `json:"runtimeSessionUid"`
	// +kubebuilder:validation:Minimum=1
	RuntimeSessionGeneration int64 `json:"runtimeSessionGeneration"`
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	ProfileDigest string `json:"profileDigest"`
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	MCPDigest string `json:"mcpDigest"`
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	WorkspaceDigest string `json:"workspaceDigest"`
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	LineageConfigDigest string `json:"lineageConfigDigest"`
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	ReuseDigest string `json:"reuseDigest"`
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	TranscriptHeadDigest string `json:"transcriptHeadDigest"`

	// +optional
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	RebasedTreeDigest string `json:"rebasedTreeDigest,omitempty"`
	// +optional
	VerifiedBaseline *ControlVerifiedBranchBaseline `json:"verifiedBaseline,omitempty"`
	// +optional
	PromptDelivered bool `json:"promptDelivered,omitempty"`

	KeptAt    metav1.Time `json:"keptAt"`
	ExpiresAt metav1.Time `json:"expiresAt"`

	State RuntimeSessionResidentRuntimeState `json:"state"`
	// +optional
	Claim *RuntimeSessionResidentRuntimeClaim `json:"claim,omitempty"`
}

// RuntimeSessionControlStatus contains the lifecycle, generation, mutation
// Lease, and independently verified recovery baseline.
// +kubebuilder:validation:XValidation:rule="!has(self.availability) || self.availability != 'Available' || ((!has(self.blockedReason) || size(self.blockedReason) == 0) && (!has(self.relatedPromptAttemptId) || size(self.relatedPromptAttemptId) == 0) && (!has(self.relatedPublicationId) || size(self.relatedPublicationId) == 0))",message="available sessions must clear reconciliation block metadata"
// +kubebuilder:validation:XValidation:rule="!has(self.availability) || self.availability != 'ReconciliationBlocked' || (has(self.blockedReason) && size(self.blockedReason) > 0)",message="reconciliation-blocked sessions require a reason"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.lineage) || (has(self.lineage) && self.lineage == oldSelf.lineage)",message="runtime Session lineage is append-once and immutable"
type RuntimeSessionControlStatus struct {
	// Generation is the monotonic ACP RuntimeSession generation.
	// +optional
	// +kubebuilder:validation:Minimum=1
	Generation int64 `json:"generation,omitempty"`

	// +optional
	Lifecycle RuntimeSessionControlLifecycle `json:"lifecycle,omitempty"`

	// +optional
	Availability RuntimeSessionControlAvailability `json:"availability,omitempty"`

	// MutationLeaseGeneration is monotonic and never reused for SessionUID.
	// +optional
	// +kubebuilder:validation:Minimum=0
	MutationLeaseGeneration int64 `json:"mutationLeaseGeneration,omitempty"`

	// +optional
	MutationLease *RuntimeSessionMutationLeaseStatus `json:"mutationLease,omitempty"`

	// +optional
	// +kubebuilder:validation:MaxLength=16384
	BlockedReason string `json:"blockedReason,omitempty"`

	// +optional
	// +kubebuilder:validation:MaxLength=1024
	RelatedPromptAttemptID string `json:"relatedPromptAttemptId,omitempty"`

	// +optional
	// +kubebuilder:validation:MaxLength=1024
	RelatedPublicationID string `json:"relatedPublicationId,omitempty"`

	// +optional
	VerifiedBaseline *ControlVerifiedBranchBaseline `json:"verifiedBaseline,omitempty"`

	// Lineage is established or verified in the same RuntimeSessionControl
	// status CAS that mirrors the authoritative mutation Lease.
	// +optional
	Lineage *RuntimeSessionLineageStatus `json:"lineage,omitempty"`

	// ResidentRuntime is the ADR 0059 handoff record, when one turn kept its
	// live RuntimeSession for the next.
	// +optional
	ResidentRuntime *RuntimeSessionResidentRuntimeStatus `json:"residentRuntime,omitempty"`

	ControlRecordMutationStatus `json:",inline"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=rsctrl
// +kubebuilder:printcolumn:name="Lifecycle",type=string,JSONPath=`.status.lifecycle`
// +kubebuilder:printcolumn:name="Availability",type=string,JSONPath=`.status.availability`
// +kubebuilder:printcolumn:name="Generation",type=integer,JSONPath=`.status.generation`
// +kubebuilder:printcolumn:name="Lease",type=integer,JSONPath=`.status.mutationLeaseGeneration`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="!has(self.status) || !has(self.status.lineage) || self.status.lineage.sessionUid == self.spec.sessionUid",message="runtime Session lineage UID must match the immutable control Session UID"

// RuntimeSessionControl is the Kubernetes-authoritative RuntimeSession control
// record. SessionTurn/transcript/deferred-outbox data remains in one durable
// SQLite transaction; the Kubernetes store completes the authoritative
// SessionControl/BranchClaim CAS before activating the terminal projection.
type RuntimeSessionControl struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RuntimeSessionControlSpec   `json:"spec"`
	Status RuntimeSessionControlStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RuntimeSessionControlList contains a list of RuntimeSessionControl.
type RuntimeSessionControlList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RuntimeSessionControl `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RuntimeSessionControl{}, &RuntimeSessionControlList{})
}
