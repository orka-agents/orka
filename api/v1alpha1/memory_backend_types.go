/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	// MemoryBackendDefaultName is the only supported MemoryBackend resource name.
	MemoryBackendDefaultName = "default"

	// MemoryBackendSchemaAnnotation identifies the generated CRD schema contract.
	MemoryBackendSchemaAnnotation = "memory.orka.ai/schema-version"
	// MemoryBackendSchemaVersion is the current MemoryBackend CRD schema contract.
	MemoryBackendSchemaVersion = "v1alpha1"

	// MemoryBackendLifecycleIntentAnnotation binds a protected lifecycle request to
	// its durable, generation-specific administrative intent.
	MemoryBackendLifecycleIntentAnnotation = "memory.orka.ai/lifecycle-intent"
	// MemoryBackendValidationCandidateAnnotation stores the complete non-secret
	// route snapshot that must be used to retire a possibly claimed candidate.
	MemoryBackendValidationCandidateAnnotation = "memory.orka.ai/validation-candidate"
)

// MemoryBackendOMSVersion identifies the OMS API version selected by a backend.
// +kubebuilder:validation:Enum="0.1"
type MemoryBackendOMSVersion string

const (
	// MemoryBackendOMSVersionV01 is the initial OMS API version.
	MemoryBackendOMSVersionV01 MemoryBackendOMSVersion = "0.1"
)

// MemoryBackendProfile identifies the closed Orka OMS profile.
// +kubebuilder:validation:Enum=orka.oms.v0alpha1
type MemoryBackendProfile string

const (
	// MemoryBackendProfileV0Alpha1 is the initial strict Orka OMS profile.
	MemoryBackendProfileV0Alpha1 MemoryBackendProfile = "orka.oms.v0alpha1"
)

// MemoryBackendDeploymentMode selects how Orka reaches the OMS adapter.
// +kubebuilder:validation:Enum=external-endpoint
type MemoryBackendDeploymentMode string

const (
	// MemoryBackendDeploymentModeExternalEndpoint points at a public HTTPS OMS endpoint.
	MemoryBackendDeploymentModeExternalEndpoint MemoryBackendDeploymentMode = "external-endpoint"
)

// MemoryBackendLifecycleState is the operator-requested lifecycle state.
// +kubebuilder:validation:Enum=Staged;Active;ReadOnly;Disabled;Decommissioning
type MemoryBackendLifecycleState string

const (
	// MemoryBackendLifecycleStaged validates a backend without changing the effective authority.
	MemoryBackendLifecycleStaged MemoryBackendLifecycleState = "Staged"
	// MemoryBackendLifecycleActive requests a durable cutover to a writable remote backend.
	MemoryBackendLifecycleActive MemoryBackendLifecycleState = "Active"
	// MemoryBackendLifecycleReadOnly permits remote reads after the durable drain barrier.
	MemoryBackendLifecycleReadOnly MemoryBackendLifecycleState = "ReadOnly"
	// MemoryBackendLifecycleDisabled fences remote admission and dispatch.
	MemoryBackendLifecycleDisabled MemoryBackendLifecycleState = "Disabled"
	// MemoryBackendLifecycleDecommissioning runs the explicit durable removal workflow.
	MemoryBackendLifecycleDecommissioning MemoryBackendLifecycleState = "Decommissioning"
)

// MemoryBackendEffectiveLifecycleState is the controller-observed durable lifecycle state.
// +kubebuilder:validation:Enum=Staged;Validating;Active;ReadOnly;Disabled;Decommissioning;Draining;Recovering;IdentityMismatch;IdentityConflict;Diverged;Removed;Decommissioned
type MemoryBackendEffectiveLifecycleState string

const (
	MemoryBackendEffectiveLifecycleStaged           MemoryBackendEffectiveLifecycleState = "Staged"
	MemoryBackendEffectiveLifecycleValidating       MemoryBackendEffectiveLifecycleState = "Validating"
	MemoryBackendEffectiveLifecycleActive           MemoryBackendEffectiveLifecycleState = "Active"
	MemoryBackendEffectiveLifecycleReadOnly         MemoryBackendEffectiveLifecycleState = "ReadOnly"
	MemoryBackendEffectiveLifecycleDisabled         MemoryBackendEffectiveLifecycleState = "Disabled"
	MemoryBackendEffectiveLifecycleDecommissioning  MemoryBackendEffectiveLifecycleState = "Decommissioning"
	MemoryBackendEffectiveLifecycleDraining         MemoryBackendEffectiveLifecycleState = "Draining"
	MemoryBackendEffectiveLifecycleRecovering       MemoryBackendEffectiveLifecycleState = "Recovering"
	MemoryBackendEffectiveLifecycleIdentityMismatch MemoryBackendEffectiveLifecycleState = "IdentityMismatch"
	MemoryBackendEffectiveLifecycleIdentityConflict MemoryBackendEffectiveLifecycleState = "IdentityConflict"
	MemoryBackendEffectiveLifecycleDiverged         MemoryBackendEffectiveLifecycleState = "Diverged"
	MemoryBackendEffectiveLifecycleRemoved          MemoryBackendEffectiveLifecycleState = "Removed"
	MemoryBackendEffectiveLifecycleDecommissioned   MemoryBackendEffectiveLifecycleState = "Decommissioned"
)

// MemoryBackendCapability is a closed capability advertised by the Orka OMS profile.
// +kubebuilder:validation:Enum=durableIdempotency;idempotencyDigestConflicts;createIfAbsent;conditionalMutation;monotonicGenerations;deleteHighWatermark;durableRoutingFence;operationLookup;exactGet;stablePagination;exclusiveOwnership;keywordSearch;auditVersionVisibility;semanticSearch;hybridSearch
type MemoryBackendCapability string

const (
	MemoryBackendCapabilityDurableIdempotency         MemoryBackendCapability = "durableIdempotency"
	MemoryBackendCapabilityIdempotencyDigestConflicts MemoryBackendCapability = "idempotencyDigestConflicts"
	MemoryBackendCapabilityCreateIfAbsent             MemoryBackendCapability = "createIfAbsent"
	MemoryBackendCapabilityConditionalMutation        MemoryBackendCapability = "conditionalMutation"
	MemoryBackendCapabilityMonotonicGenerations       MemoryBackendCapability = "monotonicGenerations"
	MemoryBackendCapabilityDeleteHighWatermark        MemoryBackendCapability = "deleteHighWatermark"
	MemoryBackendCapabilityDurableRoutingFence        MemoryBackendCapability = "durableRoutingFence"
	MemoryBackendCapabilityOperationLookup            MemoryBackendCapability = "operationLookup"
	MemoryBackendCapabilityExactGet                   MemoryBackendCapability = "exactGet"
	MemoryBackendCapabilityStablePagination           MemoryBackendCapability = "stablePagination"
	MemoryBackendCapabilityExclusiveOwnership         MemoryBackendCapability = "exclusiveOwnership"
	MemoryBackendCapabilityKeywordSearch              MemoryBackendCapability = "keywordSearch"
	MemoryBackendCapabilityAuditVersionVisibility     MemoryBackendCapability = "auditVersionVisibility"
	MemoryBackendCapabilitySemanticSearch             MemoryBackendCapability = "semanticSearch"
	MemoryBackendCapabilityHybridSearch               MemoryBackendCapability = "hybridSearch"
)

// MemoryBackendConditionType identifies a status condition owned by the controller.
type MemoryBackendConditionType string

const (
	MemoryBackendConditionAccepted               MemoryBackendConditionType = "Accepted"
	MemoryBackendConditionProtected              MemoryBackendConditionType = "Protected"
	MemoryBackendConditionResolvedRefs           MemoryBackendConditionType = "ResolvedRefs"
	MemoryBackendConditionConnected              MemoryBackendConditionType = "Connected"
	MemoryBackendConditionCapabilitiesCompatible MemoryBackendConditionType = "CapabilitiesCompatible"
	MemoryBackendConditionFresh                  MemoryBackendConditionType = "Fresh"
	MemoryBackendConditionReady                  MemoryBackendConditionType = "Ready"
	MemoryBackendConditionDeletionSafe           MemoryBackendConditionType = "DeletionSafe"
)

// MemoryBackendProtocol selects the exact OMS wire contract.
type MemoryBackendProtocol struct {
	// OMSVersion is the adapter API version.
	OMSVersion MemoryBackendOMSVersion `json:"omsVersion"`

	// Profile is the closed Orka OMS profile implemented by the adapter.
	Profile MemoryBackendProfile `json:"profile"`
}

// MemoryBackendDeployment configures the external OMS adapter endpoint.
type MemoryBackendDeployment struct {
	// Mode is the endpoint deployment mode.
	Mode MemoryBackendDeploymentMode `json:"mode"`

	// Endpoint is a public HTTPS base URL. Credentials, queries, fragments, local
	// names, private addresses, and Kubernetes Service names are rejected at runtime.
	// +kubebuilder:validation:MinLength=9
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^https://[^\s@?#]+$`
	Endpoint string `json:"endpoint"`
}

// MemoryBackendBearerTokenSecretReference identifies the Secret key containing the OMS bearer token.
type MemoryBackendBearerTokenSecretReference struct {
	// Name is the same-namespace Secret name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	Name string `json:"name"`

	// Key is the Secret data key containing the bearer token.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[-._a-zA-Z0-9]+$`
	Key string `json:"key"`
}

// MemoryBackendClientAuth configures Orka-to-adapter authentication.
type MemoryBackendClientAuth struct {
	// BearerTokenSecretRef references a Secret explicitly bound to this backend,
	// namespace incarnation, endpoint identity, tenant identity, and store.
	BearerTokenSecretRef MemoryBackendBearerTokenSecretReference `json:"bearerTokenSecretRef"`
}

// MemoryBackendStore identifies an operator-precreated provider store.
type MemoryBackendStore struct {
	// Name is the immutable provider store name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`
	Name string `json:"name"`
}

// +kubebuilder:validation:XValidation:rule="self.protocol == oldSelf.protocol",message="spec.protocol is immutable"
// +kubebuilder:validation:XValidation:rule="self.store.name == oldSelf.store.name",message="spec.store.name is immutable"

// MemoryBackendSpec defines a namespace's requested external memory backend.
type MemoryBackendSpec struct {
	Protocol   MemoryBackendProtocol   `json:"protocol"`
	Deployment MemoryBackendDeployment `json:"deployment"`
	ClientAuth MemoryBackendClientAuth `json:"clientAuth"`
	Store      MemoryBackendStore      `json:"store"`

	// LifecycleState is requested intent. Effective authority changes only after
	// the durable binding coordinator completes the corresponding barrier.
	// +kubebuilder:default:=Staged
	LifecycleState MemoryBackendLifecycleState `json:"lifecycleState,omitempty"`
}

// RequestedLifecycle returns the schema default for clients that have not passed
// through Kubernetes defaulting (for example, unit-test or migration readers).
func (s MemoryBackendSpec) RequestedLifecycle() MemoryBackendLifecycleState {
	if s.LifecycleState == "" {
		return MemoryBackendLifecycleStaged
	}
	return s.LifecycleState
}

// MemoryBackendCapabilityLimits contains bounded provider-advertised limits.
type MemoryBackendCapabilityLimits struct {
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=2097152
	MaxRequestBytes int64 `json:"maxRequestBytes"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=4194304
	MaxResponseBytes int64 `json:"maxResponseBytes"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=262144
	MaxContentBytes int64 `json:"maxContentBytes"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=64
	MaxTags int32 `json:"maxTags"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=128
	MaxTagBytes int32 `json:"maxTagBytes"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=32
	MaxMetadataEntries int32 `json:"maxMetadataEntries"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=64
	MaxMetadataKeyBytes int32 `json:"maxMetadataKeyBytes"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1024
	MaxMetadataValueBytes int32 `json:"maxMetadataValueBytes"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1024
	MaxQueryBytes int32 `json:"maxQueryBytes"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=8
	MaxPageSize int32 `json:"maxPageSize"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1024
	MaxSnapshotRecords int32 `json:"maxSnapshotRecords"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=86400
	SnapshotTTLSeconds int32 `json:"snapshotTtlSeconds"`
}

// MemoryBackendObservedCapabilities records strict, non-secret OMS capability metadata.
type MemoryBackendObservedCapabilities struct {
	// AdapterName is the bounded adapter implementation identity.
	// +kubebuilder:validation:MaxLength=256
	AdapterName string `json:"adapterName"`

	// AdapterVersion is the bounded adapter build/version identity.
	// +kubebuilder:validation:MaxLength=256
	AdapterVersion string `json:"adapterVersion"`

	// Revision is the adapter's stable capability-set revision.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Revision string `json:"revision"`

	// Native is the closed capability set advertised by the adapter.
	// +listType=set
	// +kubebuilder:validation:MaxItems=32
	Native []MemoryBackendCapability `json:"native"`

	// Effective is the capability set Orka can safely use.
	// +listType=set
	// +kubebuilder:validation:MaxItems=32
	Effective []MemoryBackendCapability `json:"effective"`

	Limits MemoryBackendCapabilityLimits `json:"limits"`

	// ExpiresAt is the adapter-declared capability expiry.
	ExpiresAt metav1.Time `json:"expiresAt"`
}

// MemoryBackendStatus contains bounded, non-secret control-plane observations.
type MemoryBackendStatus struct {
	// Accepted reports whether the current object shape and fixed-name contract are valid.
	Accepted bool `json:"accepted,omitempty"`
	// Protected reports whether the deletion-protection finalizer is persisted.
	Protected bool `json:"protected,omitempty"`
	// ResolvedRefs reports whether the namespace and bound Secret were freshly resolved.
	ResolvedRefs bool `json:"resolvedRefs,omitempty"`
	// Connected reports whether strict OMS store, capability, and ownership probes succeeded.
	Connected bool `json:"connected,omitempty"`
	// Ready reports whether the requested lifecycle is safely usable. Staged readiness
	// means validation succeeded while legacy SQLite remains authoritative.
	Ready bool `json:"ready,omitempty"`

	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// EffectiveLifecycleState changes only after the durable lifecycle barrier completes.
	EffectiveLifecycleState MemoryBackendEffectiveLifecycleState `json:"effectiveLifecycleState,omitempty"`

	// ValidatedSpecDigest is the canonical digest of the validated spec.
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	ValidatedSpecDigest string `json:"validatedSpecDigest,omitempty"`
	// ClusterIdentityDigest records only a digest of the configured cluster identity.
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	ClusterIdentityDigest string `json:"clusterIdentityDigest,omitempty"`

	// +kubebuilder:validation:MaxLength=128
	NamespaceUID string `json:"namespaceUID,omitempty"`
	// +kubebuilder:validation:MaxLength=128
	BackendUID string `json:"backendUID,omitempty"`
	// +kubebuilder:validation:Minimum=0
	AuthorityEpoch int64 `json:"authorityEpoch,omitempty"`
	// +kubebuilder:validation:Minimum=0
	RoutingEpoch int64 `json:"routingEpoch,omitempty"`

	// +kubebuilder:validation:MaxLength=128
	SecretUID string `json:"secretUID,omitempty"`
	// +kubebuilder:validation:MaxLength=128
	SecretResourceVersion string `json:"secretResourceVersion,omitempty"`

	// EndpointIdentity is the canonical, credential-free HTTPS endpoint identity.
	// +kubebuilder:validation:MaxLength=2048
	EndpointIdentity string `json:"endpointIdentity,omitempty"`
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	EndpointDigest string `json:"endpointDigest,omitempty"`
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	ResolvedAddressDigest string `json:"resolvedAddressDigest,omitempty"`
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	ServerCertificateDigest string `json:"serverCertificateDigest,omitempty"`

	// +kubebuilder:validation:MaxLength=128
	StoreUUID string `json:"storeUUID,omitempty"`
	// +kubebuilder:validation:MaxLength=256
	OwnershipClaimIdentity string `json:"ownershipClaimIdentity,omitempty"`

	ObservedCapabilities *MemoryBackendObservedCapabilities `json:"observedCapabilities,omitempty"`

	LastValidated       *metav1.Time `json:"lastValidated,omitempty"`
	ValidationExpiresAt *metav1.Time `json:"validationExpiresAt,omitempty"`

	// Reason is a stable, sanitized controller reason code.
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[A-Za-z][A-Za-z0-9]*$`
	Reason string `json:"reason,omitempty"`
	// Message is bounded and contains no bearer values or provider response content.
	// +kubebuilder:validation:MaxLength=1024
	Message string `json:"message,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=16
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:metadata:annotations=memory.orka.ai/schema-version=v1alpha1
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'default'",message="MemoryBackend metadata.name must be default"
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,categories=orka,shortName=mb
// +kubebuilder:printcolumn:name="Requested",type=string,JSONPath=`.spec.lifecycleState`
// +kubebuilder:printcolumn:name="Effective",type=string,JSONPath=`.status.effectiveLifecycleState`
// +kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=`.status.ready`
// +kubebuilder:printcolumn:name="Store",type=string,JSONPath=`.spec.store.name`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// MemoryBackend is the namespaced control-plane binding for external OMS memory.
type MemoryBackend struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MemoryBackendSpec   `json:"spec"`
	Status MemoryBackendStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// MemoryBackendList contains MemoryBackend objects.
type MemoryBackendList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MemoryBackend `json:"items"`
}

// DeepCopyInto manually keeps this source-only slice buildable before the shared
// generation owner refreshes zz_generated.deepcopy.go.
func (in *MemoryBackend) DeepCopyInto(out *MemoryBackend) {
	*out = *in
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Status.deepCopyInto(&out.Status)
}

// DeepCopy copies a MemoryBackend.
func (in *MemoryBackend) DeepCopy() *MemoryBackend {
	if in == nil {
		return nil
	}
	out := new(MemoryBackend)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject copies a MemoryBackend as a runtime.Object.
func (in *MemoryBackend) DeepCopyObject() runtime.Object {
	if copy := in.DeepCopy(); copy != nil {
		return copy
	}
	return nil
}

// DeepCopyInto manually keeps this source-only slice buildable before generation.
func (in *MemoryBackendList) DeepCopyInto(out *MemoryBackendList) {
	*out = *in
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]MemoryBackend, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

// DeepCopy copies a MemoryBackendList.
func (in *MemoryBackendList) DeepCopy() *MemoryBackendList {
	if in == nil {
		return nil
	}
	out := new(MemoryBackendList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject copies a MemoryBackendList as a runtime.Object.
func (in *MemoryBackendList) DeepCopyObject() runtime.Object {
	if copy := in.DeepCopy(); copy != nil {
		return copy
	}
	return nil
}

func (in *MemoryBackendStatus) deepCopyInto(out *MemoryBackendStatus) {
	*out = *in
	if in.ObservedCapabilities != nil {
		out.ObservedCapabilities = &MemoryBackendObservedCapabilities{
			Revision:       in.ObservedCapabilities.Revision,
			AdapterName:    in.ObservedCapabilities.AdapterName,
			AdapterVersion: in.ObservedCapabilities.AdapterVersion,
			Limits:         in.ObservedCapabilities.Limits,
			ExpiresAt:      *in.ObservedCapabilities.ExpiresAt.DeepCopy(),
		}
		out.ObservedCapabilities.Native = append([]MemoryBackendCapability(nil), in.ObservedCapabilities.Native...)
		out.ObservedCapabilities.Effective = append([]MemoryBackendCapability(nil), in.ObservedCapabilities.Effective...)
	}
	if in.LastValidated != nil {
		out.LastValidated = in.LastValidated.DeepCopy()
	}
	if in.ValidationExpiresAt != nil {
		out.ValidationExpiresAt = in.ValidationExpiresAt.DeepCopy()
	}
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

func init() {
	SchemeBuilder.Register(&MemoryBackend{}, &MemoryBackendList{})
}
