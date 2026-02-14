/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AgentSpec defines the desired state of Agent
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

	// Tools lists the default tools available to this agent
	// +optional
	Tools []ToolReference `json:"tools,omitempty"`

	// Skills lists the default skills for this agent
	// +optional
	Skills []SkillReference `json:"skills,omitempty"`

	// Resources defines the resource limits for tasks using this agent
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// SecretRef references a Secret containing LLM API keys
	// +optional
	SecretRef *corev1.LocalObjectReference `json:"secretRef,omitempty"`

	// Session defines session configuration defaults
	// +optional
	Session *SessionConfig `json:"session,omitempty"`

	// RateLimit defines rate limiting configuration
	// +optional
	RateLimit *RateLimitConfig `json:"rateLimit,omitempty"`

	// Coordination enables agent-to-agent delegation
	// +optional
	Coordination *CoordinationConfig `json:"coordination,omitempty"`

	// Runtime configures this Agent for external CLI runtimes (type: agent tasks).
	// When set, this Agent is for type: agent tasks only (mutually exclusive with providerRef).
	// +optional
	Runtime *AgentCLIRuntime `json:"runtime,omitempty"`

	// TTLAfterLastTask defines how long the agent persists after its last task completes.
	// When set and no tasks are active, the agent is deleted after this duration.
	// Zero means the agent is never auto-deleted (permanent). Default is no TTL (permanent).
	// +optional
	TTLAfterLastTask *metav1.Duration `json:"ttlAfterLastTask,omitempty"`
}

// AgentCLIRuntime defines agent CLI runtime configuration for an Agent.
type AgentCLIRuntime struct {
	// Type specifies which CLI runtime to use
	// +kubebuilder:validation:Required
	Type AgentRuntimeType `json:"type"`

	// DefaultMaxTurns is the default maximum agent loop iterations for tasks using this Agent
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000
	// +kubebuilder:default=50
	// +optional
	DefaultMaxTurns *int32 `json:"defaultMaxTurns,omitempty"`

	// DefaultAllowedTools lists the default tools allowed for tasks using this Agent
	// +optional
	DefaultAllowedTools []string `json:"defaultAllowedTools,omitempty"`

	// DefaultAllowBash controls whether bash is allowed by default for tasks using this Agent
	// +kubebuilder:default=true
	// +optional
	DefaultAllowBash bool `json:"defaultAllowBash,omitempty"`
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
	// +kubebuilder:default=0.7
	// +optional
	Temperature *float64 `json:"temperature,omitempty"`

	// MaxTokens limits the response length
	// +optional
	MaxTokens *int32 `json:"maxTokens,omitempty"`

	// Fallbacks defines alternative providers to try when the primary fails.
	// Each fallback specifies a Provider CRD and optional model override.
	// +optional
	Fallbacks []ModelFallback `json:"fallbacks,omitempty"`
}

// PromptSource defines where to get a prompt from
type PromptSource struct {
	// Inline is the inline prompt text
	// +optional
	Inline string `json:"inline,omitempty"`

	// ConfigMapRef references a ConfigMap containing the prompt
	// +optional
	ConfigMapRef *ConfigMapKeySelector `json:"configMapRef,omitempty"`
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
	// Persistence defines the storage backend (configmap, pvc, none)
	// +kubebuilder:validation:Enum=configmap;pvc;none
	// +kubebuilder:default=configmap
	// +optional
	Persistence string `json:"persistence,omitempty"`

	// TTL defines the session time-to-live (auto-expire)
	// +optional
	TTL *metav1.Duration `json:"ttl,omitempty"`

	// MaxMessages is the maximum messages to load from session
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=50
	// +optional
	MaxMessages int32 `json:"maxMessages,omitempty"`
}

// RateLimitConfig defines rate limiting for an agent
type RateLimitConfig struct {
	// RequestsPerMinute limits requests per minute
	// +optional
	RequestsPerMinute *int32 `json:"requestsPerMinute,omitempty"`

	// TokensPerMinute limits tokens per minute
	// +optional
	TokensPerMinute *int64 `json:"tokensPerMinute,omitempty"`
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
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.spec.model.provider`
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.spec.model.name`
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
