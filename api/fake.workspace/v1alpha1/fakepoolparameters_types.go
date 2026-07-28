/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// FakePoolParametersSpec is intentionally empty. Pool capacity is expressed by
// the generic ExecutionWorkspacePool resource; this object provides an
// adapter-owned namespaced parameters identity for conformance.
type FakePoolParametersSpec struct{}

// +kubebuilder:object:root=true
// +kubebuilder:resource:categories=orka

// FakePoolParameters is the namespaced parameters identity used by the
// development-only fake workspace pool reconciler.
type FakePoolParameters struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +optional
	Spec FakePoolParametersSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// FakePoolParametersList contains FakePoolParameters objects.
type FakePoolParametersList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FakePoolParameters `json:"items"`
}

func init() {
	SchemeBuilder.Register(&FakePoolParameters{}, &FakePoolParametersList{})
}
