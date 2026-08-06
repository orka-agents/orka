/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AgentExecutionRetryEligibility bounds which v1 workloads may retry.
// +kubebuilder:validation:Enum=none;duplicate-safe-only
type AgentExecutionRetryEligibility string

const (
	// AgentExecutionRetryNone forbids every v1 retry under this policy.
	AgentExecutionRetryNone AgentExecutionRetryEligibility = "none"
	// AgentExecutionRetryDuplicateSafeOnly permits retry only after a
	// definitive pre-submission rejection or definitive retryable terminal
	// failure of a workload whose immutable snapshot is duplicate-safe.
	AgentExecutionRetryDuplicateSafeOnly AgentExecutionRetryEligibility = "duplicate-safe-only"
)

// AgentExecutionProhibitedField names one field family that stays rejected for
// new v1 bindings. The mandatory floors cannot be removed from a policy.
// +kubebuilder:validation:Enum=workspace-credentials;forge-credentials;direct-publication;transaction-tokens;observed-write-tools
type AgentExecutionProhibitedField string

const (
	AgentExecutionProhibitWorkspaceCredentials AgentExecutionProhibitedField = "workspace-credentials"
	AgentExecutionProhibitForgeCredentials     AgentExecutionProhibitedField = "forge-credentials"
	AgentExecutionProhibitDirectPublication    AgentExecutionProhibitedField = "direct-publication"
	AgentExecutionProhibitTransactionTokens    AgentExecutionProhibitedField = "transaction-tokens"
	AgentExecutionProhibitObservedWriteTools   AgentExecutionProhibitedField = "observed-write-tools"
)

// AgentExecutionNetworkIsolationProfile is the wrapper network posture
// required for workloads admitted under a policy.
// +kubebuilder:validation:Enum=default-deny;per-trust-domain
type AgentExecutionNetworkIsolationProfile string

const (
	// AgentExecutionNetworkIsolationDefaultDeny requires default-deny ingress,
	// narrowly scoped controller ingress, and documented provider/SCM egress on
	// a shared wrapper.
	AgentExecutionNetworkIsolationDefaultDeny AgentExecutionNetworkIsolationProfile = "default-deny"
	// AgentExecutionNetworkIsolationPerTrustDomain additionally requires one
	// wrapper per trust domain when isolation cannot otherwise be proven.
	AgentExecutionNetworkIsolationPerTrustDomain AgentExecutionNetworkIsolationProfile = "per-trust-domain"
)

// AgentExecutionPolicySpec is the admin-owned compatibility policy that new
// v1 bindings must be evaluated against. The evaluated policy UID, generation,
// and digest are frozen into each Task binding; later policy changes never
// broaden an existing binding.
// +kubebuilder:validation:XValidation:rule="['workspace-credentials','forge-credentials','direct-publication','transaction-tokens'].all(f, f in self.prohibitedFields)",message="prohibitedFields must retain the mandatory fail-closed floors"
// +kubebuilder:validation:XValidation:rule="!has(self.allowedBuiltInRuntimeTypes) || !('opencode' in self.allowedBuiltInRuntimeTypes)",message="new v1 OpenCode bindings are prohibited; legacy v1 OpenCode is sealed-inventory adoption only"
type AgentExecutionPolicySpec struct {
	// AllowNewV1Bindings gates creation of new v1 execution bindings in this
	// trust domain. Adopted legacy and cleanup-only work is unaffected.
	// +kubebuilder:validation:Required
	AllowNewV1Bindings bool `json:"allowNewV1Bindings"`

	// AllowedBuiltInRuntimeTypes lists built-in runtime types admitted for new
	// v1 bindings. Empty or omitted allows none.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=8
	AllowedBuiltInRuntimeTypes []AgentRuntimeType `json:"allowedBuiltInRuntimeTypes,omitempty"`

	// AllowPublicReadOnlyWorkspaces admits v1 workloads whose workspace is a
	// public repository technically mounted read-only with write tools disabled
	// and publication-blocking network policy.
	// +optional
	AllowPublicReadOnlyWorkspaces bool `json:"allowPublicReadOnlyWorkspaces,omitempty"`

	// AllowTrustedObservedModeRuntimes admits external v1 runtimes that execute
	// tools in observed mode. Such runtimes are trusted/non-governed and never
	// satisfy strict-governance claims.
	// +optional
	AllowTrustedObservedModeRuntimes bool `json:"allowTrustedObservedModeRuntimes,omitempty"`

	// AllowedBrokeredToolClasses lists tool classes Orka may broker for v1
	// runtimes. Empty or omitted allows none.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=8
	AllowedBrokeredToolClasses []AgentRuntimeBrokeredToolClass `json:"allowedBrokeredToolClasses,omitempty"`

	// RetryEligibility bounds v1 retry behavior for workloads under this policy.
	// +kubebuilder:validation:Required
	RetryEligibility AgentExecutionRetryEligibility `json:"retryEligibility"`

	// ProhibitedFields records the field families rejected for new v1 bindings.
	// The mandatory floors are enforced structurally and cannot be removed.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=4
	// +kubebuilder:validation:MaxItems=8
	// +listType=set
	ProhibitedFields []AgentExecutionProhibitedField `json:"prohibitedFields"`

	// NetworkIsolationProfile is the wrapper isolation posture required for
	// workloads admitted under this policy.
	// +kubebuilder:validation:Required
	NetworkIsolationProfile AgentExecutionNetworkIsolationProfile `json:"networkIsolationProfile"`
}

// AgentExecutionPolicyStatus publishes the controller-computed policy digest
// recorded into bindings evaluated against this generation.
type AgentExecutionPolicyStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// SpecDigest is the canonical digest of the observed policy spec.
	// +optional
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	SpecDigest string `json:"specDigest,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=aexpol
// +kubebuilder:printcolumn:name="V1Admission",type=boolean,JSONPath=`.spec.allowNewV1Bindings`
// +kubebuilder:printcolumn:name="Isolation",type=string,JSONPath=`.spec.networkIsolationProfile`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AgentExecutionPolicy is the cluster-admin-owned harness v1 compatibility
// policy for one trust domain. It is namespaced to the trust domain but
// writable only by administrators.
type AgentExecutionPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentExecutionPolicySpec   `json:"spec"`
	Status AgentExecutionPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentExecutionPolicyList contains a list of AgentExecutionPolicy.
type AgentExecutionPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentExecutionPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentExecutionPolicy{}, &AgentExecutionPolicyList{})
}
