package sqlite

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

func admitMessageTask(t *testing.T, s *Store, now time.Time) store.GatewayEvent {
	t.Helper()
	ctx := context.Background()
	event := testGatewayEvent(now, "messages")
	event.BindingUID = testGatewayBindingUID
	event.ThreadID = "thread"
	event.ReplyTarget = "reply"
	if _, _, err := s.AdmitGatewayEvent(ctx, store.GatewayEventAdmission{Event: event, AppendUserMessage: true, PendingLimit: 100}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextGatewayEvent(ctx, event.Namespace, "dispatcher", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkGatewayEventTaskCreated(ctx, event.Namespace, event.ID, event.TaskName, "task-uid", "dispatcher", now); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetGatewayEvent(ctx, event.Namespace, event.ID)
	if err != nil {
		t.Fatal(err)
	}
	return *got
}

func messageEnqueue(event store.GatewayEvent, now time.Time, requestID string) store.GatewayMessageEnqueue {
	return store.GatewayMessageEnqueue{
		Namespace: event.Namespace, NamespaceUID: event.NamespaceUID, EventID: event.ID,
		TaskName: event.TaskName, TaskUID: event.TaskUID, RequestID: requestID, Text: "working",
		MaxMessages: 10, MaxAttempts: 3, Now: now, ExpiresAt: event.ExpiresAt,
	}
}

func enqueueMessage(t *testing.T, s *Store, request store.GatewayMessageEnqueue) *store.GatewayDelivery {
	t.Helper()
	delivery, created, err := s.EnqueueGatewayMessage(context.Background(), request)
	if err != nil || !created {
		t.Fatalf("enqueue = (%+v, %v, %v)", delivery, created, err)
	}
	return delivery
}

func messageTerminal(event store.GatewayEvent, now time.Time, kind string) store.GatewayTerminalProjection {
	messageID := store.GatewayAssistantMessageID(event.ID)
	if kind != "final" {
		messageID = store.GatewayErrorMessageID(event.ID)
	}
	return store.GatewayTerminalProjection{
		EventID: event.ID, CompletedAt: now,
		Message: store.SessionMessage{ID: messageID, Role: "assistant", Content: "done"},
		Delivery: store.GatewayDelivery{
			ID: "aaa-terminal", IdempotencyID: "aaa-terminal", Namespace: event.Namespace,
			NamespaceUID: event.NamespaceUID, GatewayUID: event.GatewayUID, GatewayGeneration: event.GatewayGeneration,
			GatewayName: event.GatewayName, BindingName: event.BindingName, EventID: event.ID,
			TaskName: event.TaskName, SessionName: event.SessionName, Kind: kind,
			AccountID: event.AccountID, ContextID: event.ContextID, ThreadID: event.ThreadID, ReplyTarget: event.ReplyTarget,
			Text: "done", MaxAttempts: 3, ExpiresAt: now.Add(24 * time.Hour), CreatedAt: now, UpdatedAt: now,
		},
	}
}

func claimMessageDelivery(t *testing.T, s *Store, now time.Time, wantID string) *store.GatewayDelivery {
	t.Helper()
	got, err := s.ClaimNextGatewayDelivery(context.Background(), "default", "sender", now, time.Minute)
	if err != nil || got.ID != wantID {
		t.Fatalf("claim = (%+v, %v), want %s", got, err, wantID)
	}
	return got
}

func TestGatewayMessageAdmissionIsNonterminalAndDeduplicated(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	event := admitMessageTask(t, s, now)
	sessionBefore, err := s.GetSession(ctx, event.Namespace, event.SessionName)
	if err != nil {
		t.Fatal(err)
	}
	request := messageEnqueue(event, now, "operation-1")
	delivery := enqueueMessage(t, s, request)
	want := store.GatewayDelivery{
		ID: delivery.ID, IdempotencyID: delivery.ID, Namespace: "default", NamespaceUID: "namespace-uid",
		GatewayUID: "gateway-uid", GatewayGeneration: 1, GatewayName: "chat", BindingName: "room",
		EventID: "gev-messages", TaskName: "gateway-task-messages", SessionName: "gateway-session",
		Kind: "message", State: store.GatewayDeliveryPending, AccountID: "acct", ContextID: "context",
		ThreadID: "thread", ReplyTarget: "reply", Text: "working", MaxAttempts: 3,
		NextAttemptAt: now, ExpiresAt: now.Add(24 * time.Hour), CreatedAt: now, UpdatedAt: now,
	}
	if delivery.ID == "" || !reflect.DeepEqual(*delivery, want) {
		t.Fatalf("incorrect admitted delivery: %+v", delivery)
	}
	request.Now = now.Add(time.Second)
	request.ExpiresAt = now.Add(2 * time.Hour)
	request.MaxMessages = 1
	duplicate, created, err := s.EnqueueGatewayMessage(ctx, request)
	if err != nil || created || duplicate.ID != delivery.ID {
		t.Fatalf("replay = (%+v, %v, %v)", duplicate, created, err)
	}
	request.Text = "different"
	if _, _, err := s.EnqueueGatewayMessage(ctx, request); !errors.Is(err, store.ErrDuplicateMismatch) {
		t.Fatalf("changed replay = %v", err)
	}
	claimMessageDelivery(t, s, now, delivery.ID)
	if err := s.MarkGatewayDeliveryDelivered(ctx, event.Namespace, delivery.ID, "sender", "interim-provider", now); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkGatewayDeliveryDelivered(ctx, event.Namespace, delivery.ID, "sender", "interim-provider", now); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkGatewayDeliveryDelivered(ctx, event.Namespace, delivery.ID, "sender", "other-provider", now); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("changed receipt = %v", err)
	}
	stored, err := s.GetGatewayDelivery(ctx, event.Namespace, delivery.ID)
	if err != nil || stored.ProviderMessageID != "interim-provider" || stored.State != store.GatewayDeliveryDelivered {
		t.Fatalf("receipt = (%+v, %v)", stored, err)
	}
	after, err := s.GetGatewayEvent(ctx, event.Namespace, event.ID)
	if err != nil || !reflect.DeepEqual(&event, after) {
		t.Fatalf("message changed event: before=%+v after=%+v error=%v", event, after, err)
	}
	sessionAfter, err := s.GetSession(ctx, event.Namespace, event.SessionName)
	if err != nil || !reflect.DeepEqual(sessionBefore, sessionAfter) {
		t.Fatalf("message changed Session: before=%+v after=%+v error=%v", sessionBefore, sessionAfter, err)
	}
	var executionEvents int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM execution_events WHERE namespace = ? AND task_name = ?`, event.Namespace, event.TaskName).Scan(&executionEvents); err != nil {
		t.Fatal(err)
	}
	if executionEvents != 0 {
		t.Fatalf("interim receipt wrote %d Task completion events", executionEvents)
	}
}

func TestGatewayMessageReplayOnlyIsAtomicReceiptLookup(t *testing.T) {
	for _, state := range []store.GatewayDeliveryState{store.GatewayDeliveryPending, store.GatewayDeliveryDelivered, store.GatewayDeliveryDeadLettered} {
		t.Run(string(state), func(t *testing.T) {
			s := setupTestStore(t)
			ctx := t.Context()
			now := time.Now().UTC().Truncate(time.Second)
			event := admitMessageTask(t, s, now)
			request := messageEnqueue(event, now, "receipt")
			request.MaxMessages = 1
			seed := enqueueMessage(t, s, request)
			if state != store.GatewayDeliveryPending {
				claimMessageDelivery(t, s, now, seed.ID)
				var err error
				if state == store.GatewayDeliveryDelivered {
					err = s.MarkGatewayDeliveryDelivered(ctx, event.Namespace, seed.ID, "sender", "receipt", now)
				} else {
					err = s.MarkGatewayDeliveryTerminal(ctx, event.Namespace, seed.ID, "sender", state, "abandoned", now)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			before, err := s.GetGatewayDelivery(ctx, event.Namespace, seed.ID)
			if err != nil {
				t.Fatal(err)
			}
			// Receipt lookup precedes lifecycle and quota, but never event identity.
			if err := s.ReleaseLock(ctx, event.Namespace, event.SessionName, event.TaskName, event.TaskUID); err != nil {
				t.Fatal(err)
			}
			request.ReplayOnly = true
			request.Now = now.Add(25 * time.Hour)
			request.ExpiresAt = request.Now.Add(time.Hour)
			err = s.WithTaskDataTransaction(ctx, func(txCtx context.Context) error {
				replay, created, err := s.EnqueueGatewayMessage(txCtx, request)
				if err != nil || created || !reflect.DeepEqual(before, replay) {
					t.Fatalf("receipt = (%+v, %v, %v), want unchanged %+v", replay, created, err, before)
				}
				changed := request
				changed.Text = "different"
				if _, _, err := s.EnqueueGatewayMessage(txCtx, changed); !errors.Is(err, store.ErrDuplicateMismatch) {
					t.Fatalf("changed replay = %v", err)
				}
				changed = request
				changed.TaskUID = "replacement"
				if _, _, err := s.EnqueueGatewayMessage(txCtx, changed); !errors.Is(err, store.ErrConflict) {
					t.Fatalf("wrong identity = %v", err)
				}
				changed = request
				changed.EventID = "missing"
				if _, _, err := s.EnqueueGatewayMessage(txCtx, changed); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("missing event = %v", err)
				}
				changed = request
				changed.RequestID = "new"
				row, created, err := s.EnqueueGatewayMessage(txCtx, changed)
				if !errors.Is(err, store.ErrGatewayMessageReplayOnly) || created || row != nil {
					t.Fatalf("receipt miss = (%+v, %v, %v)", row, created, err)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			rows, err := s.ListGatewayDeliveries(ctx, store.GatewayDeliveryFilter{Namespace: event.Namespace})
			if err != nil || !reflect.DeepEqual(rows, []store.GatewayDelivery{*before}) {
				t.Fatalf("receipt lookup changed rows: %+v, %v", rows, err)
			}
		})
	}
}

func TestGatewayMessageReplayOnlyMissDoesNotConsumeQuota(t *testing.T) {
	s := setupTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	event := admitMessageTask(t, s, now)
	request := messageEnqueue(event, now, "new")
	request.MaxMessages = 1
	request.ReplayOnly = true
	if row, created, err := s.EnqueueGatewayMessage(t.Context(), request); !errors.Is(err, store.ErrGatewayMessageReplayOnly) || created || row != nil {
		t.Fatalf("receipt miss = (%+v, %v, %v)", row, created, err)
	}
	request.ReplayOnly = false
	enqueueMessage(t, s, request)
}

func TestGatewayMessageCapIsAtomicAndLifetime(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	event := admitMessageTask(t, s, now)
	var wg sync.WaitGroup
	results := make(chan error, 30)
	for i := range 30 {
		wg.Go(func() {
			_, _, err := s.EnqueueGatewayMessage(ctx, messageEnqueue(event, now, fmt.Sprintf("operation-%d", i)))
			results <- err
		})
	}
	wg.Wait()
	close(results)
	accepted, full := 0, 0
	for err := range results {
		switch {
		case err == nil:
			accepted++
		case errors.Is(err, store.ErrCapacity):
			full++
		default:
			t.Fatal(err)
		}
	}
	if accepted != 10 || full != 20 {
		t.Fatalf("accepted=%d full=%d", accepted, full)
	}
	for i := range 10 {
		delivery, err := s.ClaimNextGatewayDelivery(ctx, event.Namespace, "sender", now, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		switch i % 3 {
		case 0:
			err = s.MarkGatewayDeliveryDelivered(ctx, event.Namespace, delivery.ID, "sender", "receipt", now)
		case 1:
			err = s.MarkGatewayDeliveryTerminal(ctx, event.Namespace, delivery.ID, "sender", store.GatewayDeliveryFailed, "permanent", now)
		case 2:
			err = s.MarkGatewayDeliveryTerminal(ctx, event.Namespace, delivery.ID, "sender", store.GatewayDeliveryExpired, "expired", now)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.MaintainGatewayRecords(ctx, event.Namespace, now.Add(2*time.Hour), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.EnqueueGatewayMessage(ctx, messageEnqueue(event, now.Add(2*time.Hour), "after-retention")); !errors.Is(err, store.ErrCapacity) {
		t.Fatalf("lifetime cap = %v", err)
	}
	deliveries, err := s.ListGatewayDeliveries(ctx, store.GatewayDeliveryFilter{Namespace: event.Namespace, EventID: event.ID})
	if err != nil || len(deliveries) != 10 {
		t.Fatalf("retained messages = %d, %v", len(deliveries), err)
	}
}

func TestGatewayMessageParallelReplayConsumesOneSlot(t *testing.T) {
	s := setupTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	event := admitMessageTask(t, s, now)
	request := messageEnqueue(event, now, "same")
	request.MaxMessages = 1
	var wg sync.WaitGroup
	results := make(chan *store.GatewayDelivery, 20)
	for range 20 {
		wg.Go(func() {
			delivery, _, err := s.EnqueueGatewayMessage(context.Background(), request)
			if err != nil {
				t.Error(err)
				return
			}
			results <- delivery
		})
	}
	wg.Wait()
	close(results)
	var id string
	for delivery := range results {
		if id != "" && id != delivery.ID {
			t.Fatalf("duplicate IDs: %s and %s", id, delivery.ID)
		}
		id = delivery.ID
	}
	request.RequestID = "next"
	if _, _, err := s.EnqueueGatewayMessage(context.Background(), request); !errors.Is(err, store.ErrCapacity) {
		t.Fatalf("cap = %v", err)
	}
}

func TestGatewayMessageRejectsInvalidAdmission(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*store.GatewayMessageEnqueue)
		want   error
	}{
		{"Task replacement", func(r *store.GatewayMessageEnqueue) { r.TaskUID = "replacement" }, store.ErrConflict},
		{"Task name", func(r *store.GatewayMessageEnqueue) { r.TaskName = "other" }, store.ErrConflict},
		{"namespace replacement", func(r *store.GatewayMessageEnqueue) { r.NamespaceUID = "replacement" }, store.ErrConflict},
		{"wrong event", func(r *store.GatewayMessageEnqueue) { r.EventID = "other" }, store.ErrNotFound},
		{"wrong namespace", func(r *store.GatewayMessageEnqueue) { r.Namespace = "other" }, store.ErrNotFound},
		{"empty identity", func(r *store.GatewayMessageEnqueue) { r.RequestID = "" }, store.ErrValidation},
		{"large identity", func(r *store.GatewayMessageEnqueue) { r.RequestID = strings.Repeat("x", 257) }, store.ErrValidation},
		{"zero cap", func(r *store.GatewayMessageEnqueue) { r.MaxMessages = 0 }, store.ErrValidation},
		{"negative cap", func(r *store.GatewayMessageEnqueue) { r.MaxMessages = -1 }, store.ErrValidation},
		{"zero attempts", func(r *store.GatewayMessageEnqueue) { r.MaxAttempts = 0 }, store.ErrValidation},
		{"empty text", func(r *store.GatewayMessageEnqueue) { r.Text = " " }, store.ErrValidation},
		{"oversize text", func(r *store.GatewayMessageEnqueue) { r.Text = strings.Repeat("x", (16<<10)+1) }, store.ErrValidation},
		{"invalid UTF8", func(r *store.GatewayMessageEnqueue) { r.Text = "\xff" }, store.ErrValidation},
		{"control text", func(r *store.GatewayMessageEnqueue) { r.Text = "x\x00y" }, store.ErrValidation},
		{"expired request", func(r *store.GatewayMessageEnqueue) { r.ExpiresAt = r.Now }, store.ErrValidation},
		{"expired event", func(r *store.GatewayMessageEnqueue) {
			r.Now = r.Now.Add(25 * time.Hour)
			r.ExpiresAt = r.Now.Add(time.Hour)
		}, store.ErrConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := setupTestStore(t)
			now := time.Now().UTC().Truncate(time.Second)
			event := admitMessageTask(t, s, now)
			request := messageEnqueue(event, now, "operation")
			tc.mutate(&request)
			if _, _, err := s.EnqueueGatewayMessage(context.Background(), request); !errors.Is(err, tc.want) {
				t.Fatalf("enqueue = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestGatewayMessageOrderingAndTerminalReceiptIsolation(t *testing.T) {
	for _, kind := range []string{"final", "error"} {
		for _, skew := range []time.Duration{0, -time.Hour} {
			t.Run(fmt.Sprintf("%s/%s", kind, skew), func(t *testing.T) {
				s := setupTestStore(t)
				ctx := context.Background()
				now := time.Now().UTC().Truncate(time.Second)
				event := admitMessageTask(t, s, now)
				first := enqueueMessage(t, s, messageEnqueue(event, now, "first"))
				second := enqueueMessage(t, s, messageEnqueue(event, now.Add(skew), "second"))
				projection := messageTerminal(event, now.Add(skew), kind)
				terminal, created, err := s.ProjectGatewayTerminal(ctx, projection)
				if err != nil || !created {
					t.Fatalf("terminal = (%+v, %v, %v)", terminal, created, err)
				}
				if !first.CreatedAt.Before(second.CreatedAt) || !second.CreatedAt.Before(terminal.CreatedAt) {
					t.Fatalf("admission clock not monotonic: %v %v %v", first.CreatedAt, second.CreatedAt, terminal.CreatedAt)
				}
				replay, created, err := s.ProjectGatewayTerminal(ctx, projection)
				if err != nil || created || replay.ID != terminal.ID {
					t.Fatalf("terminal replay = (%+v, %v, %v)", replay, created, err)
				}
				if _, _, err := s.EnqueueGatewayMessage(ctx, messageEnqueue(event, now, "late")); !errors.Is(err, store.ErrConflict) {
					t.Fatalf("closed event enqueue = %v", err)
				}
				duplicate, created, err := s.EnqueueGatewayMessage(ctx, messageEnqueue(event, now, "first"))
				if err != nil || created || duplicate.ID != first.ID {
					t.Fatalf("closed event receipt replay = (%+v, %v, %v)", duplicate, created, err)
				}
				for _, message := range []*store.GatewayDelivery{first, second} {
					claimMessageDelivery(t, s, now, message.ID)
					if _, err := s.ClaimNextGatewayDelivery(ctx, event.Namespace, "other", now, time.Minute); !errors.Is(err, store.ErrNotFound) {
						t.Fatalf("live predecessor bypassed: %v", err)
					}
					if err := s.MarkGatewayDeliveryDelivered(ctx, event.Namespace, message.ID, "sender", "interim-provider", now); err != nil {
						t.Fatal(err)
					}
					after, err := s.GetGatewayEvent(ctx, event.Namespace, event.ID)
					if err != nil || after.DeliveryID != terminal.ID || after.ProviderMessageID != "" {
						t.Fatalf("interim overwrote terminal pointer: %+v, %v", after, err)
					}
					session, err := s.GetSession(ctx, event.Namespace, event.SessionName)
					if err != nil || len(session.Messages) != 2 || session.Messages[1].Metadata["providerMessageId"] != "" {
						t.Fatalf("interim overwrote terminal transcript: %+v, %v", session, err)
					}
				}
				claimMessageDelivery(t, s, now, terminal.ID)
				if err := s.MarkGatewayDeliveryDelivered(ctx, event.Namespace, terminal.ID, "sender", "terminal-provider", now); err != nil {
					t.Fatal(err)
				}
				after, err := s.GetGatewayEvent(ctx, event.Namespace, event.ID)
				if err != nil || after.ProviderMessageID != "terminal-provider" {
					t.Fatalf("terminal receipt = (%+v, %v)", after, err)
				}
			})
		}
	}
}

func TestGatewayMessageManualRetryRejectsAnyLaterStartedDelivery(t *testing.T) {
	for _, laterState := range []store.GatewayDeliveryState{store.GatewayDeliverySending, store.GatewayDeliveryRetryScheduled, store.GatewayDeliveryDelivered, store.GatewayDeliveryFailed, store.GatewayDeliveryDeadLettered, store.GatewayDeliveryExpired, store.GatewayDeliveryPending} {
		t.Run(string(laterState), func(t *testing.T) {
			s := setupTestStore(t)
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Second)
			event := admitMessageTask(t, s, now)
			first := enqueueMessage(t, s, messageEnqueue(event, now, "first"))
			later := enqueueMessage(t, s, messageEnqueue(event, now.Add(-time.Second), "later"))
			claimMessageDelivery(t, s, now, first.ID)
			if err := s.MarkGatewayDeliveryTerminal(ctx, event.Namespace, first.ID, "sender", store.GatewayDeliveryFailed, "failed", now); err != nil {
				t.Fatal(err)
			}
			// A pending, never-started successor does not prohibit manual retry.
			if _, err := s.RetryGatewayDelivery(ctx, event.Namespace, first.ID, now, now.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			claimMessageDelivery(t, s, now, first.ID)
			if err := s.MarkGatewayDeliveryTerminal(ctx, event.Namespace, first.ID, "sender", store.GatewayDeliveryDeadLettered, "failed", now); err != nil {
				t.Fatal(err)
			}
			claimMessageDelivery(t, s, now, later.ID)
			switch laterState {
			case store.GatewayDeliverySending:
			case store.GatewayDeliveryDelivered:
				if err := s.MarkGatewayDeliveryDelivered(ctx, event.Namespace, later.ID, "sender", "receipt", now); err != nil {
					t.Fatal(err)
				}
			case store.GatewayDeliveryRetryScheduled:
				if err := s.ScheduleGatewayDeliveryRetry(ctx, event.Namespace, later.ID, "sender", "uncertain", now.Add(time.Minute)); err != nil {
					t.Fatal(err)
				}
			default:
				state := laterState
				if state == store.GatewayDeliveryPending {
					state = store.GatewayDeliveryFailed
				}
				if err := s.MarkGatewayDeliveryTerminal(ctx, event.Namespace, later.ID, "sender", state, "abandoned", now); err != nil {
					t.Fatal(err)
				}
				if laterState == store.GatewayDeliveryPending {
					if _, err := s.RetryGatewayDelivery(ctx, event.Namespace, later.ID, now, now.Add(time.Hour)); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := s.RetryGatewayDelivery(ctx, event.Namespace, first.ID, now, now.Add(time.Hour)); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("late retry after %s = %v", laterState, err)
			}
		})
	}
}

func TestGatewayMessageExpiredPredecessorIsAbandoned(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	event := admitMessageTask(t, s, now)
	request := messageEnqueue(event, now, "expiring")
	request.ExpiresAt = now.Add(time.Second)
	first := enqueueMessage(t, s, request)
	terminal, _, err := s.ProjectGatewayTerminal(ctx, messageTerminal(event, now, "final"))
	if err != nil {
		t.Fatal(err)
	}
	// Claim processing must not depend on a separate maintenance tick to abandon expired messages.
	claimMessageDelivery(t, s, now.Add(2*time.Second), terminal.ID)
	stored, err := s.GetGatewayDelivery(ctx, event.Namespace, first.ID)
	if err != nil || stored.State != store.GatewayDeliveryExpired {
		t.Fatalf("expired message = (%+v, %v)", stored, err)
	}
}

func TestGatewayMessageRetentionPreservesLaterStartedEvidence(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	event := admitMessageTask(t, s, now)
	first := enqueueMessage(t, s, messageEnqueue(event, now, "failed"))
	terminal, _, err := s.ProjectGatewayTerminal(ctx, messageTerminal(event, now, "final"))
	if err != nil {
		t.Fatal(err)
	}
	claimMessageDelivery(t, s, now, first.ID)
	if err := s.MarkGatewayDeliveryTerminal(ctx, event.Namespace, first.ID, "sender", store.GatewayDeliveryFailed, "abandoned", now.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	claimMessageDelivery(t, s, now, terminal.ID)
	if _, err := s.RetryGatewayDelivery(ctx, event.Namespace, first.ID, now, now.Add(time.Hour)); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("retry during uncertain final send = %v", err)
	}
	if err := s.MarkGatewayDeliveryDelivered(ctx, event.Namespace, terminal.ID, "sender", "final-receipt", now); err != nil {
		t.Fatal(err)
	}
	result, err := s.MaintainGatewayRecords(ctx, event.Namespace, now.Add(4*time.Hour), now.Add(2*time.Hour))
	if err != nil || result.DeletedDeliveries != 0 {
		t.Fatalf("evidence pruned = (%+v, %v)", result, err)
	}
	if _, err := s.RetryGatewayDelivery(ctx, event.Namespace, first.ID, now.Add(4*time.Hour), now.Add(5*time.Hour)); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("post-retention revival = %v", err)
	}
	result, err = s.MaintainGatewayRecords(ctx, event.Namespace, now.Add(6*time.Hour), now.Add(5*time.Hour))
	if err != nil || result.DeletedDeliveries != 2 || result.DeletedEvents != 1 {
		t.Fatalf("whole event cleanup = (%+v, %v)", result, err)
	}
}

func TestGatewayMessageTransactionRollbackAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "messages.db")
	db, err := NewDB(path)
	if err != nil {
		t.Fatal(err)
	}
	s := NewStore(db, path)
	t.Cleanup(func() { _ = s.db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	now := time.Now().UTC().Truncate(time.Second)
	event := admitMessageTask(t, s, now)
	request := messageEnqueue(event, now, "operation")
	request.MaxMessages = 1
	rollback := errors.New("rollback")
	if err := s.WithTaskDataTransaction(ctx, func(txCtx context.Context) error {
		if _, _, err := s.EnqueueGatewayMessage(txCtx, request); err != nil {
			return err
		}
		return rollback
	}); !errors.Is(err, rollback) {
		t.Fatalf("transaction = %v", err)
	}
	var delivery *store.GatewayDelivery
	if err := s.WithTaskDataTransaction(ctx, func(txCtx context.Context) error {
		var err error
		delivery, _, err = s.EnqueueGatewayMessage(txCtx, request)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = NewDB(path)
	if err != nil {
		t.Fatal(err)
	}
	s = NewStore(db, path)
	duplicate, created, err := s.EnqueueGatewayMessage(ctx, request)
	if err != nil || created || duplicate.ID != delivery.ID {
		t.Fatalf("reopened replay = (%+v, %v, %v)", duplicate, created, err)
	}
	request.RequestID = "next"
	if _, _, err := s.EnqueueGatewayMessage(ctx, request); !errors.Is(err, store.ErrCapacity) {
		t.Fatalf("reopened cap = %v", err)
	}
}

func TestGatewayMessageTerminalRaceIsAtomic(t *testing.T) {
	for range 10 {
		s := setupTestStore(t)
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		event := admitMessageTask(t, s, now)
		var wg sync.WaitGroup
		var message *store.GatewayDelivery
		var messageErr error
		wg.Go(func() { message, _, messageErr = s.EnqueueGatewayMessage(ctx, messageEnqueue(event, now, "race")) })
		terminal, _, err := s.ProjectGatewayTerminal(ctx, messageTerminal(event, now.Add(-time.Hour), "final"))
		if err != nil {
			t.Fatal(err)
		}
		wg.Wait()
		if messageErr != nil && !errors.Is(messageErr, store.ErrConflict) {
			t.Fatal(messageErr)
		}
		if messageErr == nil {
			claimMessageDelivery(t, s, now, message.ID)
		} else {
			claimMessageDelivery(t, s, now, terminal.ID)
		}
		if _, _, err := s.EnqueueGatewayMessage(ctx, messageEnqueue(event, now, "after")); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("closed event enqueue = %v", err)
		}
	}
}

func TestGatewayMessageCannotBypassDedicatedAdmission(t *testing.T) {
	for _, entrypoint := range []string{"create", "terminal", "expiry"} {
		t.Run(entrypoint, func(t *testing.T) {
			s := setupTestStore(t)
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Second)
			event := admitMessageTask(t, s, now)
			projection := messageTerminal(event, now, "message")
			var err error
			switch entrypoint {
			case "create":
				_, _, err = s.CreateGatewayDelivery(ctx, &projection.Delivery)
			case "terminal":
				_, _, err = s.ProjectGatewayTerminal(ctx, projection)
			case "expiry":
				_, _, err = s.ExpireGatewayEventWithDelivery(ctx, store.GatewayExpiryProjection{EventID: event.ID, Delivery: projection.Delivery, CompletedAt: now})
			}
			if !errors.Is(err, store.ErrValidation) {
				t.Fatalf("bypass error = %v", err)
			}
			after, err := s.GetGatewayEvent(ctx, event.Namespace, event.ID)
			if err != nil || !reflect.DeepEqual(&event, after) {
				t.Fatalf("bypass mutated event: %+v, %v", after, err)
			}
		})
	}
}

func TestGatewayMessageAdmissionRequiresExactSessionLock(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	event := admitMessageTask(t, s, now)
	if err := s.ReleaseLock(ctx, event.Namespace, event.SessionName, event.TaskName, event.TaskUID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.EnqueueGatewayMessage(ctx, messageEnqueue(event, now, "stale")); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("released lock enqueue = %v", err)
	}
}

func TestGatewayMessageBoundsDoNotChangeGenericTerminalAdmission(t *testing.T) {
	for _, tc := range []struct {
		kind      string
		size      int
		wantError bool
	}{
		{"message", 16 << 10, false}, {"message", (16 << 10) + 1, true},
		// Terminal size validation remains at the service/protocol boundary.
		{"final", 64 << 10, false}, {"final", (64 << 10) + 1, false},
		{"error", 64 << 10, false}, {"error", (64 << 10) + 1, false},
	} {
		t.Run(fmt.Sprintf("%s/%d", tc.kind, tc.size), func(t *testing.T) {
			s := setupTestStore(t)
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Second)
			event := admitMessageTask(t, s, now)
			text := strings.Repeat("x", tc.size)
			var err error
			if tc.kind == "message" {
				request := messageEnqueue(event, now, "bounded")
				request.Text = text
				_, _, err = s.EnqueueGatewayMessage(ctx, request)
			} else {
				projection := messageTerminal(event, now, tc.kind)
				projection.Delivery.Text = text
				projection.Message.Content = text
				_, _, err = s.ProjectGatewayTerminal(ctx, projection)
			}
			if (err != nil) != tc.wantError {
				t.Fatalf("text admission = %v", err)
			}
		})
	}
}

func TestGatewayMessageActiveSendStillBlocksAfterExpiry(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	event := admitMessageTask(t, s, now)
	request := messageEnqueue(event, now, "uncertain")
	request.ExpiresAt = now.Add(time.Second)
	first := enqueueMessage(t, s, request)
	terminal, _, err := s.ProjectGatewayTerminal(ctx, messageTerminal(event, now, "final"))
	if err != nil {
		t.Fatal(err)
	}
	claimMessageDelivery(t, s, now, first.ID)
	if _, err := s.ClaimNextGatewayDelivery(ctx, event.Namespace, "other", now.Add(2*time.Second), time.Minute); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("active expired send bypassed = %v", err)
	}
	claimMessageDelivery(t, s, now.Add(time.Minute), terminal.ID)
}

func TestGatewayMessageExpiryReplayFindsTerminalOnly(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	event := admitMessageTask(t, s, now)
	enqueueMessage(t, s, messageEnqueue(event, now, "interim"))
	projection := store.GatewayExpiryProjection{EventID: event.ID, Reason: "expired", CompletedAt: now, Delivery: messageTerminal(event, now.Add(-time.Hour), "error").Delivery}
	terminal, created, err := s.ExpireGatewayEventWithDelivery(ctx, projection)
	if err != nil || !created {
		t.Fatalf("expiry = (%+v, %v, %v)", terminal, created, err)
	}
	replay, created, err := s.ExpireGatewayEventWithDelivery(ctx, projection)
	if err != nil || created || replay.ID != terminal.ID {
		t.Fatalf("expiry replay = (%+v, %v, %v)", replay, created, err)
	}
}
