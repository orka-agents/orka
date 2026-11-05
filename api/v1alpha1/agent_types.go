/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import (
	"encoding/json"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AgentSpec defines the desired state of Agent
// +kubebuilder:validation:XValidation:rule="!has(self.execution) || !has(self.execution.workspace)",message="execution.workspace is only supported on Task specs"
type AgentSpec struct {
	// ProviderRef references a Provider CRD for LLM configuration
	// If set, model.provider is optional (inherited from Provider)
	// +optional
	ProviderRef *ProviderReference `json:"providerRef,omitempty"`

	// Model defines the LLM model configuration
	// Provider field is optional if providerRef is set
	// +optional
	Model *ModelConfig `json:"model,omitempty"`

	// SystemPrompt defines the system prompt configuration
	// +optional
	SystemPrompt *PromptSource `json:"systemPrompt,omitempty"`

	// Soul supplies persistent persona and communication defaults, separate from the role prompt.
	// Supported by AI workers and built-in harness v2 runtimes, not runtimeRef runtimes.
	// +optional
	Soul *SoulSource `json:"soul,omitempty"`

	// Tools lists the default tools available to this agent
	// +optional
	Tools []ToolReference `json:"tools,omitempty"`

	// Skills lists the default skills for this agent
	// +optional
	Skills []SkillReference `json:"skills,omitempty"`

	// Resources defines the resource limits for tasks using this agent
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// Execution defines default worker pod runtime and placement settings.
	// +optional
	Execution *ExecutionSpec `json:"execution,omitempty"`

	// SecretRef references a Secret containing LLM API keys
	// +optional
	SecretRef *corev1.LocalObjectReference `json:"secretRef,omitempty"`

	// Session defines session configuration defaults
	// +optional
	Session *SessionConfig `json:"session,omitempty"`

	// Coordination enables agent-to-agent delegation
	// +optional
	Coordination *CoordinationConfig `json:"coordination,omitempty"`

	// Runtime configures this Agent for external CLI runtimes (type: agent tasks).
	// When set, this Agent is for type: agent tasks only (mutually exclusive with providerRef).
	// +optional
	Runtime *AgentCLIRuntime `json:"runtime,omitempty"`

	// Identity binds this Agent to one external identity that its outbound
	// calls can act as. Setting or changing it requires the `bind` verb on
	// agentidentities/<issuer> for that identity. An identity-bound Agent must
	// set runtime.ceiling.allowedTools, and Tasks using it require Agent `use`
	// authorization on the controller.
	// +optional
	Identity *AgentIdentity `json:"identity,omitempty"`

	// TTLAfterLastTask defines how long the agent persists after its last task completes.
	// When set and no tasks are active, the agent is deleted after this duration.
	// Zero means the agent is never auto-deleted (permanent). Default is no TTL (permanent).
	// +optional
	TTLAfterLastTask *metav1.Duration `json:"ttlAfterLastTask,omitempty"`
}

// MarshalJSON preserves an explicitly empty native tool policy instead of
// omitting it. Nil and nonempty lists retain their existing wire representation.
func (in AgentSpec) MarshalJSON() ([]byte, error) {
	type agentSpecJSON AgentSpec
	if in.Tools == nil || len(in.Tools) > 0 {
		return json.Marshal(agentSpecJSON(in))
	}
	return json.Marshal(struct {
		agentSpecJSON
		Tools []ToolReference `json:"tools"`
	}{agentSpecJSON: agentSpecJSON(in), Tools: in.Tools})
}

// AgentCLIRuntime defines agent CLI runtime configuration for an Agent.
// Unknown fields reach admission so removed fields are rejected.
// +kubebuilder:pruning:PreserveUnknownFields
// +kubebuilder:validation:XValidation:rule="has(self.type) != has(self.runtimeRef)",message="exactly one of type or runtimeRef is required"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.contractVersion) || (has(self.contractVersion) && self.contractVersion == oldSelf.contractVersion)",message="runtime.contractVersion is immutable once set"
// +kubebuilder:validation:XValidation:rule="!has(self.contractVersion) || has(self.type)",message="runtime.contractVersion applies only to built-in runtime types; runtimeRef derives the protocol from the referenced AgentRuntime"
// +kubebuilder:validation:XValidation:rule="!has(self.toolboxes) || self.toolboxes.size() == 0 || has(self.type)",message="runtime.toolboxes are supported only on built-in runtime types; external runtimeRef runtimes cannot mount toolboxes"
type AgentCLIRuntime struct {
	// Type specifies which built-in CLI runtime to use. Use runtimeRef for admin-registered custom runtimes.
	// +optional
	Type AgentRuntimeType `json:"type,omitempty"`

	// ContractVersion is the harness protocol selector for built-in runtime
	// types. Only orka.harness.v2 is supported, so an omitted selector means
	// orka.harness.v2. Once written, the value never changes. runtime.type
	// alone is never protocol evidence.
	// +optional
	ContractVersion *AgentRuntimeContractVersion `json:"contractVersion,omitempty"`

	// RuntimeRef selects an admin-governed AgentRuntime for custom/BYO harness runtimes.
	// +optional
	RuntimeRef *AgentRuntimeReference `json:"runtimeRef,omitempty"`

	// DefaultMaxTurns is the default maximum agent loop iterations for tasks using this Agent
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000
	// +optional
	DefaultMaxTurns *int32 `json:"defaultMaxTurns,omitempty"`

	// DefaultAllowedTools lists the default tools allowed for tasks using this Agent
	// +optional
	DefaultAllowedTools []string `json:"defaultAllowedTools,omitempty"`

	// DefaultAllowBash controls whether bash is allowed by default for tasks using this Agent.
	// Defaults to true if not specified.
	// +optional
	DefaultAllowBash *bool `json:"defaultAllowBash,omitempty"`

	// DefaultReasoningEffort configures the CLI runtime reasoning effort for tasks using this Agent.
	// Runtime adapters reject values they do not support (none is Codex-only; max is Claude-only).
	// +kubebuilder:validation:Enum=none;low;medium;high;xhigh;max
	// +optional
	DefaultReasoningEffort string `json:"defaultReasoningEffort,omitempty"`

	// Ceiling is the upper bound of what any Task using this Agent may
	// request. Unlike the default* fields, which a Task can replace, a Task
	// that asks for more than the ceiling (directly or through the Agent
	// defaults) fails before it is bound. Omitted means no ceiling.
	// +optional
	Ceiling *AgentRuntimeCeiling `json:"ceiling,omitempty"`

	// Toolboxes lists extra command-line tool images for built-in runtimes.
	// Each toolbox folder is bound read-only into the runtime Pod at the same
	// path and its pathEntries are appended, in order, to the agent's PATH.
	// Nothing from a toolbox runs with elevated permissions. The feature is
	// off by default and the controller restricts images to allowed registries.
	// +kubebuilder:validation:MaxItems=4
	// +optional
	Toolboxes []AgentToolbox `json:"toolboxes,omitempty"`
}

// AgentToolbox is a container image whose files are added, read-only, to the
// runtime Pod so the agent can run the tools inside it.
type AgentToolbox struct {
	// Image must be a fully qualified reference pinned by digest:
	// <registry>/<repository>@sha256:<64 hex>.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern=`^[^\s@:]+(:[0-9]{1,5})?(/[^\s@:]+)+@sha256:[a-f0-9]{64}$`
	Image string `json:"image"`

	// MountPath is the absolute folder inside the toolbox image that holds the
	// tools. Orka makes it appear at the same path in the runtime container.
	// It must be exactly /home/linuxbrew/.linuxbrew or /opt/<name>, where
	// <name> is not one the runtime images already use.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=5
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^(/opt/[A-Za-z0-9][A-Za-z0-9._-]{0,127}|/home/linuxbrew/\.linuxbrew)$`
	MountPath string `json:"mountPath"`

	// PathEntries are folders relative to MountPath that are appended, in
	// order, to the agent's PATH. Example: ["bin"].
	// +kubebuilder:validation:MaxItems=8
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=256
	// +kubebuilder:validation:items:Pattern=`^[A-Za-z0-9._-][A-Za-z0-9._/-]*$`
	// +optional
	PathEntries []string `json:"pathEntries,omitempty"`
}

// AgentRuntimeCeiling bounds the tool policy and iteration budget that Tasks
// using an Agent may run with. Each omitted field leaves that dimension
// unbounded. Orka-injected coordination messaging tools for delegated children
// are platform plumbing and are not counted against AllowedTools.
type AgentRuntimeCeiling struct {
	// AllowedTools is the largest tool set a Task may run with. Tool names
	// ignore case. An entry without a scope, such as "Read", also permits
	// scoped requests such as "Read(/workspace/**)"; a scoped entry permits
	// only that exact, case-sensitive scope. An explicitly empty list permits
	// no tools.
	// +kubebuilder:validation:MaxItems=256
	// +kubebuilder:validation:items:MaxLength=256
	// +optional
	AllowedTools []string `json:"allowedTools,omitempty"`

	// AllowBash false forbids shell access for every Task using this Agent.
	// +optional
	AllowBash *bool `json:"allowBash,omitempty"`

	// MaxTurns is the largest agent loop iteration budget a Task may use.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000
	// +optional
	MaxTurns *int32 `json:"maxTurns,omitempty"`
}

// MarshalJSON preserves an explicitly empty AllowedTools ceiling, which
// permits no tools, instead of omitting it, which leaves tools unbounded.
func (in AgentRuntimeCeiling) MarshalJSON() ([]byte, error) {
	type agentRuntimeCeilingJSON struct {
		AllowedTools *[]string `json:"allowedTools,omitempty"`
		AllowBash    *bool     `json:"allowBash,omitempty"`
		MaxTurns     *int32    `json:"maxTurns,omitempty"`
	}
	var allowedTools *[]string
	if in.AllowedTools != nil {
		tools := append([]string{}, in.AllowedTools...)
		allowedTools = &tools
	}
	return json.Marshal(agentRuntimeCeilingJSON{
		AllowedTools: allowedTools,
		AllowBash:    in.AllowBash,
		MaxTurns:     in.MaxTurns,
	})
}

// MarshalJSON preserves the distinction between an omitted tool allowlist and
// an explicitly empty deny-all allowlist. The standard omitempty handling for
// slices would otherwise serialize both states as omission.
func (in AgentCLIRuntime) MarshalJSON() ([]byte, error) {
	type agentCLIRuntimeJSON struct {
		Type                   AgentRuntimeType             `json:"type,omitempty"`
		ContractVersion        *AgentRuntimeContractVersion `json:"contractVersion,omitempty"`
		RuntimeRef             *AgentRuntimeReference       `json:"runtimeRef,omitempty"`
		DefaultMaxTurns        *int32                       `json:"defaultMaxTurns,omitempty"`
		DefaultAllowedTools    *[]string                    `json:"defaultAllowedTools,omitempty"`
		DefaultAllowBash       *bool                        `json:"defaultAllowBash,omitempty"`
		DefaultReasoningEffort string                       `json:"defaultReasoningEffort,omitempty"`
		Ceiling                *AgentRuntimeCeiling         `json:"ceiling,omitempty"`
		Toolboxes              []AgentToolbox               `json:"toolboxes,omitempty"`
	}
	var defaultAllowedTools *[]string
	if in.DefaultAllowedTools != nil {
		tools := append([]string{}, in.DefaultAllowedTools...)
		defaultAllowedTools = &tools
	}
	return json.Marshal(agentCLIRuntimeJSON{
		Type:                   in.Type,
		ContractVersion:        in.ContractVersion,
		RuntimeRef:             in.RuntimeRef,
		DefaultMaxTurns:        in.DefaultMaxTurns,
		DefaultAllowedTools:    defaultAllowedTools,
		DefaultAllowBash:       in.DefaultAllowBash,
		DefaultReasoningEffort: in.DefaultReasoningEffort,
		Ceiling:                in.Ceiling,
		Toolboxes:              in.Toolboxes,
	})
}

// BuiltInContractVersion returns the Agent's explicit built-in harness
// protocol, or empty when absent. Callers must reject an absent contract.
// AgentIdentityIssuerEntra names Microsoft Entra Agent ID identities.
const AgentIdentityIssuerEntra = "entra"

// AgentIdentity selects exactly one issuer-typed external identity.
// +kubebuilder:validation:XValidation:rule="has(self.entra)",message="identity must set exactly one issuer"
type AgentIdentity struct {
	// Entra binds a Microsoft Entra Agent ID agent identity.
	// +optional
	Entra *EntraAgentIdentity `json:"entra,omitempty"`
}

// EntraAgentIdentity references one Entra agent identity under the blueprint
// configured on the controller.
type EntraAgentIdentity struct {
	// AgentIdentityID is the agent identity's client (application) ID.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`
	AgentIdentityID string `json:"agentIdentityID"`
}

// IdentityRef returns the Agent's issuer type and identity ID. Identity IDs
// compare case-insensitively, so the ID is lowercased.
func (in *Agent) IdentityRef() (issuer, id string, ok bool) {
	if in == nil || in.Spec.Identity == nil || in.Spec.Identity.Entra == nil {
		return "", "", false
	}
	id = strings.ToLower(strings.TrimSpace(in.Spec.Identity.Entra.AgentIdentityID))
	if id == "" {
		return "", "", false
	}
	return AgentIdentityIssuerEntra, id, true
}

func (in *Agent) BuiltInContractVersion() AgentRuntimeContractVersion {
	if in == nil || in.Spec.Runtime == nil || in.Spec.Runtime.ContractVersion == nil {
		return ""
	}
	return *in.Spec.Runtime.ContractVersion
}

// ModelFallback defines a fallback provider configuration
type ModelFallback struct {
	// ProviderRef is the name of a Provider CRD to fall back to
	// +kubebuilder:validation:Required
	ProviderRef string `json:"providerRef"`

	// Model to use with this provider (optional, uses provider's defaultModel if empty)
	// +optional
	Model string `json:"model,omitempty"`
}

// ModelConfig defines LLM model configuration
type ModelConfig struct {
	// Provider is the LLM provider (anthropic, openai)
	// Optional if providerRef is set on the Agent
	// +kubebuilder:validation:Enum=anthropic;openai
	// +optional
	Provider string `json:"provider,omitempty"`

	// Name is the model identifier
	// Optional if providerRef is set and Provider has defaultModel
	// +optional
	Name string `json:"name,omitempty"`

	// Temperature controls randomness in generation
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=2
	// +optional
	Temperature *float64 `json:"temperature,omitempty"`

	// ContextWindow is the reviewed model context capacity in tokens. Built-in
	// runtimes that manage their own compaction require this value explicitly.
	// +kubebuilder:validation:Minimum=1
	// +optional
	ContextWindow *int32 `json:"contextWindow,omitempty"`

	// MaxTokens limits the response length. OpenCode validates positive reviewed
	// limits at its runtime-specific admission boundary; existing Agent objects
	// may retain the legacy zero value, which selects the worker default.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxTokens *int32 `json:"maxTokens,omitempty"`

	// Fallbacks defines alternative providers to try when the primary fails.
	// Each fallback specifies a Provider CRD and optional model override.
	// +optional
	Fallbacks []ModelFallback `json:"fallbacks,omitempty"`
}

// PromptSource defines where to get a prompt from
// +kubebuilder:validation:XValidation:rule="!(has(self.inline) && self.inline.size() > 0 && has(self.configMapRef))",message="system prompt must use only one of inline or configMapRef"
type PromptSource struct {
	// Inline is the inline prompt text
	// +optional
	Inline string `json:"inline,omitempty"`

	// ConfigMapRef references a ConfigMap containing the prompt
	// +optional
	ConfigMapRef *ConfigMapKeySelector `json:"configMapRef,omitempty"`
}

// SoulSource is non-secret, versioned persona text. ConfigMap sources require an
// expected content digest so replacing a named ConfigMap cannot publish a new soul.
// +kubebuilder:validation:XValidation:rule="(has(self.inline) && self.inline.size() > 0) != has(self.configMapRef)",message="soul must use exactly one of inline or configMapRef"
// +kubebuilder:validation:XValidation:rule="!has(self.configMapRef) || (has(self.digest) && self.digest.size() > 0)",message="ConfigMap-backed souls require an expected digest"
type SoulSource struct {
	// Inline is UTF-8 Markdown, limited to 8192 bytes by the controller.
	// +kubebuilder:validation:MaxLength=8192
	// +optional
	Inline string `json:"inline,omitempty"`

	// ConfigMapRef selects non-secret text in the Agent's namespace.
	// +optional
	ConfigMapRef *ConfigMapKeySelector `json:"configMapRef,omitempty"`

	// Digest is the expected SHA-256 digest of the exact UTF-8 source bytes.
	// Required for ConfigMap sources; optional for inline text.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	// +kubebuilder:validation:MaxLength=71
	// +optional
	Digest string `json:"digest,omitempty"`
}

// ConfigMapKeySelector selects a key from a ConfigMap
type ConfigMapKeySelector struct {
	// Name is the name of the ConfigMap
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Key is the key within the ConfigMap
	// +kubebuilder:validation:Required
	Key string `json:"key"`
}

// ToolReference references a tool for an agent
type ToolReference struct {
	// Name is the tool name (built-in or Tool CRD name)
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Enabled indicates if the tool is enabled (default: true)
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
}

// SessionConfig defines session behavior defaults
type SessionConfig struct {
	// MaxMessages is the maximum messages to load from session
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=50
	// +optional
	MaxMessages int32 `json:"maxMessages,omitempty"`
}

// CoordinationConfig enables agent-to-agent delegation
type CoordinationConfig struct {
	// Enabled indicates if coordination is enabled
	// +kubebuilder:default=false
	Enabled bool `json:"enabled"`

	// AllowedAgents lists agents this agent can delegate to
	// +optional
	AllowedAgents []AllowedAgent `json:"allowedAgents,omitempty"`

	// MaxConcurrentChildren limits concurrent child tasks
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=5
	// +optional
	MaxConcurrentChildren int32 `json:"maxConcurrentChildren,omitempty"`

	// MaxDepth limits delegation depth to prevent infinite loops
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	// +kubebuilder:default=3
	// +optional
	MaxDepth int32 `json:"maxDepth,omitempty"`

	// Autonomous enables autonomous loop mode for coordinator agents using this config.
	// When enabled, the controller re-creates Jobs in a loop instead of marking the task as Succeeded.
	// +optional
	Autonomous bool `json:"autonomous,omitempty"`

	// MaxIterations limits the number of autonomous loop iterations (0 = unlimited).
	// Only used when Autonomous is true.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxIterations int32 `json:"maxIterations,omitempty"`

	// ApprovalRequiredTools lists custom tool names that require a human approval before execution.
	// This field is only honored when coordination is enabled and autonomous mode is true.
	// Built-in tools such as request_approval, delegate_task, and web_search are rejected.
	// +optional
	ApprovalRequiredTools []string `json:"approvalRequiredTools,omitempty"`
}

// AllowedAgent defines an agent that can be delegated to
type AllowedAgent struct {
	// Name is the name of the agent
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Namespace is the namespace of the agent (defaults to same namespace)
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// AgentStatus defines the observed state of Agent
type AgentStatus struct {
	// Ready indicates whether the agent configuration is valid and usable
	// +optional
	Ready bool `json:"ready,omitempty"`

	// ActiveTasks is the number of active tasks using this agent
	ActiveTasks int32 `json:"activeTasks"`

	// LastUsed is the timestamp of when this agent was last used
	// +optional
	LastUsed *metav1.Time `json:"lastUsed,omitempty"`

	// Conditions represent the current state of the Agent
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// IdentityClaim is the external identity this Agent holds. The controller
	// grants a claim only when no other Agent holds it, and keeps it after the
	// spec changes until every Task bound to it has finished.
	// +optional
	IdentityClaim *AgentIdentityClaim `json:"identityClaim,omitempty"`
}

// AgentIdentityClaim records an Agent's exclusive hold on one identity.
type AgentIdentityClaim struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Issuer string `json:"issuer"`
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	ID string `json:"id"`
	// Releasing marks a claim the Agent's spec no longer names. New Task
	// bindings refuse it, and the controller releases it on a later pass once
	// no unfinished Task is bound to it.
	// +optional
	Releasing bool `json:"releasing,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.spec.model.provider`
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.spec.model.name`
// +kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=`.status.ready`
// +kubebuilder:printcolumn:name="Active",type=integer,JSONPath=`.status.activeTasks`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Agent is the Schema for the agents API
type Agent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentSpec   `json:"spec,omitempty"`
	Status AgentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentList contains a list of Agent
type AgentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Agent `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Agent{}, &AgentList{})
}
