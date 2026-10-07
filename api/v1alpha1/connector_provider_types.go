/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	ConnectorProviderConditionAccepted     = "Accepted"
	ConnectorProviderConditionResolvedRefs = "ResolvedRefs"

	ConnectorClientAuthSecretBasic = "ClientSecretBasic"
	ConnectorClientAuthSecretPost  = "ClientSecretPost"

	ConnectorToolSourceBuiltin = "Builtin"
	ConnectorToolSourceHTTP    = "HTTP"
)

// ConnectorToolClass separates read tools, which a Connection may use without
// approval, from write tools, which require approval and a readWrite Connection.
// +kubebuilder:validation:Enum=read;write
type ConnectorToolClass string

const (
	ConnectorToolClassRead  ConnectorToolClass = "read"
	ConnectorToolClassWrite ConnectorToolClass = "write"
)

// ConnectorProviderSpec is the operator-owned catalog entry for one third-party
// service that people may link through OAuth.
type ConnectorProviderSpec struct {
	// DisplayName is shown to people in the dashboard and CLI. Defaults to the
	// object name.
	// +kubebuilder:validation:MaxLength=128
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// OAuth configures the authorization-code flow used to link an account.
	// +kubebuilder:validation:Required
	OAuth ConnectorOAuthConfig `json:"oauth"`

	// Tools declares the tools this provider offers to agents acting for a
	// linked person. Built-in tools name an Orka built-in; HTTP tools carry a
	// curated definition executed by the controller with the person's token.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +listType=map
	// +listMapKey=name
	Tools []ConnectorTool `json:"tools"`
}

// ConnectorOAuthConfig holds the OAuth 2.0 client settings for a provider.
// Only the client secret lives in a Secret; everything else is public.
// +kubebuilder:validation:XValidation:rule="self.authorizeURL.startsWith('https://')",message="authorizeURL must use https"
// +kubebuilder:validation:XValidation:rule="self.tokenURL.startsWith('https://')",message="tokenURL must use https"
// +kubebuilder:validation:XValidation:rule="!has(self.revocationURL) || self.revocationURL.startsWith('https://')",message="revocationURL must use https"
// +kubebuilder:validation:XValidation:rule="!has(self.additionalAuthorizeParameters) || self.additionalAuthorizeParameters.all(k, !(k.lowerAscii() in ['client_id','client_secret','redirect_uri','response_type','scope','state','code_challenge','code_challenge_method','code','code_verifier','grant_type','refresh_token']))",message="additionalAuthorizeParameters must not contain reserved OAuth fields"
type ConnectorOAuthConfig struct {
	// AuthorizeURL is the provider's absolute HTTPS authorization endpoint.
	// +kubebuilder:validation:MinLength=1
	AuthorizeURL string `json:"authorizeURL"`

	// TokenURL is the provider's absolute HTTPS token endpoint used for the
	// code exchange and for refresh.
	// +kubebuilder:validation:MinLength=1
	TokenURL string `json:"tokenURL"`

	// RevocationURL is an optional RFC 7009 revocation endpoint called on
	// disconnect for the committed credential. Revocation is best-effort, and
	// Orka never revokes material it did not commit: a token from a consent
	// nobody completed is deleted and left to expire, because it may belong
	// to a different person's grant.
	// +optional
	RevocationURL string `json:"revocationURL,omitempty"`

	// ClientID is the public OAuth client identifier registered with the provider.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	ClientID string `json:"clientID"`

	// ClientSecretRef selects the OAuth client secret from a same-namespace Secret.
	// +kubebuilder:validation:Required
	ClientSecretRef SecretKeySelector `json:"clientSecretRef"`

	// ClientAuthentication selects how the client secret is presented to the
	// token endpoint. Defaults to ClientSecretBasic.
	// +kubebuilder:validation:Enum=ClientSecretBasic;ClientSecretPost
	// +kubebuilder:default=ClientSecretBasic
	// +optional
	ClientAuthentication string `json:"clientAuthentication,omitempty"`

	// PKCE enables RFC 7636 code verification. Defaults to true and must stay
	// true unless the provider cannot accept a code_challenge.
	// +kubebuilder:default=true
	// +optional
	PKCE *bool `json:"pkce,omitempty"`

	// Scopes groups the OAuth scopes requested for each Connection mode. A
	// readOnly Connection requests only read scopes; readWrite requests both.
	// +optional
	Scopes ConnectorScopes `json:"scopes,omitempty"`

	// AdditionalAuthorizeParameters are static query parameters appended to the
	// authorize URL. Reserved OAuth fields are rejected.
	// +kubebuilder:validation:MaxProperties=16
	// +optional
	AdditionalAuthorizeParameters map[string]string `json:"additionalAuthorizeParameters,omitempty"`
}

// ConnectorScopes lists OAuth scopes by capability.
type ConnectorScopes struct {
	// Read scopes are requested for every Connection.
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MaxLength=256
	// +optional
	Read []string `json:"read,omitempty"`

	// Write scopes are additionally requested for readWrite Connections.
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MaxLength=256
	// +optional
	Write []string `json:"write,omitempty"`
}

// ConnectorTool declares one tool a provider offers.
// +kubebuilder:validation:XValidation:rule="self.source != 'HTTP' || has(self.http)",message="HTTP tools require http"
// +kubebuilder:validation:XValidation:rule="self.source != 'Builtin' || (!has(self.http) && !has(self.description))",message="Builtin tools must not set http or description"
type ConnectorTool struct {
	// Name is the tool name exposed to agents. For Builtin tools it must match
	// an Orka built-in tool name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9_]*$`
	Name string `json:"name"`

	// Class is read or write. Write tools are hidden from readOnly Connections
	// and default into the Agent's approval-required set.
	Class ConnectorToolClass `json:"class"`

	// Source is Builtin for an existing Orka tool or HTTP for a curated
	// definition carried in this provider.
	// +kubebuilder:validation:Enum=Builtin;HTTP
	Source string `json:"source"`

	// Description is shown to the LLM for HTTP tools.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	Description string `json:"description,omitempty"`

	// Parameters is the JSON Schema for HTTP tool parameters. When set, its
	// root must declare type "object".
	// +optional
	Parameters *apiextensionsv1.JSON `json:"parameters,omitempty"`

	// HTTP is the request definition for HTTP tools. The controller adds the
	// person's bearer credential at call time; no other auth is configurable.
	// +optional
	HTTP *ConnectorHTTPTool `json:"http,omitempty"`
}

// ConnectorHTTPTool defines a curated HTTPS request executed with a person's
// linked credential.
// +kubebuilder:validation:XValidation:rule="self.url.startsWith('https://')",message="connector tool url must use https"
type ConnectorHTTPTool struct {
	// URL is the absolute HTTPS endpoint called when the tool is invoked.
	// +kubebuilder:validation:MinLength=1
	URL string `json:"url"`

	// Method defaults to POST.
	// +kubebuilder:validation:Enum=GET;POST;PUT;PATCH;DELETE
	// +kubebuilder:default=POST
	// +optional
	Method string `json:"method,omitempty"`

	// Headers are static non-credential headers. Authorization is reserved.
	// +kubebuilder:validation:MaxProperties=16
	// +optional
	Headers map[string]string `json:"headers,omitempty"`

	// Timeout bounds the request. Defaults to 30s and may not exceed 10m. It
	// is a single bounded component ("30s", "2.5m", "500µs") or the canonical
	// form a typed client serializes ("10m0s", "2m30s"). Every component is
	// bounded, so each stored value decodes as a Go duration without
	// overflow and one malformed provider cannot break the typed informer
	// for every provider.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=16
	// +kubebuilder:validation:Pattern=`^([0-9]{1,4}(\.[0-9]{1,9})?(ns|us|µs|μs|ms|s|m|h)|([0-9]{1,4}h)?([0-9]{1,4}m)?[0-9]{1,4}(\.[0-9]{1,9})?s)$`
	// +kubebuilder:validation:XValidation:rule="duration(self) > duration('0s') && duration(self) <= duration('10m')",message="timeout must be positive and at most 10m"
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`
}

// ConnectorProviderStatus contains only safe validation state.
type ConnectorProviderStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.metadata.name.size() <= 63",message="metadata.name must be at most 63 characters: it labels every Connection"
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Accepted",type=string,JSONPath=`.status.conditions[?(@.type=="Accepted")].status`
// +kubebuilder:printcolumn:name="ResolvedRefs",type=string,JSONPath=`.status.conditions[?(@.type=="ResolvedRefs")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ConnectorProvider is an operator-owned catalog entry describing how people
// link one third-party service and which tools it offers.
type ConnectorProvider struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ConnectorProviderSpec   `json:"spec"`
	Status ConnectorProviderStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ConnectorProviderList contains a list of ConnectorProvider objects.
type ConnectorProviderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ConnectorProvider `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ConnectorProvider{}, &ConnectorProviderList{})
}
