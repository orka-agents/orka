/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// ExecutionWorkspaceProviderLifecycleState controls whether a provider accepts new allocations.
// +kubebuilder:validation:Enum=Active;Draining;Disabled
type ExecutionWorkspaceProviderLifecycleState string

const (
	ExecutionWorkspaceProviderActive   ExecutionWorkspaceProviderLifecycleState = "Active"
	ExecutionWorkspaceProviderDraining ExecutionWorkspaceProviderLifecycleState = "Draining"
	ExecutionWorkspaceProviderDisabled ExecutionWorkspaceProviderLifecycleState = "Disabled"
)

// ExecutionWorkspaceProviderUsagePolicy constrains namespaces allowed to use a provider.
type ExecutionWorkspaceProviderUsagePolicy struct {
	// AllowedNamespaceSelector selects namespaces that may resolve classes to this provider.
	// An empty selector matches all namespaces.
	// +optional
	AllowedNamespaceSelector *metav1.LabelSelector `json:"allowedNamespaceSelector,omitempty"`
}

// ExecutionWorkspaceProviderSpec defines a configured provider adapter installation.
// +kubebuilder:validation:XValidation:rule="self.controllerName == oldSelf.controllerName",message="controllerName is immutable"
// +kubebuilder:validation:XValidation:rule="self.parametersRef == oldSelf.parametersRef",message="parametersRef is immutable"
// +kubebuilder:validation:XValidation:rule="self.requiredContracts == oldSelf.requiredContracts",message="requiredContracts is immutable"
type ExecutionWorkspaceProviderSpec struct {
	// ControllerName is the globally unique adapter controller identity.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	ControllerName string `json:"controllerName"`

	// ParametersRef points at the adapter-owned provider configuration object.
	ParametersRef TypedObjectReference `json:"parametersRef"`

	// LifecycleState controls new allocations while preserving cleanup for existing workspaces.
	// +kubebuilder:default=Active
	LifecycleState ExecutionWorkspaceProviderLifecycleState `json:"lifecycleState"`

	// RequiredContracts lists contracts the adapter must advertise before this provider is usable.
	// +kubebuilder:validation:MinItems=1
	// +listType=set
	RequiredContracts []string `json:"requiredContracts"`

	// UsagePolicy constrains which namespaces may resolve classes to this provider.
	// +optional
	UsagePolicy *ExecutionWorkspaceProviderUsagePolicy `json:"usagePolicy,omitempty"`
}

// ExecutionWorkspaceAdapterStatus reports the adapter build serving a provider.
type ExecutionWorkspaceAdapterStatus struct {
	// Version is the adapter semantic version.
	// +optional
	Version string `json:"version,omitempty"`

	// Digest is the immutable adapter image or build digest.
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	// +optional
	Digest string `json:"digest,omitempty"`
}

// ExecutionWorkspaceBackendStatus reports safe backend compatibility metadata.
type ExecutionWorkspaceBackendStatus struct {
	// Version is the observed backend version.
	// +optional
	Version string `json:"version,omitempty"`

	// APIVersions lists provider-native API versions understood by the adapter.
	// +optional
	APIVersions []string `json:"apiVersions,omitempty"`
}

// ExecutionWorkspaceProviderStatus defines the observed provider state. The matching adapter is the
// sole writer of adapter/backend/features/heartbeat; Orka core owns only generic usability conditions.
type ExecutionWorkspaceProviderStatus struct {
	// ObservedGeneration is the most recent spec generation observed by the adapter.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Adapter reports adapter build identity.
	// +optional
	Adapter *ExecutionWorkspaceAdapterStatus `json:"adapter,omitempty"`

	// Backend reports safe provider-native compatibility metadata.
	// +optional
	Backend *ExecutionWorkspaceBackendStatus `json:"backend,omitempty"`

	// SupportedContracts lists generic control/data-plane contracts implemented by this adapter.
	// +listType=set
	// +optional
	SupportedContracts []string `json:"supportedContracts,omitempty"`

	// SupportedFeatures lists generic features implemented by this installation.
	// +optional
	SupportedFeatures []ExecutionWorkspaceFeature `json:"supportedFeatures,omitempty"`

	// LastHeartbeat is the last successful adapter heartbeat.
	// +optional
	LastHeartbeat *metav1.Time `json:"lastHeartbeat,omitempty"`

	// Conditions represent generic readiness and compatibility.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,categories=orka,shortName=ewp
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.spec.lifecycleState`
// +kubebuilder:printcolumn:name="Controller",type=string,JSONPath=`.spec.controllerName`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ExecutionWorkspaceProvider is a cluster-scoped provider adapter installation.
type ExecutionWorkspaceProvider struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ExecutionWorkspaceProviderSpec `json:"spec"`
	// +optional
	Status ExecutionWorkspaceProviderStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ExecutionWorkspaceProviderList contains a list of ExecutionWorkspaceProvider objects.
type ExecutionWorkspaceProviderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ExecutionWorkspaceProvider `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ExecutionWorkspaceProvider{}, &ExecutionWorkspaceProviderList{})
}
