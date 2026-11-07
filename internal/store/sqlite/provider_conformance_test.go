package sqlite_test

import (
	"path/filepath"
	"testing"

	"github.com/orka-agents/orka/internal/store/sqlite"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/orka-agents/orka/pkg/storage/v1/storagetest"
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

func TestStateProviderConformance(t *testing.T) {
	storagetest.RunState(t, func(t *testing.T) storagev1.StateProvider {
		return openConformanceStore(t)
	})
}

func TestArtifactProviderConformance(t *testing.T) {
	storagetest.RunArtifacts(t, func(t *testing.T) storagev1.ArtifactProvider {
		return openConformanceStore(t)
	})
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
