package storagetest

import (
	"testing"
	"time"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

// runOperationReceipts checks how a provider settles operations and keeps
// their receipts, beyond the per-method write catalog.
func runOperationReceipts(t *testing.T, newProvider StateFactory) {
	resolveRequest := func(dataset string, envelope storagev1.WriteEnvelope, method string) storagev1.ResolveOperationRequest {
		return storagev1.ResolveOperationRequest{
			DatasetID: dataset, OperationID: envelope.OperationID, Method: method,
			RequestDigest: envelope.RequestDigest, IssuedAt: envelope.IssuedAt,
		}
	}

	t.Run("resolution refuses what it cannot settle", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		advanceEpoch(t, provider, suiteFence)
		dataset := datasetID(t, provider)
		createSessionFixture(t, provider, "tenant", "session", sessionsTypeTask, writeCaseCreated)
		envelope := NewWriteEnvelope(t, dataset, suiteFence, "UpdateTokenCounts", "tenant", "session", 1, 1)

		_, err := provider.ResolveOperation(ctx, storagev1.ResolveOperationRequest{DatasetID: dataset})
		require.ErrorIs(t, err, storagev1.ErrValidation)
		read := resolveRequest(dataset, envelope, "GetResult")
		_, err = provider.ResolveOperation(ctx, read)
		require.ErrorIs(t, err, storagev1.ErrValidation, "only a write method has an operation to settle")
		other := resolveRequest("00000000-0000-4000-8000-000000000000", envelope, "UpdateTokenCounts")
		_, err = provider.ResolveOperation(ctx, other)
		require.ErrorIs(t, err, storagev1.ErrDatasetMismatch)

		old := resolveRequest(dataset, envelope, "UpdateTokenCounts")
		old.IssuedAt = time.Now().UTC().Add(-storagev1.MinReceiptRetention)
		_, err = provider.ResolveOperation(ctx, old)
		require.ErrorIs(t, err, storagev1.ErrOperationExpired, "a missing receipt proves nothing once it could have been pruned")
		ahead := resolveRequest(dataset, envelope, "UpdateTokenCounts")
		ahead.IssuedAt = time.Now().UTC().Add(storagev1.MaxClockSkew + time.Minute)
		_, err = provider.ResolveOperation(ctx, ahead)
		require.ErrorIs(t, err, storagev1.ErrClockSkew)

		require.NoError(t, provider.UpdateTokenCounts(storagev1.WithWriteEnvelope(ctx, envelope), "tenant", "session", 1, 1),
			"the refused resolutions recorded nothing, so the operation still commits")
		mismatch := resolveRequest(dataset, envelope, "UpdateTokenCounts")
		mismatch.RequestDigest = controlDigest("another request")
		_, err = provider.ResolveOperation(ctx, mismatch)
		require.ErrorIs(t, err, storagev1.ErrDuplicateMismatch)
		otherMethod := resolveRequest(dataset, envelope, "UpdateTokenCountsWithLock")
		_, err = provider.ResolveOperation(ctx, otherMethod)
		require.ErrorIs(t, err, storagev1.ErrDuplicateMismatch)

		advanceEpoch(t, provider, nextFence)
		resolved, err := provider.ResolveOperation(ctx, resolveRequest(dataset, envelope, "UpdateTokenCounts"))
		require.NoError(t, err, "any controller may resolve, including a replaced one")
		require.Equal(t, storagev1.OperationCommitted, resolved.State)
		require.Equal(t, suiteFence, resolved.Fence)
		require.False(t, resolved.RecordedAt.IsZero())
		require.Equal(t, time.UTC, resolved.RecordedAt.Location())
	})

	t.Run("pruning keeps receipts within their retention", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		info, err := provider.Describe(ctx)
		require.NoError(t, err)
		require.GreaterOrEqual(t, info.Limits.ReceiptRetention, storagev1.MinReceiptRetention)
		require.Positive(t, info.Limits.MaxReceiptResultBytes, "a provider keeping receipts reports its result limit")

		_, err = provider.PruneOperationReceipts(ctx, 0)
		require.ErrorIs(t, err, storagev1.ErrValidation)
		advanceEpoch(t, provider, suiteFence)
		envelope := NewWriteEnvelope(t, info.DatasetID, suiteFence, "SaveResult", "tenant", "task", []byte("kept"))
		envelopeCtx := storagev1.WithWriteEnvelope(ctx, envelope)
		require.NoError(t, provider.SaveResult(envelopeCtx, "tenant", "task", []byte("kept")))
		removed, err := provider.PruneOperationReceipts(ctx, 100)
		require.NoError(t, err)
		require.Zero(t, removed, "a fresh receipt survives pruning")
		require.NoError(t, provider.SaveResult(ctx, "tenant", "task", []byte("newer")))
		require.NoError(t, provider.SaveResult(envelopeCtx, "tenant", "task", []byte("kept")),
			"the receipt still answers a retry")
		data, err := provider.GetResult(ctx, "tenant", "task")
		require.NoError(t, err)
		require.Equal(t, []byte("newer"), data, "the replay changed nothing")
	})

	t.Run("concurrent callers of one operation see one change", func(t *testing.T) {
		provider := newProvider(t)
		advanceEpoch(t, provider, suiteFence)
		createSessionFixture(t, provider, "tenant", "session", sessionsTypeTask, writeCaseCreated)
		counter := writeCase[storagev1.StateProvider]{method: "UpdateTokenCounts", args: fixed("tenant", "session", 3, 4)}
		runConcurrentReplays(t, provider, counter, 8)
		session, err := provider.GetSession(t.Context(), "tenant", "session")
		require.NoError(t, err)
		require.Equal(t, 3, session.InputTokens)
		require.Equal(t, 4, session.OutputTokens)
	})
}
