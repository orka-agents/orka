/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ToolSpec defines the desired state of Tool
// +kubebuilder:validation:XValidation:rule="has(self.http) || (has(self.mcp) && has(self.mcp.substrateActor))",message="http or mcp.substrateActor is required"
type ToolSpec struct {
	// Description is the tool description shown to the LLM
	// +kubebuilder:validation:Required
	Description string `json:"description"`

	// Parameters is the JSON Schema for tool parameters (OpenAI function calling format)
	// +optional
	Parameters *apiextensionsv1.JSON `json:"parameters,omitempty"`

	// HTTP defines the HTTP execution configuration. It is required unless MCP
	// is set.
	// +optional
	HTTP *HTTPExecution `json:"http,omitempty"`

	// MCP defines a durable MCP server backend for this tool. MCP tools must set
	// substrateActor.
	// +optional
	MCP *MCPToolServer `json:"mcp,omitempty"`
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
	// Token is injected as Authorization: Bearer <token> (when AuthInject=header)
	// or into the request body (when AuthInject=body)
	// +optional
	AuthSecretRef *SecretKeySelector `json:"authSecretRef,omitempty"`

	// AuthInject specifies where to inject the auth token: "header" (default) or "body"
	// When "header", the token is added as Authorization: Bearer <token>
	// When "body", the token is added to the JSON request body using AuthBodyKey
	// +kubebuilder:validation:Enum=header;body
	// +kubebuilder:default=header
	// +optional
	AuthInject string `json:"authInject,omitempty"`

	// AuthBodyKey is the JSON key name to use when AuthInject=body
	// For example, "api_key" would inject {"api_key": "<token>", ...}
	// Only used when AuthInject=body
	// +optional
	AuthBodyKey string `json:"authBodyKey,omitempty"`
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

// MCPToolServer configures a Model Context Protocol server backend.
type MCPToolServer struct {
	// Path is the HTTP path exposed by the MCP server inside the actor.
	// Defaults to /mcp.
	// +optional
	Path string `json:"path,omitempty"`

	// SubstrateActor configures a durable Substrate actor that hosts the MCP server.
	// +kubebuilder:validation:Required
	SubstrateActor *SubstrateMCPActor `json:"substrateActor"`
}

// SubstrateMCPActor selects the durable actor that hosts an MCP server.
type SubstrateMCPActor struct {
	// TemplateRef references the ActorTemplate that runs the MCP server.
	// +kubebuilder:validation:Required
	TemplateRef WorkspaceTemplateReference `json:"templateRef"`

	// PoolRef optionally assigns this tool actor to an Orka SubstrateActorPool.
	// +optional
	PoolRef *SubstrateActorPoolReference `json:"poolRef,omitempty"`

	// Boot asks Substrate to boot this actor from scratch on first resume.
	// +optional
	Boot bool `json:"boot,omitempty"`
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

	// Endpoint is the resolved non-secret endpoint used by workers.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// Actor reports durable actor metadata when this tool is MCP actor-backed.
	// +optional
	Actor *ToolActorStatus `json:"actor,omitempty"`

	// Conditions represent the current state of the Tool
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ToolActorStatus reports safe durable actor metadata for a Tool.
type ToolActorStatus struct {
	// Provider is the actor provider.
	// +optional
	Provider WorkspaceProvider `json:"provider,omitempty"`

	// ActorID is the stable provider actor id.
	// +optional
	ActorID string `json:"actorID,omitempty"`

	// RouteHost is the HTTP Host value used with the provider router.
	// +optional
	RouteHost string `json:"routeHost,omitempty"`

	// TemplateRef is the resolved actor template.
	// +optional
	TemplateRef *WorkspaceTemplateReference `json:"templateRef,omitempty"`

	// PoolRef is the resolved Orka actor pool.
	// +optional
	PoolRef *SubstrateActorPoolReference `json:"poolRef,omitempty"`
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
