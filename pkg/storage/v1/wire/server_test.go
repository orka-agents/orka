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

// testEnvelope returns an envelope whose request digest covers method and
// args, as the storage client issues it.
func testEnvelope(t *testing.T, method string, args ...any) storagev1.WriteEnvelope {
	t.Helper()
	digest, err := storagev1.RequestDigest(method, args...)
	require.NoError(t, err)
	return storagev1.WriteEnvelope{
		DatasetID:     "dataset-1",
		Fence:         storagev1.ControllerEpochFence{Name: "orka", Epoch: 7, HolderID: "holder"},
		OperationID:   "operation-" + method,
		RequestDigest: digest,
		IssuedAt:      time.Date(2026, 11, 17, 1, 2, 3, 4, time.UTC),
		Sequence:      9,
	}
}

var testFence = storagev1.TaskDataFence{
	Namespace: "n", TaskName: "t", Generation: 4,
	Jobs: []storagev1.TaskJobIdentity{{Namespace: "n", TaskUID: "task-uid", JobUID: "job-uid"}},
}

// Every method with a wire RPC carries every argument, its Task data fence
// and, for a write, its envelope to the provider, and every result and
// written record back, through the generated client and server over real
// TLS. The server recomputes each write's request digest from the decoded
// arguments, so a fill the wire did not preserve exactly fails here.
func TestEveryWireMethodRoundTripsArgumentsAndResults(t *testing.T) {
	provider := newRecordingProvider()
	client := wiretest.Serve(t, provider)
	clientValue := reflect.ValueOf(client)
	require.NotEmpty(t, wireMethods)
	for _, name := range wireMethods {
		t.Run(name, func(t *testing.T) {
			method := clientValue.MethodByName(name)
			require.True(t, method.IsValid(), "the client implements %s", name)
			info, ok := storagev1.LookupMethod(name)
			require.True(t, ok, "%s is in the method catalog", name)
			methodType := method.Type()
			var args, sent []any
			callArgs := []reflect.Value{{}}
			for index := 1; index < methodType.NumIn(); index++ {
				arg := wire.FillValue(methodType.In(index), fmt.Sprintf("%s.arg%d", name, index))
				callArgs = append(callArgs, arg)
				args = append(args, arg.Interface())
				sent = append(sent, wire.NormalizeValue(arg.Interface()))
			}
			ctx := storagev1.WithTaskDataFence(t.Context(), testFence)
			var envelope storagev1.WriteEnvelope
			if info.Kind == storagev1.MethodWrite {
				envelope = testEnvelope(t, name, args...)
				ctx = storagev1.WithWriteEnvelope(ctx, envelope)
			}
			callArgs[0] = reflect.ValueOf(ctx)
			var results []any
			for index := 0; index < methodType.NumOut()-1; index++ {
				results = append(results, wire.FillValue(methodType.Out(index), fmt.Sprintf("%s.result%d", name, index)).Interface())
			}
			if name == "Describe" {
				// The client is bound to the dataset the provider serves.
				info := results[0].(storagev1.ProviderInfo)
				info.DatasetID = testDataset
				results[0] = info
			}
			provider.set(name, results, nil)
			written := make([]any, 0, len(wireWriteBacks[name]))
			for _, position := range wireWriteBacks[name] {
				written = append(written, wire.FillValue(methodType.In(position).Elem(), name+".written").Interface())
			}
			provider.setWritten(name, written)

			out := method.Call(callArgs)
			require.Nil(t, out[len(out)-1].Interface(), "%s succeeds", name)
			calls := provider.callsTo(name)
			require.NotEmpty(t, calls)
			call := calls[len(calls)-1]
			require.Len(t, call.args, len(sent))
			for index, want := range sent {
				require.Equal(t, want, wire.NormalizeValue(call.args[index]), "argument %d", index+1)
			}
			require.True(t, call.hasFence, "the Task data fence reaches the provider")
			require.Equal(t, testFence, call.fence)
			require.Equal(t, info.Kind == storagev1.MethodWrite, call.hasEnvelope, "only a write carries its envelope")
			if call.hasEnvelope {
				require.Equal(t, envelope, call.envelope)
			}
			for index, want := range results {
				require.Equal(t, wire.NormalizeValue(want), wire.NormalizeValue(out[index].Interface()), "result %d", index)
			}
			for index, position := range wireWriteBacks[name] {
				require.Equal(t, wire.NormalizeValue(written[index]), wire.NormalizeValue(callArgs[position].Elem().Interface()),
					"argument %d gets the record the provider wrote", position)
			}
		})
	}
}

// A provider writes the ID it assigns into SendMessage's record; the caller
// sees it over the wire as it would in process.
func TestSendMessageWritesTheAssignedIDBack(t *testing.T) {
	provider := newRecordingProvider()
	client := wiretest.Serve(t, provider)
	sentAt := time.Date(2026, 11, 17, 3, 4, 5, 6, time.UTC)
	provider.setWritten("SendMessage", []any{storagev1.Message{ID: 42, Namespace: "n", Content: "hi", CreatedAt: sentAt}})

	msg := &storagev1.Message{Namespace: "n", Content: "hi"}
	require.NoError(t, client.SendMessage(t.Context(), msg))
	require.Equal(t, int64(42), msg.ID)
	require.True(t, sentAt.Equal(msg.CreatedAt))
}

// A zero Task data fence crosses as an empty header value and still reaches
// the provider as a fence, as in process, never as no fence at all.
func TestAZeroTaskDataFenceStillReachesTheProvider(t *testing.T) {
	provider := newRecordingProvider()
	client := wiretest.Serve(t, provider)
	_, err := client.GetPlan(storagev1.WithTaskDataFence(t.Context(), storagev1.TaskDataFence{}), "n", "t")
	require.NoError(t, err)
	calls := provider.callsTo("GetPlan")
	require.Len(t, calls, 1)
	require.True(t, calls[0].hasFence, "the zero fence reaches the provider")
	require.Equal(t, storagev1.TaskDataFence{}, calls[0].fence)
}

func TestServerRefusesARequestThatDoesNotMatchItsDigest(t *testing.T) {
	provider := newRecordingProvider()
	client := wiretest.Serve(t, provider)
	plan := &storagev1.PlanState{Summary: "s"}
	other := testEnvelope(t, "SavePlan", "n", "t", &storagev1.PlanState{Summary: "other"})
	err := client.SavePlan(storagev1.WithWriteEnvelope(t.Context(), other), "n", "t", plan)
	require.ErrorIs(t, err, storagev1.ErrValidation)
	require.ErrorContains(t, err, "request digest")
	require.False(t, errors.Is(err, storagev1.ErrOutcomeUnknown), "a refused write committed nothing")
	require.Empty(t, provider.callsTo("SavePlan"))

	renamed := testEnvelope(t, "DeletePlan", "n", "t", plan)
	err = client.SavePlan(storagev1.WithWriteEnvelope(t.Context(), renamed), "n", "t", plan)
	require.ErrorIs(t, err, storagev1.ErrValidation, "the digest covers the method name")
	require.Empty(t, provider.callsTo("SavePlan"))
}

// Only a write carries an envelope. The client leaves it off other calls,
// and the server refuses one a read carries.
func TestServerRefusesAnEnvelopeOnARead(t *testing.T) {
	provider := newRecordingProvider()
	plans := storagepb.NewResultsClient(wiretest.Dial(t, provider))
	encoded, err := proto.Marshal(&storagepb.WriteEnvelope{DatasetId: "dataset-1", OperationId: "operation-1"})
	require.NoError(t, err)
	ctx := metadata.AppendToOutgoingContext(contractContext(t.Context()), wire.HeaderEnvelope, string(encoded))
	_, err = plans.GetPlan(ctx, &storagepb.GetPlanRequest{Namespace: "n", TaskName: "t"})
	code, reason := reasonOf(t, err)
	require.Equal(t, codes.InvalidArgument, code)
	require.Equal(t, wire.ReasonValidation, reason)
	require.Empty(t, provider.callsTo("GetPlan"))
}

func TestServerRefusesAnUndecodableTaskDataFence(t *testing.T) {
	provider := newRecordingProvider()
	plans := storagepb.NewResultsClient(wiretest.Dial(t, provider))
	ctx := metadata.AppendToOutgoingContext(contractContext(t.Context()), wire.HeaderTaskDataFence, "\xff\xff")
	_, err := plans.GetPlan(ctx, &storagepb.GetPlanRequest{Namespace: "n", TaskName: "t"})
	_, reason := reasonOf(t, err)
	require.Equal(t, wire.ReasonValidation, reason)

	unknown, err := proto.Marshal(&storagepb.TaskDataFence{Namespace: "n"})
	require.NoError(t, err)
	unknown = append(unknown, 0xf8, 0x07, 0x01) // field 127, varint 1
	ctx = metadata.AppendToOutgoingContext(contractContext(t.Context()), wire.HeaderTaskDataFence, string(unknown))
	_, err = plans.GetPlan(ctx, &storagepb.GetPlanRequest{Namespace: "n", TaskName: "t"})
	_, reason = reasonOf(t, err)
	require.Equal(t, wire.ReasonUnsupportedField, reason)
	require.Empty(t, provider.callsTo("GetPlan"))
}

// contractContext names the contract and revision, as the client does, for a
// raw stub call.
// contractContext names the contract, revision 1 and the recording
// provider's dataset, as a bound client does.
func contractContext(ctx context.Context) context.Context {
	return datasetContext(ctx, testDataset)
}

func datasetContext(ctx context.Context, dataset string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, wire.HeaderContract, storagev1.ContractVersion, wire.HeaderRevision, "1",
		wire.HeaderDataset, dataset)
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
	plans := storagepb.NewResultsClient(wiretest.Dial(t, provider))

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
	plans := storagepb.NewResultsClient(wiretest.Dial(t, provider))
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
	client := wiretest.Serve(t, provider)
	envelope := testEnvelope(t, "SavePlan", "n", "t", &storagev1.PlanState{Summary: "s"})
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

func TestProviderFailuresCrossTheWire(t *testing.T) {
	provider := newRecordingProvider()
	client := wiretest.Serve(t, provider)

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
	client := wiretest.Serve(t, provider)

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
	client := wiretest.Serve(t, provider)

	provider.set("ConsumeMessages", []any{[]storagev1.Message{{Content: "stored \xff"}}}, nil)
	_, err := client.ConsumeMessages(t.Context(), storagev1.ConsumeMessagesRequest{Namespace: "n", TaskName: "t", ParentTask: "p"})
	require.ErrorIs(t, err, storagev1.ErrOutcomeUnknown, "ConsumeMessages marks what it returns read, so it is a write")

	provider.set("ListUnreadMessages", []any{[]storagev1.Message{{Content: "stored \xff"}}}, nil)
	_, err = client.ListUnreadMessages(t.Context(), "n", "t", "p", 0)
	require.Error(t, err)
	require.False(t, errors.Is(err, storagev1.ErrOutcomeUnknown), "a read is never an unknown outcome")

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
	conn := wiretest.Dial(t, provider)
	info, server, err := wire.NewClient(conn, wire.ClientConfig{}).DescribeServer(t.Context())
	require.NoError(t, err)
	require.Equal(t, "dataset-7", info.DatasetID)
	require.Equal(t, int64(wire.ProtocolRevision), server.ProtocolRevision)
	require.Equal(t, "wiretest", server.AdapterName)
	require.NotEmpty(t, server.LibraryVersion)

	var header metadata.MD
	_, err = storagepb.NewResultsClient(conn).GetPlan(datasetContext(t.Context(), "dataset-7"), &storagepb.GetPlanRequest{}, grpc.Header(&header))
	require.NoError(t, err)
	require.Equal(t, []string{storagev1.ContractVersion}, header.Get(wire.HeaderContract))
	require.Equal(t, []string{"1"}, header.Get(wire.HeaderRevision))
	require.Equal(t, []string{"dataset-7"}, header.Get(wire.HeaderDataset))
}

// The server refuses, before the provider runs, a call that names another
// dataset, names none, or names more than one. Describe and HealthCheck may
// name none, so a client can learn its dataset, but one that names a dataset
// is checked too.
func TestServerChecksTheRequestDataset(t *testing.T) {
	provider := newRecordingProvider()
	conn := wiretest.Dial(t, provider)

	other := wire.NewClient(conn, wire.ClientConfig{Dataset: "dataset-other"})
	_, err := other.GetPlan(t.Context(), "n", "t")
	require.ErrorIs(t, err, storagev1.ErrDatasetMismatch)
	err = other.SavePlan(t.Context(), "n", "t", &storagev1.PlanState{})
	require.ErrorIs(t, err, storagev1.ErrDatasetMismatch)
	require.False(t, errors.Is(err, storagev1.ErrOutcomeUnknown), "a refused write committed nothing")
	described, err := other.Describe(t.Context())
	require.NoError(t, err, "a bound client's Describe reports the serving dataset, as in process")
	require.Equal(t, testDataset, described.DatasetID)
	_, err = storagepb.NewDatasetClient(conn).Describe(datasetContext(t.Context(), "dataset-other"), &storagepb.DescribeRequest{})
	_, reason := reasonOf(t, err)
	require.Equal(t, wire.ReasonDatasetMismatch, reason, "a Describe that names a dataset is checked")

	unbound := wire.NewClient(conn, wire.ClientConfig{})
	_, err = unbound.GetPlan(t.Context(), "n", "t")
	require.ErrorIs(t, err, storagev1.ErrValidation)
	require.NoError(t, unbound.HealthCheck(t.Context()))
	info, err := unbound.Describe(t.Context())
	require.NoError(t, err)
	require.Equal(t, testDataset, info.DatasetID)

	repeated := metadata.AppendToOutgoingContext(contractContext(t.Context()), wire.HeaderDataset, testDataset)
	_, err = storagepb.NewResultsClient(conn).GetPlan(repeated, &storagepb.GetPlanRequest{})
	_, reason = reasonOf(t, err)
	require.Equal(t, wire.ReasonValidation, reason)

	require.Empty(t, provider.callsTo("GetPlan"), "a refused call never reaches the provider")
	require.Empty(t, provider.callsTo("SavePlan"))
}

// A read describes the provider again before it runs, so a provider whose
// dataset changed while it serves never answers a client bound to the dataset
// it served before, even when no Describe call came in between.
func TestServerRechecksTheDatasetBeforeEachRead(t *testing.T) {
	provider := newRecordingProvider()
	conn := wiretest.Dial(t, provider)
	client := wire.NewClient(conn, wire.ClientConfig{Dataset: testDataset})
	_, err := client.GetPlan(t.Context(), "n", "t")
	require.NoError(t, err)
	require.Len(t, provider.callsTo("GetPlan"), 1)

	provider.set("Describe", []any{storagev1.ProviderInfo{DatasetID: "dataset-switched"}}, nil)
	_, err = client.GetPlan(t.Context(), "n", "t")
	require.ErrorIs(t, err, storagev1.ErrDatasetMismatch)
	_, err = client.ListUnreadMessages(t.Context(), "n", "t", "p", 0)
	require.ErrorIs(t, err, storagev1.ErrDatasetMismatch)
	require.Len(t, provider.callsTo("GetPlan"), 1, "a read for the dataset served before never reaches the provider")
	require.Empty(t, provider.callsTo("ListUnreadMessages"))

	switched := wire.NewClient(conn, wire.ClientConfig{Dataset: "dataset-switched"})
	var header metadata.MD
	_, err = storagepb.NewResultsClient(conn).GetPlan(datasetContext(t.Context(), "dataset-switched"), &storagepb.GetPlanRequest{}, grpc.Header(&header))
	require.NoError(t, err)
	require.Equal(t, []string{"dataset-switched"}, header.Get(wire.HeaderDataset), "the response names the dataset the read was checked against")
	_, err = switched.GetPlan(t.Context(), "n", "t")
	require.NoError(t, err)

	provider.set("Describe", nil, errors.New("the dataset is not open"))
	_, err = switched.GetPlan(t.Context(), "n", "t")
	require.ErrorIs(t, err, storagev1.ErrUnavailable, "a read the server cannot check against a fresh description is refused")
	require.Len(t, provider.callsTo("GetPlan"), 3)
}

// A server whose provider cannot describe its dataset cannot check a call's
// dataset, so it refuses the call as unavailable without running it.
func TestServerThatCannotNameItsDatasetIsUnavailable(t *testing.T) {
	provider := newRecordingProvider()
	provider.set("Describe", nil, errors.New("the dataset is not open"))
	client := wire.NewClient(wiretest.Dial(t, provider), wire.ClientConfig{Dataset: testDataset})

	err := client.SavePlan(t.Context(), "n", "t", &storagev1.PlanState{})
	require.ErrorIs(t, err, storagev1.ErrUnavailable)
	require.False(t, errors.Is(err, storagev1.ErrOutcomeUnknown), "the write never ran")
	require.NotContains(t, err.Error(), "not open", "the provider's error text stays on the server")
	require.Empty(t, provider.callsTo("SavePlan"))

	provider.set("Describe", nil, fmt.Errorf("dataset schema is newer: %w", storagev1.ErrIncompatible))
	err = client.SavePlan(t.Context(), "n", "t", &storagev1.PlanState{})
	require.ErrorIs(t, err, storagev1.ErrIncompatible, "a contract error from Describe keeps its reason")
	require.Empty(t, provider.callsTo("SavePlan"))
}

// foreignResults answers plan calls for whatever datasets it is given, as a
// server that does not check the request's dataset might.
type foreignResults struct {
	storagepb.UnimplementedResultsServer
	served []string
}

func (r foreignResults) answer(ctx context.Context) error {
	header := metadata.Pairs(wire.HeaderContract, storagev1.ContractVersion, wire.HeaderRevision, "1")
	for _, dataset := range r.served {
		header.Append(wire.HeaderDataset, dataset)
	}
	return grpc.SetHeader(ctx, header)
}

func (r foreignResults) GetPlan(ctx context.Context, _ *storagepb.GetPlanRequest) (*storagepb.GetPlanResponse, error) {
	return &storagepb.GetPlanResponse{}, r.answer(ctx)
}

func (r foreignResults) SavePlan(ctx context.Context, _ *storagepb.SavePlanRequest) (*storagepb.SavePlanResponse, error) {
	return &storagepb.SavePlanResponse{}, r.answer(ctx)
}

// A bound client refuses a response that does not come from its dataset. A
// write that reached such a server may have changed another dataset, so its
// outcome is unknown.
func TestClientRefusesAResponseFromAnotherDataset(t *testing.T) {
	for name, served := range map[string][]string{
		"another dataset":       {"dataset-other"},
		"no dataset":            nil,
		"more than one dataset": {testDataset, "dataset-other"},
	} {
		t.Run(name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			grpcServer := grpc.NewServer()
			storagepb.RegisterResultsServer(grpcServer, foreignResults{served: served})
			go func() { _ = grpcServer.Serve(listener) }()
			t.Cleanup(grpcServer.Stop)
			client := wire.NewClient(dialInsecure(t, listener.Addr().String()), wire.ClientConfig{Dataset: testDataset})

			_, err = client.GetPlan(t.Context(), "n", "t")
			require.ErrorIs(t, err, storagev1.ErrDatasetMismatch)
			err = client.SavePlan(t.Context(), "n", "t", &storagev1.PlanState{})
			require.ErrorIs(t, err, storagev1.ErrOutcomeUnknown)
		})
	}
}

// rejectAll fails every authentication.
type rejectAll struct{}

func (rejectAll) Authenticate(context.Context, string, metadata.MD) error {
	return errors.New("no credentials")
}

// The server authenticates from headers before it reads the request.
func TestFailedAuthenticationIsUnauthenticated(t *testing.T) {
	provider := newRecordingProvider()
	server, err := wire.NewServer(wire.ServerConfig{State: provider, Authenticator: rejectAll{}})
	require.NoError(t, err)
	conn := serveInsecure(t, server)
	client := wire.NewClient(conn, wire.ClientConfig{})

	err = client.SavePlan(t.Context(), "n", "t", &storagev1.PlanState{})
	require.ErrorIs(t, err, storagev1.ErrUnauthenticated)
	require.False(t, errors.Is(err, storagev1.ErrOutcomeUnknown), "an unauthenticated write committed nothing")
	require.Empty(t, provider.callsTo("SavePlan"))
}

func TestNewServerRequiresAStateProvider(t *testing.T) {
	_, err := wire.NewServer(wire.ServerConfig{})
	require.Error(t, err)
}

// serveInsecure serves server on a TCP loopback without TLS. Only tests that
// inject transport faults below TLS use it; wiretest always uses TLS.
func serveInsecure(t *testing.T, server *wire.Server) *grpc.ClientConn {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	grpcServer := grpc.NewServer(server.ServerOptions()...)
	server.Register(grpcServer)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)
	return dialInsecure(t, listener.Addr().String())
}

func dialInsecure(t *testing.T, address string) *grpc.ClientConn {
	t.Helper()
	conn, err := wire.Dial("passthrough:///"+address, wire.DialConfig{TransportCredentials: insecure.NewCredentials()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}
