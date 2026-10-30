package wire_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/orka-agents/orka/pkg/storage/v1/wire"
	"github.com/orka-agents/orka/pkg/storage/v1/wire/wiretest"
)

// firstDescribeBlocks holds its first description until released, like a
// provider whose database is slow to answer the first time.
type firstDescribeBlocks struct {
	*recordingProvider
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (p *firstDescribeBlocks) Describe(ctx context.Context) (storagev1.ProviderInfo, error) {
	if p.calls.Add(1) == 1 {
		close(p.entered)
		<-p.release
	}
	return p.recordingProvider.Describe(ctx)
}

// observedMethods reports each finished call's method, without blocking the
// server.
type observedMethods chan string

func (o observedMethods) ObserveCall(call wire.CallInfo) {
	select {
	case o <- call.Method:
	default:
	}
}

// A call that waits for another call's first description of the providers
// stops waiting when its own deadline passes, while that description is
// still running, and the description still completes for everyone else.
func TestCallsWaitingForTheFirstDescriptionHonourTheirOwnDeadline(t *testing.T) {
	provider := &firstDescribeBlocks{recordingProvider: newRecordingProvider(),
		entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(provider.release) }) }
	t.Cleanup(release) // never leave the server blocked if the test fails early
	observed := make(observedMethods, 8)
	client := wire.NewClient(wiretest.Dial(t, wire.ServerConfig{State: provider, Observer: observed}), boundConfig)

	first := make(chan error, 1)
	go func() { first <- client.SavePlan(t.Context(), "n", "t", &storagev1.PlanState{}) }()
	<-provider.entered

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	_, err := client.GetPlan(ctx, "n", "t")
	require.Error(t, err)
	select {
	case method := <-observed:
		require.Equal(t, "GetPlan", method, "the waiting call ended on the server while the description still ran")
	case <-time.After(5 * time.Second):
		t.Fatal("the server kept the waiting call past its deadline")
	}
	select {
	case <-first:
		t.Fatal("the first call finished before its description was released")
	default:
	}

	release()
	require.NoError(t, <-first, "the description the first call started still completes")
	require.Len(t, provider.callsTo("SavePlan"), 1)
	require.Empty(t, provider.callsTo("GetPlan"), "the waiting call never reached the provider")
	_, err = client.GetPlan(t.Context(), "n", "t")
	require.NoError(t, err)
}
