package controller

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/storageclient"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// blockingEpochDataset holds every advance until release is closed.
type blockingEpochDataset struct {
	*recordingEpochDataset
	release chan struct{}
}

func (d *blockingEpochDataset) AdvanceControllerEpoch(
	ctx context.Context, request storagev1.AdvanceControllerEpochRequest,
) (storagev1.ControllerEpochMirrorState, error) {
	select {
	case <-d.release:
	case <-ctx.Done():
		return storagev1.ControllerEpochMirrorState{}, ctx.Err()
	}
	return d.recordingEpochDataset.AdvanceControllerEpoch(ctx, request)
}

func startEpochManager(t *testing.T, manager *ControllerEpochManager) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- manager.Start(ctx) }()
	t.Cleanup(cancel)
	return cancel, done
}

func writeFenceWithin(manager *ControllerEpochManager, contract string, wait time.Duration) (string, store.ControllerEpochFence, error) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	return manager.WriteFence(ctx, contract)
}

// An artifact store that cannot advance at takeover does not hold back the
// state store: state writes and CurrentFence open as soon as the state
// dataset has advanced, and artifact writes wait for their own dataset.
func TestControllerEpochManagerOpensEachStorageDatasetIndependently(t *testing.T) {
	ctx := context.Background()
	authority, _ := newKubernetesControllerEpochAuthority(t)
	state := newRecordingEpochDataset("dataset-state")
	// A description that fails for a reason retrying can fix is retried.
	state.describeErrs = []error{errors.New("connection refused")}
	artifacts := &blockingEpochDataset{recordingEpochDataset: newRecordingEpochDataset("dataset-artifacts"), release: make(chan struct{})}
	manager := NewControllerEpochManager(authority, "controller-a").
		WithStorage(storagev1.StateContract, state).
		WithStorage(storagev1.ArtifactsContract, artifacts)
	cancel, done := startEpochManager(t, manager)

	fence, err := fenceWithTimeout(manager)
	if err != nil {
		t.Fatalf("CurrentFence while the artifact store is held: %v", err)
	}
	datasetID, stateFence, err := writeFenceWithin(manager, storagev1.StateContract, 5*time.Second)
	if err != nil || datasetID != "dataset-state" || stateFence != fence {
		t.Fatalf("state write fence = %q, %#v, %v; want dataset-state, %#v", datasetID, stateFence, err, fence)
	}
	if _, _, err := writeFenceWithin(manager, storagev1.ArtifactsContract, 100*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("artifact write fence before its advance = %v, want it still closed", err)
	}

	close(artifacts.release)
	datasetID, artifactFence, err := writeFenceWithin(manager, storagev1.ArtifactsContract, 5*time.Second)
	if err != nil || datasetID != "dataset-artifacts" || artifactFence != fence {
		t.Fatalf("artifact write fence = %q, %#v, %v; want dataset-artifacts, %#v", datasetID, artifactFence, err, fence)
	}

	acquired, err := authority.GetControllerEpoch(ctx, fence.Name)
	if err != nil {
		t.Fatal(err)
	}
	installationID, err := authority.ControllerEpochInstallationID(ctx, fence.Name)
	if err != nil {
		t.Fatal(err)
	}
	for name, dataset := range map[string]*recordingEpochDataset{"state": state, "artifacts": artifacts.recordingEpochDataset} {
		requests := dataset.advanceRequests()
		if len(requests) != 1 {
			t.Fatalf("%s advances = %d, want 1", name, len(requests))
		}
		if !controllerEpochsEqual(&requests[0].Epoch, acquired) || requests[0].InstallationID != installationID ||
			requests[0].DatasetID != dataset.datasetID {
			t.Fatalf("%s advance = %#v, want the acquired epoch %#v under installation %q", name, requests[0], acquired, installationID)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// Contracts served by one dataset share one advance.
func TestControllerEpochManagerAdvancesASharedDatasetOnce(t *testing.T) {
	authority, _ := newKubernetesControllerEpochAuthority(t)
	dataset := newRecordingEpochDataset("dataset-shared")
	manager := NewControllerEpochManager(authority, "controller-a").
		WithStorage(storagev1.StateContract, dataset).
		WithStorage(storagev1.ArtifactsContract, dataset)
	cancel, done := startEpochManager(t, manager)
	for _, contract := range []string{storagev1.StateContract, storagev1.ArtifactsContract} {
		datasetID, _, err := writeFenceWithin(manager, contract, 5*time.Second)
		if err != nil || datasetID != "dataset-shared" {
			t.Fatalf("%s write fence = %q, %v", contract, datasetID, err)
		}
	}
	if advances := len(dataset.advanceRequests()); advances != 1 {
		t.Fatalf("advances of the shared dataset = %d, want 1", advances)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A dataset that refuses the advance, because it belongs to another
// installation or holds a newer epoch, stops the manager before any fence
// opens. A transient failure is retried instead.
func TestControllerEpochManagerStopsWhenADatasetRefusesTheAdvance(t *testing.T) {
	authority, _ := newKubernetesControllerEpochAuthority(t)
	dataset := newRecordingEpochDataset("dataset-a")
	dataset.advanceErrs = []error{
		store.ErrUnavailable,
		store.FencedErrorf("the dataset belongs to another installation"),
	}
	manager := NewControllerEpochManager(authority, "controller-a").WithStorage(storagev1.StateContract, dataset)
	_, done := startEpochManager(t, manager)
	select {
	case err := <-done:
		if !errors.Is(err, store.ErrFenced) {
			t.Fatalf("Start error = %v, want ErrFenced", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Start did not stop on a refused advance")
	}
	if _, ok := manager.Current(); ok {
		t.Fatal("CurrentFence opened although the state dataset refused the advance")
	}
	if _, _, err := writeFenceWithin(manager, storagev1.StateContract, 50*time.Millisecond); err == nil {
		t.Fatal("the state write fence opened although its dataset refused the advance")
	}
	if _, _, err := writeFenceWithin(manager, "other.v1", time.Second); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("write fence of an unserved contract = %v, want an immediate error", err)
	}
}

// heldWriteStore holds SaveResult after it reached the provider with its
// write envelope, as a slow network would deliver it late.
type heldWriteStore struct {
	*sqlite.Store
	entered     chan struct{}
	release     chan struct{}
	enteredOnce sync.Once
}

func (s *heldWriteStore) SaveResult(ctx context.Context, key store.ResultKey, data []byte) error {
	if _, ok := storagev1.WriteEnvelopeFrom(ctx); !ok {
		return errors.New("the storage client sent a write without an envelope")
	}
	s.enteredOnce.Do(func() { close(s.entered) })
	<-s.release
	return s.Store.SaveResult(ctx, key, data)
}

func newFencedStorageClient(t *testing.T, backend storageclient.EmbeddedStore, manager *ControllerEpochManager) *storageclient.Client {
	t.Helper()
	client, err := storageclient.NewEmbedded(backend)
	if err != nil {
		t.Fatal(err)
	}
	for contract, dataset := range client.Datasets() {
		manager.WithStorage(contract, dataset)
	}
	if err := client.BindFences(manager); err != nil {
		t.Fatal(err)
	}
	return client
}

// A write the replaced controller issued before the takeover, and that
// reaches the store only after the successor advanced it, changes nothing.
func TestReplacedControllerDelayedWriteIsFencedAfterTakeover(t *testing.T) {
	ctx := context.Background()
	authority, _ := newKubernetesControllerEpochAuthority(t)
	backend, err := sqlite.OpenLockedStore(filepath.Join(t.TempDir(), "orka.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	backend.RequireWriteEnvelopes()

	held := &heldWriteStore{Store: backend, entered: make(chan struct{}), release: make(chan struct{})}
	replaced := NewControllerEpochManager(authority, "controller-replaced")
	replacedClient := newFencedStorageClient(t, held, replaced)
	startEpochManager(t, replaced)
	if _, err := fenceWithTimeout(replaced); err != nil {
		t.Fatal(err)
	}
	delayed := make(chan error, 1)
	go func() {
		delayed <- replacedClient.SaveResult(ctx, store.ResultKey{Namespace: "tenant", TaskName: "task", TaskUID: "task-uid"}, []byte("stale"))
	}()
	select {
	case <-held.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the replaced controller's write never reached the store")
	}

	successor := NewControllerEpochManager(authority, "controller-successor")
	successorClient := newFencedStorageClient(t, backend, successor)
	startEpochManager(t, successor)
	fence, err := fenceWithTimeout(successor)
	if err != nil || fence.Epoch != 2 {
		t.Fatalf("successor fence = %#v, %v; want epoch 2", fence, err)
	}

	close(held.release)
	if err := <-delayed; !errors.Is(err, store.ErrFenced) {
		t.Fatalf("delayed write error = %v, want ErrFenced", err)
	}
	if _, err := backend.GetResult(ctx, store.ResultKey{Namespace: "tenant", TaskName: "task", TaskUID: "task-uid"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("result after the fenced write = %v, want none", err)
	}
	if err := successorClient.SaveResult(ctx, store.ResultKey{Namespace: "tenant", TaskName: "task", TaskUID: "task-uid"}, []byte("current")); err != nil {
		t.Fatalf("successor write: %v", err)
	}
	if got, err := backend.GetResult(ctx, store.ResultKey{Namespace: "tenant", TaskName: "task", TaskUID: "task-uid"}); err != nil || string(got) != "current" {
		t.Fatalf("result = %q, %v; want the successor's", got, err)
	}
}
