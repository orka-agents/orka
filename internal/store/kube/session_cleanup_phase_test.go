package kube

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	controlstore "github.com/orka-agents/orka/internal/store"
)

func TestSessionCleanupPhasesAllowUnrelatedControlAndSerializeRetries(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, _, persistence, _, fence := newSessionCleanupTestStore(t, nil)
	control, _ := seedSessionCleanupState(t, ctx, s, persistence, fence, "phase-original")
	request := sessionCleanupPhaseRequest(control, fence)
	entered, release := make(chan struct{}), make(chan struct{})
	var closeOnce sync.Once
	defer closeOnce.Do(func() { close(release) })
	var calls atomic.Int32
	s.sessionRuntimeCleanup = func(callCtx context.Context, intent controlstore.SessionCleanupIntent, actual controlstore.ControllerEpochFence) error {
		if calls.Add(1) != 1 || actual != fence || intent.SessionUID != control.SessionUID {
			return errors.New("duplicate callback or changed original authority")
		}
		saved, err := persistence.GetSessionCleanupIntent(callCtx, intent.Namespace, intent.SessionName)
		if err != nil || saved.OperationID != request.OperationID || saved.OperationDigest != request.OperationDigest {
			return errors.New("runtime callback did not follow durable intent creation")
		}
		close(entered)
		select {
		case <-release:
			return nil
		case <-callCtx.Done():
			return callCtx.Err()
		}
	}
	first := make(chan error, 1)
	go func() { first <- s.ReclaimSession(ctx, request) }()
	select {
	case <-entered:
	case err := <-first:
		t.Fatalf("cleanup failed before runtime: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	waitCtx, stopWait := context.WithTimeout(ctx, 50*time.Millisecond)
	waitErr := s.ReclaimSession(waitCtx, request)
	stopWait()
	if !errors.Is(waitErr, context.DeadlineExceeded) {
		t.Errorf("canceled same-Session waiter = %v", waitErr)
	}
	second := make(chan error, 1)
	go func() { second <- s.ReclaimSession(ctx, request) }()
	other := &controlstore.SessionControl{
		Namespace: control.Namespace, SessionName: "phase-other", SessionUID: "phase-other-uid",
		RequestDigest: testDigest("phase-other"), LeaseGeneration: 1,
		Availability: controlstore.SessionAvailable, CreatedAt: testNow,
	}
	if err := persistence.CreateSession(ctx, &controlstore.SessionRecord{
		Namespace: other.Namespace, Name: other.SessionName, SessionType: "task", CreatedAt: testNow, UpdatedAt: testNow,
	}); err != nil {
		t.Fatal(err)
	}
	writeCtx, stopWrite := context.WithTimeout(ctx, time.Second)
	_, writeErr := s.CreateSessionControl(writeCtx, other, fence)
	stopWrite()
	if writeErr == nil {
		// The durable intent still fences new authority for the deleting Session.
		_, err := s.CreateBranchClaim(ctx, &controlstore.BranchClaim{
			RepositoryID: "github.com/orka/late", Ref: "refs/heads/late",
			OwnerKind: controlstore.BranchClaimOwnerSession, OwnerUID: control.SessionUID,
			LastVerified: controlstore.RemoteRefState{Absent: true}, RequestDigest: testDigest("phase-late"), CreatedAt: testNow,
		}, fence)
		if !errors.Is(err, controlstore.ErrConflict) {
			t.Errorf("new authority escaped the persisted cleanup intent: %v", err)
		}
	}
	closeOnce.Do(func() { close(release) })
	if err := <-first; err != nil {
		t.Errorf("original cleanup: %v", err)
	}
	if err := <-second; err != nil {
		t.Errorf("same-operation retry: %v", err)
	}
	if writeErr != nil {
		t.Fatalf("unrelated Session control blocked behind runtime I/O: %v", writeErr)
	}
	if err := s.ReclaimSession(ctx, request); err != nil || calls.Load() != 1 {
		t.Fatalf("completed cleanup replayed: calls=%d err=%v", calls.Load(), err)
	}
	otherOperation := request
	otherOperation.OperationID = "different-delete"
	otherOperation.OperationDigest = testDigest("different-delete")
	if err := s.ReclaimSession(ctx, otherOperation); !errors.Is(err, controlstore.ErrConflict) || calls.Load() != 1 {
		t.Fatalf("different cleanup operation was coalesced: calls=%d err=%v", calls.Load(), err)
	}
}

func TestSessionCleanupPhasesRejectTakeoverBeforeReclamation(t *testing.T) {
	ctx := context.Background()
	s, _, persistence, _, fence := newSessionCleanupTestStore(t, nil)
	control, claim := seedSessionCleanupState(t, ctx, s, persistence, fence, "phase-takeover")
	request := sessionCleanupPhaseRequest(control, fence)
	var nextFence controlstore.ControllerEpochFence
	var calls int
	s.sessionRuntimeCleanup = func(callCtx context.Context, _ controlstore.SessionCleanupIntent, _ controlstore.ControllerEpochFence) error {
		calls++
		current, err := s.GetControllerEpoch(callCtx, fence.Name)
		if err != nil {
			return err
		}
		next, err := s.CompareAndSwapControllerEpoch(callCtx, controlstore.ControllerEpochCAS{
			Name: current.Name, ExpectedVersion: current.Version, ExpectedEpoch: current.Epoch, NewEpoch: current.Epoch + 1,
			HolderID: "cleanup-successor", UpdatedAt: testNow.Add(time.Minute), RequestDigest: testDigest("cleanup-successor"),
		})
		if err == nil {
			nextFence = controlstore.ControllerEpochFence{Name: next.Name, Epoch: next.Epoch, HolderID: next.HolderID}
		}
		return err
	}
	if err := s.ReclaimSession(ctx, request); !errors.Is(err, controlstore.ErrConflict) || nextFence.Epoch != fence.Epoch+1 {
		t.Fatalf("takeover during runtime cleanup = %v, next epoch = %d", err, nextFence.Epoch)
	}
	assertSessionCleanupPhaseRetained(t, s, persistence, control, claim)
	if err := s.ReclaimSession(ctx, request); !errors.Is(err, controlstore.ErrConflict) || calls != 1 {
		t.Fatalf("stale retry reached runtime: calls=%d err=%v", calls, err)
	}
	s.sessionRuntimeCleanup = func(_ context.Context, _ controlstore.SessionCleanupIntent, actual controlstore.ControllerEpochFence) error {
		if actual != nextFence {
			return errors.New("recovery changed the successor fence")
		}
		return nil
	}
	request.Fence = nextFence
	if err := s.ReclaimSession(ctx, request); err != nil {
		t.Fatalf("current owner could not resume the original intent: %v", err)
	}
}

func TestSessionCleanupPhasesRejectChangedOrMissingIntent(t *testing.T) {
	for _, field := range []string{"missing", "operation", "lease", "claim", "prepared-at"} {
		t.Run(field, func(t *testing.T) {
			ctx := context.Background()
			s, _, persistence, db, fence := newSessionCleanupTestStore(t, nil)
			control, claim := seedSessionCleanupState(t, ctx, s, persistence, fence, "phase-intent")
			s.sessionRuntimeCleanup = func(callCtx context.Context, intent controlstore.SessionCleanupIntent, _ controlstore.ControllerEpochFence) error {
				if field == "missing" {
					_, err := db.ExecContext(callCtx, `DELETE FROM session_cleanup_intents WHERE namespace = ? AND session_name = ?`, intent.Namespace, intent.SessionName)
					return err
				}
				switch field {
				case "operation":
					intent.OperationDigest = testDigest("another-operation")
				case "lease":
					intent.ExpectedLeaseGeneration++
				case "claim":
					intent.BranchClaims[0].ObjectUID = "replacement-claim"
				case "prepared-at":
					intent.PreparedAt = intent.PreparedAt.Add(time.Second)
				}
				encoded, err := json.Marshal(intent)
				if err != nil {
					return err
				}
				_, err = db.ExecContext(callCtx, `UPDATE session_cleanup_intents SET plan = ? WHERE namespace = ? AND session_name = ?`, encoded, intent.Namespace, intent.SessionName)
				return err
			}
			err := s.ReclaimSession(ctx, sessionCleanupPhaseRequest(control, fence))
			if field == "missing" {
				if !errors.Is(err, controlstore.ErrNotFound) {
					t.Fatalf("missing intent accepted: %v", err)
				}
			} else if !errors.Is(err, controlstore.ErrConflict) {
				t.Fatalf("changed intent accepted: %v", err)
			}
			assertSessionCleanupPhaseRetained(t, s, persistence, control, claim)
		})
	}
}

func TestSessionCleanupPhasesDoNotReuseCallbackAliases(t *testing.T) {
	ctx := context.Background()
	s, _, persistence, _, fence := newSessionCleanupTestStore(t, nil)
	control, _ := seedSessionCleanupState(t, ctx, s, persistence, fence, "phase-alias")
	s.sessionRuntimeCleanup = func(_ context.Context, intent controlstore.SessionCleanupIntent, _ controlstore.ControllerEpochFence) error {
		intent.BranchClaims[0].ObjectUID = "callback-only-change"
		return nil
	}
	if err := s.ReclaimSession(ctx, sessionCleanupPhaseRequest(control, fence)); err != nil {
		t.Fatalf("callback alias altered the persisted reclamation plan: %v", err)
	}
}

func TestSessionCleanupPhasesRetainAuthorityOnFailureOrCancellation(t *testing.T) {
	for _, failure := range []string{"unproven", "canceled"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s, _, persistence, _, fence := newSessionCleanupTestStore(t, nil)
			control, claim := seedSessionCleanupState(t, ctx, s, persistence, fence, "phase-failure")
			s.sessionRuntimeCleanup = func(_ context.Context, _ controlstore.SessionCleanupIntent, _ controlstore.ControllerEpochFence) error {
				if failure == "canceled" {
					cancel()
					return nil
				}
				return controlstore.ErrNotReady
			}
			err := s.ReclaimSession(ctx, sessionCleanupPhaseRequest(control, fence))
			if (failure == "unproven" && !errors.Is(err, controlstore.ErrNotReady)) ||
				(failure == "canceled" && !errors.Is(err, context.Canceled)) {
				t.Fatalf("runtime failure or cancellation accepted: %v", err)
			}
			assertSessionCleanupPhaseRetained(t, s, persistence, control, claim)
			if _, err := persistence.GetSessionCleanupIntent(context.Background(), control.Namespace, control.SessionName); err != nil {
				t.Fatalf("failure discarded the original intent: %v", err)
			}
		})
	}
}

func sessionCleanupPhaseRequest(control *controlstore.SessionControl, fence controlstore.ControllerEpochFence) controlstore.ReclaimSessionRequest {
	return controlstore.ReclaimSessionRequest{
		Namespace: control.Namespace, SessionName: control.SessionName, Fence: fence,
		OperationID: "delete-" + control.SessionName, OperationDigest: testDigest("delete-" + control.SessionName), RequestedAt: testNow,
	}
}

func assertSessionCleanupPhaseRetained(t *testing.T, s *Store, persistence controlstore.SessionCleanupPersistenceStore, control *controlstore.SessionControl, claim *controlstore.BranchClaim) {
	t.Helper()
	ctx := context.Background()
	if _, err := persistence.GetSessionCleanupCompletion(ctx, control.Namespace, control.SessionName); !errors.Is(err, controlstore.ErrNotFound) {
		t.Errorf("failed cleanup has a completion: %v", err)
	}
	if _, err := s.GetSessionControl(ctx, control.Namespace, control.SessionName); err != nil {
		t.Errorf("failed cleanup lost control: %v", err)
	}
	if _, err := s.GetBranchClaim(ctx, claim.ID); err != nil {
		t.Errorf("failed cleanup lost its branch claim: %v", err)
	}
	transcripts := persistence.(interface {
		GetSession(context.Context, string, string) (*controlstore.SessionRecord, error)
	})
	if _, err := transcripts.GetSession(ctx, control.Namespace, control.SessionName); err != nil {
		t.Errorf("failed cleanup lost its transcript: %v", err)
	}
}
