package api

import (
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
