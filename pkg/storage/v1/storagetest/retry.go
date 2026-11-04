package storagetest

import (
	"testing"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

// runLostResponses checks the retries a lost response causes: each replays
// the original outcome without a second effect.
func runLostResponses(t *testing.T, newProvider StateFactory) {
	prepare := func(t *testing.T) (storagev1.StateProvider, string) {
		provider := newProvider(t)
		advanceEpoch(t, provider, suiteFence)
		return provider, datasetID(t, provider)
	}
	envelope := func(t *testing.T, dataset, method string, request ...any) storagev1.WriteEnvelope {
		return NewWriteEnvelope(t, dataset, suiteFence, method, request...)
	}

	t.Run("a replayed chat turn reservation keeps the reservation", func(t *testing.T) {
		provider, dataset := prepare(t)
		ctx := t.Context()
		acquire := envelope(t, dataset, "AcquireChatTurn", chatSession(), "turn-1", writeCaseFuture)
		acquireCtx := storagev1.WithWriteEnvelope(ctx, acquire)
		created, err := provider.AcquireChatTurn(acquireCtx, chatSession(), "turn-1", writeCaseFuture)
		require.NoError(t, err)
		require.True(t, created)
		created, err = provider.AcquireChatTurn(acquireCtx, chatSession(), "turn-1", writeCaseFuture)
		require.NoError(t, err, "a retry of the reservation is not a busy Session")
		require.True(t, created, "the retry returns the original outcome")
		_, err = provider.AcquireChatTurn(ctx, chatSession(), "turn-2", writeCaseFuture)
		require.ErrorIs(t, err, storagev1.ErrConflict, "the reservation still belongs to its turn")
		require.NoError(t, provider.CommitSessionTurn(ctx, chatSession(), "turn-1", 0,
			[]storagev1.SessionMessage{{ID: "m1", Role: "user", Content: "hello"}}, 1, 1))
	})

	t.Run("a replayed turn commit applies once", func(t *testing.T) {
		provider, dataset := prepare(t)
		ctx := t.Context()
		reservedChat(t, provider)
		messages := []storagev1.SessionMessage{{ID: "m1", Role: "user", Content: "hello"}, {ID: "m2", Role: "assistant", Content: "hi"}}
		commit := envelope(t, dataset, "CommitSessionTurn", chatSession(), "turn-1", 0, messages, 11, 13)
		commitCtx := storagev1.WithWriteEnvelope(ctx, commit)
		require.NoError(t, provider.CommitSessionTurn(commitCtx, chatSession(), "turn-1", 0, messages, 11, 13))
		require.NoError(t, provider.CommitSessionTurn(commitCtx, chatSession(), "turn-1", 0, messages, 11, 13),
			"a retry of a committed turn reports its success")
		session, err := provider.GetSession(ctx, "tenant", "chat")
		require.NoError(t, err)
		require.Equal(t, 2, session.MessageCount)
		require.Equal(t, 11, session.InputTokens)
		require.Equal(t, 13, session.OutputTokens)
	})

	t.Run("a delayed lock release does not touch the next attempt's lock", func(t *testing.T) {
		provider, dataset := prepare(t)
		ctx := t.Context()
		lockedTenantSession(t, provider)
		release := envelope(t, dataset, "ReleaseLockWithTokenUsage", "tenant", "session", "task-a", "uid-a", 5, 6)
		releaseCtx := storagev1.WithWriteEnvelope(ctx, release)
		require.NoError(t, provider.ReleaseLockWithTokenUsage(releaseCtx, "tenant", "session", "task-a", "uid-a", 5, 6))
		// The Task's next attempt keeps its UID and takes the lock again.
		require.NoError(t, provider.AcquireLock(ctx, "tenant", "session", "task-a", "uid-a"))
		require.NoError(t, provider.ReleaseLockWithTokenUsage(releaseCtx, "tenant", "session", "task-a", "uid-a", 5, 6))

		listed, err := provider.ListSessions(ctx, "tenant")
		require.NoError(t, err)
		require.Len(t, listed, 1)
		require.Equal(t, "uid-a", listed[0].ActiveTaskUID, "the next attempt still holds its lock")
		require.Equal(t, 5, listed[0].InputTokens, "the tokens are counted once")
		require.Equal(t, 6, listed[0].OutputTokens)
	})

	t.Run("a reused event dedupe key must belong to the same Task, Session and tool call", func(t *testing.T) {
		provider, _ := prepare(t)
		ctx := t.Context()
		toolEvent := func() *storagev1.ExecutionEvent {
			event := caseEvent()
			event.SessionName = "session-a"
			event.ToolCallID = "call-1"
			return event
		}
		stored, appended, err := provider.AppendExecutionEventIfAbsent(ctx, toolEvent(), "event-1")
		require.NoError(t, err)
		require.True(t, appended)

		for name, change := range map[string]func(*storagev1.ExecutionEvent){
			"content":                  func(e *storagev1.ExecutionEvent) { e.Summary = "re-derived after a restart" },
			"type of a competing step": func(e *storagev1.ExecutionEvent) { e.Type = storagev1.ExecutionEventTypeTaskSucceeded },
			"unknown Session":          func(e *storagev1.ExecutionEvent) { e.SessionName = "" },
		} {
			same := toolEvent()
			change(same)
			again, appended, err := provider.AppendExecutionEventIfAbsent(ctx, same, "event-1")
			require.NoErrorf(t, err, "a request that differs only in its %s finds the stored event", name)
			require.False(t, appended)
			require.Equal(t, stored.ID, again.ID)
			require.Equal(t, "planned", again.Summary, "the first durable event stays")
		}

		for name, change := range map[string]func(*storagev1.ExecutionEvent){
			"Task":         func(e *storagev1.ExecutionEvent) { e.TaskName = "other-task" },
			"Session":      func(e *storagev1.ExecutionEvent) { e.SessionName = "session-b" },
			"tool call ID": func(e *storagev1.ExecutionEvent) { e.ToolCallID = "call-2" },
		} {
			other := toolEvent()
			change(other)
			_, _, err := provider.AppendExecutionEventIfAbsent(ctx, other, "event-1")
			require.ErrorIsf(t, err, storagev1.ErrDuplicateMismatch, "a different %s is a different event", name)
			_, _, err = provider.AppendExecutionEventWithPlanIfAbsent(ctx, other, "event-1",
				&storagev1.PlanState{Namespace: other.Namespace, TaskName: other.TaskName, Summary: "replaced"})
			require.ErrorIsf(t, err, storagev1.ErrDuplicateMismatch, "a different %s is a different event", name)
		}
		events := listEvents(t, provider, storagev1.ExecutionEventFilter{
			Namespace: "tenant", StreamType: storagev1.ExecutionEventStreamTypeTask, StreamID: "task",
		})
		require.Len(t, events, 1)
		_, err = provider.GetPlan(ctx, "tenant", "task")
		require.ErrorIs(t, err, storagev1.ErrNotFound, "a refused append leaves the plan alone")

		bare, appended, err := provider.AppendExecutionEventIfAbsent(ctx, caseEvent(), "event-2")
		require.NoError(t, err)
		require.True(t, appended)
		again, appended, err := provider.AppendExecutionEventIfAbsent(ctx, toolEvent(), "event-2")
		require.NoError(t, err, "a stored event without a Session or tool call matches a request that has them")
		require.False(t, appended)
		require.Equal(t, bare.ID, again.ID)
	})

	t.Run("outbox records commit only under the current fence", func(t *testing.T) {
		provider, _ := prepare(t)
		ctx := t.Context()
		enqueueCaseProjection(t, provider)
		claimed, err := provider.ClaimOutboxProjectionRecords(ctx, claimCaseProjection(suiteFence))
		require.NoError(t, err)
		require.Len(t, claimed, 1)
		advanceEpoch(t, provider, nextFence)

		_, err = provider.EnqueueOutboxProjectionRecord(ctx, outboxProjection("projection-2", `{"n":2}`, controlBaseTime), suiteFence)
		requireFenced(t, err)
		_, err = provider.ClaimOutboxProjectionRecords(ctx, claimCaseProjection(suiteFence))
		requireFenced(t, err)
		_, err = provider.CompleteOutboxProjectionRecord(ctx, storagev1.CompleteOutboxProjectionRequest{
			ID: outboxCaseID, Fence: suiteFence, ExpectedVersion: claimed[0].Version, LeaseOwner: "worker-a",
			OperationID: "deliver-projection", OperationDigest: controlDigest("deliver-projection"),
			NewState: storagev1.OutboxProjectionDelivered, DeliveryDigest: controlDigest("delivery"),
			UpdatedAt: controlBaseTime,
		})
		requireFenced(t, err)
		_, err = provider.GetOutboxProjection(ctx, "projection-2")
		require.ErrorIs(t, err, storagev1.ErrNotFound)
		projection, err := provider.GetOutboxProjection(ctx, outboxCaseID)
		require.NoError(t, err)
		require.Equal(t, claimed[0].Version, projection.Version, "the replaced controller's completion changed nothing")
	})
}
