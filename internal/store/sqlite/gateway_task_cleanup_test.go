package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store"
	"github.com/stretchr/testify/require"
)

func gatewayTaskCleanupTestEvent(now time.Time, suffix string) store.GatewayEvent {
	event := testGatewayEvent(now, suffix)
	event.BindingUID = testGatewayBindingUID
	event.TaskUID = "task-uid-" + suffix
	return event
}

func admitGatewayTaskCleanupTestEvent(t *testing.T, s *Store, event store.GatewayEvent) store.GatewayEvent {
	t.Helper()
	ctx := t.Context()
	taskUID := event.TaskUID
	event.TaskUID = ""
	admitted, created, err := s.AdmitGatewayEvent(ctx, store.GatewayEventAdmission{Event: event, AppendUserMessage: true})
	require.NoError(t, err)
	require.True(t, created)
	claimed, err := s.ClaimNextGatewayEvent(ctx, event.Namespace, "receipt-test", event.CreatedAt, time.Minute)
	require.NoError(t, err)
	require.Equal(t, admitted.ID, claimed.ID)
	require.NoError(t, s.MarkGatewayEventTaskCreated(ctx, event.Namespace, event.ID, event.TaskName, taskUID, "receipt-test", event.CreatedAt))
	linked, err := s.GetGatewayEvent(ctx, event.Namespace, event.ID)
	require.NoError(t, err)
	return *linked
}

func requireGatewayTaskCleanupReceipt(t *testing.T, s *Store, event store.GatewayEvent, compactedAt time.Time) *store.GatewayTaskCleanupReceipt {
	t.Helper()
	receipt, err := s.GetGatewayTaskCleanupReceipt(t.Context(), event.Namespace, event.TaskName, event.TaskUID)
	require.NoError(t, err)
	require.Equal(t, &store.GatewayTaskCleanupReceipt{
		Namespace: event.Namespace, NamespaceUID: event.NamespaceUID,
		GatewayName: event.GatewayName, GatewayUID: event.GatewayUID,
		BindingName: event.BindingName, BindingUID: event.BindingUID,
		EventID: event.ID, TaskName: event.TaskName, TaskUID: event.TaskUID,
		SessionName: event.SessionName, CompactedAt: compactedAt.UTC(),
	}, receipt)
	return receipt
}

func TestGatewayTaskCleanupReceiptRequiresRetentionCompaction(t *testing.T) {
	s := setupTestStore(t)
	ctx := t.Context()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	event := admitGatewayTaskCleanupTestEvent(t, s, gatewayTaskCleanupTestEvent(now.Add(-2*time.Hour), "retention-receipt"))
	cutoff := now.Add(-time.Hour)

	// A linked Task alone is not retention evidence, even when it is old.
	result, err := s.MaintainGatewayRecords(ctx, event.Namespace, now, cutoff)
	require.NoError(t, err)
	require.Zero(t, result.DeletedEvents)
	_, err = s.GetGatewayTaskCleanupReceipt(ctx, event.Namespace, event.TaskName, event.TaskUID)
	require.ErrorIs(t, err, store.ErrNotFound)

	attachDeliveredExpiryDelivery(t, s, ctx, event, cutoff)
	result, err = s.MaintainGatewayRecords(ctx, event.Namespace, now, cutoff)
	require.NoError(t, err)
	require.Zero(t, result.DeletedEvents, "the retention cutoff is exclusive")
	_, err = s.GetGatewayTaskCleanupReceipt(ctx, event.Namespace, event.TaskName, event.TaskUID)
	require.ErrorIs(t, err, store.ErrNotFound)

	result, err = s.MaintainGatewayRecords(ctx, "another-namespace", now.Add(time.Second), cutoff.Add(time.Second))
	require.NoError(t, err)
	require.Zero(t, result.DeletedEvents)
	_, err = s.GetGatewayTaskCleanupReceipt(ctx, event.Namespace, event.TaskName, event.TaskUID)
	require.ErrorIs(t, err, store.ErrNotFound)

	compactedAt := now.Add(time.Second)
	result, err = s.MaintainGatewayRecords(ctx, event.Namespace, compactedAt, cutoff.Add(time.Second))
	require.NoError(t, err)
	require.Equal(t, 1, result.DeletedEvents)
	require.Equal(t, 1, result.UpsertedTombstones)
	requireGatewayTaskCleanupReceipt(t, s, event, compactedAt)
	_, err = s.GetGatewayEvent(ctx, event.Namespace, event.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
	completion, err := s.GetSessionCleanupCompletion(ctx, event.Namespace, event.SessionName)
	require.NoError(t, err)
	require.Empty(t, completion.SessionUID, "a preadmission receipt must not invent ACP identity")
	var turns int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_turns`).Scan(&turns))
	require.Zero(t, turns)
}

func TestGatewayTaskCleanupReceiptWaitsForDeliveryRetention(t *testing.T) {
	s := setupTestStore(t)
	ctx := t.Context()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-2 * time.Hour)
	event := admitGatewayTaskCleanupTestEvent(t, s, gatewayTaskCleanupTestEvent(old, "delivery-receipt"))
	delivery, _, err := s.ExpireGatewayEventWithDelivery(ctx, store.GatewayExpiryProjection{
		EventID: event.ID, Reason: "expired", CompletedAt: old.Add(time.Minute),
		Delivery: store.GatewayDelivery{
			ID: "pending-receipt-delivery", IdempotencyID: "pending-receipt-delivery",
			Namespace: event.Namespace, NamespaceUID: event.NamespaceUID,
			GatewayUID: event.GatewayUID, GatewayGeneration: event.GatewayGeneration,
			GatewayName: event.GatewayName, BindingName: event.BindingName, EventID: event.ID,
			SessionName: event.SessionName, Kind: "error", State: store.GatewayDeliveryPending,
			AccountID: event.AccountID, ContextID: event.ContextID, ReplyTarget: event.ReplyTarget,
			Text: "failed", MaxAttempts: 10, NextAttemptAt: old, ExpiresAt: now.Add(24 * time.Hour),
			CreatedAt: old, UpdatedAt: old,
		},
	})
	require.NoError(t, err)
	result, err := s.MaintainGatewayRecords(ctx, event.Namespace, now, now.Add(-time.Hour))
	require.NoError(t, err)
	require.Zero(t, result.DeletedEvents)
	_, err = s.GetGatewayTaskCleanupReceipt(ctx, event.Namespace, event.TaskName, event.TaskUID)
	require.ErrorIs(t, err, store.ErrNotFound)

	claimed, err := s.ClaimNextGatewayDelivery(ctx, event.Namespace, "delivery-receipt-test", now, time.Minute)
	require.NoError(t, err)
	require.Equal(t, delivery.ID, claimed.ID)
	require.NoError(t, s.MarkGatewayDeliveryDelivered(ctx, event.Namespace, delivery.ID, "delivery-receipt-test", "provider-reply", now))
	result, err = s.MaintainGatewayRecords(ctx, event.Namespace, now.Add(time.Minute), now.Add(-59*time.Minute))
	require.NoError(t, err)
	require.Zero(t, result.DeletedEvents, "the delivered reply still has its own retention window")
	_, err = s.GetGatewayTaskCleanupReceipt(ctx, event.Namespace, event.TaskName, event.TaskUID)
	require.ErrorIs(t, err, store.ErrNotFound)

	compactedAt := now.Add(2 * time.Hour)
	result, err = s.MaintainGatewayRecords(ctx, event.Namespace, compactedAt, compactedAt.Add(-time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, result.DeletedEvents)
	requireGatewayTaskCleanupReceipt(t, s, event, compactedAt)
}

func TestGatewayTaskCleanupReceiptSurvivesTombstoneExpiryAndReopen(t *testing.T) {
	s := setupDiskStore(t)
	ctx := t.Context()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	event := admitGatewayTaskCleanupTestEvent(t, s, gatewayTaskCleanupTestEvent(now.Add(-2*time.Hour), "persistent-receipt"))
	attachDeliveredExpiryDelivery(t, s, ctx, event, now.Add(-90*time.Minute))
	result, err := s.MaintainGatewayRecords(ctx, event.Namespace, now, now.Add(-time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, result.DeletedEvents)
	originalReceipt := requireGatewayTaskCleanupReceipt(t, s, event, now)
	duplicate, err := s.GetGatewayEventDuplicate(ctx, &event, now.Add(30*time.Minute))
	require.NoError(t, err)
	require.Equal(t, event.SessionName, duplicate.SessionName)

	afterExpiry := now.Add(2 * time.Hour)
	result, err = s.MaintainGatewayRecords(ctx, event.Namespace, afterExpiry, afterExpiry.Add(-time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, result.DeletedTombstones)
	tombstoned, err := s.HasGatewayTaskTombstone(ctx, event.Namespace, event.TaskName, event.TaskUID)
	require.NoError(t, err)
	require.False(t, tombstoned)
	_, err = s.GetGatewayEventDuplicate(ctx, &event, afterExpiry)
	require.ErrorIs(t, err, store.ErrNotFound, "Task cleanup evidence must not extend event deduplication")
	require.NoError(t, s.db.Close())
	reopened, err := NewDB(s.dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reopened.Close() })
	s = NewStore(reopened, s.dbPath)
	require.Equal(t, originalReceipt, requireGatewayTaskCleanupReceipt(t, s, event, now))

	// A later admission deliberately reuses the external ID and Task name,
	// but its distinct Task UID must retain separate cleanup evidence.
	fresh := gatewayTaskCleanupTestEvent(afterExpiry, "persistent-receipt")
	fresh.TaskUID = "replacement-task-uid"
	fresh = admitGatewayTaskCleanupTestEvent(t, s, fresh)
	require.Equal(t, event.ID, fresh.ID)
	require.Equal(t, event.ExternalEventID, fresh.ExternalEventID)
	require.Equal(t, event.TaskName, fresh.TaskName)
	require.NotEqual(t, event.SessionName, fresh.SessionName)
	attachDeliveredExpiryDelivery(t, s, ctx, fresh, afterExpiry.Add(time.Minute))
	nextCompaction := afterExpiry.Add(2 * time.Hour)
	result, err = s.MaintainGatewayRecords(ctx, event.Namespace, nextCompaction, nextCompaction.Add(-time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, result.DeletedEvents)
	requireGatewayTaskCleanupReceipt(t, s, fresh, nextCompaction)
	require.Equal(t, originalReceipt, requireGatewayTaskCleanupReceipt(t, s, event, now))
	var receipts int
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM gateway_task_cleanup_receipts`).Scan(&receipts))
	require.Equal(t, 2, receipts)

	// Once the original Task is gone, removing its exact receipt must preserve
	// the replacement's authority and both Sessions' independent completions.
	for _, identity := range [][3]string{
		{"other", event.TaskName, event.TaskUID},
		{event.Namespace, "other-task", event.TaskUID},
		{event.Namespace, event.TaskName, "other-uid"},
	} {
		require.NoError(t, s.DeleteGatewayTaskCleanupReceipt(ctx, identity[0], identity[1], identity[2]))
		require.Equal(t, originalReceipt, requireGatewayTaskCleanupReceipt(t, s, event, now))
	}
	require.NoError(t, s.DeleteGatewayTaskCleanupReceipt(ctx, event.Namespace, event.TaskName, event.TaskUID))
	require.NoError(t, s.DeleteGatewayTaskCleanupReceipt(ctx, event.Namespace, event.TaskName, event.TaskUID))
	_, err = s.GetGatewayTaskCleanupReceipt(ctx, event.Namespace, event.TaskName, event.TaskUID)
	require.ErrorIs(t, err, store.ErrNotFound)
	requireGatewayTaskCleanupReceipt(t, s, fresh, nextCompaction)
	for _, sessionName := range []string{event.SessionName, fresh.SessionName} {
		_, err = s.GetSessionCleanupCompletion(ctx, event.Namespace, sessionName)
		require.NoError(t, err)
	}
	duplicate, err = s.GetGatewayEventDuplicate(ctx, &fresh, nextCompaction.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, fresh.SessionName, duplicate.SessionName)
}

func TestGatewayTaskCleanupReceiptRollsBackWithCompaction(t *testing.T) {
	for _, failure := range []struct {
		name    string
		trigger string
	}{
		{name: "receipt insert", trigger: "BEFORE INSERT ON gateway_task_cleanup_receipts"},
		{name: "event deletion", trigger: "BEFORE DELETE ON gateway_events"},
		{name: "Session completion", trigger: "BEFORE INSERT ON session_cleanup_completions"},
	} {
		t.Run(failure.name, func(t *testing.T) {
			s := setupTestStore(t)
			ctx := t.Context()
			now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
			event := admitGatewayTaskCleanupTestEvent(t, s, gatewayTaskCleanupTestEvent(now.Add(-2*time.Hour), "atomic-receipt"))
			attachDeliveredExpiryDelivery(t, s, ctx, event, now.Add(-90*time.Minute))
			before, err := s.GetGatewayEvent(ctx, event.Namespace, event.ID)
			require.NoError(t, err)
			transcript, err := s.LoadTranscript(ctx, event.Namespace, event.SessionName, 0)
			require.NoError(t, err)
			_, err = s.db.ExecContext(ctx, "CREATE TRIGGER fail_cleanup_receipt "+failure.trigger+" BEGIN SELECT RAISE(ABORT, 'injected compaction failure'); END")
			require.NoError(t, err)

			_, err = s.MaintainGatewayRecords(ctx, event.Namespace, now, now.Add(-time.Hour))
			require.ErrorContains(t, err, "injected compaction failure")
			_, err = s.GetGatewayTaskCleanupReceipt(ctx, event.Namespace, event.TaskName, event.TaskUID)
			require.ErrorIs(t, err, store.ErrNotFound)
			after, err := s.GetGatewayEvent(ctx, event.Namespace, event.ID)
			require.NoError(t, err)
			require.Equal(t, before, after)
			retainedTranscript, err := s.LoadTranscript(ctx, event.Namespace, event.SessionName, 0)
			require.NoError(t, err)
			require.Equal(t, transcript, retainedTranscript)
			_, err = s.GetGatewayDelivery(ctx, event.Namespace, before.DeliveryID)
			require.NoError(t, err, "reply retention deletion must also roll back")
			tombstoned, err := s.HasGatewayTaskTombstone(ctx, event.Namespace, event.TaskName, event.TaskUID)
			require.NoError(t, err)
			require.False(t, tombstoned)
			_, err = s.GetSessionCleanupCompletion(ctx, event.Namespace, event.SessionName)
			require.ErrorIs(t, err, store.ErrNotFound)

			_, err = s.db.ExecContext(ctx, "DROP TRIGGER fail_cleanup_receipt")
			require.NoError(t, err)
			_, err = s.MaintainGatewayRecords(ctx, event.Namespace, now, now.Add(-time.Hour))
			require.NoError(t, err)
			requireGatewayTaskCleanupReceipt(t, s, event, now)
		})
	}
}

func TestGatewayTaskCleanupReceiptRefusesContradictoryOwnership(t *testing.T) {
	for _, change := range []struct {
		name   string
		mutate func(*store.GatewayEvent)
	}{
		{name: "namespace UID", mutate: func(event *store.GatewayEvent) { event.NamespaceUID += "-replaced" }},
		{name: "Gateway name", mutate: func(event *store.GatewayEvent) { event.GatewayName += "-replaced" }},
		{name: "Gateway UID", mutate: func(event *store.GatewayEvent) { event.GatewayUID += "-replaced" }},
		{name: "Binding name", mutate: func(event *store.GatewayEvent) { event.BindingName += "-replaced" }},
		{name: "Binding UID", mutate: func(event *store.GatewayEvent) { event.BindingUID += "-replaced" }},
		{name: "event ID", mutate: func(event *store.GatewayEvent) { event.ID += "-replaced" }},
		{name: "Task name", mutate: func(event *store.GatewayEvent) { event.TaskName += "-replaced" }},
		{name: "Session name", mutate: func(event *store.GatewayEvent) { event.SessionName += "-replaced" }},
	} {
		t.Run(change.name, func(t *testing.T) {
			s := setupTestStore(t)
			ctx := t.Context()
			now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
			event := admitGatewayTaskCleanupTestEvent(t, s, gatewayTaskCleanupTestEvent(now.Add(-2*time.Hour), "immutable-receipt"))
			attachDeliveredExpiryDelivery(t, s, ctx, event, now.Add(-90*time.Minute))
			_, err := s.MaintainGatewayRecords(ctx, event.Namespace, now, now.Add(-time.Hour))
			require.NoError(t, err)
			original := requireGatewayTaskCleanupReceipt(t, s, event, now)

			// Exercise an exact retry and a contradictory writer inside the same
			// transaction boundary used by maintenance. Neither may rewrite evidence.
			tx, err := s.db.BeginTx(ctx, nil)
			require.NoError(t, err)
			require.NoError(t, archiveGatewayTaskCleanupReceiptTx(ctx, tx, &event, now.Add(time.Hour)))
			require.NoError(t, tx.Commit())
			require.Equal(t, original, requireGatewayTaskCleanupReceipt(t, s, event, now))
			changed := event
			change.mutate(&changed)
			for _, attemptedAt := range []time.Time{now.Add(-time.Hour), now.Add(time.Hour)} {
				tx, err = s.db.BeginTx(ctx, nil)
				require.NoError(t, err)
				err = archiveGatewayTaskCleanupReceiptTx(ctx, tx, &changed, attemptedAt)
				require.ErrorIs(t, err, store.ErrConflict)
				require.NoError(t, tx.Rollback())
				require.Equal(t, original, requireGatewayTaskCleanupReceipt(t, s, event, now))
			}
		})
	}
}

func TestGatewayTaskCleanupReceiptRequiresLinkedTask(t *testing.T) {
	for _, missing := range []string{"Task name", "Task UID", "both"} {
		t.Run(missing, func(t *testing.T) {
			s := setupTestStore(t)
			ctx := t.Context()
			now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
			event := gatewayTaskCleanupTestEvent(now.Add(-2*time.Hour), "unlinked-receipt")
			event.State = store.GatewayEventRejected
			if missing != "Task UID" {
				event.TaskName = ""
			}
			if missing != "Task name" {
				event.TaskUID = ""
			}
			_, created, err := s.AdmitGatewayEvent(ctx, store.GatewayEventAdmission{Event: event})
			require.NoError(t, err)
			require.True(t, created)
			result, err := s.MaintainGatewayRecords(ctx, event.Namespace, now, now.Add(-time.Hour))
			require.NoError(t, err)
			require.Equal(t, 1, result.DeletedEvents)
			var receipts int
			require.NoError(t, s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM gateway_task_cleanup_receipts`).Scan(&receipts))
			require.Zero(t, receipts)
		})
	}
}

func TestGatewayTaskCleanupReceiptRejectsInvalidReads(t *testing.T) {
	s := setupTestStore(t)
	ctx := t.Context()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	event := admitGatewayTaskCleanupTestEvent(t, s, gatewayTaskCleanupTestEvent(now.Add(-2*time.Hour), "read-receipt"))
	attachDeliveredExpiryDelivery(t, s, ctx, event, now.Add(-90*time.Minute))
	_, err := s.MaintainGatewayRecords(ctx, event.Namespace, now, now.Add(-time.Hour))
	require.NoError(t, err)
	for _, input := range []struct {
		name                         string
		namespace, taskName, taskUID string
		want                         error
	}{
		{name: "another namespace", namespace: "other", taskName: event.TaskName, taskUID: event.TaskUID, want: store.ErrNotFound},
		{name: "replaced Task", namespace: event.Namespace, taskName: event.TaskName, taskUID: "replacement-uid", want: store.ErrNotFound},
		{name: "wrong Task name", namespace: event.Namespace, taskName: "different-task", taskUID: event.TaskUID, want: store.ErrConflict},
		{name: "missing namespace", taskName: event.TaskName, taskUID: event.TaskUID, want: store.ErrValidation},
		{name: "missing Task name", namespace: event.Namespace, taskUID: event.TaskUID, want: store.ErrValidation},
		{name: "missing Task UID", namespace: event.Namespace, taskName: event.TaskName, want: store.ErrValidation},
		{name: "inexact namespace", namespace: event.Namespace + " ", taskName: event.TaskName, taskUID: event.TaskUID, want: store.ErrValidation},
		{name: "inexact Task name", namespace: event.Namespace, taskName: " " + event.TaskName, taskUID: event.TaskUID, want: store.ErrValidation},
		{name: "control character", namespace: event.Namespace, taskName: event.TaskName, taskUID: event.TaskUID + "\n", want: store.ErrValidation},
	} {
		t.Run(input.name, func(t *testing.T) {
			receipt, err := s.GetGatewayTaskCleanupReceipt(ctx, input.namespace, input.taskName, input.taskUID)
			require.ErrorIs(t, err, input.want)
			require.Nil(t, receipt)
		})
	}
	_, err = s.db.ExecContext(ctx, `UPDATE gateway_task_cleanup_receipts SET binding_uid = '' WHERE namespace = ? AND task_uid = ?`, event.Namespace, event.TaskUID)
	require.NoError(t, err)
	receipt, err := s.GetGatewayTaskCleanupReceipt(ctx, event.Namespace, event.TaskName, event.TaskUID)
	require.ErrorIs(t, err, store.ErrValidation)
	require.Nil(t, receipt)
	_, err = s.db.ExecContext(ctx, `UPDATE gateway_task_cleanup_receipts SET binding_uid = ?, compacted_at = ? WHERE namespace = ? AND task_uid = ?`,
		event.BindingUID, time.Time{}, event.Namespace, event.TaskUID)
	require.NoError(t, err)
	receipt, err = s.GetGatewayTaskCleanupReceipt(ctx, event.Namespace, event.TaskName, event.TaskUID)
	require.ErrorIs(t, err, store.ErrValidation)
	require.Nil(t, receipt)
}

func TestGatewayTaskCleanupReceiptReadPropagatesStoreError(t *testing.T) {
	s := setupTestStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	receipt, err := s.GetGatewayTaskCleanupReceipt(ctx, "default", "task", "task-uid")
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, errors.Is(err, store.ErrNotFound))
	require.Nil(t, receipt)
}

func TestGatewayTaskCleanupReceiptPagesUseExactCursor(t *testing.T) {
	s := setupTestStore(t)
	ctx := t.Context()
	now := time.Now().UTC()
	for _, identity := range [][2]string{{"team-b", "b-uid"}, {"team-a", "b-uid"}, {"team-b", "a-uid"}, {"team-a", "a-uid"}} {
		event := gatewayTaskCleanupTestEvent(now.Add(-time.Hour), identity[0]+identity[1])
		event.Namespace, event.TaskName, event.TaskUID = identity[0], "reused-name", identity[1]
		tx, err := s.db.BeginTx(ctx, nil)
		require.NoError(t, err)
		require.NoError(t, archiveGatewayTaskCleanupReceiptTx(ctx, tx, &event, now))
		require.NoError(t, tx.Commit())
	}
	first, err := s.ListGatewayTaskCleanupReceipts(ctx, store.GatewayTaskCleanupReceiptFilter{Limit: 2})
	require.NoError(t, err)
	require.Len(t, first, 2)
	require.Equal(t, "team-a", first[0].Namespace)
	require.Equal(t, "a-uid", first[0].TaskUID)
	require.Equal(t, "team-a", first[1].Namespace)
	require.Equal(t, "b-uid", first[1].TaskUID)
	for _, receipt := range first {
		require.NoError(t, s.DeleteGatewayTaskCleanupReceipt(ctx, receipt.Namespace, receipt.TaskName, receipt.TaskUID))
	}
	second, err := s.ListGatewayTaskCleanupReceipts(ctx, store.GatewayTaskCleanupReceiptFilter{
		AfterNamespace: first[1].Namespace, AfterTaskUID: first[1].TaskUID, Limit: 2,
	})
	require.NoError(t, err)
	require.Len(t, second, 2, "deleting a prior page must not skip rows in the next namespace")
	require.Equal(t, "team-b", second[0].Namespace)
	require.Equal(t, "a-uid", second[0].TaskUID)
	require.Equal(t, "team-b", second[1].Namespace)
	require.Equal(t, "b-uid", second[1].TaskUID)
	last, err := s.ListGatewayTaskCleanupReceipts(ctx, store.GatewayTaskCleanupReceiptFilter{
		AfterNamespace: second[1].Namespace, AfterTaskUID: second[1].TaskUID, Limit: 2,
	})
	require.NoError(t, err)
	require.Empty(t, last)
	scoped, err := s.ListGatewayTaskCleanupReceipts(ctx, store.GatewayTaskCleanupReceiptFilter{Namespace: "team-a", Limit: 2})
	require.NoError(t, err)
	require.Empty(t, scoped, "namespace scope must not match another namespace's identical Task UIDs")
	scoped, err = s.ListGatewayTaskCleanupReceipts(ctx, store.GatewayTaskCleanupReceiptFilter{Namespace: "team-b", Limit: 1})
	require.NoError(t, err)
	require.Equal(t, second[:1], scoped)
}

func TestGatewayTaskCleanupReceiptMaintenanceRejectsInvalidInputAndStoreErrors(t *testing.T) {
	s := setupTestStore(t)
	for _, filter := range []store.GatewayTaskCleanupReceiptFilter{
		{Limit: 0}, {Limit: -1}, {Limit: 101},
		{Limit: 1, Namespace: " default"},
		{Limit: 1, AfterNamespace: "default"},
		{Limit: 1, AfterTaskUID: "task-uid"},
		{Limit: 1, AfterNamespace: "default", AfterTaskUID: "task-uid\n"},
	} {
		_, err := s.ListGatewayTaskCleanupReceipts(t.Context(), filter)
		require.ErrorIs(t, err, store.ErrValidation)
	}
	for _, identity := range [][3]string{
		{"", "task", "uid"}, {"default", "", "uid"}, {"default", "task", ""}, {"default", " task", "uid"},
	} {
		require.ErrorIs(t, s.DeleteGatewayTaskCleanupReceipt(t.Context(), identity[0], identity[1], identity[2]), store.ErrValidation)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := s.ListGatewayTaskCleanupReceipts(ctx, store.GatewayTaskCleanupReceiptFilter{Limit: 1})
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, s.DeleteGatewayTaskCleanupReceipt(ctx, "default", "task", "uid"), context.Canceled)
}
