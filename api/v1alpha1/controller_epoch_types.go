/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// ControllerEpochSpec is the immutable epoch-domain identity. The associated
// coordination.k8s.io Lease is the CAS authority for holder and epoch changes.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="controller epoch spec is immutable"
type ControllerEpochSpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// ControllerEpochStatus mirrors the authoritative Lease state for inspection
// and recovery. LeaseResourceVersion identifies the exact Lease revision.
type ControllerEpochStatus struct {
	// +optional
	// +kubebuilder:validation:Minimum=1
	Epoch int64 `json:"epoch,omitempty"`

	// +optional
	// +kubebuilder:validation:MaxLength=1024
	HolderID string `json:"holderId,omitempty"`

	// +optional
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	RequestDigest string `json:"requestDigest,omitempty"`

	// +optional
	// +kubebuilder:validation:Minimum=1
	Version int64 `json:"version,omitempty"`

	// +optional
	AcquiredAt *metav1.Time `json:"acquiredAt,omitempty"`

	// +optional
	UpdatedAt *metav1.Time `json:"updatedAt,omitempty"`

	// +optional
	// +kubebuilder:validation:MaxLength=253
	LeaseName string `json:"leaseName,omitempty"`

	// +optional
	// +kubebuilder:validation:MaxLength=64
	LeaseResourceVersion string `json:"leaseResourceVersion,omitempty"`

	// StorageEpochs records, for each storage contract, the dataset that
	// serves it, the controller epoch that dataset acknowledged and the
	// highest write watermark it acknowledged. At takeover, and whenever a
	// dataset's database restarted or switched, Orka compares the dataset with
	// this record; a dataset behind it has lost acknowledged writes, and Orka
	// holds the installation for storage recovery.
	// +optional
	// +listType=map
	// +listMapKey=contract
	// +kubebuilder:validation:MaxItems=8
	StorageEpochs []StorageEpochAcknowledgement `json:"storageEpochs,omitempty"`

	// StorageRecovery is the installation's storage recovery hold. While it is
	// set, Orka refuses storage contract writes and control-store changes,
	// creates no Jobs and reports no Task success. An operator releases it, after
	// recovering storage, by setting the storage.orka.ai/recovery-acknowledged
	// annotation of this object to the hold's ID.
	// +optional
	StorageRecovery *StorageRecoveryHold `json:"storageRecovery,omitempty"`

	// Conditions include StorageRecoveryRequired, which is True while the
	// installation is held for storage recovery.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=8
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// StorageEpochAcknowledgement is what one storage contract's dataset
// acknowledged to this installation.
type StorageEpochAcknowledgement struct {
	// Contract is the storage contract, such as state.v1 or artifacts.v1.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	Contract string `json:"contract"`

	// DatasetID identifies the dataset that serves the contract.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	DatasetID string `json:"datasetId"`

	// Epoch is the controller epoch the dataset acknowledged.
	// +kubebuilder:validation:Minimum=1
	Epoch int64 `json:"epoch"`

	// AcknowledgedAt is when Orka recorded Epoch.
	AcknowledgedAt metav1.Time `json:"acknowledgedAt"`

	// WriteWatermark is the highest write watermark the dataset acknowledged.
	// +optional
	// +kubebuilder:validation:Minimum=0
	WriteWatermark int64 `json:"writeWatermark,omitempty"`

	// WatermarkAcknowledgedAt is when Orka recorded WriteWatermark.
	// +optional
	WatermarkAcknowledgedAt *metav1.Time `json:"watermarkAcknowledgedAt,omitempty"`
}

const (
	// StorageRecoveryAcknowledgedAnnotation releases the storage recovery hold
	// whose ID it names. An operator sets it on the ControllerEpoch after
	// recovering storage.
	StorageRecoveryAcknowledgedAnnotation = "storage.orka.ai/recovery-acknowledged"
	// ConditionStorageRecoveryRequired is True while the installation is held
	// for storage recovery.
	ConditionStorageRecoveryRequired = "StorageRecoveryRequired"
	// StorageRecoveryAcknowledgedReason is the reason of a
	// StorageRecoveryRequired condition that an operator released.
	StorageRecoveryAcknowledgedReason = "RecoveryAcknowledged"
)

// StorageRecoveryReason says why Orka held the installation for storage
// recovery.
// +kubebuilder:validation:Enum=EpochRegressed;WatermarkRegressed;DatasetChanged;UnsafeSwitch
type StorageRecoveryReason string

const (
	// StorageRecoveryEpochRegressed means a dataset holds a lower controller
	// epoch than it acknowledged.
	StorageRecoveryEpochRegressed StorageRecoveryReason = "EpochRegressed"
	// StorageRecoveryWatermarkRegressed means a dataset holds a lower write
	// watermark than it acknowledged.
	StorageRecoveryWatermarkRegressed StorageRecoveryReason = "WatermarkRegressed"
	// StorageRecoveryDatasetChanged means a storage contract is served by a
	// different dataset than the one that acknowledged it.
	StorageRecoveryDatasetChanged StorageRecoveryReason = "DatasetChanged"
	// StorageRecoveryUnsafeSwitch means the database of a dataset whose
	// durability class can lose acknowledged writes in a failover restarted or
	// switched.
	StorageRecoveryUnsafeSwitch StorageRecoveryReason = "UnsafeSwitch"
)

// StorageRecoveryHold records why and where Orka stopped work for storage
// recovery.
type StorageRecoveryHold struct {
	// ID identifies the hold. Setting the storage.orka.ai/recovery-acknowledged
	// annotation to it releases this hold and no other.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	ID string `json:"id"`

	// Reason says what Orka found.
	Reason StorageRecoveryReason `json:"reason"`

	// Message describes what Orka found.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	Message string `json:"message,omitempty"`

	// Contract is the storage contract whose dataset Orka was checking.
	// +optional
	// +kubebuilder:validation:MaxLength=64
	Contract string `json:"contract,omitempty"`

	// DatasetID identifies the dataset that serves Contract now.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	DatasetID string `json:"datasetId,omitempty"`

	// AcknowledgedDatasetID identifies the dataset that acknowledged Contract
	// before.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	AcknowledgedDatasetID string `json:"acknowledgedDatasetId,omitempty"`

	// AcknowledgedEpoch and StoredEpoch are the controller epoch the dataset
	// acknowledged and the one it held.
	// +optional
	// +kubebuilder:validation:Minimum=0
	AcknowledgedEpoch int64 `json:"acknowledgedEpoch,omitempty"`
	// +optional
	// +kubebuilder:validation:Minimum=0
	StoredEpoch int64 `json:"storedEpoch,omitempty"`

	// AcknowledgedWatermark and StoredWatermark are the write watermark the
	// dataset acknowledged and the one it held.
	// +optional
	// +kubebuilder:validation:Minimum=0
	AcknowledgedWatermark int64 `json:"acknowledgedWatermark,omitempty"`
	// +optional
	// +kubebuilder:validation:Minimum=0
	StoredWatermark int64 `json:"storedWatermark,omitempty"`

	// Durability is the durability class the dataset reported.
	// +optional
	// +kubebuilder:validation:MaxLength=64
	Durability string `json:"durability,omitempty"`

	// RaisedAt is when Orka recorded the hold.
	RaisedAt metav1.Time `json:"raisedAt"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=controllerepochs,scope=Namespaced,shortName=cepoch
// +kubebuilder:printcolumn:name="Epoch",type=integer,JSONPath=`.status.epoch`
// +kubebuilder:printcolumn:name="Holder",type=string,JSONPath=`.status.holderId`
// +kubebuilder:printcolumn:name="Version",type=integer,JSONPath=`.status.version`
// +kubebuilder:printcolumn:name="Storage Hold",type=string,JSONPath=`.status.storageRecovery.reason`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ControllerEpoch is the human-visible Kubernetes control record paired with
// an authoritative namespaced Lease.
type ControllerEpoch struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ControllerEpochSpec   `json:"spec"`
	Status ControllerEpochStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ControllerEpochList contains a list of ControllerEpoch.
type ControllerEpochList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ControllerEpoch `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ControllerEpoch{}, &ControllerEpochList{})
}
