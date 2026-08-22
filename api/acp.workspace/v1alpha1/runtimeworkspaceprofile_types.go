/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// SubstrateTemplateReference names the operator-owned Substrate infrastructure
// ActorTemplate a profile binds. The referenced template supplies WorkerPool
// placement, the runsc build, and snapshot storage; its containers never
// execute ACP work because the controller renders its own derived runtime
// template from it.
type SubstrateTemplateReference struct {
	// Name is the infrastructure ActorTemplate name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Namespace is the infrastructure ActorTemplate namespace. It defaults to
	// the profile namespace.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// SubstrateProfileSpec carries operator-owned Substrate infrastructure inputs.
type SubstrateProfileSpec struct {
	// TemplateRef names the operator-owned infrastructure ActorTemplate.
	TemplateRef SubstrateTemplateReference `json:"templateRef"`
}

// RuntimeWorkspaceProfileSpec is the namespaced class-parameters contract for
// the in-tree ACP RuntimePool execution-workspace adapter. A class whose
// provider backend is substrate must reference a profile that sets substrate;
// an agent-sandbox class profile must leave it empty because agent-sandbox
// RuntimeSessions run only controller-rendered sandbox templates.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="RuntimeWorkspaceProfile spec is immutable; create a new profile"
type RuntimeWorkspaceProfileSpec struct {
	// Substrate carries the operator-owned Substrate infrastructure inputs.
	// +optional
	Substrate *SubstrateProfileSpec `json:"substrate,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:categories=orka
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// RuntimeWorkspaceProfile is the namespaced adapter-owned workspace profile
// parameters kind referenced by ExecutionWorkspaceClass.spec.parametersRef for
// ACP RuntimePool execution workspaces.
type RuntimeWorkspaceProfile struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +optional
	Spec RuntimeWorkspaceProfileSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// RuntimeWorkspaceProfileList contains RuntimeWorkspaceProfile objects.
type RuntimeWorkspaceProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RuntimeWorkspaceProfile `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RuntimeWorkspaceProfile{}, &RuntimeWorkspaceProfileList{})
}
