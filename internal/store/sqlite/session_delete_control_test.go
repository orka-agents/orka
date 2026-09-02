package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

func TestDeleteSessionRejectsOpenDurableTurn(t *testing.T) {
	s, control, fence := seedSessionForDurableDeleteTest(t, store.SessionTurnOpen, "")

	if err := s.DeleteSession(context.Background(), control.Namespace, control.SessionName); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("DeleteSession() error = %v, want ErrConflict", err)
	}
	if _, err := s.GetSession(context.Background(), control.Namespace, control.SessionName); err != nil {
		t.Fatalf("GetSession() after rejected delete = %v", err)
	}
	turnID, err := (store.SessionTurnKey{
		SessionUID: control.SessionUID, LeaseGeneration: 1, TaskUID: "task-1", Attempt: 1, PromptID: "prompt-1",
	}).CanonicalID()
	if err != nil {
		t.Fatalf("CanonicalID() error = %v", err)
	}
	if _, err := s.GetSessionTurn(context.Background(), turnID); err != nil {
		t.Fatalf("GetSessionTurn() after rejected delete = %v", err)
	}
	_ = fence
}

func TestDeleteSessionRejectsUndeliveredTurnProjection(t *testing.T) {
	s, control, _ := seedSessionForDurableDeleteTest(t, store.SessionTurnFinalized, store.OutboxProjectionPending)

	if err := s.DeleteSession(context.Background(), control.Namespace, control.SessionName); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("DeleteSession() error = %v, want ErrConflict", err)
	}
	if _, err := s.GetOutboxProjection(context.Background(), "outbox-session-delete"); err != nil {
		t.Fatalf("GetOutboxProjection() after rejected delete = %v", err)
	}
}

func TestDeleteSessionRejectsActiveMutationLease(t *testing.T) {
	s, control, fence := seedSessionForDurableDeleteTest(t, "", "")
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	leased, err := s.AcquireSessionMutationLease(context.Background(), store.AcquireSessionMutationLeaseRequest{
		Namespace:               control.Namespace,
		SessionName:             control.SessionName,
		SessionUID:              control.SessionUID,
		Fence:                   fence,
		ExpectedVersion:         control.Version,
		ExpectedLeaseGeneration: control.LeaseGeneration,
		TaskUID:                 "task-lease",
		Attempt:                 1,
		PromptID:                "prompt-lease",
		RequestDigest:           controlTestDigest("session-delete-lease"),
		AcquiredAt:              now,
	})
	if err != nil {
		t.Fatalf("AcquireSessionMutationLease() error = %v", err)
	}
	if leased.Lease == nil {
		t.Fatal("AcquireSessionMutationLease() did not persist a lease")
	}

	if err := s.DeleteSession(context.Background(), control.Namespace, control.SessionName); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("DeleteSession() error = %v, want ErrConflict", err)
	}
}

func TestDeleteSessionRejectsPublicationBackedSessionClaim(t *testing.T) {
	s, control, _ := seedSessionForDurableDeleteTest(t, store.SessionTurnFinalized, store.OutboxProjectionDelivered)
	if _, err := s.db.ExecContext(context.Background(),
		`UPDATE session_turns SET publication_id = 'publication-session-delete' WHERE namespace = ? AND session_name = ?`,
		control.Namespace, control.SessionName,
	); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteSession(context.Background(), control.Namespace, control.SessionName); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("DeleteSession() error = %v, want ErrConflict", err)
	}
	if _, err := s.GetSession(context.Background(), control.Namespace, control.SessionName); err != nil {
		t.Fatalf("publication-backed session was deleted: %v", err)
	}
}

func TestDeleteSessionReclaimsSettledDurableTurnState(t *testing.T) {
	s, control, _ := seedSessionForDurableDeleteTest(t, store.SessionTurnFinalized, store.OutboxProjectionDelivered)
	ctx := context.Background()

	if err := s.DeleteSession(ctx, control.Namespace, control.SessionName); err != nil {
		t.Fatalf("DeleteSession() error = %v", err)
	}
	if _, err := s.GetSession(ctx, control.Namespace, control.SessionName); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetSession() error = %v, want ErrNotFound", err)
	}
	if _, err := s.GetSessionControl(ctx, control.Namespace, control.SessionName); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetSessionControl() error = %v, want ErrNotFound", err)
	}
	turnID, err := (store.SessionTurnKey{
		SessionUID: control.SessionUID, LeaseGeneration: 1, TaskUID: "task-1", Attempt: 1, PromptID: "prompt-1",
	}).CanonicalID()
	if err != nil {
		t.Fatalf("CanonicalID() error = %v", err)
	}
	if _, err := s.GetSessionTurn(ctx, turnID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetSessionTurn() error = %v, want ErrNotFound", err)
	}
	if _, err := s.GetOutboxProjection(ctx, "outbox-session-delete"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetOutboxProjection() error = %v, want ErrNotFound", err)
	}
}

func seedSessionForDurableDeleteTest(
	t *testing.T,
	turnState store.SessionTurnState,
	projectionState store.OutboxProjectionState,
) (*Store, *store.SessionControl, store.ControllerEpochFence) {
	t.Helper()
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	fence := seedControlEpoch(t, s)
	createControlTranscriptSession(t, s, "ns-delete", "session-delete", now)
	control, err := s.CreateSessionControl(ctx, &store.SessionControl{
		Namespace:     "ns-delete",
		SessionName:   "session-delete",
		SessionUID:    "session-delete-uid",
		RequestDigest: controlTestDigest("session-delete-control"),
		CreatedAt:     now,
	}, fence)
	if err != nil {
		t.Fatalf("CreateSessionControl() error = %v", err)
	}
	if turnState == "" {
		return s, control, fence
	}
	turnKey := store.SessionTurnKey{
		SessionUID: control.SessionUID, LeaseGeneration: 1, TaskUID: "task-1", Attempt: 1, PromptID: "prompt-1",
	}
	turnID, err := turnKey.CanonicalID()
	if err != nil {
		t.Fatalf("CanonicalID() error = %v", err)
	}
	if turnState == store.SessionTurnOpen {
		_, err = s.db.ExecContext(ctx, `INSERT INTO session_turns(
			id, namespace, session_name, session_uid, lease_generation, task_uid, attempt, prompt_id,
			prompt_attempt_id, request_digest, user_prompt, state, controller_epoch_name, controller_epoch,
			version, created_at, updated_at
		) VALUES (?, ?, ?, ?, 1, 'task-1', 1, 'prompt-1', 'attempt-1', ?, 'hello', 'Open', ?, ?, 1, ?, ?)`,
			turnID, control.Namespace, control.SessionName, control.SessionUID,
			controlTestDigest("session-delete-turn"), fence.Name, fence.Epoch, now, now,
		)
	} else {
		_, err = s.db.ExecContext(ctx, `INSERT INTO session_turns(
			id, namespace, session_name, session_uid, lease_generation, task_uid, attempt, prompt_id,
			prompt_attempt_id, request_digest, user_prompt, state, terminal_kind, terminal_content,
			finalization_digest, controller_epoch_name, controller_epoch, version, created_at, finalized_at, updated_at
		) VALUES (?, ?, ?, ?, 1, 'task-1', 1, 'prompt-1', 'attempt-1', ?, 'hello', 'Finalized',
			'AssistantResult', 'done', ?, ?, ?, 2, ?, ?, ?)`,
			turnID, control.Namespace, control.SessionName, control.SessionUID,
			controlTestDigest("session-delete-turn"), controlTestDigest("session-delete-finalization"),
			fence.Name, fence.Epoch, now, now.Add(time.Minute), now.Add(time.Minute),
		)
	}
	if err != nil {
		t.Fatalf("insert session turn: %v", err)
	}
	if projectionState == "" {
		return s, control, fence
	}
	var deliveredAt any
	if projectionState == store.OutboxProjectionDelivered {
		deliveredAt = now.Add(2 * time.Minute)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO outbox_projections(
		id, aggregate_kind, aggregate_id, projection_kind, payload_digest, payload, state,
		initial_available_at, available_at, controller_epoch_name, controller_epoch, version,
		created_at, updated_at, delivered_at
	) VALUES ('outbox-session-delete', ?, ?, 'TaskStatus', ?, '{}', ?, ?, ?, ?, ?, 1, ?, ?, ?)`,
		sessionTurnAggregateKind, turnID, controlTestDigest("session-delete-projection"), projectionState,
		now, now, fence.Name, fence.Epoch, now, now, deliveredAt,
	); err != nil {
		t.Fatalf("insert outbox projection: %v", err)
	}
	return s, control, fence
}
