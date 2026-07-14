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

// AgentRuntimeTransportSecurity declares how Orka secures the connection to an AgentRuntime endpoint.
// +kubebuilder:validation:Enum=tls;insecure-cluster-local-http
type AgentRuntimeTransportSecurity string

const (
	// AgentRuntimeTransportSecurityTLS requires an HTTPS AgentRuntime endpoint.
	AgentRuntimeTransportSecurityTLS AgentRuntimeTransportSecurity = "tls"
	// AgentRuntimeTransportSecurityInsecureClusterLocalHTTP permits HTTP only for a selector-backed,
	// non-ExternalName Service in the same namespace as the AgentRuntime.
	AgentRuntimeTransportSecurityInsecureClusterLocalHTTP AgentRuntimeTransportSecurity = "insecure-cluster-local-http"
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

// AgentRuntimeBrokeredToolClass declares which classes of Orka-brokered tools a runtime can request.
// +kubebuilder:validation:Enum=read;write;coordination
type AgentRuntimeBrokeredToolClass string

const (
	// AgentRuntimeBrokeredToolClassRead covers read-only evidence and lookup tools.
	AgentRuntimeBrokeredToolClassRead AgentRuntimeBrokeredToolClass = "read"
	// AgentRuntimeBrokeredToolClassWrite covers consequential tools that may require approval and idempotency.
	AgentRuntimeBrokeredToolClassWrite AgentRuntimeBrokeredToolClass = "write"
	// AgentRuntimeBrokeredToolClassCoordination covers Orka coordination tools such as delegate_task/wait_for_tasks.
	AgentRuntimeBrokeredToolClassCoordination AgentRuntimeBrokeredToolClass = "coordination"
)

// AgentRuntimeReference selects a registered AgentRuntime for a type: agent task.
type AgentRuntimeReference struct {
	// Name is the AgentRuntime name. RuntimeRefs are resolved in the task namespace.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// AgentRuntimeDeploymentSpec configures where Orka reaches the harness runtime.
// +kubebuilder:validation:XValidation:rule="!has(self.transportSecurity) || self.transportSecurity != 'tls' || self.endpoint.startsWith('https://')",message="deployment.endpoint must use https when transportSecurity is tls"
// +kubebuilder:validation:XValidation:rule="!has(self.transportSecurity) || self.transportSecurity != 'insecure-cluster-local-http' || self.endpoint.startsWith('http://')",message="deployment.endpoint must use http when transportSecurity is insecure-cluster-local-http"
type AgentRuntimeDeploymentSpec struct {
	// Mode is the deployment mode. The first milestone supports external endpoints only.
	// +kubebuilder:validation:Required
	Mode AgentRuntimeDeploymentMode `json:"mode"`

	// Endpoint is the base URL for a pre-deployed or external orka.harness.v1 service.
	// It must not contain credentials; bearer auth is configured via clientAuth.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^https?://[^\s@?#]+$`
	Endpoint string `json:"endpoint"`

	// TransportSecurity declares the required endpoint transport. An unmarked
	// omitted value is treated as TLS. The supported CRD upgrade helper marks
	// objects created before transport policy existed so the controller can
	// backfill HTTPS as tls and validated same-namespace HTTP as
	// insecure-cluster-local-http.
	// insecure-cluster-local-http is an explicit opt-in limited to a selector-backed,
	// non-ExternalName Service in the same namespace as the AgentRuntime, addressed
	// with its service.namespace.svc.<cluster-domain> FQDN.
	// +optional
	TransportSecurity AgentRuntimeTransportSecurity `json:"transportSecurity,omitempty"`
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
	// The referenced Secret must opt in with label orka.ai/agent-runtime-auth=true,
	// may set orka.ai/agent-runtime-name=<runtime> to restrict use to one AgentRuntime,
	// and must set annotation orka.ai/agent-runtime-endpoint=<deployment.endpoint> to bind the token to one endpoint.
	// +kubebuilder:validation:Required
	BearerAuthRef AgentRuntimeBearerAuthReference `json:"bearerTokenSecretRef"`
}

// AgentRuntimeCapabilitiesSpec describes required capabilities for runtime readiness.
type AgentRuntimeCapabilitiesSpec struct {
	// ToolExecutionModes lists tool execution modes the runtime must advertise.
	// +listType=set
	// +optional
	ToolExecutionModes []AgentRuntimeToolExecutionMode `json:"toolExecutionModes,omitempty"`

	// BrokeredToolClasses lists brokered tool classes the runtime must advertise when brokered mode is required.
	// +listType=set
	// +optional
	BrokeredToolClasses []AgentRuntimeBrokeredToolClass `json:"brokeredToolClasses,omitempty"`

	// SupportsCancel requires the runtime to advertise cancellation support when true.
	// +optional
	SupportsCancel *bool `json:"supportsCancel,omitempty"`

	// SupportsRuntimeSessions requires the runtime to advertise stable runtime sessions when true.
	// +optional
	SupportsRuntimeSessions *bool `json:"supportsRuntimeSessions,omitempty"`

	// SupportsContinuation requires the runtime to advertise continuation after Orka-brokered tool results when true.
	// +optional
	SupportsContinuation *bool `json:"supportsContinuation,omitempty"`

	// SupportsArtifacts requires the runtime to advertise artifact/result reference support when true.
	// +optional
	SupportsArtifacts *bool `json:"supportsArtifacts,omitempty"`
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

	// BrokeredToolClasses are the brokered tool classes advertised by /v1/capabilities.
	// +listType=set
	// +optional
	BrokeredToolClasses []AgentRuntimeBrokeredToolClass `json:"brokeredToolClasses,omitempty"`

	// SupportsCancel reports whether the runtime advertises cancellation support.
	// +optional
	SupportsCancel bool `json:"supportsCancel,omitempty"`

	// SupportsRuntimeSessions reports whether the runtime advertises runtime-session support.
	// +optional
	SupportsRuntimeSessions bool `json:"supportsRuntimeSessions,omitempty"`

	// SupportsContinuation reports whether the runtime advertises continuation support.
	// +optional
	SupportsContinuation bool `json:"supportsContinuation,omitempty"`

	// SupportsArtifacts reports whether the runtime advertises artifact/result reference support.
	// +optional
	SupportsArtifacts bool `json:"supportsArtifacts,omitempty"`

	// SupportsSuspend reports whether the runtime advertises suspend support.
	// +optional
	SupportsSuspend bool `json:"supportsSuspend,omitempty"`

	// SupportsWorkspaceSnapshot reports whether the runtime advertises workspace snapshots.
	// +optional
	SupportsWorkspaceSnapshot bool `json:"supportsWorkspaceSnapshot,omitempty"`

	// MaxConcurrentTurns is the advertised concurrency ceiling.
	// +optional
	MaxConcurrentTurns int `json:"maxConcurrentTurns,omitempty"`

	// MaxTurnSeconds is the advertised per-turn duration ceiling.
	// +optional
	MaxTurnSeconds int `json:"maxTurnSeconds,omitempty"`

	// MaxOutputBytes is the advertised maximum output payload size.
	// +optional
	MaxOutputBytes int64 `json:"maxOutputBytes,omitempty"`
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
