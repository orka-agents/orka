package sqlite

import (
	"testing"

	"github.com/orka-agents/orka/internal/storageclient"
)

// orkaClient wraps s in the storage client. Orka's redaction, payload
// sanitization and usage attribution run in the client, so tests that cover
// those behaviors end to end write through it.
func orkaClient(t *testing.T, s *Store) *storageclient.Client {
	t.Helper()
	client, err := storageclient.NewEmbedded(s)
	if err != nil {
		t.Fatalf("NewEmbedded: %v", err)
	}
	return client
}
