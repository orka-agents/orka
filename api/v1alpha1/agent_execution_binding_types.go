/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// AgentExecutionBindingMode bounds what the binding authorizes.
// +kubebuilder:validation:Enum=execute;cleanup-only
type AgentExecutionBindingMode string

const (
	// AgentExecutionBindingModeExecute authorizes execution and cleanup.
	AgentExecutionBindingModeExecute AgentExecutionBindingMode = "execute"
	// AgentExecutionBindingModeCleanupOnly authorizes observation,
	// cancellation, and settlement only; never retry, continuation, or a
	// recreated runtime.
	AgentExecutionBindingModeCleanupOnly AgentExecutionBindingMode = "cleanup-only"
)

// AgentExecutionBackend identifies the isolated execution dispatcher backend.
// +kubebuilder:validation:Enum=harness-wrapper;runtime-pool;external-endpoint
type AgentExecutionBackend string

const (
	// AgentExecutionBackendHarnessWrapper is the built-in harness v1 wrapper.
	AgentExecutionBackendHarnessWrapper AgentExecutionBackend = "harness-wrapper"
	// AgentExecutionBackendRuntimePool is the managed ACP v2 RuntimePool path.
	AgentExecutionBackendRuntimePool AgentExecutionBackend = "runtime-pool"
	// AgentExecutionBackendExternalEndpoint is an external AgentRuntime endpoint.
	AgentExecutionBackendExternalEndpoint AgentExecutionBackend = "external-endpoint"
)

// AgentExecutionBindingProvenance records how the binding was established.
// +kubebuilder:validation:Enum=newly-bound;legacy-adopted;legacy-cleanup-only
type AgentExecutionBindingProvenance string

const (
	// AgentExecutionProvenanceNewlyBound is a binding created by the dual
	// controller through the reservation-backed binding stage.
	AgentExecutionProvenanceNewlyBound AgentExecutionBindingProvenance = "newly-bound"
	// AgentExecutionProvenanceLegacyAdopted is a pre-existing Task adopted from
	// exact single-protocol authoritative evidence during the sealed inventory.
	AgentExecutionProvenanceLegacyAdopted AgentExecutionBindingProvenance = "legacy-adopted"
	// AgentExecutionProvenanceLegacyCleanupOnly is a legacy record lacking
	// complete identity evidence; observation, cancellation, and settlement
	// only.
	AgentExecutionProvenanceLegacyCleanupOnly AgentExecutionBindingProvenance = "legacy-cleanup-only"
)

// AgentExecutionBindingTaskRef pins the bound Task identity.
type AgentExecutionBindingTaskRef struct {
	// NamespaceUID is the UID of the Task namespace, preventing same-name
	// namespace recreation from satisfying old identities.
	// +kubebuilder:validation:Required
	NamespaceUID types.UID `json:"namespaceUID"`

	// +kubebuilder:validation:Required
	UID types.UID `json:"uid"`

	// BoundSpecGeneration is the Task spec generation frozen into the snapshot.
	// +kubebuilder:validation:Minimum=1
	BoundSpecGeneration int64 `json:"boundSpecGeneration"`
}

// AgentExecutionBackendControlRef pins the AgentExecutionControl admission
// revision the binding was admitted under.
type AgentExecutionBackendControlRef struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// +kubebuilder:validation:Required
	UID types.UID `json:"uid"`

	// +kubebuilder:validation:Minimum=1
	Generation int64 `json:"generation"`

	// +kubebuilder:validation:Minimum=1
	ModeRevision int64 `json:"modeRevision"`

	// AdmittedMode is the effective backend mode observed at admission.
	// +kubebuilder:validation:Required
	AdmittedMode AgentExecutionEffectiveMode `json:"admittedMode"`
}

// AgentExecutionPolicyRef freezes the evaluated compatibility policy identity.
type AgentExecutionPolicyRef struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// +kubebuilder:validation:Required
	UID types.UID `json:"uid"`

	// +kubebuilder:validation:Minimum=1
	Generation int64 `json:"generation"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	Digest string `json:"digest"`
}

// AgentExecutionAgentRef pins the exact Agent identity resolved at binding.
type AgentExecutionAgentRef struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// +kubebuilder:validation:Required
	UID types.UID `json:"uid"`

	// +kubebuilder:validation:Minimum=1
	Generation int64 `json:"generation"`
}

// AgentExecutionSnapshotRef links the immutable non-secret execution snapshot.
type AgentExecutionSnapshotRef struct {
	// ID is the snapshot identity in the form <task-uid>/sha256:<digest>.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	ID string `json:"id"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	Digest string `json:"digest"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=1
	SchemaVersion int32 `json:"schemaVersion"`
}

// AgentExecutionRuntimeRef pins a referenced AgentRuntime identity. UID and
// generation are recorded only for legacy cleanup-only bindings whose live
// object may no longer exist.
type AgentExecutionRuntimeRef struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// +optional
	UID types.UID `json:"uid,omitempty"`

	// +optional
	// +kubebuilder:validation:Minimum=1
	Generation int64 `json:"generation,omitempty"`
}

// AgentExecutionBinding is the controller-owned, write-once, immutable
// execution binding for one executable agent Task. It is authoritative for
// routing, recovery, cancellation, terminal settlement, and finalization, and
// is preserved across retries. Dispatchers build requests only from the
// referenced immutable snapshot.
// +kubebuilder:validation:XValidation:rule="self.backend != 'harness-wrapper' || self.contractVersion == 'orka.harness.v1'",message="the harness-wrapper backend requires an orka.harness.v1 binding"
// +kubebuilder:validation:XValidation:rule="self.backend != 'runtime-pool' || self.contractVersion == 'orka.harness.v2'",message="the runtime-pool backend requires an orka.harness.v2 binding"
// +kubebuilder:validation:XValidation:rule="self.provenance != 'legacy-cleanup-only' || self.mode == 'cleanup-only'",message="legacy-cleanup-only provenance requires cleanup-only mode"
// +kubebuilder:validation:XValidation:rule="self.provenance == 'legacy-cleanup-only' ? has(self.migrationInventoryID) : !has(self.migrationInventoryID)",message="migrationInventoryID is required only for legacy-cleanup-only provenance"
type AgentExecutionBinding struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=1
	SchemaVersion int32 `json:"schemaVersion"`

	// +kubebuilder:validation:Required
	Mode AgentExecutionBindingMode `json:"mode"`

	// ContractVersion is the frozen execution protocol for the Task lifetime.
	// +kubebuilder:validation:Required
	ContractVersion AgentRuntimeContractVersion `json:"contractVersion"`

	// +kubebuilder:validation:Required
	Backend AgentExecutionBackend `json:"backend"`

	// +kubebuilder:validation:Required
	Provenance AgentExecutionBindingProvenance `json:"provenance"`

	// BindingDigest is the canonical digest of this binding; every durable
	// demand, attempt, turn, Session lease, publication, and cleanup record
	// copies it.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	BindingDigest string `json:"bindingDigest"`

	// +kubebuilder:validation:Required
	Task AgentExecutionBindingTaskRef `json:"task"`

	// MigrationInventoryID binds legacy cleanup-only authority to the exact
	// sealed classification inventory that admitted it. It is absent from all
	// executable and ordinary legacy-adopted bindings.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	MigrationInventoryID string `json:"migrationInventoryID,omitempty"`

	// BackendControl is absent only on legacy-adopted and legacy-cleanup-only
	// bindings created by the sealed migration inventory before durable backend
	// modes existed for the workload.
	// +optional
	BackendControl *AgentExecutionBackendControlRef `json:"backendControl,omitempty"`

	// Policy freezes the evaluated v1 compatibility policy. Required for
	// newly-bound v1 bindings; absent for v2 bindings.
	// +optional
	Policy *AgentExecutionPolicyRef `json:"policy,omitempty"`

	// +optional
	Agent *AgentExecutionAgentRef `json:"agent,omitempty"`

	// +kubebuilder:validation:Required
	Snapshot AgentExecutionSnapshotRef `json:"snapshot"`

	// RuntimeType is the built-in runtime type, empty for runtimeRef bindings.
	// +optional
	RuntimeType AgentRuntimeType `json:"runtimeType,omitempty"`

	// +optional
	RuntimeRef *AgentExecutionRuntimeRef `json:"runtimeRef,omitempty"`

	// +optional
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	RuntimeProfileDigest string `json:"runtimeProfileDigest,omitempty"`

	// +optional
	// +kubebuilder:validation:Enum=1
	RuntimeProfileDigestSchemaVersion int32 `json:"runtimeProfileDigestSchemaVersion,omitempty"`

	// +kubebuilder:validation:Required
	BoundAt metav1.Time `json:"boundAt"`
}

// AgentExecutionNoExecutionState is the proven no-route-state disposition.
// +kubebuilder:validation:Enum=UnboundNoExecution
type AgentExecutionNoExecutionState string

// AgentExecutionNoExecutionUnbound records authoritative proof that a Task
// never acquired route-specific executor state.
const AgentExecutionNoExecutionUnbound AgentExecutionNoExecutionState = "UnboundNoExecution"

// AgentExecutionNoExecution is the immutable proven-no-execution disposition.
// It permits common cleanup only and can never become an executable binding.
type AgentExecutionNoExecution struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=1
	SchemaVersion int32 `json:"schemaVersion"`

	// +kubebuilder:validation:Required
	State AgentExecutionNoExecutionState `json:"state"`

	// MigrationInventoryID names the sealed inventory that proved the absence
	// of route-specific state.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	MigrationInventoryID string `json:"migrationInventoryID"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	EvidenceDigest string `json:"evidenceDigest"`

	// +kubebuilder:validation:Required
	RecordedAt metav1.Time `json:"recordedAt"`
}

// AgentExecutionQuarantineReason is a bounded quarantine reason code.
// +kubebuilder:validation:Enum=MixedV1V2Evidence;AmbiguousLegacyEvidence;UnclassifiedAgent;UnprovableSessionLineage;PostCutoffBinding;MissingBindingReservation;StaleModeRevision
type AgentExecutionQuarantineReason string

const (
	AgentExecutionQuarantineMixedEvidence            AgentExecutionQuarantineReason = "MixedV1V2Evidence"
	AgentExecutionQuarantineAmbiguousLegacyEvidence  AgentExecutionQuarantineReason = "AmbiguousLegacyEvidence"
	AgentExecutionQuarantineUnclassifiedAgent        AgentExecutionQuarantineReason = "UnclassifiedAgent"
	AgentExecutionQuarantineUnprovableSessionLineage AgentExecutionQuarantineReason = "UnprovableSessionLineage"
	AgentExecutionQuarantinePostCutoffBinding        AgentExecutionQuarantineReason = "PostCutoffBinding"
	AgentExecutionQuarantineMissingReservation       AgentExecutionQuarantineReason = "MissingBindingReservation"
	AgentExecutionQuarantineStaleModeRevision        AgentExecutionQuarantineReason = "StaleModeRevision"
)

// AgentExecutionQuarantine is the immutable record of mixed, contradictory, or
// unprovable route evidence. Quarantined Tasks admit no new submission,
// continuation, retry, or publication and remain blocked until explicit
// adjudication or verified cleanup of all lineages. Evidence is never cleared
// or rewritten; resolution is a separate adjudication record.
type AgentExecutionQuarantine struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=1
	SchemaVersion int32 `json:"schemaVersion"`

	// +kubebuilder:validation:Required
	Reason AgentExecutionQuarantineReason `json:"reason"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	MigrationInventoryID string `json:"migrationInventoryID"`

	// +optional
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	V1EvidenceDigest string `json:"v1EvidenceDigest,omitempty"`

	// +optional
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	V2EvidenceDigest string `json:"v2EvidenceDigest,omitempty"`

	// +kubebuilder:validation:Required
	RecordedAt metav1.Time `json:"recordedAt"`
}

// AgentExecutionResolutionRef is the immutable subject-side reference to an
// Applied AgentExecutionAdjudication. Route-aware finalization consumes only a
// resolution referenced by this exact record.
type AgentExecutionResolutionRef struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	AdjudicationName string `json:"adjudicationName"`

	// +kubebuilder:validation:Required
	AdjudicationUID types.UID `json:"adjudicationUID"`

	// +kubebuilder:validation:Required
	Action AgentExecutionAdjudicationAction `json:"action"`

	// OperationDigest matches the applying adjudication's status.operationDigest.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	OperationDigest string `json:"operationDigest"`

	// ResolutionDigest is the canonical digest of this resolution reference and
	// matches the adjudication's status.resolutionRefDigest.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	ResolutionDigest string `json:"resolutionDigest"`

	// +kubebuilder:validation:Required
	AppliedAt metav1.Time `json:"appliedAt"`
}
