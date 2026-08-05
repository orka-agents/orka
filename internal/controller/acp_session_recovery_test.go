package controller

import (
	"context"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

func TestRecoverBlockedSessionResumesAfterBranchIntentCommitted(t *testing.T) {
	verifiedAt := time.Date(2026, 7, 25, 13, 0, 0, 0, time.UTC)
	receipt, err := NewACPIndependentBranchReceipt(
		"reconcile-session", "repo-id", "refs/heads/session", acpSessionTestSHA2, verifiedAt,
	)
	if err != nil {
		t.Fatal(err)
	}
	operationDigest, err := acpDomainDigest("session-reconciliation", map[string]any{
		"sessionUID": "session-uid", "publicationID": "publication-id",
		"branchClaimID": "branch-claim-id", "receiptDigest": receipt.Digest,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture := &blockedSessionRecoveryStore{
		control: store.SessionControl{
			Namespace: "default", SessionName: "session", SessionUID: "session-uid",
			Availability: store.SessionReconciliationBlocked, LeaseGeneration: 1,
			RelatedPublicationID: "publication-id", Version: 4,
		},
		publication: store.Publication{
			ID: "publication-id", State: store.PublicationOutcomeUnknown,
			BranchClaimID: "branch-claim-id", TargetRepositoryID: receipt.RepositoryID, TargetRef: receipt.Ref,
		},
		claim: store.BranchClaim{
			ID: "branch-claim-id", RepositoryID: receipt.RepositoryID, Ref: receipt.Ref,
			Generation: 2, Version: 8, LastVerified: store.RemoteRefState{SHA: receipt.SHA},
			Availability: store.BranchClaimAvailable, LastOperationID: receipt.OperationID,
			LastOperationDigest: operationDigest,
		},
	}
	continuity := &ACPSessionContinuity{controls: fixture, publications: fixture, branchClaims: fixture}
	fence := store.ControllerEpochFence{Name: store.DefaultControllerEpochName, Epoch: 3, HolderID: "controller-new"}
	recovered, err := continuity.RecoverBlockedSession(context.Background(), ACPRecoverBlockedSessionRequest{
		Namespace: "default", SessionName: "session", SessionUID: "session-uid", Fence: fence, Receipt: receipt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Availability != store.SessionAvailable || recovered.VerifiedBaseline == nil || recovered.VerifiedBaseline.SHA != receipt.SHA {
		t.Fatalf("recovered SessionControl = %#v", recovered)
	}
	if fixture.reconcile == nil || fixture.reconcile.OperationDigest != operationDigest ||
		fixture.reconcile.ExpectedBranchClaimVersion != fixture.claim.Version ||
		fixture.reconcile.ExpectedBranchBaseline != fixture.claim.LastVerified {
		t.Fatalf("reconciliation request did not preserve the committed intent: %#v", fixture.reconcile)
	}
}

type blockedSessionRecoveryStore struct {
	store.DurableControlStore
	control     store.SessionControl
	publication store.Publication
	claim       store.BranchClaim
	reconcile   *store.ReconcileSessionControlRequest
}

func (s *blockedSessionRecoveryStore) GetSessionControl(_ context.Context, namespace, sessionName string) (*store.SessionControl, error) {
	if namespace != s.control.Namespace || sessionName != s.control.SessionName {
		return nil, store.ErrNotFound
	}
	value := s.control
	return &value, nil
}

func (s *blockedSessionRecoveryStore) GetPublication(_ context.Context, id string) (*store.Publication, error) {
	if id != s.publication.ID {
		return nil, store.ErrNotFound
	}
	value := s.publication
	return &value, nil
}

func (s *blockedSessionRecoveryStore) GetBranchClaim(_ context.Context, id string) (*store.BranchClaim, error) {
	if id != s.claim.ID {
		return nil, store.ErrNotFound
	}
	value := s.claim
	return &value, nil
}

func (s *blockedSessionRecoveryStore) ReconcileSessionControl(_ context.Context, request store.ReconcileSessionControlRequest) (*store.SessionControl, error) {
	s.reconcile = &request
	value := s.control
	value.Availability = store.SessionAvailable
	value.BlockedReason = ""
	value.RelatedPublicationID = ""
	value.VerifiedBaseline = &request.VerifiedBaseline
	return &value, nil
}

func TestRecoverBlockedSessionFinishesCommittedStatusLeaseTail(t *testing.T) {
	verifiedAt := time.Date(2026, 7, 25, 14, 0, 0, 0, time.UTC)
	receipt, err := NewACPIndependentBranchReceipt("reconcile-tail", "repo-tail", "refs/heads/session", acpSessionTestSHA2, verifiedAt)
	if err != nil {
		t.Fatal(err)
	}
	digest := acpSessionTestDigest("reconcile-tail-digest")
	fixture := &blockedSessionRecoveryStore{
		control: store.SessionControl{
			Namespace: "default", SessionName: "session-tail", SessionUID: "session-tail-uid",
			Availability: store.SessionAvailable, LeaseGeneration: 2, Version: 5,
			VerifiedBaseline: &store.VerifiedBranchBaseline{RepositoryID: receipt.RepositoryID, Ref: receipt.Ref, SHA: receipt.SHA},
			LastOperationID:  receipt.OperationID, LastOperationDigest: digest,
		},
		claim: store.BranchClaim{
			ID: store.CanonicalControlID("branch-claim", receipt.RepositoryID, receipt.Ref), RepositoryID: receipt.RepositoryID, Ref: receipt.Ref,
			Generation: 1, Version: 4, LastVerified: store.RemoteRefState{SHA: receipt.SHA}, Availability: store.BranchClaimAvailable,
			LastOperationID: receipt.OperationID, LastOperationDigest: digest,
		},
	}
	continuity := &ACPSessionContinuity{controls: fixture, publications: fixture, branchClaims: fixture}
	if _, err := continuity.RecoverBlockedSession(context.Background(), ACPRecoverBlockedSessionRequest{
		Namespace: fixture.control.Namespace, SessionName: fixture.control.SessionName, SessionUID: fixture.control.SessionUID,
		Fence: store.ControllerEpochFence{Name: store.DefaultControllerEpochName, Epoch: 3, HolderID: "controller"}, Receipt: receipt,
	}); err != nil {
		t.Fatal(err)
	}
	if fixture.reconcile == nil || fixture.reconcile.ExpectedRelatedPublicationID != "" || fixture.reconcile.OperationDigest != digest {
		t.Fatalf("committed reconciliation tail request = %#v", fixture.reconcile)
	}
}
