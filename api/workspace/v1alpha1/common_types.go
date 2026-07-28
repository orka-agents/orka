/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	types "k8s.io/apimachinery/pkg/types"
)

const (
	// ContractVersionV1 is the first provider/data-plane contract understood by Orka.
	ContractVersionV1 = "workspace.orka.ai/v1"

	// LegacyGeneratedLabel marks resources materialized from legacy provider-specific configuration.
	LegacyGeneratedLabel = "workspace.orka.ai/legacy-generated"
	// ProviderControllerLabel records the adapter controller that owns a provider-bound resource.
	ProviderControllerLabel = "workspace.orka.ai/controller-name"
	// QuarantinedLabel identifies workspaces that must never be selected for reuse.
	QuarantinedLabel = "workspace.orka.ai/quarantined"
)

// TypedObjectReference identifies a provider-owned parameter object. References never carry a namespace:
// namespaced resources resolve them in their own namespace and cluster-scoped resources may reference only
// cluster-scoped parameter kinds.
type TypedObjectReference struct {
	// Group is the API group of the referenced object.
	// +kubebuilder:validation:MinLength=1
	Group string `json:"group"`

	// Kind is the kind of the referenced object.
	// +kubebuilder:validation:MinLength=1
	Kind string `json:"kind"`

	// Name is the name of the referenced object.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// ClusterObjectReference identifies a cluster-scoped workspace API object.
type ClusterObjectReference struct {
	// Name is the name of the referenced cluster-scoped object.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// ImmutableObjectBinding pins a reference to the exact object revision resolved by Orka.
// +kubebuilder:validation:XValidation:rule="self.uid.size() > 0",message="uid is required"
type ImmutableObjectBinding struct {
	// Name is the bound object's name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// UID is the bound object's immutable Kubernetes UID.
	UID types.UID `json:"uid"`

	// Generation is the observed generation used to resolve the binding.
	// +kubebuilder:validation:Minimum=1
	Generation int64 `json:"generation"`

	// ProfileHash is a SHA-256 digest of the functional profile resolved for this binding.
	// It is populated for class bindings and may be omitted for provider bindings.
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	// +optional
	ProfileHash string `json:"profileHash,omitempty"`
}

// ObjectIdentityReference pins a namespaced owner/session object by name and UID.
// +kubebuilder:validation:XValidation:rule="self.uid.size() > 0",message="uid is required"
type ObjectIdentityReference struct {
	// Name is the object name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// UID is the immutable object UID.
	UID types.UID `json:"uid"`
}

// ExecutionWorkspaceFeature names a provider capability required by a class.
// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9.-]{0,62}$`
type ExecutionWorkspaceFeature string

const (
	WorkspaceFeatureExec         ExecutionWorkspaceFeature = "exec"
	WorkspaceFeatureFiles        ExecutionWorkspaceFeature = "files"
	WorkspaceFeatureReset        ExecutionWorkspaceFeature = "reset"
	WorkspaceFeatureSuspend      ExecutionWorkspaceFeature = "suspend"
	WorkspaceFeatureServicePorts ExecutionWorkspaceFeature = "service-ports"
	WorkspaceFeaturePools        ExecutionWorkspaceFeature = "pools"
	WorkspaceFeatureTLS          ExecutionWorkspaceFeature = "tls"
)

// ExecutionWorkspaceMode distinguishes task-attached workspaces from persistent service workspaces.
// +kubebuilder:validation:Enum=Interactive;Service
type ExecutionWorkspaceMode string

const (
	ExecutionWorkspaceModeInteractive ExecutionWorkspaceMode = "Interactive"
	ExecutionWorkspaceModeService     ExecutionWorkspaceMode = "Service"
)

// WorkspaceReuseScope is a reuse scope a class permits.
// +kubebuilder:validation:Enum=None;Session
type WorkspaceReuseScope string

const (
	WorkspaceReuseScopeNone    WorkspaceReuseScope = "None"
	WorkspaceReuseScopeSession WorkspaceReuseScope = "Session"
)

// WorkspaceOnDetach is the lifecycle action taken after an interactive attachment is revoked.
// +kubebuilder:validation:Enum=Suspend;Delete
type WorkspaceOnDetach string

const (
	WorkspaceOnDetachSuspend WorkspaceOnDetach = "Suspend"
	WorkspaceOnDetachDelete  WorkspaceOnDetach = "Delete"
)

// WorkspaceDeletionAction declares the disposition of a category of workspace data.
// +kubebuilder:validation:Enum=Delete;Retain
type WorkspaceDeletionAction string

const (
	WorkspaceDeletionActionDelete WorkspaceDeletionAction = "Delete"
	WorkspaceDeletionActionRetain WorkspaceDeletionAction = "Retain"
)

// ExecutionWorkspaceLifecycle is the resolved lifecycle contract for a class or concrete workspace.
// +kubebuilder:validation:XValidation:rule="self.allowedOnDetach.exists(action, action == self.defaultOnDetach)",message="defaultOnDetach must be included in allowedOnDetach"
// +kubebuilder:validation:XValidation:rule="duration(self.detachTimeout) > duration('0s')",message="detachTimeout must be positive"
// +kubebuilder:validation:XValidation:rule="!has(self.idleTimeout) || duration(self.idleTimeout) > duration('0s')",message="idleTimeout must be positive when set"
// +kubebuilder:validation:XValidation:rule="!has(self.maxLifetime) || duration(self.maxLifetime) > duration('0s')",message="maxLifetime must be positive when set"
// +kubebuilder:validation:XValidation:rule="!has(self.idleTimeout) || !has(self.maxLifetime) || duration(self.maxLifetime) >= duration(self.idleTimeout)",message="maxLifetime must be greater than or equal to idleTimeout"
type ExecutionWorkspaceLifecycle struct {
	// DefaultOnDetach is used when a Task does not request an allowed override.
	// +kubebuilder:default=Suspend
	DefaultOnDetach WorkspaceOnDetach `json:"defaultOnDetach"`

	// AllowedOnDetach lists the actions a Task may request.
	// +kubebuilder:validation:MinItems=1
	// +listType=set
	AllowedOnDetach []WorkspaceOnDetach `json:"allowedOnDetach"`

	// DetachTimeout bounds how long Task finalization waits for attachment revocation.
	// +kubebuilder:default="2m"
	DetachTimeout metav1.Duration `json:"detachTimeout"`

	// IdleTimeout is the maximum idle duration before the provider may apply the class policy.
	// +optional
	IdleTimeout *metav1.Duration `json:"idleTimeout,omitempty"`

	// MaxLifetime is the maximum lifetime of a concrete workspace.
	// +optional
	MaxLifetime *metav1.Duration `json:"maxLifetime,omitempty"`

	// DeletionPolicy describes the required disposition of provider and retained data.
	DeletionPolicy ExecutionWorkspaceDeletionPolicy `json:"deletionPolicy"`
}

// ExecutionWorkspaceDeletionPolicy declares disposition requirements when a workspace is deleted.
type ExecutionWorkspaceDeletionPolicy struct {
	// ProviderResources controls provider-native compute and control-plane objects.
	ProviderResources WorkspaceDeletionAction `json:"providerResources"`

	// PersistentVolumes controls durable volume retention.
	PersistentVolumes WorkspaceDeletionAction `json:"persistentVolumes"`

	// Checkpoints controls provider checkpoints and snapshots.
	Checkpoints WorkspaceDeletionAction `json:"checkpoints"`
}

// ExecutionWorkspaceConditionType is a stable generic condition name.
type ExecutionWorkspaceConditionType string

const (
	ConditionProviderReady           ExecutionWorkspaceConditionType = "Ready"
	ConditionProviderCompatible      ExecutionWorkspaceConditionType = "Compatible"
	ConditionProviderHeartbeat       ExecutionWorkspaceConditionType = "HeartbeatFresh"
	ConditionClassReady              ExecutionWorkspaceConditionType = "Ready"
	ConditionPoolReady               ExecutionWorkspaceConditionType = "Ready"
	ConditionPoolAdmitted            ExecutionWorkspaceConditionType = "Admitted"
	ConditionWorkspaceAdmitted       ExecutionWorkspaceConditionType = "Admitted"
	ConditionWorkspaceProvisioned    ExecutionWorkspaceConditionType = "Provisioned"
	ConditionWorkspaceDataPlaneReady ExecutionWorkspaceConditionType = "DataPlaneReady"
	ConditionWorkspaceAttached       ExecutionWorkspaceConditionType = "Attached"
	ConditionWorkspaceFinalized      ExecutionWorkspaceConditionType = "Finalized"
	ConditionWorkspaceQuarantined    ExecutionWorkspaceConditionType = "Quarantined"
)

// ExecutionWorkspaceConditionReason is a bounded reason used by generic controllers and adapters.
type ExecutionWorkspaceConditionReason string

const (
	ReasonReady                ExecutionWorkspaceConditionReason = "Ready"
	ReasonProgressing          ExecutionWorkspaceConditionReason = "Progressing"
	ReasonHeartbeatExpired     ExecutionWorkspaceConditionReason = "HeartbeatExpired"
	ReasonIncompatibleContract ExecutionWorkspaceConditionReason = "IncompatibleContract"
	ReasonProviderDraining     ExecutionWorkspaceConditionReason = "ProviderDraining"
	ReasonProviderDisabled     ExecutionWorkspaceConditionReason = "ProviderDisabled"
	ReasonCapacityUnavailable  ExecutionWorkspaceConditionReason = "CapacityUnavailable"
	ReasonAuthorizationDenied  ExecutionWorkspaceConditionReason = "AuthorizationDenied"
	ReasonAttachmentRevoked    ExecutionWorkspaceConditionReason = "AttachmentRevoked"
	ReasonCleanupFailed        ExecutionWorkspaceConditionReason = "CleanupFailed"
	ReasonQuarantined          ExecutionWorkspaceConditionReason = "Quarantined"
)

// SecretReference is a local Secret reference used for connection and attachment credentials.
type SecretReference struct {
	// Name is the Secret name in the workspace namespace.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// LocalObjectReference aliases the Kubernetes local reference shape in public workspace APIs.
type LocalObjectReference = corev1.LocalObjectReference
