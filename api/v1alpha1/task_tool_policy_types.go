package v1alpha1

// TaskToolPolicyStatus reports the policy frozen for this Task attempt.
type TaskToolPolicyStatus struct {
	// Mode is legacy when the Agent did not select an explicit tool policy.
	// +kubebuilder:validation:Enum=legacy;full;restricted
	Mode string `json:"mode"`

	// Runner is the built-in runner family.
	// +kubebuilder:validation:MaxLength=64
	Runner string `json:"runner"`

	// RunnerVersion is the approved version built into the selected image.
	// +kubebuilder:validation:MaxLength=64
	RunnerVersion string `json:"runnerVersion"`

	// Digest binds this diagnostic to the immutable Session tool policy.
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	Digest string `json:"digest"`

	// Features separates runner features from explicitly authorized Orka tools.
	// +kubebuilder:validation:MaxItems=160
	Features []TaskToolFeatureStatus `json:"features"`
}

// TaskToolFeatureStatus distinguishes configuration from observed availability.
type TaskToolFeatureStatus struct {
	// Name identifies the feature or brokered tool.
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// Source distinguishes the runner from Orka's prompt-scoped tool broker.
	// +kubebuilder:validation:Enum=runner;orka
	Source string `json:"source"`

	// State is unverified when external setup or a real call has not been checked.
	// +kubebuilder:validation:Enum=ready;disabled;unavailable;unsupported;unverified
	State string `json:"state"`

	// Reason explains the policy decision, missing setup, or support limitation.
	// +kubebuilder:validation:MaxLength=2048
	Reason string `json:"reason"`
}
