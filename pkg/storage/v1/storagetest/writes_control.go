package storagetest

import (
	"errors"
	"testing"
	"time"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

func cleanupIntent() storagev1.SessionCleanupIntent {
	return storagev1.SessionCleanupIntent{
		Namespace: "tenant", SessionName: "chat", OperationID: "delete-chat",
		OperationDigest: controlDigest("delete-chat"), PreparedAt: controlBaseTime,
	}
}

func cleanupCompletion() storagev1.CompleteSessionCleanupRequest {
	intent := cleanupIntent()
	return storagev1.CompleteSessionCleanupRequest{
		Namespace: intent.Namespace, SessionName: intent.SessionName,
		OperationID: intent.OperationID, OperationDigest: intent.OperationDigest,
	}
}

func cleanupSession(t *testing.T, p storagev1.StateProvider) {
	t.Helper()
	createControlSession(t, p, "tenant", "chat", "chat")
}

func observeCleanup(t *testing.T, p storagev1.StateProvider) any {
	t.Helper()
	ctx := t.Context()
	session, err := p.GetSession(ctx, "tenant", "chat")
	intent, intentErr := p.GetSessionCleanupIntent(ctx, "tenant", "chat")
	completion, completionErr := p.GetSessionCleanupCompletion(ctx, "tenant", "chat")
	identity, identityErr := p.GetSessionCleanupIdentity(ctx, "tenant", "chat")
	fenced, fencedErr := p.HasSessionCleanupFenceForUID(ctx, "session-uid")
	require.NoError(t, fencedErr)
	return []any{
		observed(t, session, err), observed(t, intent, intentErr), observed(t, completion, completionErr),
		observed(t, identity, identityErr), fenced,
	}
}

const outboxCaseID = "projection-1"

func enqueueCaseProjection(t *testing.T, p storagev1.StateProvider) {
	t.Helper()
	_, err := p.EnqueueOutboxProjectionRecord(t.Context(), outboxProjection(outboxCaseID, `{"n":1}`, controlBaseTime), suiteFence)
	require.NoError(t, err)
}

func claimCaseProjection(fence storagev1.ControllerEpochFence) storagev1.ClaimOutboxProjectionsRequest {
	return storagev1.ClaimOutboxProjectionsRequest{
		Fence: fence, WorkerID: "worker-a", Limit: 10, LeaseDuration: time.Minute, Now: controlBaseTime,
	}
}

func observeOutbox(t *testing.T, p storagev1.StateProvider) any {
	t.Helper()
	projection, err := p.GetOutboxProjection(t.Context(), outboxCaseID)
	backlog, backlogErr := p.CountUnsettledOutboxProjections(t.Context())
	require.NoError(t, backlogErr)
	return []any{observed(t, projection, err), backlog}
}

var caseJob = storagev1.TaskJobIdentity{Namespace: "tenant", TaskUID: "task-uid", JobUID: "job-1"}

func observeJobAuthority(t *testing.T, p storagev1.StateProvider) any {
	t.Helper()
	err := p.CheckTaskJobAuthority(t.Context(), caseJob)
	if errors.Is(err, storagev1.ErrTaskJobRevoked) {
		return "revoked"
	}
	require.NoError(t, err)
	return "authorized"
}

func caseEvent() *storagev1.ExecutionEvent {
	event := taskEvent("tenant", "task", storagev1.ExecutionEventTypePlanUpdated)
	event.Summary = "planned"
	event.CreatedAt = controlBaseTime
	return event
}

func observeEvents(t *testing.T, p storagev1.StateProvider) any {
	t.Helper()
	events := listEvents(t, p, storagev1.ExecutionEventFilter{
		Namespace: "tenant", StreamType: storagev1.ExecutionEventStreamTypeTask, StreamID: "task",
	})
	plan, err := p.GetPlan(t.Context(), "tenant", "task")
	return []any{events, observed(t, plan, err)}
}

func init() {
	stateWriteCases = append(stateWriteCases,
		writeCase[storagev1.StateProvider]{
			method:  "BindSessionCleanupIdentity",
			setup:   cleanupSession,
			args:    fixed("tenant", "chat", "session-uid"),
			observe: observeCleanup,
		},
		writeCase[storagev1.StateProvider]{
			method:  "PrepareSessionCleanup",
			setup:   cleanupSession,
			args:    func(storagev1.ControllerEpochFence) []any { return []any{cleanupIntent()} },
			observe: observeCleanup,
		},
		writeCase[storagev1.StateProvider]{
			method: "CompleteSessionCleanup",
			setup: func(t *testing.T, p storagev1.StateProvider) {
				cleanupSession(t, p)
				_, err := p.PrepareSessionCleanup(t.Context(), cleanupIntent())
				require.NoError(t, err)
			},
			args:    func(storagev1.ControllerEpochFence) []any { return []any{cleanupCompletion()} },
			observe: observeCleanup,
		},
		writeCase[storagev1.StateProvider]{
			method: "EnqueueOutboxProjectionRecord",
			args: func(fence storagev1.ControllerEpochFence) []any {
				return []any{outboxProjection(outboxCaseID, `{"n":1}`, controlBaseTime), fence}
			},
			explicitFence: true,
			observe:       observeOutbox,
		},
		writeCase[storagev1.StateProvider]{
			method:        "ClaimOutboxProjectionRecords",
			setup:         enqueueCaseProjection,
			args:          func(fence storagev1.ControllerEpochFence) []any { return []any{claimCaseProjection(fence)} },
			explicitFence: true,
			observe:       observeOutbox,
		},
		writeCase[storagev1.StateProvider]{
			method: "CompleteOutboxProjectionRecord",
			setup: func(t *testing.T, p storagev1.StateProvider) {
				enqueueCaseProjection(t, p)
				claimed, err := p.ClaimOutboxProjectionRecords(t.Context(), claimCaseProjection(suiteFence))
				require.NoError(t, err)
				require.Len(t, claimed, 1)
			},
			args: func(fence storagev1.ControllerEpochFence) []any {
				return []any{storagev1.CompleteOutboxProjectionRequest{
					ID: outboxCaseID, Fence: fence, ExpectedVersion: 2, LeaseOwner: "worker-a",
					OperationID: "deliver-projection", OperationDigest: controlDigest("deliver-projection"),
					NewState: storagev1.OutboxProjectionDelivered, DeliveryDigest: controlDigest("delivery"),
					UpdatedAt: controlBaseTime.Add(time.Second),
				}}
			},
			explicitFence: true,
			observe:       observeOutbox,
		},
		writeCase[storagev1.StateProvider]{
			method:  "RevokeTaskJob",
			args:    fixed(caseJob),
			observe: observeJobAuthority,
		},
		writeCase[storagev1.StateProvider]{
			method: "DeleteTaskJobRevocations",
			setup: func(t *testing.T, p storagev1.StateProvider) {
				require.NoError(t, p.RevokeTaskJob(t.Context(), caseJob))
			},
			args:    fixed("tenant", "task", "task-uid"),
			observe: observeJobAuthority,
		},
		writeCase[storagev1.StateProvider]{
			method:  "AppendExecutionEvent",
			args:    func(storagev1.ControllerEpochFence) []any { return []any{caseEvent()} },
			observe: observeEvents,
		},
		writeCase[storagev1.StateProvider]{
			method:  "AppendExecutionEventIfAbsent",
			args:    func(storagev1.ControllerEpochFence) []any { return []any{caseEvent(), "event-1"} },
			observe: observeEvents,
		},
		writeCase[storagev1.StateProvider]{
			method: "AppendExecutionEventWithPlanIfAbsent",
			args: func(storagev1.ControllerEpochFence) []any {
				return []any{caseEvent(), "event-1", &storagev1.PlanState{Namespace: "tenant", TaskName: "task", Iteration: 1, Summary: "planned"}}
			},
			observe: observeEvents,
		},
		writeCase[storagev1.StateProvider]{
			method: "DeleteExecutionEvents",
			setup: func(t *testing.T, p storagev1.StateProvider) {
				appendEvent(t, p, caseEvent())
			},
			args:    fixed("tenant", storagev1.ExecutionEventStreamTypeTask, "task"),
			observe: observeEvents,
		},
	)
}
