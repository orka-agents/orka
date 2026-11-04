package storagetest

import (
	"sync"
	"testing"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

// runControllerOwnership checks that a dataset's controller epoch only moves
// forward, belongs to one installation, and fences writes at commit.
func runControllerOwnership(t *testing.T, newProvider StateFactory) {
	advance := func(provider storagev1.StateProvider, dataset string, epoch storagev1.ControllerEpoch, installation string) (storagev1.ControllerEpochMirrorState, error) {
		return provider.AdvanceControllerEpoch(t.Context(), storagev1.AdvanceControllerEpochRequest{
			DatasetID: dataset, Epoch: epoch, InstallationID: installation,
		})
	}

	t.Run("advances only forward for one installation", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		dataset := datasetID(t, provider)
		_, err := provider.GetControllerEpochMirror(ctx, controlEpochName)
		require.ErrorIs(t, err, storagev1.ErrNotFound)
		_, err = advance(provider, dataset, storagev1.ControllerEpoch{Name: controlEpochName}, suiteInstance)
		require.ErrorIs(t, err, storagev1.ErrValidation)
		_, err = advance(provider, "00000000-0000-4000-8000-000000000000", mirroredEpoch(1, "owner-a"), suiteInstance)
		require.ErrorIs(t, err, storagev1.ErrDatasetMismatch)

		first, err := advance(provider, dataset, mirroredEpoch(1, "owner-a"), suiteInstance)
		require.NoError(t, err)
		require.Equal(t, dataset, first.DatasetID)
		require.Equal(t, mirroredEpoch(1, "owner-a"), first.Epoch)
		require.Equal(t, suiteInstance, first.InstallationID)
		require.Nil(t, first.Previous)
		require.False(t, first.AdvancedAt.IsZero())

		repeat, err := advance(provider, dataset, mirroredEpoch(1, "owner-a"), suiteInstance)
		require.NoError(t, err, "an identical advance is idempotent")
		require.Equal(t, first, repeat)
		conflicting := mirroredEpoch(1, "owner-b")
		_, err = advance(provider, dataset, conflicting, suiteInstance)
		require.ErrorIs(t, err, storagev1.ErrConflict)
		conflicting = mirroredEpoch(1, "owner-a")
		conflicting.RequestDigest = controlDigest("another acquisition")
		_, err = advance(provider, dataset, conflicting, suiteInstance)
		require.ErrorIs(t, err, storagev1.ErrConflict)
		_, err = advance(provider, dataset, mirroredEpoch(2, "owner-b"), "installation-b")
		requireFenced(t, err)

		jumped, err := advance(provider, dataset, mirroredEpoch(3, "owner-c"), suiteInstance)
		require.NoError(t, err, "an epoch may jump forward")
		require.Equal(t, &storagev1.ControllerEpochFence{Name: controlEpochName, Epoch: 1, HolderID: "owner-a"}, jumped.Previous)
		_, err = advance(provider, dataset, mirroredEpoch(2, "owner-b"), suiteInstance)
		requireFenced(t, err)
		stored, err := provider.GetControllerEpochMirror(ctx, controlEpochName)
		require.NoError(t, err)
		require.Equal(t, jumped, stored)

		requireFenced(t, provider.SyncControllerEpochMirror(ctx, mirroredEpoch(4, "owner-d")))
		stored, err = provider.GetControllerEpochMirror(ctx, controlEpochName)
		require.NoError(t, err)
		require.Equal(t, jumped, stored, "a bound dataset moves only through AdvanceControllerEpoch")
	})

	t.Run("an epoch stored before installations binds the first advance", func(t *testing.T) {
		provider := newProvider(t)
		dataset := datasetID(t, provider)
		syncEpoch(t, provider, 2, "owner-b")
		_, err := advance(provider, dataset, mirroredEpoch(1, "owner-a"), suiteInstance)
		requireFenced(t, err)
		state, err := provider.GetControllerEpochMirror(t.Context(), controlEpochName)
		require.NoError(t, err)
		require.Empty(t, state.InstallationID, "a refused advance binds nothing")

		repeat, err := advance(provider, dataset, mirroredEpoch(2, "owner-b"), suiteInstance)
		require.NoError(t, err)
		require.Equal(t, suiteInstance, repeat.InstallationID)
		_, err = advance(provider, dataset, mirroredEpoch(3, "owner-c"), "installation-b")
		requireFenced(t, err)
	})

	t.Run("a write delayed past an advance cannot commit", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		advanceEpoch(t, provider, suiteFence)
		dataset := datasetID(t, provider)
		createSessionFixture(t, provider, "tenant", "session", sessionsTypeTask, writeCaseCreated)

		const writers = 8
		envelopes := make([]storagev1.WriteEnvelope, writers)
		for i := range envelopes {
			envelopes[i] = NewWriteEnvelope(t, dataset, suiteFence, "UpdateTokenCounts", "tenant", "session", 1, 0)
		}
		errs := make([]error, writers)
		var wg sync.WaitGroup
		for i := range writers {
			wg.Go(func() {
				errs[i] = provider.UpdateTokenCounts(storagev1.WithWriteEnvelope(ctx, envelopes[i]), "tenant", "session", 1, 0)
			})
		}
		advanceEpoch(t, provider, nextFence)
		snapshot, err := provider.GetSession(ctx, "tenant", "session")
		require.NoError(t, err)
		wg.Wait()

		committed := 0
		for _, err := range errs {
			if err == nil {
				committed++
				continue
			}
			requireFenced(t, err)
		}
		final, err := provider.GetSession(ctx, "tenant", "session")
		require.NoError(t, err)
		require.Equal(t, snapshot.InputTokens, final.InputTokens, "no write under the old fence committed after the advance returned")
		require.Equal(t, committed, final.InputTokens, "every write that returned nil committed before the advance")
		for i, err := range errs {
			if err == nil {
				require.NoError(t, provider.UpdateTokenCounts(storagev1.WithWriteEnvelope(ctx, envelopes[i]), "tenant", "session", 1, 0),
					"a replaced controller still reads its original result")
			}
		}
		final, err = provider.GetSession(ctx, "tenant", "session")
		require.NoError(t, err)
		require.Equal(t, committed, final.InputTokens)
	})
}
