/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

const (
	OutboundAccessPolicyConditionAccepted     = "Accepted"
	OutboundAccessPolicyConditionResolvedRefs = "ResolvedRefs"

	OutboundGrantTokenExchange = "TokenExchange"
	OutboundGrantJWTBearer     = "JWTBearer"

	OutboundTokenSourceTransactionToken = "TransactionToken"
	OutboundTokenSourceServiceAccount   = "ServiceAccount"
	OutboundTokenSourceSecretRef        = "SecretRef"

	OutboundClientAuthNone          = "None"
	OutboundClientAuthSecretBasic   = "ClientSecretBasic"
	OutboundClientAuthSecretPost    = "ClientSecretPost"
	OutboundClientAuthPrivateKeyJWT = "PrivateKeyJWT"
)

// OutboundAccessPolicySpec configures exactly one outbound credential or
// routing adapter.
// +kubebuilder:validation:XValidation:rule="has(self.direct) != has(self.gateway)",message="exactly one of direct or gateway is required"
type OutboundAccessPolicySpec struct {
	// Direct exchanges a resolved subject for a downstream resource credential.
	// +optional
	Direct *DirectOutboundAccess `json:"direct,omitempty"`

	// Gateway routes the original Tool request through a trusted Kubernetes Service.
	// +optional
	Gateway *GatewayOutboundAccess `json:"gateway,omitempty"`
}

// DirectOutboundAccess configures RFC 8693 or RFC 7523 resource credentials.
// +kubebuilder:validation:XValidation:rule="self.grant != 'JWTBearer' || !has(self.actor)",message="JWTBearer does not support actor"
// +kubebuilder:validation:XValidation:rule="self.grant != 'JWTBearer' || !has(self.subject.tokenType)",message="JWTBearer subject must not set tokenType"
// +kubebuilder:validation:XValidation:rule="self.grant != 'TokenExchange' || self.subject.source != 'SecretRef' || (has(self.subject.tokenType) && self.subject.tokenType.size() > 0)",message="SecretRef token exchange subjects require tokenType"
// +kubebuilder:validation:XValidation:rule="self.grant != 'TokenExchange' || !has(self.actor) || self.actor.source != 'SecretRef' || (has(self.actor.tokenType) && self.actor.tokenType.size() > 0)",message="SecretRef token exchange actors require tokenType"
// +kubebuilder:validation:XValidation:rule="self.grant != 'TokenExchange' || (has(self.expectedIssuedTokenType) && self.expectedIssuedTokenType.size() > 0)",message="TokenExchange requires expectedIssuedTokenType"
// +kubebuilder:validation:XValidation:rule="!has(self.additionalParameters) || self.additionalParameters.all(k, !(k.lowerAscii() in ['grant_type','subject_token','subject_token_type','actor_token','actor_token_type','assertion','scope','audience','resource','requested_token_type','client_id','client_secret','client_assertion','client_assertion_type']))",message="additionalParameters must not contain reserved OAuth fields"
type DirectOutboundAccess struct {
	// Grant selects RFC 8693 token exchange or RFC 7523 JWT bearer.
	// +kubebuilder:validation:Enum=TokenExchange;JWTBearer
	// +kubebuilder:default=TokenExchange
	// +optional
	Grant string `json:"grant,omitempty"`

	// TokenEndpoint is the exact OAuth token endpoint.
	// +kubebuilder:validation:Required
	TokenEndpoint OutboundTokenEndpoint `json:"tokenEndpoint"`

	// Subject resolves the subject token or JWT assertion.
	// +kubebuilder:validation:Required
	Subject OutboundTokenSource `json:"subject"`

	// Actor optionally resolves an RFC 8693 actor token.
	// +optional
	Actor *OutboundTokenSource `json:"actor,omitempty"`

	// Audiences are emitted as repeated OAuth audience parameters.
	// +optional
	Audiences []string `json:"audiences,omitempty"`

	// Scopes are emitted as a space-separated OAuth scope parameter.
	// TransactionToken subjects may request only a subset of the parent scope.
	// +optional
	Scopes []string `json:"scopes,omitempty"`

	// Resources are emitted as repeated RFC 8707 resource parameters.
	// +optional
	Resources []string `json:"resources,omitempty"`

	// RequestedTokenType is an arbitrary OAuth token-type URN.
	// +optional
	RequestedTokenType string `json:"requestedTokenType,omitempty"`

	// ExpectedIssuedTokenType is the exact issued_token_type required for RFC 8693.
	// RFC 7523 responses may omit issued_token_type.
	// +kubebuilder:validation:MinLength=1
	// +optional
	ExpectedIssuedTokenType string `json:"expectedIssuedTokenType,omitempty"`

	// AdditionalParameters are static form parameters. Reserved OAuth fields are rejected.
	// +kubebuilder:validation:MaxProperties=32
	// +optional
	AdditionalParameters map[string]string `json:"additionalParameters,omitempty"`

	// ClientAuthentication configures OAuth client authentication.
	// +optional
	ClientAuthentication *OutboundClientAuthentication `json:"clientAuthentication,omitempty"`

	// Output configures resource credential injection.
	// +optional
	Output *OutboundCredentialOutput `json:"output,omitempty"`
}

// OutboundTokenEndpoint selects either an absolute URL or a Kubernetes Service.
// +kubebuilder:validation:XValidation:rule="has(self.url) != has(self.serviceRef)",message="exactly one of url or serviceRef is required"
// +kubebuilder:validation:XValidation:rule="!has(self.url) || (!has(self.scheme) && !has(self.path))",message="scheme and path are only valid with serviceRef"
// +kubebuilder:validation:XValidation:rule="!has(self.url) || self.url.startsWith('https://')",message="url token endpoints must use https"
type OutboundTokenEndpoint struct {
	// URL is an exact absolute HTTPS token endpoint without userinfo.
	// +optional
	URL string `json:"url,omitempty"`

	// ServiceRef selects a token endpoint Service. Cross-namespace refs require
	// an exact controller allowlist entry.
	// +optional
	ServiceRef *OutboundServiceReference `json:"serviceRef,omitempty"`

	// Scheme is used with ServiceRef and defaults to https.
	// +kubebuilder:validation:Enum=http;https
	// +optional
	Scheme string `json:"scheme,omitempty"`

	// Path is the exact endpoint path used with ServiceRef.
	// +optional
	Path string `json:"path,omitempty"`

	// TLS configures verification for an HTTPS endpoint.
	// +optional
	TLS *OutboundTLSConfig `json:"tls,omitempty"`
}

// OutboundTokenSource selects a direct-exchange subject or actor.
// +kubebuilder:validation:XValidation:rule="self.source != 'TransactionToken' || (!has(self.secretRef) && !has(self.serviceAccountRef))",message="TransactionToken must not configure Secret or ServiceAccount references"
// +kubebuilder:validation:XValidation:rule="self.source != 'ServiceAccount' || (has(self.serviceAccountRef) && !has(self.secretRef))",message="ServiceAccount requires only serviceAccountRef"
// +kubebuilder:validation:XValidation:rule="self.source != 'SecretRef' || (has(self.secretRef) && !has(self.serviceAccountRef))",message="SecretRef requires only secretRef"
type OutboundTokenSource struct {
	// Source selects TransactionToken, ServiceAccount, or SecretRef.
	// +kubebuilder:validation:Enum=TransactionToken;ServiceAccount;SecretRef
	Source string `json:"source"`

	// TokenType is an arbitrary RFC 8693 token-type URN. It is not used by
	// RFC 7523 JWT bearer grants.
	// +optional
	TokenType string `json:"tokenType,omitempty"`

	// SecretRef selects a same-namespace Secret value.
	// +optional
	SecretRef *NamespacedSecretKeySelector `json:"secretRef,omitempty"`

	// ServiceAccountRef selects a ServiceAccount and TokenRequest parameters.
	// +optional
	ServiceAccountRef *OutboundServiceAccountReference `json:"serviceAccountRef,omitempty"`
}

// OutboundServiceAccountReference configures a Kubernetes TokenRequest.
type OutboundServiceAccountReference struct {
	// Name is the same-namespace ServiceAccount name.
	Name string `json:"name"`

	// Audiences are requested for the projected ServiceAccount token.
	// +optional
	Audiences []string `json:"audiences,omitempty"`

	// ExpirationSeconds requests a bounded token lifetime.
	// +kubebuilder:validation:Minimum=600
	// +kubebuilder:validation:Maximum=3600
	// +kubebuilder:default=600
	// +optional
	ExpirationSeconds *int64 `json:"expirationSeconds,omitempty"`
}

// OutboundClientAuthentication configures one OAuth client authentication method.
// +kubebuilder:validation:XValidation:rule="self.method != 'None' || (!has(self.clientSecretRef) && !has(self.privateKeyRef))",message="None must not configure client credentials"
// +kubebuilder:validation:XValidation:rule="!(self.method in ['ClientSecretBasic','ClientSecretPost']) || (has(self.clientID) && self.clientID.size() > 0 && has(self.clientSecretRef) && !has(self.privateKeyRef))",message="client-secret methods require clientID and clientSecretRef only"
// +kubebuilder:validation:XValidation:rule="self.method != 'PrivateKeyJWT' || (has(self.clientID) && self.clientID.size() > 0 && has(self.privateKeyRef) && !has(self.clientSecretRef))",message="PrivateKeyJWT requires clientID and privateKeyRef only"
type OutboundClientAuthentication struct {
	// Method defaults to None.
	// +kubebuilder:validation:Enum=None;ClientSecretBasic;ClientSecretPost;PrivateKeyJWT
	// +kubebuilder:default=None
	// +optional
	Method string `json:"method,omitempty"`

	// ClientID is required by confidential-client methods.
	// +optional
	ClientID string `json:"clientID,omitempty"`

	// ClientSecretRef supplies client-secret basic/post credentials.
	// +optional
	ClientSecretRef *NamespacedSecretKeySelector `json:"clientSecretRef,omitempty"`

	// PrivateKeyRef supplies a PEM RSA or P-256 key for private_key_jwt.
	// +optional
	PrivateKeyRef *NamespacedSecretKeySelector `json:"privateKeyRef,omitempty"`

	// KeyID is copied into the private_key_jwt header.
	// +optional
	KeyID string `json:"keyID,omitempty"`

	// Audience overrides the private_key_jwt aud claim. The endpoint is the default.
	// +optional
	Audience string `json:"audience,omitempty"`
}

// OutboundCredentialOutput configures downstream header injection.
// +kubebuilder:validation:XValidation:rule="!has(self.header) || self.header.lowerAscii() != 'txn-token'",message="Txn-Token cannot be used as the resource credential output header"
type OutboundCredentialOutput struct {
	// Header defaults to Authorization. Txn-Token is forbidden.
	// +kubebuilder:default=Authorization
	// +optional
	Header string `json:"header,omitempty"`

	// Prefix defaults to "Bearer ". Set an explicit empty string for no prefix.
	// +kubebuilder:default="Bearer "
	// +optional
	Prefix *string `json:"prefix,omitempty"`
}

// GatewayOutboundAccess routes Tool requests through a trusted Service.
type GatewayOutboundAccess struct {
	// ServiceRef selects the gateway Service.
	ServiceRef OutboundServiceReference `json:"serviceRef"`

	// Scheme defaults to http for in-cluster gateways.
	// +kubebuilder:validation:Enum=http;https
	// +kubebuilder:default=http
	// +optional
	Scheme string `json:"scheme,omitempty"`

	// TLS configures gateway verification when scheme is https.
	// +optional
	TLS *OutboundTLSConfig `json:"tls,omitempty"`
}

// OutboundServiceReference is an exact namespace/name/port tuple.
type OutboundServiceReference struct {
	// Namespace defaults to the policy namespace. Cross-namespace refs require
	// an exact trusted Service-reference allowlist entry.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	Name string `json:"name"`

	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
}

// OutboundTLSConfig configures TLS server identity and an optional CA Secret.
type OutboundTLSConfig struct {
	// ServerName overrides TLS SNI and hostname verification.
	// +optional
	ServerName string `json:"serverName,omitempty"`

	// CASecretRef selects PEM CA data from a same-namespace Secret.
	// +optional
	CASecretRef *NamespacedSecretKeySelector `json:"caSecretRef,omitempty"`
}

// NamespacedSecretKeySelector is key-specific. Namespace defaults to the policy
// namespace and any different namespace is rejected.
type NamespacedSecretKeySelector struct {
	// +optional
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Key       string `json:"key"`
}

// OutboundAccessPolicyStatus contains only safe validation state.
type OutboundAccessPolicyStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Accepted",type=string,JSONPath=`.status.conditions[?(@.type=="Accepted")].status`
// +kubebuilder:printcolumn:name="ResolvedRefs",type=string,JSONPath=`.status.conditions[?(@.type=="ResolvedRefs")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// OutboundAccessPolicy governs resource credentials or trusted gateway routing
// for Tools in the same namespace.
type OutboundAccessPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   OutboundAccessPolicySpec   `json:"spec,omitempty"`
	Status OutboundAccessPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// OutboundAccessPolicyList contains a list of OutboundAccessPolicy objects.
type OutboundAccessPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OutboundAccessPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&OutboundAccessPolicy{}, &OutboundAccessPolicyList{})
}
