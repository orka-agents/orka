/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// ExecutionWorkspacePoolCapacity defines provider-managed warm/reusable capacity.
// +kubebuilder:validation:XValidation:rule="self.maxSize >= self.minReady",message="maxSize must be greater than or equal to minReady"
type ExecutionWorkspacePoolCapacity struct {
	// MinReady is the desired minimum number of immediately allocatable workspaces.
	// +kubebuilder:validation:Minimum=0
	MinReady int32 `json:"minReady"`

	// MaxSize is the hard provider-side allocation limit for this pool.
	// +kubebuilder:validation:Minimum=0
	MaxSize int32 `json:"maxSize"`
}

// ExecutionWorkspacePoolSpec defines generic provider-managed capacity.
// +kubebuilder:validation:XValidation:rule="self.providerRef == oldSelf.providerRef",message="providerRef is immutable"
// +kubebuilder:validation:XValidation:rule="self.parametersRef == oldSelf.parametersRef",message="parametersRef is immutable"
type ExecutionWorkspacePoolSpec struct {
	// ProviderRef selects the cluster-scoped provider installation.
	ProviderRef ClusterObjectReference `json:"providerRef"`

	// ParametersRef points at namespaced adapter-owned pool parameters in this namespace.
	ParametersRef TypedObjectReference `json:"parametersRef"`

	// Capacity is the desired provider-managed capacity envelope.
	Capacity ExecutionWorkspacePoolCapacity `json:"capacity"`
}

// ExecutionWorkspacePoolStatus reports provider-independent pool counts.
type ExecutionWorkspacePoolStatus struct {
	// ObservedGeneration is the most recent generation observed by the adapter.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Available is immediately allocatable capacity.
	// +optional
	Available int32 `json:"available,omitempty"`

	// Allocated is capacity currently bound to concrete workspaces.
	// +optional
	Allocated int32 `json:"allocated,omitempty"`

	// Suspended is reusable capacity not consuming active compute.
	// +optional
	Suspended int32 `json:"suspended,omitempty"`

	// Total is all capacity represented by the pool.
	// +optional
	Total int32 `json:"total,omitempty"`

	// Conditions represent readiness, admission, draining, and capacity pressure.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=orka,shortName=ewpool
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.spec.providerRef.name`
// +kubebuilder:printcolumn:name="Available",type=integer,JSONPath=`.status.available`
// +kubebuilder:printcolumn:name="Allocated",type=integer,JSONPath=`.status.allocated`
// +kubebuilder:printcolumn:name="Total",type=integer,JSONPath=`.status.total`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ExecutionWorkspacePool represents provider-managed warm or reusable capacity.
type ExecutionWorkspacePool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ExecutionWorkspacePoolSpec `json:"spec"`
	// +optional
	Status ExecutionWorkspacePoolStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ExecutionWorkspacePoolList contains a list of ExecutionWorkspacePool objects.
type ExecutionWorkspacePoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ExecutionWorkspacePool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ExecutionWorkspacePool{}, &ExecutionWorkspacePoolList{})
}
