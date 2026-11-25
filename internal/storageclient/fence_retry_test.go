package storageclient

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

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

// stallingFences opens its fence for a write's first attempt and then waits
// on every later call until its context ends, as a fence source does whose
// own reads stall, without the fence ever closing. It closes stalled when
// the first such wait starts, and returns endErr, or the context's error
// wrapped when endErr is nil, once the context ends.
type stallingFences struct {
	datasetID string
	fence     storagev1.ControllerEpochFence
	endErr    error
	calls     atomic.Int32
	stalled   chan struct{}
}

func (f *stallingFences) WriteFence(ctx context.Context, _ string) (string, storagev1.ControllerEpochFence, error) {
	if call := f.calls.Add(1); call > 1 {
		if call == 2 {
			close(f.stalled)
		}
		<-ctx.Done()
		if f.endErr != nil {
			return "", storagev1.ControllerEpochFence{}, f.endErr
		}
		return "", storagev1.ControllerEpochFence{}, fmt.Errorf("read dataset installation: %w", ctx.Err())
	}
	return f.datasetID, f.fence, nil
}

// A retry whose fence wait outlives the call or its retry horizon ends the
// retries as when they end between attempts: the write returns the last
// attempt's error, with the call's own context error when the call ended,
// or is resolved when an attempt's outcome was unknown. A fence that stays
// closed for the whole fence wait while the call is live refuses the retry
// as not ready.
func TestRetryFenceWaitThatOutlivesTheCallKeepsTheLastError(t *testing.T) {
	newFixture := func(t *testing.T, fenceWait, horizon time.Duration, first scriptedStep) (*lossyFixture, *stallingFences) {
		f := newLossyFixture(t)
		fences := &stallingFences{datasetID: f.datasetID, fence: testFence, stalled: make(chan struct{})}
		f.client.ops.fencesMu.Lock()
		f.client.ops.fences = fences
		f.client.ops.fencesMu.Unlock()
		f.client.ops.fenceWait = fenceWait
		f.client.ops.retryHorizon = horizon
		f.lossy.plan("SaveResult", first)
		return f, fences
	}
	requireNotRefused := func(t *testing.T, err error) {
		t.Helper()
		if errors.Is(err, storagev1.ErrNotReady) {
			t.Fatalf("SaveResult error = %v, refused as not ready although the fence never closed", err)
		}
	}
	t.Run("the call's deadline", func(t *testing.T) {
		f, fences := newFixture(t, time.Hour, time.Hour, stepUnavailable)
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		defer cancel()
		err := f.client.SaveResult(ctx, "tenant", "task", []byte("result"))
		requireNotRefused(t, err)
		if !errors.Is(err, storagev1.ErrUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("SaveResult error = %v, want the last attempt's ErrUnavailable with the call's deadline", err)
		}
		if fences.calls.Load() < 2 {
			t.Fatal("the retry never waited for the fence")
		}
		requireRetriedWithOneEnvelope(t, f, "SaveResult", 1)
	})
	t.Run("the call cancelled", func(t *testing.T) {
		f, fences := newFixture(t, time.Hour, time.Hour, stepUnavailable)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() {
			<-fences.stalled
			cancel()
		}()
		err := f.client.SaveResult(ctx, "tenant", "task", []byte("result"))
		requireNotRefused(t, err)
		if !errors.Is(err, storagev1.ErrUnavailable) || !errors.Is(err, context.Canceled) {
			t.Fatalf("SaveResult error = %v, want the last attempt's ErrUnavailable with the cancellation", err)
		}
		requireRetriedWithOneEnvelope(t, f, "SaveResult", 1)
	})
	t.Run("a source error other than the context's", func(t *testing.T) {
		f, fences := newFixture(t, time.Hour, time.Hour, stepUnavailable)
		fences.endErr = errors.New("sql: transaction has already been committed or rolled back")
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() {
			<-fences.stalled
			cancel()
		}()
		err := f.client.SaveResult(ctx, "tenant", "task", []byte("result"))
		requireNotRefused(t, err)
		if !errors.Is(err, storagev1.ErrUnavailable) || !errors.Is(err, context.Canceled) {
			t.Fatalf("SaveResult error = %v, want the last attempt's ErrUnavailable with the cancellation", err)
		}
	})
	t.Run("an unknown outcome", func(t *testing.T) {
		f, _ := newFixture(t, time.Hour, time.Hour, stepUnknown)
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		defer cancel()
		err := f.client.SaveResult(ctx, "tenant", "task", []byte("result"))
		requireNotRefused(t, err)
		if !errors.Is(err, storagev1.ErrOperationAborted) {
			t.Fatalf("SaveResult error = %v, want the operation resolved as aborted", err)
		}
		if resolutions := len(f.lossy.seen("ResolveOperation")); resolutions != 1 {
			t.Fatalf("resolutions = %d, want 1", resolutions)
		}
		requireRetriedWithOneEnvelope(t, f, "SaveResult", 1)
	})
	t.Run("the retry horizon", func(t *testing.T) {
		f, _ := newFixture(t, time.Hour, 200*time.Millisecond, stepUnavailable)
		err := f.client.SaveResult(t.Context(), "tenant", "task", []byte("result"))
		requireNotRefused(t, err)
		if !errors.Is(err, storagev1.ErrUnavailable) {
			t.Fatalf("SaveResult error = %v, want the last attempt's ErrUnavailable", err)
		}
		requireRetriedWithOneEnvelope(t, f, "SaveResult", 1)
	})
	t.Run("the fence wait while the call is live", func(t *testing.T) {
		f, _ := newFixture(t, 100*time.Millisecond, time.Hour, stepUnavailable)
		err := f.client.SaveResult(t.Context(), "tenant", "task", []byte("result"))
		if !errors.Is(err, storagev1.ErrNotReady) {
			t.Fatalf("SaveResult error = %v, want ErrNotReady for a fence that stayed closed", err)
		}
		requireRetriedWithOneEnvelope(t, f, "SaveResult", 1)
	})
}
