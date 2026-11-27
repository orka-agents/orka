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

var Key = bytes.Repeat([]byte{0x5a}, storageclient.SnapshotKeyBytes)

type TB interface {
	Helper()
	Fatalf(format string, args ...any)
}

type Store interface {
	storageclient.EmbeddedStore
	SetNativeSessionFinalizationSealer(sealer store.NativeSessionFinalizationSealer)
}

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

func Client(t TB, s Store) *storageclient.Client {
	t.Helper()
	client, err := New(s)
	if err != nil {
		t.Fatalf("create storage client with an active snapshot key: %v", err)
	}
	return client
}

const (
	Holder       = "storageclienttest-controller"
	Installation = "storageclienttest-installation"
)

type Dataset interface {
	storagev1.ProviderDescriber
	storagev1.ControllerEpochMirror
	storagev1.ControllerEpochAdvancer
}

type FollowDataset struct {
	Dataset Dataset

	mu sync.Mutex
}

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

func InitialEpoch() storagev1.ControllerEpoch {
	acquired := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	return storagev1.ControllerEpoch{
		Name: storagev1.DefaultControllerEpochName, Epoch: 1, Version: 1, HolderID: Holder,
		RequestDigest: storagev1.CanonicalBytesDigest([]byte(Holder)),
		AcquiredAt:    acquired, UpdatedAt: acquired,
	}
}

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
