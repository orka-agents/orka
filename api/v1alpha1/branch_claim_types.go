/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// BranchClaimOwnerKind identifies the durable owner of an Orka-managed branch.
// +kubebuilder:validation:Enum=Task;Session
type BranchClaimOwnerKind string

// BranchClaimAvailability gates further branch mutation.
// +kubebuilder:validation:Enum=Available;ReconciliationBlocked
type BranchClaimAvailability string

// BranchClaimSpec is the immutable repository/ref ownership identity.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="branch claim spec is immutable"
type BranchClaimSpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	ID string `json:"id"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	RepositoryID string `json:"repositoryId"`

	// +kubebuilder:validation:Pattern=`^refs/heads/.+$`
	// +kubebuilder:validation:MaxLength=1024
	Ref string `json:"ref"`

	OwnerKind BranchClaimOwnerKind `json:"ownerKind"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	OwnerUID string `json:"ownerUid"`

	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	RequestDigest string `json:"requestDigest"`
}

// BranchClaimStatus is the exact generation, baseline, and availability CAS.
// +kubebuilder:validation:XValidation:rule="!has(self.availability) || self.availability != 'Available' || ((!has(self.blockedReason) || size(self.blockedReason) == 0) && (!has(self.relatedPublicationId) || size(self.relatedPublicationId) == 0))",message="available branch claims must clear block metadata"
// +kubebuilder:validation:XValidation:rule="!has(self.availability) || self.availability != 'ReconciliationBlocked' || (has(self.blockedReason) && size(self.blockedReason) > 0)",message="reconciliation-blocked branch claims require a reason"
type BranchClaimStatus struct {
	// +optional
	// +kubebuilder:validation:Minimum=1
	Generation int64 `json:"generation,omitempty"`

	// LastVerified is the independently observed exact target ref.
	// +optional
	LastVerified *ControlRemoteRefState `json:"lastVerified,omitempty"`

	// +optional
	Availability BranchClaimAvailability `json:"availability,omitempty"`

	// +optional
	// +kubebuilder:validation:MaxLength=16384
	BlockedReason string `json:"blockedReason,omitempty"`

	// +optional
	// +kubebuilder:validation:MaxLength=1024
	RelatedPublicationID string `json:"relatedPublicationId,omitempty"`

	ControlRecordMutationStatus `json:",inline"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=bclaim
// +kubebuilder:printcolumn:name="Repository",type=string,JSONPath=`.spec.repositoryId`
// +kubebuilder:printcolumn:name="Ref",type=string,JSONPath=`.spec.ref`
// +kubebuilder:printcolumn:name="Owner",type=string,JSONPath=`.spec.ownerKind`
// +kubebuilder:printcolumn:name="Generation",type=integer,JSONPath=`.status.generation`
// +kubebuilder:printcolumn:name="Availability",type=string,JSONPath=`.status.availability`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// BranchClaim is the cluster-wide Kubernetes-authoritative ownership and exact
// baseline record for one canonical repository branch.
type BranchClaim struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BranchClaimSpec   `json:"spec"`
	Status BranchClaimStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// BranchClaimList contains a list of BranchClaim.
type BranchClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BranchClaim `json:"items"`
}

func init() {
	SchemeBuilder.Register(&BranchClaim{}, &BranchClaimList{})
}
