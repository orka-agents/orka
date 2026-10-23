package storageclient_test

import (
	"testing"

	"github.com/orka-agents/orka/internal/storageclient"
	"github.com/orka-agents/orka/internal/storageclient/storageclienttest"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/orka-agents/orka/pkg/storage/v1/storagetest"
)

// newEmbeddedClient returns a client over a SQLite store that, as in
// production, refuses every write without an envelope.
func newEmbeddedClient(t *testing.T) *storageclient.Client {
	t.Helper()
	backend := openSQLite(t)
	backend.RequireWriteEnvelopes()
	return storageclienttest.NewEmbedded(t, backend)
}

// The shared provider suite also runs through the client, proving that each
// contract family reaches the in-process provider unchanged and that every
// write the suite makes without an envelope gets one the provider accepts.
func TestClientStateConformance(t *testing.T) {
	storagetest.RunState(t, func(t *testing.T) storagev1.StateProvider {
		return newEmbeddedClient(t)
	})
}

func TestClientArtifactConformance(t *testing.T) {
	storagetest.RunArtifacts(t, func(t *testing.T) storagev1.ArtifactProvider {
		return newEmbeddedClient(t)
	})
}
