package storageclient_test

import (
	"testing"

	"github.com/orka-agents/orka/internal/storageclient"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/orka-agents/orka/pkg/storage/v1/storagetest"
)

func newEmbeddedClient(t *testing.T) *storageclient.Client {
	t.Helper()
	client, err := storageclient.NewEmbedded(openSQLite(t))
	if err != nil {
		t.Fatalf("NewEmbedded: %v", err)
	}
	return client
}

// The shared provider suite also runs through the client, proving that each
// contract family reaches the in-process provider unchanged.
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
