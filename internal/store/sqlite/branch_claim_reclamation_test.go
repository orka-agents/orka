package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

func TestReclaimBranchClaimIsExactAndRecoverySafe(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	fence := seedControlEpoch(t, s)
	claim, err := s.CreateBranchClaim(ctx, &store.BranchClaim{
		RepositoryID: "github.com/orka/target", Ref: "refs/heads/shared",
		OwnerKind: store.BranchClaimOwnerTask, OwnerUID: "task-old", LastVerified: store.RemoteRefState{Absent: true},
		RequestDigest: controlTestDigest("branch-old"), CreatedAt: time.Now().UTC(),
	}, fence)
	if err != nil {
		t.Fatalf("CreateBranchClaim: %v", err)
	}
	request := reclaimBranchClaimRequestForTest(claim, fence)
	staleBaseline := request
	staleBaseline.ExpectedLastVerified = store.RemoteRefState{SHA: controlTestGitSHA}
	if err := s.ReclaimBranchClaim(ctx, staleBaseline); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("ReclaimBranchClaim(stale baseline) error = %v, want ErrConflict", err)
	}
	if err := s.ReclaimBranchClaim(ctx, request); err != nil {
		t.Fatalf("ReclaimBranchClaim: %v", err)
	}
	if _, err := s.GetBranchClaim(ctx, claim.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetBranchClaim after reclaim error = %v, want ErrNotFound", err)
	}
	if err := s.ReclaimBranchClaim(ctx, request); err != nil {
		t.Fatalf("ReclaimBranchClaim(idempotent retry): %v", err)
	}

	replacement, err := s.CreateBranchClaim(ctx, &store.BranchClaim{
		RepositoryID: claim.RepositoryID, Ref: claim.Ref,
		OwnerKind: store.BranchClaimOwnerTask, OwnerUID: "task-new", LastVerified: store.RemoteRefState{SHA: controlTestGitSHA},
		RequestDigest: controlTestDigest("branch-new"), CreatedAt: time.Now().UTC(),
	}, fence)
	if err != nil {
		t.Fatalf("CreateBranchClaim(replacement): %v", err)
	}
	if err := s.ReclaimBranchClaim(ctx, request); err != nil {
		t.Fatalf("ReclaimBranchClaim(old retry after replacement): %v", err)
	}
	preserved, err := s.GetBranchClaim(ctx, replacement.ID)
	if err != nil {
		t.Fatalf("GetBranchClaim(replacement): %v", err)
	}
	if preserved.OwnerUID != replacement.OwnerUID || preserved.RequestDigest != replacement.RequestDigest {
		t.Fatalf("replacement claim was changed: %#v", preserved)
	}
}

func TestReclaimBranchClaimReclaimsSessionAndPreservesBlockedClaims(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	fence := seedControlEpoch(t, s)

	sessionClaim, err := s.CreateBranchClaim(ctx, &store.BranchClaim{
		RepositoryID: "github.com/orka/target", Ref: "refs/heads/session",
		OwnerKind: store.BranchClaimOwnerSession, OwnerUID: "session-uid", LastVerified: store.RemoteRefState{SHA: controlTestGitSHA},
		RequestDigest: controlTestDigest("session-claim"), CreatedAt: time.Now().UTC(),
	}, fence)
	if err != nil {
		t.Fatalf("CreateBranchClaim(session): %v", err)
	}
	sessionRequest := reclaimBranchClaimRequestForTest(sessionClaim, fence)
	sessionRequest.ExpectedOwnerKind = store.BranchClaimOwnerSession
	if err := s.ReclaimBranchClaim(ctx, sessionRequest); err != nil {
		t.Fatalf("ReclaimBranchClaim(session): %v", err)
	}
	if _, err := s.GetBranchClaim(ctx, sessionClaim.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetBranchClaim(session) error = %v, want ErrNotFound", err)
	}

	taskClaim, err := s.CreateBranchClaim(ctx, &store.BranchClaim{
		RepositoryID: "github.com/orka/target", Ref: "refs/heads/blocked",
		OwnerKind: store.BranchClaimOwnerTask, OwnerUID: "task-blocked", LastVerified: store.RemoteRefState{Absent: true},
		RequestDigest: controlTestDigest("blocked-claim"), CreatedAt: time.Now().UTC(),
	}, fence)
	if err != nil {
		t.Fatalf("CreateBranchClaim(blocked): %v", err)
	}
	blocked, err := s.CompareAndSwapBranchClaim(ctx, store.BranchClaimCAS{
		ID: taskClaim.ID, Fence: fence, ExpectedVersion: taskClaim.Version,
		ExpectedGeneration: taskClaim.Generation, NewGeneration: taskClaim.Generation,
		ExpectedLastVerified: taskClaim.LastVerified, NewLastVerified: taskClaim.LastVerified,
		ExpectedAvailability: taskClaim.Availability, NewAvailability: store.BranchClaimReconciliationBlocked,
		BlockedReason: "manual reconciliation required", RelatedPublicationID: "publication-blocked",
		OperationID: "block-claim", OperationDigest: controlTestDigest("block-claim"), UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("CompareAndSwapBranchClaim(block): %v", err)
	}
	blockedRequest := reclaimBranchClaimRequestForTest(blocked, fence)
	blockedRequest.ExpectedAvailability = store.BranchClaimAvailable
	if err := s.ReclaimBranchClaim(ctx, blockedRequest); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("ReclaimBranchClaim(blocked) error = %v, want ErrConflict", err)
	}
	if _, err := s.GetBranchClaim(ctx, blocked.ID); err != nil {
		t.Fatalf("blocked claim was removed: %v", err)
	}
}

func reclaimBranchClaimRequestForTest(claim *store.BranchClaim, fence store.ControllerEpochFence) store.ReclaimBranchClaimRequest {
	return store.ReclaimBranchClaimRequest{
		ID: claim.ID, Fence: fence, ExpectedVersion: claim.Version, ExpectedGeneration: claim.Generation,
		ExpectedRepositoryID: claim.RepositoryID, ExpectedRef: claim.Ref,
		ExpectedOwnerKind: claim.OwnerKind, ExpectedOwnerUID: claim.OwnerUID,
		ExpectedLastVerified: claim.LastVerified, ExpectedAvailability: claim.Availability,
		ExpectedRequestDigest: claim.RequestDigest,
	}
}
