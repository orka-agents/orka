/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// PublicationControlState is the clean-room publication state machine.
// +kubebuilder:validation:Enum=Preparing;Prepared;Publishing;Verifying;VerifiedExact;DeliveredSuperseded;CancelledBeforePublish;DeliveryConflict;CredentialBlocked;PreparationFailed;PublicationOutcomeUnknown
type PublicationControlState string

// PublicationSpec is the immutable clean-room publication identity and input.
// Mutable receipts and forge intent live only in status.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="publication spec is immutable"
type PublicationSpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	ID string `json:"id"`

	// +kubebuilder:validation:Minimum=1
	Generation int64 `json:"generation"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	TaskUID string `json:"taskUid"`

	// +kubebuilder:validation:Minimum=1
	Attempt int64 `json:"attempt"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	PromptID string `json:"promptId"`

	// +optional
	// +kubebuilder:validation:MaxLength=1024
	SessionUID string `json:"sessionUid,omitempty"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	BranchClaimID string `json:"branchClaimId"`

	// +kubebuilder:validation:Minimum=1
	BranchClaimGeneration int64 `json:"branchClaimGeneration"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	SourceRepositoryID string `json:"sourceRepositoryId"`

	// SourceRef is the exact immutable source ref or revision selector.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	SourceRef string `json:"sourceRef"`

	// +kubebuilder:validation:Pattern=`^([a-f0-9]{40}|[a-f0-9]{64})$`
	SourceBaselineSHA string `json:"sourceBaselineSha"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	TargetRepositoryID string `json:"targetRepositoryId"`

	// +kubebuilder:validation:Pattern=`^refs/heads/.+$`
	// +kubebuilder:validation:MaxLength=1024
	TargetRef string `json:"targetRef"`

	Baseline ControlRemoteRefState `json:"baseline"`

	// ArtifactID identifies the durable content-addressed change artifact.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	ArtifactID string `json:"artifactId"`

	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	ArtifactDigest string `json:"artifactDigest"`

	// +kubebuilder:validation:Minimum=1
	ArtifactSizeBytes int64 `json:"artifactSizeBytes"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	ArtifactMediaType string `json:"artifactMediaType"`

	// PublicationCredentialRef identifies an operation-scoped Secret reference;
	// it never contains credential material.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	PublicationCredentialRef string `json:"publicationCredentialRef"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	CommitIdentity string `json:"commitIdentity"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=16384
	CommitMessage string `json:"commitMessage"`

	CommitTimestamp metav1.Time `json:"commitTimestamp"`

	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	RequestDigest string `json:"requestDigest"`
}

// PublicationPullRequestIntent is the exact forge tuple persisted before the
// first forge API call.
type PublicationPullRequestIntent struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	BaseRepositoryID string `json:"baseRepositoryId"`

	// +kubebuilder:validation:Pattern=`^refs/heads/.+$`
	// +kubebuilder:validation:MaxLength=1024
	BaseRef string `json:"baseRef"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	HeadRepositoryID string `json:"headRepositoryId"`

	// +kubebuilder:validation:Pattern=`^refs/heads/.+$`
	// +kubebuilder:validation:MaxLength=1024
	HeadRef string `json:"headRef"`

	// +kubebuilder:validation:Minimum=1
	PublicationGeneration int64 `json:"publicationGeneration"`

	// +kubebuilder:validation:Pattern=`^([a-f0-9]{40}|[a-f0-9]{64})$`
	ExpectedHeadSHA string `json:"expectedHeadSha"`
}

// PreparedPublicationControlReceipt records deterministic commit preparation.
type PreparedPublicationControlReceipt struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	OperationID string `json:"operationId"`

	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	RequestDigest string `json:"requestDigest"`

	// +kubebuilder:validation:Pattern=`^([a-f0-9]{40}|[a-f0-9]{64})$`
	TreeSHA string `json:"treeSha"`

	// +kubebuilder:validation:Pattern=`^([a-f0-9]{40}|[a-f0-9]{64})$`
	CommitSHA string `json:"commitSha"`

	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	ManifestDigest string `json:"manifestDigest"`

	// RelativeRoot is the canonical repository-relative workspace root applied
	// to every path in the immutable delta artifact.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	RelativeRoot string `json:"relativeRoot,omitempty"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	BundleArtifactID string `json:"bundleArtifactId"`

	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	BundleDigest string `json:"bundleDigest"`

	// +kubebuilder:validation:Minimum=1
	BundleSizeBytes int64 `json:"bundleSizeBytes"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	BundleMediaType string `json:"bundleMediaType"`

	// +kubebuilder:validation:Pattern=`^refs/orka/publications/[a-f0-9]{64}$`
	BundleRef string `json:"bundleRef"`

	PreparedAt metav1.Time `json:"preparedAt"`
}

// PublishOperationControlReceipt records the exact server-enforced ref CAS.
type PublishOperationControlReceipt struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	OperationID string `json:"operationId"`

	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	RequestDigest string `json:"requestDigest"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	TargetRepositoryID string `json:"targetRepositoryId"`

	// +kubebuilder:validation:Pattern=`^refs/heads/.+$`
	// +kubebuilder:validation:MaxLength=1024
	TargetRef string `json:"targetRef"`

	RemoteBefore ControlRemoteRefState `json:"remoteBefore"`

	// +kubebuilder:validation:Pattern=`^([a-f0-9]{40}|[a-f0-9]{64})$`
	ExpectedCommitSHA string `json:"expectedCommitSha"`

	AcknowledgementUnknown bool `json:"acknowledgementUnknown"`

	PublishedAt metav1.Time `json:"publishedAt"`
}

// PublicationVerificationControlReceipt is an independent remote observation.
type PublicationVerificationControlReceipt struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	OperationID string `json:"operationId"`

	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	RequestDigest string `json:"requestDigest"`

	Outcome PublicationControlState `json:"outcome"`

	// +kubebuilder:validation:Pattern=`^([a-f0-9]{40}|[a-f0-9]{64})$`
	ExpectedCommitSHA string `json:"expectedCommitSha"`

	ObservedRemote ControlRemoteRefState `json:"observedRemote"`

	// +optional
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	DescendantProofDigest string `json:"descendantProofDigest,omitempty"`

	VerifiedAt metav1.Time `json:"verifiedAt"`
}

// PullRequestOperationControlReceipt snapshots exact forge reconciliation.
type PullRequestOperationControlReceipt struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	OperationID string `json:"operationId"`

	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	RequestDigest string `json:"requestDigest"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	IntentKey string `json:"intentKey"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	ForgeID string `json:"forgeId"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	URL string `json:"url"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	State string `json:"state"`

	// +kubebuilder:validation:Pattern=`^([a-f0-9]{40}|[a-f0-9]{64})$`
	HeadSHA string `json:"headSha"`

	ReconciledAt metav1.Time `json:"reconciledAt"`
}

// PublicationStatus contains mutable state, exact receipts, and epoch fencing.
// +kubebuilder:validation:XValidation:rule="!has(self.state) || !(self.state in ['DeliveryConflict', 'CredentialBlocked', 'PreparationFailed', 'PublicationOutcomeUnknown']) || (has(self.terminalReason) && size(self.terminalReason) > 0)",message="failure and unknown publication states require a terminal reason"
type PublicationStatus struct {
	// +optional
	State PublicationControlState `json:"state,omitempty"`

	// +optional
	PRIntent *PublicationPullRequestIntent `json:"prIntent,omitempty"`

	// +optional
	PreparedReceipt *PreparedPublicationControlReceipt `json:"preparedReceipt,omitempty"`

	// +optional
	PublishReceipt *PublishOperationControlReceipt `json:"publishReceipt,omitempty"`

	// +optional
	VerificationReceipt *PublicationVerificationControlReceipt `json:"verificationReceipt,omitempty"`

	// +optional
	PullRequestReceipt *PullRequestOperationControlReceipt `json:"pullRequestReceipt,omitempty"`

	// +optional
	// +kubebuilder:validation:MaxLength=16384
	TerminalReason string `json:"terminalReason,omitempty"`

	ControlRecordMutationStatus `json:",inline"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=pubctl
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
// +kubebuilder:printcolumn:name="Generation",type=integer,JSONPath=`.spec.generation`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.targetRef`
// +kubebuilder:printcolumn:name="Version",type=integer,JSONPath=`.status.version`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Publication is the Kubernetes-authoritative clean-room publication record.
type Publication struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PublicationSpec   `json:"spec"`
	Status PublicationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PublicationList contains a list of Publication.
type PublicationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Publication `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Publication{}, &PublicationList{})
}
