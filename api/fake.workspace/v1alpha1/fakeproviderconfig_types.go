/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// FakeProviderConfigSpec is intentionally empty. The in-memory development
// provider needs only a concrete cluster-scoped identity for reference and
// REST-mapping conformance.
type FakeProviderConfigSpec struct{}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,categories=orka

// FakeProviderConfig is the cluster-scoped configuration identity used by the
// development-only fake workspace provider.
type FakeProviderConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +optional
	Spec FakeProviderConfigSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// FakeProviderConfigList contains FakeProviderConfig objects.
type FakeProviderConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FakeProviderConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&FakeProviderConfig{}, &FakeProviderConfigList{})
}
