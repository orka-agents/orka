/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// SubstrateTemplateReference names the operator-owned Substrate infrastructure
// ActorTemplate a profile binds. The referenced template supplies WorkerPool
// placement, the runsc build, and snapshot storage; its containers never
// execute ACP work because the controller renders its own derived runtime
// template from it.
type SubstrateTemplateReference struct {
	// Name is the infrastructure ActorTemplate name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Namespace is the infrastructure ActorTemplate namespace. It defaults to
	// the profile namespace.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// SubstrateSuspendMode names an operator-permitted suspension scope.
// +kubebuilder:validation:Enum=DataOnly
type SubstrateSuspendMode string

// SubstrateSuspendModeDataOnly persists only the controller-owned DurableDir
// workspace volume. Process memory, the supervisor session tree, and every
// credential stay ephemeral; resume cold-boots a fresh supervisor.
const SubstrateSuspendModeDataOnly SubstrateSuspendMode = "DataOnly"

// SubstrateSuspendPolicy permits operator-governed data-only cold suspension
// for actors derived from this profile's infrastructure template. When set,
// the controller renders the derived ActorTemplate with a dedicated DurableDir
// workspace volume and an explicit snapshotsConfig of onPause: Data,
// onCommit: Data, and onResume.fromData: ColdBoot, so a checkpoint can never
// capture process memory. Omitting the policy preserves the existing
// delete-and-recreate behavior and keeps every suspension request fail-closed.
type SubstrateSuspendPolicy struct {
	// Mode selects the snapshot scope. Only DataOnly is supported; full
	// process-memory snapshots remain rejected for ACP RuntimeSessions.
	Mode SubstrateSuspendMode `json:"mode"`
}

// SubstrateProfileSpec carries operator-owned Substrate infrastructure inputs.
type SubstrateProfileSpec struct {
	// TemplateRef names the operator-owned infrastructure ActorTemplate.
	TemplateRef SubstrateTemplateReference `json:"templateRef"`

	// Suspend permits data-only cold suspension for this profile.
	// +optional
	Suspend *SubstrateSuspendPolicy `json:"suspend,omitempty"`
}

// AgentSandboxDurableVolume freezes the durable workspace
// PersistentVolumeClaim shape for PVC-backed cold suspension. The mount path
// stays controller-owned; the volume holds only repository/workspace data.
type AgentSandboxDurableVolume struct {
	// StorageClassName optionally selects the storage class. Empty uses the
	// cluster default.
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`

	// AccessModes defaults to ReadWriteOnce when empty.
	// +listType=set
	// +optional
	AccessModes []string `json:"accessModes,omitempty"`

	// Capacity is the requested storage quantity, for example 1Gi.
	// +kubebuilder:validation:MinLength=1
	Capacity string `json:"capacity"`
}

// AgentSandboxSuspendPolicy permits operator-governed PVC-backed cold
// suspension for Agent Sandbox classes. When set, the pool's SandboxClaim
// requests one dedicated durable workspace PVC (which forces a cold start
// instead of warm-pool adoption), and a requested suspension terminates the
// Sandbox Pod through operatingMode: Suspended while the PVC persists. The
// supervisor session tree, home, temporary files, identity bookkeeping, and
// every credential stay on ephemeral storage; process memory is never
// preserved.
type AgentSandboxSuspendPolicy struct {
	// Mode selects the suspension scope. Only DataOnly is supported.
	Mode SubstrateSuspendMode `json:"mode"`

	// Volume freezes the durable workspace PVC shape.
	Volume AgentSandboxDurableVolume `json:"volume"`
}

// RetentionPolicy bounds retained workspaces for one class.
type RetentionPolicy struct {
	// MaxSuspendedWorkspaces caps concurrently suspended workspaces of this
	// class per namespace. A new suspension beyond the cap is rejected at Task
	// admission. Settlement and idle retention leave the frozen Suspend action
	// pending when the cap is exhausted; a queued continuation may take a
	// still-Ready workspace directly. The class lifecycle must also set
	// idleTimeout or maxLifetime because this cap does not expire quota-exhausted
	// Ready workspaces or open capacity held by suspended workspaces.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxSuspendedWorkspaces *int32 `json:"maxSuspendedWorkspaces,omitempty"`
}

// AgentSandboxProfileSpec carries operator-owned Agent Sandbox inputs.
type AgentSandboxProfileSpec struct {
	// Suspend permits PVC-backed cold suspension for this profile.
	// +optional
	Suspend *AgentSandboxSuspendPolicy `json:"suspend,omitempty"`
}

// RuntimeWorkspaceProfileSpec is the namespaced class-parameters contract for
// the in-tree ACP RuntimePool execution-workspace adapter. A profile sets only
// the inputs for its provider backend: substrate for Substrate, or
// agentSandbox.suspend for Agent Sandbox when PVC-backed DataOnly suspension
// is enabled.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="RuntimeWorkspaceProfile spec is immutable; create a new profile"
type RuntimeWorkspaceProfileSpec struct {
	// Substrate carries the operator-owned Substrate infrastructure inputs.
	// +optional
	Substrate *SubstrateProfileSpec `json:"substrate,omitempty"`

	// AgentSandbox carries the operator-owned Agent Sandbox inputs.
	// +optional
	AgentSandbox *AgentSandboxProfileSpec `json:"agentSandbox,omitempty"`

	// Retention bounds retained workspaces for classes using this profile.
	// +optional
	Retention *RetentionPolicy `json:"retention,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:categories=orka
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// RuntimeWorkspaceProfile is the namespaced adapter-owned workspace profile
// parameters kind referenced by ExecutionWorkspaceClass.spec.parametersRef for
// ACP RuntimePool execution workspaces.
type RuntimeWorkspaceProfile struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +optional
	Spec RuntimeWorkspaceProfileSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// RuntimeWorkspaceProfileList contains RuntimeWorkspaceProfile objects.
type RuntimeWorkspaceProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RuntimeWorkspaceProfile `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RuntimeWorkspaceProfile{}, &RuntimeWorkspaceProfileList{})
}
