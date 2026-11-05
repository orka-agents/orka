// Package storageclienttest builds storage clients over the embedded SQLite
// store for tests in other packages. The clients' writes carry the
// controller epoch the dataset holds (see FollowDataset), so tests write
// through the storage client without running the controller epoch manager.
package storageclienttest

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/storageclient"
	"github.com/orka-agents/orka/internal/store"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// Key is the snapshot key the clients activate.
var Key = bytes.Repeat([]byte{0x5a}, storageclient.SnapshotKeyBytes)

// TB is the part of testing.TB that Client uses.
type TB interface {
	Helper()
	Fatalf(format string, args ...any)
}

// Store is the embedded store New wraps, such as *sqlite.Store.
type Store interface {
	storageclient.EmbeddedStore
	SetNativeSessionFinalizationSealer(sealer store.NativeSessionFinalizationSealer)
}

// New wraps s in the storage client and activates Key, as controller
// startup does with the configured key. The client's writes carry the
// controller epoch s holds (see FollowDataset). It also installs the client
// as the native session sealer of SQLite's duplicate SQL control
// implementation, so tests that finalize turns through that path can carry
// native state. Every client for one dataset must use the same key.
func New(s Store) (*storageclient.Client, error) {
	client, err := newFollowing(s)
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
func Client(t TB, s Store) *storageclient.Client {
	t.Helper()
	client, err := New(s)
	if err != nil {
		t.Fatalf("create storage client with an active snapshot key: %v", err)
	}
	return client
}

// Holder and Installation identify the controller epoch FollowDataset gives
// a dataset that holds none.
const (
	Holder       = "storageclienttest-controller"
	Installation = "storageclienttest-installation"
)

// Dataset is what FollowDataset needs from a provider.
type Dataset interface {
	storagev1.ProviderDescriber
	storagev1.ControllerEpochMirror
	storagev1.ControllerEpochAdvancer
}

// FollowDataset is a fence source that serves, for every contract, the
// controller epoch the dataset holds when the write is issued, as a
// controller that owns the dataset would. A dataset that holds no epoch is
// given epoch 1 of Holder first, through SyncControllerEpochMirror, which
// binds no installation, so a test can still advance it or synchronize it
// itself.
type FollowDataset struct {
	Dataset Dataset

	mu sync.Mutex
}

// WriteFence implements storageclient.FenceSource.
func (f *FollowDataset) WriteFence(ctx context.Context, _ string) (string, storagev1.ControllerEpochFence, error) {
	info, err := f.Dataset.Describe(ctx)
	if err != nil {
		return "", storagev1.ControllerEpochFence{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	state, err := f.Dataset.GetControllerEpochMirror(ctx, storagev1.DefaultControllerEpochName)
	if errors.Is(err, storagev1.ErrNotFound) {
		if err := f.Dataset.SyncControllerEpochMirror(ctx, InitialEpoch()); err != nil {
			return "", storagev1.ControllerEpochFence{}, err
		}
		state, err = f.Dataset.GetControllerEpochMirror(ctx, storagev1.DefaultControllerEpochName)
	}
	if err != nil {
		return "", storagev1.ControllerEpochFence{}, err
	}
	return info.DatasetID, storagev1.ControllerEpochFence{
		Name: state.Epoch.Name, Epoch: state.Epoch.Epoch, HolderID: state.Epoch.HolderID,
	}, nil
}

// InitialEpoch is the epoch FollowDataset gives a dataset that holds none.
func InitialEpoch() storagev1.ControllerEpoch {
	acquired := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	return storagev1.ControllerEpoch{
		Name: storagev1.DefaultControllerEpochName, Epoch: 1, Version: 1, HolderID: Holder,
		RequestDigest: storagev1.CanonicalBytesDigest([]byte(Holder)),
		AcquiredAt:    acquired, UpdatedAt: acquired,
	}
}

// NewEmbedded returns a storage client over backend whose writes carry the
// controller epoch the dataset holds (see FollowDataset). It activates no
// snapshot key.
func NewEmbedded(t testing.TB, backend storageclient.EmbeddedStore) *storageclient.Client {
	t.Helper()
	client, err := newFollowing(backend)
	if err != nil {
		t.Fatalf("create storage client: %v", err)
	}
	return client
}

func newFollowing(backend storageclient.EmbeddedStore) (*storageclient.Client, error) {
	return storageclient.New(storageclient.Options{
		State: backend, Artifacts: backend, Embedded: backend,
		Fences: &FollowDataset{Dataset: backend},
	})
}
