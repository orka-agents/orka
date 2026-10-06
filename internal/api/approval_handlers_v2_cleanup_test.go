package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/store"
)

func TestV2ApprovalAPICleanedCancellationCannotBeRevived(t *testing.T) {
	f := newV2ApprovalAPIFixture(t)
	waiting := f.start(t)
	pending := f.pending(t)
	content, err := json.Marshal(map[string]string{
		"approvalID": pending.ID, "taskUID": string(f.task.UID), "reason": "approval_cancelled",
	})
	require.NoError(t, err)
	_, err = f.store.AppendExecutionEvent(t.Context(), &store.ExecutionEvent{
		Namespace: f.task.Namespace, StreamType: events.ExecutionEventStreamTypeTask, StreamID: f.task.Name, TaskName: f.task.Name,
		Type: events.ExecutionEventTypeApprovalCancelled, ToolCallID: pending.ID, Content: content,
	})
	require.NoError(t, err)
	denied := waiting.result(t)
	requireV2ApprovalAPIError(t, denied, "approval_cancelled")
	retained := f.list(t)
	require.Len(t, retained, 1)
	require.Equal(t, approvals.StatusCancelled, retained[0].Status)
	require.Equal(t, "not_started", retained[0].ExecutionOutcome)
	require.Equal(t, "approval_cancelled", retained[0].ExecutionReason)
	require.NotNil(t, retained[0].Binding)
	identity := store.ExternalEffectIdentity{
		Kind: "acp-mcp-tool", Namespace: f.task.Namespace,
		AggregateID: string(f.call.Authorization.RuntimeSessionUID), OperationID: string(f.call.Metadata.OperationID),
	}
	id, err := identity.CanonicalID()
	require.NoError(t, err)
	saved, err := f.store.GetExternalEffect(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, store.ExternalEffectFailed, saved.State)
	require.Zero(t, saved.Attempts)
	require.Empty(t, saved.LeaseOwner)
	require.Nil(t, saved.LeaseExpiresAt)
	require.Equal(t, denied.Result, saved.Response)
	require.Equal(t, store.CanonicalBytesDigest(denied.Result), saved.ResponseDigest)
	require.Zero(t, f.count.Load())

	// Ordinary timeline cleanup is allowed even while this Task remains.
	// Terminal redelivery must replay its receipt, not reconstruct that history.
	require.NoError(t, f.store.DeleteExecutionEvents(t.Context(), f.task.Namespace, events.ExecutionEventStreamTypeTask, f.task.Name))
	replayed := f.start(t).result(t)
	require.True(t, replayed.Replayed)
	require.Equal(t, denied.Result, replayed.Result)
	require.Empty(t, f.list(t))
	late := f.decide("reviewer-a", pending.ID, "approve")
	require.NoError(t, late.err)
	require.Equal(t, http.StatusNotFound, late.status)
	listed, err := approvals.ListEvents(t.Context(), f.store, f.task.Namespace, f.task.Name)
	require.NoError(t, err)
	require.Empty(t, listed)
	current, err := f.store.GetExternalEffect(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, saved, current)
	require.Zero(t, f.count.Load())
}
