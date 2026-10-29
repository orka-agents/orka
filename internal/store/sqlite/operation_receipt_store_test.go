package sqlite

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

var testWriteFence = store.ControllerEpochFence{Name: store.DefaultControllerEpochName, Epoch: 1, HolderID: "controller"}

// advanceTestEpoch binds s to a test installation at fence's epoch.
func advanceTestEpoch(t *testing.T, s *Store, fence store.ControllerEpochFence) string {
	t.Helper()
	info, err := s.Describe(t.Context())
	require.NoError(t, err)
	at := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	_, err = s.AdvanceControllerEpoch(t.Context(), storagev1.AdvanceControllerEpochRequest{
		DatasetID: info.DatasetID, InstallationID: "installation",
		Epoch: store.ControllerEpoch{
			Name: fence.Name, Epoch: fence.Epoch, HolderID: fence.HolderID,
			RequestDigest: controlTestDigest("epoch-" + fence.HolderID), Version: fence.Epoch,
			AcquiredAt: at, UpdatedAt: at,
		},
	})
	require.NoError(t, err)
	return info.DatasetID
}

// testEnvelope returns ctx with a new envelope for one call of method.
func testEnvelope(t *testing.T, ctx context.Context, dataset, method string, issuedAt time.Time, request ...any) (context.Context, storagev1.WriteEnvelope) {
	t.Helper()
	digest, err := storagev1.RequestDigest(method, request...)
	require.NoError(t, err)
	var id [8]byte
	_, err = rand.Read(id[:])
	require.NoError(t, err)
	envelope := storagev1.WriteEnvelope{
		DatasetID: dataset, Fence: testWriteFence, OperationID: "op-" + hex.EncodeToString(id[:]),
		RequestDigest: digest, IssuedAt: issuedAt, Sequence: 1,
	}
	return storagev1.WithWriteEnvelope(ctx, envelope), envelope
}

func TestSessionTurnPersistenceCommitsOnlyUnderTheCurrentFence(t *testing.T) {
	ctx := context.Background()
	s := setupTestStore(t)
	request := sessionTurnFinalizationOwnershipFixture(t, s, true, "fenced-task-uid")
	successor := store.ControllerEpochFence{Name: store.DefaultControllerEpochName, Epoch: 2, HolderID: "successor"}
	mirrorTestFence(t, s, successor)

	key := request.Key
	key.Attempt = 2
	_, err := s.CreateSessionTurnRecord(ctx, store.CreateSessionTurnRecordRequest{
		Namespace: "ns", SessionName: "session", Fence: request.Fence,
		Turn: store.SessionTurn{Key: key, PromptAttemptID: "attempt-2", RequestDigest: controlTestDigest("turn-2"), UserPrompt: "again"},
	})
	require.ErrorIs(t, err, store.ErrFenced, "a delayed turn creation from the replaced controller")

	finalization := store.CommitSessionTurnFinalizationRequest{
		Key: request.Key, Namespace: "ns", SessionName: "session", Fence: request.Fence,
		ExpectedTurnVersion: request.ExpectedTurnVersion, FinalizationDigest: request.FinalizationDigest,
		TerminalKind: request.TerminalKind, TerminalContent: request.TerminalContent,
		Projection: request.Projection, FinalizedAt: request.FinalizedAt,
	}
	client := orkaClient(t, s)
	_, err = client.CommitSessionTurnFinalization(ctx, finalization)
	require.ErrorIs(t, err, store.ErrFenced, "a delayed finalization from the replaced controller")
	transcript, err := s.LoadTranscript(ctx, "ns", "session", 0)
	require.NoError(t, err)
	require.Empty(t, transcript, "the refused finalization wrote nothing")

	finalization.Fence = successor
	finalized, err := client.CommitSessionTurnFinalization(ctx, finalization)
	require.NoError(t, err)
	activation := store.ActivateSessionTurnProjectionRequest{
		TurnID: finalized.ID, ProjectionID: finalized.ProjectionID, Fence: request.Fence,
		FinalizationDigest: finalized.FinalizationDigest, ExpectedAggregateKind: request.Projection.AggregateKind,
		ExpectedProjectionKind: request.Projection.ProjectionKind, ExpectedPayloadDigest: request.Projection.PayloadDigest,
		AvailableAt: request.FinalizedAt, UpdatedAt: request.FinalizedAt,
	}
	_, err = s.ActivateSessionTurnProjection(ctx, activation)
	require.ErrorIs(t, err, store.ErrFenced, "a delayed activation from the replaced controller")
	activation.Fence = successor
	_, err = s.ActivateSessionTurnProjection(ctx, activation)
	require.NoError(t, err)
}

func TestOperationReceiptsArePrunedAfterTheirRetention(t *testing.T) {
	ctx := context.Background()
	s := setupDiskStore(t)
	now := time.Now().UTC()
	s.clock = func() time.Time { return now }
	dataset := advanceTestEpoch(t, s, testWriteFence)

	saveCtx, envelope := testEnvelope(t, ctx, dataset, "SaveResult", now, "ns", "task", []byte("first"))
	require.NoError(t, s.SaveResult(saveCtx, "ns", "task", []byte("first")))
	for _, name := range []string{"a", "b"} {
		deleteCtx, _ := testEnvelope(t, ctx, dataset, "DeleteResult", now, "ns", name)
		require.NoError(t, s.DeleteResult(deleteCtx, "ns", name))
	}

	now = now.Add(receiptRetention)
	removed, err := s.PruneOperationReceipts(ctx, 10)
	require.NoError(t, err)
	require.Zero(t, removed, "a receipt is kept for its whole retention")

	now = now.Add(time.Second)
	removed, err = s.PruneOperationReceipts(ctx, 2)
	require.NoError(t, err)
	require.Equal(t, 2, removed, "pruning is bounded by its limit")
	require.NoError(t, s.pruneExpiredOperationReceipts(ctx))
	_, err = s.ResolveOperation(ctx, storagev1.ResolveOperationRequest{
		DatasetID: dataset, OperationID: envelope.OperationID, Method: "SaveResult",
		RequestDigest: envelope.RequestDigest, IssuedAt: envelope.IssuedAt,
	})
	require.ErrorIs(t, err, storagev1.ErrOperationExpired, "a pruned receipt is not reported as an abort")
	require.ErrorIs(t, s.SaveResult(saveCtx, "ns", "task", []byte("first")), storagev1.ErrOperationExpired,
		"once its receipt is gone, a late retry is refused rather than applied again")
}

func TestWriteChecksRunInTheContractOrder(t *testing.T) {
	ctx := context.Background()
	s := setupTestStore(t)
	dataset := advanceTestEpoch(t, s, testWriteFence)
	now := time.Now().UTC()
	committedCtx, committed := testEnvelope(t, ctx, dataset, "UpdateTokenCounts", now, "ns", "session", 1, 1)
	require.NoError(t, s.CreateSession(ctx, &store.SessionRecord{Namespace: "ns", Name: "session", SessionType: "task"}))
	require.NoError(t, s.UpdateTokenCounts(committedCtx, "ns", "session", 1, 1))

	state := storagev1.DatasetMigrating
	s.writeState = func() storagev1.DatasetState { return state }
	require.NoError(t, s.UpdateTokenCounts(committedCtx, "ns", "session", 1, 1), "a replay is served while the dataset is not ready")
	resolved, err := s.ResolveOperation(ctx, storagev1.ResolveOperationRequest{
		DatasetID: dataset, OperationID: committed.OperationID, Method: "UpdateTokenCounts",
		RequestDigest: committed.RequestDigest, IssuedAt: committed.IssuedAt,
	})
	require.NoError(t, err)
	require.Equal(t, storagev1.OperationCommitted, resolved.State)

	pendingCtx, pending := testEnvelope(t, ctx, dataset, "UpdateTokenCounts", now, "ns", "session", 2, 2)
	require.ErrorIs(t, s.UpdateTokenCounts(pendingCtx, "ns", "session", 2, 2), store.ErrNotReady)
	_, err = s.ResolveOperation(ctx, storagev1.ResolveOperationRequest{
		DatasetID: dataset, OperationID: pending.OperationID, Method: "UpdateTokenCounts",
		RequestDigest: pending.RequestDigest, IssuedAt: pending.IssuedAt,
	})
	require.ErrorIs(t, err, store.ErrNotReady, "an unsettled operation is not aborted while the dataset is not ready")
	expiredCtx, _ := testEnvelope(t, ctx, dataset, "UpdateTokenCounts", now.Add(-storagev1.MaxOperationAge-time.Minute), "ns", "session", 3, 3)
	require.ErrorIs(t, s.UpdateTokenCounts(expiredCtx, "ns", "session", 3, 3), storagev1.ErrOperationExpired,
		"the operation's age is checked before the dataset state")
	advanceTestEpoch(t, s, store.ControllerEpochFence{Name: store.DefaultControllerEpochName, Epoch: 2, HolderID: "successor"})
	require.ErrorIs(t, s.UpdateTokenCounts(pendingCtx, "ns", "session", 2, 2), store.ErrNotReady,
		"the dataset state is checked before the fence")

	state = storagev1.DatasetReady
	require.ErrorIs(t, s.UpdateTokenCounts(pendingCtx, "ns", "session", 2, 2), store.ErrFenced)
	session, err := s.GetSession(ctx, "ns", "session")
	require.NoError(t, err)
	require.Equal(t, 1, session.InputTokens, "only the first operation committed")
}

func TestWriteOperationInsideTaskDataTransaction(t *testing.T) {
	ctx := context.Background()
	s := setupTestStore(t)
	dataset := advanceTestEpoch(t, s, testWriteFence)
	now := time.Now().UTC()
	require.NoError(t, s.CreateSession(ctx, &store.SessionRecord{Namespace: "ns", Name: "session", SessionType: "task"}))
	require.NoError(t, s.AppendMessages(ctx, "ns", "session", []store.SessionMessage{{ID: "m2", Role: "user", Content: "stored"}}))

	saveCtx, _ := testEnvelope(t, ctx, dataset, "SaveResult", now, "ns", "task", []byte("result"))
	conflicting := []store.SessionMessage{{ID: "m1", Role: "user", Content: "new"}, {ID: "m2", Role: "user", Content: "different"}}
	err := s.WithTaskDataTransaction(ctx, func(txCtx context.Context) error {
		envelope, _ := storagev1.WriteEnvelopeFrom(saveCtx)
		if err := s.SaveResult(storagev1.WithWriteEnvelope(txCtx, envelope), "ns", "task", []byte("result")); err != nil {
			return err
		}
		appendCtx, _ := testEnvelope(t, txCtx, dataset, "AppendMessages", now, "ns", "session", conflicting)
		err := s.AppendMessages(appendCtx, "ns", "session", conflicting)
		require.ErrorIs(t, err, store.ErrDuplicateMismatch)
		return nil
	})
	require.NoError(t, err)

	session, err := s.GetSession(ctx, "ns", "session")
	require.NoError(t, err)
	require.Len(t, session.Messages, 1, "a failed operation in an enclosing transaction leaves none of its writes")
	require.NoError(t, s.SaveResult(ctx, "ns", "task", []byte("newer")))
	require.NoError(t, s.SaveResult(saveCtx, "ns", "task", []byte("result")), "the receipt committed with the enclosing transaction")
	data, err := s.GetResult(ctx, "ns", "task")
	require.NoError(t, err)
	require.Equal(t, "newer", string(data), "the replay changed nothing")
}

func TestWriteOperationRequiresACatalogWriteMethod(t *testing.T) {
	ctx := context.Background()
	s := setupTestStore(t)
	dataset := advanceTestEpoch(t, s, testWriteFence)
	envelopeCtx, _ := testEnvelope(t, ctx, dataset, "GetResult", time.Now().UTC(), "ns", "task")
	_, err := writeOperation(envelopeCtx, s, "GetResult", nil, func(context.Context) (noResult, error) {
		return noResult{}, errors.New("must not run")
	})
	require.ErrorContains(t, err, "not a contract write method")
	explicit := testWriteFence
	explicit.HolderID = "other"
	saveCtx, _ := testEnvelope(t, ctx, dataset, "SaveResult", time.Now().UTC(), "ns", "task", []byte("x"))
	_, err = writeOperation(saveCtx, s, "SaveResult", &explicit, func(context.Context) (noResult, error) {
		return noResult{}, errors.New("must not run")
	})
	require.ErrorIs(t, err, store.ErrValidation)
}
