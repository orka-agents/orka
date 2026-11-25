package publisher

import (
	"context"
	"time"
)

const (
	DigestPrefix      = "sha256:"
	CommitAuthorName  = "Orka Publisher"
	CommitAuthorEmail = "publisher@orka.ai"
)

type Repository struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
	URL      string `json:"url"`
}

type RemoteRef struct {
	Absent bool   `json:"absent"`
	OID    string `json:"oid,omitempty"`
}

func (r RemoteRef) Equal(other RemoteRef) bool {
	return r.Absent == other.Absent && r.OID == other.OID
}

type BranchClaim struct {
	RepositoryID string    `json:"repositoryId"`
	Ref          string    `json:"ref"`
	OwnerKind    string    `json:"ownerKind"`
	OwnerUID     string    `json:"ownerUid"`
	Generation   int64     `json:"generation"`
	LastVerified RemoteRef `json:"lastVerified"`
}

type PrepareRequest struct {
	PublicationID         string     `json:"publicationId"`
	PublicationGeneration int64      `json:"publicationGeneration"`
	OperationID           string     `json:"operationId"`
	Source                Repository `json:"source"`
	SourceRef             string     `json:"sourceRef"`
	Target                Repository `json:"target"`
	TargetRef             string     `json:"targetRef"`
	BranchClaimGeneration int64      `json:"branchClaimGeneration"`
	BaselineOID           string     `json:"baselineOid"`
	RemoteBefore          RemoteRef  `json:"remoteBefore"`
	DeltaArtifact         []byte     `json:"-"`
	DeltaArtifactDigest   string     `json:"deltaArtifactDigest"`
	RelativeRoot          string     `json:"relativeRoot,omitempty"`
	CommitAuthorName      string     `json:"commitAuthorName,omitempty"`
	CommitAuthorEmail     string     `json:"commitAuthorEmail,omitempty"`
	CommitMessage         string     `json:"commitMessage"`
	CommitTimestamp       time.Time  `json:"commitTimestamp"`
	SigningKey            []byte     `json:"-"`
	SigningKeyFingerprint string     `json:"signingKeyFingerprint,omitempty"`
}

type ReclaimRequest struct {
	PublicationID         string `json:"publicationId"`
	PublicationGeneration int64  `json:"publicationGeneration"`
}

type ReclaimResult struct {
	PublicationID         string `json:"publicationId"`
	PublicationGeneration int64  `json:"publicationGeneration"`
	Reclaimed             bool   `json:"reclaimed"`
}

type PreparedPublication struct {
	PublicationID         string     `json:"publicationId"`
	PublicationGeneration int64      `json:"publicationGeneration"`
	OperationID           string     `json:"operationId"`
	RequestDigest         string     `json:"requestDigest"`
	Source                Repository `json:"source"`
	SourceRef             string     `json:"sourceRef"`
	Target                Repository `json:"target"`
	TargetRef             string     `json:"targetRef"`
	BranchClaimGeneration int64      `json:"branchClaimGeneration"`
	BaselineOID           string     `json:"baselineOid"`
	RemoteBefore          RemoteRef  `json:"remoteBefore"`
	DeltaArtifactDigest   string     `json:"deltaArtifactDigest"`
	RelativeRoot          string     `json:"relativeRoot,omitempty"`
	ManifestDigest        string     `json:"manifestDigest"`
	TreeOID               string     `json:"treeOid"`
	CommitOID             string     `json:"commitOid"`
	BundleDigest          string     `json:"bundleDigest"`
	BundleSize            int64      `json:"bundleSize"`
	BundleRef             string     `json:"bundleRef"`
	BundlePath            string     `json:"bundlePath"`
	CommitMessage         string     `json:"commitMessage"`
	CommitTimestamp       time.Time  `json:"commitTimestamp"`
}

type PreflightRequest struct {
	Target Repository  `json:"target"`
	Claim  BranchClaim `json:"claim"`
}

type PreflightResult struct {
	Expected RemoteRef `json:"expected"`
	Observed RemoteRef `json:"observed"`
	Matches  bool      `json:"matches"`
}

type PublishRequest struct {
	PublicationID         string      `json:"publicationId"`
	PublicationGeneration int64       `json:"publicationGeneration"`
	OperationID           string      `json:"operationId"`
	Target                Repository  `json:"target"`
	TargetRef             string      `json:"targetRef"`
	Claim                 BranchClaim `json:"claim"`
	RemoteBefore          RemoteRef   `json:"remoteBefore"`
	ExpectedCommitOID     string      `json:"expectedCommitOid"`
	BundleDigest          string      `json:"bundleDigest"`
}

type PublishOutcome string

const (
	PublishAcknowledged   PublishOutcome = "Acknowledged"
	PublishAlreadyExact   PublishOutcome = "AlreadyExact"
	PublishCASRejected    PublishOutcome = "CASRejected"
	PublishOutcomeUnknown PublishOutcome = "PublicationOutcomeUnknown"
)

type PublishReceipt struct {
	PublicationID         string         `json:"publicationId"`
	PublicationGeneration int64          `json:"publicationGeneration"`
	OperationID           string         `json:"operationId"`
	RequestDigest         string         `json:"requestDigest"`
	Outcome               PublishOutcome `json:"outcome"`
	TargetRepositoryID    string         `json:"targetRepositoryId"`
	TargetRef             string         `json:"targetRef"`
	RemoteBefore          RemoteRef      `json:"remoteBefore"`
	ExpectedCommitOID     string         `json:"expectedCommitOid"`
	ObservedRemote        RemoteRef      `json:"observedRemote"`
}

type VerificationOutcome string

const (
	VerifiedExact             VerificationOutcome = "VerifiedExact"
	DeliveredSuperseded       VerificationOutcome = "DeliveredSuperseded"
	DeliveryConflict          VerificationOutcome = "DeliveryConflict"
	PublicationOutcomeUnknown VerificationOutcome = "PublicationOutcomeUnknown"
)

type VerifyRequest struct {
	PublicationID         string      `json:"publicationId"`
	PublicationGeneration int64       `json:"publicationGeneration"`
	OperationID           string      `json:"operationId"`
	Target                Repository  `json:"target"`
	TargetRef             string      `json:"targetRef"`
	Claim                 BranchClaim `json:"claim"`
	ExpectedCommitOID     string      `json:"expectedCommitOid"`
	BundleDigest          string      `json:"bundleDigest"`
}

type VerificationReceipt struct {
	PublicationID         string              `json:"publicationId"`
	PublicationGeneration int64               `json:"publicationGeneration"`
	OperationID           string              `json:"operationId"`
	RequestDigest         string              `json:"requestDigest"`
	Outcome               VerificationOutcome `json:"outcome"`
	TargetRepositoryID    string              `json:"targetRepositoryId"`
	TargetRef             string              `json:"targetRef"`
	ExpectedCommitOID     string              `json:"expectedCommitOid"`
	ObservedRemote        RemoteRef           `json:"observedRemote"`
	DescendantProofDigest string              `json:"descendantProofDigest,omitempty"`
}

type PullRequestIntent struct {
	BaseRepository        Repository `json:"baseRepository"`
	BaseRef               string     `json:"baseRef"`
	HeadRepository        Repository `json:"headRepository"`
	HeadRef               string     `json:"headRef"`
	PublicationGeneration int64      `json:"publicationGeneration"`
	ExpectedHeadOID       string     `json:"expectedHeadOid"`
	SessionUID            string     `json:"sessionUid,omitempty"`

	Title         string `json:"title,omitempty"`
	Body          string `json:"body,omitempty"`
	TaskName      string `json:"taskName,omitempty"`
	TaskNamespace string `json:"taskNamespace,omitempty"`
}

type PullRequestState string

const (
	PullRequestOpen   PullRequestState = "Open"
	PullRequestClosed PullRequestState = "Closed"
	PullRequestMerged PullRequestState = "Merged"
)

type PullRequestReceipt struct {
	IntentKey string           `json:"intentKey"`
	ForgeID   string           `json:"forgeId"`
	URL       string           `json:"url"`
	State     PullRequestState `json:"state"`
	HeadOID   string           `json:"headOid"`
}

type PullRequestReconciler interface {
	Reconcile(ctx context.Context, intent PullRequestIntent) (PullRequestReceipt, error)
}
