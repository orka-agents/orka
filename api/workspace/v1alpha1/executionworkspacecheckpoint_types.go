/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// ExecutionWorkspaceCheckpointSpec exports a workspace's last completed Data
// checkpoint. Capture waits for an idle, suspended workspace. It never interrupts
// an attached Task or captures process memory.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="checkpoint source is immutable"
type ExecutionWorkspaceCheckpointSpec struct {
	// WorkspaceRef pins the source ExecutionWorkspace in this namespace.
	WorkspaceRef ObjectIdentityReference `json:"workspaceRef"`
	// RecoverLastCheckpoint explicitly accepts the last verified checkpoint of
	// a failed workspace. Later work may be missing. Recovery creates a new
	// workspace and never resumes or retries the uncertain source Task.
	// +optional
	RecoverLastCheckpoint bool `json:"recoverLastCheckpoint,omitempty"`
}

// ExecutionWorkspaceCheckpointStatus contains no native IDs, snapshot URLs, or credentials.
type ExecutionWorkspaceCheckpointStatus struct {
	// Phase records export or cleanup progress.
	// +kubebuilder:validation:Enum=Pending;Ready;Deleting;Failed
	// +optional
	Phase string `json:"phase,omitempty"`
	// Digest pins the immutable Data artifact and its provenance. Restore requests
	// must supply both this digest and the checkpoint object's UID.
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	// +optional
	Digest string `json:"digest,omitempty"`
	// ClassBinding identifies the only class revision permitted to restore this data.
	// +optional
	ClassBinding *ImmutableObjectBinding `json:"classBinding,omitempty"`
	// CreatedAt is the time the source Data checkpoint completed.
	// +optional
	CreatedAt *metav1.Time `json:"createdAt,omitempty"`
	// Conditions explain readiness and recovery failures without provider internals.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=orka,shortName=ewcp
// +kubebuilder:printcolumn:name="Workspace",type=string,JSONPath=`.spec.workspaceRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ExecutionWorkspaceCheckpoint retains a Data artifact independently of its
// source workspace. Deletion releases its reference after in-flight restores
// have acquired their own durable references.
type ExecutionWorkspaceCheckpoint struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ExecutionWorkspaceCheckpointSpec `json:"spec"`
	// +optional
	Status ExecutionWorkspaceCheckpointStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ExecutionWorkspaceCheckpointList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ExecutionWorkspaceCheckpoint `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ExecutionWorkspaceCheckpoint{}, &ExecutionWorkspaceCheckpointList{})
}
