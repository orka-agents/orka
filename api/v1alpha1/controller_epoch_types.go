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
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=controllerepochs,scope=Namespaced,shortName=cepoch
// +kubebuilder:printcolumn:name="Epoch",type=integer,JSONPath=`.status.epoch`
// +kubebuilder:printcolumn:name="Holder",type=string,JSONPath=`.status.holderId`
// +kubebuilder:printcolumn:name="Version",type=integer,JSONPath=`.status.version`
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
