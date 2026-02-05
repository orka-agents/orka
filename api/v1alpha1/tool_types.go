/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ToolSpec defines the desired state of Tool
type ToolSpec struct {
	// Description is the tool description shown to the LLM
	// +kubebuilder:validation:Required
	Description string `json:"description"`

	// Parameters is the JSON Schema for tool parameters (OpenAI function calling format)
	// +optional
	Parameters *apiextensionsv1.JSON `json:"parameters,omitempty"`

	// HTTP defines the HTTP execution configuration
	// +kubebuilder:validation:Required
	HTTP HTTPExecution `json:"http"`
}

// HTTPExecution defines how to execute the tool via HTTP
type HTTPExecution struct {
	// URL is the endpoint to call when the tool is invoked
	// +kubebuilder:validation:Required
	URL string `json:"url"`

	// Method is the HTTP method to use (default: POST)
	// +kubebuilder:validation:Enum=GET;POST;PUT;PATCH;DELETE
	// +kubebuilder:default=POST
	// +optional
	Method string `json:"method,omitempty"`

	// Headers are additional headers to include in requests
	// +optional
	Headers map[string]string `json:"headers,omitempty"`

	// Timeout is the request timeout (default: 30s)
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// AuthSecretRef references a Secret containing authentication token
	// Token is injected as Authorization: Bearer <token>
	// +optional
	AuthSecretRef *SecretKeySelector `json:"authSecretRef,omitempty"`
}

// SecretKeySelector selects a key from a Secret
type SecretKeySelector struct {
	// Name is the name of the Secret
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Key is the key within the Secret
	// +kubebuilder:validation:Required
	Key string `json:"key"`
}

// ToolStatus defines the observed state of Tool
type ToolStatus struct {
	// Available indicates whether the tool endpoint is reachable
	Available bool `json:"available"`

	// LastCheck is the timestamp of the last health check
	// +optional
	LastCheck *metav1.Time `json:"lastCheck,omitempty"`

	// Error is the error message if the tool is unavailable
	// +optional
	Error string `json:"error,omitempty"`

	// Conditions represent the current state of the Tool
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.spec.http.url`
// +kubebuilder:printcolumn:name="Available",type=boolean,JSONPath=`.status.available`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Tool is the Schema for the tools API
type Tool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ToolSpec   `json:"spec,omitempty"`
	Status ToolStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ToolList contains a list of Tool
type ToolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Tool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Tool{}, &ToolList{})
}
