package adapterkit_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/orka-agents/orka/pkg/storage/v1/wire/adapterkit"
)

// When the shutdown grace period passes, Serve returns even while a handler
// is still settling a commit with a longer settle timeout.
func TestServeReturnsWhenTheShutdownGracePeriodPasses(t *testing.T) {
	f := newFixture(t)
	provider := newMemoryArtifacts()
	entered, settling := make(chan struct{}), make(chan struct{})
	finished := make(chan struct{})
	t.Cleanup(func() {
		close(settling)
		<-finished
	})
	provider.hold = func(ctx context.Context) {
		defer close(finished)
		close(entered)
		<-ctx.Done()
		settleCtx, cancel := adapterkit.SettleContext(ctx)
		defer cancel()
		select {
		case <-settling:
		case <-settleCtx.Done():
		}
	}
	config := f.bearerConfig(provider)
	config.SettleTimeout = time.Minute
	config.ShutdownTimeout = 300 * time.Millisecond
	running := start(t, f, config)
	client := bind(t, running.dial(t, nil, newTokenSource("first-token")))

	go func() {
		_ = client.SaveArtifact(t.Context(), storagev1.SaveArtifactRequest{
			Owner: storagev1.ArtifactOwner{Namespace: "n", TaskName: "t"}, Filename: "f", Data: []byte("x"),
		})
	}()
	<-entered
	stopped := time.Now()
	running.stop()
	select {
	case err := <-running.done:
		require.NoError(t, err)
		running.done <- nil
		require.Less(t, time.Since(stopped), 5*time.Second)
	case <-time.After(10 * time.Second):
		t.Fatal("Serve waited for a settling handler past its shutdown grace period")
	}
}
