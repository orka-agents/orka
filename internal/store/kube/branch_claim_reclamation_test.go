package kube

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	controlstore "github.com/orka-agents/orka/internal/store"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestCreateBranchClaimRecoversLostCreateAcknowledgement(t *testing.T) {
	ctx := context.Background()
	kubeStore, kubeClient, fence := newTestStoreWithEpoch(t)
	kubeStore.client = &persistBranchClaimThenErrorClient{Client: kubeClient}
	claim, inserted, err := kubeStore.CreateBranchClaimWithResult(ctx, &controlstore.BranchClaim{
		RepositoryID: "github.com/orka/target", Ref: "refs/heads/lost-ack",
		OwnerKind: controlstore.BranchClaimOwnerTask, OwnerUID: "task-lost-ack",
		LastVerified: controlstore.RemoteRefState{Absent: true}, RequestDigest: testDigest("lost-ack"), CreatedAt: testNow,
	}, fence)
	if err != nil {
		t.Fatal(err)
	}
	if inserted {
		t.Fatal("acknowledgement-ambiguous create must not report a certain insertion")
	}
	if claim == nil || claim.OwnerUID != "task-lost-ack" {
		t.Fatalf("recovered claim = %#v", claim)
	}
}

type persistBranchClaimThenErrorClient struct {
	client.Client
	failed bool
}

func (c *persistBranchClaimThenErrorClient) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	if err := c.Client.Create(ctx, object, options...); err != nil {
		return err
	}
	if _, ok := object.(*corev1alpha1.BranchClaim); ok && !c.failed {
		c.failed = true
		return errors.New("simulated lost BranchClaim create acknowledgement")
	}
	return nil
}

func TestReclaimBranchClaimIsExactAndRecoverySafe(t *testing.T) {
	ctx := context.Background()
	kubeStore, _, fence := newTestStoreWithEpoch(t)
	claim, err := kubeStore.CreateBranchClaim(ctx, &controlstore.BranchClaim{
		RepositoryID: "github.com/orka/target", Ref: "refs/heads/shared",
		OwnerKind: controlstore.BranchClaimOwnerTask, OwnerUID: "task-old", LastVerified: controlstore.RemoteRefState{Absent: true},
		RequestDigest: testDigest("branch-old"), CreatedAt: testNow,
	}, fence)
	if err != nil {
		t.Fatalf("CreateBranchClaim: %v", err)
	}
	request := reclaimBranchClaimRequestForTest(claim, fence)
	staleVersion := request
	staleVersion.ExpectedVersion++
	if err := kubeStore.ReclaimBranchClaim(ctx, staleVersion); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("ReclaimBranchClaim(stale version) error = %v, want ErrConflict", err)
	}
	if err := kubeStore.ReclaimBranchClaim(ctx, request); err != nil {
		t.Fatalf("ReclaimBranchClaim: %v", err)
	}
	if _, err := kubeStore.GetBranchClaim(ctx, claim.ID); !errors.Is(err, controlstore.ErrNotFound) {
		t.Fatalf("GetBranchClaim after reclaim error = %v, want ErrNotFound", err)
	}
	if err := kubeStore.ReclaimBranchClaim(ctx, request); err != nil {
		t.Fatalf("ReclaimBranchClaim(idempotent retry): %v", err)
	}

	replacement, err := kubeStore.CreateBranchClaim(ctx, &controlstore.BranchClaim{
		RepositoryID: claim.RepositoryID, Ref: claim.Ref,
		OwnerKind: controlstore.BranchClaimOwnerTask, OwnerUID: "task-new", LastVerified: controlstore.RemoteRefState{SHA: strings.Repeat("a", 40)},
		RequestDigest: testDigest("branch-new"), CreatedAt: testNow,
	}, fence)
	if err != nil {
		t.Fatalf("CreateBranchClaim(replacement): %v", err)
	}
	if err := kubeStore.ReclaimBranchClaim(ctx, request); err != nil {
		t.Fatalf("ReclaimBranchClaim(old retry after replacement): %v", err)
	}
	preserved, err := kubeStore.GetBranchClaim(ctx, replacement.ID)
	if err != nil {
		t.Fatalf("GetBranchClaim(replacement): %v", err)
	}
	if preserved.OwnerUID != replacement.OwnerUID || preserved.RequestDigest != replacement.RequestDigest {
		t.Fatalf("replacement claim was changed: %#v", preserved)
	}
}

func TestReclaimBranchClaimReclaimsSessionAndPreservesBlockedClaims(t *testing.T) {
	ctx := context.Background()
	kubeStore, _, fence := newTestStoreWithEpoch(t)
	sessionClaim, err := kubeStore.CreateBranchClaim(ctx, &controlstore.BranchClaim{
		RepositoryID: "github.com/orka/target", Ref: "refs/heads/session",
		OwnerKind: controlstore.BranchClaimOwnerSession, OwnerUID: "session-uid", LastVerified: controlstore.RemoteRefState{SHA: strings.Repeat("a", 40)},
		RequestDigest: testDigest("session-claim"), CreatedAt: testNow,
	}, fence)
	if err != nil {
		t.Fatalf("CreateBranchClaim(session): %v", err)
	}
	sessionRequest := reclaimBranchClaimRequestForTest(sessionClaim, fence)
	sessionRequest.ExpectedOwnerKind = controlstore.BranchClaimOwnerSession
	if err := kubeStore.ReclaimBranchClaim(ctx, sessionRequest); err != nil {
		t.Fatalf("ReclaimBranchClaim(session): %v", err)
	}
	if _, err := kubeStore.GetBranchClaim(ctx, sessionClaim.ID); !errors.Is(err, controlstore.ErrNotFound) {
		t.Fatalf("GetBranchClaim(session) error = %v, want ErrNotFound", err)
	}

	taskClaim, err := kubeStore.CreateBranchClaim(ctx, &controlstore.BranchClaim{
		RepositoryID: "github.com/orka/target", Ref: "refs/heads/blocked",
		OwnerKind: controlstore.BranchClaimOwnerTask, OwnerUID: "task-blocked", LastVerified: controlstore.RemoteRefState{Absent: true},
		RequestDigest: testDigest("blocked-claim"), CreatedAt: testNow,
	}, fence)
	if err != nil {
		t.Fatalf("CreateBranchClaim(blocked): %v", err)
	}
	blocked, err := kubeStore.CompareAndSwapBranchClaim(ctx, controlstore.BranchClaimCAS{
		ID: taskClaim.ID, Fence: fence, ExpectedVersion: taskClaim.Version,
		ExpectedGeneration: taskClaim.Generation, NewGeneration: taskClaim.Generation,
		ExpectedLastVerified: taskClaim.LastVerified, NewLastVerified: taskClaim.LastVerified,
		ExpectedAvailability: taskClaim.Availability, NewAvailability: controlstore.BranchClaimReconciliationBlocked,
		BlockedReason: "manual reconciliation required", RelatedPublicationID: "publication-blocked",
		OperationID: "block-claim", OperationDigest: testDigest("block-claim"), UpdatedAt: testNow,
	})
	if err != nil {
		t.Fatalf("CompareAndSwapBranchClaim(block): %v", err)
	}
	blockedRequest := reclaimBranchClaimRequestForTest(blocked, fence)
	blockedRequest.ExpectedAvailability = controlstore.BranchClaimAvailable
	if err := kubeStore.ReclaimBranchClaim(ctx, blockedRequest); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("ReclaimBranchClaim(blocked) error = %v, want ErrConflict", err)
	}
	if _, err := kubeStore.GetBranchClaim(ctx, blocked.ID); err != nil {
		t.Fatalf("blocked claim was removed: %v", err)
	}
}

func reclaimBranchClaimRequestForTest(claim *controlstore.BranchClaim, fence controlstore.ControllerEpochFence) controlstore.ReclaimBranchClaimRequest {
	return controlstore.ReclaimBranchClaimRequest{
		ID: claim.ID, Fence: fence, ExpectedVersion: claim.Version, ExpectedGeneration: claim.Generation,
		ExpectedRepositoryID: claim.RepositoryID, ExpectedRef: claim.Ref,
		ExpectedOwnerKind: claim.OwnerKind, ExpectedOwnerUID: claim.OwnerUID,
		ExpectedLastVerified: claim.LastVerified, ExpectedAvailability: claim.Availability,
		ExpectedRequestDigest: claim.RequestDigest,
	}
}
