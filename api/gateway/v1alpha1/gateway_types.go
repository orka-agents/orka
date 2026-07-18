/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

const (
	// ContractVersionV1 is the provider-neutral adapter contract implemented by Orka.
	ContractVersionV1 = "orka.gateway.v1"
)

// GatewayCategory describes the broad integration shape without naming a provider.
// +kubebuilder:validation:Enum=chat;webhook;http;event;internal
// +kubebuilder:default=chat
type GatewayCategory string

const (
	GatewayCategoryChat     GatewayCategory = "chat"
	GatewayCategoryWebhook  GatewayCategory = "webhook"
	GatewayCategoryHTTP     GatewayCategory = "http"
	GatewayCategoryEvent    GatewayCategory = "event"
	GatewayCategoryInternal GatewayCategory = "internal"
)

// GatewayCapabilities describes provider-neutral adapter behavior.
type GatewayCapabilities struct {
	// InboundText indicates support for authenticated inbound text events.
	// +optional
	InboundText bool `json:"inboundText,omitempty"`

	// OutboundText indicates support for final text and sanitized error delivery.
	// +optional
	OutboundText bool `json:"outboundText,omitempty"`

	// Threads indicates that thread identity is preserved.
	// +optional
	Threads bool `json:"threads,omitempty"`

	// SenderIdentity indicates that stable sender identity is preserved.
	// +optional
	SenderIdentity bool `json:"senderIdentity,omitempty"`

	// ExplicitSessions indicates that explicit Session names are supported by bindings.
	// +optional
	ExplicitSessions bool `json:"explicitSessions,omitempty"`

	// IdempotentDelivery indicates that repeated delivery IDs do not create duplicate provider sends.
	// +optional
	IdempotentDelivery bool `json:"idempotentDelivery,omitempty"`
}

// GatewayClassSpec defines one administrator-owned adapter profile.
type GatewayClassSpec struct {
	// ContractVersion is the adapter protocol implemented by this class.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=orka.gateway.v1
	ContractVersion string `json:"contractVersion"`

	// Category describes the integration shape without selecting a provider SDK.
	// +kubebuilder:validation:Required
	Category GatewayCategory `json:"category"`

	// Capabilities are the behaviors every Gateway using this class must advertise.
	// +optional
	Capabilities GatewayCapabilities `json:"capabilities,omitempty"`

	// AllowedMetadataKeys is the bounded allowlist of normalized event metadata keys.
	// Empty means no event metadata is accepted.
	// +listType=set
	// +kubebuilder:validation:MaxItems=32
	// +optional
	AllowedMetadataKeys []string `json:"allowedMetadataKeys,omitempty"`
}

// GatewayClassStatus reports class validation.
type GatewayClassStatus struct {
	// Accepted reports whether the class is supported by Orka core.
	// +optional
	Accepted bool `json:"accepted,omitempty"`

	// ObservedGeneration is the latest reconciled generation.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Message is sanitized validation context.
	// +optional
	Message string `json:"message,omitempty"`

	// Conditions report validation state.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=gwclass
// +kubebuilder:printcolumn:name="Accepted",type=boolean,JSONPath=`.status.accepted`
// +kubebuilder:printcolumn:name="Contract",type=string,JSONPath=`.spec.contractVersion`
// +kubebuilder:printcolumn:name="Category",type=string,JSONPath=`.spec.category`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// GatewayClass is the cluster-scoped registry for gateway adapter profiles.
type GatewayClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GatewayClassSpec   `json:"spec,omitempty"`
	Status GatewayClassStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// GatewayClassList contains GatewayClass resources.
type GatewayClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GatewayClass `json:"items"`
}

// GatewayServiceReference selects a same-namespace Kubernetes Service.
type GatewayServiceReference struct {
	// Name is the Service name in the Gateway namespace.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Port is the Service port. Zero selects the first declared Service port.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port int32 `json:"port,omitempty"`
}

// GatewayAdapterLocation selects exactly one adapter address.
// +kubebuilder:validation:XValidation:rule="has(self.endpoint) != has(self.serviceRef)",message="exactly one of endpoint or serviceRef is required"
type GatewayAdapterLocation struct {
	// Endpoint is an absolute HTTPS adapter base URL.
	// +kubebuilder:validation:Pattern=`^https://[^\s@?#]+$`
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// ServiceRef selects a selector-backed TLS Service in the Gateway namespace.
	// +optional
	ServiceRef *GatewayServiceReference `json:"serviceRef,omitempty"`
}

// GatewayBearerAuthReference selects a bearer token from a same-namespace Secret.
type GatewayBearerAuthReference struct {
	// Name is the Secret name.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Key is the Secret data key.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`
}

// GatewaySpec defines one configured adapter instance.
// +kubebuilder:validation:XValidation:rule="self.inboundAuthRef.name != self.outboundAuthRef.name",message="inbound and outbound authentication must use separate Secrets"
type GatewaySpec struct {
	// GatewayClassName references a cluster-scoped GatewayClass.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	GatewayClassName string `json:"gatewayClassName"`

	// Adapter selects an HTTPS endpoint or TLS-authenticated same-namespace Service.
	Adapter GatewayAdapterLocation `json:"adapter"`

	// InboundAuthRef authenticates adapter calls into Orka.
	InboundAuthRef GatewayBearerAuthReference `json:"inboundAuthRef"`

	// OutboundAuthRef authenticates Orka calls to the adapter.
	OutboundAuthRef GatewayBearerAuthReference `json:"outboundAuthRef"`

	// Metadata is bounded, non-secret operator metadata.
	// +kubebuilder:validation:MaxProperties=32
	// +optional
	Metadata map[string]string `json:"metadata,omitempty"`
}

// GatewayObservedCapabilities contains sanitized adapter capability data.
type GatewayObservedCapabilities struct {
	// ContractVersion is the protocol reported by the adapter.
	// +optional
	ContractVersion string `json:"contractVersion,omitempty"`

	// AdapterName is the sanitized adapter name.
	// +optional
	AdapterName string `json:"adapterName,omitempty"`

	// AdapterVersion is the sanitized adapter version.
	// +optional
	AdapterVersion string `json:"adapterVersion,omitempty"`

	// Capabilities are provider-neutral observed behaviors.
	// +optional
	Capabilities GatewayCapabilities `json:"capabilities,omitempty"`
}

// GatewayStatus reports reference resolution and adapter connectivity.
type GatewayStatus struct {
	// Accepted reports spec validation.
	// +optional
	Accepted bool `json:"accepted,omitempty"`

	// ResolvedRefs reports class, Service, and Secret resolution.
	// +optional
	ResolvedRefs bool `json:"resolvedRefs,omitempty"`

	// Connected reports a successful authenticated adapter probe.
	// +optional
	Connected bool `json:"connected,omitempty"`

	// Ready reports that ingress and delivery may use this Gateway.
	// +optional
	Ready bool `json:"ready,omitempty"`

	// ObservedGeneration is the latest reconciled generation.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ResolvedEndpoint is a sanitized scheme and host suitable for operators.
	// +optional
	ResolvedEndpoint string `json:"resolvedEndpoint,omitempty"`

	// ObservedCapabilities is the sanitized adapter capability response.
	// +optional
	ObservedCapabilities *GatewayObservedCapabilities `json:"observedCapabilities,omitempty"`

	// ObservedInboundAuthRefVersion is non-secret Secret resource metadata.
	// +optional
	ObservedInboundAuthRefVersion string `json:"observedInboundAuthSecretVersion,omitempty"`

	// ObservedOutboundAuthRefVersion is non-secret Secret resource metadata.
	// +optional
	ObservedOutboundAuthRefVersion string `json:"observedOutboundAuthSecretVersion,omitempty"`

	// LastSuccessfulProbe is the last successful health/capability probe.
	// +optional
	LastSuccessfulProbe *metav1.Time `json:"lastSuccessfulProbe,omitempty"`

	// Message is sanitized readiness context.
	// +optional
	Message string `json:"message,omitempty"`

	// Conditions report Accepted, ResolvedRefs, Connected, and Ready state.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=gw
// +kubebuilder:printcolumn:name="Class",type=string,JSONPath=`.spec.gatewayClassName`
// +kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=`.status.ready`
// +kubebuilder:printcolumn:name="Connected",type=boolean,JSONPath=`.status.connected`
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.status.resolvedEndpoint`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Gateway configures one provider-neutral adapter instance.
type Gateway struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GatewaySpec   `json:"spec,omitempty"`
	Status GatewayStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// GatewayList contains Gateway resources.
type GatewayList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Gateway `json:"items"`
}

// GatewayBindingReference selects a same-namespace object.
type GatewayBindingReference struct {
	// Name is the object name.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// GatewayBindingMatch selects exact normalized external identity.
type GatewayBindingMatch struct {
	// AccountID is the exact normalized external account identity.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	AccountID string `json:"accountId"`

	// ContextID is the exact normalized external conversation or request context.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	ContextID string `json:"contextId"`

	// ThreadID optionally restricts the binding to one exact thread.
	// +kubebuilder:validation:MaxLength=256
	// +optional
	ThreadID string `json:"threadId,omitempty"`

	// SenderID optionally restricts the binding to one exact sender before sender policy is evaluated.
	// +kubebuilder:validation:MaxLength=256
	// +optional
	SenderID string `json:"senderId,omitempty"`
}

// GatewaySenderPolicyMode controls sender authorization.
// +kubebuilder:validation:Enum=allowlist;all
// +kubebuilder:default=allowlist
type GatewaySenderPolicyMode string

const (
	GatewaySenderPolicyAllowlist GatewaySenderPolicyMode = "allowlist"
	GatewaySenderPolicyAll       GatewaySenderPolicyMode = "all"
)

// GatewaySenderPolicy authorizes senders for a bound context.
type GatewaySenderPolicy struct {
	// Mode defaults to allowlist. all is an explicit trusted-context opt-in.
	// +optional
	Mode GatewaySenderPolicyMode `json:"mode,omitempty"`

	// AllowedSenderIDs contains exact normalized sender IDs.
	// +listType=set
	// +kubebuilder:validation:MaxItems=100
	// +optional
	AllowedSenderIDs []string `json:"allowedSenderIds,omitempty"`
}

// GatewaySessionMode selects deterministic Session derivation.
// +kubebuilder:validation:Enum=ephemeral;context;thread;sender;context-sender;thread-sender;explicit
// +kubebuilder:default=context
type GatewaySessionMode string

const (
	GatewaySessionEphemeral     GatewaySessionMode = "ephemeral"
	GatewaySessionContext       GatewaySessionMode = "context"
	GatewaySessionThread        GatewaySessionMode = "thread"
	GatewaySessionSender        GatewaySessionMode = "sender"
	GatewaySessionContextSender GatewaySessionMode = "context-sender"
	GatewaySessionThreadSender  GatewaySessionMode = "thread-sender"
	GatewaySessionExplicit      GatewaySessionMode = "explicit"
)

// GatewaySessionSpec controls deterministic Session routing.
type GatewaySessionSpec struct {
	// Mode selects the derivation inputs.
	// +optional
	Mode GatewaySessionMode `json:"mode,omitempty"`

	// Name is required only for explicit mode.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	Name string `json:"name,omitempty"`
}

// GatewayTaskRetryPolicy is the bounded subset of Task retry configuration exposed to a binding.
type GatewayTaskRetryPolicy struct {
	// MaxRetries is the maximum Task retry count.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=10
	// +optional
	MaxRetries int32 `json:"maxRetries,omitempty"`

	// BackoffMultiplier controls exponential Task retry delay.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	// +optional
	BackoffMultiplier float64 `json:"backoffMultiplier,omitempty"`

	// InitialDelay is the first Task retry delay.
	// +kubebuilder:validation:XValidation:rule="!(duration(self) <= duration('0s') || duration(self) > duration('1h'))",message="initialDelay must be positive and no greater than 1h"
	// +optional
	InitialDelay *metav1.Duration `json:"initialDelay,omitempty"`
}

// GatewayTaskDefaults is the bounded set of Task defaults a binding may apply.
type GatewayTaskDefaults struct {
	// Priority is the Task queue priority.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1000
	// +optional
	Priority *int32 `json:"priority,omitempty"`

	// Timeout is the Task timeout.
	// +kubebuilder:validation:XValidation:rule="!(duration(self) <= duration('0s') || duration(self) > duration('24h'))",message="timeout must be positive and no greater than 24h"
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// RetryPolicy configures bounded Task retries.
	// +optional
	RetryPolicy *GatewayTaskRetryPolicy `json:"retryPolicy,omitempty"`

	// AgentRuntimeMaxTurns is the only runtime-specific override permitted in V1.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000
	// +optional
	AgentRuntimeMaxTurns *int32 `json:"agentRuntimeMaxTurns,omitempty"`
}

// GatewayActiveTurnBehavior controls new messages while a Session is busy.
// +kubebuilder:validation:Enum=queue
// +kubebuilder:default=queue
type GatewayActiveTurnBehavior string

const GatewayActiveTurnQueue GatewayActiveTurnBehavior = "queue"

// GatewayBindingSpec maps normalized external identity to one Agent.
type GatewayBindingSpec struct {
	// GatewayRef selects one Gateway in this namespace.
	GatewayRef GatewayBindingReference `json:"gatewayRef"`

	// AgentRef selects one Agent in this namespace.
	AgentRef GatewayBindingReference `json:"agentRef"`

	// Match selects exact normalized account/context identity and optional constraints.
	Match GatewayBindingMatch `json:"match"`

	// SenderPolicy denies unknown senders by default.
	// +optional
	SenderPolicy GatewaySenderPolicy `json:"senderPolicy,omitempty"`

	// Priority resolves overlapping matches. Equal-priority matches fail closed.
	// +kubebuilder:validation:Minimum=-1000
	// +kubebuilder:validation:Maximum=1000
	// +optional
	Priority int32 `json:"priority,omitempty"`

	// Session controls deterministic Session derivation.
	// +optional
	Session GatewaySessionSpec `json:"session,omitempty"`

	// TaskDefaults is the bounded Task configuration applied to dispatched events.
	// +optional
	TaskDefaults GatewayTaskDefaults `json:"taskDefaults,omitempty"`

	// ActiveTurnBehavior is queue in V1.
	// +optional
	ActiveTurnBehavior GatewayActiveTurnBehavior `json:"activeTurnBehavior,omitempty"`
}

// GatewayBindingStatus reports routing readiness.
type GatewayBindingStatus struct {
	// Accepted reports spec validation.
	// +optional
	Accepted bool `json:"accepted,omitempty"`

	// ResolvedRefs reports Gateway and Agent resolution.
	// +optional
	ResolvedRefs bool `json:"resolvedRefs,omitempty"`

	// Programmed reports that the binding has no equal-priority ambiguity.
	// +optional
	Programmed bool `json:"programmed,omitempty"`

	// Ready reports that ingress may route events through this binding.
	// +optional
	Ready bool `json:"ready,omitempty"`

	// ObservedGeneration is the latest reconciled generation.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ResolvedCapabilities is the provider-neutral capability summary used for validation.
	// +optional
	ResolvedCapabilities *GatewayCapabilities `json:"resolvedCapabilities,omitempty"`

	// LastInboundActivity is the latest accepted inbound event time.
	// +optional
	LastInboundActivity *metav1.Time `json:"lastInboundActivity,omitempty"`

	// LastOutboundActivity is the latest delivered outbound reply time.
	// +optional
	LastOutboundActivity *metav1.Time `json:"lastOutboundActivity,omitempty"`

	// Message is sanitized readiness context.
	// +optional
	Message string `json:"message,omitempty"`

	// Conditions report Accepted, ResolvedRefs, Programmed, and Ready state.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=gwbind
// +kubebuilder:printcolumn:name="Gateway",type=string,JSONPath=`.spec.gatewayRef.name`
// +kubebuilder:printcolumn:name="Agent",type=string,JSONPath=`.spec.agentRef.name`
// +kubebuilder:printcolumn:name="Priority",type=integer,JSONPath=`.spec.priority`
// +kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=`.status.ready`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// GatewayBinding maps one normalized external context to one Agent.
type GatewayBinding struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GatewayBindingSpec   `json:"spec,omitempty"`
	Status GatewayBindingStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// GatewayBindingList contains GatewayBinding resources.
type GatewayBindingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GatewayBinding `json:"items"`
}

func init() {
	SchemeBuilder.Register(
		&GatewayClass{}, &GatewayClassList{},
		&Gateway{}, &GatewayList{},
		&GatewayBinding{}, &GatewayBindingList{},
	)
}
