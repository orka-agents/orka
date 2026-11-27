package storageclient_test

import (
	"testing"

	"github.com/orka-agents/orka/internal/storageclient"
	"github.com/orka-agents/orka/internal/storageclient/storageclienttest"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/orka-agents/orka/pkg/storage/v1/storagetest"
)

func newEmbeddedClient(t *testing.T) *storageclient.Client {
	t.Helper()
	backend := openSQLite(t)
	backend.RequireWriteEnvelopes()
	return storageclienttest.NewEmbedded(t, backend)
}

func TestClientStateConformance(t *testing.T) {
	storagetest.RunState(t, func(t *testing.T) storagev1.StateProvider {
		return newEmbeddedClient(t)
	}, storagetest.ProviderAttachesEnvelopes())
}

func TestClientArtifactConformance(t *testing.T) {
	storagetest.RunArtifacts(t, func(t *testing.T) storagev1.ArtifactProvider {
		return newEmbeddedClient(t)
	}, storagetest.ProviderAttachesEnvelopes())
}
