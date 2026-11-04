package wire_test

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/orka-agents/orka/pkg/storage/v1/wire"
	"github.com/orka-agents/orka/pkg/storage/v1/wire/storagepb"
	"github.com/orka-agents/orka/pkg/storage/v1/wire/wiretest"
)

// bulkLimits allow one streaming call at a time for at most limit.
func bulkLimits(limit time.Duration) wire.Limits {
	return wire.Limits{
		MaxConcurrent: map[wire.Class]int{wire.ClassBulk: 1},
		MaxDuration:   map[wire.Class]time.Duration{wire.ClassBulk: limit},
	}
}

// An upload that stalls without ending its stream, and with no deadline of
// its own, is ended at the bulk duration limit without reaching the
// provider, and its slot is free again.
func TestServerEndsStalledUploadsAtTheDurationLimit(t *testing.T) {
	provider := newRecordingProvider()
	config := serveBoth(provider)
	config.Limits = bulkLimits(300 * time.Millisecond)
	conn := wiretest.Dial(t, config)
	data := pattern(2 * wire.MaxChunkBytes)

	stream, err := storagepb.NewResultsClient(conn).SaveResult(uploadContext(t.Context(), len(data), digestOf(data)))
	require.NoError(t, err)
	require.NoError(t, stream.Send(&storagepb.SaveResultRequest{Namespace: "n", TaskName: "t", Data: data[:wire.MaxChunkBytes]}))
	started := time.Now()
	ended := make(chan error, 1)
	go func() { ended <- stream.RecvMsg(&storagepb.SaveResultResponse{}) }()
	select {
	case err := <-ended:
		_, reason := reasonOf(t, err)
		require.Equal(t, wire.ReasonUnavailable, reason, "nothing was committed")
		require.Less(t, time.Since(started), 5*time.Second)
	case <-time.After(10 * time.Second):
		t.Fatal("the stalled upload was never ended")
	}
	require.Empty(t, provider.callsTo("SaveResult"))

	client := wire.NewClient(conn, boundConfig)
	require.NoError(t, client.SaveResult(t.Context(), "n", "t", []byte("r")), "the bulk slot is free again")
}

// A download whose reader stops reading is ended at the bulk duration limit,
// and its slot is free again.
func TestServerEndsStalledDownloadsAtTheDurationLimit(t *testing.T) {
	provider := newRecordingProvider()
	provider.set("GetResult", []any{pattern(16 * wire.MaxChunkBytes)}, nil)
	config := serveBoth(provider)
	config.Limits = bulkLimits(300 * time.Millisecond)
	conn := wiretest.Dial(t, config)

	stalled, err := storagepb.NewResultsClient(conn).GetResult(contractContext(t.Context()), &storagepb.GetResultRequest{})
	require.NoError(t, err)
	_, err = stalled.Header()
	require.NoError(t, err, "the download started")

	// A small download finishes well inside the limit, so it succeeds as
	// soon as the stalled one has released the only bulk slot.
	provider.set("GetArtifact", []any{[]byte("small"), "text/plain"}, nil)
	client := wire.NewClient(conn, boundConfig)
	require.Eventually(t, func() bool {
		_, _, err := client.GetArtifact(t.Context(), storagev1.ArtifactKey{Filename: "f"})
		return err == nil
	}, 10*time.Second, 50*time.Millisecond, "the stalled download releases its slot at the limit")
}

// slowDescriber describes its dataset only when its context ends, like a
// provider whose database does not answer.
type slowDescriber struct {
	*recordingProvider
	entered chan struct{}
}

func (p slowDescriber) Describe(ctx context.Context) (storagev1.ProviderInfo, error) {
	select {
	case p.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return storagev1.ProviderInfo{}, ctx.Err()
}

// The first description of the providers runs inside the call's admission
// and duration limit: other calls are shed rather than queued behind it, and
// it ends at the limit.
func TestServerBoundsTheFirstDescription(t *testing.T) {
	provider := slowDescriber{recordingProvider: newRecordingProvider(), entered: make(chan struct{}, 1)}
	client := wire.NewClient(wiretest.Dial(t, wire.ServerConfig{State: provider, Limits: wire.Limits{
		MaxConcurrent: map[wire.Class]int{wire.ClassControl: 1},
		MaxDuration:   map[wire.Class]time.Duration{wire.ClassControl: 2 * time.Second},
	}}), boundConfig)

	first := make(chan error, 1)
	started := time.Now()
	go func() { first <- client.SavePlan(t.Context(), "n", "t", &storagev1.PlanState{}) }()
	<-provider.entered

	err := client.SavePlan(t.Context(), "n", "t", &storagev1.PlanState{})
	require.ErrorIs(t, err, storagev1.ErrUnavailable)
	select {
	case <-first:
		t.Fatal("the second call waited for the first description to end")
	default:
	}

	err = <-first
	require.ErrorIs(t, err, storagev1.ErrUnavailable)
	require.False(t, errors.Is(err, storagev1.ErrOutcomeUnknown))
	require.Less(t, time.Since(started), 5*time.Second, "the description ends at the duration limit")
	require.Empty(t, provider.callsTo("SavePlan"))
}

// orderedDescriber blocks its first description, after reading its dataset,
// until released, so a newer description can finish first.
type orderedDescriber struct {
	*recordingProvider
	dataset atomic.Pointer[string]
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (p *orderedDescriber) Describe(context.Context) (storagev1.ProviderInfo, error) {
	info := describeDefault()
	info.DatasetID = *p.dataset.Load()
	if p.calls.Add(1) == 1 {
		close(p.entered)
		<-p.release
	}
	return info, nil
}

// A description that finishes after a newer one never restores the dataset
// the newer one replaced.
func TestServerKeepsTheNewestDescription(t *testing.T) {
	provider := &orderedDescriber{recordingProvider: newRecordingProvider(), entered: make(chan struct{}), release: make(chan struct{})}
	provider.dataset.Store(new("old-dataset"))
	server, err := wire.NewServer(wire.ServerConfig{State: provider})
	require.NoError(t, err)

	slow := make(chan error, 1)
	go func() { slow <- server.Refresh(t.Context()) }()
	<-provider.entered
	provider.dataset.Store(new("new-dataset"))
	require.NoError(t, server.Refresh(t.Context()))
	require.Equal(t, "new-dataset", server.DatasetID())
	close(provider.release)
	require.NoError(t, <-slow)
	require.Equal(t, "new-dataset", server.DatasetID(), "the older description does not overwrite the newer one")
}

// A download over the client's limit is refused from its headers, before any
// of its bytes are received.
func TestClientRefusesOversizedDownloadsFromTheirHeaders(t *testing.T) {
	provider := newRecordingProvider()
	server, err := wire.NewServer(serveBoth(provider))
	require.NoError(t, err)
	address := serveInsecureWith(t, server)
	counter := &chunkCounter{}
	conn, err := grpc.NewClient("passthrough:///"+address, append(wire.DialOptions(),
		grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithStatsHandler(counter))...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	client := wire.NewClient(conn, wire.ClientConfig{Dataset: recordingDataset, MaxBlobBytes: 1024})

	provider.set("GetResult", []any{pattern(4 * wire.MaxChunkBytes)}, nil)
	_, err = client.GetResult(t.Context(), "n", "t")
	var tooLarge storagev1.TooLargeError
	require.ErrorAs(t, err, &tooLarge)
	require.Equal(t, int64(4*wire.MaxChunkBytes), tooLarge.Size)
	require.Zero(t, counter.inbound.Load(), "no response message was received")

	provider.set("GetResult", []any{[]byte("small")}, nil)
	got, err := client.GetResult(t.Context(), "n", "t")
	require.NoError(t, err)
	require.True(t, bytes.Equal([]byte("small"), got))
	require.Positive(t, counter.inbound.Load())
}
