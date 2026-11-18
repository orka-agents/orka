package api

import (
	"github.com/orka-agents/orka/internal/storageclient"
	"github.com/orka-agents/orka/internal/storageclient/storageclienttest"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

// testStorageClient wraps an in-process SQLite store in the storage client,
// as production wiring does. Orka's side of the store protocols, such as
// fenced task data access and memory proposal application, lives there. Its
// writes carry the controller epoch the dataset holds.
func testStorageClient(backend *sqlite.Store) *storageclient.Client {
	client, err := storageclient.New(storageclient.Options{
		State: backend, Artifacts: backend, Embedded: backend,
		Fences: &storageclienttest.FollowDataset{Dataset: backend},
	})
	if err != nil {
		panic(err)
	}
	return client
}
