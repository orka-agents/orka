/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

const (
	// DefaultRuntimePoolDesiredReplicas keeps an idle pool scaled to zero until
	// durable demand is present.
	DefaultRuntimePoolDesiredReplicas int32 = 0
	// DefaultRuntimePoolMaxResidentSessions is the first-release per-pool
	// resident-session limit.
	DefaultRuntimePoolMaxResidentSessions int32 = 10
	// DefaultRuntimePoolMaxRunningPrompts is the first-release per-pool prompt
	// concurrency limit.
	DefaultRuntimePoolMaxRunningPrompts int32 = 4
	// DefaultRuntimePoolColdStartTimeoutSeconds bounds a 0 -> 1 pool startup.
	DefaultRuntimePoolColdStartTimeoutSeconds int32 = 120
	// MaxRuntimePoolCapacityReservations bounds the durable reservation list in
	// RuntimePool status. RuntimePool capacity is itself limited to 1000.
	MaxRuntimePoolCapacityReservations = 1000
)

// RuntimePoolProtocolVersion is the controller-to-supervisor protocol profile.
// +kubebuilder:validation:Enum=orka.harness.v2
type RuntimePoolProtocolVersion string

const (
	// RuntimePoolProtocolHarnessV2 is the ACP session-centric harness contract.
	RuntimePoolProtocolHarnessV2 RuntimePoolProtocolVersion = "orka.harness.v2"
)

// RuntimePoolLifecycle is the controller-observed lifecycle of a logical pool.
// Only Serving may admit new RuntimeSessions.
// +kubebuilder:validation:Enum=Stopped;Starting;Serving;Draining;Quiescent;Stopping;Degraded;Ambiguous
type RuntimePoolLifecycle string

const (
	RuntimePoolLifecycleStopped   RuntimePoolLifecycle = "Stopped"
	RuntimePoolLifecycleStarting  RuntimePoolLifecycle = "Starting"
	RuntimePoolLifecycleServing   RuntimePoolLifecycle = "Serving"
	RuntimePoolLifecycleDraining  RuntimePoolLifecycle = "Draining"
	RuntimePoolLifecycleQuiescent RuntimePoolLifecycle = "Quiescent"
	RuntimePoolLifecycleStopping  RuntimePoolLifecycle = "Stopping"
	RuntimePoolLifecycleDegraded  RuntimePoolLifecycle = "Degraded"
	RuntimePoolLifecycleAmbiguous RuntimePoolLifecycle = "Ambiguous"
)

// RuntimePoolAdmissionState is the authoritative admission gate for new
// RuntimeSessions. Existing session control traffic may continue while draining.
// +kubebuilder:validation:Enum=Closed;Accepting;Draining;Ambiguous
type RuntimePoolAdmissionState string

const (
	RuntimePoolAdmissionClosed    RuntimePoolAdmissionState = "Closed"
	RuntimePoolAdmissionAccepting RuntimePoolAdmissionState = "Accepting"
	RuntimePoolAdmissionDraining  RuntimePoolAdmissionState = "Draining"
	RuntimePoolAdmissionAmbiguous RuntimePoolAdmissionState = "Ambiguous"
)

const (
	// RuntimePoolConditionAdmissionReady reports whether new RuntimeSessions may
	// be admitted without violating lifecycle, fencing, or capacity rules.
	RuntimePoolConditionAdmissionReady = "AdmissionReady"
	// RuntimePoolConditionPodSecurityReady reports whether the runtime Pod passes
	// the selected namespace's Pod Security admission requirements.
	RuntimePoolConditionPodSecurityReady = "PodSecurityReady"
	// RuntimePoolConditionQuotaReady reports whether quota permits the pool's
	// controller-owned resources.
	RuntimePoolConditionQuotaReady = "QuotaReady"
	// RuntimePoolConditionSchedulingReady reports whether the selected runtime
	// Pod can be scheduled.
	RuntimePoolConditionSchedulingReady = "SchedulingReady"
	// RuntimePoolConditionRolloutReady reports whether the immutable runtime
	// profile has been installed without version skew.
	RuntimePoolConditionRolloutReady = "RolloutReady"
)

const (
	RuntimePoolReasonAdmissionClosed     = "AdmissionClosed"
	RuntimePoolReasonAtCapacity          = "AtCapacity"
	RuntimePoolReasonPodSecurityRejected = "PodSecurityRejected"
	RuntimePoolReasonQuotaRejected       = "QuotaRejected"
	RuntimePoolReasonSchedulingFailed    = "SchedulingFailed"
	RuntimePoolReasonRolloutFailed       = "RolloutFailed"
	RuntimePoolReasonRuntimeAmbiguous    = "RuntimeAmbiguous"
)

// RuntimePoolTrustDomain identifies the logical same-trust-domain boundary
// served by one pool. It is not a tenant-isolation claim.
type RuntimePoolTrustDomain struct {
	// Namespace is the Task namespace represented by this trust domain.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Namespace string `json:"namespace"`

	// Identity is the controller-defined, canonical trust-domain identity. It
	// must remain stable across physical runtime namespace or Pod replacement.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Identity string `json:"identity"`
}

// ModelTokenLimits pins the reviewed token capacities used by a
// runtime's local context-management policy.
// +kubebuilder:validation:XValidation:rule="self.context > self.output",message="model context limit must exceed output limit"
type ModelTokenLimits struct {
	// Context is the maximum model context capacity in tokens.
	// +kubebuilder:validation:Minimum=1
	Context int64 `json:"context"`

	// Output is the maximum generated output in tokens.
	// +kubebuilder:validation:Minimum=1
	Output int64 `json:"output"`
}

// RuntimePoolProfileSpec pins the immutable runtime behavior selected for a
// pool. The digest covers the adapter and CLI builds, ACP profile, provider and
// model limits, agent configuration, tool and approval policy, MCP
// configuration, workspace intent, proxy credential scope, and resource class.
// +kubebuilder:validation:XValidation:rule="self.providerKind != 'opencode' || has(self.modelLimits)",message="OpenCode runtime profiles require modelLimits"
type RuntimePoolProfileSpec struct {
	// ProtocolVersion is the controller-to-supervisor protocol profile.
	// +kubebuilder:default=orka.harness.v2
	// +optional
	ProtocolVersion RuntimePoolProtocolVersion `json:"protocolVersion,omitempty"`

	// Digest is the canonical immutable runtime-profile digest.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	Digest string `json:"digest"`

	// DigestSchemaVersion identifies the canonicalization schema used to compute Digest.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`
	DigestSchemaVersion string `json:"digestSchemaVersion"`

	// ACPProfile is the reviewed ACP wire/profile identifier.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=acp.v1
	ACPProfile string `json:"acpProfile"`

	// AdapterDigests pins every adapter and provider CLI artifact used by the pool.
	// +kubebuilder:validation:MinProperties=1
	// +kubebuilder:validation:MaxProperties=32
	AdapterDigests map[string]string `json:"adapterDigests"`

	// ProviderKind selects the one provider adapter present in the immutable image.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=codex;claude;copilot;opencode
	ProviderKind string `json:"providerKind"`

	// Model is the exact reviewed model identifier.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	Model string `json:"model"`

	// ModelLimits pins the reviewed context and output capacities used by the
	// runtime's local compaction policy.
	// +optional
	ModelLimits *ModelTokenLimits `json:"modelLimits,omitempty"`

	// AgentConfigurationDigest freezes non-secret Agent/runtime configuration.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	AgentConfigurationDigest string `json:"agentConfigurationDigest"`

	// ToolPolicyDigest freezes the effective tool allow/deny policy.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	ToolPolicyDigest string `json:"toolPolicyDigest"`

	// ApprovalPolicyDigest freezes the effective approval policy.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	ApprovalPolicyDigest string `json:"approvalPolicyDigest"`

	// MCPConfigurationDigest freezes prompt-scoped broker/MCP configuration.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	MCPConfigurationDigest string `json:"mcpConfigurationDigest"`

	// WorkspaceIntent is part of the immutable runtime profile.
	// +kubebuilder:validation:Required
	WorkspaceIntent WorkspaceIntent `json:"workspaceIntent"`

	// ProxyCredentialRole identifies the provider-proxy client role, never a secret value.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	ProxyCredentialRole string `json:"proxyCredentialRole"`

	// ProxyCredentialScope is the bounded model/session capability scope.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	ProxyCredentialScope string `json:"proxyCredentialScope"`

	// ResourceClass is the controller-supported pool resource class included in Digest.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	ResourceClass string `json:"resourceClass"`
}

// RuntimePoolExecutionWorkspaceSpec binds a pool's runtime workload to an
// externally operated execution-workspace provider instead of a
// controller-owned Deployment. The rendered supervisor workload, image
// allowlist, epoch-scoped Secrets, endpoint fencing, and admission semantics
// are identical to plain pools; only workload materialization changes.
// Provider-native identifiers never enter public Task status.
// +kubebuilder:validation:XValidation:rule="(self.provider == 'substrate') == has(self.substrate)",message="substrate settings are required exactly when provider is substrate"
type RuntimePoolExecutionWorkspaceSpec struct {
	// Provider selects the execution-workspace provider control plane hosting
	// this pool's single runtime instance.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=agent-sandbox;substrate
	Provider WorkspaceProvider `json:"provider"`

	// BindingDigest is the canonical digest of the frozen workspace binding
	// (provider, policies, and session key) that names this pool. It is part of
	// the pool identity and carries no provider-native or secret material.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	BindingDigest string `json:"bindingDigest"`

	// Substrate configures the Substrate-provider backend.
	// +optional
	Substrate *RuntimePoolSubstrateWorkspaceSpec `json:"substrate,omitempty"`
}

// RuntimePoolSubstrateWorkspaceSpec binds a Substrate-backed pool to the
// operator-owned infrastructure ActorTemplate whose placement fields
// (workerPoolRef, runsc, snapshotsConfig) seed the controller-rendered
// runtime template. The runtime container itself is always controller-owned.
type RuntimePoolSubstrateWorkspaceSpec struct {
	// BaseTemplateNamespace is the namespace of the operator-owned
	// infrastructure ActorTemplate. Controller-rendered runtime templates and
	// their epoch-scoped Secrets are created in the same namespace so the
	// provider can resolve them.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	BaseTemplateNamespace string `json:"baseTemplateNamespace"`

	// BaseTemplateName names the operator-owned infrastructure ActorTemplate.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	BaseTemplateName string `json:"baseTemplateName"`
}

// RuntimePoolRuntimeSpec selects the immutable supervisor image and profile.
type RuntimePoolRuntimeSpec struct {
	// Image is a digest-pinned OCI image. Mutable tags are intentionally rejected.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^[^\s@]+@sha256:[a-f0-9]{64}$`
	Image string `json:"image"`

	// Profile is the immutable runtime profile enforced for every active instance.
	// +kubebuilder:validation:Required
	Profile RuntimePoolProfileSpec `json:"profile"`
}

// RuntimePoolCapacitySpec defines hard logical pool concurrency limits.
// +kubebuilder:validation:XValidation:rule="self.maxRunningPrompts <= self.maxResidentSessions",message="maxRunningPrompts cannot exceed maxResidentSessions"
type RuntimePoolCapacitySpec struct {
	// MaxResidentSessions is the maximum number of resident RuntimeSessions.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000
	// +kubebuilder:default=10
	// +optional
	MaxResidentSessions int32 `json:"maxResidentSessions,omitempty"`

	// MaxRunningPrompts is the maximum number of concurrently running prompts.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000
	// +kubebuilder:default=4
	// +optional
	MaxRunningPrompts int32 `json:"maxRunningPrompts,omitempty"`
}

// RuntimePoolSpec defines the desired state of a controller-owned logical pool.
// Trust-domain placement and the runtime image/profile are immutable; rollout
// uses drain-and-replace rather than changing an in-memory instance in place.
// +kubebuilder:validation:XValidation:rule="self.trustDomain == oldSelf.trustDomain",message="trustDomain is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.runtimeNamespace) == has(oldSelf.runtimeNamespace) && (!has(self.runtimeNamespace) || self.runtimeNamespace == oldSelf.runtimeNamespace)",message="runtimeNamespace is immutable"
// +kubebuilder:validation:XValidation:rule="self.runtime == oldSelf.runtime",message="runtime image and profile are immutable"
// +kubebuilder:validation:XValidation:rule="has(self.executionWorkspace) == has(oldSelf.executionWorkspace) && (!has(self.executionWorkspace) || self.executionWorkspace == oldSelf.executionWorkspace)",message="executionWorkspace binding is immutable"
type RuntimePoolSpec struct {
	// TrustDomain is the logical namespace/identity boundary served by this pool.
	// +kubebuilder:validation:Required
	TrustDomain RuntimePoolTrustDomain `json:"trustDomain"`

	// RuntimeNamespace is the physical namespace for controller-owned runtime
	// resources. When omitted, the controller selects its configured runtime namespace.
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +optional
	RuntimeNamespace string `json:"runtimeNamespace,omitempty"`

	// Runtime pins the immutable supervisor image and behavior profile.
	// +kubebuilder:validation:Required
	Runtime RuntimePoolRuntimeSpec `json:"runtime"`

	// ExecutionWorkspace, when set, materializes the pool's single runtime
	// instance through an externally operated execution-workspace provider
	// instead of a controller-owned Deployment. The binding is immutable;
	// workspace-backed pools host exactly one resident RuntimeSession.
	// +optional
	ExecutionWorkspace *RuntimePoolExecutionWorkspaceSpec `json:"executionWorkspace,omitempty"`

	// DesiredReplicas is zero or one. More than one runtime Pod would make
	// stateful exact-instance routing ambiguous.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1
	// +kubebuilder:default=0
	// +optional
	DesiredReplicas int32 `json:"desiredReplicas,omitempty"`

	// Capacity sets resident-session and running-prompt limits.
	// +kubebuilder:default={maxResidentSessions:10,maxRunningPrompts:4}
	// +optional
	Capacity *RuntimePoolCapacitySpec `json:"capacity,omitempty"`

	// ColdStartTimeoutSeconds bounds a 0 -> 1 startup before the pool is marked degraded.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=3600
	// +kubebuilder:default=120
	// +optional
	ColdStartTimeoutSeconds int32 `json:"coldStartTimeoutSeconds,omitempty"`
}

// RuntimePoolActiveInstanceStatus is the exact selected runtime Pod and
// supervisor boot. Every stateful request is fenced to this identity.
type RuntimePoolActiveInstanceStatus struct {
	// PodNamespace is the namespace containing the selected runtime Pod.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	PodNamespace string `json:"podNamespace"`

	// PodName is the exact selected runtime Pod name.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	PodName string `json:"podName"`

	// PodAddress is the exact Pod address used for stateful routing, not a
	// load-balanced Service endpoint.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	PodAddress string `json:"podAddress"`

	// PodUID is the Kubernetes UID of the selected Pod.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	PodUID string `json:"podUID"`

	// BootID is the immutable supervisor boot identifier inside the selected Pod.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	BootID string `json:"bootID"`

	// RuntimeInstanceID is the portable v2 instance fence derived from PodUID and BootID.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	RuntimeInstanceID string `json:"runtimeInstanceID"`

	// ControllerEpoch is the durable controller epoch to which this instance is bound.
	// +kubebuilder:validation:Minimum=1
	ControllerEpoch int64 `json:"controllerEpoch"`

	// ProtocolVersion is the supervisor protocol actually advertised by this instance.
	// +kubebuilder:validation:Required
	ProtocolVersion RuntimePoolProtocolVersion `json:"protocolVersion"`

	// ProfileDigest is the immutable runtime-profile digest advertised by this instance.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	ProfileDigest string `json:"profileDigest"`

	// ProfileDigestSchemaVersion is the digest schema advertised by this instance.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	ProfileDigestSchemaVersion string `json:"profileDigestSchemaVersion"`

	// ProviderTokenGeneration is a non-secret digest generation for the exact
	// provider capability mounted into this runtime Pod. It lets the controller
	// prove that a selected instance converged on the intended proxy credential
	// without exposing the bearer token.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-f0-9]{16}$`
	ProviderTokenGeneration string `json:"providerTokenGeneration"`

	// LastObservedTime is the last authenticated status observation for this instance.
	// +optional
	LastObservedTime *metav1.Time `json:"lastObservedTime,omitempty"`
}

// RuntimePoolCapacityReservationStatus is one durable, exact-instance capacity
// claim. The composite key is the pool UID, Task UID, attempt, and controller
// epoch. A reservation claims resident-session and prompt admission slots until
// the supervisor accepts the corresponding work or the reservation expires.
// +kubebuilder:validation:XValidation:rule="self.residentSlots + self.promptSlots > 0",message="a capacity reservation must claim at least one slot"
type RuntimePoolCapacityReservationStatus struct {
	// PoolUID fences the claim to the exact RuntimePool object.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	PoolUID string `json:"poolUID"`

	// TaskUID is the immutable Task identity that owns the claim.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	TaskUID string `json:"taskUID"`

	// Attempt is the Task attempt that owns the claim.
	// +kubebuilder:validation:Minimum=1
	Attempt int32 `json:"attempt"`

	// ControllerEpoch fences the claim to one controller leadership epoch.
	// +kubebuilder:validation:Minimum=1
	ControllerEpoch int64 `json:"controllerEpoch"`

	// RuntimeInstanceID binds admission to the exact selected Pod/boot pair.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	RuntimeInstanceID string `json:"runtimeInstanceID"`

	// ResidentSlots is zero after a RuntimeSession is admitted and one while a
	// new resident-session slot is still reserved.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1
	ResidentSlots int32 `json:"residentSlots"`

	// PromptSlots is one until the prompt is accepted by the supervisor.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1
	PromptSlots int32 `json:"promptSlots"`

	// ReservedAt is the first successful resource-version CAS for this claim.
	// +kubebuilder:validation:Required
	ReservedAt metav1.Time `json:"reservedAt"`

	// ExpiresAt is renewed while pre-admission work is active. A later
	// dispatcher may reclaim the claim after this time.
	// +kubebuilder:validation:Required
	ExpiresAt metav1.Time `json:"expiresAt"`
}

// RuntimePoolCapacityStatus reports controller and authenticated-supervisor
// capacity counters. These counters are advisory unless fenced by the current
// ActiveInstance and controller epoch. Reservation records are authoritative for
// coordinator-owned pre-admission capacity.
type RuntimePoolCapacityStatus struct {
	// MaxResidentSessions is the effective configured resident-session limit.
	// +kubebuilder:validation:Minimum=0
	MaxResidentSessions int32 `json:"maxResidentSessions,omitempty"`

	// MaxRunningPrompts is the effective configured running-prompt limit.
	// +kubebuilder:validation:Minimum=0
	MaxRunningPrompts int32 `json:"maxRunningPrompts,omitempty"`

	// ResidentSessions is the authenticated supervisor count of resident sessions.
	// +kubebuilder:validation:Minimum=0
	ResidentSessions int32 `json:"residentSessions,omitempty"`

	// RunningPrompts is the authenticated supervisor count of active prompts.
	// +kubebuilder:validation:Minimum=0
	RunningPrompts int32 `json:"runningPrompts,omitempty"`

	// QueuedTasks is durable unsatisfied demand assigned to this pool.
	// +kubebuilder:validation:Minimum=0
	QueuedTasks int32 `json:"queuedTasks,omitempty"`

	// ReservedSessions is the sum of resident slots in Reservations.
	// +kubebuilder:validation:Minimum=0
	ReservedSessions int32 `json:"reservedSessions,omitempty"`

	// ReservedPrompts is the sum of prompt slots in Reservations.
	// +kubebuilder:validation:Minimum=0
	ReservedPrompts int32 `json:"reservedPrompts,omitempty"`

	// Reservations is the bounded authoritative set of coordinator-owned
	// pre-admission capacity claims.
	// +listType=map
	// +listMapKey=poolUID
	// +listMapKey=taskUID
	// +listMapKey=attempt
	// +listMapKey=controllerEpoch
	// +kubebuilder:validation:MaxItems=1000
	// +optional
	Reservations []RuntimePoolCapacityReservationStatus `json:"reservations,omitempty"`

	// PendingPermissions is the authenticated count of unresolved prompt permissions.
	// +kubebuilder:validation:Minimum=0
	PendingPermissions int32 `json:"pendingPermissions,omitempty"`

	// FinalizingSessions is the count reserved for validation, publication, or finalization.
	// +kubebuilder:validation:Minimum=0
	FinalizingSessions int32 `json:"finalizingSessions,omitempty"`

	// LiveDescendants is the authenticated count of tracked runtime descendants.
	// +kubebuilder:validation:Minimum=0
	LiveDescendants int32 `json:"liveDescendants,omitempty"`
}

// RuntimePoolStatus defines the observed state of a controller-owned pool.
type RuntimePoolStatus struct {
	// ObservedGeneration is the latest RuntimePool generation reconciled by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ControllerEpoch is the durable epoch required for authoritative pool writes.
	// +kubebuilder:validation:Minimum=0
	// +optional
	ControllerEpoch int64 `json:"controllerEpoch,omitempty"`

	// DesiredReplicas is the desired replica count observed by the controller.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1
	// +optional
	DesiredReplicas int32 `json:"desiredReplicas,omitempty"`

	// CurrentReplicas is the number of non-terminated runtime Pods owned by the pool.
	// +kubebuilder:validation:Minimum=0
	// +optional
	CurrentReplicas int32 `json:"currentReplicas,omitempty"`

	// Lifecycle is the explicit pool lifecycle.
	// +optional
	Lifecycle RuntimePoolLifecycle `json:"lifecycle,omitempty"`

	// AdmissionState is the authoritative admission gate for new RuntimeSessions.
	// +optional
	AdmissionState RuntimePoolAdmissionState `json:"admissionState,omitempty"`

	// ActiveInstance is the exact selected Pod and supervisor boot. It is empty
	// unless one instance has been authoritatively selected.
	// +optional
	ActiveInstance *RuntimePoolActiveInstanceStatus `json:"activeInstance,omitempty"`

	// Capacity reports effective limits, use, and queued demand.
	// +optional
	Capacity RuntimePoolCapacityStatus `json:"capacity,omitempty"`

	// Message contains bounded, sanitized reconciliation context.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	Message string `json:"message,omitempty"`

	// Conditions report admission, Pod Security, quota, scheduling, rollout, and
	// other controller-observed failures.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=rtpool
// +kubebuilder:printcolumn:name="Lifecycle",type=string,JSONPath=`.status.lifecycle`
// +kubebuilder:printcolumn:name="Admission",type=string,JSONPath=`.status.admissionState`
// +kubebuilder:printcolumn:name="Desired",type=integer,JSONPath=`.status.desiredReplicas`
// +kubebuilder:printcolumn:name="Current",type=integer,JSONPath=`.status.currentReplicas`
// +kubebuilder:printcolumn:name="Sessions",type=integer,JSONPath=`.status.capacity.residentSessions`
// +kubebuilder:printcolumn:name="Prompts",type=integer,JSONPath=`.status.capacity.runningPrompts`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// RuntimePool is the Schema for controller-owned ACP runtime pools.
type RuntimePool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RuntimePoolSpec   `json:"spec,omitempty"`
	Status RuntimePoolStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RuntimePoolList contains a list of RuntimePool.
type RuntimePoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RuntimePool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RuntimePool{}, &RuntimePoolList{})
}
