package storagetest

import (
	"testing"
	"time"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

// advanceWatermark advances provider's write watermark as one operation under
// fence and returns the envelope with the result.
func advanceWatermark[P suiteProvider](t *testing.T, provider P, fence storagev1.ControllerEpochFence) (storagev1.WriteEnvelope, storagev1.WriteWatermark, error) {
	t.Helper()
	envelope := NewWriteEnvelope(t, datasetID(t, provider), fence, "AdvanceWriteWatermark")
	watermark, err := provider.AdvanceWriteWatermark(storagev1.WithWriteEnvelope(t.Context(), envelope))
	return envelope, watermark, err
}

// runWriteWatermark checks that a dataset's write watermark starts at zero,
// increases by one per committed operation, and is never advanced again by a
// replay.
func runWriteWatermark[P suiteProvider](t *testing.T, newProvider func(t *testing.T) P) {
	t.Run("advances by one per operation", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		dataset := datasetID(t, provider)
		fresh, err := provider.GetWriteWatermark(ctx)
		require.NoError(t, err)
		require.Equal(t, storagev1.WriteWatermark{DatasetID: dataset}, fresh, "a new dataset's watermark is zero")

		advanceEpoch(t, provider, suiteFence)
		first, firstWatermark, err := advanceWatermark(t, provider, suiteFence)
		require.NoError(t, err)
		require.Equal(t, dataset, firstWatermark.DatasetID)
		require.Equal(t, int64(1), firstWatermark.Value)
		require.False(t, firstWatermark.AdvancedAt.IsZero())
		require.Equal(t, time.UTC, firstWatermark.AdvancedAt.Location())
		stored, err := provider.GetWriteWatermark(ctx)
		require.NoError(t, err)
		require.Equal(t, firstWatermark, stored)

		_, second, err := advanceWatermark(t, provider, suiteFence)
		require.NoError(t, err)
		require.Equal(t, int64(2), second.Value)
		require.False(t, second.AdvancedAt.Before(firstWatermark.AdvancedAt))

		replayed, err := provider.AdvanceWriteWatermark(storagev1.WithWriteEnvelope(ctx, first))
		require.NoError(t, err)
		require.Equal(t, firstWatermark, replayed, "a replay returns the original watermark")
		stored, err = provider.GetWriteWatermark(ctx)
		require.NoError(t, err)
		require.Equal(t, second, stored, "a replay does not advance the watermark")

		advanceEpoch(t, provider, nextFence)
		_, _, err = advanceWatermark(t, provider, suiteFence)
		requireFenced(t, err)
		_, third, err := advanceWatermark(t, provider, nextFence)
		require.NoError(t, err)
		require.Equal(t, int64(3), third.Value, "the watermark carries across controller epochs")
	})
}
