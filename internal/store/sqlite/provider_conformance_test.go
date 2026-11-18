package sqlite_test

import (
	"path/filepath"
	"testing"

	"github.com/orka-agents/orka/internal/store/sqlite"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/orka-agents/orka/pkg/storage/v1/storagetest"
	"github.com/stretchr/testify/require"
)

// openConformanceStore opens a fresh file-backed store through the production
// open path: process lock, schema creation and dataset identity. As in
// production, it refuses every write without a write envelope; the suite
// attaches one.
func openConformanceStore(t *testing.T) *sqlite.Store {
	t.Helper()
	backend, err := sqlite.OpenLockedStore(filepath.Join(t.TempDir(), "orka.db"))
	if err != nil {
		t.Fatalf("open SQLite provider: %v", err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	backend.RequireWriteEnvelopes()
	return backend
}

// conformanceHarnesses are the optional suite harnesses SQLite supplies. A
// restart closes the store and opens its file again through the production
// open path, refusing writes without an envelope as before. SQLite migrates
// its schema only while it opens, under its process lock, so the maintenance
// harness holds the dataset in the migrating state through a test hook.
// SQLite declares no automatic switch.
func conformanceHarnesses() []storagetest.Option {
	return []storagetest.Option{
		storagetest.WithMaintenance(storagetest.MaintenanceHarness{
			Enter: func(t *testing.T, provider storagev1.ProviderDescriber) {
				sqlite.SetDatasetStateForTest(provider.(*sqlite.Store), storagev1.DatasetMigrating)
			},
			Exit: func(t *testing.T, provider storagev1.ProviderDescriber) {
				sqlite.SetDatasetStateForTest(provider.(*sqlite.Store), storagev1.DatasetReady)
			},
			// The suite sends IsLocked with an envelope, as every catalog
			// write, and SQLite refuses it while migrating.
			RefusesReadingWrites: true,
		}),
		storagetest.WithDurability(storagetest.DurabilityHarness{
			Restart: func(t *testing.T, provider storagev1.ProviderDescriber) storagev1.ProviderDescriber {
				t.Helper()
				backend := provider.(*sqlite.Store)
				path := sqlite.DatabasePathForTest(backend)
				require.NoError(t, backend.Close())
				reopened, err := sqlite.OpenLockedStore(path)
				require.NoError(t, err)
				t.Cleanup(func() { _ = reopened.Close() })
				reopened.RequireWriteEnvelopes()
				return reopened
			},
		}),
	}
}

func TestStateProviderConformance(t *testing.T) {
	storagetest.RunState(t, func(t *testing.T) storagev1.StateProvider {
		return openConformanceStore(t)
	}, conformanceHarnesses()...)
}

func TestArtifactProviderConformance(t *testing.T) {
	storagetest.RunArtifacts(t, func(t *testing.T) storagev1.ArtifactProvider {
		return openConformanceStore(t)
	}, conformanceHarnesses()...)
}

// artifactsOnly serves only artifacts.v1. The suite cannot give its dataset
// the first controller epoch through the state mirror, so it advances the
// dataset, which records the suite's installation, as it would for a
// provider that serves artifacts alone.
type artifactsOnly struct {
	storagev1.ArtifactProvider
}

func TestArtifactsOnlyProviderConformance(t *testing.T) {
	storagetest.RunArtifacts(t, func(t *testing.T) storagev1.ArtifactProvider {
		return artifactsOnly{ArtifactProvider: openConformanceStore(t)}
	})
}
