package storagetest

import (
	"testing"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

// runInstallations checks that a dataset belongs to the installation whose
// controller first advanced it: a second installation pointed at the same
// dataset cannot advance it or write to it, and the first installation keeps
// writing.
func runInstallations[P suiteProvider](t *testing.T, newProvider func(t *testing.T) P) {
	t.Run("a second installation cannot take a dataset", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		dataset := datasetID(t, provider)
		bound := advanceEpoch(t, provider, suiteFence)
		require.Equal(t, suiteInstance, bound.InstallationID)
		_, before, err := advanceWatermark(t, provider, suiteFence)
		require.NoError(t, err)

		const otherInstallation = "installation-b"
		for name, epoch := range map[string]storagev1.ControllerEpoch{
			"a newer epoch":                  mirroredEpoch(2, "controller-x"),
			"the same epoch and holder":      mirroredEpoch(suiteFence.Epoch, suiteFence.HolderID),
			"a much newer epoch":             mirroredEpoch(100, "controller-x"),
			"the same epoch, another holder": mirroredEpoch(suiteFence.Epoch, "controller-x"),
		} {
			_, err := provider.AdvanceControllerEpoch(ctx, storagev1.AdvanceControllerEpochRequest{
				DatasetID: dataset, Epoch: epoch, InstallationID: otherInstallation,
			})
			require.ErrorIsf(t, err, storagev1.ErrFenced, "%s: the dataset belongs to another installation", name)
		}
		mirror, err := provider.GetControllerEpochMirror(ctx, controlEpochName)
		require.NoError(t, err)
		require.Equal(t, bound, mirror, "a refused advance changes nothing")

		// The other installation's controller writes under the fence its own
		// Kubernetes granted, which this dataset never stored.
		_, _, err = advanceWatermark(t, provider, controlFence(2, "controller-x"))
		requireFenced(t, err)
		unchanged, err := provider.GetWriteWatermark(ctx)
		require.NoError(t, err)
		require.Equal(t, before, unchanged)

		_, after, err := advanceWatermark(t, provider, suiteFence)
		require.NoError(t, err, "the owning installation keeps writing")
		require.Equal(t, before.Value+1, after.Value)
		advanced := advanceEpoch(t, provider, nextFence)
		require.Equal(t, suiteInstance, advanced.InstallationID)
		_, _, err = advanceWatermark(t, provider, nextFence)
		require.NoError(t, err, "and keeps advancing its own epochs")
	})
}
