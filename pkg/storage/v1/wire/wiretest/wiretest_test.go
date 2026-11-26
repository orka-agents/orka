package wiretest_test

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/orka-agents/orka/pkg/storage/v1/wire"
	"github.com/orka-agents/orka/pkg/storage/v1/wire/wiretest"
)

type slowPlans struct {
	storagev1.StateProvider
	entered  chan struct{}
	release  chan struct{}
	finished *atomic.Bool
}

func (slowPlans) Describe(context.Context) (storagev1.ProviderInfo, error) {
	return storagev1.ProviderInfo{DatasetID: "dataset-1"}, nil
}

func (p slowPlans) GetPlan(context.Context, string, string) (*storagev1.PlanState, error) {
	close(p.entered)
	<-p.release
	p.finished.Store(true)
	return nil, storagev1.ErrNotFound
}

type heldCleanups struct {
	*testing.T
	cleanups []func()
}

func (h *heldCleanups) Cleanup(cleanup func()) {
	h.cleanups = append(h.cleanups, cleanup)
}

func (h *heldCleanups) run() {
	for _, cleanup := range slices.Backward(h.cleanups) {
		cleanup()
	}
}

func TestCleanupWaitsForRunningHandlers(t *testing.T) {
	provider := slowPlans{entered: make(chan struct{}), release: make(chan struct{}), finished: new(atomic.Bool)}
	release := sync.OnceFunc(func() { close(provider.release) })
	loopback := &heldCleanups{T: t}
	stop := sync.OnceFunc(loopback.run)
	t.Cleanup(func() {
		release()
		stop()
	})
	client := wiretest.Serve(loopback, wire.ServerConfig{State: provider})

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	go func() {
		select {
		case <-provider.entered:
			cancel()
		case <-ctx.Done():
		}
	}()
	_, err := client.GetPlan(ctx, "n", "t")
	require.ErrorIs(t, err, context.Canceled, "the call reaches the provider and is cancelled there")
	require.False(t, provider.finished.Load(), "the handler returned before it was released")

	stopped := make(chan struct{})
	go func() {
		stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("the loopback stopped while its handler was still running")
	case <-time.After(200 * time.Millisecond):
	}
	release()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("the loopback did not stop after its handler returned")
	}
	require.True(t, provider.finished.Load())
}
