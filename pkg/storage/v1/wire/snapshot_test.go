package wire_test

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/orka-agents/orka/pkg/storage/v1/wire"
	"github.com/orka-agents/orka/pkg/storage/v1/wire/storagepb"
	"github.com/orka-agents/orka/pkg/storage/v1/wire/wiretest"
)

// transcript returns count messages whose content is size bytes each and
// differs per message, so a dropped or reordered batch changes the result.
func transcript(count, size int) []storagev1.SessionMessage {
	messages := make([]storagev1.SessionMessage, count)
	for index := range messages {
		content := strings.Repeat(string(rune('a'+index%26)), size)
		messages[index] = storagev1.SessionMessage{
			ID: fmt.Sprintf("m-%d", index), Order: int64(index + 1), Role: "user", Content: content,
			Metadata:  map[string]string{"index": strconv.Itoa(index)},
			Timestamp: time.Date(2026, 10, 29, 1, 2, 3, index, time.UTC),
		}
	}
	return messages
}

// Batched reads return lists larger than one message whole and in order,
// with the fields around the list, an absent record and an empty list
// preserved.
func TestBatchedReadsCrossTheWireWhole(t *testing.T) {
	provider := newRecordingProvider()
	client := wiretest.Serve(t, serveBoth(provider))
	messages := transcript(48, 64<<10)

	session := &storagev1.SessionRecord{
		Namespace: "n", Name: "s", SessionType: "chat", ActiveTask: "t", ActiveTaskUID: "uid", MessageCount: len(messages),
		InputTokens: 3, OutputTokens: 4, CreatedAt: time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 10, 29, 0, 0, 1, 0, time.UTC), Messages: messages,
	}
	provider.set("GetSession", []any{session}, nil)
	got, err := client.GetSession(t.Context(), "n", "s")
	require.NoError(t, err)
	require.Equal(t, wire.NormalizeValue(session), wire.NormalizeValue(got), "the Session and its whole transcript come back")

	provider.set("LoadTranscript", []any{messages}, nil)
	loaded, err := client.LoadTranscript(t.Context(), "n", "s", 0)
	require.NoError(t, err)
	require.Equal(t, wire.NormalizeValue(messages), wire.NormalizeValue(loaded))

	provider.set("LoadTranscriptThrough", []any{messages[:3]}, nil)
	through, err := client.LoadTranscriptThrough(t.Context(), "n", "s", "m-2", 0)
	require.NoError(t, err)
	require.Equal(t, wire.NormalizeValue(messages[:3]), wire.NormalizeValue(through))

	listed := make([]storagev1.SessionMetadata, 20000)
	for index := range listed {
		listed[index] = storagev1.SessionMetadata{Name: fmt.Sprintf("session-%05d", index), SessionType: "task", MessageCount: index}
	}
	provider.set("ListSessions", []any{listed}, nil)
	sessions, err := client.ListSessions(t.Context(), "n")
	require.NoError(t, err)
	require.Equal(t, listed, sessions)

	intents := []storagev1.SessionCleanupIntent{{Namespace: "n", SessionName: "a"}, {Namespace: "n", SessionName: "b"}}
	provider.set("ListSessionCleanupIntents", []any{intents}, nil)
	gotIntents, err := client.ListSessionCleanupIntents(t.Context())
	require.NoError(t, err)
	require.Equal(t, wire.NormalizeValue(intents), wire.NormalizeValue(gotIntents))

	turns := []storagev1.SessionTurn{{ID: "turn-1", UserPrompt: strings.Repeat("p", 2*wire.MaxChunkBytes)}, {ID: "turn-2"}}
	provider.set("ListSessionCleanupTurns", []any{turns}, nil)
	gotTurns, err := client.ListSessionCleanupTurns(t.Context(), intents[0])
	require.NoError(t, err)
	require.Equal(t, wire.NormalizeValue(turns), wire.NormalizeValue(gotTurns), "a record larger than one batch crosses alone")

	provider.set("GetSession", []any{(*storagev1.SessionRecord)(nil)}, nil)
	got, err = client.GetSession(t.Context(), "n", "missing")
	require.NoError(t, err)
	require.Nil(t, got, "an absent Session stays absent")

	provider.set("GetSession", []any{&storagev1.SessionRecord{Namespace: "n", Name: "empty"}}, nil)
	got, err = client.GetSession(t.Context(), "n", "empty")
	require.NoError(t, err)
	require.Equal(t, "empty", got.Name)
	require.Empty(t, got.Messages)

	provider.set("ListSessions", []any{[]storagev1.SessionMetadata{}}, nil)
	sessions, err = client.ListSessions(t.Context(), "n")
	require.NoError(t, err)
	require.Empty(t, sessions)

	provider.set("GetSession", nil, fmt.Errorf("lookup: %w", storagev1.ErrNotFound))
	_, err = client.GetSession(t.Context(), "n", "gone")
	require.ErrorIs(t, err, storagev1.ErrNotFound, "a refused batched read carries its contract error")
}

// A batched read moves in messages of about one batch, so a large result
// never travels as one large message.
func TestBatchedReadsMoveInBatches(t *testing.T) {
	provider := newRecordingProvider()
	server, err := wire.NewServer(serveBoth(provider))
	require.NoError(t, err)
	counter := &chunkCounter{}
	client := wire.NewClient(dialInsecure(t, serveInsecureWith(t, server, grpc.StatsHandler(counter))), boundConfig)

	messages := transcript(80, 64<<10)
	provider.set("LoadTranscript", []any{messages}, nil)
	loaded, err := client.LoadTranscript(t.Context(), "n", "s", 0)
	require.NoError(t, err)
	require.Len(t, loaded, len(messages))
	require.GreaterOrEqual(t, counter.messages.Load(), int64(5), "five MiB of records cross in at least five messages")
	require.LessOrEqual(t, counter.largest.Load(), int64(wire.MaxChunkBytes+1024))
}

// The client refuses a list larger than it accepts from the declared size,
// and the server refuses a record it cannot send in one message without
// sending any part of the list.
func TestBatchedReadsAreBounded(t *testing.T) {
	provider := newRecordingProvider()
	small := wire.NewClient(wiretest.Dial(t, serveBoth(provider)), wire.ClientConfig{Dataset: recordingDataset, MaxSnapshotBytes: 1024})
	provider.set("LoadTranscript", []any{transcript(4, 1024)}, nil)
	_, err := small.LoadTranscript(t.Context(), "n", "s", 0)
	var tooLarge storagev1.TooLargeError
	require.ErrorAs(t, err, &tooLarge)
	require.Equal(t, int64(1024), tooLarge.Limit)
	require.Greater(t, tooLarge.Size, int64(4*1024))
	require.False(t, errors.Is(err, storagev1.ErrOutcomeUnknown))

	client := wiretest.Serve(t, serveBoth(provider))
	provider.set("LoadTranscript", []any{append(transcript(2, 16), transcript(1, wire.MaxMessageBytes)...)}, nil)
	_, err = client.LoadTranscript(t.Context(), "n", "s", 0)
	require.Error(t, err, "a record over the message limit cannot be sent")
	require.False(t, errors.Is(err, storagev1.ErrOutcomeUnknown), "a read is never an unknown outcome")
	require.False(t, errors.Is(err, wire.ErrIncompatible), "the server refused before sending, not mid-stream")
}

// craftedSessions answers GetSession with fixed headers and messages, as a
// broken or hostile server might.
type craftedSessions struct {
	storagepb.UnimplementedSessionsServer
	header   metadata.MD
	messages []*storagepb.GetSessionResponse
}

func (c *craftedSessions) GetSession(_ *storagepb.GetSessionRequest, stream grpc.ServerStreamingServer[storagepb.GetSessionResponse]) error {
	if c.header != nil {
		if err := stream.SetHeader(c.header); err != nil {
			return err
		}
	}
	for _, message := range c.messages {
		if err := stream.Send(message); err != nil {
			return err
		}
	}
	return nil
}

func serveCrafted(t *testing.T, sessions *craftedSessions) *wire.Client {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := grpc.NewServer()
	storagepb.RegisterSessionsServer(server, sessions)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return wire.NewClient(dialInsecure(t, listener.Addr().String()), boundConfig)
}

func declare(records int, size int) metadata.MD {
	return metadata.Pairs(wire.HeaderSnapshotRecords, strconv.Itoa(records), wire.HeaderSnapshotBytes, strconv.Itoa(size))
}

// framed returns the encoded size of messages as elements of
// SessionRecord.messages: each one's tag, length prefix and bytes.
func framed(messages ...*storagepb.SessionMessage) int {
	number := (&storagepb.SessionRecord{}).ProtoReflect().Descriptor().Fields().ByName("messages").Number()
	size := 0
	for _, message := range messages {
		size += protowire.SizeTag(number) + protowire.SizeBytes(proto.Size(message))
	}
	return size
}

// The client reassembles only a list that follows the framing rules and
// matches its declared count and size.
func TestClientRefusesMalformedBatchedReads(t *testing.T) {
	first := &storagepb.SessionMessage{Id: "m-1", Content: "first"}
	second := &storagepb.SessionMessage{Id: "m-2", Content: "second"}
	size := framed(first, second)
	session := func(messages ...*storagepb.SessionMessage) *storagepb.GetSessionResponse {
		return &storagepb.GetSessionResponse{Session: &storagepb.SessionRecord{Name: "s", Messages: messages}}
	}
	later := func(messages ...*storagepb.SessionMessage) *storagepb.GetSessionResponse {
		return &storagepb.GetSessionResponse{Session: &storagepb.SessionRecord{Messages: messages}}
	}

	good := serveCrafted(t, &craftedSessions{header: declare(2, size), messages: []*storagepb.GetSessionResponse{
		session(first), later(second),
	}})
	got, err := good.GetSession(t.Context(), "n", "s")
	require.NoError(t, err, "a well-framed response reassembles")
	require.Equal(t, []string{"m-1", "m-2"}, []string{got.Messages[0].ID, got.Messages[1].ID})

	for _, broken := range []struct {
		name     string
		sessions *craftedSessions
	}{
		{"no declaration", &craftedSessions{messages: []*storagepb.GetSessionResponse{session(first)}}},
		{"fewer records than declared", &craftedSessions{header: declare(2, size),
			messages: []*storagepb.GetSessionResponse{session(first)}}},
		{"more records than declared", &craftedSessions{header: declare(1, framed(first)),
			messages: []*storagepb.GetSessionResponse{session(first), later(second)}}},
		{"more bytes than declared", &craftedSessions{header: declare(2, size-1),
			messages: []*storagepb.GetSessionResponse{session(first), later(second)}}},
		{"a later message carries another field", &craftedSessions{header: declare(2, size),
			messages: []*storagepb.GetSessionResponse{session(first), {Session: &storagepb.SessionRecord{Name: "x",
				Messages: []*storagepb.SessionMessage{second}}}}}},
		{"an empty later message", &craftedSessions{header: declare(2, size),
			messages: []*storagepb.GetSessionResponse{session(first), {}, later(second)}}},
		{"records for a record the first message leaves out", &craftedSessions{header: declare(1, framed(second)),
			messages: []*storagepb.GetSessionResponse{{}, later(second)}}},
		{"empty records declared as no bytes", &craftedSessions{header: declare(3, 0),
			messages: []*storagepb.GetSessionResponse{session(), later(&storagepb.SessionMessage{}, &storagepb.SessionMessage{}, &storagepb.SessionMessage{})}}},
	} {
		t.Run(broken.name, func(t *testing.T) {
			_, err := serveCrafted(t, broken.sessions).GetSession(t.Context(), "n", "s")
			require.Error(t, err)
			require.ErrorIs(t, err, wire.ErrIncompatible)
			require.False(t, errors.Is(err, storagev1.ErrOutcomeUnknown))
		})
	}
}

// A list declared with more records than the client accepts is refused
// before any record is read, so many empty records cannot hold its memory.
func TestClientRefusesTooManyBatchedRecords(t *testing.T) {
	huge := serveCrafted(t, &craftedSessions{header: declare(1<<40, 0),
		messages: []*storagepb.GetSessionResponse{{Session: &storagepb.SessionRecord{Name: "s"}}}})
	_, err := huge.GetSession(t.Context(), "n", "s")
	var tooLarge storagev1.TooLargeError
	require.ErrorAs(t, err, &tooLarge, "a declared record count over the limit is refused, whatever the declared bytes")
	require.Equal(t, int64(wire.DefaultMaxSnapshotRecords), tooLarge.Limit)

	provider := newRecordingProvider()
	limited := wire.NewClient(wiretest.Dial(t, serveBoth(provider)), wire.ClientConfig{Dataset: recordingDataset, MaxSnapshotRecords: 3})
	provider.set("LoadTranscript", []any{transcript(4, 1)}, nil)
	_, err = limited.LoadTranscript(t.Context(), "n", "s", 0)
	require.ErrorAs(t, err, &tooLarge)
	require.Equal(t, int64(3), tooLarge.Limit)
	require.Equal(t, int64(4), tooLarge.Size)
	provider.set("LoadTranscript", []any{transcript(3, 1)}, nil)
	loaded, err := limited.LoadTranscript(t.Context(), "n", "s", 0)
	require.NoError(t, err)
	require.Len(t, loaded, 3)

	empty := serveCrafted(t, &craftedSessions{header: declare(2, framed(&storagepb.SessionMessage{}, &storagepb.SessionMessage{})),
		messages: []*storagepb.GetSessionResponse{{Session: &storagepb.SessionRecord{Name: "s"}},
			{Session: &storagepb.SessionRecord{Messages: []*storagepb.SessionMessage{{}, {}}}}}})
	got, err := empty.GetSession(t.Context(), "n", "s")
	require.NoError(t, err, "empty records count their framing")
	require.Len(t, got.Messages, 2)
}

// Batched reads use the bulk connection.
func TestBatchedReadsUseTheBulkConnection(t *testing.T) {
	control, bulk := newRecordingProvider(), newRecordingProvider()
	client := wire.NewClient(wiretest.Dial(t, serveBoth(control)), wire.ClientConfig{
		Dataset: recordingDataset, Bulk: wiretest.Dial(t, serveBoth(bulk)),
	})
	_, err := client.GetSession(t.Context(), "n", "s")
	require.NoError(t, err)
	_, err = client.GetSessionType(t.Context(), "n", "s")
	require.NoError(t, err)
	require.Len(t, bulk.callsTo("GetSession"), 1)
	require.Empty(t, control.callsTo("GetSession"))
	require.Len(t, control.callsTo("GetSessionType"), 1)
	require.Empty(t, bulk.callsTo("GetSessionType"))
}

// stateOnly hides every method of a provider beyond state.v1, such as the
// optional Ownership interfaces.
type stateOnly struct {
	storagev1.StateProvider
}

// Ownership is served only from a provider that implements it, and
// otherwise answered as unimplemented without reaching any provider.
func TestOwnershipIsServedOnlyByProvidersThatImplementIt(t *testing.T) {
	provider := newRecordingProvider()
	request := storagev1.ResolveOperationRequest{DatasetID: recordingDataset, OperationID: "op", Method: "SavePlan",
		RequestDigest: storagev1.CanonicalBytesDigest([]byte("r")), IssuedAt: time.Now()}

	plain := wiretest.Serve(t, wire.ServerConfig{State: stateOnly{provider}})
	_, err := plain.ResolveOperation(t.Context(), request)
	require.ErrorIs(t, err, wire.ErrIncompatible)
	require.ErrorIs(t, err, storagev1.ErrValidation)
	require.False(t, errors.Is(err, storagev1.ErrOutcomeUnknown), "an unimplemented RPC committed nothing")
	_, err = plain.GetControllerEpochMirror(t.Context(), storagev1.DefaultControllerEpochName)
	require.ErrorIs(t, err, wire.ErrIncompatible)
	require.Empty(t, provider.callsTo("ResolveOperation"))
	require.Empty(t, provider.callsTo("GetControllerEpochMirror"))

	provider.set("ResolveOperation", []any{storagev1.OperationOutcome{OperationID: "op", State: storagev1.OperationAborted}}, nil)
	artifactsOnly := wiretest.Serve(t, wire.ServerConfig{Artifacts: provider})
	outcome, err := artifactsOnly.ResolveOperation(t.Context(), request)
	require.NoError(t, err, "an artifacts provider that implements Ownership serves it")
	require.Equal(t, storagev1.OperationAborted, outcome.State)
	require.Len(t, provider.callsTo("ResolveOperation"), 1)
}

// Only contract writes carry an envelope: the administrative methods that
// settle operations and advance the fence never do, and the server refuses
// one sent to them.
func TestAdministrativeCallsCarryNoEnvelope(t *testing.T) {
	provider := newRecordingProvider()
	client := wiretest.Serve(t, serveBoth(provider))
	envelope := storagev1.WriteEnvelope{
		DatasetID: recordingDataset, Fence: storagev1.ControllerEpochFence{Name: "orka", Epoch: 2, HolderID: "h"},
		OperationID: "op", RequestDigest: storagev1.CanonicalBytesDigest([]byte("r")), IssuedAt: time.Now(), Sequence: 1,
	}
	ctx := storagev1.WithWriteEnvelope(t.Context(), envelope)

	_, err := client.ResolveOperation(ctx, storagev1.ResolveOperationRequest{OperationID: "op"})
	require.NoError(t, err)
	_, err = client.PruneOperationReceipts(ctx, 10)
	require.NoError(t, err)
	_, err = client.AdvanceControllerEpoch(ctx, storagev1.AdvanceControllerEpochRequest{DatasetID: recordingDataset})
	require.NoError(t, err)
	_, err = client.IsLocked(ctx, "n", "s", "t", "uid")
	require.NoError(t, err)
	for _, method := range []string{"ResolveOperation", "PruneOperationReceipts", "AdvanceControllerEpoch"} {
		calls := provider.callsTo(method)
		require.Len(t, calls, 1)
		require.False(t, calls[0].hasEnvelope, "%s carries no envelope", method)
	}
	require.True(t, provider.callsTo("IsLocked")[0].hasEnvelope, "IsLocked is a contract write and carries one")

	encoded, err := proto.Marshal(&storagepb.WriteEnvelope{DatasetId: recordingDataset, OperationId: "op"})
	require.NoError(t, err)
	raw := storagepb.NewOwnershipClient(wiretest.Dial(t, serveBoth(provider)))
	_, err = raw.ResolveOperation(metadata.AppendToOutgoingContext(contractContext(t.Context()), wire.HeaderEnvelope,
		string(encoded)), &storagepb.ResolveOperationRequest{OperationId: "op"})
	_, reason := reasonOf(t, err)
	require.Equal(t, wire.ReasonValidation, reason)
	require.Len(t, provider.callsTo("ResolveOperation"), 1, "the refused call never reached the provider")

	unbound := metadata.AppendToOutgoingContext(t.Context(), wire.HeaderContract, storagev1.ContractVersion, wire.HeaderRevision, "1")
	_, err = raw.PruneOperationReceipts(unbound, &storagepb.PruneOperationReceiptsRequest{Limit: 1})
	_, reason = reasonOf(t, err)
	require.Equal(t, wire.ReasonDatasetMismatch, reason, "Ownership calls name their dataset like every call but Describe and HealthCheck")
	require.Len(t, provider.callsTo("PruneOperationReceipts"), 1)
}

// callRecorder records the class of every call a server observes.
type callRecorder struct {
	classes chan wire.CallInfo
}

func (r callRecorder) ObserveCall(call wire.CallInfo) { r.classes <- call }

// Ownership calls are control calls, and batched reads bulk calls, for the
// server's per-class limits.
func TestNewServicesUseTheirCallClasses(t *testing.T) {
	provider := newRecordingProvider()
	recorder := callRecorder{classes: make(chan wire.CallInfo, 64)}
	config := serveBoth(provider)
	config.Observer = recorder
	client := wiretest.Serve(t, config)
	_, err := client.ResolveOperation(t.Context(), storagev1.ResolveOperationRequest{OperationID: "op"})
	require.NoError(t, err)
	_, err = client.GetSession(t.Context(), "n", "s")
	require.NoError(t, err)
	_, err = client.ListExecutionEvents(t.Context(), storagev1.ExecutionEventFilter{Namespace: "n"})
	require.NoError(t, err)

	classes := map[string]wire.Class{}
	deadline := time.After(10 * time.Second)
	for len(classes) < 3 {
		select {
		case call := <-recorder.classes:
			if call.Method != "Describe" {
				classes[call.Method] = call.Class
			}
		case <-deadline:
			t.Fatalf("observed only %v", classes)
		}
	}
	require.Equal(t, map[string]wire.Class{
		"ResolveOperation": wire.ClassControl, "GetSession": wire.ClassBulk, "ListExecutionEvents": wire.ClassControl,
	}, classes)
}
