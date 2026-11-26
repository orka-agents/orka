package wire_test

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/orka-agents/orka/pkg/storage/v1/wire"
	"github.com/orka-agents/orka/pkg/storage/v1/wire/storagepb"
	"github.com/orka-agents/orka/pkg/storage/v1/wire/wiretest"
)

func TestServerRefusesCallsForAnotherDataset(t *testing.T) {
	provider := newRecordingProvider()
	conn := wiretest.Dial(t, serveBoth(provider))
	plans := storagepb.NewResultsClient(conn)
	unnamed := metadata.AppendToOutgoingContext(t.Context(), wire.HeaderContract, storagev1.ContractVersion, wire.HeaderRevision, "1")

	_, err := plans.SavePlan(unnamed, &storagepb.SavePlanRequest{Namespace: "n", TaskName: "t"})
	code, reason := reasonOf(t, err)
	require.Equal(t, codes.InvalidArgument, code)
	require.Equal(t, wire.ReasonValidation, reason, "a call must name its dataset")

	var header metadata.MD
	_, err = plans.GetPlan(datasetContext(t.Context(), "another-dataset"), &storagepb.GetPlanRequest{}, grpc.Header(&header))
	_, reason = reasonOf(t, err)
	require.Equal(t, wire.ReasonDatasetMismatch, reason, "a read for another dataset is refused")
	require.Equal(t, []string{testDataset}, header.Get(wire.HeaderDataset), "the refusal names the serving dataset")

	twice := metadata.AppendToOutgoingContext(contractContext(t.Context()), wire.HeaderDataset, testDataset)
	_, err = plans.GetPlan(twice, &storagepb.GetPlanRequest{})
	_, reason = reasonOf(t, err)
	require.Equal(t, wire.ReasonValidation, reason, "a call names one dataset")

	_, err = storagepb.NewDatasetClient(conn).HealthCheck(unnamed, &storagepb.HealthCheckRequest{})
	require.NoError(t, err, "an unbound health check is served")
	_, err = storagepb.NewDatasetClient(conn).Describe(unnamed, &storagepb.DescribeRequest{})
	require.NoError(t, err, "an unbound Describe is served")
	_, err = storagepb.NewDatasetClient(conn).Describe(datasetContext(t.Context(), "another-dataset"), &storagepb.DescribeRequest{})
	_, reason = reasonOf(t, err)
	require.Equal(t, wire.ReasonDatasetMismatch, reason, "a bound Describe must name the serving dataset")

	data := []byte("result")
	stream, err := storagepb.NewResultsClient(conn).SaveResult(metadata.AppendToOutgoingContext(
		datasetContext(t.Context(), "another-dataset"), wire.HeaderBlobSize, "6", wire.HeaderBlobSHA256, "00"))
	require.NoError(t, err)
	_ = stream.Send(&storagepb.SaveResultRequest{Namespace: "n", TaskName: "t", Data: data})
	_, err = stream.CloseAndRecv()
	_, reason = reasonOf(t, err)
	require.Equal(t, wire.ReasonDatasetMismatch, reason, "an upload for another dataset is refused before its blob is read")

	require.Empty(t, provider.callsTo("SavePlan"))
	require.Empty(t, provider.callsTo("GetPlan"))
	require.Empty(t, provider.callsTo("SaveResult"))

	other := wire.NewClient(conn, wire.ClientConfig{Dataset: "another-dataset"})
	err = other.SavePlan(t.Context(), "n", "t", &storagev1.PlanState{})
	require.ErrorIs(t, err, storagev1.ErrDatasetMismatch)
	require.False(t, errors.Is(err, storagev1.ErrOutcomeUnknown))
	err = other.SaveArtifact(t.Context(), storagev1.SaveArtifactRequest{Filename: "f", Data: []byte("x")})
	require.ErrorIs(t, err, storagev1.ErrDatasetMismatch)
}

func TestServerFollowsTheProvidersDataset(t *testing.T) {
	provider := newRecordingProvider()
	server, err := wire.NewServer(serveBoth(provider))
	require.NoError(t, err)
	require.Empty(t, server.DatasetID())
	client := wire.NewClient(serveInsecure(t, server), boundConfig)

	require.NoError(t, client.SavePlan(t.Context(), "n", "t", &storagev1.PlanState{}))
	require.Equal(t, testDataset, server.DatasetID())
	require.Len(t, provider.callsTo("Describe"), 2, "the server described the state and artifacts providers")

	moved := describeDefault()
	moved.DatasetID = "replacement-dataset"
	provider.set("Describe", []any{moved}, nil)
	require.NoError(t, server.Refresh(t.Context()))
	require.Equal(t, "replacement-dataset", server.DatasetID())
	err = client.SavePlan(t.Context(), "n", "t", &storagev1.PlanState{})
	require.ErrorIs(t, err, storagev1.ErrDatasetMismatch, "calls for the old dataset are refused once the provider moved")
	require.Len(t, provider.callsTo("SavePlan"), 1)
}

func TestServerRefusesCallsWhenTheDatasetIsUnknown(t *testing.T) {
	for _, describeErr := range []error{errors.New("database is down"), storagev1.ErrNotReady} {
		provider := newRecordingProvider()
		provider.set("Describe", nil, describeErr)
		server, err := wire.NewServer(serveBoth(provider))
		require.NoError(t, err)
		client := wire.NewClient(serveInsecure(t, server), boundConfig)
		err = client.SavePlan(t.Context(), "n", "t", &storagev1.PlanState{})
		require.False(t, errors.Is(err, storagev1.ErrOutcomeUnknown), "nothing was committed: %v", err)
		require.ErrorIs(t, err, storagev1.ErrUnavailable)
		require.Empty(t, provider.callsTo("SavePlan"))
	}

	empty := newRecordingProvider()
	empty.set("Describe", []any{storagev1.ProviderInfo{}}, nil)
	server, err := wire.NewServer(serveBoth(empty))
	require.NoError(t, err)
	err = wire.NewClient(serveInsecure(t, server), boundConfig).SavePlan(t.Context(), "n", "t", &storagev1.PlanState{})
	require.ErrorIs(t, err, storagev1.ErrUnavailable, "a provider that reports no dataset serves nothing")
}

type separateProvider struct {
	*recordingProvider
	dataset *atomic.Pointer[string]
}

func newSeparateProvider(dataset string) separateProvider {
	provider := separateProvider{recordingProvider: newRecordingProvider(), dataset: &atomic.Pointer[string]{}}
	provider.dataset.Store(&dataset)
	return provider
}

func (p separateProvider) Describe(context.Context) (storagev1.ProviderInfo, error) {
	info := describeDefault()
	info.DatasetID = *p.dataset.Load()
	return info, nil
}

func TestServerRefusesProvidersOfDifferentDatasets(t *testing.T) {
	state := newRecordingProvider()
	artifacts := newSeparateProvider("artifacts-dataset")
	server, err := wire.NewServer(wire.ServerConfig{State: state, Artifacts: artifacts})
	require.NoError(t, err)
	require.Error(t, server.Refresh(t.Context()))
	err = wire.NewClient(serveInsecure(t, server), boundConfig).SavePlan(t.Context(), "n", "t", &storagev1.PlanState{})
	require.ErrorIs(t, err, storagev1.ErrUnavailable)
	require.Empty(t, state.callsTo("SavePlan"))
}

func TestServerForgetsItsDatasetWhenTheProvidersDisagree(t *testing.T) {
	state := newRecordingProvider()
	artifacts := newSeparateProvider(testDataset)
	server, err := wire.NewServer(wire.ServerConfig{State: state, Artifacts: artifacts})
	require.NoError(t, err)
	client := wire.NewClient(serveInsecure(t, server), boundConfig)
	require.NoError(t, client.SavePlan(t.Context(), "n", "t", &storagev1.PlanState{}))

	artifacts.dataset.Store(new("replacement-artifacts"))
	require.Error(t, server.Refresh(t.Context()))
	require.Empty(t, server.DatasetID())
	err = client.SavePlan(t.Context(), "n", "t", &storagev1.PlanState{})
	require.ErrorIs(t, err, storagev1.ErrUnavailable, "no call is admitted while the providers disagree")
	require.False(t, errors.Is(err, storagev1.ErrOutcomeUnknown))
	err = client.SaveArtifact(t.Context(), storagev1.SaveArtifactRequest{Filename: "f", Data: []byte("x")})
	require.ErrorIs(t, err, storagev1.ErrUnavailable)
	require.Len(t, state.callsTo("SavePlan"), 1)
	require.Empty(t, artifacts.callsTo("SaveArtifact"))

	artifacts.dataset.Store(new(testDataset))
	require.NoError(t, server.Refresh(t.Context()))
	require.NoError(t, client.SavePlan(t.Context(), "n", "t", &storagev1.PlanState{}), "calls resume once the providers agree")

	state.set("Describe", []any{storagev1.ProviderInfo{}}, nil)
	require.Error(t, server.Refresh(t.Context()))
	err = client.SavePlan(t.Context(), "n", "t", &storagev1.PlanState{})
	require.ErrorIs(t, err, storagev1.ErrUnavailable, "a provider that stops reporting a dataset serves nothing")
	require.Len(t, state.callsTo("SavePlan"), 2)
}

type lateDescriber struct {
	*recordingProvider
	delay time.Duration
}

func (p lateDescriber) Describe(ctx context.Context) (storagev1.ProviderInfo, error) {
	time.Sleep(p.delay)
	return p.recordingProvider.Describe(context.WithoutCancel(ctx))
}

func TestServerStartsNoProviderWorkAfterTheTimeLimit(t *testing.T) {
	provider := lateDescriber{recordingProvider: newRecordingProvider(), delay: 200 * time.Millisecond}
	server, err := wire.NewServer(wire.ServerConfig{State: provider, Limits: wire.Limits{
		MaxDuration: map[storagev1.CallClass]time.Duration{storagev1.CallInteractive: 50 * time.Millisecond},
	}})
	require.NoError(t, err)
	_, err = wire.NewClient(serveInsecure(t, server), boundConfig).GetPlan(t.Context(), "n", "t")
	require.ErrorIs(t, err, storagev1.ErrUnavailable)
	require.Empty(t, provider.callsTo("GetPlan"), "no provider work starts after the limit")
}

type gatedProvider struct {
	separateProvider
	gate    *atomic.Pointer[chan struct{}]
	entered chan struct{}
}

func (p gatedProvider) Describe(ctx context.Context) (storagev1.ProviderInfo, error) {
	info, err := p.separateProvider.Describe(ctx)
	if gate := p.gate.Load(); gate != nil {
		p.entered <- struct{}{}
		<-*gate
	}
	return info, err
}

func TestServerRefusesReadsWhoseDescriptionWasSuperseded(t *testing.T) {
	state := newRecordingProvider()
	artifacts := gatedProvider{separateProvider: newSeparateProvider(testDataset),
		gate: &atomic.Pointer[chan struct{}]{}, entered: make(chan struct{}, 1)}
	server, err := wire.NewServer(wire.ServerConfig{State: state, Artifacts: artifacts})
	require.NoError(t, err)
	client := wire.NewClient(serveInsecure(t, server), boundConfig)
	require.NoError(t, server.Refresh(t.Context()))

	gate := make(chan struct{})
	artifacts.gate.Store(&gate)
	read := make(chan error, 1)
	go func() {
		_, err := client.GetPlan(t.Context(), "n", "t")
		read <- err
	}()
	select {
	case <-artifacts.entered:
	case err := <-read:
		t.Fatalf("the read ended before its description: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the read never described the providers")
	}
	artifacts.gate.Store(nil)
	artifacts.dataset.Store(new("replacement-artifacts"))
	require.Error(t, server.Refresh(t.Context()))
	require.Empty(t, server.DatasetID())

	close(gate)
	err = <-read
	require.ErrorIs(t, err, storagev1.ErrUnavailable, "the superseded description admits nothing")
	require.Empty(t, state.callsTo("GetPlan"))
}

func TestServerFailsClosedForMethodsOutsideTheProtocol(t *testing.T) {
	for _, authenticator := range []wire.Authenticator{nil, rejectAll{}} {
		server, err := wire.NewServer(wire.ServerConfig{State: newRecordingProvider(), Authenticator: authenticator})
		require.NoError(t, err)
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		grpcServer := grpc.NewServer(server.ServerOptions()...)
		server.Register(grpcServer)
		healthpb.RegisterHealthServer(grpcServer, health.NewServer())
		go func() { _ = grpcServer.Serve(listener) }()
		t.Cleanup(grpcServer.Stop)
		conn := dialInsecure(t, listener.Addr().String())

		_, err = healthpb.NewHealthClient(conn).Check(t.Context(), &healthpb.HealthCheckRequest{})
		want := codes.Unimplemented
		if authenticator != nil {
			want = codes.Unauthenticated
		}
		require.Equal(t, want, status.Code(err), "authenticator %T", authenticator)
	}
}

func TestServerServesOnlyItsContractParts(t *testing.T) {
	provider := newRecordingProvider()
	artifactsOnly := wiretest.Serve(t, wire.ServerConfig{Artifacts: provider})
	require.NoError(t, artifactsOnly.SaveArtifact(t.Context(), storagev1.SaveArtifactRequest{Filename: "f", Data: []byte("x")}))
	err := artifactsOnly.SavePlan(t.Context(), "n", "t", &storagev1.PlanState{})
	require.ErrorIs(t, err, storagev1.ErrIncompatible, "a state RPC on an artifacts-only server is unimplemented")
	require.Empty(t, provider.callsTo("SavePlan"))

	stateOnly := wiretest.Serve(t, wire.ServerConfig{State: provider})
	_, err = stateOnly.ListArtifacts(t.Context(), storagev1.ArtifactOwner{})
	require.ErrorIs(t, err, storagev1.ErrIncompatible)
	require.Empty(t, provider.callsTo("ListArtifacts"))

	info, err := artifactsOnly.Describe(t.Context())
	require.NoError(t, err)
	require.Equal(t, []string{storagev1.ArtifactsContract}, info.Contracts)
	require.Equal(t, int64(storagev1.MaxArtifactBytes), info.Limits.MaxArtifactBytes)
	info, err = stateOnly.Describe(t.Context())
	require.NoError(t, err)
	require.Equal(t, []string{storagev1.StateContract}, info.Contracts)
	require.Zero(t, info.Limits.MaxArtifactBytes, "a state-only server advertises no artifact limit")
}

func TestServerShedsCallsOverTheClassLimit(t *testing.T) {
	provider := newRecordingProvider()
	release := make(chan struct{})
	provider.block["SavePlan"] = release
	client := wiretest.Serve(t, wire.ServerConfig{State: provider, Artifacts: provider, Limits: wire.Limits{
		MaxConcurrent: map[storagev1.CallClass]int{storagev1.CallControl: 1, storagev1.CallInteractive: 1, storagev1.CallBulk: 1},
	}})
	blocked := make(chan error, 1)
	go func() { blocked <- client.SavePlan(t.Context(), "n", "t", &storagev1.PlanState{}) }()
	provider.waitEntered(t, "SavePlan")

	err := client.SavePlan(t.Context(), "n", "t", &storagev1.PlanState{})
	require.ErrorIs(t, err, storagev1.ErrUnavailable)
	require.False(t, errors.Is(err, storagev1.ErrOutcomeUnknown), "a shed write committed nothing")
	_, err = client.GetPlan(t.Context(), "n", "t")
	require.ErrorIs(t, err, storagev1.ErrUnavailable, "the control class is full for reads too")
	require.NoError(t, client.HealthCheck(t.Context()), "health checks have their own capacity")
	require.NoError(t, client.SaveResult(t.Context(), "n", "t", []byte("r")), "bulk calls have their own capacity")

	close(release)
	require.NoError(t, <-blocked)
	require.NoError(t, client.SavePlan(t.Context(), "n", "t", &storagev1.PlanState{}), "capacity returns when a call ends")
	require.Len(t, provider.callsTo("SavePlan"), 2)
	require.Empty(t, provider.callsTo("GetPlan"))
}

func TestShedCallsUseAReasonTheClientKnows(t *testing.T) {
	provider := newRecordingProvider()
	release := make(chan struct{})
	provider.block["SavePlan"] = release
	server, err := wire.NewServer(wire.ServerConfig{State: provider, Limits: wire.Limits{
		MaxConcurrent: map[storagev1.CallClass]int{storagev1.CallInteractive: 1},
	}})
	require.NoError(t, err)
	conn := serveInsecure(t, server)
	blocked := make(chan error, 1)
	go func() {
		blocked <- wire.NewClient(conn, boundConfig).SavePlan(t.Context(), "n", "t", &storagev1.PlanState{})
	}()
	provider.waitEntered(t, "SavePlan")

	plans := storagepb.NewResultsClient(conn)
	for revision, want := range map[string]string{"1": wire.ReasonUnavailable, "2": wire.ReasonOverloaded} {
		ctx := metadata.AppendToOutgoingContext(t.Context(), wire.HeaderContract, storagev1.ContractVersion,
			wire.HeaderRevision, revision, wire.HeaderDataset, testDataset)
		_, err := plans.SavePlan(ctx, &storagepb.SavePlanRequest{Namespace: "n", TaskName: "t"})
		code, reason := reasonOf(t, err)
		require.Equal(t, codes.Unavailable, code)
		require.Equal(t, want, reason, "revision %s", revision)
	}
	close(release)
	require.NoError(t, <-blocked)
	require.Len(t, provider.callsTo("SavePlan"), 1)
}

type deadlineProvider struct {
	*recordingProvider
	deadline chan time.Time
}

func (p deadlineProvider) GetPlan(ctx context.Context, _, _ string) (*storagev1.PlanState, error) {
	deadline, _ := ctx.Deadline()
	p.deadline <- deadline
	return nil, nil
}

func TestServerCapsCallDurations(t *testing.T) {
	provider := deadlineProvider{recordingProvider: newRecordingProvider(), deadline: make(chan time.Time, 2)}
	client := wiretest.Serve(t, wire.ServerConfig{State: provider, Limits: wire.Limits{
		MaxDuration: map[storagev1.CallClass]time.Duration{storagev1.CallInteractive: time.Second},
	}})
	started := time.Now()
	_, err := client.GetPlan(t.Context(), "n", "t")
	require.NoError(t, err)
	deadline := <-provider.deadline
	require.False(t, deadline.IsZero(), "the provider has a deadline")
	require.WithinDuration(t, started.Add(time.Second), deadline, 500*time.Millisecond)

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	_, err = client.GetPlan(ctx, "n", "t")
	require.NoError(t, err)
	shorter := <-provider.deadline
	require.True(t, shorter.Before(started.Add(time.Second)), "a shorter caller deadline wins")
}

type observedCalls struct {
	calls chan wire.CallInfo
}

func (o observedCalls) ObserveCall(call wire.CallInfo) { o.calls <- call }

func TestServerReportsCalls(t *testing.T) {
	provider := newRecordingProvider()
	observer := observedCalls{calls: make(chan wire.CallInfo, 8)}
	client := wiretest.Serve(t, wire.ServerConfig{State: provider, Artifacts: provider, Observer: observer})
	<-observer.calls

	require.NoError(t, client.SaveResult(t.Context(), "n", "t", []byte("r")))
	call := <-observer.calls
	require.Equal(t, "SaveResult", call.Method)
	require.Equal(t, storagev1.MethodWrite, call.Kind)
	require.Equal(t, storagev1.CallBulk, call.Class)
	require.Equal(t, codes.OK, call.Code)

	provider.set("GetPlan", nil, storagev1.ErrNotFound)
	_, err := client.GetPlan(t.Context(), "n", "t")
	require.ErrorIs(t, err, storagev1.ErrNotFound)
	call = <-observer.calls
	require.Equal(t, storagev1.CallInteractive, call.Class)
	require.Equal(t, wire.ReasonNotFound, call.Reason)
}
