package sqlite_test

import (
	"path/filepath"
	"testing"

	"github.com/orka-agents/orka/internal/store/sqlite"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/orka-agents/orka/pkg/storage/v1/storagetest"
	"github.com/orka-agents/orka/pkg/storage/v1/wire/wiretest"
	"github.com/stretchr/testify/require"
)

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

func conformanceHarnesses() []storagetest.Option {
	return []storagetest.Option{
		storagetest.WithMaintenance(storagetest.MaintenanceHarness{
			Enter: func(t *testing.T, provider storagev1.ProviderDescriber) {
				sqlite.SetDatasetStateForTest(provider.(*sqlite.Store), storagev1.DatasetMigrating)
			},
			Exit: func(t *testing.T, provider storagev1.ProviderDescriber) {
				sqlite.SetDatasetStateForTest(provider.(*sqlite.Store), storagev1.DatasetReady)
			},
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

func TestStateProviderConformanceOverWire(t *testing.T) {
	storagetest.RunState(t, func(t *testing.T) storagev1.StateProvider {
		return wiretest.State(t, openConformanceStore(t))
	}, storagetest.Parallel(), storagetest.RecomputesRequestDigests(wiretest.Methods()...))
}

func TestArtifactProviderConformance(t *testing.T) {
	storagetest.RunArtifacts(t, func(t *testing.T) storagev1.ArtifactProvider {
		return openConformanceStore(t)
	}, conformanceHarnesses()...)
}

type artifactsOnly struct {
	storagev1.ArtifactProvider
}

func TestArtifactsOnlyProviderConformance(t *testing.T) {
	storagetest.RunArtifacts(t, func(t *testing.T) storagev1.ArtifactProvider {
		return artifactsOnly{ArtifactProvider: openConformanceStore(t)}
	})
}

func TestArtifactProviderConformanceOverWire(t *testing.T) {
	storagetest.RunArtifacts(t, func(t *testing.T) storagev1.ArtifactProvider {
		return wiretest.Artifacts(t, openConformanceStore(t))
	}, storagetest.Parallel(), storagetest.RecomputesRequestDigests(wiretest.ArtifactsMethods()...))
}
