// Package storageclienttest builds storage clients over the embedded SQLite
// store for tests in other packages.
package storageclienttest

import (
	"bytes"
	"context"

	"github.com/orka-agents/orka/internal/storageclient"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

// Key is the snapshot key the clients activate.
var Key = bytes.Repeat([]byte{0x5a}, storageclient.SnapshotKeyBytes)

// TB is the part of testing.TB that Client uses.
type TB interface {
	Helper()
	Fatalf(format string, args ...any)
}

// New wraps s in the storage client and activates Key, as controller
// startup does with the configured key. It also installs the client as the
// native session sealer of SQLite's duplicate SQL control implementation, so
// tests that finalize turns through that path can carry native state. Every
// client for one dataset must use the same key.
func New(s *sqlite.Store) (*storageclient.Client, error) {
	client, err := storageclient.NewEmbedded(s)
	if err != nil {
		return nil, err
	}
	if err := client.ActivateSnapshotKey(context.Background(), Key); err != nil {
		return nil, err
	}
	s.SetNativeSessionFinalizationSealer(client)
	return client, nil
}

// Client is New for a test, which it fails on error.
func Client(t TB, s *sqlite.Store) *storageclient.Client {
	t.Helper()
	client, err := New(s)
	if err != nil {
		t.Fatalf("create storage client with an active snapshot key: %v", err)
	}
	return client
}
