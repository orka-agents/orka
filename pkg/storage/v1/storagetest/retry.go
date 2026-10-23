package storagetest

import (
	"context"
	"fmt"
	"testing"
	"time"

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
		acquire := envelope(t, dataset, "AcquireChatTurn", chatTurn("turn-1"))
		acquireCtx := storagev1.WithWriteEnvelope(ctx, acquire)
		created, err := provider.AcquireChatTurn(acquireCtx, chatTurn("turn-1"))
		require.NoError(t, err)
		require.True(t, created)
		created, err = provider.AcquireChatTurn(acquireCtx, chatTurn("turn-1"))
		require.NoError(t, err, "a retry of the reservation is not a busy Session")
		require.True(t, created, "the retry returns the original outcome")
		_, err = provider.AcquireChatTurn(ctx, chatTurn("turn-2"))
		require.ErrorIs(t, err, storagev1.ErrConflict, "the reservation still belongs to its turn")
		require.NoError(t, provider.CommitSessionTurn(ctx, turnCommit(chatSession(), "turn-1", 0,
			[]storagev1.SessionMessage{{ID: "m1", Role: "user", Content: "hello"}}, 1, 1)))
	})

	t.Run("a replayed turn commit applies once", func(t *testing.T) {
		provider, dataset := prepare(t)
		ctx := t.Context()
		reservedChat(t, provider)
		messages := []storagev1.SessionMessage{{ID: "m1", Role: "user", Content: "hello"}, {ID: "m2", Role: "assistant", Content: "hi"}}
		request := turnCommit(chatSession(), "turn-1", 0, messages, 11, 13)
		commitCtx := storagev1.WithWriteEnvelope(ctx, envelope(t, dataset, "CommitSessionTurn", request))
		require.NoError(t, provider.CommitSessionTurn(commitCtx, request))
		require.NoError(t, provider.CommitSessionTurn(commitCtx, request),
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
		release := envelope(t, dataset, "ReleaseLockWithTokenUsage", lockOwner("tenant", "session", "task-a", "uid-a"), 5, 6)
		releaseCtx := storagev1.WithWriteEnvelope(ctx, release)
		require.NoError(t, provider.ReleaseLockWithTokenUsage(releaseCtx, lockOwner("tenant", "session", "task-a", "uid-a"), 5, 6))
		// The Task's next attempt keeps its UID and takes the lock again.
		require.NoError(t, provider.AcquireLock(ctx, "tenant", "session", "task-a", "uid-a", leaseNow))
		require.NoError(t, provider.ReleaseLockWithTokenUsage(releaseCtx, lockOwner("tenant", "session", "task-a", "uid-a"), 5, 6))

		listed, err := provider.ListSessions(ctx, "tenant")
		require.NoError(t, err)
		require.Len(t, listed, 1)
		require.Equal(t, "uid-a", listed[0].ActiveTaskUID, "the next attempt still holds its lock")
		require.Equal(t, 5, listed[0].InputTokens, "the tokens are counted once")
		require.Equal(t, 6, listed[0].OutputTokens)
	})

	t.Run("a replayed consuming read returns its batch and delivers each message once", func(t *testing.T) {
		provider, dataset := prepare(t)
		ctx := t.Context()
		for _, content := range []string{"one", "two", "three"} {
			sendInboxMessage(t, provider, content)
		}
		consumeA := envelope(t, dataset, "ConsumeMessages", inboxRequest(0))
		ctxA := storagev1.WithWriteEnvelope(ctx, consumeA)
		first, err := provider.ConsumeMessages(ctxA, inboxRequest(0))
		require.NoError(t, err)
		require.Equal(t, []string{"one", "two", "three"}, messageContents(first))
		sendInboxMessage(t, provider, "four")

		replay, err := provider.ConsumeMessages(ctxA, inboxRequest(0))
		require.NoError(t, err)
		require.Equal(t, first, replay, "a lost response is answered with the original batch, not the new message")
		consumeB, err := provider.ConsumeMessages(ctx, inboxRequest(0))
		require.NoError(t, err)
		require.Equal(t, []string{"four"}, messageContents(consumeB), "the next consumer receives only the new message")
		left, err := provider.ListUnreadMessages(ctx, "tenant", "reader", "parent", 0)
		require.NoError(t, err)
		require.Empty(t, left, "each message was delivered exactly once")
	})

	t.Run("a replayed consuming read leaves out messages deleted since", func(t *testing.T) {
		provider, dataset := prepare(t)
		ctx := t.Context()
		sendInboxMessage(t, provider, "kept")
		require.NoError(t, provider.SendMessage(ctx, &storagev1.Message{
			Namespace: "tenant", FromTask: "deleted-writer", ToTask: "reader", ParentTask: "parent", Content: "deleted",
		}))
		consume := envelope(t, dataset, "ConsumeMessages", inboxRequest(0))
		consumeCtx := storagev1.WithWriteEnvelope(ctx, consume)
		first, err := provider.ConsumeMessages(consumeCtx, inboxRequest(0))
		require.NoError(t, err)
		require.Equal(t, []string{"kept", "deleted"}, messageContents(first))
		require.NoError(t, provider.DeleteTaskMessages(ctx, "tenant", "deleted-writer"))
		replay, err := provider.ConsumeMessages(consumeCtx, inboxRequest(0))
		require.NoError(t, err)
		require.Equal(t, first[:1], replay)
	})

	t.Run("consuming reads are bounded", func(t *testing.T) {
		provider, _ := prepare(t)
		ctx := t.Context()
		for i := range storagev1.MaxMessageBatch + 100 {
			sendInboxMessage(t, provider, fmt.Sprintf("message %d", i))
		}
		for _, limit := range []int{-1, storagev1.MaxMessageBatch + 1} {
			_, err := provider.ConsumeMessages(ctx, inboxRequest(limit))
			require.ErrorIsf(t, err, storagev1.ErrValidation, "limit %d", limit)
			_, err = provider.ListUnreadMessages(ctx, "tenant", "reader", "parent", limit)
			require.ErrorIsf(t, err, storagev1.ErrValidation, "limit %d", limit)
		}
		peek, err := provider.ListUnreadMessages(ctx, "tenant", "reader", "parent", 3)
		require.NoError(t, err)
		require.Equal(t, []string{"message 0", "message 1", "message 2"}, messageContents(peek), "the oldest come first")
		listed, err := provider.ListUnreadMessages(ctx, "tenant", "reader", "parent", 0)
		require.NoError(t, err)
		require.Len(t, listed, storagev1.MaxMessageBatch, "a zero limit selects the batch cap")

		first, err := provider.ConsumeMessages(ctx, inboxRequest(0))
		require.NoError(t, err)
		require.Len(t, first, storagev1.MaxMessageBatch)
		require.Equal(t, "message 0", first[0].Content)
		second, err := provider.ConsumeMessages(ctx, inboxRequest(0))
		require.NoError(t, err)
		require.Len(t, second, 100)
		require.Equal(t, fmt.Sprintf("message %d", storagev1.MaxMessageBatch), second[0].Content)
		for i := 1; i < len(first); i++ {
			require.Less(t, first[i-1].ID, first[i].ID, "a batch is ordered by ID")
		}
		require.Less(t, first[len(first)-1].ID, second[0].ID)
	})

	t.Run("concurrent consumers of one inbox receive disjoint batches", func(t *testing.T) {
		provider, _ := prepare(t)
		const total = 40
		for i := range total {
			sendInboxMessage(t, provider, fmt.Sprintf("message %d", i))
		}
		batches := make([][]storagev1.Message, concurrentCallers)
		errs := runConcurrently(func(caller int) error {
			var err error
			batches[caller], err = provider.ConsumeMessages(t.Context(), inboxRequest(7))
			return err
		})
		seen := map[int64]bool{}
		for caller, err := range errs {
			require.NoError(t, err)
			for _, message := range batches[caller] {
				require.Falsef(t, seen[message.ID], "message %d was delivered twice", message.ID)
				seen[message.ID] = true
			}
		}
		left, err := provider.ConsumeMessages(t.Context(), inboxRequest(0))
		require.NoError(t, err)
		for _, message := range left {
			require.Falsef(t, seen[message.ID], "message %d was delivered twice", message.ID)
			seen[message.ID] = true
		}
		require.Len(t, seen, total, "every message was delivered")
	})

	t.Run("a replayed send returns the assigned ID without a second message", func(t *testing.T) {
		provider, dataset := prepare(t)
		ctx := t.Context()
		sent := inboxMessage("hello")
		send := envelope(t, dataset, "SendMessage", sent)
		sendCtx := storagev1.WithWriteEnvelope(ctx, send)
		require.NoError(t, provider.SendMessage(sendCtx, sent))
		require.NotZero(t, sent.ID, "the provider writes the assigned ID back")
		require.False(t, sent.CreatedAt.IsZero(), "the provider writes the creation time back")
		retry := inboxMessage("hello")
		require.NoError(t, provider.SendMessage(sendCtx, retry))
		require.Equal(t, sent, retry, "the retry is given the original ID and time")

		inbox, err := provider.ConsumeMessages(ctx, inboxRequest(0))
		require.NoError(t, err)
		require.Len(t, inbox, 1, "the retry stored no second message")
		require.Equal(t, sent.ID, inbox[0].ID)
		again, err := provider.ConsumeMessages(ctx, inboxRequest(0))
		require.NoError(t, err)
		require.Empty(t, again, "the message is consumed once")
	})

	t.Run("a sent message keeps the caller's creation time", func(t *testing.T) {
		provider, _ := prepare(t)
		ctx := t.Context()
		sent := inboxMessage("timed")
		sent.CreatedAt = time.Date(2026, 8, 1, 12, 0, 0, 123456789, time.FixedZone("plus-two", 2*60*60))
		require.NoError(t, provider.SendMessage(ctx, sent))
		inbox, err := provider.ListUnreadMessages(ctx, "tenant", "reader", "parent", 0)
		require.NoError(t, err)
		require.Len(t, inbox, 1)
		require.True(t, sent.CreatedAt.Equal(inbox[0].CreatedAt), "the time is kept to the nanosecond")
		require.Equal(t, time.UTC, inbox[0].CreatedAt.Location())
	})

	t.Run("replayed counter updates count once", func(t *testing.T) {
		provider, dataset := prepare(t)
		ctx := t.Context()
		lockedTenantSession(t, provider)
		updates := []struct {
			method string
			args   []any
			call   func(context.Context) error
		}{
			{"UpdateTokenCounts", []any{"tenant", "session", 1, 2}, func(ctx context.Context) error {
				return provider.UpdateTokenCounts(ctx, "tenant", "session", 1, 2)
			}},
			{"UpdateTokenCountsWithLock", []any{lockOwner("tenant", "session", "task-a", "uid-a"), 10, 20}, func(ctx context.Context) error {
				return provider.UpdateTokenCountsWithLock(ctx, lockOwner("tenant", "session", "task-a", "uid-a"), 10, 20)
			}},
			{"ReleaseLockWithTokenUsage", []any{lockOwner("tenant", "session", "task-a", "uid-a"), 100, 200}, func(ctx context.Context) error {
				return provider.ReleaseLockWithTokenUsage(ctx, lockOwner("tenant", "session", "task-a", "uid-a"), 100, 200)
			}},
		}
		for _, update := range updates {
			updateCtx := storagev1.WithWriteEnvelope(ctx, envelope(t, dataset, update.method, update.args...))
			require.NoErrorf(t, update.call(updateCtx), "%s", update.method)
			require.NoErrorf(t, update.call(updateCtx), "a retry of %s reports its success", update.method)
		}
		session, err := provider.GetSession(ctx, "tenant", "session")
		require.NoError(t, err)
		require.Equal(t, 111, session.InputTokens, "each update counted once")
		require.Equal(t, 222, session.OutputTokens)
		require.Empty(t, session.ActiveTask)
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

// inboxMessage returns a message to the inbox the retry cases read.
func inboxMessage(content string) *storagev1.Message {
	return &storagev1.Message{Namespace: "tenant", FromTask: "writer", ToTask: "reader", ParentTask: "parent", Content: content}
}

func sendInboxMessage(t *testing.T, provider storagev1.StateProvider, content string) {
	t.Helper()
	require.NoError(t, provider.SendMessage(t.Context(), inboxMessage(content)))
}

// inboxRequest consumes up to limit messages of the retry cases' inbox.
func inboxRequest(limit int) storagev1.ConsumeMessagesRequest {
	return storagev1.ConsumeMessagesRequest{Namespace: "tenant", TaskName: "reader", ParentTask: "parent", Limit: limit}
}
