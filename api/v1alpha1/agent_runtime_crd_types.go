/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// AgentRuntimeContractVersion identifies the Orka-facing runtime contract.
// +kubebuilder:validation:Enum=orka.harness.v1
type AgentRuntimeContractVersion string

const (
	// AgentRuntimeContractHarnessV1 is the frozen harness v1 HTTP+SSE contract.
	AgentRuntimeContractHarnessV1 AgentRuntimeContractVersion = "orka.harness.v1"
)

// AgentRuntimeDeploymentMode selects how the runtime endpoint is provided.
// +kubebuilder:validation:Enum=external-endpoint
type AgentRuntimeDeploymentMode string

const (
	// AgentRuntimeDeploymentModeExternalEndpoint points at a pre-deployed harness endpoint.
	AgentRuntimeDeploymentModeExternalEndpoint AgentRuntimeDeploymentMode = "external-endpoint"
)

// AgentRuntimeToolExecutionMode declares how custom runtimes interact with tools.
// +kubebuilder:validation:Enum=observed;brokered
type AgentRuntimeToolExecutionMode string

const (
	// AgentRuntimeToolExecutionModeObserved means the runtime owns its internal tools and Orka observes lifecycle only.
	AgentRuntimeToolExecutionModeObserved AgentRuntimeToolExecutionMode = "observed"
	// AgentRuntimeToolExecutionModeBrokered means the runtime asks Orka to execute governed Tool CRDs.
	AgentRuntimeToolExecutionModeBrokered AgentRuntimeToolExecutionMode = "brokered"
)

// AgentRuntimeReference selects a registered AgentRuntime for a type: agent task.
type AgentRuntimeReference struct {
	// Name is the AgentRuntime name. RuntimeRefs are resolved in the task namespace.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// AgentRuntimeDeploymentSpec configures where Orka reaches the harness runtime.
type AgentRuntimeDeploymentSpec struct {
	// Mode is the deployment mode. The first milestone supports external endpoints only.
	// +kubebuilder:validation:Required
	Mode AgentRuntimeDeploymentMode `json:"mode"`

	// Endpoint is the base URL for a pre-deployed or external orka.harness.v1 service.
	// It must not contain credentials; bearer auth is configured via clientAuth.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^https?://[^\s@?#]+$`
	Endpoint string `json:"endpoint"`
}

// AgentRuntimeBearerAuthReference identifies the Secret key holding a harness bearer token.
type AgentRuntimeBearerAuthReference struct {
	// Name is the Secret name.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Key is the Secret data key containing the bearer token.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`
}

// AgentRuntimeClientAuth configures Orka's client authentication to the harness endpoint.
type AgentRuntimeClientAuth struct {
	// BearerAuthRef points to the bearer token Secret used for mutating harness endpoints.
	// The referenced Secret must opt in with label orka.ai/agent-runtime-auth=true;
	// it may also set orka.ai/agent-runtime-name=<runtime> to restrict use to one AgentRuntime.
	// +kubebuilder:validation:Required
	BearerAuthRef AgentRuntimeBearerAuthReference `json:"bearerTokenSecretRef"`
}

// AgentRuntimeCapabilitiesSpec describes required capabilities for runtime readiness.
type AgentRuntimeCapabilitiesSpec struct {
	// ToolExecutionModes lists tool execution modes the runtime must advertise.
	// +listType=set
	// +optional
	ToolExecutionModes []AgentRuntimeToolExecutionMode `json:"toolExecutionModes,omitempty"`

	// SupportsCancel requires the runtime to advertise cancellation support when true.
	// +optional
	SupportsCancel *bool `json:"supportsCancel,omitempty"`

	// SupportsRuntimeSessions requires the runtime to advertise stable runtime sessions when true.
	// +optional
	SupportsRuntimeSessions *bool `json:"supportsRuntimeSessions,omitempty"`
}

// AgentRuntimeRegistrySpec defines the desired state of a registered Orka harness runtime.
type AgentRuntimeRegistrySpec struct {
	// ContractVersion is the Orka harness contract this runtime must implement.
	// +kubebuilder:validation:Required
	// +kubebuilder:default=orka.harness.v1
	ContractVersion AgentRuntimeContractVersion `json:"contractVersion"`

	// Deployment identifies the runtime endpoint provider.
	// +kubebuilder:validation:Required
	Deployment AgentRuntimeDeploymentSpec `json:"deployment"`

	// ClientAuth configures controller-to-runtime authentication.
	// +kubebuilder:validation:Required
	ClientAuth AgentRuntimeClientAuth `json:"clientAuth"`

	// Capabilities declares readiness requirements Orka checks against the runtime.
	// +optional
	Capabilities *AgentRuntimeCapabilitiesSpec `json:"capabilities,omitempty"`
}

// AgentRuntimeObservedCapabilities records the sanitized capability data observed by Orka.
type AgentRuntimeObservedCapabilities struct {
	// ProtocolVersion is the runtime's advertised Orka protocol version.
	// +optional
	ProtocolVersion string `json:"protocolVersion,omitempty"`

	// Transport is the runtime transport, normally http+sse.
	// +optional
	Transport string `json:"transport,omitempty"`

	// RuntimeName is the runtime name advertised by /v1/capabilities.
	// +optional
	RuntimeName string `json:"runtimeName,omitempty"`

	// RuntimeVersion is the runtime version advertised by /v1/capabilities.
	// +optional
	RuntimeVersion string `json:"runtimeVersion,omitempty"`

	// ProviderKind is the provider kind advertised by /v1/capabilities.
	// +optional
	ProviderKind string `json:"providerKind,omitempty"`

	// ToolExecutionModes are the tool modes advertised by /v1/capabilities.
	// +listType=set
	// +optional
	ToolExecutionModes []AgentRuntimeToolExecutionMode `json:"toolExecutionModes,omitempty"`

	// SupportsCancel reports whether the runtime advertises cancellation support.
	// +optional
	SupportsCancel bool `json:"supportsCancel,omitempty"`

	// SupportsRuntimeSessions reports whether the runtime advertises runtime-session support.
	// +optional
	SupportsRuntimeSessions bool `json:"supportsRuntimeSessions,omitempty"`

	// SupportsSuspend reports whether the runtime advertises suspend support.
	// +optional
	SupportsSuspend bool `json:"supportsSuspend,omitempty"`

	// SupportsWorkspaceSnapshot reports whether the runtime advertises workspace snapshots.
	// +optional
	SupportsWorkspaceSnapshot bool `json:"supportsWorkspaceSnapshot,omitempty"`

	// MaxConcurrentTurns is the advertised concurrency ceiling.
	// +optional
	MaxConcurrentTurns int `json:"maxConcurrentTurns,omitempty"`
}

// AgentRuntimeStatus defines the observed state of an AgentRuntime.
type AgentRuntimeStatus struct {
	// Ready indicates the runtime passed the configured Orka readiness checks.
	// +optional
	Ready bool `json:"ready,omitempty"`

	// ObservedGeneration is the latest generation reconciled into this status.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ObservedCapabilities contains sanitized capabilities from the last probe.
	// +optional
	ObservedCapabilities *AgentRuntimeObservedCapabilities `json:"observedCapabilities,omitempty"`

	// LastValidated is the timestamp of the last readiness probe.
	// +optional
	LastValidated *metav1.Time `json:"lastValidated,omitempty"`

	// ObservedAuthRefResourceVersion is the resourceVersion of the bearer auth Secret
	// used for the last readiness probe. It is non-secret metadata used to decide
	// when token rotation requires a fresh authenticated conformance turn.
	// +optional
	ObservedAuthRefResourceVersion string `json:"observedAuthRefResourceVersion,omitempty"`

	// Message provides sanitized readiness context.
	// +optional
	Message string `json:"message,omitempty"`

	// Conditions represent the current state of the AgentRuntime.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=`.status.ready`
// +kubebuilder:printcolumn:name="Contract",type=string,JSONPath=`.spec.contractVersion`
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.deployment.mode`
// +kubebuilder:printcolumn:name="Runtime",type=string,JSONPath=`.status.observedCapabilities.runtimeName`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AgentRuntime is the Schema for registered Orka harness runtimes.
type AgentRuntime struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentRuntimeRegistrySpec `json:"spec,omitempty"`
	Status AgentRuntimeStatus       `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentRuntimeList contains a list of AgentRuntime.
type AgentRuntimeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentRuntime `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentRuntime{}, &AgentRuntimeList{})
}
