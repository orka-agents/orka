package sqlite_test

import (
	"path/filepath"
	"testing"

	"github.com/orka-agents/orka/internal/store/sqlite"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/orka-agents/orka/pkg/storage/v1/storagetest"
	"github.com/orka-agents/orka/pkg/storage/v1/wire/wiretest"
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

// The suite runs again with every family that has wire RPCs served over a
// real TLS loopback by the server library and called through the generated
// client, proving the wire mapping preserves SQLite's behavior. Families
// without RPCs yet are served in process.
func TestStateProviderConformanceOverWire(t *testing.T) {
	storagetest.RunState(t, func(t *testing.T) storagev1.StateProvider {
		return wiretest.State(t, openConformanceStore(t))
	}, storagetest.Parallel())
}

func TestArtifactProviderConformance(t *testing.T) {
	storagetest.RunArtifacts(t, func(t *testing.T) storagev1.ArtifactProvider {
		return openConformanceStore(t)
	})
}

// The artifacts suite runs again with every artifacts.v1 method served over
// the TLS loopback, artifact bytes moving with blob framing on a separate
// bulk connection.
func TestArtifactProviderConformanceOverWire(t *testing.T) {
	storagetest.RunArtifacts(t, func(t *testing.T) storagev1.ArtifactProvider {
		return wiretest.Artifacts(t, openConformanceStore(t))
	})
}
