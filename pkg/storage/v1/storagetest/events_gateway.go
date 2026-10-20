package storagetest

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

const (
	eventsTenant      = "tenant-a"
	eventsOtherTenant = "tenant-b"
	eventsSession     = "session-1"
)

func taskEvent(namespace, task, eventType string) *storagev1.ExecutionEvent {
	return &storagev1.ExecutionEvent{
		Namespace: namespace, StreamType: storagev1.ExecutionEventStreamTypeTask,
		StreamID: task, TaskName: task, Type: eventType,
	}
}

func appendEvent(t *testing.T, p storagev1.StateProvider, event *storagev1.ExecutionEvent) *storagev1.ExecutionEvent {
	t.Helper()
	appended, err := p.AppendExecutionEvent(t.Context(), event)
	require.NoError(t, err)
	require.NotNil(t, appended)
	return appended
}

func listEvents(t *testing.T, p storagev1.StateProvider, filter storagev1.ExecutionEventFilter) []storagev1.ExecutionEvent {
	t.Helper()
	listed, err := p.ListExecutionEvents(t.Context(), filter)
	require.NoError(t, err)
	return listed
}

func runExecutionEvents(t *testing.T, newProvider StateFactory) {
	t.Run("sequences are monotonic per stream and isolated by namespace", func(t *testing.T) {
		p := newProvider(t)
		first := appendEvent(t, p, taskEvent(eventsTenant, "task-a", storagev1.ExecutionEventTypeTaskCreated))
		succeeded := taskEvent(eventsTenant, "task-a", storagev1.ExecutionEventTypeTaskSucceeded)
		succeeded.Severity = "WARNING"
		second := appendEvent(t, p, succeeded)
		other := appendEvent(t, p, taskEvent(eventsTenant, "task-b", storagev1.ExecutionEventTypeTaskCreated))
		foreign := appendEvent(t, p, taskEvent(eventsOtherTenant, "task-a", storagev1.ExecutionEventTypeTaskCreated))
		require.Equal(t, []int64{1, 2, 1, 1}, []int64{first.Seq, second.Seq, other.Seq, foreign.Seq})
		require.Equal(t, storagev1.ExecutionEventSeverityWarning, second.Severity)
		require.NotEmpty(t, first.ID)
		require.NotEqual(t, first.ID, second.ID)
		require.NotEqual(t, first.ID, foreign.ID)

		latest, err := p.GetLatestExecutionEventSeq(t.Context(), eventsTenant, storagev1.ExecutionEventStreamTypeTask, "task-a")
		require.NoError(t, err)
		require.EqualValues(t, 2, latest)
		heads, err := p.GetLatestExecutionEventSeqs(t.Context(), eventsTenant, storagev1.ExecutionEventStreamTypeTask,
			[]string{"task-a", " task-b ", "missing"})
		require.NoError(t, err)
		require.Equal(t, map[string]int64{"task-a": 2, "task-b": 1, "missing": 0}, heads)

		after := listEvents(t, p, storagev1.ExecutionEventFilter{Namespace: eventsTenant, StreamID: "task-a", AfterSeq: 1})
		require.Len(t, after, 1)
		require.EqualValues(t, 2, after[0].Seq)
		require.Equal(t, storagev1.ExecutionEventTypeTaskSucceeded, after[0].Type)
		limited := listEvents(t, p, storagev1.ExecutionEventFilter{Namespace: eventsTenant, StreamID: "task-a", Limit: 1})
		require.Len(t, limited, 1)
		require.EqualValues(t, 1, limited[0].Seq)
		typed := listEvents(t, p, storagev1.ExecutionEventFilter{Namespace: eventsTenant, StreamID: "task-a",
			EventTypes: []string{storagev1.ExecutionEventTypeTaskCreated}})
		require.Len(t, typed, 1)
		require.Equal(t, first.ID, typed[0].ID)
		isolated := listEvents(t, p, storagev1.ExecutionEventFilter{Namespace: eventsOtherTenant, StreamID: "task-a"})
		require.Len(t, isolated, 1)
		require.Equal(t, foreign.ID, isolated[0].ID)
	})

	t.Run("payload fields round trip", func(t *testing.T) {
		p := newProvider(t)
		createdAt := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)
		event := taskEvent(eventsTenant, "task-payload", storagev1.ExecutionEventTypeToolCallCompleted)
		event.SessionName, event.AgentName, event.ToolName, event.ToolCallID = eventsSession, "agent", "file_read", "call-1"
		event.Summary, event.ContentText = "read a file", "file contents"
		event.Content = json.RawMessage(`{"path":"README.md","bytes":12}`)
		event.CreatedAt = createdAt
		appendEvent(t, p, event)
		listed := listEvents(t, p, storagev1.ExecutionEventFilter{Namespace: eventsTenant, StreamID: "task-payload"})
		require.Len(t, listed, 1)
		got := listed[0]
		require.Equal(t, []string{eventsSession, "agent", "file_read", "call-1", "read a file", "file contents"},
			[]string{got.SessionName, got.AgentName, got.ToolName, got.ToolCallID, got.Summary, got.ContentText})
		require.JSONEq(t, string(event.Content), string(got.Content))
		require.True(t, createdAt.Equal(got.CreatedAt), "created at %s, want %s", got.CreatedAt, createdAt)
	})

	t.Run("invalid events and filters are rejected", func(t *testing.T) {
		p := newProvider(t)
		for name, event := range map[string]*storagev1.ExecutionEvent{
			"nil":            nil,
			"unknown type":   taskEvent(eventsTenant, "task", "Nope"),
			"no namespace":   taskEvent("", "task", storagev1.ExecutionEventTypeTaskCreated),
			"no stream":      taskEvent(eventsTenant, "", storagev1.ExecutionEventTypeTaskCreated),
			"unknown stream": {Namespace: eventsTenant, StreamType: "pod", StreamID: "x", Type: storagev1.ExecutionEventTypeTaskCreated},
		} {
			_, err := p.AppendExecutionEvent(t.Context(), event)
			require.ErrorIs(t, err, storagev1.ErrValidation, name)
		}
		_, err := p.ListExecutionEvents(t.Context(), storagev1.ExecutionEventFilter{Namespace: eventsTenant, EventTypes: []string{"Nope"}})
		require.ErrorIs(t, err, storagev1.ErrValidation)
		_, _, err = p.ListSessionExecutionEvents(t.Context(), storagev1.SessionExecutionEventFilter{Namespace: eventsTenant})
		require.ErrorIs(t, err, storagev1.ErrValidation)
		_, _, err = p.AppendExecutionEventIfAbsent(t.Context(), taskEvent(eventsTenant, "task", storagev1.ExecutionEventTypeTaskCreated), " ")
		require.ErrorIs(t, err, storagev1.ErrValidation)
	})

	t.Run("deduplication keys are scoped to one stream", func(t *testing.T) {
		p := newProvider(t)
		event := taskEvent(eventsTenant, "task-dedupe", storagev1.ExecutionEventTypeTaskStarted)
		first, appended, err := p.AppendExecutionEventIfAbsent(t.Context(), event, "event-key")
		require.NoError(t, err)
		require.True(t, appended)
		changed := *event
		changed.Summary = "replayed with different content"
		replay, appended, err := p.AppendExecutionEventIfAbsent(t.Context(), &changed, "event-key")
		require.NoError(t, err)
		require.False(t, appended)
		require.Equal(t, first.ID, replay.ID)
		require.Equal(t, first.Seq, replay.Seq)
		_, appended, err = p.AppendExecutionEventIfAbsent(t.Context(),
			taskEvent(eventsTenant, "task-other", storagev1.ExecutionEventTypeTaskStarted), "event-key")
		require.NoError(t, err)
		require.True(t, appended)
		require.Len(t, listEvents(t, p, storagev1.ExecutionEventFilter{Namespace: eventsTenant, StreamID: "task-dedupe"}), 1)
	})

	t.Run("event and plan commit together", func(t *testing.T) {
		p := newProvider(t)
		event := taskEvent(eventsTenant, "task-plan", storagev1.ExecutionEventTypePlanUpdated)
		plan := &storagev1.PlanState{Namespace: eventsTenant, TaskName: "task-plan", Iteration: 1, Summary: "first"}
		first, appended, err := p.AppendExecutionEventWithPlanIfAbsent(t.Context(), event, "plan-1", plan)
		require.NoError(t, err)
		require.True(t, appended)
		stored, err := p.GetPlan(t.Context(), eventsTenant, "task-plan")
		require.NoError(t, err)
		require.Equal(t, "first", stored.Summary)

		replayPlan := *plan
		replayPlan.Summary = "second"
		replay, appended, err := p.AppendExecutionEventWithPlanIfAbsent(t.Context(), event, "plan-1", &replayPlan)
		require.NoError(t, err)
		require.False(t, appended)
		require.Equal(t, first.ID, replay.ID)
		stored, err = p.GetPlan(t.Context(), eventsTenant, "task-plan")
		require.NoError(t, err)
		require.Equal(t, "first", stored.Summary, "a replayed event must not change the plan")

		_, _, err = p.AppendExecutionEventWithPlanIfAbsent(t.Context(), event, "plan-2", nil)
		require.ErrorIs(t, err, storagev1.ErrValidation)
		foreign := &storagev1.PlanState{Namespace: eventsTenant, TaskName: "another-task", Summary: "foreign"}
		_, _, err = p.AppendExecutionEventWithPlanIfAbsent(t.Context(), event, "plan-3", foreign)
		require.ErrorIs(t, err, storagev1.ErrValidation)
		latest, err := p.GetLatestExecutionEventSeq(t.Context(), eventsTenant, storagev1.ExecutionEventStreamTypeTask, "task-plan")
		require.NoError(t, err)
		require.EqualValues(t, 1, latest, "rejected grouped writes must not append an event")
	})

	t.Run("terminal approval decisions are unique", func(t *testing.T) {
		p := newProvider(t)
		approved := taskEvent(eventsTenant, "task-approval", storagev1.ExecutionEventTypeApprovalApproved)
		approved.Content = json.RawMessage(`{"approvalID":"approval-1"}`)
		appendEvent(t, p, approved)
		declined := *approved
		declined.Type = storagev1.ExecutionEventTypeApprovalDeclined
		_, err := p.AppendExecutionEvent(t.Context(), &declined)
		require.ErrorIs(t, err, storagev1.ErrConflict)
		other := *approved
		other.Content = json.RawMessage(`{"approvalID":"approval-2"}`)
		appendEvent(t, p, &other)
	})

	t.Run("session aggregation keeps a stable cursor", func(t *testing.T) {
		p := newProvider(t)
		for _, item := range []struct{ task, eventType string }{
			{"task-a", storagev1.ExecutionEventTypeTaskStarted},
			{"task-b", storagev1.ExecutionEventTypeWorkerStarted},
			{"task-a", storagev1.ExecutionEventTypeTaskSucceeded},
		} {
			event := taskEvent(eventsTenant, item.task, item.eventType)
			event.SessionName = eventsSession
			appendEvent(t, p, event)
		}
		filter := storagev1.SessionExecutionEventFilter{Namespace: eventsTenant, SessionName: eventsSession, AfterSeq: 1}
		listed, latest, err := p.ListSessionExecutionEvents(t.Context(), filter)
		require.NoError(t, err)
		require.EqualValues(t, 3, latest)
		require.Len(t, listed, 2)
		require.Equal(t, "task-b", listed[0].TaskName)
		require.EqualValues(t, 2, listed[0].SessionSeq)
		require.EqualValues(t, 1, listed[0].TaskSeq)

		typed, latest, err := p.ListSessionExecutionEvents(t.Context(), storagev1.SessionExecutionEventFilter{
			Namespace: eventsTenant, SessionName: eventsSession, EventTypes: []string{storagev1.ExecutionEventTypeWorkerStarted},
		})
		require.NoError(t, err)
		require.EqualValues(t, 3, latest)
		require.Len(t, typed, 1)
		require.EqualValues(t, 2, typed[0].SessionSeq, "type filters must keep the session sequence")

		require.NoError(t, p.DeleteExecutionEvents(t.Context(), eventsTenant, storagev1.ExecutionEventStreamTypeTask, "task-a"))
		later := taskEvent(eventsTenant, "task-c", storagev1.ExecutionEventTypeTaskSucceeded)
		later.SessionName = eventsSession
		appendEvent(t, p, later)
		listed, latest, err = p.ListSessionExecutionEvents(t.Context(), storagev1.SessionExecutionEventFilter{
			Namespace: eventsTenant, SessionName: eventsSession, AfterSeq: 3,
		})
		require.NoError(t, err)
		require.EqualValues(t, 4, latest, "deleting a Task stream must not reuse session sequences")
		require.Len(t, listed, 1)
		require.Equal(t, "task-c", listed[0].TaskName)
		require.EqualValues(t, 4, listed[0].SessionSeq)
	})

	t.Run("deletion removes exactly one stream", func(t *testing.T) {
		p := newProvider(t)
		appendEvent(t, p, taskEvent(eventsTenant, "task-a", storagev1.ExecutionEventTypeTaskCreated))
		appendEvent(t, p, taskEvent(eventsTenant, "task-b", storagev1.ExecutionEventTypeTaskCreated))
		appendEvent(t, p, taskEvent(eventsOtherTenant, "task-a", storagev1.ExecutionEventTypeTaskCreated))
		require.NoError(t, p.DeleteExecutionEvents(t.Context(), eventsTenant, storagev1.ExecutionEventStreamTypeTask, "task-a"))
		latest, err := p.GetLatestExecutionEventSeq(t.Context(), eventsTenant, storagev1.ExecutionEventStreamTypeTask, "task-a")
		require.NoError(t, err)
		require.Zero(t, latest)
		require.Empty(t, listEvents(t, p, storagev1.ExecutionEventFilter{Namespace: eventsTenant, StreamID: "task-a"}))
		require.Len(t, listEvents(t, p, storagev1.ExecutionEventFilter{Namespace: eventsTenant, StreamID: "task-b"}), 1)
		require.Len(t, listEvents(t, p, storagev1.ExecutionEventFilter{Namespace: eventsOtherTenant, StreamID: "task-a"}), 1)
	})
}

func gatewayEvent(now time.Time, namespace, suffix string) storagev1.GatewayEvent {
	return storagev1.GatewayEvent{
		ID: "gev-" + suffix, Namespace: namespace, NamespaceUID: namespace + "-uid",
		GatewayUID: "gateway-uid", GatewayGeneration: 1, GatewayName: "chat",
		BindingName: "room", BindingUID: "binding-uid",
		ExternalEventID: "external-" + suffix, ProtocolVersion: "orka.gateway.v1", EventType: "text",
		AccountID: "acct", ContextID: "context", SenderID: "sender", Text: "hello " + suffix,
		ReplyTarget: "context", SessionName: "gateway-session-" + suffix, TaskName: "gateway-task-" + suffix,
		ReceivedAt: now, NextAttemptAt: now, ExpiresAt: now.Add(24 * time.Hour), CreatedAt: now, UpdatedAt: now,
	}
}

func admitGateway(t *testing.T, p storagev1.StateProvider, event storagev1.GatewayEvent) *storagev1.GatewayEvent {
	t.Helper()
	admitted, created, err := p.AdmitGatewayEvent(t.Context(), storagev1.GatewayEventAdmission{
		Event: event, AppendUserMessage: true, PendingLimit: 100,
	})
	require.NoError(t, err)
	require.True(t, created)
	return admitted
}

// linkGatewayTask admits, claims and links one event to a created Task.
func linkGatewayTask(t *testing.T, p storagev1.StateProvider, event storagev1.GatewayEvent, taskUID string) storagev1.GatewayEvent {
	t.Helper()
	admitGateway(t, p, event)
	claimed, err := p.ClaimNextGatewayEvent(t.Context(), event.Namespace, "dispatcher", event.CreatedAt, time.Minute)
	require.NoError(t, err)
	require.Equal(t, event.ID, claimed.ID)
	require.NoError(t, p.MarkGatewayEventTaskCreated(t.Context(), event.Namespace, event.ID, event.TaskName, taskUID, "dispatcher", event.CreatedAt))
	linked, err := p.GetGatewayEvent(t.Context(), event.Namespace, event.ID)
	require.NoError(t, err)
	return *linked
}

func gatewayDelivery(event storagev1.GatewayEvent, id, kind string, now time.Time) storagev1.GatewayDelivery {
	return storagev1.GatewayDelivery{
		ID: id, IdempotencyID: id, Namespace: event.Namespace, NamespaceUID: event.NamespaceUID,
		GatewayUID: event.GatewayUID, GatewayGeneration: event.GatewayGeneration, GatewayName: event.GatewayName,
		BindingName: event.BindingName, EventID: event.ID, TaskName: event.TaskName, SessionName: event.SessionName,
		Kind: kind, AccountID: event.AccountID, ContextID: event.ContextID, ReplyTarget: event.ReplyTarget,
		Text: "done", MaxAttempts: 10, NextAttemptAt: now, ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now,
	}
}

// requireRejected accepts either refusal a provider may use for a stale owner.
func requireRejected(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	require.True(t, errors.Is(err, storagev1.ErrConflict) || errors.Is(err, storagev1.ErrNotFound), "error = %v", err)
}

func runGatewayEvents(t *testing.T, newProvider StateFactory) {
	now := time.Now().UTC().Truncate(time.Second)

	t.Run("admission deduplicates the event and its transcript message", func(t *testing.T) {
		p := newProvider(t)
		event := gatewayEvent(now, eventsTenant, "dedupe")
		admitted := admitGateway(t, p, event)
		require.Equal(t, storagev1.GatewayEventQueued, admitted.State)
		replay, created, err := p.AdmitGatewayEvent(t.Context(), storagev1.GatewayEventAdmission{Event: event, AppendUserMessage: true, PendingLimit: 100})
		require.NoError(t, err)
		require.False(t, created)
		require.Equal(t, event.ID, replay.ID)
		session, err := p.GetSession(t.Context(), event.Namespace, event.SessionName)
		require.NoError(t, err)
		require.Len(t, session.Messages, 1)
		require.Equal(t, storagev1.GatewayUserMessageID(event.ID), session.Messages[0].ID)

		duplicate, err := p.GetGatewayEventDuplicate(t.Context(), &event, now)
		require.NoError(t, err)
		require.Equal(t, event.ID, duplicate.ID)
		changed := event
		changed.Text = "changed envelope"
		_, _, err = p.AdmitGatewayEvent(t.Context(), storagev1.GatewayEventAdmission{Event: changed, AppendUserMessage: true, PendingLimit: 100})
		require.ErrorIs(t, err, storagev1.ErrDuplicateMismatch)
		changed.ID += "-other"
		_, _, err = p.AdmitGatewayEvent(t.Context(), storagev1.GatewayEventAdmission{Event: changed, AppendUserMessage: true, PendingLimit: 100})
		require.ErrorIs(t, err, storagev1.ErrDuplicateMismatch)
		_, err = p.GetGatewayEventDuplicate(t.Context(), &changed, now)
		require.ErrorIs(t, err, storagev1.ErrDuplicateMismatch)
		unseen := gatewayEvent(now, eventsTenant, "unseen")
		_, err = p.GetGatewayEventDuplicate(t.Context(), &unseen, now)
		require.ErrorIs(t, err, storagev1.ErrNotFound)
		_, err = p.GetGatewayEvent(t.Context(), eventsOtherTenant, event.ID)
		require.ErrorIs(t, err, storagev1.ErrNotFound)
	})

	t.Run("pending limit dead-letters without growing the transcript", func(t *testing.T) {
		p := newProvider(t)
		first := gatewayEvent(now, eventsTenant, "first")
		second := gatewayEvent(now.Add(time.Second), eventsTenant, "second")
		second.SessionName = first.SessionName
		_, _, err := p.AdmitGatewayEvent(t.Context(), storagev1.GatewayEventAdmission{Event: first, AppendUserMessage: true, PendingLimit: 1})
		require.NoError(t, err)
		got, created, err := p.AdmitGatewayEvent(t.Context(), storagev1.GatewayEventAdmission{Event: second, AppendUserMessage: true, PendingLimit: 1})
		require.NoError(t, err)
		require.True(t, created)
		require.Equal(t, storagev1.GatewayEventDeadLettered, got.State)
		session, err := p.GetSession(t.Context(), first.Namespace, first.SessionName)
		require.NoError(t, err)
		require.Len(t, session.Messages, 1)
	})

	t.Run("claims are leased and fenced by owner", func(t *testing.T) {
		p := newProvider(t)
		event := gatewayEvent(now, eventsTenant, "claim")
		admitGateway(t, p, event)
		claimed, err := p.ClaimNextGatewayEvent(t.Context(), event.Namespace, "owner-a", now, time.Second)
		require.NoError(t, err)
		require.Equal(t, event.ID, claimed.ID)
		require.Equal(t, storagev1.GatewayEventDispatching, claimed.State)
		_, err = p.ClaimNextGatewayEvent(t.Context(), event.Namespace, "owner-b", now, time.Minute)
		require.ErrorIs(t, err, storagev1.ErrNotFound, "a leased event must not be claimed twice")
		_, err = p.RenewGatewayEventClaim(t.Context(), event.Namespace, event.ID, "owner-a", now, time.Second)
		require.NoError(t, err)

		reclaimed, err := p.ClaimNextGatewayEvent(t.Context(), event.Namespace, "owner-b", now.Add(time.Minute), time.Minute)
		require.NoError(t, err)
		require.Equal(t, event.ID, reclaimed.ID)
		require.Equal(t, 2, reclaimed.AttemptCount)
		_, err = p.RenewGatewayEventClaim(t.Context(), event.Namespace, event.ID, "owner-a", now.Add(time.Minute), time.Minute)
		require.ErrorIs(t, err, storagev1.ErrConflict)
		require.ErrorIs(t, p.RetryGatewayEvent(t.Context(), event.Namespace, event.ID, "owner-a", "stale", now), storagev1.ErrConflict)
		_, err = p.FreezeGatewayEventTaskRuntimeAllowedTools(t.Context(), event.Namespace, event.ID, "owner-a", []string{"tool"}, now)
		require.ErrorIs(t, err, storagev1.ErrConflict)
		frozen, err := p.FreezeGatewayEventTaskRuntimeAllowedTools(t.Context(), event.Namespace, event.ID, "owner-b", []string{"tool"}, now)
		require.NoError(t, err)
		require.True(t, frozen.TaskPolicyFrozen)
		require.Equal(t, []string{"tool"}, frozen.TaskAllowedTools)
		_, err = p.FreezeGatewayEventTaskRuntimeAllowedTools(t.Context(), event.Namespace, event.ID, "owner-b", []string{"other"}, now)
		require.ErrorIs(t, err, storagev1.ErrConflict, "the first frozen tool policy wins")

		require.NoError(t, p.MarkGatewayEventTaskCreated(t.Context(), event.Namespace, event.ID, event.TaskName, "task-uid", "owner-b", now))
		linked, err := p.GetGatewayEventForTask(t.Context(), event.Namespace, event.TaskName, "task-uid")
		require.NoError(t, err)
		require.Equal(t, event.ID, linked.ID)
		require.Equal(t, storagev1.GatewayEventTaskCreated, linked.State)
		_, err = p.GetGatewayEventForTask(t.Context(), event.Namespace, event.TaskName, "replacement-uid")
		require.ErrorIs(t, err, storagev1.ErrNotFound)
		_, err = p.GetGatewayEventForTask(t.Context(), event.Namespace, event.TaskName, "")
		require.ErrorIs(t, err, storagev1.ErrValidation)
		tombstoned, err := p.HasGatewayTaskTombstone(t.Context(), event.Namespace, event.TaskName, "task-uid")
		require.NoError(t, err)
		require.False(t, tombstoned)
	})

	t.Run("namespace-scoped claims stay in their namespace", func(t *testing.T) {
		p := newProvider(t)
		admitGateway(t, p, gatewayEvent(now, eventsTenant, "scope-a"))
		other := gatewayEvent(now.Add(time.Second), eventsOtherTenant, "scope-b")
		admitGateway(t, p, other)
		claimed, err := p.ClaimNextGatewayEvent(t.Context(), eventsOtherTenant, "worker", now.Add(2*time.Second), time.Minute)
		require.NoError(t, err)
		require.Equal(t, other.ID, claimed.ID)
		require.Equal(t, eventsOtherTenant, claimed.Namespace)
		stats, err := p.GetGatewayQueueStats(t.Context(), eventsTenant)
		require.NoError(t, err)
		require.Equal(t, 1, stats.PendingEvents)
	})

	t.Run("terminal projection completes the event once", func(t *testing.T) {
		p := newProvider(t)
		event := linkGatewayTask(t, p, gatewayEvent(now, eventsTenant, "terminal"), "task-uid")
		projection := storagev1.GatewayTerminalProjection{
			EventID: event.ID, CompletedAt: now,
			Message: storagev1.SessionMessage{
				ID: storagev1.GatewayAssistantMessageID(event.ID), Role: "assistant", Content: "done",
				SourceType: "gateway-task", SourceRef: event.TaskName, Timestamp: now,
			},
			Delivery: gatewayDelivery(event, "gdl-terminal", "final", now),
		}
		delivery, created, err := p.ProjectGatewayTerminal(t.Context(), projection)
		require.NoError(t, err)
		require.True(t, created)
		require.Equal(t, "gdl-terminal", delivery.ID)
		_, created, err = p.ProjectGatewayTerminal(t.Context(), projection)
		require.NoError(t, err)
		require.False(t, created)
		completed, err := p.GetGatewayEvent(t.Context(), event.Namespace, event.ID)
		require.NoError(t, err)
		require.Equal(t, storagev1.GatewayEventCompleted, completed.State)
		session, err := p.GetSession(t.Context(), event.Namespace, event.SessionName)
		require.NoError(t, err)
		require.Len(t, session.Messages, 2)
		require.Empty(t, session.ActiveTask)
	})
}

func runGatewayDeliveries(t *testing.T, newProvider StateFactory) {
	now := time.Now().UTC().Truncate(time.Second)

	t.Run("delivery claims retry and record one receipt", func(t *testing.T) {
		p := newProvider(t)
		delivery := gatewayDelivery(gatewayEvent(now, eventsTenant, "delivery"), "gdl-delivery", "final", now)
		_, created, err := p.CreateGatewayDelivery(t.Context(), &delivery)
		require.NoError(t, err)
		require.True(t, created)
		replay := delivery
		_, created, err = p.CreateGatewayDelivery(t.Context(), &replay)
		require.NoError(t, err)
		require.False(t, created)
		reused := delivery
		reused.ID, reused.EventID, reused.Text = "gdl-different", "other-event", "different"
		_, _, err = p.CreateGatewayDelivery(t.Context(), &reused)
		require.ErrorIs(t, err, storagev1.ErrDuplicateMismatch)
		_, err = p.GetGatewayDelivery(t.Context(), delivery.Namespace, reused.ID)
		require.ErrorIs(t, err, storagev1.ErrNotFound)

		claimed, err := p.ClaimNextGatewayDelivery(t.Context(), delivery.Namespace, "sender-a", now, time.Minute)
		require.NoError(t, err)
		require.Equal(t, delivery.ID, claimed.ID)
		require.Equal(t, 1, claimed.AttemptCount)
		require.NoError(t, p.ScheduleGatewayDeliveryRetry(t.Context(), delivery.Namespace, delivery.ID, "sender-a", "temporary", now.Add(time.Second)))
		_, err = p.ClaimNextGatewayDelivery(t.Context(), delivery.Namespace, "sender-b", now, time.Minute)
		require.ErrorIs(t, err, storagev1.ErrNotFound, "a scheduled retry is not due yet")
		claimed, err = p.ClaimNextGatewayDelivery(t.Context(), delivery.Namespace, "sender-b", now.Add(time.Second), time.Minute)
		require.NoError(t, err)
		require.Equal(t, 2, claimed.AttemptCount)
		requireRejected(t, p.MarkGatewayDeliveryDelivered(t.Context(), delivery.Namespace, delivery.ID, "sender-a", "stale", now))
		require.NoError(t, p.MarkGatewayDeliveryDelivered(t.Context(), delivery.Namespace, delivery.ID, "sender-b", "provider-1", now))
		require.NoError(t, p.MarkGatewayDeliveryDelivered(t.Context(), delivery.Namespace, delivery.ID, "sender-b", "provider-1", now))
		require.ErrorIs(t, p.MarkGatewayDeliveryDelivered(t.Context(), delivery.Namespace, delivery.ID, "sender-b", "provider-2", now), storagev1.ErrConflict)
		stored, err := p.GetGatewayDelivery(t.Context(), delivery.Namespace, delivery.ID)
		require.NoError(t, err)
		require.Equal(t, storagev1.GatewayDeliveryDelivered, stored.State)
		require.Equal(t, "provider-1", stored.ProviderMessageID)

		listed, err := p.ListGatewayDeliveries(t.Context(), storagev1.GatewayDeliveryFilter{Namespace: delivery.Namespace, EventID: delivery.EventID})
		require.NoError(t, err)
		require.Len(t, listed, 1)
		foreign, err := p.ListGatewayDeliveries(t.Context(), storagev1.GatewayDeliveryFilter{Namespace: eventsOtherTenant})
		require.NoError(t, err)
		require.Empty(t, foreign)
		_, err = p.ListGatewayDeliveries(t.Context(), storagev1.GatewayDeliveryFilter{})
		require.ErrorIs(t, err, storagev1.ErrValidation)
	})

	t.Run("terminal failures can be retried manually", func(t *testing.T) {
		p := newProvider(t)
		delivery := gatewayDelivery(gatewayEvent(now, eventsTenant, "failed"), "gdl-failed", "final", now)
		_, _, err := p.CreateGatewayDelivery(t.Context(), &delivery)
		require.NoError(t, err)
		_, err = p.ClaimNextGatewayDelivery(t.Context(), delivery.Namespace, "sender", now, time.Minute)
		require.NoError(t, err)
		require.ErrorIs(t, p.MarkGatewayDeliveryTerminal(t.Context(), delivery.Namespace, delivery.ID, "sender",
			storagev1.GatewayDeliveryDelivered, "not terminal", now), storagev1.ErrValidation)
		require.NoError(t, p.MarkGatewayDeliveryTerminal(t.Context(), delivery.Namespace, delivery.ID, "sender",
			storagev1.GatewayDeliveryFailed, "permanent", now))
		retried, err := p.RetryGatewayDelivery(t.Context(), delivery.Namespace, delivery.ID, now.Add(time.Minute), now.Add(time.Hour))
		require.NoError(t, err)
		require.Equal(t, storagev1.GatewayDeliveryPending, retried.State)
		require.Equal(t, 1, retried.ManualRetryCount)
		require.Zero(t, retried.AttemptCount)
		_, err = p.RetryGatewayDelivery(t.Context(), delivery.Namespace, delivery.ID, now.Add(time.Minute), now.Add(time.Hour))
		require.ErrorIs(t, err, storagev1.ErrConflict, "only failed or dead-lettered deliveries can be retried")
		_, err = p.RetryGatewayDelivery(t.Context(), delivery.Namespace, "missing", now, now.Add(time.Hour))
		require.ErrorIs(t, err, storagev1.ErrNotFound)
	})

	t.Run("interim messages are budgeted and replay safely", func(t *testing.T) {
		p := newProvider(t)
		event := linkGatewayTask(t, p, gatewayEvent(now, eventsTenant, "messages"), "task-uid")
		query := storagev1.GatewayMessageBudgetQuery{Namespace: event.Namespace, NamespaceUID: event.NamespaceUID,
			EventID: event.ID, TaskName: event.TaskName, TaskUID: event.TaskUID, RequestID: "operation-1"}
		budget, err := p.GetGatewayMessageBudget(t.Context(), query)
		require.NoError(t, err)
		require.Equal(t, storagev1.GatewayMessageBudget{}, *budget)

		request := storagev1.GatewayMessageEnqueue{Namespace: event.Namespace, NamespaceUID: event.NamespaceUID,
			EventID: event.ID, TaskName: event.TaskName, TaskUID: event.TaskUID, RequestID: "operation-1",
			Text: "working", MaxMessages: 2, MaxAttempts: 3, Now: now, ExpiresAt: event.ExpiresAt}
		miss := request
		miss.ReplayOnly = true
		_, created, err := p.EnqueueGatewayMessage(t.Context(), miss)
		require.ErrorIs(t, err, storagev1.ErrGatewayMessageReplayOnly)
		require.False(t, created)
		delivery, created, err := p.EnqueueGatewayMessage(t.Context(), request)
		require.NoError(t, err)
		require.True(t, created)
		require.Equal(t, "message", delivery.Kind)
		require.Equal(t, storagev1.GatewayDeliveryPending, delivery.State)
		require.Equal(t, "working", delivery.Text)

		replay := request
		replay.Now, replay.MaxMessages = now.Add(time.Second), 1
		again, created, err := p.EnqueueGatewayMessage(t.Context(), replay)
		require.NoError(t, err)
		require.False(t, created)
		require.Equal(t, delivery.ID, again.ID)
		replayOnly := request
		replayOnly.ReplayOnly = true
		again, created, err = p.EnqueueGatewayMessage(t.Context(), replayOnly)
		require.NoError(t, err)
		require.False(t, created)
		require.Equal(t, delivery.ID, again.ID)
		replay.Text = "different"
		_, _, err = p.EnqueueGatewayMessage(t.Context(), replay)
		require.ErrorIs(t, err, storagev1.ErrDuplicateMismatch)

		budget, err = p.GetGatewayMessageBudget(t.Context(), query)
		require.NoError(t, err)
		require.Equal(t, storagev1.GatewayMessageBudget{Accepted: 1, RequestExists: true}, *budget)
		second := request
		second.RequestID = "operation-2"
		_, created, err = p.EnqueueGatewayMessage(t.Context(), second)
		require.NoError(t, err)
		require.True(t, created)
		third := request
		third.RequestID = "operation-3"
		_, _, err = p.EnqueueGatewayMessage(t.Context(), third)
		require.ErrorIs(t, err, storagev1.ErrCapacity)
		wrongTask := query
		wrongTask.TaskUID = "other-uid"
		_, err = p.GetGatewayMessageBudget(t.Context(), wrongTask)
		require.ErrorIs(t, err, storagev1.ErrNotFound, "a budget belongs to one Task")
	})
}

func runGatewayCleanup(t *testing.T, newProvider StateFactory) {
	t.Run("task cleanup receipts follow retention compaction", func(t *testing.T) {
		p := newProvider(t)
		now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
		cutoff := now.Add(-time.Hour)
		event := linkGatewayTask(t, p, gatewayEvent(now.Add(-2*time.Hour), eventsTenant, "retention"), "task-uid-retention")
		_, err := p.GetGatewayTaskCleanupReceipt(t.Context(), event.Namespace, event.TaskName, event.TaskUID)
		require.ErrorIs(t, err, storagev1.ErrNotFound)

		delivered := gatewayDelivery(event, "gdl-retention", "error", cutoff)
		delivered.State, delivered.ProviderMessageID, delivered.DeliveredAt = storagev1.GatewayDeliveryDelivered, "provider", &cutoff
		_, _, err = p.ExpireGatewayEventWithDelivery(t.Context(), storagev1.GatewayExpiryProjection{
			EventID: event.ID, Reason: "expired", CompletedAt: cutoff, Delivery: delivered,
		})
		require.NoError(t, err)
		result, err := p.MaintainGatewayRecords(t.Context(), event.Namespace, now, cutoff)
		require.NoError(t, err)
		require.Zero(t, result.DeletedEvents, "the retention cutoff is exclusive")
		result, err = p.MaintainGatewayRecords(t.Context(), eventsOtherTenant, now.Add(time.Second), cutoff.Add(time.Second))
		require.NoError(t, err)
		require.Zero(t, result.DeletedEvents, "maintenance must stay in its namespace")

		compactedAt := now.Add(time.Second)
		result, err = p.MaintainGatewayRecords(t.Context(), event.Namespace, compactedAt, cutoff.Add(time.Second))
		require.NoError(t, err)
		require.Equal(t, 1, result.DeletedEvents)
		receipt, err := p.GetGatewayTaskCleanupReceipt(t.Context(), event.Namespace, event.TaskName, event.TaskUID)
		require.NoError(t, err)
		require.NoError(t, receipt.Validate(event.Namespace, event.TaskName, event.TaskUID))
		require.Equal(t, event.ID, receipt.EventID)
		require.Equal(t, event.SessionName, receipt.SessionName)
		require.True(t, compactedAt.Equal(receipt.CompactedAt))
		tombstoned, err := p.HasGatewayTaskTombstone(t.Context(), event.Namespace, event.TaskName, event.TaskUID)
		require.NoError(t, err)
		require.True(t, tombstoned)
		_, err = p.GetGatewayEvent(t.Context(), event.Namespace, event.ID)
		require.ErrorIs(t, err, storagev1.ErrNotFound)

		for name, read := range map[string]struct {
			namespace, task, uid string
			want                 error
		}{
			"another namespace": {eventsOtherTenant, event.TaskName, event.TaskUID, storagev1.ErrNotFound},
			"replaced Task":     {event.Namespace, event.TaskName, "replacement-uid", storagev1.ErrNotFound},
			"wrong Task name":   {event.Namespace, "different-task", event.TaskUID, storagev1.ErrConflict},
			"missing Task UID":  {event.Namespace, event.TaskName, "", storagev1.ErrValidation},
		} {
			_, err := p.GetGatewayTaskCleanupReceipt(t.Context(), read.namespace, read.task, read.uid)
			require.ErrorIs(t, err, read.want, name)
		}

		listed, err := p.ListGatewayTaskCleanupReceipts(t.Context(), storagev1.GatewayTaskCleanupReceiptFilter{Limit: 10})
		require.NoError(t, err)
		require.Len(t, listed, 1)
		scoped, err := p.ListGatewayTaskCleanupReceipts(t.Context(), storagev1.GatewayTaskCleanupReceiptFilter{Namespace: eventsOtherTenant, Limit: 10})
		require.NoError(t, err)
		require.Empty(t, scoped)
		_, err = p.ListGatewayTaskCleanupReceipts(t.Context(), storagev1.GatewayTaskCleanupReceiptFilter{Limit: 0})
		require.ErrorIs(t, err, storagev1.ErrValidation)
		require.NoError(t, p.DeleteGatewayTaskCleanupReceipt(t.Context(), event.Namespace, event.TaskName, event.TaskUID))
		_, err = p.GetGatewayTaskCleanupReceipt(t.Context(), event.Namespace, event.TaskName, event.TaskUID)
		require.ErrorIs(t, err, storagev1.ErrNotFound)
		require.ErrorIs(t, p.DeleteGatewayTaskCleanupReceipt(t.Context(), "", event.TaskName, event.TaskUID), storagev1.ErrValidation)
	})

	t.Run("session cleanup candidates are bounded and exclude active transcripts", func(t *testing.T) {
		p := newProvider(t)
		for _, filter := range []storagev1.GatewaySessionCleanupFilter{
			{Limit: 0}, {Limit: 101}, {Limit: 1, Namespace: " tenant"},
			{Limit: 1, AfterNamespace: eventsTenant}, {Limit: 1, AfterSessionName: "session"},
		} {
			page, err := p.ListGatewaySessionCleanupCandidates(t.Context(), filter)
			require.ErrorIs(t, err, storagev1.ErrValidation)
			require.Equal(t, storagev1.GatewaySessionCleanupPage{}, page, "invalid filters must not report progress")
		}
		now := time.Now().UTC().Truncate(time.Second)
		admitGateway(t, p, gatewayEvent(now.Add(-time.Hour), eventsTenant, "active"))
		page, err := p.ListGatewaySessionCleanupCandidates(t.Context(), storagev1.GatewaySessionCleanupFilter{
			TerminalCutoff: now.Add(time.Hour), Limit: 10,
		})
		require.NoError(t, err)
		require.Empty(t, page.Candidates, "a transcript with retained messages or events is not a candidate")
		require.True(t, page.Complete)
	})
}
