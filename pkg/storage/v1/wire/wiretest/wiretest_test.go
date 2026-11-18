package wiretest_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/orka-agents/orka/pkg/storage/v1/wire/wiretest"
)

// slowPlans answers GetPlan only once released, as a provider that does not
// watch its context might. Every other method is unimplemented.
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

// The loopback's cleanup waits for a handler that is still running after its
// call was cancelled, so nothing the test closes afterwards is still in use.
func TestCleanupWaitsForRunningHandlers(t *testing.T) {
	provider := slowPlans{entered: make(chan struct{}), release: make(chan struct{}), finished: new(atomic.Bool)}
	t.Run("loopback", func(t *testing.T) {
		client := wiretest.Serve(t, provider)
		ctx, cancel := context.WithCancel(t.Context())
		go func() {
			<-provider.entered
			cancel()
		}()
		_, err := client.GetPlan(ctx, "n", "t")
		running := !provider.finished.Load()
		// Release the handler whatever happens below, so a failure reports
		// rather than hanging the cleanup.
		time.AfterFunc(50*time.Millisecond, func() { close(provider.release) })
		require.ErrorIs(t, err, context.Canceled)
		require.True(t, running, "the handler returned before it was released")
	})
	require.True(t, provider.finished.Load(), "the loopback stopped before its handler returned")
}
