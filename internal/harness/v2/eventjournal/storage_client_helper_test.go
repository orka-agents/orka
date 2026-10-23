package eventjournal

import (
	"database/sql"
	"testing"

	"github.com/orka-agents/orka/internal/storageclient"
	"github.com/orka-agents/orka/internal/storageclient/storageclienttest"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

// orkaEventStore returns the storage client over a SQLite store on db, as
// production wires the journal. The client derives usage from journal events.
func orkaEventStore(t *testing.T, db *sql.DB) *storageclient.Client {
	t.Helper()
	client := storageclienttest.NewEmbedded(t, sqlite.NewStore(db, "usage"))
	return client
}
