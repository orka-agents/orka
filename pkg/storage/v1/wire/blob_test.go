package wire_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/stats"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/orka-agents/orka/pkg/storage/v1/wire"
	"github.com/orka-agents/orka/pkg/storage/v1/wire/storagepb"
	"github.com/orka-agents/orka/pkg/storage/v1/wire/wiretest"
)

// pattern returns size bytes that differ at every position modulo 251, so a
// reordered or dropped chunk changes the content.
func pattern(size int) []byte {
	data := make([]byte, size)
	for index := range data {
		data[index] = byte(index % 251)
	}
	return data
}

// Blobs larger than one message cross the wire whole, in both directions,
// with nested blobs, absent records and empty blobs preserved.
func TestBlobsCrossTheWireWhole(t *testing.T) {
	provider := newRecordingProvider()
	client := wiretest.Serve(t, serveBoth(provider))
	data := pattern(3*wire.MaxChunkBytes + 12345)

	request := storagev1.SaveArtifactRequest{
		Owner:    storagev1.ArtifactOwner{Namespace: "n", TaskName: "t", TaskUID: "uid"},
		Filename: "report.bin", ContentType: "application/octet-stream", Data: data,
	}
	require.NoError(t, client.SaveArtifact(t.Context(), request))
	saved := provider.callsTo("SaveArtifact")
	require.Len(t, saved, 1)
	require.Equal(t, request, saved[0].args[0])

	provider.set("GetArtifact", []any{data, "application/octet-stream"}, nil)
	got, contentType, err := client.GetArtifact(t.Context(), storagev1.ArtifactKey{Owner: request.Owner, Filename: "report.bin"})
	require.NoError(t, err)
	require.True(t, bytes.Equal(data, got), "the artifact comes back byte for byte")
	require.Equal(t, "application/octet-stream", contentType)

	receipt := storagev1.PromptResultReceipt{AttemptID: "a", Namespace: "n", TaskName: "t", OperationID: "op",
		OperationDigest: storagev1.CanonicalBytesDigest([]byte("op")), Data: data}
	require.NoError(t, client.SavePromptResultReceipt(t.Context(), receipt))
	require.Equal(t, receipt, provider.callsTo("SavePromptResultReceipt")[0].args[0])
	provider.set("GetPromptResultReceipt", []any{&receipt}, nil)
	gotReceipt, err := client.GetPromptResultReceipt(t.Context(), "a")
	require.NoError(t, err)
	require.Equal(t, &receipt, gotReceipt)

	provider.set("GetPromptResultReceipt", []any{(*storagev1.PromptResultReceipt)(nil)}, nil)
	gotReceipt, err = client.GetPromptResultReceipt(t.Context(), "missing")
	require.NoError(t, err)
	require.Nil(t, gotReceipt, "an absent receipt stays absent")

	empty := storagev1.PromptResultReceipt{AttemptID: "e"}
	provider.set("GetPromptResultReceipt", []any{&empty}, nil)
	gotReceipt, err = client.GetPromptResultReceipt(t.Context(), "e")
	require.NoError(t, err)
	require.Equal(t, &empty, gotReceipt, "a receipt with no bytes stays present")

	require.NoError(t, client.SaveResult(t.Context(), "n", "t", nil))
	require.Empty(t, provider.callsTo("SaveResult")[0].args[2])
	provider.set("GetResult", []any{[]byte(nil)}, nil)
	result, err := client.GetResult(t.Context(), "n", "t")
	require.NoError(t, err)
	require.Empty(t, result)
}

// chunkCounter counts the messages its side of a call sends, records the
// largest, and counts the messages it receives.
type chunkCounter struct {
	messages atomic.Int64
	largest  atomic.Int64
	inbound  atomic.Int64
}

func (*chunkCounter) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context { return ctx }
func (c *chunkCounter) HandleRPC(_ context.Context, event stats.RPCStats) {
	if _, ok := event.(*stats.InPayload); ok {
		c.inbound.Add(1)
	}
	if payload, ok := event.(*stats.OutPayload); ok {
		c.messages.Add(1)
		for size := int64(payload.Length); ; {
			largest := c.largest.Load()
			if size <= largest || c.largest.CompareAndSwap(largest, size) {
				break
			}
		}
	}
}
func (*chunkCounter) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context { return ctx }
func (*chunkCounter) HandleConn(context.Context, stats.ConnStats)                       {}

// A blob moves in messages of at most one chunk, so a large transfer never
// sends one large message.
func TestBlobsMoveInChunks(t *testing.T) {
	provider := newRecordingProvider()
	server, err := wire.NewServer(serveBoth(provider))
	require.NoError(t, err)
	counter := &chunkCounter{}
	listener := serveInsecureWith(t, server, grpc.StatsHandler(counter))
	client := wire.NewClient(dialInsecure(t, listener), boundConfig)

	data := pattern(5*wire.MaxChunkBytes + 1)
	provider.set("GetResult", []any{data}, nil)
	got, err := client.GetResult(t.Context(), "n", "t")
	require.NoError(t, err)
	require.True(t, bytes.Equal(data, got))
	require.Equal(t, int64(6), counter.messages.Load(), "five full chunks and one byte")
	require.LessOrEqual(t, counter.largest.Load(), int64(wire.MaxChunkBytes+64))
}

// uploadContext names the contract, revision and dataset, and declares a blob
// of size bytes with digest.
func uploadContext(ctx context.Context, size int, digest []byte) context.Context {
	return metadata.AppendToOutgoingContext(contractContext(ctx),
		wire.HeaderBlobSize, strconv.Itoa(size), wire.HeaderBlobSHA256, hex.EncodeToString(digest))
}

func digestOf(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}

// rawUpload sends messages on a raw SaveResult stream and returns the call's
// result.
func rawUpload(ctx context.Context, conn *grpc.ClientConn, messages ...*storagepb.SaveResultRequest) error {
	stream, err := storagepb.NewResultsClient(conn).SaveResult(ctx)
	if err != nil {
		return err
	}
	for _, message := range messages {
		if err := stream.Send(message); err != nil {
			break
		}
	}
	_, err = stream.CloseAndRecv()
	return err
}

// The server reassembles and checks an upload before the provider runs, so
// a broken, oversized or malformed upload never reaches it.
func TestUploadsAreCheckedBeforeTheProviderRuns(t *testing.T) {
	provider := newRecordingProvider()
	conn := wiretest.Dial(t, wire.ServerConfig{State: provider, Artifacts: provider, MaxBlobBytes: 4 * wire.MaxChunkBytes})
	data := pattern(2*wire.MaxChunkBytes + 7)
	first := &storagepb.SaveResultRequest{Namespace: "n", TaskName: "t", Data: data[:wire.MaxChunkBytes]}
	second := &storagepb.SaveResultRequest{Data: data[wire.MaxChunkBytes : 2*wire.MaxChunkBytes]}
	third := &storagepb.SaveResultRequest{Data: data[2*wire.MaxChunkBytes:]}
	withUnknown := &storagepb.SaveResultRequest{Data: data[2*wire.MaxChunkBytes:]}
	withUnknown.ProtoReflect().SetUnknown([]byte{0xf8, 0x07, 0x01})

	for _, refused := range []struct {
		name     string
		ctx      context.Context
		messages []*storagepb.SaveResultRequest
		reason   string
	}{
		{"no blob headers", contractContext(t.Context()), []*storagepb.SaveResultRequest{first, second, third}, wire.ReasonValidation},
		{"over the server's limit", uploadContext(t.Context(), 4*wire.MaxChunkBytes+1, digestOf(data)),
			[]*storagepb.SaveResultRequest{first}, wire.ReasonTooLarge},
		{"ends early", uploadContext(t.Context(), len(data), digestOf(data)),
			[]*storagepb.SaveResultRequest{first, second}, wire.ReasonUnavailable},
		{"wrong digest", uploadContext(t.Context(), len(data), digestOf(data[1:])),
			[]*storagepb.SaveResultRequest{first, second, third}, wire.ReasonUnavailable},
		{"more than declared", uploadContext(t.Context(), 2*wire.MaxChunkBytes, digestOf(data)),
			[]*storagepb.SaveResultRequest{first, second, third}, wire.ReasonValidation},
		{"a later message repeats a field", uploadContext(t.Context(), len(data), digestOf(data)),
			[]*storagepb.SaveResultRequest{first, {Namespace: "n", Data: second.Data}, third}, wire.ReasonValidation},
		{"an empty later message", uploadContext(t.Context(), len(data), digestOf(data)),
			[]*storagepb.SaveResultRequest{first, {}, second, third}, wire.ReasonValidation},
		{"a chunk over the limit", uploadContext(t.Context(), len(data), digestOf(data)),
			[]*storagepb.SaveResultRequest{{Namespace: "n", TaskName: "t", Data: data[:wire.MaxChunkBytes+1]},
				{Data: data[wire.MaxChunkBytes+1:]}}, wire.ReasonValidation},
		{"an unknown field in a later message", uploadContext(t.Context(), len(data), digestOf(data)),
			[]*storagepb.SaveResultRequest{first, second, withUnknown}, wire.ReasonUnsupportedField},
		{"no request", uploadContext(t.Context(), 0, digestOf(nil)), nil, wire.ReasonValidation},
	} {
		t.Run(refused.name, func(t *testing.T) {
			_, reason := reasonOf(t, rawUpload(refused.ctx, conn, refused.messages...))
			require.Equal(t, refused.reason, reason)
			require.Empty(t, provider.callsTo("SaveResult"), "a refused upload never reaches the provider")
		})
	}

	require.NoError(t, rawUpload(uploadContext(t.Context(), len(data), digestOf(data)), conn, first, second, third))
	calls := provider.callsTo("SaveResult")
	require.Len(t, calls, 1)
	require.True(t, bytes.Equal(data, calls[0].args[2].([]byte)))
}

// A nested blob belongs to a record the first message carries; a sender
// cannot create the record with a later message.
func TestNestedBlobsNeedTheirRecord(t *testing.T) {
	provider := newRecordingProvider()
	conn := wiretest.Dial(t, serveBoth(provider))
	data := []byte("receipt bytes")
	stream, err := storagepb.NewResultsClient(conn).SavePromptResultReceipt(uploadContext(t.Context(), len(data), digestOf(data)))
	require.NoError(t, err)
	require.NoError(t, stream.Send(&storagepb.SavePromptResultReceiptRequest{}))
	require.NoError(t, stream.Send(&storagepb.SavePromptResultReceiptRequest{Receipt: &storagepb.PromptResultReceipt{Data: data}}))
	_, err = stream.CloseAndRecv()
	_, reason := reasonOf(t, err)
	require.Equal(t, wire.ReasonValidation, reason)
	require.Empty(t, provider.callsTo("SavePromptResultReceipt"))
}

// The client refuses a blob over its limits before sending anything.
func TestClientRefusesOversizedBlobsWithoutSending(t *testing.T) {
	provider := newRecordingProvider()
	client := wiretest.Serve(t, serveBoth(provider))

	err := client.SaveArtifact(t.Context(), storagev1.SaveArtifactRequest{
		Owner: storagev1.ArtifactOwner{Namespace: "n", TaskName: "t"}, Filename: "big", Data: make([]byte, storagev1.MaxArtifactBytes+1),
	})
	var tooLarge storagev1.TooLargeError
	require.ErrorAs(t, err, &tooLarge)
	require.Equal(t, int64(storagev1.MaxArtifactBytes), tooLarge.Limit, "the provider's described artifact limit applies")
	require.Empty(t, provider.callsTo("SaveArtifact"))

	small := wire.NewClient(wiretest.Dial(t, serveBoth(provider)), wire.ClientConfig{Dataset: recordingDataset, MaxBlobBytes: 1024})
	err = small.SaveResult(t.Context(), "n", "t", make([]byte, 1025))
	require.ErrorAs(t, err, &tooLarge)
	require.Equal(t, int64(1024), tooLarge.Limit)
	require.Empty(t, provider.callsTo("SaveResult"))

	provider.set("GetResult", []any{make([]byte, 2048)}, nil)
	_, err = small.GetResult(t.Context(), "n", "t")
	require.ErrorAs(t, err, &tooLarge, "a download over the client's limit is refused before it is read")
	require.False(t, errors.Is(err, storagev1.ErrOutcomeUnknown))
}

// An artifact over the provider's described limit is refused by the server
// before its bytes are read, even from a client that does not check.
func TestServerRefusesArtifactsOverTheProviderLimit(t *testing.T) {
	provider := newRecordingProvider()
	conn := wiretest.Dial(t, serveBoth(provider))
	data := make([]byte, storagev1.MaxArtifactBytes+1)
	stream, err := storagepb.NewArtifactsClient(conn).SaveArtifact(uploadContext(t.Context(), len(data), digestOf(data)))
	require.NoError(t, err)
	_ = stream.Send(&storagepb.SaveArtifactRequest{Filename: "big"})
	_, err = stream.CloseAndRecv()
	code, reason := reasonOf(t, err)
	require.Equal(t, codes.ResourceExhausted, code)
	require.Equal(t, wire.ReasonTooLarge, reason)
	require.Empty(t, provider.callsTo("SaveArtifact"))
}

// Streaming calls use the bulk connection and unary calls the main one.
func TestBulkCallsUseTheBulkConnection(t *testing.T) {
	control, bulk := newRecordingProvider(), newRecordingProvider()
	client := wire.NewClient(wiretest.Dial(t, serveBoth(control)), wire.ClientConfig{
		Dataset: recordingDataset, Bulk: wiretest.Dial(t, serveBoth(bulk)),
	})
	require.NoError(t, client.SaveResult(t.Context(), "n", "t", []byte("r")))
	_, _, err := client.GetArtifact(t.Context(), storagev1.ArtifactKey{Filename: "f"})
	require.NoError(t, err)
	require.NoError(t, client.SavePlan(t.Context(), "n", "t", &storagev1.PlanState{}))
	require.Len(t, bulk.callsTo("SaveResult"), 1)
	require.Len(t, bulk.callsTo("GetArtifact"), 1)
	require.Empty(t, bulk.callsTo("SavePlan"))
	require.Len(t, control.callsTo("SavePlan"), 1)
	require.Empty(t, control.callsTo("SaveResult"))
}

// A provider failure on a streaming write may follow a commit; on a
// streaming read it is a plain error.
func TestStreamingProviderFailures(t *testing.T) {
	provider := newRecordingProvider()
	client := wiretest.Serve(t, serveBoth(provider))

	provider.set("SaveArtifact", nil, errors.New("disk said \"secret-value\""))
	err := client.SaveArtifact(t.Context(), storagev1.SaveArtifactRequest{Filename: "f", Data: []byte("x")})
	require.ErrorIs(t, err, storagev1.ErrOutcomeUnknown)
	require.NotContains(t, err.Error(), "secret-value")

	provider.set("SaveArtifact", nil, storagev1.TooLargeError{What: "artifact \"f\"", Size: 11, Limit: 10})
	err = client.SaveArtifact(t.Context(), storagev1.SaveArtifactRequest{Filename: "f", Data: []byte("x")})
	var tooLarge storagev1.TooLargeError
	require.ErrorAs(t, err, &tooLarge)
	require.Equal(t, int64(10), tooLarge.Limit)

	provider.set("GetArtifact", nil, storagev1.ErrNotFound)
	_, _, err = client.GetArtifact(t.Context(), storagev1.ArtifactKey{Filename: "f"})
	require.ErrorIs(t, err, storagev1.ErrNotFound)

	provider.set("GetArtifact", []any{[]byte("x"), "text/plain \xff"}, nil)
	_, _, err = client.GetArtifact(t.Context(), storagev1.ArtifactKey{Filename: "f"})
	require.Error(t, err, "a response protobuf cannot encode is a provider error")
	require.False(t, errors.Is(err, storagev1.ErrOutcomeUnknown), "a read is never an unknown outcome")

	provider.mu.Lock()
	provider.panics["GetResult"] = "provider bug"
	provider.mu.Unlock()
	_, err = client.GetResult(t.Context(), "n", "t")
	require.Error(t, err)
	require.NoError(t, client.SaveResult(t.Context(), "n", "t", []byte("r")), "the server keeps serving after a panic")
}

// A cancelled upload never reaches the provider.
func TestCancelledUploadsNeverReachTheProvider(t *testing.T) {
	provider := newRecordingProvider()
	conn := wiretest.Dial(t, serveBoth(provider))
	data := pattern(2 * wire.MaxChunkBytes)
	ctx, cancel := context.WithCancel(uploadContext(t.Context(), len(data), digestOf(data)))
	stream, err := storagepb.NewResultsClient(conn).SaveResult(ctx)
	require.NoError(t, err)
	require.NoError(t, stream.Send(&storagepb.SaveResultRequest{Namespace: "n", TaskName: "t", Data: data[:wire.MaxChunkBytes]}))
	cancel()
	_, err = stream.CloseAndRecv()
	require.Error(t, err)
	time.Sleep(50 * time.Millisecond)
	require.Empty(t, provider.callsTo("SaveResult"))
}
