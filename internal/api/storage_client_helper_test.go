package api

import (
	"path/filepath"
	"testing"

	"github.com/orka-agents/orka/internal/storageclient"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

// testStorageClient wraps an in-process SQLite store in the storage client,
// as production wiring does. Orka's side of the store protocols, such as
// fenced task data access and memory proposal application, lives there.
func testStorageClient(backend *sqlite.Store) *storageclient.Client {
	client, err := storageclient.NewEmbedded(backend)
	if err != nil {
		panic(err)
	}
	return client
}

// newRawSQLiteStoreForTest returns an in-process SQLite store for tests of
// SQLite-only control records, such as controller epochs and external
// effects, which the storage client does not serve.
func newRawSQLiteStoreForTest(t *testing.T) *sqlite.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "control.db")
	db, err := sqlite.NewDB(path)
	if err != nil {
		t.Fatalf("sqlite.NewDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return sqlite.NewStore(db, path)
}
