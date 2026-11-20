/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ProviderType defines the type of LLM provider
// +kubebuilder:validation:Enum=anthropic;openai;azure-openai
type ProviderType string

const (
	// ProviderTypeAnthropic is the Anthropic Claude provider
	ProviderTypeAnthropic ProviderType = "anthropic"
	// ProviderTypeOpenAI is the OpenAI provider
	ProviderTypeOpenAI ProviderType = "openai"
	// ProviderTypeAzureOpenAI is the Azure OpenAI provider
	ProviderTypeAzureOpenAI ProviderType = "azure-openai"
)

// ProviderSpec defines the desired state of Provider
// +kubebuilder:validation:XValidation:rule="has(self.secretRef) != has(self.auth)",message="set exactly one of spec.secretRef or spec.auth"
// +kubebuilder:validation:XValidation:rule="!has(self.auth) || self.type == 'azure-openai'",message="spec.auth is supported only for azure-openai Providers"
// +kubebuilder:validation:XValidation:rule="!has(self.auth) || (has(self.baseURL) && self.baseURL.startsWith('https://'))",message="spec.auth requires an https spec.baseURL"
type ProviderSpec struct {
	// Type is the LLM provider type (anthropic, openai, azure-openai)
	// +kubebuilder:validation:Required
	Type ProviderType `json:"type"`

	// SecretRef references the Secret containing the API key. Exactly one of
	// secretRef and auth is set.
	// +optional
	SecretRef *ProviderSecretRef `json:"secretRef,omitempty"`

	// Auth selects keyless authentication. The controller obtains a
	// short-lived token for each model request it sends; no credential is
	// stored in a Secret or mounted into a Pod. Exactly one of secretRef and
	// auth is set.
	// +optional
	Auth *ProviderAuth `json:"auth,omitempty"`

	// BaseURL is an optional custom API endpoint (for proxies or self-hosted)
	// +optional
	BaseURL string `json:"baseURL,omitempty"`

	// DefaultModel is the default model to use if not specified in Task/Agent
	// +optional
	DefaultModel string `json:"defaultModel,omitempty"`

	// Azure contains Azure-specific configuration
	// +optional
	Azure *AzureConfig `json:"azure,omitempty"`
}

// ProviderSecretRef references a Secret containing API credentials
type ProviderSecretRef struct {
	// Name is the name of the Secret
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Key is the key within the Secret containing the API key
	// +kubebuilder:default="api-key"
	// +optional
	Key string `json:"key,omitempty"`
}

// ProviderAuth selects a keyless authentication issuer for a Provider.
// +kubebuilder:validation:XValidation:rule="has(self.entra)",message="spec.auth needs an issuer; set spec.auth.entra"
type ProviderAuth struct {
	// Entra authenticates with a Microsoft Entra ID workload identity that
	// the operator configured on the controller.
	// +optional
	Entra *ProviderEntraAuth `json:"entra,omitempty"`
}

// ProviderEntraAuth names an Entra identity configured on the controller.
type ProviderEntraAuth struct {
	// Identity is the name of a Provider identity configured on the
	// controller (--provider-identities). The identity fixes the tenant,
	// client, cloud, token scope, and the endpoints its tokens may reach.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Identity string `json:"identity"`
}

// EntraIdentity returns the configured Entra identity name, or "" when the
// Provider does not use Entra authentication.
func (p *Provider) EntraIdentity() string {
	if p == nil || p.Spec.Auth == nil || p.Spec.Auth.Entra == nil {
		return ""
	}
	return p.Spec.Auth.Entra.Identity
}

// UsesKeylessAuth reports whether the Provider authenticates without an API
// key Secret.
func (p *Provider) UsesKeylessAuth() bool {
	return p != nil && p.Spec.Auth != nil
}

// AzureConfig contains Azure OpenAI specific configuration
type AzureConfig struct {
	// DeploymentName is the Azure OpenAI deployment name
	// +kubebuilder:validation:Required
	DeploymentName string `json:"deploymentName"`

	// APIVersion is the Azure OpenAI API version
	// +kubebuilder:default="2024-02-15-preview"
	// +optional
	APIVersion string `json:"apiVersion,omitempty"`
}

// ProviderStatus defines the observed state of Provider
type ProviderStatus struct {
	// Ready indicates whether the provider is configured and validated
	// +optional
	Ready bool `json:"ready,omitempty"`

	// LastValidated is the timestamp of the last successful validation
	// +optional
	LastValidated *metav1.Time `json:"lastValidated,omitempty"`

	// Message provides additional status information
	// +optional
	Message string `json:"message,omitempty"`

	// Conditions represent the current state of the Provider
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=`.status.ready`
// +kubebuilder:printcolumn:name="Default Model",type=string,JSONPath=`.spec.defaultModel`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Provider is the Schema for the providers API
type Provider struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ProviderSpec   `json:"spec,omitempty"`
	Status ProviderStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ProviderList contains a list of Provider
type ProviderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Provider `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Provider{}, &ProviderList{})
}
