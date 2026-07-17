/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// ExecutionWorkspaceDesiredState is the core-requested lifecycle state.
// +kubebuilder:validation:Enum=Ready;Suspended;Deleted;Quarantined
type ExecutionWorkspaceDesiredState string

const (
	ExecutionWorkspaceDesiredReady       ExecutionWorkspaceDesiredState = "Ready"
	ExecutionWorkspaceDesiredSuspended   ExecutionWorkspaceDesiredState = "Suspended"
	ExecutionWorkspaceDesiredDeleted     ExecutionWorkspaceDesiredState = "Deleted"
	ExecutionWorkspaceDesiredQuarantined ExecutionWorkspaceDesiredState = "Quarantined"
)

// ExecutionWorkspaceState is the provider-observed generic lifecycle state.
// +kubebuilder:validation:Enum=Pending;Provisioning;Ready;Attaching;Attached;Detaching;Suspending;Suspended;Deleting;Deleted;Quarantined;Failed
type ExecutionWorkspaceState string

const (
	ExecutionWorkspaceStatePending      ExecutionWorkspaceState = "Pending"
	ExecutionWorkspaceStateProvisioning ExecutionWorkspaceState = "Provisioning"
	ExecutionWorkspaceStateReady        ExecutionWorkspaceState = "Ready"
	ExecutionWorkspaceStateAttaching    ExecutionWorkspaceState = "Attaching"
	ExecutionWorkspaceStateAttached     ExecutionWorkspaceState = "Attached"
	ExecutionWorkspaceStateDetaching    ExecutionWorkspaceState = "Detaching"
	ExecutionWorkspaceStateSuspending   ExecutionWorkspaceState = "Suspending"
	ExecutionWorkspaceStateSuspended    ExecutionWorkspaceState = "Suspended"
	ExecutionWorkspaceStateDeleting     ExecutionWorkspaceState = "Deleting"
	ExecutionWorkspaceStateDeleted      ExecutionWorkspaceState = "Deleted"
	ExecutionWorkspaceStateQuarantined  ExecutionWorkspaceState = "Quarantined"
	ExecutionWorkspaceStateFailed       ExecutionWorkspaceState = "Failed"
)

// ExecutionWorkspaceAttachment grants one Task exclusive, epoch-fenced access.
type ExecutionWorkspaceAttachment struct {
	// TaskRef identifies the attached Task.
	TaskRef ObjectIdentityReference `json:"taskRef"`

	// Epoch monotonically increases for every attachment attempt.
	// +kubebuilder:validation:Minimum=1
	Epoch int64 `json:"epoch"`

	// TokenSHA256 is the digest of the bearer token held only in tokenSecretRef.
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	TokenSHA256 string `json:"tokenSHA256"`

	// TokenSecretRef references the core-owned attachment Secret in this namespace.
	TokenSecretRef SecretReference `json:"tokenSecretRef"`

	// ExpiresAt is the hard attachment credential expiry.
	ExpiresAt metav1.Time `json:"expiresAt"`
}

// ExecutionWorkspaceServicePort declares a Service-mode port requested by the owning Tool.
type ExecutionWorkspaceServicePort struct {
	// Name is a stable endpoint name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// Port is the container/service port.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`

	// Protocol is the endpoint application protocol.
	// +kubebuilder:validation:Enum=HTTP;HTTPS;TCP
	Protocol string `json:"protocol"`
}

// ExecutionWorkspaceServiceSpec declares requested Service-mode endpoints.
type ExecutionWorkspaceServiceSpec struct {
	// Ports lists requested service ports.
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MinItems=1
	Ports []ExecutionWorkspaceServicePort `json:"ports"`
}

// ExecutionWorkspaceSpec defines one concrete provider-bound environment.
// +kubebuilder:validation:XValidation:rule="self.classBinding.profileHash.size() > 0",message="classBinding.profileHash is required"
// +kubebuilder:validation:XValidation:rule="self.mode == oldSelf.mode",message="mode is immutable"
// +kubebuilder:validation:XValidation:rule="self.classBinding == oldSelf.classBinding",message="classBinding is immutable"
// +kubebuilder:validation:XValidation:rule="self.providerBinding == oldSelf.providerBinding",message="providerBinding is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.sessionRef) == has(oldSelf.sessionRef) && (!has(self.sessionRef) || self.sessionRef == oldSelf.sessionRef)",message="sessionRef is immutable"
// +kubebuilder:validation:XValidation:rule="self.slot == oldSelf.slot",message="slot is immutable"
// +kubebuilder:validation:XValidation:rule="self.lifecycle == oldSelf.lifecycle",message="lifecycle is immutable"
// +kubebuilder:validation:XValidation:rule="!has(self.attachment) || self.mode == 'Interactive'",message="attachments are only valid for Interactive workspaces"
// +kubebuilder:validation:XValidation:rule="!has(self.service) || self.mode == 'Service'",message="service ports are only valid for Service workspaces"
// +kubebuilder:validation:XValidation:rule="self.mode != 'Service' || has(self.service)",message="Service workspaces require service configuration"
// +kubebuilder:validation:XValidation:rule="self.mode != 'Interactive' || !has(self.service)",message="Interactive workspaces cannot request service ports"
type ExecutionWorkspaceSpec struct {
	// Mode is copied from the class.
	Mode ExecutionWorkspaceMode `json:"mode"`

	// ClassBinding pins the immutable class revision used to create this workspace.
	ClassBinding ImmutableObjectBinding `json:"classBinding"`

	// ProviderBinding pins the provider installation used to create this workspace.
	ProviderBinding ImmutableObjectBinding `json:"providerBinding"`

	// SessionRef pins the conversation Session for session-scoped reuse.
	// +optional
	SessionRef *ObjectIdentityReference `json:"sessionRef,omitempty"`

	// Slot allows one Session to hold multiple independently named workspaces.
	// +kubebuilder:default=default
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Slot string `json:"slot"`

	// DesiredState is owned by Orka core.
	// +kubebuilder:default=Ready
	DesiredState ExecutionWorkspaceDesiredState `json:"desiredState"`

	// Lifecycle is the class policy resolved at creation time.
	Lifecycle ExecutionWorkspaceLifecycle `json:"lifecycle"`

	// Attachment grants exclusive Task access for Interactive mode.
	// +optional
	Attachment *ExecutionWorkspaceAttachment `json:"attachment,omitempty"`

	// Service declares requested endpoints for Service mode.
	// +optional
	Service *ExecutionWorkspaceServiceSpec `json:"service,omitempty"`
}

// ExecutionWorkspaceProviderBindingStatus records the concrete adapter/backend contract serving a workspace.
type ExecutionWorkspaceProviderBindingStatus struct {
	// ContractVersion is the selected generic provider contract.
	// +optional
	ContractVersion string `json:"contractVersion,omitempty"`

	// AdapterVersion is the adapter semantic version.
	// +optional
	AdapterVersion string `json:"adapterVersion,omitempty"`

	// AdapterDigest is the immutable adapter build digest.
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	// +optional
	AdapterDigest string `json:"adapterDigest,omitempty"`

	// BackendAPIVersion is the selected provider-native API version.
	// +optional
	BackendAPIVersion string `json:"backendAPIVersion,omitempty"`
}

// ExecutionWorkspaceEndpoint is sanitized endpoint metadata. Credentials and private connection material
// are always delivered through a Secret and never placed in status.
type ExecutionWorkspaceEndpoint struct {
	// Name matches a requested service port or the reserved data-plane name.
	Name string `json:"name"`

	// URL is the sanitized endpoint URL. It must not contain userinfo or credentials.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^(https?|tcp)://[^/:@?#[:space:]][^@?#[:space:]]*$`
	URL string `json:"url"`

	// Protocol is the endpoint application protocol.
	// +optional
	Protocol string `json:"protocol,omitempty"`
}

// ExecutionWorkspaceDispositionState reports progress for one cleanup category.
// +kubebuilder:validation:Enum=Pending;Active;Revoked;Retained;Deleted;Failed;NotApplicable
type ExecutionWorkspaceDispositionState string

const (
	DispositionPending       ExecutionWorkspaceDispositionState = "Pending"
	DispositionActive        ExecutionWorkspaceDispositionState = "Active"
	DispositionRevoked       ExecutionWorkspaceDispositionState = "Revoked"
	DispositionRetained      ExecutionWorkspaceDispositionState = "Retained"
	DispositionDeleted       ExecutionWorkspaceDispositionState = "Deleted"
	DispositionFailed        ExecutionWorkspaceDispositionState = "Failed"
	DispositionNotApplicable ExecutionWorkspaceDispositionState = "NotApplicable"
)

// ExecutionWorkspaceDisposition reports cleanup guarantees independently from Task execution outcome.
type ExecutionWorkspaceDisposition struct {
	Compute           ExecutionWorkspaceDispositionState `json:"compute"`
	AccessCredentials ExecutionWorkspaceDispositionState `json:"accessCredentials"`
	EphemeralSecrets  ExecutionWorkspaceDispositionState `json:"ephemeralSecrets"`
	WorkspaceData     ExecutionWorkspaceDispositionState `json:"workspaceData"`
	PersistentVolumes ExecutionWorkspaceDispositionState `json:"persistentVolumes"`
	Checkpoints       ExecutionWorkspaceDispositionState `json:"checkpoints"`
	ProviderResources ExecutionWorkspaceDispositionState `json:"providerResources"`
}

// ExecutionWorkspaceStatus defines adapter-observed state. Exactly one provider adapter owns this status;
// Orka core projects it into Task and Tool status rather than allowing adapters to write those resources.
type ExecutionWorkspaceStatus struct {
	// ObservedGeneration is the most recent spec generation observed by the adapter.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// State is the provider-neutral lifecycle state.
	// +optional
	State ExecutionWorkspaceState `json:"state,omitempty"`

	// ExternalID is a sanitized opaque provider resource identifier for operator diagnostics.
	// +optional
	ExternalID string `json:"externalID,omitempty"`

	// AttachedEpoch is the epoch currently enforced by the data plane.
	// +optional
	AttachedEpoch int64 `json:"attachedEpoch,omitempty"`

	// ProviderBinding records the exact adapter/backend contract serving this workspace.
	// +optional
	ProviderBinding *ExecutionWorkspaceProviderBindingStatus `json:"providerBinding,omitempty"`

	// ConnectionSecretRef contains endpoint, CA, and provider lifecycle credentials for trusted control-plane consumers.
	// +optional
	ConnectionSecretRef *SecretReference `json:"connectionSecretRef,omitempty"`

	// Endpoints contains sanitized endpoint metadata only.
	// +optional
	Endpoints []ExecutionWorkspaceEndpoint `json:"endpoints,omitempty"`

	// Disposition reports cleanup progress independent of owning Task/Tool terminal state.
	// +optional
	Disposition *ExecutionWorkspaceDisposition `json:"disposition,omitempty"`

	// Conditions represent admission, provisioning, data-plane, attachment, and finalization state.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=orka,shortName=ew
// +kubebuilder:printcolumn:name="Class",type=string,JSONPath=`.spec.classBinding.name`
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.spec.providerBinding.name`
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
// +kubebuilder:printcolumn:name="Epoch",type=integer,JSONPath=`.status.attachedEpoch`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ExecutionWorkspace represents one concrete provider-bound environment. It is controller-created.
type ExecutionWorkspace struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ExecutionWorkspaceSpec `json:"spec"`
	// +optional
	Status ExecutionWorkspaceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ExecutionWorkspaceList contains a list of ExecutionWorkspace objects.
type ExecutionWorkspaceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ExecutionWorkspace `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ExecutionWorkspace{}, &ExecutionWorkspaceList{})
}
