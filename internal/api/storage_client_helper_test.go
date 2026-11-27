package api

import (
	"github.com/orka-agents/orka/internal/storageclient"
	"github.com/orka-agents/orka/internal/storageclient/storageclienttest"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

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
