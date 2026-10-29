package storagetest

import (
	"errors"
	"testing"
	"time"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

// runGroupedWrites checks that a grouped write whose last part fails leaves
// no part visible. Each case makes the part a provider would naturally write
// last collide with a stored record, so a provider that writes the group in
// order and does not roll back the earlier parts fails the case.
func runGroupedWrites(t *testing.T, newProvider StateFactory) {
	t.Run("an event and plan whose usage observation conflicts leave nothing", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		at := time.Date(2026, 6, 12, 10, 0, 0, 0, time.UTC)
		const task = "task-grouped"
		usage := func(input int64) *storagev1.ExecutionEventUsage {
			return &storagev1.ExecutionEventUsage{Observation: storagev1.UsageObservation{
				ID: "observation-1", Namespace: eventsTenant, TaskUID: "task-uid", TaskName: task, CounterID: "counter",
				Scope: storagev1.UsageScopeCall, Source: storagev1.UsageSourceProvider, InputTokens: &input,
				Status: storagev1.UsageStatusCompleted, ObservedAt: at,
			}}
		}
		observations := func() []storagev1.UsageObservation {
			t.Helper()
			data, err := p.LoadUsage(ctx, storagev1.UsageFilter{Namespaces: []string{eventsTenant}, AsOf: at.Add(time.Hour)})
			require.NoError(t, err)
			return data.Observations
		}
		recorded := taskEvent(eventsTenant, task, storagev1.ExecutionEventTypeModelMessage)
		recorded.Usage = usage(5)
		appendEvent(t, p, recorded)
		before := &storagev1.PlanState{Namespace: eventsTenant, TaskName: task, Iteration: 1, Summary: "before"}
		require.NoError(t, p.SavePlan(ctx, eventsTenant, task, before))
		head, err := p.GetLatestExecutionEventSeq(ctx, eventsTenant, storagev1.ExecutionEventStreamTypeTask, task)
		require.NoError(t, err)

		grouped := taskEvent(eventsTenant, task, storagev1.ExecutionEventTypePlanUpdated)
		grouped.Usage = usage(6)
		after := &storagev1.PlanState{Namespace: eventsTenant, TaskName: task, Iteration: 2, Summary: "after"}
		_, _, err = p.AppendExecutionEventWithPlanIfAbsent(ctx, grouped, "grouped-1", after)
		require.ErrorIs(t, err, storagev1.ErrConflict, "the observation ID was reused with other counts")

		latest, err := p.GetLatestExecutionEventSeq(ctx, eventsTenant, storagev1.ExecutionEventStreamTypeTask, task)
		require.NoError(t, err)
		require.Equal(t, head, latest, "the event was appended")
		plan, err := p.GetPlan(ctx, eventsTenant, task)
		require.NoError(t, err)
		require.Equal(t, "before", plan.Summary, "the plan was changed")
		stored := observations()
		require.Len(t, stored, 1)
		require.EqualValues(t, 5, *stored[0].InputTokens, "the observation was changed")
		// The dedupe key was not consumed either.
		_, appended, err := p.AppendExecutionEventIfAbsent(ctx, taskEvent(eventsTenant, task, storagev1.ExecutionEventTypePlanUpdated), "grouped-1")
		require.NoError(t, err)
		require.True(t, appended, "the failed group kept its dedupe key")
	})

	t.Run("a chat turn whose last message is already stored leaves nothing", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		reservedChat(t, p)
		require.NoError(t, p.CommitSessionTurn(ctx, chatSession(), "turn-1", 0,
			[]storagev1.SessionMessage{{ID: "m1", Role: "user", Content: "hello"}}, 3, 4))
		require.NoError(t, p.ReleaseChatTurn(ctx, "tenant", "chat", "turn-1", false))
		created, err := p.AcquireChatTurn(ctx, chatSession(), "turn-2", writeCaseFuture)
		require.NoError(t, err)
		require.False(t, created)
		before, err := p.GetSession(ctx, "tenant", "chat")
		require.NoError(t, err)

		err = p.CommitSessionTurn(ctx, chatSession(), "turn-2", 1, []storagev1.SessionMessage{
			{ID: "m2", Role: "user", Content: "again"}, {ID: "m1", Role: "user", Content: "hello"},
		}, 5, 6)
		require.ErrorIs(t, err, storagev1.ErrConflict, "a message the transcript already holds")

		after, err := p.GetSession(ctx, "tenant", "chat")
		require.NoError(t, err)
		require.Equal(t, sessionMessageIDs(before.Messages), sessionMessageIDs(after.Messages), "a message was appended")
		require.Equal(t, before.MessageCount, after.MessageCount)
		require.Equal(t, before.InputTokens, after.InputTokens, "the token counters were changed")
		require.Equal(t, before.OutputTokens, after.OutputTokens, "the token counters were changed")
	})

	now := time.Now().UTC().Truncate(time.Second)
	// collidingDelivery stores a delivery for event under id whose text
	// differs from the one gatewayDelivery builds, as another operation that
	// reused the ID would.
	collidingDelivery := func(t *testing.T, p storagev1.StateProvider, event storagev1.GatewayEvent, id, kind string) storagev1.GatewayDelivery {
		t.Helper()
		other := gatewayDelivery(event, id, kind, now)
		other.Text = "a reply from another operation"
		_, created, err := p.CreateGatewayDelivery(t.Context(), &other)
		require.NoError(t, err)
		require.True(t, created)
		return other
	}
	requireReuseRefused := func(t *testing.T, err error) {
		t.Helper()
		require.Error(t, err)
		require.True(t, errors.Is(err, storagev1.ErrDuplicateMismatch) || errors.Is(err, storagev1.ErrConflict),
			"a reused delivery ID: %v", err)
	}
	// gatewayBefore reads the event and its Session before the grouped write.
	gatewayBefore := func(t *testing.T, p storagev1.StateProvider, event storagev1.GatewayEvent) (*storagev1.GatewayEvent, *storagev1.SessionRecord) {
		t.Helper()
		stored, err := p.GetGatewayEvent(t.Context(), event.Namespace, event.ID)
		require.NoError(t, err)
		require.Equal(t, storagev1.GatewayEventTaskCreated, stored.State)
		session, err := p.GetSession(t.Context(), event.Namespace, event.SessionName)
		require.NoError(t, err)
		return stored, session
	}
	requireGatewayUnchanged := func(t *testing.T, p storagev1.StateProvider, event *storagev1.GatewayEvent, session *storagev1.SessionRecord, delivery storagev1.GatewayDelivery) {
		t.Helper()
		ctx := t.Context()
		stored, err := p.GetGatewayEvent(ctx, event.Namespace, event.ID)
		require.NoError(t, err)
		require.Equal(t, event.State, stored.State, "the event state was changed")
		require.Equal(t, event.DeliveryID, stored.DeliveryID, "the event was linked to another delivery")
		require.Equal(t, event.CompletedAt, stored.CompletedAt, "the event was completed")
		after, err := p.GetSession(ctx, event.Namespace, event.SessionName)
		require.NoError(t, err)
		require.Equal(t, session.MessageCount, after.MessageCount, "the transcript count was changed")
		require.Equal(t, sessionMessageIDs(session.Messages), sessionMessageIDs(after.Messages), "a transcript message was appended")
		require.Equal(t, session.ActiveTask, after.ActiveTask, "the Session lock was released")
		kept, err := p.GetGatewayDelivery(ctx, delivery.Namespace, delivery.ID)
		require.NoError(t, err)
		require.Equal(t, delivery.Text, kept.Text, "the stored delivery was replaced")
	}

	t.Run("a terminal projection whose delivery ID collides leaves nothing", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		event := linkGatewayTask(t, p, gatewayEvent(now, eventsTenant, "grouped-terminal"), "task-uid")
		other := collidingDelivery(t, p, event, "gdl-grouped-terminal", "final")
		before, session := gatewayBefore(t, p, event)

		_, _, err := p.ProjectGatewayTerminal(ctx, storagev1.GatewayTerminalProjection{
			EventID: event.ID, CompletedAt: now,
			Message: storagev1.SessionMessage{
				ID: storagev1.GatewayAssistantMessageID(event.ID), Role: "assistant", Content: "done",
				SourceType: "gateway-task", SourceRef: event.TaskName, Timestamp: now,
			},
			Delivery: gatewayDelivery(event, other.ID, "final", now),
		})
		requireReuseRefused(t, err)
		requireGatewayUnchanged(t, p, before, session, other)
	})

	t.Run("an expiry whose delivery ID collides leaves nothing", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		event := linkGatewayTask(t, p, gatewayEvent(now, eventsTenant, "grouped-expiry"), "task-uid")
		other := collidingDelivery(t, p, event, "gdl-grouped-expiry", "error")
		before, session := gatewayBefore(t, p, event)

		_, _, err := p.ExpireGatewayEventWithDelivery(ctx, storagev1.GatewayExpiryProjection{
			EventID: event.ID, Reason: "expired", CompletedAt: now, Delivery: gatewayDelivery(event, other.ID, "error", now),
		})
		requireReuseRefused(t, err)
		requireGatewayUnchanged(t, p, before, session, other)
	})
}
