package controller

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

func TestControllerEpochManagerCreatesAndAdvances(t *testing.T) {
	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck
	controlStore := sqlite.NewStore(db, "test")

	first := NewControllerEpochManager(controlStore, "controller-a")
	ctx1, cancel1 := context.WithCancel(context.Background())
	done1 := make(chan error, 1)
	go func() { done1 <- first.Start(ctx1) }()
	fence1, err := fenceWithTimeout(first)
	if err != nil {
		t.Fatal(err)
	}
	if fence1.Epoch != 1 || fence1.HolderID != "controller-a" {
		t.Fatalf("first fence = %#v", fence1)
	}
	cancel1()
	if err := <-done1; err != nil {
		t.Fatal(err)
	}

	second := NewControllerEpochManager(controlStore, "controller-b")
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan error, 1)
	go func() { done2 <- second.Start(ctx2) }()
	fence2, err := fenceWithTimeout(second)
	if err != nil {
		t.Fatal(err)
	}
	if fence2.Epoch != 2 || fence2.HolderID != "controller-b" {
		t.Fatalf("second fence = %#v", fence2)
	}
	cancel2()
	if err := <-done2; err != nil {
		t.Fatal(err)
	}
}

func TestControllerEpochManagerRequiresHolderAndStore(t *testing.T) {
	manager := NewControllerEpochManager(nil, "holder")
	if err := manager.Start(context.Background()); err == nil {
		t.Fatal("missing store unexpectedly accepted")
	}
	manager = NewControllerEpochManager(&conflictingEpochStore{}, "")
	if err := manager.Start(context.Background()); err == nil {
		t.Fatal("missing holder unexpectedly accepted")
	}
}

func TestControllerEpochDigestChangesAcrossTargets(t *testing.T) {
	first := controllerEpochDigest("epoch", "holder", 1, 1, 2)
	second := controllerEpochDigest("epoch", "holder", 2, 2, 3)
	if first == second {
		t.Fatal("epoch digests unexpectedly equal")
	}
	if err := store.ValidateCanonicalDigest("epoch digest", first); err != nil {
		t.Fatal(err)
	}
}

func fenceWithTimeout(manager *ControllerEpochManager) (store.ControllerEpochFence, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return manager.CurrentFence(ctx)
}

type conflictingEpochStore struct{}

func (*conflictingEpochStore) GetControllerEpoch(context.Context, string) (*store.ControllerEpoch, error) {
	return nil, store.ErrNotFound
}
func (*conflictingEpochStore) CompareAndSwapControllerEpoch(context.Context, store.ControllerEpochCAS) (*store.ControllerEpoch, error) {
	return nil, store.ErrConflict
}

var _ store.ControllerEpochStore = (*conflictingEpochStore)(nil)
