package adapterkit_test

import (
	"context"
	"errors"
	"io"
	"net"
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
		// Only a call that reached the provider has a handler to wait for.
		select {
		case <-entered:
		default:
			return
		}
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			t.Error("the settling handler did not finish")
		}
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

	saved := make(chan error, 1)
	go func() {
		saved <- client.SaveArtifact(t.Context(), storagev1.SaveArtifactRequest{
			Owner: storagev1.ArtifactOwner{Namespace: "n", TaskName: "t"}, Filename: "f", Data: []byte("x"),
		})
	}()
	awaitEntry(t, entered, saved)
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

// A forced stop closes every connection before Serve returns, including one
// still in its TLS handshake, which the gRPC server's Stop would wait for
// rather than close.
func TestServeClosesHandshakingConnectionsOnAForcedStop(t *testing.T) {
	f := newFixture(t)
	config := f.bearerConfig(newMemoryArtifacts())
	config.ShutdownTimeout = 300 * time.Millisecond
	running := start(t, f, config)
	running.waitReady(t, true)

	stalled, err := net.Dial("tcp", running.address)
	require.NoError(t, err)
	t.Cleanup(func() { _ = stalled.Close() })
	// The connection never sends its ClientHello, so its handshake stays
	// open until the gRPC server's connection timeout.
	time.Sleep(50 * time.Millisecond)
	running.stop()
	select {
	case err := <-running.done:
		require.NoError(t, err)
		running.done <- nil
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return")
	}
	require.NoError(t, stalled.SetReadDeadline(time.Now().Add(2*time.Second)))
	_, err = io.Copy(io.Discard, stalled)
	var timeout net.Error
	require.False(t, errors.As(err, &timeout) && timeout.Timeout(), "Serve returned with the connection still open")
}
