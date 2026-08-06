/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// AgentExecutionControlName is the required name of the singleton control
// object, and AgentExecutionControlNamespace its required namespace. The
// controller ownership Lease shares the same fixed namespace.
const (
	AgentExecutionControlName      = "cluster"
	AgentExecutionControlNamespace = "orka-system"

	// AgentExecutionOwnershipLeaseName is the fixed global leader-election
	// Lease shared by every dual-controller release that can watch the same
	// Task population. It never varies by Helm release or namespace.
	AgentExecutionOwnershipLeaseName = "orka-agent-execution"

	// AgentExecutionLegacyLeaseName is the historical leader-election Lease
	// identity used by pre-coexistence controllers. Every holder must be
	// enumerated and fenced before dual ownership begins.
	AgentExecutionLegacyLeaseName = "03b49a10.orka.ai"
)

// AgentExecutionDesiredMode is an operator-requested admission mode for one
// harness backend. The internal closing barrier state is not requestable.
// +kubebuilder:validation:Enum=enabled;drain-only;disabled
type AgentExecutionDesiredMode string

const (
	// AgentExecutionModeEnabled admits new bindings and processes existing state.
	AgentExecutionModeEnabled AgentExecutionDesiredMode = "enabled"
	// AgentExecutionModeDrainOnly rejects new bindings but resumes, cancels,
	// settles, and finalizes state proven admitted before the cutoff.
	AgentExecutionModeDrainOnly AgentExecutionDesiredMode = "drain-only"
	// AgentExecutionModeDisabled rejects new bindings and new execution while
	// cleanup and recovery code remains registered.
	AgentExecutionModeDisabled AgentExecutionDesiredMode = "disabled"
)

// AgentExecutionEffectiveMode is the controller-observed admission mode,
// including the serialized closing barrier between enabled and drain-only.
// +kubebuilder:validation:Enum=enabled;closing;drain-only;disabled
type AgentExecutionEffectiveMode string

const (
	// AgentExecutionEffectiveModeEnabled admits new binding reservations.
	AgentExecutionEffectiveModeEnabled AgentExecutionEffectiveMode = "enabled"
	// AgentExecutionEffectiveModeClosing rejects new binding reservations while
	// every reservation created under the prior enabled revision is settled and
	// the cutoff closure is proved.
	AgentExecutionEffectiveModeClosing AgentExecutionEffectiveMode = "closing"
	// AgentExecutionEffectiveModeDrainOnly rejects new bindings after a proven
	// cutoff; pre-cutoff work may resume, cancel, settle, and finalize.
	AgentExecutionEffectiveModeDrainOnly AgentExecutionEffectiveMode = "drain-only"
	// AgentExecutionEffectiveModeDisabled rejects new bindings and execution.
	AgentExecutionEffectiveModeDisabled AgentExecutionEffectiveMode = "disabled"
)

// AgentExecutionBackendSpec is the desired admission mode for one backend.
type AgentExecutionBackendSpec struct {
	// DesiredMode requests an admission mode. The controller reaches drain-only
	// from enabled only through the internal closing barrier with closure proof.
	// +kubebuilder:validation:Required
	DesiredMode AgentExecutionDesiredMode `json:"desiredMode"`
}

// AgentExecutionBackendsSpec holds the desired mode for both harness backends.
type AgentExecutionBackendsSpec struct {
	// +kubebuilder:validation:Required
	V1 AgentExecutionBackendSpec `json:"v1"`

	// +kubebuilder:validation:Required
	V2 AgentExecutionBackendSpec `json:"v2"`
}

// AgentExecutionControlSpec is the admin-owned desired backend admission state.
type AgentExecutionControlSpec struct {
	// +kubebuilder:validation:Required
	Backends AgentExecutionBackendsSpec `json:"backends"`
}

// AgentExecutionLegacyLeaseFence records one continuously renewed legacy
// leader-election Lease held as a migration fence.
type AgentExecutionLegacyLeaseFence struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Namespace string `json:"namespace"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// +kubebuilder:validation:Required
	UID types.UID `json:"uid"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	ResourceVersion string `json:"resourceVersion"`
}

// AgentExecutionOwnershipStatus records the complete controller ownership
// fence set. Loss of any member closes readiness and stops mutating runnables.
type AgentExecutionOwnershipStatus struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	LeaseNamespace string `json:"leaseNamespace"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	LeaseName string `json:"leaseName"`

	// +kubebuilder:validation:Required
	UID types.UID `json:"leaseUID"`

	// ControllerEpoch is the monotonic fenced ownership epoch.
	// +kubebuilder:validation:Minimum=0
	ControllerEpoch int64 `json:"controllerEpoch"`

	// LegacyLeaseFences lists every fenced legacy controller Lease.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	// +listType=atomic
	LegacyLeaseFences []AgentExecutionLegacyLeaseFence `json:"legacyLeaseFences,omitempty"`
}

// AgentExecutionBackendStatus is the controller-observed mode for one backend.
// The control UID, generation, and modeRevision form the admission revision.
// +kubebuilder:validation:XValidation:rule="self.modeRevision >= oldSelf.modeRevision",message="modeRevision must not decrease"
// +kubebuilder:validation:XValidation:rule="!has(self.admissionClosedAt) || self.effectiveMode == 'drain-only' || self.effectiveMode == 'disabled'",message="admissionClosedAt requires a drain-only or disabled effective mode"
type AgentExecutionBackendStatus struct {
	// +kubebuilder:validation:Required
	EffectiveMode AgentExecutionEffectiveMode `json:"effectiveMode"`

	// ModeRevision increases on every effective mode transition.
	// +kubebuilder:validation:Minimum=1
	ModeRevision int64 `json:"modeRevision"`

	// AdmissionClosedAt records the proven admission cutoff.
	// +optional
	AdmissionClosedAt *metav1.Time `json:"admissionClosedAt,omitempty"`

	// CutoffInventoryDigest is the digest of the sealed cutoff inventory
	// recorded when the closing barrier completed.
	// +optional
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	CutoffInventoryDigest string `json:"cutoffInventoryDigest,omitempty"`
}

// AgentExecutionBackendsStatus holds observed state for both backends.
type AgentExecutionBackendsStatus struct {
	// +kubebuilder:validation:Required
	V1 AgentExecutionBackendStatus `json:"v1"`

	// +kubebuilder:validation:Required
	V2 AgentExecutionBackendStatus `json:"v2"`
}

// AgentExecutionClassificationState is the controller-owned migration
// inventory state. Execution remains closed while the inventory is Open.
// +kubebuilder:validation:Enum=Open;Sealed
type AgentExecutionClassificationState string

const (
	// AgentExecutionClassificationOpen means the source-aware legacy inventory
	// has not yet reached a complete, stable fixed point.
	AgentExecutionClassificationOpen AgentExecutionClassificationState = "Open"
	// AgentExecutionClassificationSealed means two stable uncached inventory
	// passes proved every execution- and cleanup-relevant object classified.
	AgentExecutionClassificationSealed AgentExecutionClassificationState = "Sealed"
)

// AgentExecutionClassificationStatus is the durable execution gate for the
// coexistence bridge. ControlUID and ControlGeneration bind a seal to one
// exact AgentExecutionControl incarnation and desired-state generation; a
// recreated or edited control therefore closes execution immediately.
type AgentExecutionClassificationStatus struct {
	// +kubebuilder:validation:Required
	State AgentExecutionClassificationState `json:"state"`

	// +kubebuilder:validation:Required
	ControlUID types.UID `json:"controlUID"`

	// +kubebuilder:validation:Minimum=1
	ControlGeneration int64 `json:"controlGeneration"`

	// InventoryID identifies the bounded sealed migration sweep that wrote
	// legacy Task dispositions and Session lineage.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	InventoryID string `json:"inventoryID"`

	// InventoryDigest commits to the exact source identities, resource
	// versions, evidence, and resulting dispositions observed by the sweep.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	InventoryDigest string `json:"inventoryDigest"`

	// ObservedAt is the first observation time for an Open digest and the seal
	// time for a Sealed digest.
	// +kubebuilder:validation:Required
	ObservedAt metav1.Time `json:"observedAt"`
}

// AgentExecutionControlStatus is the controller-owned observed admission and
// ownership state.
type AgentExecutionControlStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +optional
	Ownership *AgentExecutionOwnershipStatus `json:"ownership,omitempty"`

	// +optional
	Backends *AgentExecutionBackendsStatus `json:"backends,omitempty"`

	// Classification is the controller-owned, source-aware sealed inventory
	// marker. Binding and every execution/mutating runnable must verify an exact
	// Sealed marker through an uncached read; readiness alone is not authority.
	// +optional
	Classification *AgentExecutionClassificationStatus `json:"classification,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=aexctl
// +kubebuilder:printcolumn:name="V1",type=string,JSONPath=`.status.backends.v1.effectiveMode`
// +kubebuilder:printcolumn:name="V2",type=string,JSONPath=`.status.backends.v2.effectiveMode`
// +kubebuilder:printcolumn:name="Epoch",type=integer,JSONPath=`.status.ownership.controllerEpoch`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'cluster'",message="AgentExecutionControl is a singleton named cluster"

// AgentExecutionControl is the durable, revisioned backend admission control
// object. Recreating it is not a normal mode transition: a UID change forces
// cleanup-only behavior until operator reconciliation.
type AgentExecutionControl struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentExecutionControlSpec   `json:"spec"`
	Status AgentExecutionControlStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentExecutionControlList contains a list of AgentExecutionControl.
type AgentExecutionControlList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentExecutionControl `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentExecutionControl{}, &AgentExecutionControlList{})
}
