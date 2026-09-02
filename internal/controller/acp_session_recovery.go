package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

// ACPIndependentBranchReceipt is the controller boundary for a fresh remote
// observation performed outside the runtime Pod. Digest covers the exact
// repository, ref, SHA, operation, and observation time.
type ACPIndependentBranchReceipt struct {
	OperationID  string
	RepositoryID string
	Ref          string
	SHA          string
	VerifiedAt   time.Time
	Digest       string
}

// NewACPIndependentBranchReceipt creates a canonical recovery receipt after an
// independent clean-room remote observation has established the actual branch.
func NewACPIndependentBranchReceipt(operationID, repositoryID, ref, sha string, verifiedAt time.Time) (ACPIndependentBranchReceipt, error) {
	receipt := ACPIndependentBranchReceipt{
		OperationID: strings.TrimSpace(operationID), RepositoryID: strings.TrimSpace(repositoryID),
		Ref: strings.TrimSpace(ref), SHA: strings.TrimSpace(sha), VerifiedAt: verifiedAt.UTC(),
	}
	if receipt.VerifiedAt.IsZero() {
		return ACPIndependentBranchReceipt{}, store.ValidationErrorf("independent branch verification time is required")
	}
	if err := receipt.validateIdentity(); err != nil {
		return ACPIndependentBranchReceipt{}, err
	}
	digest, err := acpDomainDigest("independent-branch-verification", map[string]any{
		"operationID": receipt.OperationID, "repositoryID": receipt.RepositoryID,
		"ref": receipt.Ref, "sha": receipt.SHA, "verifiedAt": receipt.VerifiedAt,
	})
	if err != nil {
		return ACPIndependentBranchReceipt{}, fmt.Errorf("digest independent branch receipt: %w", err)
	}
	receipt.Digest = digest
	return receipt, nil
}

func (r ACPIndependentBranchReceipt) validate() error {
	if err := r.validateIdentity(); err != nil {
		return err
	}
	if r.VerifiedAt.IsZero() {
		return store.ValidationErrorf("independent branch verification time is required")
	}
	expected, err := NewACPIndependentBranchReceipt(r.OperationID, r.RepositoryID, r.Ref, r.SHA, r.VerifiedAt)
	if err != nil {
		return err
	}
	if r.Digest != expected.Digest {
		return store.ValidationErrorf("independent branch verification digest does not match receipt")
	}
	return nil
}

func (r ACPIndependentBranchReceipt) validateIdentity() error {
	if err := store.ValidateControlIdentifier("branch verification operation ID", strings.TrimSpace(r.OperationID)); err != nil {
		return err
	}
	if err := store.ValidateControlIdentifier("verified repository ID", strings.TrimSpace(r.RepositoryID)); err != nil {
		return err
	}
	if err := store.ValidateFullBranchRef(strings.TrimSpace(r.Ref)); err != nil {
		return err
	}
	return store.ValidateGitObjectID("verified branch SHA", strings.TrimSpace(r.SHA))
}

// ACPRecoverBlockedSessionRequest supplies the immutable Session identity and a
// fresh independent branch receipt. Store versions and generations are loaded
// immediately before the cross-aggregate CAS reconciliation.
type ACPRecoverBlockedSessionRequest struct {
	Namespace   string
	SessionName string
	SessionUID  string
	Fence       store.ControllerEpochFence
	Receipt     ACPIndependentBranchReceipt
}

// RecoverBlockedSession establishes the actual remote baseline and atomically
// returns a PublicationOutcomeUnknown/DeliveryConflict Session and BranchClaim
// to Available. It cannot be used to bypass a missing independent receipt.
func (c *ACPSessionContinuity) RecoverBlockedSession(ctx context.Context, request ACPRecoverBlockedSessionRequest) (*store.SessionControl, error) {
	request.Namespace = strings.TrimSpace(request.Namespace)
	request.SessionName = strings.TrimSpace(request.SessionName)
	request.SessionUID = strings.TrimSpace(request.SessionUID)
	if err := request.Receipt.validate(); err != nil {
		return nil, err
	}
	control, err := c.controls.GetSessionControl(ctx, request.Namespace, request.SessionName)
	if err != nil {
		return nil, fmt.Errorf("load blocked ACP session: %w", err)
	}
	if control.SessionUID != request.SessionUID {
		return nil, fmt.Errorf("%w: blocked ACP session UID does not match immutable record", store.ErrConflict)
	}
	if committedSessionReconciliationMatchesReceipt(control, request.Receipt) {
		return c.resumeCommittedSessionReconciliation(ctx, request, control)
	}
	if control.Availability != store.SessionReconciliationBlocked || control.Lease != nil || control.RelatedPublicationID == "" {
		return nil, fmt.Errorf("%w: ACP session is not a lease-free reconciliation-blocked publication", store.ErrConflict)
	}
	publication, err := c.publications.GetPublication(ctx, control.RelatedPublicationID)
	if err != nil {
		return nil, fmt.Errorf("load blocked ACP publication: %w", err)
	}
	if publication.State != store.PublicationOutcomeUnknown && publication.State != store.PublicationDeliveryConflict {
		return nil, fmt.Errorf("%w: publication %q state %s is not reconciliation-blocked", store.ErrConflict, publication.ID, publication.State)
	}
	claim, err := c.branchClaims.GetBranchClaim(ctx, publication.BranchClaimID)
	if err != nil {
		return nil, fmt.Errorf("load blocked ACP branch claim: %w", err)
	}
	if request.Receipt.RepositoryID != publication.TargetRepositoryID || request.Receipt.RepositoryID != claim.RepositoryID ||
		request.Receipt.Ref != publication.TargetRef || request.Receipt.Ref != claim.Ref {
		return nil, fmt.Errorf("%w: independent branch receipt does not match the blocked publication target", store.ErrConflict)
	}
	operationDigest, err := acpDomainDigest("session-reconciliation", map[string]any{
		"sessionUID": control.SessionUID, "publicationID": publication.ID,
		"branchClaimID": claim.ID, "receiptDigest": request.Receipt.Digest,
	})
	if err != nil {
		return nil, fmt.Errorf("digest ACP session reconciliation: %w", err)
	}
	claimBlocked := claim.Availability == store.BranchClaimReconciliationBlocked && claim.RelatedPublicationID == publication.ID
	claimAlreadyAdvanced := claim.LastOperationID == request.Receipt.OperationID && claim.LastOperationDigest == operationDigest &&
		claim.Availability == store.BranchClaimAvailable && claim.RelatedPublicationID == "" && claim.BlockedReason == "" &&
		claim.LastVerified.Equal(store.RemoteRefState{SHA: request.Receipt.SHA})
	if !claimBlocked && !claimAlreadyAdvanced {
		return nil, fmt.Errorf("%w: branch claim %q is neither blocked by publication %q nor advanced by the same reconciliation intent", store.ErrConflict, claim.ID, publication.ID)
	}
	return c.controls.ReconcileSessionControl(ctx, store.ReconcileSessionControlRequest{
		Namespace: control.Namespace, SessionName: control.SessionName, SessionUID: control.SessionUID,
		Fence: request.Fence, ExpectedVersion: control.Version, ExpectedLeaseGeneration: control.LeaseGeneration,
		ExpectedRelatedPublicationID: publication.ID, BranchClaimID: claim.ID,
		ExpectedBranchClaimVersion: claim.Version, ExpectedBranchClaimGeneration: claim.Generation,
		ExpectedBranchBaseline: claim.LastVerified,
		VerifiedBaseline: store.VerifiedBranchBaseline{
			RepositoryID: request.Receipt.RepositoryID, Ref: request.Receipt.Ref, SHA: request.Receipt.SHA,
		},
		OperationID: request.Receipt.OperationID, OperationDigest: operationDigest,
		ReconciledAt: request.Receipt.VerifiedAt,
	})
}

func committedSessionReconciliationMatchesReceipt(control *store.SessionControl, receipt ACPIndependentBranchReceipt) bool {
	return control.Availability == store.SessionAvailable && control.Lease == nil && control.LastOperationID == receipt.OperationID &&
		control.VerifiedBaseline != nil && sameWorkspaceRepositoryIdentity(control.VerifiedBaseline.RepositoryID, receipt.RepositoryID) &&
		control.VerifiedBaseline.Ref == receipt.Ref && control.VerifiedBaseline.SHA == receipt.SHA
}

func (c *ACPSessionContinuity) resumeCommittedSessionReconciliation(ctx context.Context, request ACPRecoverBlockedSessionRequest, control *store.SessionControl) (*store.SessionControl, error) {
	claimID, err := store.CanonicalBranchClaimID(request.Receipt.RepositoryID, request.Receipt.Ref)
	if err != nil {
		return nil, err
	}
	claim, err := c.branchClaims.GetBranchClaim(ctx, claimID)
	if err != nil {
		return nil, fmt.Errorf("load committed ACP branch reconciliation: %w", err)
	}
	if claim.LastOperationID != request.Receipt.OperationID || claim.LastOperationDigest != control.LastOperationDigest ||
		claim.Availability != store.BranchClaimAvailable || claim.LastVerified.SHA != request.Receipt.SHA {
		return nil, fmt.Errorf("%w: committed ACP session reconciliation does not match its BranchClaim", store.ErrConflict)
	}
	return c.controls.ReconcileSessionControl(ctx, store.ReconcileSessionControlRequest{
		Namespace: control.Namespace, SessionName: control.SessionName, SessionUID: control.SessionUID,
		Fence: request.Fence, ExpectedVersion: control.Version, ExpectedLeaseGeneration: control.LeaseGeneration,
		BranchClaimID: claim.ID, ExpectedBranchClaimVersion: claim.Version,
		ExpectedBranchClaimGeneration: claim.Generation, ExpectedBranchBaseline: claim.LastVerified,
		VerifiedBaseline: *control.VerifiedBaseline, OperationID: request.Receipt.OperationID,
		OperationDigest: control.LastOperationDigest, ReconciledAt: request.Receipt.VerifiedAt,
	})
}
