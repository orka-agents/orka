/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

const (
	// ConnectionConditionProviderResolved reports whether providerRef names an
	// Accepted ConnectorProvider in the same namespace.
	ConnectionConditionProviderResolved = "ProviderResolved"
	// ConnectionConditionReady reports whether linked token material is held
	// and usable by the controller. It is owned by the consent and refresh
	// paths and records the outcome of the last consent.
	ConnectionConditionReady = "Ready"
	// ConnectionConditionScopesGranted reports whether the granted scopes
	// cover the scopes the current mode requires. It is owned by the
	// controller, so widening the mode after consent projects Pending without
	// erasing the still-valid consent, and narrowing restores readiness.
	ConnectionConditionScopesGranted = "ScopesGranted"

	ConnectionModeReadOnly  = "readOnly"
	ConnectionModeReadWrite = "readWrite"

	// ConnectionStatePending means consent has not completed.
	ConnectionStatePending = "Pending"
	// ConnectionStateReady means token material is held and not known to be expired.
	ConnectionStateReady = "Ready"
	// ConnectionStateExpired means the held material expired and could not be refreshed.
	ConnectionStateExpired = "Expired"
	// ConnectionStateRevoked means the provider rejected the material or the
	// person disconnected.
	ConnectionStateRevoked = "Revoked"
	// ConnectionStateError means the Connection cannot be used, for example
	// because its provider is missing or invalid.
	ConnectionStateError = "Error"

	ConnectionReasonProviderResolved   = "ProviderResolved"
	ConnectionReasonProviderMissing    = "ProviderMissing"
	ConnectionReasonProviderInvalid    = "ProviderInvalid"
	ConnectionReasonProviderReadFailed = "ProviderReadFailed"
	// ConnectionReasonProviderUnavailable marks ScopesGranted as Unknown
	// while the provider is missing, invalid, or unreadable: scopes cannot
	// be judged against a provider that is not resolved.
	ConnectionReasonProviderUnavailable = "ProviderUnavailable"

	// Ready condition reasons. The reason, not status.state, is the durable
	// record of the link, so a transient provider outage cannot erase it.
	ConnectionReasonPendingConsent  = "PendingConsent"
	ConnectionReasonLinked          = "Linked"
	ConnectionReasonExpired         = "Expired"
	ConnectionReasonRevoked         = "Revoked"
	ConnectionReasonConsentRequired = "ConsentRequired"
	ConnectionReasonScopesGranted   = "ScopesGranted"
)

// ConnectionSubject is the verified identity that owns a Connection. It is
// copied from the authenticated request and never from a request body.
type ConnectionSubject struct {
	// Issuer is the identity issuer that verified the subject.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	Issuer string `json:"issuer"`

	// Subject is the issuer-scoped stable subject identifier.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	Subject string `json:"subject"`
}

// ConnectionSpec links one person to one ConnectorProvider.
// +kubebuilder:validation:XValidation:rule="self.subject == oldSelf.subject",message="subject is immutable"
// +kubebuilder:validation:XValidation:rule="self.providerRef == oldSelf.providerRef",message="providerRef is immutable"
type ConnectionSpec struct {
	// Subject is the owning verified identity.
	// +kubebuilder:validation:Required
	Subject ConnectionSubject `json:"subject"`

	// ProviderRef names a ConnectorProvider in the same namespace.
	// +kubebuilder:validation:Required
	ProviderRef LocalObjectReference `json:"providerRef"`

	// Mode is readOnly or readWrite. readOnly hides the provider's write tools
	// from agents and requests only read scopes.
	// +kubebuilder:validation:Enum=readOnly;readWrite
	// +kubebuilder:default=readOnly
	// +optional
	Mode string `json:"mode,omitempty"`
}

// ConnectionConsent records which provider OAuth client a consent belongs to.
type ConnectionConsent struct {
	// ProviderUID is the UID of the ConnectorProvider at consent time.
	// +kubebuilder:validation:MaxLength=253
	ProviderUID string `json:"providerUID,omitempty"`
	// AuthorityDigest is a hex SHA-256 digest of the provider's OAuth client
	// identity (client ID, client secret reference, authentication method,
	// and endpoints) at consent time. It carries no secret material.
	// +kubebuilder:validation:MaxLength=64
	AuthorityDigest string `json:"authorityDigest,omitempty"`
}

// ConnectionStatus reports link state. It never carries token material.
type ConnectionStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// State is Pending, Ready, Expired, Revoked, or Error.
	// +optional
	State string `json:"state,omitempty"`

	// GrantedScopes are the scopes the provider reported at consent time, or
	// the requested scopes when the provider reported none.
	// +optional
	GrantedScopes []string `json:"grantedScopes,omitempty"`

	// LinkedAt is when consent last completed.
	// +optional
	LinkedAt *metav1.Time `json:"linkedAt,omitempty"`

	// GrantSequence counts completed consents on this Connection. It rises
	// on every commit, so authority bound to one grant (a frozen execution
	// snapshot or a standing approval) does not carry over to a later
	// re-link of the same Connection object.
	// +optional
	GrantSequence int64 `json:"grantSequence,omitempty"`

	// ExpiresAt is the held access token's expiry, if the provider reported one.
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`

	// LastRefreshTime is when the held material was last refreshed.
	// +optional
	LastRefreshTime *metav1.Time `json:"lastRefreshTime,omitempty"`

	// Consent identifies the ConnectorProvider OAuth client the last consent
	// was granted against. The controller requires a new consent when the
	// provider is replaced or its OAuth authority changes.
	// +optional
	Consent *ConnectionConsent `json:"consent,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.spec.providerRef.name`
// +kubebuilder:printcolumn:name="Subject",type=string,JSONPath=`.spec.subject.subject`
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Connection is one person's linked account with one ConnectorProvider.
// Deleting it is the disconnect.
type Connection struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ConnectionSpec   `json:"spec"`
	Status ConnectionStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ConnectionList contains a list of Connection objects.
type ConnectionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Connection `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Connection{}, &ConnectionList{})
}
