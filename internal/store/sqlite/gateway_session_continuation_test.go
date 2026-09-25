package sqlite

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store"
	"k8s.io/apimachinery/pkg/util/validation"
)

func TestGatewaySessionContinuationAcrossCleanupCycles(t *testing.T) {
	s := setupTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	base := gatewayContinuationEvent(now, "base")
	base.SessionName = strings.Repeat("a", 63) + "." + strings.Repeat("b", 63)
	physicalName := base.SessionName
	seen := map[string]bool{physicalName: true}
	var inputs, admitted []store.GatewayEvent
	var completions []store.SessionCleanupCompletion

	for cycle := 1; cycle <= 3; cycle++ {
		completion := gatewayContinuationCompletion(base, physicalName, fmt.Sprintf("session-uid-%d", cycle))
		seedGatewayContinuationCompletion(t, s, completion)
		completions = append(completions, completion)
		event := gatewayContinuationEvent(now.Add(time.Duration(cycle)*time.Second), fmt.Sprintf("cycle-%d", cycle))
		event.SessionName = base.SessionName
		got := admitGatewayContinuation(t, s, event, 100)
		if seen[got.SessionName] || len(validation.IsDNS1123Label(got.SessionName)) != 0 {
			t.Fatalf("successor name = %q, want a fresh DNS label", got.SessionName)
		}
		seen[got.SessionName] = true
		inputs = append(inputs, event)
		admitted = append(admitted, *got)
		physicalName = got.SessionName
		for i, input := range inputs {
			duplicate, created, err := s.AdmitGatewayEvent(t.Context(), store.GatewayEventAdmission{Event: input, AppendUserMessage: true})
			if err != nil || created || duplicate.SessionName != admitted[i].SessionName {
				t.Fatalf("cycle %d duplicate %d = (%+v, %v, %v)", cycle, i, duplicate, created, err)
			}
		}
		for _, completed := range completions {
			assertGatewayContinuationCompletion(t, s, completed)
			if _, err := s.GetSession(t.Context(), completed.Namespace, completed.SessionName); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("retired name %q regained a Session: %v", completed.SessionName, err)
			}
		}
	}
}

func TestGatewaySessionContinuationConcurrentAdmissions(t *testing.T) {
	s := setupTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	base := gatewayContinuationEvent(now, "base")
	seedGatewayContinuationCompletion(t, s, gatewayContinuationCompletion(base, base.SessionName, "retired-session-uid"))
	const count = 16
	type result struct {
		event   *store.GatewayEvent
		created bool
		err     error
	}
	results := make(chan result, count)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for i := range count {
		workers.Go(func() {
			<-start
			event := gatewayContinuationEvent(now, fmt.Sprintf("concurrent-%d", i))
			got, created, err := s.AdmitGatewayEvent(t.Context(), store.GatewayEventAdmission{
				Event: event, AppendUserMessage: true, PendingLimit: count,
			})
			results <- result{got, created, err}
		})
	}
	close(start)
	workers.Wait()
	close(results)
	var sessionName string
	orders := make(map[int64]bool, count)
	for result := range results {
		if result.err != nil || !result.created || result.event.State != store.GatewayEventQueued {
			t.Fatalf("concurrent admission = %+v", result)
		}
		if sessionName == "" {
			sessionName = result.event.SessionName
		}
		if result.event.SessionName != sessionName || sessionName == base.SessionName || orders[result.event.TranscriptOrder] {
			t.Fatalf("concurrent events did not converge with distinct transcript positions: %+v", result.event)
		}
		orders[result.event.TranscriptOrder] = true
	}
	session, err := s.GetSession(t.Context(), base.Namespace, sessionName)
	if err != nil || len(session.Messages) != count || session.MessageCount != count {
		t.Fatalf("concurrent transcript = (%+v, %v)", session, err)
	}
}

func TestGatewaySessionContinuationPendingCleanupIsRetryable(t *testing.T) {
	s := setupTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	first := gatewayContinuationEvent(now, "first")
	seedGatewayContinuationCompletion(t, s, gatewayContinuationCompletion(first, first.SessionName, "old-session-uid"))
	admitted := admitGatewayContinuation(t, s, first, 100)
	completion := gatewayContinuationCompletion(first, admitted.SessionName, "next-session-uid")
	seedGatewayContinuationIntent(t, s, completion, first)
	fresh := gatewayContinuationEvent(now, "retry")
	got, created, err := s.AdmitGatewayEvent(t.Context(), store.GatewayEventAdmission{Event: fresh, AppendUserMessage: true})
	if !errors.Is(err, store.ErrGatewaySessionCleanupPending) || created || got != nil {
		t.Fatalf("pending cleanup admission = (%+v, %v, %v)", got, created, err)
	}
	if _, err := s.GetGatewayEvent(t.Context(), fresh.Namespace, fresh.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("pending cleanup consumed the fresh event identity: %v", err)
	}
	duplicate, created, err := s.AdmitGatewayEvent(t.Context(), store.GatewayEventAdmission{Event: first, AppendUserMessage: true})
	if err != nil || created || duplicate.SessionName != admitted.SessionName {
		t.Fatalf("duplicate during cleanup = (%+v, %v, %v)", duplicate, created, err)
	}
	session, err := s.GetSession(t.Context(), first.Namespace, admitted.SessionName)
	if err != nil || len(session.Messages) != 1 {
		t.Fatalf("pending cleanup changed the transcript: (%+v, %v)", session, err)
	}
	rejected := gatewayContinuationEvent(now, "rejected")
	if got, created, err := s.AdmitGatewayEvent(t.Context(), store.GatewayEventAdmission{Event: rejected}); err != nil || !created || got.State != store.GatewayEventRejected {
		t.Fatalf("rejection-only admission = (%+v, %v, %v)", got, created, err)
	}
	if _, err := s.db.ExecContext(t.Context(), `DELETE FROM session_cleanup_intents WHERE namespace = ? AND session_name = ?`,
		completion.Namespace, completion.SessionName); err != nil {
		t.Fatal(err)
	}
	seedGatewayContinuationCompletion(t, s, completion)
	retried := admitGatewayContinuation(t, s, fresh, 100)
	if retried.SessionName == admitted.SessionName || retried.TranscriptOrder != admitted.TranscriptOrder {
		t.Fatalf("retry did not start the next incarnation: %+v", retried)
	}
}

func TestGatewaySessionContinuationRejectsUnrelatedCompletions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*store.SessionCleanupCompletion, store.GatewayEvent)
	}{
		{"generic operation", func(c *store.SessionCleanupCompletion, _ store.GatewayEvent) {
			c.OperationID, c.OperationDigest = "generic-session-cleanup", controlTestDigest("generic-session-cleanup")
		}},
		{"other Gateway", func(c *store.SessionCleanupCompletion, e store.GatewayEvent) {
			c.OperationID, c.OperationDigest = store.GatewaySessionCleanupOperation(c.Namespace, c.SessionName, c.SessionUID, "other-gateway", e.BindingUID)
		}},
		{"other Binding", func(c *store.SessionCleanupCompletion, e store.GatewayEvent) {
			c.OperationID, c.OperationDigest = store.GatewaySessionCleanupOperation(c.Namespace, c.SessionName, c.SessionUID, e.GatewayUID, "other-binding")
		}},
		{"changed Session UID", func(c *store.SessionCleanupCompletion, _ store.GatewayEvent) { c.SessionUID += "-changed" }},
		{"changed digest", func(c *store.SessionCleanupCompletion, _ store.GatewayEvent) {
			c.OperationDigest = controlTestDigest("changed")
		}},
		{"other physical name", func(c *store.SessionCleanupCompletion, e store.GatewayEvent) {
			c.OperationID, c.OperationDigest = store.GatewaySessionCleanupOperation(c.Namespace, "another-session", c.SessionUID, e.GatewayUID, e.BindingUID)
		}},
		{"other namespace", func(c *store.SessionCleanupCompletion, e store.GatewayEvent) {
			c.OperationID, c.OperationDigest = store.GatewaySessionCleanupOperation("other-namespace", c.SessionName, c.SessionUID, e.GatewayUID, e.BindingUID)
		}},
		{"reserved prefix alone", func(c *store.SessionCleanupCompletion, _ store.GatewayEvent) {
			c.OperationID = store.GatewaySessionCleanupOperationPrefix + strings.Repeat("0", 64)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := setupTestStore(t)
			event := gatewayContinuationEvent(time.Now().UTC().Truncate(time.Second), "fresh")
			completion := gatewayContinuationCompletion(event, event.SessionName, "old-session-uid")
			tc.change(&completion, event)
			seedGatewayContinuationCompletion(t, s, completion)
			got, created, err := s.AdmitGatewayEvent(t.Context(), store.GatewayEventAdmission{Event: event, AppendUserMessage: true})
			if !errors.Is(err, store.ErrConflict) || created || got != nil {
				t.Fatalf("unrelated completion admission = (%+v, %v, %v)", got, created, err)
			}
			assertGatewayContinuationCompletion(t, s, completion)
			if _, err := s.GetGatewayEvent(t.Context(), event.Namespace, event.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unrelated completion admitted an event: %v", err)
			}
		})
	}
}

func TestGatewaySessionContinuationDuplicateSources(t *testing.T) {
	for _, source := range []string{"event", "event with changed record ID", "canonical message", "tombstone"} {
		t.Run(source, func(t *testing.T) {
			s := setupTestStore(t)
			now := time.Now().UTC().Truncate(time.Second)
			input := gatewayContinuationEvent(now, "duplicate")
			seedGatewayContinuationCompletion(t, s, gatewayContinuationCompletion(input, input.SessionName, "old-session-uid"))
			admitted := admitGatewayContinuation(t, s, input, 100)
			completion := gatewayContinuationCompletion(input, admitted.SessionName, "next-session-uid")
			if source == "tombstone" {
				seedGatewayContinuationCompletion(t, s, completion)
				if _, err := s.db.ExecContext(t.Context(), `INSERT INTO gateway_event_tombstones
					(namespace, gateway_uid, external_event_id, event_id, envelope_digest, session_name, transcript_order, expires_at, created_at)
					VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
					input.Namespace, input.GatewayUID, input.ExternalEventID, input.ID, store.GatewayEventEnvelopeDigest(&input),
					admitted.SessionName, admitted.TranscriptOrder, now.Add(time.Hour), now); err != nil {
					t.Fatal(err)
				}
			} else {
				seedGatewayContinuationIntent(t, s, completion, input)
			}
			if source == "canonical message" || source == "tombstone" {
				if _, err := s.db.ExecContext(t.Context(), `DELETE FROM gateway_events WHERE namespace = ? AND id = ?`, input.Namespace, input.ID); err != nil {
					t.Fatal(err)
				}
			}
			if source == "tombstone" || source == "event with changed record ID" {
				input.ID += "-different-record-id"
			}
			duplicate, created, err := s.AdmitGatewayEvent(t.Context(), store.GatewayEventAdmission{Event: input, AppendUserMessage: true})
			if err != nil || created || duplicate.SessionName != admitted.SessionName || duplicate.TranscriptOrder != admitted.TranscriptOrder {
				t.Fatalf("duplicate from %s = (%+v, %v, %v)", source, duplicate, created, err)
			}
			input.Text += " changed"
			if _, _, err := s.AdmitGatewayEvent(t.Context(), store.GatewayEventAdmission{Event: input, AppendUserMessage: true}); !errors.Is(err, store.ErrDuplicateMismatch) {
				t.Fatalf("changed duplicate from %s error = %v", source, err)
			}
		})
	}
}

func TestGatewaySessionContinuationRejectsRecreatedRetiredName(t *testing.T) {
	s := setupTestStore(t)
	event := gatewayContinuationEvent(time.Now().UTC().Truncate(time.Second), "recreated")
	completion := gatewayContinuationCompletion(event, event.SessionName, "retired-session-uid")
	seedGatewayContinuationCompletion(t, s, completion)
	// Model a legacy writer that bypassed the generic creation fence.
	if _, err := s.db.ExecContext(t.Context(), `INSERT INTO sessions
		(namespace, name, session_type, owner_type, owner_ref) VALUES (?, ?, 'gateway', 'gateway', ?)`,
		event.Namespace, event.SessionName, event.GatewayUID+"/"+event.BindingUID); err != nil {
		t.Fatal(err)
	}
	got, created, err := s.AdmitGatewayEvent(t.Context(), store.GatewayEventAdmission{Event: event, AppendUserMessage: true})
	if !errors.Is(err, store.ErrConflict) || created || got != nil {
		t.Fatalf("recreated retired name admission = (%+v, %v, %v)", got, created, err)
	}
	assertGatewayContinuationCompletion(t, s, completion)
}

func TestGatewaySessionContinuationRespectsNamespace(t *testing.T) {
	s := setupTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	event := gatewayContinuationEvent(now, "other-namespace")
	other := event
	other.Namespace = "other-namespace"
	completion := gatewayContinuationCompletion(other, event.SessionName, "other-session-uid")
	seedGatewayContinuationCompletion(t, s, completion)
	admitted := admitGatewayContinuation(t, s, event, 100)
	if admitted.SessionName != event.SessionName {
		t.Fatalf("another namespace redirected admission: %+v", admitted)
	}
	assertGatewayContinuationCompletion(t, s, completion)
}

func TestGatewaySessionContinuationAppliesPendingLimitToSuccessor(t *testing.T) {
	s := setupTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	first := gatewayContinuationEvent(now, "queue-first")
	seedGatewayContinuationCompletion(t, s, gatewayContinuationCompletion(first, first.SessionName, "old-session-uid"))
	admitted := admitGatewayContinuation(t, s, first, 1)
	second := gatewayContinuationEvent(now, "queue-second")
	limited, created, err := s.AdmitGatewayEvent(t.Context(), store.GatewayEventAdmission{Event: second, AppendUserMessage: true, PendingLimit: 1})
	if err != nil || !created || limited.State != store.GatewayEventDeadLettered || limited.TaskName != "" || limited.SessionName != admitted.SessionName {
		t.Fatalf("successor pending limit = (%+v, %v, %v)", limited, created, err)
	}
	session, err := s.GetSession(t.Context(), first.Namespace, admitted.SessionName)
	if err != nil || len(session.Messages) != 1 {
		t.Fatalf("queue-limited transcript = (%+v, %v)", session, err)
	}
}

func TestGatewaySessionContinuationPreservesReservedCompletions(t *testing.T) {
	for _, chat := range []bool{false, true} {
		for _, sessionUID := range []string{"", "old-session-uid"} {
			t.Run(fmt.Sprintf("chat=%v/UID=%s", chat, sessionUID), func(t *testing.T) {
				s := setupTestStore(t)
				now := time.Now().UTC().Truncate(time.Second)
				event := gatewayContinuationEvent(now, "reserved")
				completion := gatewayContinuationCompletion(event, event.SessionName, sessionUID)
				seedGatewayContinuationCompletion(t, s, completion)
				record := &store.SessionRecord{Namespace: event.Namespace, Name: event.SessionName, SessionType: "chat", CreatedAt: now, UpdatedAt: now}
				var err error
				if chat {
					_, err = s.AcquireChatTurn(t.Context(), record, "new-chat-turn", now.Add(time.Hour))
				} else {
					err = s.CreateSession(t.Context(), record)
				}
				if !errors.Is(err, store.ErrConflict) {
					t.Fatalf("generic name reuse error = %v, want ErrConflict", err)
				}
				assertGatewayContinuationCompletion(t, s, completion)
				if got := admitGatewayContinuation(t, s, event, 100); got.SessionName == event.SessionName {
					t.Fatalf("reserved name was reused: %+v", got)
				}
			})
		}
		t.Run(fmt.Sprintf("generic transcript reuse/chat=%v", chat), func(t *testing.T) {
			s := setupTestStore(t)
			now := time.Now().UTC().Truncate(time.Second)
			event := gatewayContinuationEvent(now, "generic")
			completion := gatewayContinuationCompletion(event, event.SessionName, "")
			completion.OperationID = "ordinary-" + store.GatewaySessionCleanupOperationPrefix
			completion.OperationDigest = controlTestDigest(completion.OperationID)
			seedGatewayContinuationCompletion(t, s, completion)
			record := &store.SessionRecord{Namespace: event.Namespace, Name: event.SessionName, SessionType: "chat", CreatedAt: now, UpdatedAt: now}
			var err error
			if chat {
				_, err = s.AcquireChatTurn(t.Context(), record, "new-chat-turn", now.Add(time.Hour))
			} else {
				err = s.CreateSession(t.Context(), record)
			}
			if err != nil {
				t.Fatalf("generic transcript name reuse: %v", err)
			}
			if _, err := s.GetSessionCleanupCompletion(t.Context(), completion.Namespace, completion.SessionName); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("generic completion still exists after deliberate reuse: %v", err)
			}
		})
	}
}

func TestGatewaySessionContinuationPreservesTaskCleanupReceipt(t *testing.T) {
	s := setupTestStore(t)
	event := gatewayContinuationEvent(time.Now().UTC().Truncate(time.Second), "with-receipt")
	completion := gatewayContinuationCompletion(event, event.SessionName, "old-session-uid")
	seedGatewayContinuationCompletion(t, s, completion)
	receipt := seedGatewayContinuationReceipt(t, s, completion)
	before, err := s.GetSessionTurnCleanupReceipt(t.Context(), event.Namespace, event.SessionName, receipt.PromptAttemptID)
	if err != nil || !reflect.DeepEqual(before, &receipt) {
		t.Fatalf("initial archived receipt = (%+v, %v)", before, err)
	}
	admitted := admitGatewayContinuation(t, s, event, 100)
	if err := s.CompleteSessionCleanup(t.Context(), store.CompleteSessionCleanupRequest{
		Namespace: completion.Namespace, SessionName: completion.SessionName,
		OperationID: completion.OperationID, OperationDigest: completion.OperationDigest,
	}); err != nil {
		t.Fatalf("stale completed cleanup retry: %v", err)
	}
	after, err := s.GetSessionTurnCleanupReceipt(t.Context(), event.Namespace, event.SessionName, receipt.PromptAttemptID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("archived receipt after continuation = (%+v, %v)", after, err)
	}
	session, err := s.GetSession(t.Context(), event.Namespace, admitted.SessionName)
	if err != nil || len(session.Messages) != 1 {
		t.Fatalf("stale cleanup changed successor: (%+v, %v)", session, err)
	}
	assertGatewayContinuationCompletion(t, s, completion)
}

func gatewayContinuationEvent(now time.Time, suffix string) store.GatewayEvent {
	event := testGatewayEvent(now, suffix)
	event.BindingUID = testGatewayBindingUID
	return event
}

func gatewayContinuationCompletion(event store.GatewayEvent, physicalName, sessionUID string) store.SessionCleanupCompletion {
	operationID, operationDigest := store.GatewaySessionCleanupOperation(event.Namespace, physicalName, sessionUID, event.GatewayUID, event.BindingUID)
	return store.SessionCleanupCompletion{
		Namespace: event.Namespace, SessionName: physicalName, SessionUID: sessionUID,
		OperationID: operationID, OperationDigest: operationDigest, CompletedAt: time.Now().UTC().Truncate(time.Second),
	}
}

// These fixtures represent an already committed cleanup. Reclamation itself is
// tested separately; admission must preserve these old identities and receipts.
func seedGatewayContinuationCompletion(t *testing.T, s *Store, completion store.SessionCleanupCompletion) {
	t.Helper()
	tx, err := s.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck
	for _, query := range []string{
		`DELETE FROM session_messages WHERE namespace = ? AND session_name = ?`,
		`DELETE FROM sessions WHERE namespace = ? AND name = ?`,
	} {
		if _, err := tx.ExecContext(t.Context(), query, completion.Namespace, completion.SessionName); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.ExecContext(t.Context(), `UPDATE gateway_events SET state = ?, completed_at = ? WHERE namespace = ? AND session_name = ?`,
		store.GatewayEventCompleted, completion.CompletedAt, completion.Namespace, completion.SessionName); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(t.Context(), `INSERT INTO session_cleanup_completions
		(namespace, session_name, session_uid, operation_id, operation_digest, completed_at) VALUES (?, ?, ?, ?, ?, ?)`,
		completion.Namespace, completion.SessionName, completion.SessionUID, completion.OperationID, completion.OperationDigest, completion.CompletedAt); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func seedGatewayContinuationIntent(t *testing.T, s *Store, completion store.SessionCleanupCompletion, event store.GatewayEvent) {
	t.Helper()
	intent := store.SessionCleanupIntent{
		Namespace: completion.Namespace, SessionName: completion.SessionName, SessionUID: completion.SessionUID,
		OperationID: completion.OperationID, OperationDigest: completion.OperationDigest, PreparedAt: completion.CompletedAt,
		Gateway: &store.GatewaySessionCleanupProof{
			GatewayUID: event.GatewayUID, BindingUID: event.BindingUID, CreatedAt: event.CreatedAt,
			TerminalCutoff: completion.CompletedAt,
		},
	}
	encoded, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(t.Context(), `INSERT INTO session_cleanup_intents
		(namespace, session_name, operation_id, operation_digest, plan, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		intent.Namespace, intent.SessionName, intent.OperationID, intent.OperationDigest, encoded, intent.PreparedAt); err != nil {
		t.Fatal(err)
	}
}

func admitGatewayContinuation(t *testing.T, s *Store, event store.GatewayEvent, pendingLimit int) *store.GatewayEvent {
	t.Helper()
	got, created, err := s.AdmitGatewayEvent(t.Context(), store.GatewayEventAdmission{Event: event, AppendUserMessage: true, PendingLimit: pendingLimit})
	if err != nil || !created || got.State != store.GatewayEventQueued {
		t.Fatalf("AdmitGatewayEvent() = (%+v, %v, %v)", got, created, err)
	}
	return got
}

func assertGatewayContinuationCompletion(t *testing.T, s *Store, expected store.SessionCleanupCompletion) {
	t.Helper()
	got, err := s.GetSessionCleanupCompletion(t.Context(), expected.Namespace, expected.SessionName)
	if err != nil || !reflect.DeepEqual(got, &expected) {
		t.Fatalf("completion changed: got (%+v, %v), want %+v", got, err, expected)
	}
}

func seedGatewayContinuationReceipt(t *testing.T, s *Store, completion store.SessionCleanupCompletion) store.SessionTurnCleanupReceipt {
	t.Helper()
	key := store.SessionTurnKey{SessionUID: completion.SessionUID, LeaseGeneration: 1, TaskUID: "retired-task-uid", Attempt: 1, PromptID: "retired-prompt"}
	turnID, err := key.CanonicalID()
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"phase":"Succeeded"}`)
	receipt := store.SessionTurnCleanupReceipt{
		Namespace: completion.Namespace, SessionName: completion.SessionName,
		OperationID: completion.OperationID, OperationDigest: completion.OperationDigest,
		TurnID: turnID, Key: key, PromptAttemptID: "retired-attempt", TerminalKind: store.SessionTurnAssistantResult,
		FinalizedAt: completion.CompletedAt, ProjectionID: store.CanonicalControlID("outbox", turnID, "TaskTerminalStatus"),
		ProjectionKind: "TaskTerminalStatus", ProjectionDigest: store.CanonicalBytesDigest(payload), AggregateKind: "SessionTurn", AggregateID: turnID,
		Payload: payload, PayloadDigest: store.CanonicalBytesDigest(payload), ProjectionState: store.OutboxProjectionDelivered,
		DeliveryDigest: controlTestDigest("terminal-delivery"), DeliveredAt: &completion.CompletedAt,
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(t.Context(), `INSERT INTO session_turn_cleanup_receipts
		(turn_id, prompt_attempt_id, namespace, session_name, session_uid, receipt_digest, receipt) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		receipt.TurnID, receipt.PromptAttemptID, receipt.Namespace, receipt.SessionName, key.SessionUID, store.CanonicalBytesDigest(encoded), encoded); err != nil {
		t.Fatal(err)
	}
	return receipt
}
