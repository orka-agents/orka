/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

// EnforcementNeed is a safe explanatory identifier, never provider prose or authority.
// +kubebuilder:validation:Enum=complete-policy;supported-format;configured-backend;supported-target;supported-runtime;mandatory-capability-coverage;provider-recovery;prepared-container;stopped-previous-container;operator-configuration;admission-recovery
type EnforcementNeed string

// EnforcementOutcome explains blocked admission for one observed Task generation.
// It is not enforcement proof or permission to release or retire a container.
type EnforcementOutcome struct {
	// +kubebuilder:validation:Minimum=1
	ObservedGeneration int64 `json:"observedGeneration"`
	// +kubebuilder:validation:Enum=EnforcementPending;EnforcementAdmissionPending;EnforcementRefused;EnforcementUncertain
	Reason string `json:"reason"`
	// +optional
	// +kubebuilder:validation:MaxItems=32
	// +listType=set
	Needs []EnforcementNeed `json:"needs,omitempty"`
}

// EnforcementAdmission is a historical controller admission observation, not
// live provider proof or authority to reuse a container or remove its controls.
type EnforcementAdmission struct {
	BindingID          string `json:"bindingID"`
	RequirementDigest  string `json:"requirementDigest"`
	ObservedGeneration int64  `json:"observedGeneration"`
}

// EnforcementRequirement is one complete caller-supplied requirement. It is
// Task input, not Agent configuration or operator-controlled provider routing.
type EnforcementRequirement struct {
	// Format identifies the provider-interpreted policy format.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	Format string `json:"format"`

	// Policy contains unchanged opaque bytes (base64 in JSON/YAML).
	// The decoded policy is additionally bounded to 512 KiB at admission.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=699052
	Policy []byte `json:"policy"`

	// RequiredCapabilities must all be covered by the mandatory providers.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=256
	// +listType=set
	RequiredCapabilities []string `json:"requiredCapabilities"`

	// Mode has no advisory fallback.
	// +kubebuilder:validation:Enum=enforce
	Mode string `json:"mode"`
}

// ContainerEnforcementBinding retains cleanup intent independently of the
// current Task attempt or a consuming runtime session. It contains no policy,
// private credential, kernel readback or provider-specific target handle.
type ContainerEnforcementBinding struct {
	BindingID         string `json:"bindingID"`
	RequirementDigest string `json:"requirementDigest"`
	ProviderSetDigest string `json:"providerSetDigest"`
	NodeName          string `json:"nodeName"`
	NodeUID           string `json:"nodeUID"`
	PodNamespace      string `json:"podNamespace"`
	PodName           string `json:"podName"`
	PodUID            string `json:"podUID"`
	ContainerName     string `json:"containerName"`
	ContainerID       string `json:"containerID"`
	// JobUID is present only for Job-owned native workers.
	// +optional
	JobUID string `json:"jobUID,omitempty"`
	// Admitted records the controller's durable release decision, not a live
	// provider assertion or permission to detach controls.
	// +optional
	Admitted bool `json:"admitted,omitempty"`
	// Published records observation of the exact Pod release annotation, not
	// worker consumption or successful execution. It survives Pod removal so
	// admission-event repair cannot lose its supporting receipt.
	// +optional
	Published bool `json:"published,omitempty"`
}
