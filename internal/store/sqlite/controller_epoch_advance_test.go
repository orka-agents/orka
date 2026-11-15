package sqlite

import (
	"testing"

	"github.com/orka-agents/orka/internal/store"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

func TestAcknowledgedEpochGapSurvivesReopen(t *testing.T) {
	s := setupDiskStore(t)
	ctx := t.Context()
	dataset := advanceTestEpoch(t, s, store.ControllerEpochFence{
		Name: store.DefaultControllerEpochName, Epoch: 9, HolderID: "restored-owner",
	})
	before, err := s.GetControllerEpochMirror(ctx, store.DefaultControllerEpochName)
	require.NoError(t, err)
	require.NoError(t, s.Close())

	// Each takeover starts from disk, as after a crash before recording a
	// recovery hold. Neither may replace the restored epoch 9 with 11 or 12.
	for _, next := range []int64{11, 12} {
		reopened, err := OpenLockedStore(s.dbPath)
		require.NoError(t, err)
		t.Cleanup(func() { _ = reopened.Close() })
		epoch := before.Epoch
		epoch.Epoch, epoch.Version, epoch.HolderID = next, next, "replacement-owner"
		_, err = reopened.AdvanceControllerEpoch(ctx, storagev1.AdvanceControllerEpochRequest{
			DatasetID: dataset, Epoch: epoch, InstallationID: "installation", AcknowledgedEpoch: 10,
		})
		require.ErrorIs(t, err, storagev1.ErrNotReady)
		after, err := reopened.GetControllerEpochMirror(ctx, epoch.Name)
		require.NoError(t, err)
		require.Equal(t, before, after)
		require.NoError(t, reopened.Close())
	}
}
