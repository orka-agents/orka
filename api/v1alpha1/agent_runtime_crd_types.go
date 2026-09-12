/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AgentRuntimeContractVersion identifies the Orka-facing runtime contract.
// +kubebuilder:validation:Enum=orka.harness.v2
type AgentRuntimeContractVersion string

const (
	// AgentRuntimeContractHarnessV2 is the session-centric HTTP+NDJSON contract.
	AgentRuntimeContractHarnessV2 AgentRuntimeContractVersion = "orka.harness.v2"
)

// AgentRuntimeDeploymentMode selects how the runtime endpoint is provided.
// +kubebuilder:validation:Enum=external-endpoint
type AgentRuntimeDeploymentMode string

const (
	// AgentRuntimeDeploymentModeExternalEndpoint points at a pre-deployed harness endpoint.
	AgentRuntimeDeploymentModeExternalEndpoint AgentRuntimeDeploymentMode = "external-endpoint"
)

// AgentRuntimeBrokeredToolClass classifies Tool CRDs.
// +kubebuilder:validation:Enum=read;write;coordination
type AgentRuntimeBrokeredToolClass string

const (
	AgentRuntimeBrokeredToolClassRead         AgentRuntimeBrokeredToolClass = "read"
	AgentRuntimeBrokeredToolClassWrite        AgentRuntimeBrokeredToolClass = "write"
	AgentRuntimeBrokeredToolClassCoordination AgentRuntimeBrokeredToolClass = "coordination"
)

// AgentRuntimeReference selects a registered AgentRuntime for a type: agent task.
type AgentRuntimeReference struct {
	// Name is the AgentRuntime name. RuntimeRefs are resolved in the task namespace.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// AgentRuntimeDeploymentSpec configures where Orka reaches the harness runtime.
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.kubernetesRecovery) || (has(self.kubernetesRecovery) && self.kubernetesRecovery == oldSelf.kubernetesRecovery)",message="Kubernetes recovery ownership is immutable once enabled"
type AgentRuntimeDeploymentSpec struct {
	// Mode is the deployment mode. External AgentRuntime registrations are not
	// scaled or recycled by Orka unless KubernetesRecovery is explicitly enabled.
	// +kubebuilder:validation:Required
	Mode AgentRuntimeDeploymentMode `json:"mode"`

	// Endpoint is the base URL for an external harness service. It must not
	// contain credentials, query parameters, or fragments.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^https?://[^\s@?#]+$`
	Endpoint string `json:"endpoint"`

	// KubernetesRecovery opts an exact, same-namespace Deployment into fenced
	// drain and epoch replacement. The Deployment must independently consent
	// with annotation orka.ai/agent-runtime-recovery-uid=<AgentRuntime UID>.
	// Unmanaged endpoints retain the portable, fail-closed recovery contract.
	// +optional
	KubernetesRecovery *AgentRuntimeKubernetesRecoverySpec `json:"kubernetesRecovery,omitempty"`
}

// AgentRuntimeKubernetesRecoverySpec identifies the sole container whose
// private Linux process namespace may prove retirement of an enrolled boot.
// Only single-replica Recreate Deployments are supported. Container termination
// cannot prove retirement of provider-hosted compute such as Foundry sessions.
type AgentRuntimeKubernetesRecoverySpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	DeploymentName string `json:"deploymentName"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	DeploymentUID string `json:"deploymentUID"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	ContainerName string `json:"containerName"`
}

// AgentRuntimeSecretKeyReference identifies one Secret key used for v2 control traffic.
type AgentRuntimeSecretKeyReference struct {
	// Name is the Secret name in the AgentRuntime namespace.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`

	// Key is the Secret data key.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Key string `json:"key"`
}

// AgentRuntimeClientAuth configures controller authentication and per-operation
// authorization. Unknown fields reach admission so removed fields are rejected.
// +kubebuilder:pruning:PreserveUnknownFields
type AgentRuntimeClientAuth struct {
	// ControllerBearerTokenSecretRef supplies the controller bearer token used by
	// authenticated v2 status and mutation endpoints.
	// +kubebuilder:validation:Required
	ControllerBearerTokenSecretRef *AgentRuntimeSecretKeyReference `json:"controllerBearerTokenSecretRef"`

	// OperationCapabilitySecretRef supplies the HMAC secret used to bind every
	// v2 mutation to its exact fence, operation identity, request digest, and expiry.
	// +kubebuilder:validation:Required
	OperationCapabilitySecretRef *AgentRuntimeSecretKeyReference `json:"operationCapabilitySecretRef"`
}

// AgentRuntimeWorkspaceGovernanceMode describes whether Orka may rely on the
// runtime for strict workspace guarantees.
// +kubebuilder:validation:Enum=strict-governed;trusted-non-governed
type AgentRuntimeWorkspaceGovernanceMode string

const (
	// AgentRuntimeWorkspaceGovernanceStrict is eligible only for the exact
	// workspace intent pinned in the immutable runtime profile.
	AgentRuntimeWorkspaceGovernanceStrict AgentRuntimeWorkspaceGovernanceMode = "strict-governed"
	// AgentRuntimeWorkspaceGovernanceTrusted marks an explicitly trusted runtime
	// whose tools and workspace behavior are outside Orka governance.
	AgentRuntimeWorkspaceGovernanceTrusted AgentRuntimeWorkspaceGovernanceMode = "trusted-non-governed"
)

// AgentRuntimeWorkspaceGovernanceCapabilities are static claims advertised by
// /v2/capabilities and exercised by the hostile conformance cycle.
// +kubebuilder:validation:XValidation:rule="self.mode != 'trusted-non-governed' || self.trusted",message="trusted-non-governed runtimes must be explicitly marked trusted"
// +kubebuilder:validation:XValidation:rule="self.mode != 'strict-governed' || !self.trusted",message="strict-governed runtimes must not use the trusted non-governed escape hatch"
// +kubebuilder:validation:XValidation:rule="self.mode != 'strict-governed' || (self.orkaOwnedWorkspaceDeltas && self.promptScopedBrokerAuthorization && self.noDirectSCMPublication && self.orkaOwnedCleanRoomPublication && self.exactInstanceFencing && self.duplicateSafeMutations && self.cancellationSettlement)",message="strict-governed runtimes must claim every strict workspace governance guarantee"
// +kubebuilder:validation:XValidation:rule="self.mode != 'trusted-non-governed' || (!self.orkaOwnedWorkspaceDeltas && !self.promptScopedBrokerAuthorization && !self.noDirectSCMPublication && !self.orkaOwnedCleanRoomPublication && !self.exactInstanceFencing && !self.duplicateSafeMutations && !self.cancellationSettlement)",message="trusted-non-governed runtimes must not claim strict workspace guarantees"
type AgentRuntimeWorkspaceGovernanceCapabilities struct {
	// Mode selects strict Orka governance or an explicit trusted escape hatch.
	// +kubebuilder:validation:Required
	Mode AgentRuntimeWorkspaceGovernanceMode `json:"mode"`

	// Trusted must be true only for trusted-non-governed runtimes. Such runtimes
	// are ineligible for Tasks requesting strict read or write guarantees.
	Trusted bool `json:"trusted"`

	OrkaOwnedWorkspaceDeltas        bool `json:"orkaOwnedWorkspaceDeltas"`
	PromptScopedBrokerAuthorization bool `json:"promptScopedBrokerAuthorization"`
	NoDirectSCMPublication          bool `json:"noDirectSCMPublication"`
	OrkaOwnedCleanRoomPublication   bool `json:"orkaOwnedCleanRoomPublication"`
	ExactInstanceFencing            bool `json:"exactInstanceFencing"`
	DuplicateSafeMutations          bool `json:"duplicateSafeMutations"`
	CancellationSettlement          bool `json:"cancellationSettlement"`
}

// Strict reports whether every strict workspace governance guarantee is claimed.
func (c AgentRuntimeWorkspaceGovernanceCapabilities) Strict() bool {
	return c.Mode == AgentRuntimeWorkspaceGovernanceStrict && !c.Trusted &&
		c.OrkaOwnedWorkspaceDeltas && c.PromptScopedBrokerAuthorization &&
		c.NoDirectSCMPublication && c.OrkaOwnedCleanRoomPublication &&
		c.ExactInstanceFencing && c.DuplicateSafeMutations && c.CancellationSettlement
}

// AgentRuntimeProtocolLimits pins the exact bounded v2 limits expected from the runtime.
type AgentRuntimeProtocolLimits struct {
	// +kubebuilder:validation:Minimum=1
	MaxResidentSessions int32 `json:"maxResidentSessions"`
	// +kubebuilder:validation:Minimum=1
	MaxConcurrentPrompts int32 `json:"maxConcurrentPrompts"`
	// +kubebuilder:validation:Minimum=1
	MaxRequestBytes int32 `json:"maxRequestBytes"`
	// +kubebuilder:validation:Minimum=1
	MaxEventLineBytes int32 `json:"maxEventLineBytes"`
	// +kubebuilder:validation:Minimum=1
	MaxTerminalResultBytes int32 `json:"maxTerminalResultBytes"`
	// +kubebuilder:validation:Minimum=1
	MaxBufferedEvents int32 `json:"maxBufferedEvents"`
	// +kubebuilder:validation:Minimum=1
	MaxUpdateEventsPerSecond int32 `json:"maxUpdateEventsPerSecond"`
	// +kubebuilder:validation:Minimum=1
	MinPromptLeaseMillis int64 `json:"minPromptLeaseMillis"`
	// +kubebuilder:validation:Minimum=1
	MaxPromptLeaseMillis int64 `json:"maxPromptLeaseMillis"`
	// +kubebuilder:validation:Minimum=1
	MaxPendingPermissions int32 `json:"maxPendingPermissions"`
	// +kubebuilder:validation:Minimum=1
	MaxWorkspaceDeltaBytes int64 `json:"maxWorkspaceDeltaBytes"`
}

// AgentRuntimeProfileSpec pins one immutable, single-adapter v2 runtime profile.
type AgentRuntimeProfileSpec struct {
	// Digest is the canonical orka.harness.v2 runtime-profile digest.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	Digest string `json:"digest"`

	// DigestSchemaVersion identifies the canonical profile digest schema.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=1
	DigestSchemaVersion int32 `json:"digestSchemaVersion"`

	// ACPProfile is the reviewed ACP profile.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=acp.v1
	ACPProfile string `json:"acpProfile"`

	// AdapterName identifies the sole adapter contained by this external profile.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	AdapterName string `json:"adapterName"`

	// AdapterDigest pins the adapter/CLI artifact set.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	AdapterDigest string `json:"adapterDigest"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	ProviderKind string `json:"providerKind"`
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	Model string `json:"model"`

	// ModelLimits pins optional reviewed model token capacities.
	// +optional
	ModelLimits *ModelTokenLimits `json:"modelLimits,omitempty"`

	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	AgentConfigurationDigest string `json:"agentConfigurationDigest"`
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	ToolPolicyDigest string `json:"toolPolicyDigest"`
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	ApprovalPolicyDigest string `json:"approvalPolicyDigest"`
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	MCPConfigurationDigest string `json:"mcpConfigurationDigest"`

	// WorkspaceIntent is the one immutable strict intent represented by this profile.
	// +kubebuilder:validation:Required
	WorkspaceIntent WorkspaceIntent `json:"workspaceIntent"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	ProxyCredentialRole string `json:"proxyCredentialRole"`
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	ProxyCredentialScope string `json:"proxyCredentialScope"`
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	ResourceClass string `json:"resourceClass"`
}

// AgentRuntimeMCPPolicySpec materializes the exact non-secret MCP policy whose
// digests are pinned by the external runtime profile.
type AgentRuntimeMCPPolicySpec struct {
	// AllowedTools is the canonical sorted allowlist exposed through the
	// prompt-scoped MCP broker.
	// +kubebuilder:validation:MaxItems=128
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=128
	AllowedTools []string `json:"allowedTools"`

	// DisallowedTools is the canonical sorted deny list applied after the allowlist.
	// +kubebuilder:validation:MaxItems=128
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=128
	DisallowedTools []string `json:"disallowedTools"`

	// AllowBash controls whether an allowed tool named Bash may execute.
	AllowBash bool `json:"allowBash"`

	// ApprovalRequiredTools must remain an explicit empty list until the
	// controller owns external-runtime permission review.
	// +kubebuilder:validation:MaxItems=0
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=128
	ApprovalRequiredTools []string `json:"approvalRequiredTools"`
}

// AgentRuntimeCapabilitiesSpec pins runtime capability claims. Unknown fields
// reach admission so removed fields are rejected.
// +kubebuilder:pruning:PreserveUnknownFields
type AgentRuntimeCapabilitiesSpec struct {
	// RuntimeInstanceID is the immutable external supervisor instance expected from
	// authenticated /v2/status and every conformance response.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	RuntimeInstanceID string `json:"runtimeInstanceID,omitempty"`

	// Profile is the exact immutable profile accepted by v2 session creation.
	// +optional
	Profile *AgentRuntimeProfileSpec `json:"profile,omitempty"`

	// MCPPolicy materializes the exact policy represented by the profile's MCP
	// digests so readiness conformance and Task dispatch use identical inputs.
	// +optional
	MCPPolicy *AgentRuntimeMCPPolicySpec `json:"mcpPolicy,omitempty"`

	// Limits must exactly match /v2/capabilities.
	// +optional
	Limits *AgentRuntimeProtocolLimits `json:"limits,omitempty"`

	// SupportsDrain must exactly match the static v2 capability claim.
	// +optional
	SupportsDrain bool `json:"supportsDrain,omitempty"`

	// SupportsPublicationFinalization must exactly match the static v2 capability claim.
	// +optional
	SupportsPublicationFinalization bool `json:"supportsPublicationFinalization,omitempty"`

	// WorkspaceGovernance must exactly match the static v2 capability claim.
	// +optional
	WorkspaceGovernance *AgentRuntimeWorkspaceGovernanceCapabilities `json:"workspaceGovernance,omitempty"`
}

// SupportsStrictWorkspaceIntent returns true only for the exact intent pinned
// by a fully governed v2 profile. Trusted/non-governed runtimes return false.
func (c AgentRuntimeCapabilitiesSpec) SupportsStrictWorkspaceIntent(intent WorkspaceIntent) bool {
	if c.WorkspaceGovernance == nil || c.Profile == nil {
		return false
	}
	return c.WorkspaceGovernance.Strict() && c.Profile.WorkspaceIntent == intent &&
		(intent != WorkspaceIntentWrite || c.SupportsPublicationFinalization)
}

// ValidateStrictWorkspaceIntent rejects trusted/non-governed runtimes and exact-profile intent mismatches.
func (c AgentRuntimeCapabilitiesSpec) ValidateStrictWorkspaceIntent(intent WorkspaceIntent) error {
	if intent != WorkspaceIntentRead && intent != WorkspaceIntentWrite {
		return fmt.Errorf("unsupported strict workspace intent %q", intent)
	}
	if c.WorkspaceGovernance == nil || c.Profile == nil {
		return fmt.Errorf("AgentRuntime does not pin the v2 workspace governance capabilities required for strict %q workspace intent", intent)
	}
	if c.WorkspaceGovernance.Mode == AgentRuntimeWorkspaceGovernanceTrusted {
		return fmt.Errorf("trusted-non-governed AgentRuntime cannot satisfy strict %q workspace intent", intent)
	}
	if !c.WorkspaceGovernance.Strict() {
		return fmt.Errorf("AgentRuntime does not provide all strict workspace governance guarantees")
	}
	if c.Profile.WorkspaceIntent != intent {
		return fmt.Errorf("AgentRuntime profile is pinned to workspace intent %q, not %q", c.Profile.WorkspaceIntent, intent)
	}
	if intent == WorkspaceIntentWrite && !c.SupportsPublicationFinalization {
		return fmt.Errorf("AgentRuntime does not support controller-owned RuntimeSession publication finalization required for write workspaces")
	}
	return nil
}

// AgentRuntimeRegistrySpec defines the desired state of a registered Orka harness runtime.
// +kubebuilder:validation:XValidation:rule="self.contractVersion == oldSelf.contractVersion",message="contractVersion is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.capabilities.runtimeInstanceID) && has(self.capabilities.profile) && has(self.capabilities.mcpPolicy) && has(self.capabilities.limits) && has(self.capabilities.workspaceGovernance)",message="orka.harness.v2 requires pinned instance, profile, MCP policy, limits, and workspace governance capabilities"
type AgentRuntimeRegistrySpec struct {
	// ContractVersion is the immutable Orka harness contract this runtime implements.
	// +kubebuilder:validation:Required
	ContractVersion *AgentRuntimeContractVersion `json:"contractVersion"`

	// Deployment identifies the runtime endpoint provider.
	// +kubebuilder:validation:Required
	Deployment AgentRuntimeDeploymentSpec `json:"deployment"`

	// ClientAuth configures controller authentication and mutation authorization.
	// +kubebuilder:validation:Required
	ClientAuth AgentRuntimeClientAuth `json:"clientAuth"`

	// Capabilities pins the exact instance, profile, limits, and governance claims.
	// +kubebuilder:validation:Required
	Capabilities *AgentRuntimeCapabilitiesSpec `json:"capabilities"`
}

// AgentRuntimeObservedCapabilities records sanitized conformance data. Unknown
// fields reach admission so removed fields are rejected.
// +kubebuilder:pruning:PreserveUnknownFields
type AgentRuntimeObservedCapabilities struct {
	// +kubebuilder:validation:Enum=orka.harness.v2
	ProtocolVersion            string `json:"protocolVersion,omitempty"`
	Transport                  string `json:"transport,omitempty"`
	ACPVersion                 string `json:"acpVersion,omitempty"`
	RuntimeInstanceID          string `json:"runtimeInstanceID,omitempty"`
	SupervisorBootID           string `json:"supervisorBootID,omitempty"`
	ControllerEpoch            int64  `json:"controllerEpoch,omitempty"`
	RuntimePoolUID             string `json:"runtimePoolUID,omitempty"`
	RuntimePoolGeneration      int64  `json:"runtimePoolGeneration,omitempty"`
	RuntimeProfileDigest       string `json:"runtimeProfileDigest,omitempty"`
	ProfileDigestSchemaVersion int32  `json:"profileDigestSchemaVersion,omitempty"`
	AdapterName                string `json:"adapterName,omitempty"`
	AdapterDigest              string `json:"adapterDigest,omitempty"`
	ProviderKind               string `json:"providerKind,omitempty"`
	Model                      string `json:"model,omitempty"`
	// MCPToolDescriptorDigest records the exact derived descriptor set sent by
	// the controller during the most recent v2 conformance probe.
	MCPToolDescriptorDigest string `json:"mcpToolDescriptorDigest,omitempty"`
	// Limits records the v2 protocol bounds.
	// +optional
	Limits                          *AgentRuntimeProtocolLimits `json:"limits,omitempty"`
	SupportsDrain                   bool                        `json:"supportsDrain,omitempty"`
	SupportsPublicationFinalization bool                        `json:"supportsPublicationFinalization,omitempty"`
	// WorkspaceGovernance records the v2 workspace guarantees.
	// +optional
	WorkspaceGovernance *AgentRuntimeWorkspaceGovernanceCapabilities `json:"workspaceGovernance,omitempty"`
	Lifecycle           string                                       `json:"lifecycle,omitempty"`
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

	// ObservedControllerAuthRefResourceVersion is the bearer Secret version used
	// by the last successful or failed authenticated conformance probe.
	// +optional
	ObservedControllerAuthRefResourceVersion string `json:"observedControllerAuthRefResourceVersion,omitempty"`

	// ObservedOperationCapabilityRefResourceVersion is the HMAC Secret version used
	// by the last mutation conformance probe.
	// +optional
	ObservedOperationCapabilityRefResourceVersion string `json:"observedOperationCapabilityRefResourceVersion,omitempty"`

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
// +kubebuilder:printcolumn:name="Instance",type=string,JSONPath=`.status.observedCapabilities.runtimeInstanceID`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AgentRuntime is the Schema for registered external Orka harness runtimes.
type AgentRuntime struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentRuntimeRegistrySpec `json:"spec,omitempty"`
	Status AgentRuntimeStatus       `json:"status,omitempty"`
}

// RegisteredContractVersion returns the explicit contract selector, or empty
// when absent. Callers must reject an absent contract.
func (in *AgentRuntime) RegisteredContractVersion() AgentRuntimeContractVersion {
	if in == nil || in.Spec.ContractVersion == nil {
		return ""
	}
	return *in.Spec.ContractVersion
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
