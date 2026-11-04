package wire_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/orka-agents/orka/pkg/storage/v1/wire"
	"github.com/orka-agents/orka/pkg/storage/v1/wire/storagepb"
	"github.com/orka-agents/orka/pkg/storage/v1/wire/wiretest"
)

// Every method with a wire RPC carries every argument to the provider and
// every result back, through the generated client and server over real TLS.
func TestEveryWireMethodRoundTripsArgumentsAndResults(t *testing.T) {
	provider := newRecordingProvider()
	client := wiretest.Serve(t, serveBoth(provider))
	clientValue := reflect.ValueOf(client)
	require.NotEmpty(t, wireMethods)
	for _, name := range wireMethods {
		t.Run(name, func(t *testing.T) {
			method := clientValue.MethodByName(name)
			require.True(t, method.IsValid(), "the client implements %s", name)
			methodType := method.Type()
			args := []reflect.Value{reflect.ValueOf(t.Context())}
			for index := 1; index < methodType.NumIn(); index++ {
				args = append(args, wire.FillValue(methodType.In(index), fmt.Sprintf("%s.arg%d", name, index)))
			}
			var results []any
			for index := 0; index < methodType.NumOut()-1; index++ {
				results = append(results, wire.FillValue(methodType.Out(index), fmt.Sprintf("%s.result%d", name, index)).Interface())
			}
			if len(results) > 0 {
				if described, isInfo := results[0].(storagev1.ProviderInfo); isInfo {
					// A Describe notes the serving dataset, which every later
					// call must name, and the server describes only the
					// contract parts it serves and the artifact limit it
					// enforces.
					described.DatasetID = recordingDataset
					described.Contracts = []string{storagev1.StateContract, storagev1.ArtifactsContract}
					described.Limits.MaxArtifactBytes = storagev1.MaxArtifactBytes
					results[0] = described
				}
			}
			provider.set(name, results, nil)

			out := method.Call(args)
			require.Nil(t, out[len(out)-1].Interface(), "%s succeeds", name)
			calls := provider.callsTo(name)
			require.NotEmpty(t, calls)
			received := calls[len(calls)-1].args
			require.Len(t, received, len(args)-1)
			for index, arg := range args[1:] {
				require.Equal(t, wire.NormalizeValue(arg.Interface()), wire.NormalizeValue(received[index]), "argument %d", index+1)
			}
			for index, want := range results {
				require.Equal(t, wire.NormalizeValue(want), wire.NormalizeValue(out[index].Interface()), "result %d", index)
			}
		})
	}
}

// contractContext names the contract, revision and the recording provider's
// dataset, as a bound client does, for a raw stub call.
func contractContext(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx, wire.HeaderContract, storagev1.ContractVersion, wire.HeaderRevision, "1",
		wire.HeaderDataset, recordingDataset)
}

func reasonOf(t *testing.T, err error) (codes.Code, string) {
	t.Helper()
	st, ok := status.FromError(err)
	require.True(t, ok, "a status error: %v", err)
	for _, detail := range st.Details() {
		if info, ok := detail.(*errdetails.ErrorInfo); ok && info.GetDomain() == wire.ErrorDomain {
			return st.Code(), info.GetReason()
		}
	}
	t.Fatalf("status has no storage ErrorInfo: %v", err)
	return 0, ""
}

func TestServerRefusesCallsWithoutTheContractHeaders(t *testing.T) {
	provider := newRecordingProvider()
	plans := storagepb.NewResultsClient(wiretest.Dial(t, serveBoth(provider)))

	var refusedHeader metadata.MD
	_, err := plans.GetPlan(t.Context(), &storagepb.GetPlanRequest{Namespace: "n", TaskName: "t"}, grpc.Header(&refusedHeader))
	code, reason := reasonOf(t, err)
	require.Equal(t, codes.FailedPrecondition, code)
	require.Equal(t, wire.ReasonContractMismatch, reason)
	require.Equal(t, []string{storagev1.ContractVersion}, refusedHeader.Get(wire.HeaderContract),
		"a refused call still names the server's contract")
	require.Equal(t, []string{"1"}, refusedHeader.Get(wire.HeaderRevision))

	other := metadata.AppendToOutgoingContext(t.Context(), wire.HeaderContract, "orka.storage.v2", wire.HeaderRevision, "1")
	_, err = plans.GetPlan(other, &storagepb.GetPlanRequest{})
	_, reason = reasonOf(t, err)
	require.Equal(t, wire.ReasonContractMismatch, reason)

	badRevision := metadata.AppendToOutgoingContext(t.Context(), wire.HeaderContract, storagev1.ContractVersion, wire.HeaderRevision, "x")
	_, err = plans.GetPlan(badRevision, &storagepb.GetPlanRequest{})
	_, reason = reasonOf(t, err)
	require.Equal(t, wire.ReasonContractMismatch, reason)
	require.Empty(t, provider.callsTo("GetPlan"), "a refused call never reaches the provider")

	_, err = plans.GetPlan(contractContext(t.Context()), &storagepb.GetPlanRequest{})
	require.NoError(t, err, "an older or equal revision is served")
}

// An older server never ignores a field it does not know, so a newer
// precondition cannot be dropped silently.
func TestServerRefusesUnknownRequestFields(t *testing.T) {
	provider := newRecordingProvider()
	plans := storagepb.NewResultsClient(wiretest.Dial(t, serveBoth(provider)))
	unknown := []byte{0xf8, 0x07, 0x01} // field 127, varint 1

	request := &storagepb.DeletePlanRequest{Namespace: "n", TaskName: "t"}
	request.ProtoReflect().SetUnknown(unknown)
	_, err := plans.DeletePlan(contractContext(t.Context()), request)
	code, reason := reasonOf(t, err)
	require.Equal(t, codes.InvalidArgument, code)
	require.Equal(t, wire.ReasonUnsupportedField, reason)

	nested := &storagepb.SavePlanRequest{Namespace: "n", TaskName: "t", Plan: &storagepb.PlanState{Summary: "s"}}
	nested.Plan.ProtoReflect().SetUnknown(unknown)
	_, err = plans.SavePlan(contractContext(t.Context()), nested)
	_, reason = reasonOf(t, err)
	require.Equal(t, wire.ReasonUnsupportedField, reason)
	require.Empty(t, provider.callsTo("DeletePlan"))
	require.Empty(t, provider.callsTo("SavePlan"))
}

func TestWriteEnvelopeReachesTheProviderOnWrites(t *testing.T) {
	provider := newRecordingProvider()
	client := wiretest.Serve(t, serveBoth(provider))
	envelope := storagev1.WriteEnvelope{
		DatasetID:     "dataset-1",
		Fence:         storagev1.ControllerEpochFence{Name: "orka", Epoch: 7, HolderID: "holder"},
		OperationID:   "operation-1",
		RequestDigest: storagev1.CanonicalBytesDigest([]byte("request")),
		IssuedAt:      time.Date(2026, 10, 29, 1, 2, 3, 456789000, time.UTC),
		Sequence:      42,
	}
	ctx := storagev1.WithWriteEnvelope(t.Context(), envelope)

	require.NoError(t, client.SavePlan(ctx, "n", "t", &storagev1.PlanState{Summary: "s"}))
	calls := provider.callsTo("SavePlan")
	require.Len(t, calls, 1)
	require.True(t, calls[0].hasEnvelope)
	require.Equal(t, envelope, calls[0].envelope)

	_, err := client.GetPlan(ctx, "n", "t")
	require.NoError(t, err)
	require.False(t, provider.callsTo("GetPlan")[0].hasEnvelope, "a read carries no envelope")

	require.NoError(t, client.DeletePlan(t.Context(), "n", "t"))
	require.False(t, provider.callsTo("DeletePlan")[0].hasEnvelope, "a write without an envelope is accepted")
}

// Only contract writes carry an envelope, so a call to a read that names one
// is refused before the provider runs, and never sees an envelope the
// contract does not give it.
func TestServerRefusesAnEnvelopeOnAMethodWithoutOne(t *testing.T) {
	provider := newRecordingProvider()
	plans := storagepb.NewResultsClient(wiretest.Dial(t, serveBoth(provider)))
	encoded, err := proto.Marshal(&storagepb.WriteEnvelope{DatasetId: recordingDataset, OperationId: "operation-1"})
	require.NoError(t, err)
	ctx := metadata.AppendToOutgoingContext(contractContext(t.Context()), wire.HeaderEnvelope, string(encoded))

	_, err = plans.GetPlan(ctx, &storagepb.GetPlanRequest{Namespace: "n", TaskName: "t"})
	code, reason := reasonOf(t, err)
	require.Equal(t, codes.InvalidArgument, code)
	require.Equal(t, wire.ReasonValidation, reason)
	require.Empty(t, provider.callsTo("GetPlan"), "a refused call never reaches the provider")

	_, err = plans.DeletePlan(ctx, &storagepb.DeletePlanRequest{Namespace: "n", TaskName: "t"})
	require.NoError(t, err, "a write takes the same envelope")
	calls := provider.callsTo("DeletePlan")
	require.Len(t, calls, 1)
	require.True(t, calls[0].hasEnvelope)
}

func TestProviderFailuresCrossTheWire(t *testing.T) {
	provider := newRecordingProvider()
	client := wiretest.Serve(t, serveBoth(provider))

	provider.set("GetPlan", nil, fmt.Errorf("lookup: %w", storagev1.ErrNotFound))
	_, err := client.GetPlan(t.Context(), "n", "t")
	require.ErrorIs(t, err, storagev1.ErrNotFound)

	provider.set("SavePlan", nil, storagev1.FencedErrorf("epoch 3 is stale"))
	err = client.SavePlan(t.Context(), "n", "t", &storagev1.PlanState{})
	var fenced storagev1.FencedError
	require.ErrorAs(t, err, &fenced)
	require.Equal(t, "epoch 3 is stale", fenced.Message)

	provider.set("SavePlan", nil, errors.New("database said: duplicate key value \"secret-value\""))
	err = client.SavePlan(t.Context(), "n", "t", &storagev1.PlanState{})
	require.ErrorIs(t, err, storagev1.ErrOutcomeUnknown, "an untyped error from a write is an unknown outcome")
	require.NotContains(t, err.Error(), "secret-value", "provider error text stays on the server")

	provider.set("GetPlan", nil, errors.New("read failed near \"secret-value\""))
	_, err = client.GetPlan(t.Context(), "n", "t")
	require.Error(t, err)
	require.False(t, errors.Is(err, storagev1.ErrOutcomeUnknown), "a read is never an unknown outcome")
	require.NotContains(t, err.Error(), "secret-value")

	provider.mu.Lock()
	provider.panics["DeletePlan"] = "provider bug"
	provider.mu.Unlock()
	err = client.DeletePlan(t.Context(), "n", "t")
	require.ErrorIs(t, err, storagev1.ErrOutcomeUnknown, "a panic in a write is an unknown outcome")
	_, err = client.GetPlan(t.Context(), "n", "t")
	require.Error(t, err, "the server keeps serving after a provider panic")
}

func TestClientRefusesInvalidRequestsWithoutSending(t *testing.T) {
	provider := newRecordingProvider()
	client := wiretest.Serve(t, serveBoth(provider))

	err := client.SendMessage(t.Context(), &storagev1.Message{Namespace: "n", Content: "bad \xff"})
	require.ErrorIs(t, err, storagev1.ErrValidation)
	require.ErrorContains(t, err, "UTF-8")

	err = client.SavePlan(t.Context(), "n", "t", &storagev1.PlanState{CreatedAt: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)})
	require.ErrorIs(t, err, storagev1.ErrValidation)
	require.ErrorContains(t, err, "PlanState.CreatedAt")

	err = client.SendMessage(t.Context(), &storagev1.Message{Namespace: "n", Content: strings.Repeat("x", wire.MaxMessageBytes)})
	var tooLarge storagev1.TooLargeError
	require.ErrorAs(t, err, &tooLarge)
	require.Equal(t, int64(wire.MaxMessageBytes), tooLarge.Limit)

	require.Empty(t, provider.callsTo("SendMessage"), "nothing reached the server")
	require.Empty(t, provider.callsTo("SavePlan"))

	// A context that is already done starts no attempt, so even a write is
	// safely unavailable rather than an unknown outcome.
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	err = client.SavePlan(canceled, "n", "t", &storagev1.PlanState{})
	require.ErrorIs(t, err, storagev1.ErrUnavailable)
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, errors.Is(err, storagev1.ErrOutcomeUnknown))
	require.Empty(t, provider.callsTo("SavePlan"))
}

// A provider result protobuf cannot encode may follow a committed write, so
// the client reports an unknown outcome rather than a refused request.
func TestUnencodableResultsAreProviderErrors(t *testing.T) {
	provider := newRecordingProvider()
	client := wiretest.Serve(t, serveBoth(provider))

	provider.set("GetMessages", []any{[]storagev1.Message{{Content: "stored \xff"}}}, nil)
	_, err := client.GetMessages(t.Context(), "n", "t", "p", true)
	require.ErrorIs(t, err, storagev1.ErrOutcomeUnknown)

	provider.set("GetPlan", []any{&storagev1.PlanState{UpdatedAt: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}}, nil)
	_, err = client.GetPlan(t.Context(), "n", "t")
	require.Error(t, err)
	require.False(t, errors.Is(err, storagev1.ErrValidation), "the request was valid")
}

func TestResponsesNameTheContractRevisionAndDataset(t *testing.T) {
	provider := newRecordingProvider()
	provider.set("Describe", []any{storagev1.ProviderInfo{
		ContractVersion: storagev1.ContractVersion, Contracts: []string{storagev1.StateContract},
		DatasetID: "dataset-7", DatasetOrigin: storagev1.DatasetOriginNew, SchemaVersion: "1",
		Durability: storagev1.DurabilityLocalFsync,
	}}, nil)
	conn := wiretest.Dial(t, serveBoth(provider))
	info, server, err := wire.NewClient(conn, wire.ClientConfig{}).DescribeServer(t.Context())
	require.NoError(t, err)
	require.Equal(t, "dataset-7", info.DatasetID)
	require.Equal(t, int64(wire.ProtocolRevision), server.ProtocolRevision)
	require.Equal(t, "wiretest", server.AdapterName)
	require.NotEmpty(t, server.LibraryVersion)

	var header metadata.MD
	bound := metadata.AppendToOutgoingContext(t.Context(), wire.HeaderContract, storagev1.ContractVersion,
		wire.HeaderRevision, "1", wire.HeaderDataset, "dataset-7")
	_, err = storagepb.NewResultsClient(conn).GetPlan(bound, &storagepb.GetPlanRequest{}, grpc.Header(&header))
	require.NoError(t, err)
	require.Equal(t, []string{storagev1.ContractVersion}, header.Get(wire.HeaderContract))
	require.Equal(t, []string{"1"}, header.Get(wire.HeaderRevision))
	require.Equal(t, []string{"dataset-7"}, header.Get(wire.HeaderDataset))
}

// rejectAll fails every authentication.
type rejectAll struct{}

func (rejectAll) Authenticate(context.Context, string, metadata.MD) error {
	return errors.New("no credentials")
}

// The server authenticates from headers before it reads the request.
func TestFailedAuthenticationIsUnavailableAndMarked(t *testing.T) {
	provider := newRecordingProvider()
	server, err := wire.NewServer(wire.ServerConfig{State: provider, Authenticator: rejectAll{}})
	require.NoError(t, err)
	conn := serveInsecure(t, server)
	client := wire.NewClient(conn, wire.ClientConfig{})

	err = client.SavePlan(t.Context(), "n", "t", &storagev1.PlanState{})
	require.ErrorIs(t, err, storagev1.ErrUnavailable)
	require.ErrorIs(t, err, wire.ErrAuthentication)
	require.False(t, errors.Is(err, storagev1.ErrOutcomeUnknown), "an unauthenticated write committed nothing")
	require.Empty(t, provider.callsTo("SavePlan"))
}

func TestNewServerRequiresAProvider(t *testing.T) {
	_, err := wire.NewServer(wire.ServerConfig{})
	require.Error(t, err)
}

// serveInsecure serves server on a TCP loopback without TLS. Only tests that
// inject transport faults below TLS use it; wiretest always uses TLS.
func serveInsecure(t *testing.T, server *wire.Server) *grpc.ClientConn {
	t.Helper()
	return dialInsecure(t, serveInsecureWith(t, server))
}

// serveInsecureWith serves server on a TCP loopback without TLS, with extra
// gRPC server options, and returns its address.
func serveInsecureWith(t *testing.T, server *wire.Server, opts ...grpc.ServerOption) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	grpcServer := grpc.NewServer(append(server.ServerOptions(), opts...)...)
	server.Register(grpcServer)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)
	return listener.Addr().String()
}

func dialInsecure(t *testing.T, address string) *grpc.ClientConn {
	t.Helper()
	conn, err := wire.Dial("passthrough:///"+address, wire.DialConfig{TransportCredentials: insecure.NewCredentials()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}
