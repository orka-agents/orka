/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// RuntimeProviderBackend names an ACP RuntimePool execution-workspace backend.
// +kubebuilder:validation:Enum=agent-sandbox;substrate
type RuntimeProviderBackend string

const (
	// RuntimeProviderBackendAgentSandbox serves RuntimePool workloads through
	// kubernetes-sigs agent-sandbox SandboxClaims.
	RuntimeProviderBackendAgentSandbox RuntimeProviderBackend = "agent-sandbox"
	// RuntimeProviderBackendSubstrate serves RuntimePool workloads through
	// Agent Substrate Actors.
	RuntimeProviderBackendSubstrate RuntimeProviderBackend = "substrate"
)

// RuntimeProviderConfigSpec selects the backend one ExecutionWorkspaceProvider
// installation serves through the in-tree ACP RuntimePool adapter.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="RuntimeProviderConfig spec is immutable; create a new config"
type RuntimeProviderConfigSpec struct {
	// Backend selects the ACP RuntimePool execution-workspace backend.
	Backend RuntimeProviderBackend `json:"backend"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,categories=orka
// +kubebuilder:printcolumn:name="Backend",type=string,JSONPath=`.spec.backend`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// RuntimeProviderConfig is the cluster-scoped provider configuration for the
// in-tree ACP RuntimePool execution-workspace adapter. ExecutionWorkspaceProvider
// objects with the reserved ACP controllerName must reference exactly one
// RuntimeProviderConfig through spec.parametersRef.
type RuntimeProviderConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec RuntimeProviderConfigSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// RuntimeProviderConfigList contains RuntimeProviderConfig objects.
type RuntimeProviderConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RuntimeProviderConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RuntimeProviderConfig{}, &RuntimeProviderConfigList{})
}
