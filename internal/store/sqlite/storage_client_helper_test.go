package sqlite

import (
	"testing"

	"github.com/orka-agents/orka/internal/storageclient"
	"github.com/orka-agents/orka/internal/storageclient/storageclienttest"
)

// orkaClient wraps s in the storage client. Orka's redaction, payload
// sanitization and usage attribution run in the client, so tests that cover
// those behaviors end to end write through it.
func orkaClient(t *testing.T, s *Store) *storageclient.Client {
	t.Helper()
	client := storageclienttest.NewEmbedded(t, s)
	return client
}
