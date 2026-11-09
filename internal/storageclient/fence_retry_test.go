package storageclient

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/orka-agents/orka/internal/store"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// closingFences opens its fence for a write's first attempt and is closed
// with err from then on, as the controller epoch manager's fence is once it
// holds the installation for storage recovery.
type closingFences struct {
	datasetID string
	fence     storagev1.ControllerEpochFence
	err       error
	calls     atomic.Int32
}

func (f *closingFences) WriteFence(context.Context, string) (string, storagev1.ControllerEpochFence, error) {
	if f.calls.Add(1) > 1 {
		return "", storagev1.ControllerEpochFence{}, f.err
	}
	return f.datasetID, f.fence, nil
}

// A fence that closes after a write's first attempt refuses the write's
// retries: a write that committed nothing is refused as not ready, one whose
// outcome is unknown is resolved instead of retried, and one that committed
// still returns its result.
func TestRetryAfterTheFenceClosedIsRefused(t *testing.T) {
	hold := &store.StorageHoldError{Hold: store.StorageRecoveryHold{
		ID: "hold-1", Reason: store.StorageRecoveryWatermarkRegressed, Message: "the dataset lost writes",
	}}
	newFixture := func(t *testing.T) *lossyFixture {
		f := newLossyFixture(t)
		f.client.ops.fencesMu.Lock()
		f.client.ops.fences = &closingFences{datasetID: f.datasetID, fence: testFence, err: hold}
		f.client.ops.fencesMu.Unlock()
		return f
	}
	requireNoResult := func(t *testing.T, f *lossyFixture) {
		t.Helper()
		if _, err := f.backend.GetResult(t.Context(), "tenant", "task"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("result after the refused retry = %v, want none", err)
		}
	}
	t.Run("nothing committed", func(t *testing.T) {
		f := newFixture(t)
		f.lossy.plan("SaveResult", stepUnavailable)
		err := f.client.SaveResult(t.Context(), "tenant", "task", []byte("result"))
		if !errors.Is(err, storagev1.ErrNotReady) || !errors.Is(err, store.ErrStorageRecoveryRequired) {
			t.Fatalf("SaveResult error = %v, want the hold, not ready", err)
		}
		requireRetriedWithOneEnvelope(t, f, "SaveResult", 1)
		if resolutions := len(f.lossy.seen("ResolveOperation")); resolutions != 0 {
			t.Fatalf("resolutions = %d, want none for an attempt that committed nothing", resolutions)
		}
		requireNoResult(t, f)
	})
	t.Run("outcome unknown", func(t *testing.T) {
		f := newFixture(t)
		f.lossy.plan("SaveResult", stepUnknown)
		err := f.client.SaveResult(t.Context(), "tenant", "task", []byte("result"))
		if !errors.Is(err, storagev1.ErrOperationAborted) || !errors.Is(err, store.ErrStorageRecoveryRequired) {
			t.Fatalf("SaveResult error = %v, want the hold, resolved as aborted", err)
		}
		requireRetriedWithOneEnvelope(t, f, "SaveResult", 1)
		if resolutions := len(f.lossy.seen("ResolveOperation")); resolutions != 1 {
			t.Fatalf("resolutions = %d, want 1", resolutions)
		}
		if f.client.UnresolvedOperations() != 0 {
			t.Fatal("an aborted operation still blocks the dataset")
		}
		requireNoResult(t, f)
	})
	t.Run("committed with its response lost", func(t *testing.T) {
		f := newFixture(t)
		f.lossy.plan("SaveResult", stepLose)
		if err := f.client.SaveResult(t.Context(), "tenant", "task", []byte("result")); err != nil {
			t.Fatalf("SaveResult: %v, want the committed result", err)
		}
		if resolutions := len(f.lossy.seen("ResolveOperation")); resolutions != 1 {
			t.Fatalf("resolutions = %d, want 1", resolutions)
		}
		if got, err := f.backend.GetResult(t.Context(), "tenant", "task"); err != nil || string(got) != "result" {
			t.Fatalf("result = %q, %v", got, err)
		}
	})
}
