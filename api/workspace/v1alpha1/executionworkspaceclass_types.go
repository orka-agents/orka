/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ExecutionWorkspaceClassSpec defines a user-selectable environment and policy.
// Direct provisioning requires both providerRef and parametersRef; pooled provisioning requires poolRef.
// +kubebuilder:validation:XValidation:rule="(has(self.providerRef) && has(self.parametersRef) && !has(self.poolRef)) || (!has(self.providerRef) && !has(self.parametersRef) && has(self.poolRef))",message="exactly one provisioning source is required: providerRef with parametersRef, or poolRef"
// +kubebuilder:validation:XValidation:rule="!has(self.poolRef) || self.poolRef.name.size() > 0",message="poolRef.name is required"
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="ExecutionWorkspaceClass functional spec is immutable; create a new class"
// +kubebuilder:validation:XValidation:rule="self.mode == 'Interactive' || self.allowedReuseScopes.all(scope, scope == 'None')",message="Service classes may only allow the None reuse scope"
type ExecutionWorkspaceClassSpec struct {
	// ProviderRef selects a cluster-scoped provider for direct provisioning.
	// +optional
	ProviderRef *ClusterObjectReference `json:"providerRef,omitempty"`

	// ParametersRef points at namespaced adapter-owned workspace profile parameters.
	// +optional
	ParametersRef *TypedObjectReference `json:"parametersRef,omitempty"`

	// PoolRef selects a pool in the same namespace.
	// +optional
	PoolRef *corev1.LocalObjectReference `json:"poolRef,omitempty"`

	// Mode selects interactive Task attachment or persistent Service hosting.
	// +kubebuilder:default=Interactive
	Mode ExecutionWorkspaceMode `json:"mode"`

	// RequiredFeatures must be a subset of the selected provider's advertised features.
	// +listType=set
	// +optional
	RequiredFeatures []ExecutionWorkspaceFeature `json:"requiredFeatures,omitempty"`

	// AllowedReuseScopes lists the reuse scopes Tasks may request.
	// +kubebuilder:validation:MinItems=1
	// +listType=set
	AllowedReuseScopes []WorkspaceReuseScope `json:"allowedReuseScopes"`

	// Lifecycle defines detach, retention, and deletion policy.
	Lifecycle ExecutionWorkspaceLifecycle `json:"lifecycle"`
}

// ExecutionWorkspaceClassStatus reports resolution and readiness without provider-native details.
type ExecutionWorkspaceClassStatus struct {
	// ObservedGeneration is the most recent generation observed by core.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ProviderRef is the provider resolved directly or through the selected pool.
	// +optional
	ProviderRef *ClusterObjectReference `json:"providerRef,omitempty"`

	// ProfileHash pins the first successfully resolved functional class profile.
	// Later referenced-object drift makes the class NotReady instead of changing this hash.
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	// +optional
	ProfileHash string `json:"profileHash,omitempty"`

	// Conditions represent reference, feature, and policy readiness.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=orka,shortName=ewc
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.status.providerRef.name`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ExecutionWorkspaceClass represents an immutable user-selectable workspace profile and policy.
type ExecutionWorkspaceClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ExecutionWorkspaceClassSpec `json:"spec"`
	// +optional
	Status ExecutionWorkspaceClassStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ExecutionWorkspaceClassList contains a list of ExecutionWorkspaceClass objects.
type ExecutionWorkspaceClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ExecutionWorkspaceClass `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ExecutionWorkspaceClass{}, &ExecutionWorkspaceClassList{})
}
