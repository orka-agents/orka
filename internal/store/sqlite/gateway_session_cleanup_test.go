package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

func gatewayCleanupFixture(t *testing.T) (*Store, store.SessionCleanupIntent, store.GatewayEvent, store.SessionTurn) {
	t.Helper()
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-2 * time.Hour)
	event := testGatewayEvent(old, "cleanup")
	event.BindingUID = testGatewayBindingUID
	event.TaskUID = "cleanup-task-uid"
	if _, _, err := s.AdmitGatewayEvent(ctx, store.GatewayEventAdmission{Event: event, AppendUserMessage: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.BindSessionCleanupIdentity(ctx, event.Namespace, event.SessionName, "cleanup-session-uid"); err != nil {
		t.Fatal(err)
	}
	fence := store.ControllerEpochFence{Name: store.DefaultControllerEpochName, Epoch: 1, HolderID: "controller"}
	turn, err := s.CreateSessionTurnRecord(ctx, store.CreateSessionTurnRecordRequest{
		Namespace: event.Namespace, SessionName: event.SessionName, Fence: fence,
		Turn: store.SessionTurn{
			Key:             store.SessionTurnKey{SessionUID: "cleanup-session-uid", LeaseGeneration: 1, TaskUID: event.TaskUID, Attempt: 1, PromptID: "cleanup-prompt"},
			PromptAttemptID: "cleanup-attempt", RequestDigest: controlTestDigest("cleanup-turn"), UserPrompt: "a retained turn", CreatedAt: old,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"phase":"Succeeded"}`)
	turn, err = s.CommitSessionTurnFinalization(ctx, store.CommitSessionTurnFinalizationRequest{
		Key: turn.Key, Namespace: event.Namespace, SessionName: event.SessionName, Fence: fence,
		ExpectedTurnVersion: turn.Version, FinalizationDigest: controlTestDigest("cleanup-finalization"),
		TerminalKind: store.SessionTurnAssistantResult, TerminalContent: "a terminal answer", SkipTranscriptAppend: true,
		Projection: store.OutboxProjection{
			ID: store.CanonicalControlID("outbox", turn.ID, "TaskTerminalStatus"), AggregateKind: "SessionTurn", AggregateID: turn.ID,
			ProjectionKind: "TaskTerminalStatus", Payload: payload, PayloadDigest: store.CanonicalBytesDigest(payload),
		},
		FinalizedAt: old.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE outbox_projections SET state = ?, delivered_at = ?, delivery_digest = ? WHERE id = ?`,
		store.OutboxProjectionDelivered, old.Add(time.Minute), controlTestDigest("cleanup-delivery"), turn.ProjectionID); err != nil {
		t.Fatal(err)
	}
	attachDeliveredExpiryDelivery(t, s, ctx, event, old.Add(2*time.Minute))
	cutoff := now.Add(-time.Hour)
	operationID, operationDigest := store.GatewaySessionCleanupOperation(event.Namespace, event.SessionName, turn.Key.SessionUID, event.GatewayUID, event.BindingUID)
	intent := store.SessionCleanupIntent{
		Namespace: event.Namespace, SessionName: event.SessionName, SessionUID: turn.Key.SessionUID,
		OperationID: operationID, OperationDigest: operationDigest, PreparedAt: now,
		Gateway: &store.GatewaySessionCleanupProof{GatewayUID: event.GatewayUID, BindingUID: event.BindingUID, CreatedAt: old, TerminalCutoff: cutoff},
	}
	return s, intent, event, *turn
}

func compactGatewayCleanupFixture(t *testing.T, s *Store, intent store.SessionCleanupIntent) {
	t.Helper()
	result, err := s.MaintainGatewayRecords(context.Background(), intent.Namespace, intent.PreparedAt, intent.Gateway.TerminalCutoff)
	if err != nil {
		t.Fatalf("Gateway retention with stored ACP turns must not fail a foreign key: %v", err)
	}
	if result.DeletedEvents != 1 || result.UpsertedTombstones != 1 || result.DeletedSessions != 0 {
		t.Fatalf("retention should compact events while preserving ACP authority: %+v", result)
	}
}

func TestGatewayRetentionArchivesTurnsAfterEventCompaction(t *testing.T) {
	s, intent, event, turn := gatewayCleanupFixture(t)
	ctx := context.Background()
	compactGatewayCleanupFixture(t, s, intent)
	// A second maintenance pass has no event left to identify the Session.
	if _, err := s.MaintainGatewayRecords(ctx, intent.Namespace, intent.PreparedAt.Add(time.Minute), intent.Gateway.TerminalCutoff); err != nil {
		t.Fatal(err)
	}
	candidates, err := s.ListGatewaySessionCleanupCandidates(ctx, intent.Namespace, intent.Gateway.TerminalCutoff)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("candidates after event removal = %+v, %v", candidates, err)
	}
	candidate := candidates[0]
	if candidate.SessionUID != turn.Key.SessionUID || candidate.Proof.GatewayUID != event.GatewayUID ||
		candidate.Proof.BindingUID != event.BindingUID || !candidate.Proof.CreatedAt.Equal(intent.Gateway.CreatedAt) {
		t.Fatalf("candidate lost immutable ownership: %+v", candidate)
	}
	if other, err := s.ListGatewaySessionCleanupCandidates(ctx, "another-namespace", intent.Gateway.TerminalCutoff); err != nil || len(other) != 0 {
		t.Fatalf("candidate lookup crossed namespace: %+v, %v", other, err)
	}
	if _, err := s.PrepareSessionCleanup(ctx, intent); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteSessionCleanup(ctx, store.CompleteSessionCleanupRequest{
		Namespace: intent.Namespace, SessionName: intent.SessionName, OperationID: intent.OperationID, OperationDigest: intent.OperationDigest,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSession(ctx, intent.Namespace, intent.SessionName); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Session remains after archived cleanup: %v", err)
	}
	receipt, err := s.GetSessionTurnCleanupReceipt(ctx, intent.Namespace, intent.SessionName, turn.PromptAttemptID)
	if err != nil || receipt.Key != turn.Key || receipt.OperationDigest != intent.OperationDigest || receipt.ProjectionState != store.OutboxProjectionDelivered {
		t.Fatalf("archive receipt = %+v, %v", receipt, err)
	}
	duplicate, created, err := s.AdmitGatewayEvent(ctx, store.GatewayEventAdmission{Event: event, AppendUserMessage: true})
	if err != nil || created || duplicate.ID != event.ID || duplicate.SessionName != event.SessionName {
		t.Fatalf("late duplicate after cleanup = %+v, created=%v, %v", duplicate, created, err)
	}
}

func TestGatewayRetentionReservesTranscriptOnlySessionName(t *testing.T) {
	s := setupTestStore(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Second)
	old := now.Add(-2 * time.Hour)
	cutoff := now.Add(-time.Hour)
	event := testGatewayEvent(old, "transcript-only-cleanup")
	event.BindingUID = testGatewayBindingUID
	if _, _, err := s.AdmitGatewayEvent(ctx, store.GatewayEventAdmission{Event: event, AppendUserMessage: true}); err != nil {
		t.Fatal(err)
	}
	attachDeliveredExpiryDelivery(t, s, ctx, event, old.Add(time.Minute))
	if _, err := s.MaintainGatewayRecords(ctx, event.Namespace, now, cutoff); err != nil {
		t.Fatal(err)
	}
	operationID, operationDigest := store.GatewaySessionCleanupOperation(event.Namespace, event.SessionName, "", event.GatewayUID, event.BindingUID)
	// Maintenance also runs without an ACP coordinator. Its transcript-only
	// path must atomically record the completion before releasing the name.
	completion, err := s.GetSessionCleanupCompletion(ctx, event.Namespace, event.SessionName)
	if err != nil || completion.SessionUID != "" || completion.OperationID != operationID || completion.OperationDigest != operationDigest {
		t.Fatalf("transcript-only completion = %+v, %v", completion, err)
	}
	fresh := testGatewayEvent(now, "after-transcript-only-cleanup")
	fresh.BindingUID = event.BindingUID
	admitted, created, err := s.AdmitGatewayEvent(ctx, store.GatewayEventAdmission{Event: fresh, AppendUserMessage: true})
	if err != nil || !created || admitted.SessionName == event.SessionName {
		t.Fatalf("fresh event reused a retired transcript-only name: %+v, created=%v, %v", admitted, created, err)
	}
	if _, err := s.GetSession(ctx, event.Namespace, event.SessionName); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("retired transcript-only Session was recreated: %v", err)
	}
	duplicate, created, err := s.AdmitGatewayEvent(ctx, store.GatewayEventAdmission{Event: event, AppendUserMessage: true})
	if err != nil || created || duplicate.ID != event.ID || duplicate.SessionName != event.SessionName {
		t.Fatalf("old duplicate lost its retired Session identity: %+v, created=%v, %v", duplicate, created, err)
	}
}

func TestGatewayTranscriptRetentionRollsBackCompletionFailure(t *testing.T) {
	s := setupTestStore(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Second)
	old := now.Add(-2 * time.Hour)
	cutoff := now.Add(-time.Hour)
	event := testGatewayEvent(old, "transcript-completion-failure")
	event.BindingUID = testGatewayBindingUID
	if _, _, err := s.AdmitGatewayEvent(ctx, store.GatewayEventAdmission{Event: event, AppendUserMessage: true}); err != nil {
		t.Fatal(err)
	}
	attachDeliveredExpiryDelivery(t, s, ctx, event, old.Add(time.Minute))
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER fail_gateway_transcript_completion BEFORE INSERT ON session_cleanup_completions
		BEGIN SELECT RAISE(ABORT, 'injected completion failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MaintainGatewayRecords(ctx, event.Namespace, now, cutoff); err == nil {
		t.Fatal("maintenance succeeded despite a rejected cleanup completion")
	}
	if _, err := s.GetGatewayEvent(ctx, event.Namespace, event.ID); err != nil {
		t.Fatalf("failed completion discarded the event: %v", err)
	}
	if session, err := s.GetSession(ctx, event.Namespace, event.SessionName); err != nil || len(session.Messages) != 2 {
		t.Fatalf("failed completion discarded the transcript: %+v, %v", session, err)
	}
	if _, err := s.GetSessionCleanupIntent(ctx, event.Namespace, event.SessionName); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("atomic transcript cleanup left an intent after rollback: %v", err)
	}
	if _, err := s.GetSessionCleanupCompletion(ctx, event.Namespace, event.SessionName); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("failed cleanup published a completion: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `DROP TRIGGER fail_gateway_transcript_completion`); err != nil {
		t.Fatal(err)
	}
	if result, err := s.MaintainGatewayRecords(ctx, event.Namespace, now, cutoff); err != nil || result.DeletedSessions != 1 {
		t.Fatalf("transcript cleanup did not recover on the next maintenance pass: %+v, %v", result, err)
	}
	if _, err := s.GetSessionCleanupCompletion(ctx, event.Namespace, event.SessionName); err != nil {
		t.Fatalf("recovered transcript cleanup lacks its completion: %v", err)
	}
}

func TestGatewayCleanupRevalidatesRetentionAndIdentity(t *testing.T) {
	for _, atCompletion := range []bool{false, true} {
		for _, tc := range []struct {
			name  string
			query string
			args  func(store.SessionCleanupIntent, store.SessionTurn) []any
		}{
			{name: "active Task", query: `UPDATE sessions SET active_task = 'running-task'`},
			{name: "active Task UID", query: `UPDATE sessions SET active_task_uid = 'running-uid'`},
			{name: "chat reservation", query: `UPDATE sessions SET chat_turn_id = 'chat-turn'`},
			{name: "different Gateway", query: `UPDATE sessions SET owner_ref = 'replacement-gateway/binding-uid'`},
			{name: "different Binding", query: `UPDATE sessions SET owner_ref = 'gateway-uid/replacement-binding'`},
			{name: "different owner type", query: `UPDATE sessions SET owner_type = 'task'`},
			{name: "different Session type", query: `UPDATE sessions SET session_type = 'chat'`},
			{name: "replacement Session UID", query: `UPDATE sessions SET control_session_uid = 'replacement-session-uid'`},
			{name: "replacement creation time", query: `UPDATE sessions SET created_at = ?`, args: func(i store.SessionCleanupIntent, _ store.SessionTurn) []any {
				return []any{i.Gateway.CreatedAt.Add(time.Second)}
			}},
			{name: "retained Session", query: `UPDATE sessions SET updated_at = ?`, args: func(i store.SessionCleanupIntent, _ store.SessionTurn) []any { return []any{i.Gateway.TerminalCutoff} }},
			{name: "newer finalized turn", query: `UPDATE session_turns SET finalized_at = ?`, args: func(i store.SessionCleanupIntent, _ store.SessionTurn) []any { return []any{i.Gateway.TerminalCutoff} }},
			{name: "recently updated turn", query: `UPDATE session_turns SET updated_at = ?`, args: func(i store.SessionCleanupIntent, _ store.SessionTurn) []any { return []any{i.Gateway.TerminalCutoff} }},
			{name: "open turn", query: `UPDATE session_turns SET state = 'Open', finalized_at = NULL, terminal_kind = '', finalization_digest = ''`},
			{name: "unsettled projection", query: `UPDATE outbox_projections SET state = 'Pending', delivered_at = NULL, delivery_digest = ''`},
			{name: "retained canonical history", query: `INSERT INTO session_messages(namespace, session_name, role, content, created_at) VALUES ('default', 'gateway-session', 'user', 'retained', ?)`, args: func(i store.SessionCleanupIntent, _ store.SessionTurn) []any { return []any{i.Gateway.CreatedAt} }},
		} {
			stage := "prepare/"
			if atCompletion {
				stage = "complete/"
			}
			t.Run(stage+tc.name, func(t *testing.T) {
				s, intent, _, turn := gatewayCleanupFixture(t)
				ctx := context.Background()
				compactGatewayCleanupFixture(t, s, intent)
				if atCompletion {
					if _, err := s.PrepareSessionCleanup(ctx, intent); err != nil {
						t.Fatal(err)
					}
				}
				var args []any
				if tc.args != nil {
					args = tc.args(intent, turn)
				}
				if _, err := s.db.ExecContext(ctx, tc.query, args...); err != nil {
					t.Fatal(err)
				}
				var err error
				if atCompletion {
					err = s.CompleteSessionCleanup(ctx, store.CompleteSessionCleanupRequest{
						Namespace: intent.Namespace, SessionName: intent.SessionName, OperationID: intent.OperationID, OperationDigest: intent.OperationDigest,
					})
				} else {
					_, err = s.PrepareSessionCleanup(ctx, intent)
				}
				if !errors.Is(err, store.ErrConflict) {
					t.Fatalf("cleanup must preserve changed or retained Session: %v", err)
				}
				var turns, receipts, intents int
				if err := s.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM session_turns), (SELECT COUNT(*) FROM session_turn_cleanup_receipts), (SELECT COUNT(*) FROM session_cleanup_intents)`).Scan(&turns, &receipts, &intents); err != nil {
					t.Fatal(err)
				}
				wantIntents := 0
				if atCompletion {
					wantIntents = 1
				}
				if turns != 1 || receipts != 0 || intents != wantIntents {
					t.Fatalf("failed cleanup changed durable records: turns=%d receipts=%d intents=%d", turns, receipts, intents)
				}
			})
		}
	}
}

func TestGatewayCleanupKeepsPendingEventsAndReplies(t *testing.T) {
	for _, state := range []string{"Queued", "Pending", "RetryScheduled", "Sending"} {
		t.Run(state, func(t *testing.T) {
			s, intent, event, _ := gatewayCleanupFixture(t)
			ctx := context.Background()
			if state == "Queued" {
				if _, err := s.db.ExecContext(ctx, `UPDATE gateway_events SET state = 'Queued'`); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := s.db.ExecContext(ctx, `UPDATE gateway_deliveries SET state = ?, expires_at = ?`, state, intent.PreparedAt.Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
			}
			// Isolate the event/reply guard from history and timestamp guards.
			if _, err := s.db.ExecContext(ctx, `DELETE FROM session_messages`); err != nil {
				t.Fatal(err)
			}
			if state != "Queued" {
				if _, err := s.db.ExecContext(ctx, `DELETE FROM gateway_events WHERE id = ?`, event.ID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.PrepareSessionCleanup(ctx, intent); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("pending %s must prevent cleanup: %v", state, err)
			}
			candidates, err := s.ListGatewaySessionCleanupCandidates(ctx, event.Namespace, intent.Gateway.TerminalCutoff)
			if err != nil || len(candidates) != 0 {
				t.Fatalf("pending %s became a candidate: %+v, %v", state, candidates, err)
			}
		})
	}
}

func TestGatewayCleanupMixedAgeTurnsAndAtomicReceiptArchive(t *testing.T) {
	s, intent, event, first := gatewayCleanupFixture(t)
	ctx := context.Background()
	compactGatewayCleanupFixture(t, s, intent)
	fence := store.ControllerEpochFence{Name: store.DefaultControllerEpochName, Epoch: 1, HolderID: "controller"}
	key := first.Key
	key.LeaseGeneration++
	key.TaskUID = "newer-task-uid"
	key.PromptID = "newer-prompt"
	second, err := s.CreateSessionTurnRecord(ctx, store.CreateSessionTurnRecordRequest{
		Namespace: event.Namespace, SessionName: event.SessionName, Fence: fence,
		Turn: store.SessionTurn{Key: key, PromptAttemptID: "newer-attempt", RequestDigest: controlTestDigest("newer-turn"), UserPrompt: "newer question"},
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"phase":"Succeeded"}`)
	second, err = s.CommitSessionTurnFinalization(ctx, store.CommitSessionTurnFinalizationRequest{
		Key: key, Namespace: event.Namespace, SessionName: event.SessionName, Fence: fence,
		ExpectedTurnVersion: second.Version, FinalizationDigest: controlTestDigest("newer-finalization"),
		TerminalKind: store.SessionTurnAssistantResult, TerminalContent: "newer answer", SkipTranscriptAppend: true,
		Projection: store.OutboxProjection{
			ID: store.CanonicalControlID("outbox", second.ID, "TaskTerminalStatus"), AggregateKind: "SessionTurn", AggregateID: second.ID,
			ProjectionKind: "TaskTerminalStatus", Payload: payload, PayloadDigest: store.CanonicalBytesDigest(payload),
		},
		FinalizedAt: intent.Gateway.TerminalCutoff.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE outbox_projections SET state = ?, delivered_at = ?, delivery_digest = ? WHERE id = ?`,
		store.OutboxProjectionDelivered, *second.FinalizedAt, controlTestDigest("newer-delivery"), second.ProjectionID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PrepareSessionCleanup(ctx, intent); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("newer turn in a mixed-age Session must retain all turns: %v", err)
	}
	// Once both turns pass retention, an error on the second receipt must also
	// roll back the first receipt, leaving the original intent and both turns.
	intent.Gateway.TerminalCutoff = intent.PreparedAt.Add(-time.Minute)
	if _, err := s.PrepareSessionCleanup(ctx, intent); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER fail_second_gateway_receipt BEFORE INSERT ON session_turn_cleanup_receipts
		WHEN (SELECT COUNT(*) FROM session_turn_cleanup_receipts) > 0
		BEGIN SELECT RAISE(ABORT, 'second receipt storage failure'); END`); err != nil {
		t.Fatal(err)
	}
	request := store.CompleteSessionCleanupRequest{
		Namespace: intent.Namespace, SessionName: intent.SessionName, OperationID: intent.OperationID, OperationDigest: intent.OperationDigest,
	}
	if err := s.CompleteSessionCleanup(ctx, request); err == nil {
		t.Fatal("receipt storage fault did not abort cleanup")
	}
	var turns, receipts, intents int
	if err := s.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM session_turns), (SELECT COUNT(*) FROM session_turn_cleanup_receipts), (SELECT COUNT(*) FROM session_cleanup_intents)`).Scan(&turns, &receipts, &intents); err != nil {
		t.Fatal(err)
	}
	if turns != 2 || receipts != 0 || intents != 1 {
		t.Fatalf("receipt failure lost recovery data: turns=%d receipts=%d intents=%d", turns, receipts, intents)
	}
	if _, err := s.db.ExecContext(ctx, `DROP TRIGGER fail_second_gateway_receipt`); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteSessionCleanup(ctx, request); err != nil {
		t.Fatal(err)
	}
	for _, turn := range []store.SessionTurn{first, *second} {
		if _, err := s.GetSessionTurnCleanupReceipt(ctx, intent.Namespace, intent.SessionName, turn.PromptAttemptID); err != nil {
			t.Fatalf("completed cleanup lost turn receipt: %v", err)
		}
	}
}
