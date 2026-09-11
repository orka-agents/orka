/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

const (
	// ControlRecordArtifactRetentionFinalizer prevents deletion while a control
	// record can still retain or reconcile a content-addressed artifact.
	ControlRecordArtifactRetentionFinalizer = "core.orka.ai/artifact-retention"
	// ControlRecordLeaseProtectionFinalizer prevents deletion of a control record
	// while a Kubernetes Lease still fences mutations for that record.
	ControlRecordLeaseProtectionFinalizer = "core.orka.ai/lease-protection"

	// ControlRecordIDHashLabel stores a DNS-safe digest of the immutable logical
	// record ID so namespaced records can be found without trusting mutable data.
	ControlRecordIDHashLabel = "core.orka.ai/control-record-id-hash"
	// ControlRecordTaskUIDLabel associates a controller-owned record with the
	// immutable UID of the Task that caused it to be created.
	ControlRecordTaskUIDLabel = "core.orka.ai/task-uid"
)

// ControlRecordMutationStatus is the common fenced mutation metadata embedded
// in Kubernetes-authoritative ACP control records. Kubernetes resourceVersion
// provides the storage CAS; Version is the monotonic domain version expected by
// DurableControlStore callers.
type ControlRecordMutationStatus struct {
	// ControllerEpochName identifies the controller epoch domain checked before
	// the mutation.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	ControllerEpochName string `json:"controllerEpochName,omitempty"`

	// ControllerEpoch is the exact epoch that performed the last mutation.
	// +optional
	// +kubebuilder:validation:Minimum=1
	ControllerEpoch int64 `json:"controllerEpoch,omitempty"`

	// ControllerEpochLeaseResourceVersion is the resourceVersion of the
	// authoritative controller-epoch Lease observed by the mutation.
	// +optional
	// +kubebuilder:validation:MaxLength=64
	ControllerEpochLeaseResourceVersion string `json:"controllerEpochLeaseResourceVersion,omitempty"`

	// LastOperationID is the last idempotent mutation identity applied.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	LastOperationID string `json:"lastOperationId,omitempty"`

	// LastOperationDigest binds LastOperationID to exact canonical input.
	// +optional
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	LastOperationDigest string `json:"lastOperationDigest,omitempty"`

	// Version is the monotonic domain CAS version. It advances once for each
	// successfully persisted logical mutation.
	// +optional
	// +kubebuilder:validation:Minimum=1
	Version int64 `json:"version,omitempty"`

	// CreatedAt is the normalized logical creation time.
	// +optional
	CreatedAt *metav1.Time `json:"createdAt,omitempty"`

	// UpdatedAt is the normalized logical mutation time.
	// +optional
	UpdatedAt *metav1.Time `json:"updatedAt,omitempty"`
}

// ControlRemoteRefState is an exact remote-ref observation. Absent and SHA are
// mutually exclusive. The all-zero value is reserved for an explicitly unknown
// observation in PublicationOutcomeUnknown receipts.
// +kubebuilder:validation:XValidation:rule="!(self.absent && has(self.sha) && size(self.sha) > 0)",message="absent and sha are mutually exclusive"
type ControlRemoteRefState struct {
	Absent bool `json:"absent"`

	// +optional
	// +kubebuilder:validation:Pattern=`^([a-f0-9]{40}|[a-f0-9]{64})$`
	SHA string `json:"sha,omitempty"`
}

// ControlVerifiedBranchBaseline is an independently verified branch baseline.
type ControlVerifiedBranchBaseline struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	RepositoryID string `json:"repositoryId"`

	// +kubebuilder:validation:Pattern=`^refs/heads/.+$`
	// +kubebuilder:validation:MaxLength=1024
	Ref string `json:"ref"`

	// +kubebuilder:validation:Pattern=`^([a-f0-9]{40}|[a-f0-9]{64})$`
	SHA string `json:"sha"`
}

// ControlRecordOwner identifies an immutable namespaced owner when the
// Kubernetes object name is not part of the DurableControlStore interface.
type ControlRecordOwner struct {
	// +kubebuilder:validation:Enum=Task;Session;RuntimePool;PromptAttempt
	Kind string `json:"kind"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	UID string `json:"uid"`
}

// RBAC for the Kubernetes-authoritative ACP control-record store. These
// markers intentionally live with the API surface so generation does not
// require wiring the store into cmd/main.go.
// +kubebuilder:rbac:groups=core.orka.ai,resources=promptattempts;runtimesessioncontrols;branchclaims;publications;externaleffects;controllerepochs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.orka.ai,resources=promptattempts/status;runtimesessioncontrols/status;branchclaims/status;publications/status;externaleffects/status;controllerepochs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.orka.ai,resources=promptattempts/finalizers;runtimesessioncontrols/finalizers;branchclaims/finalizers;publications/finalizers;externaleffects/finalizers;controllerepochs/finalizers,verbs=update
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
