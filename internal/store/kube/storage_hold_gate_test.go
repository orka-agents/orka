package kube

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	controlstore "github.com/orka-agents/orka/internal/store"
)

func testStorageHold(id string) controlstore.StorageRecoveryHold {
	return controlstore.StorageRecoveryHold{
		ID: id, Reason: controlstore.StorageRecoveryWatermarkRegressed, Message: "dataset-a holds watermark 2",
		Contract: "state.v1", DatasetID: "dataset-a", AcknowledgedWatermark: 8, StoredWatermark: 2,
		Durability: "local-buffered", RaisedAt: testNow,
	}
}

// A recorded hold refuses every control-store change, also under a fence
// the controller obtained before the hold, until the hold is released.
func TestRecordedStorageHoldRefusesControlStoreChanges(t *testing.T) {
	ctx := t.Context()
	kubeStore, kubeClient, fence := newTestStoreWithEpoch(t)
	hold := testStorageHold("hold-1")
	if _, err := kubeStore.HoldStorage(ctx, fence, hold); err != nil {
		t.Fatalf("HoldStorage: %v", err)
	}
	called := false
	err := kubeStore.WithControllerEpochMutation(ctx, fence, func(context.Context) error { called = true; return nil })
	if called || !errors.Is(err, controlstore.ErrStorageRecoveryRequired) || !errors.Is(err, controlstore.ErrNotReady) {
		t.Fatalf("mutation while held ran=%t, err=%v; want the hold", called, err)
	}
	var holdErr *controlstore.StorageHoldError
	if !errors.As(err, &holdErr) || holdErr.Hold != hold {
		t.Fatalf("mutation error = %#v, want the recorded hold", err)
	}

	object := controllerEpochObject(t, kubeClient)
	object.Annotations[corev1alpha1.StorageRecoveryAcknowledgedAnnotation] = hold.ID
	if err := kubeClient.Update(ctx, object); err != nil {
		t.Fatalf("annotate: %v", err)
	}
	if err := kubeStore.ReleaseStorageHold(ctx, fence, hold.ID, nil); err != nil {
		t.Fatalf("ReleaseStorageHold: %v", err)
	}
	if err := kubeStore.WithControllerEpochMutation(ctx, fence, func(context.Context) error { called = true; return nil }); err != nil || !called {
		t.Fatalf("mutation after release ran=%t, err=%v", called, err)
	}
}

// A hold is recorded only once a control-store change in progress has
// finished, so no change commits after the hold.
func TestStorageHoldWaitsForAControlStoreChangeInProgress(t *testing.T) {
	ctx := t.Context()
	kubeStore, kubeClient, fence := newTestStoreWithEpoch(t)
	// A distinct Store records the hold, so the durable Lease interlock,
	// not this Store's local queue, is what makes it wait.
	recorder, err := New(kubeClient, testControlNamespace, WithAPIReader(kubeClient))
	if err != nil {
		t.Fatal(err)
	}
	started, finish := make(chan struct{}), make(chan struct{})
	mutated := make(chan error, 1)
	go func() {
		mutated <- kubeStore.WithControllerEpochMutation(ctx, fence, func(context.Context) error {
			close(started)
			<-finish
			return nil
		})
	}()
	<-started
	held := make(chan error, 1)
	go func() {
		_, err := recorder.HoldStorage(ctx, fence, testStorageHold("hold-1"))
		held <- err
	}()
	select {
	case err := <-held:
		t.Fatalf("HoldStorage returned %v while a control-store change was in progress", err)
	case <-time.After(300 * time.Millisecond):
	}
	if record := requireStorageRecord(t, kubeStore); record.Hold != nil {
		t.Fatalf("hold %#v recorded while a control-store change was in progress", record.Hold)
	}
	close(finish)
	if err := <-mutated; err != nil {
		t.Fatalf("mutation: %v", err)
	}
	select {
	case err := <-held:
		if err != nil {
			t.Fatalf("HoldStorage: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("HoldStorage did not finish after the change did")
	}
	if record := requireStorageRecord(t, kubeStore); record.Hold == nil || record.Hold.ID != "hold-1" {
		t.Fatalf("record = %#v, want the hold", record)
	}
}
