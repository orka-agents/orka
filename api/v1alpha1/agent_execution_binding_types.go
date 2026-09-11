/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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

// AgentExecutionRuntimeRef pins a referenced AgentRuntime identity.
type AgentExecutionRuntimeRef struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// +kubebuilder:validation:Required
	UID types.UID `json:"uid"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	Generation int64 `json:"generation"`
}

// AgentExecutionBinding is the controller-owned, write-once, immutable
// execution binding for one executable agent Task. It is authoritative for
// routing, recovery, cancellation, terminal settlement, and finalization, and
// is preserved across retries. Dispatchers build requests only from the
// referenced immutable snapshot.
// +kubebuilder:validation:XValidation:rule="self.backend != 'harness-wrapper' || self.contractVersion == 'orka.harness.v1'",message="the harness-wrapper backend requires an orka.harness.v1 binding"
// +kubebuilder:validation:XValidation:rule="self.backend != 'runtime-pool' || self.contractVersion == 'orka.harness.v2'",message="the runtime-pool backend requires an orka.harness.v2 binding"
type AgentExecutionBinding struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=1
	SchemaVersion int32 `json:"schemaVersion"`

	// ContractVersion is the frozen execution protocol for the Task lifetime.
	// +kubebuilder:validation:Required
	ContractVersion AgentRuntimeContractVersion `json:"contractVersion"`

	// +kubebuilder:validation:Required
	Backend AgentExecutionBackend `json:"backend"`

	// BindingDigest is the canonical digest of this binding; every durable
	// demand, attempt, turn, Session lease, publication, and cleanup record
	// copies it.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	BindingDigest string `json:"bindingDigest"`

	// +kubebuilder:validation:Required
	Task AgentExecutionBindingTaskRef `json:"task"`

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
