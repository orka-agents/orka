// Package storageclienttest builds storage clients for tests that write
// through the storage client without running the controller epoch manager.
package storageclienttest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/storageclient"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

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
// controller epoch the dataset holds (see FollowDataset).
func NewEmbedded(t testing.TB, backend storageclient.EmbeddedStore) *storageclient.Client {
	t.Helper()
	client, err := storageclient.New(storageclient.Options{
		State: backend, Artifacts: backend, Embedded: backend,
		Fences: &FollowDataset{Dataset: backend},
	})
	if err != nil {
		t.Fatalf("create storage client: %v", err)
	}
	return client
}
