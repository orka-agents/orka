package sqlite_test

import (
	"path/filepath"
	"testing"

	"github.com/orka-agents/orka/internal/store/sqlite"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/orka-agents/orka/pkg/storage/v1/storagetest"
)

// openConformanceStore opens a fresh file-backed store through the production
// open path: process lock, schema creation and dataset identity.
func openConformanceStore(t *testing.T) *sqlite.Store {
	t.Helper()
	backend, err := sqlite.OpenLockedStore(filepath.Join(t.TempDir(), "orka.db"))
	if err != nil {
		t.Fatalf("open SQLite provider: %v", err)
	}
	t.Cleanup(func() { _ = backend.Close() })
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
