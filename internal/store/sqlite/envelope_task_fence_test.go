package sqlite

import (
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

// The envelope checks and the task-data fence run in one transaction, in
// order: a replay checks the task-data fence before it returns its stored
// outcome, and ResolveOperation still reports that outcome; a write the
// task-data fence refuses leaves no receipt; and a stale envelope fence is
// reported before a stale task-data fence.
func TestEnvelopeChecksComposeWithTheTaskDataFence(t *testing.T) {
	ctx := t.Context()
	s := setupTestStore(t)
	dataset := advanceTestEpoch(t, s, testWriteFence)
	fence, _, err := s.ResolveTaskDataFence(ctx, "ns", "task")
	require.NoError(t, err)
	fenced := store.WithTaskDataFence(ctx, fence)

	saveCtx, saved := testEnvelope(t, fenced, dataset, "SaveResult", time.Now().UTC(), "ns", "task", []byte("result"))
	require.NoError(t, s.SaveResult(saveCtx, "ns", "task", []byte("result")))
	// Cleanup moves the Task's fence on.
	require.NoError(t, s.DeleteArtifacts(ctx, store.ArtifactOwner{Namespace: "ns", TaskName: "task"}))
	require.ErrorIs(t, s.SaveResult(saveCtx, "ns", "task", []byte("result")), store.ErrTaskDataCleanupChanged,
		"a replay checks the task-data fence before it returns the stored outcome")
	settled, err := s.ResolveOperation(ctx, storagev1.ResolveOperationRequest{
		DatasetID: dataset, OperationID: saved.OperationID, Method: "SaveResult",
		RequestDigest: saved.RequestDigest, IssuedAt: saved.IssuedAt,
	})
	require.NoError(t, err)
	require.Equal(t, storagev1.OperationCommitted, settled.State, "the refused replay leaves the committed outcome to resolution")

	refusedCtx, refused := testEnvelope(t, fenced, dataset, "SaveResult", time.Now().UTC(), "ns", "task", []byte("later"))
	require.ErrorIs(t, s.SaveResult(refusedCtx, "ns", "task", []byte("later")), store.ErrTaskDataCleanupChanged)
	outcome, err := s.ResolveOperation(ctx, storagev1.ResolveOperationRequest{
		DatasetID: dataset, OperationID: refused.OperationID, Method: "SaveResult",
		RequestDigest: refused.RequestDigest, IssuedAt: refused.IssuedAt,
	})
	require.NoError(t, err)
	require.Equal(t, storagev1.OperationAborted, outcome.State, "the refused write left no receipt")
	data, err := s.GetResult(ctx, "ns", "task")
	require.NoError(t, err)
	require.Equal(t, "result", string(data), "the refused write changed nothing")

	advanceTestEpoch(t, s, store.ControllerEpochFence{Name: store.DefaultControllerEpochName, Epoch: 2, HolderID: "successor"})
	staleCtx, _ := testEnvelope(t, fenced, dataset, "SaveResult", time.Now().UTC(), "ns", "task", []byte("stale"))
	err = s.SaveResult(staleCtx, "ns", "task", []byte("stale"))
	require.ErrorIs(t, err, store.ErrFenced, "the envelope fence is checked first")
	require.NotErrorIs(t, err, store.ErrTaskDataCleanupChanged)
}
