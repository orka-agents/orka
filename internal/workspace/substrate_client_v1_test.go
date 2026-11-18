package workspace

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	ateapipb "github.com/orka-agents/orka/internal/substratepb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// stubSubstrateControl answers only the RPCs a test configures; any other call
// panics through the nil embedded interface.
type stubSubstrateControl struct {
	ateapipb.ControlClient
	actor    *ateapipb.Actor
	template *ateapipb.ActorTemplate
	// templateErr, when set, fails every ActorTemplate lookup.
	templateErr error
	resumes     int
	deletes     []*ateapipb.DeleteActorRequest
	suspends    []*ateapipb.SuspendActorRequest
}

func (s *stubSubstrateControl) GetActor(context.Context, *ateapipb.GetActorRequest, ...grpc.CallOption) (*ateapipb.Actor, error) {
	return s.actor, nil
}

func (s *stubSubstrateControl) GetActorTemplate(context.Context, *ateapipb.GetActorTemplateRequest, ...grpc.CallOption) (*ateapipb.ActorTemplate, error) {
	if s.templateErr != nil {
		return nil, s.templateErr
	}
	return s.template, nil
}

func (s *stubSubstrateControl) ResumeActor(context.Context, *ateapipb.ResumeActorRequest, ...grpc.CallOption) (*ateapipb.ResumeActorResponse, error) {
	s.resumes++
	return &ateapipb.ResumeActorResponse{Actor: s.actor}, nil
}

func (s *stubSubstrateControl) DeleteActor(_ context.Context, req *ateapipb.DeleteActorRequest, _ ...grpc.CallOption) (*ateapipb.Actor, error) {
	s.deletes = append(s.deletes, req)
	return &ateapipb.Actor{}, nil
}

func (s *stubSubstrateControl) SuspendActor(_ context.Context, req *ateapipb.SuspendActorRequest, _ ...grpc.CallOption) (*ateapipb.SuspendActorResponse, error) {
	s.suspends = append(s.suspends, req)
	return &ateapipb.SuspendActorResponse{Actor: s.actor}, nil
}

func goldenTemplate(mode ateapipb.GoldenSnapshotMode) *ateapipb.ActorTemplate {
	return &ateapipb.ActorTemplate{
		Metadata: &ateapipb.ResourceMetadata{Atespace: "team-a", Name: "mcp", Uid: "template-uid"},
		SnapshotConfig: &ateapipb.SnapshotConfig{
			PreferredFidelity:    ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES,
			GoldenSnapshotConfig: &ateapipb.GoldenSnapshotConfig{Mode: mode},
		},
	}
}

func freshActor() *ateapipb.Actor {
	return &ateapipb.Actor{
		Metadata:      &ateapipb.ResourceMetadata{Atespace: "team-a", Name: "actor-1", Uid: "actor-uid"},
		ActorTemplate: &ateapipb.ObjectRef{Atespace: "team-a", Name: "mcp"},
		Status:        &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_SUSPENDED},
	}
}

func snapshot(generation int32, fidelity ateapipb.SnapshotFidelity, state ateapipb.SnapshotStorageState) *ateapipb.Snapshot {
	return &ateapipb.Snapshot{
		Uuid: fmt.Sprintf("snapshot-%d", generation), Generation: generation, Fidelity: fidelity,
		ActorTemplateUid: fmt.Sprintf("template-uid-%d", generation),
		DurableSnapshot: &ateapipb.SnapshotStorage{
			State: state, Object: &ateapipb.ObjectSnapshot{SnapshotUri: fmt.Sprintf("s3://bucket/%d", generation)},
		},
	}
}

func TestSubstrateBootRequiresGoldenSnapshotsDisabled(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate  func(*stubSubstrateControl)
		allowed bool
	}{
		"golden disabled": {mutate: func(*stubSubstrateControl) {}, allowed: true},
		"golden enabled": {mutate: func(s *stubSubstrateControl) {
			s.template = goldenTemplate(ateapipb.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_ENABLED)
		}},
		"golden default enabled": {mutate: func(s *stubSubstrateControl) { s.template.SnapshotConfig = nil }},
		"seeded from a tag": {mutate: func(s *stubSubstrateControl) {
			s.actor.SourceTag = &ateapipb.ObjectRef{Atespace: "team-a", Name: "checkpoint"}
		}},
		"has guest state": {mutate: func(s *stubSubstrateControl) {
			s.actor.Status.Snapshots = []*ateapipb.Snapshot{snapshot(1, ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES, ateapipb.SnapshotStorageState_SNAPSHOT_STORAGE_STATE_COMPLETED)}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			stub := &stubSubstrateControl{actor: freshActor(), template: goldenTemplate(ateapipb.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_DISABLED)}
			tc.mutate(stub)
			client := &grpcSubstrateControlClient{client: stub, atespace: "team-a"}
			_, err := client.ResumeActor(t.Context(), "actor-1", true)
			if tc.allowed {
				require.NoError(t, err)
				require.Equal(t, 1, stub.resumes)
				return
			}
			require.True(t, IsKind(err, ErrorKindFailedPrecondition), "error = %v", err)
			require.Zero(t, stub.resumes, "a boot request must fail before resuming from a golden or prior snapshot")
		})
	}
}

func TestSubstrateResumeWithoutBootSkipsColdBootCheck(t *testing.T) {
	stub := &stubSubstrateControl{actor: freshActor(), template: goldenTemplate(ateapipb.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_ENABLED)}
	client := &grpcSubstrateControlClient{client: stub, atespace: "team-a"}
	_, err := client.ResumeActor(t.Context(), "actor-1", false)
	require.NoError(t, err)
	require.Equal(t, 1, stub.resumes)
}

func TestSubstrateActorSnapshotMapping(t *testing.T) {
	actor := freshActor()
	actor.Status.Snapshots = []*ateapipb.Snapshot{
		snapshot(2, ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES, ateapipb.SnapshotStorageState_SNAPSHOT_STORAGE_STATE_COMPLETED),
		snapshot(1, ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY, ateapipb.SnapshotStorageState_SNAPSHOT_STORAGE_STATE_COMPLETED),
	}
	converted := substrateActorFromProto(actor)
	require.Equal(t, SubstrateSnapshotContentScopeData, converted.SnapshotScope, "the newest generation decides the scope")
	require.Equal(t, "template-uid-2", converted.TemplateUID)
	require.Equal(t, "s3://bucket/2", converted.LastSnapshot)
	require.Empty(t, converted.InProgressSnapshot)

	actor.Status.Snapshots = append(actor.Status.Snapshots,
		snapshot(3, ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_ROOTFS, ateapipb.SnapshotStorageState_SNAPSHOT_STORAGE_STATE_PENDING))
	converted = substrateActorFromProto(actor)
	require.Empty(t, converted.SnapshotScope, "ROOTFS is not data-only and must stay unclassified")
	require.Equal(t, "snapshot-3", converted.InProgressSnapshot)
	require.Empty(t, converted.LastSnapshot)

	actor.Status.Snapshots[2].Fidelity = ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY
	require.Equal(t, SubstrateSnapshotContentScopeFull, substrateActorFromProto(actor).SnapshotScope)
}

func TestSubstrateBootedActorTemplateUIDComesFromImmutableTemplate(t *testing.T) {
	actor := freshActor()
	actor.Status.State = ateapipb.ActorState_ACTOR_STATE_RUNNING
	actor.Status.WorkerAssignment = &ateapipb.WorkerAssignment{WorkerPod: "worker-0", WorkerPodUid: "pod-uid", WorkerPodIps: []string{"", "10.0.0.7"}}
	template := goldenTemplate(ateapipb.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_DISABLED)
	templateCreated := time.Unix(1_800_000_000, 0)
	template.Metadata.CreateTime = timestamppb.New(templateCreated)
	actor.Metadata.CreateTime = timestamppb.New(templateCreated.Add(time.Minute))
	stub := &stubSubstrateControl{actor: actor, template: template}
	client := &grpcSubstrateControlClient{client: stub, atespace: "team-a"}
	observed, err := client.GetActor(t.Context(), "actor-1")
	require.NoError(t, err)
	require.Equal(t, "template-uid", observed.TemplateUID, "a template older than the Actor is the one it booted from")
	require.Equal(t, "10.0.0.7", observed.PodIP)

	// The template was deleted and created again under the same name after the
	// Actor was created. The Actor may still run the deleted one, so the
	// replacement's UID is not its identity.
	replacement := proto.CloneOf(template)
	replacement.Metadata.Uid = "replacement-template-uid"
	replacement.Metadata.CreateTime = timestamppb.New(templateCreated.Add(time.Hour))
	stub.template = replacement
	observed, err = client.GetActor(t.Context(), "actor-1")
	require.NoError(t, err)
	require.Empty(t, observed.TemplateUID, "a template newer than the Actor proves nothing about what it runs")
	executor := &SubstrateWorkspaceExecutor{control: client, now: time.Now}
	_, err = executor.Claim(t.Context(), ClaimRequest{
		ClaimName: "actor-1", Template: TemplateRef{Namespace: "team-a", Name: "mcp", UID: "replacement-template-uid"},
	})
	require.True(t, IsKind(err, ErrorKindFailedPrecondition), "Claim must not reattach a workload to a template it may not run: %v", err)
	_, err = executor.WaitReady(t.Context(), WaitReadyRequest{
		Ref: WorkspaceRef{ID: "actor-1"}, Template: TemplateRef{Namespace: "team-a", Name: "mcp", UID: "replacement-template-uid"},
	})
	require.True(t, IsKind(err, ErrorKindFailedPrecondition), "WaitReady must not accept it either: %v", err)
	require.Zero(t, stub.resumes, "nothing is resumed against an unproven template")

	// Without both times nothing can be proven.
	for name, clear := range map[string]func(){
		"template": func() { stub.template.Metadata.CreateTime = nil },
		"actor":    func() { stub.actor.Metadata.CreateTime = nil },
	} {
		stub.template = proto.CloneOf(template)
		stub.actor = proto.CloneOf(actor)
		clear()
		observed, err = client.GetActor(t.Context(), "actor-1")
		require.NoError(t, err)
		require.Empty(t, observed.TemplateUID, "no %s creation time", name)
	}
	stub.template, stub.actor = template, actor

	// A never-placed Actor has no guest state and keeps an empty identity.
	stub.actor = freshActor()
	observed, err = client.GetActor(t.Context(), "actor-1")
	require.NoError(t, err)
	require.Empty(t, observed.TemplateUID)
}

func TestSubstrateDeleteAndSuspendUseNativeFences(t *testing.T) {
	stub := &stubSubstrateControl{actor: freshActor()}
	client := &grpcSubstrateControlClient{client: stub, atespace: "team-a"}
	require.NoError(t, client.DeleteActor(t.Context(), "actor-1", " actor-uid "))
	require.Len(t, stub.deletes, 1)
	require.Equal(t, "actor-uid", stub.deletes[0].GetOptions().GetUid())
	_, err := client.SuspendActor(t.Context(), "actor-1")
	require.NoError(t, err)
	require.Equal(t, ateapipb.DurabilityMode_DURABILITY_MODE_SYNCHRONOUS, stub.suspends[0].GetOptions().GetMode())
}

func TestSubstrateWorkerFitsExactlyOne(t *testing.T) {
	limits := &ateapipb.Resources{CpuMillicores: 1000, MemoryBytes: 1 << 30}
	worker := func(capacity *ateapipb.WorkerResources, overheads ...*ateapipb.RuntimeOverhead) *ateapipb.WorkerStatus {
		reported := &ateapipb.WorkerStatus{Capacity: capacity}
		if len(overheads) > 0 {
			class := &ateapipb.WorkerSandboxClass{SandboxClass: "gvisor", DefaultRuntime: &ateapipb.WorkerSandboxRuntime{ActorOverhead: overheads[0]}}
			for _, overhead := range overheads[1:] {
				class.RestorableRuntimes = append(class.RestorableRuntimes, &ateapipb.WorkerSandboxRuntime{ActorOverhead: overhead})
			}
			reported.SandboxClasses = []*ateapipb.WorkerSandboxClass{class}
		}
		return reported
	}
	fixed := &ateapipb.RuntimeOverhead{MemoryBytes: 64 << 20}
	// 32MiB for each GiB of the limit: 32MiB for this template.
	scaled := &ateapipb.RuntimeOverhead{MemoryBytesPerGib: 32 << 20}
	for name, tc := range map[string]struct {
		worker *ateapipb.WorkerStatus
		want   bool
	}{
		"sized for one":        {worker: worker(&ateapipb.WorkerResources{Actors: 1000, CpuMillicores: 1000, MemoryBytes: 1 << 30}), want: true},
		"memory bounds to one": {worker: worker(&ateapipb.WorkerResources{Actors: 1000, CpuMillicores: 4000, MemoryBytes: (1 << 30) + (64 << 20)}), want: true},
		"actor count is one":   {worker: worker(&ateapipb.WorkerResources{Actors: 1, CpuMillicores: 8000, MemoryBytes: 8 << 30}), want: true},
		"fits two":             {worker: worker(&ateapipb.WorkerResources{Actors: 1000, CpuMillicores: 2000, MemoryBytes: 2 << 30})},
		"too small":            {worker: worker(&ateapipb.WorkerResources{Actors: 1000, CpuMillicores: 500, MemoryBytes: 1 << 30})},
		"no actors":            {worker: worker(&ateapipb.WorkerResources{CpuMillicores: 1000, MemoryBytes: 1 << 30})},

		// Placement books the runtime's overhead with every Actor.
		"overhead leaves no room for one": {worker: worker(&ateapipb.WorkerResources{Actors: 1000, CpuMillicores: 1000, MemoryBytes: 1 << 30}, fixed)},
		"overhead makes one fit exactly":  {worker: worker(&ateapipb.WorkerResources{Actors: 1000, CpuMillicores: 1000, MemoryBytes: (1 << 30) + (64 << 20)}, fixed), want: true},
		"overhead keeps a second out":     {worker: worker(&ateapipb.WorkerResources{Actors: 1000, CpuMillicores: 4000, MemoryBytes: 2 << 30}, fixed), want: true},
		"two fit with their overhead":     {worker: worker(&ateapipb.WorkerResources{Actors: 1000, CpuMillicores: 4000, MemoryBytes: (2 << 30) + (128 << 20)}, fixed)},
		"per-GiB overhead leaves no room": {worker: worker(&ateapipb.WorkerResources{Actors: 1000, CpuMillicores: 1000, MemoryBytes: (1 << 30) + (32 << 20) - 1}, scaled)},
		"per-GiB overhead fits exactly":   {worker: worker(&ateapipb.WorkerResources{Actors: 1000, CpuMillicores: 1000, MemoryBytes: (1 << 30) + (32 << 20)}, scaled), want: true},
		"per-GiB overhead keeps two out":  {worker: worker(&ateapipb.WorkerResources{Actors: 1000, CpuMillicores: 4000, MemoryBytes: (2 << 30) + (64 << 20) - 1}, scaled), want: true},
		// An Actor restored from a snapshot runs under the matching restorable
		// runtime, so the worker must hold exactly one under that one too.
		"restorable runtime leaves no room": {worker: worker(&ateapipb.WorkerResources{Actors: 1000, CpuMillicores: 1000, MemoryBytes: 1 << 30}, nil, fixed)},
		"lighter restorable admits two":     {worker: worker(&ateapipb.WorkerResources{Actors: 1000, CpuMillicores: 4000, MemoryBytes: 2 << 30}, fixed, nil)},
		"overflowing overhead":              {worker: worker(&ateapipb.WorkerResources{Actors: 1, CpuMillicores: 1000, MemoryBytes: math.MaxInt64}, &ateapipb.RuntimeOverhead{MemoryBytes: math.MaxInt64})},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, substrateWorkerFitsExactlyOne(tc.worker, limits))
		})
	}
	require.False(t, substrateWorkerFitsExactlyOne(worker(&ateapipb.WorkerResources{Actors: 1, CpuMillicores: 1000, MemoryBytes: 1 << 30}), nil),
		"a template without limits cannot be bounded")

	// Half a GiB under a 1GiB-per-GiB rate adds half a GiB, rounded down to a byte.
	booking, ok := substrateActorMemoryBooking((1<<29)+1, &ateapipb.RuntimeOverhead{MemoryBytes: 7, MemoryBytesPerGib: 1 << 30})
	require.True(t, ok)
	require.EqualValues(t, (1<<29)+1+7+(1<<29)+1, booking)
	booking, ok = substrateActorMemoryBooking(3, &ateapipb.RuntimeOverhead{MemoryBytesPerGib: 1 << 29})
	require.True(t, ok)
	require.EqualValues(t, 3+1, booking, "1.5 bytes of overhead rounds down")
	_, ok = substrateActorMemoryBooking(math.MaxInt64, &ateapipb.RuntimeOverhead{MemoryBytesPerGib: 1 << 30})
	require.False(t, ok, "the limit and its overhead do not fit an int64")
	_, ok = substrateActorMemoryBooking(1<<40, &ateapipb.RuntimeOverhead{MemoryBytesPerGib: math.MaxInt64})
	require.False(t, ok, "the scaled overhead alone does not fit an int64")
	_, ok = substrateActorMemoryBooking(1<<47, &ateapipb.RuntimeOverhead{MemoryBytesPerGib: 1 << 47})
	require.False(t, ok, "a scaled overhead of exactly 1<<64 bytes must not wrap to zero")
	_, ok = substrateActorMemoryBooking(1<<30, &ateapipb.RuntimeOverhead{MemoryBytes: -1})
	require.False(t, ok, "a negative overhead is not a booking")
}

func TestSubstrateWorkerRunsGVisor(t *testing.T) {
	gvisor := &ateapipb.WorkerSandboxClass{SandboxClass: "gvisor"}
	require.True(t, substrateWorkerRunsGVisor([]*ateapipb.WorkerSandboxClass{gvisor}))
	require.False(t, substrateWorkerRunsGVisor(nil), "an unregistered worker reports no sandbox runtime")
	require.False(t, substrateWorkerRunsGVisor([]*ateapipb.WorkerSandboxClass{gvisor, {SandboxClass: "microvm"}}))
}

// An Actor placed on a worker but never snapshotted takes its template identity
// from a template lookup, and upstream lets a template be deleted while Actors
// still name it. A missing template must not read as a missing Actor: Delete
// would report a live Actor already deleted and Claim would try to create it.
func TestSubstrateMissingTemplateIsNotAMissingActor(t *testing.T) {
	placed := freshActor()
	placed.Status.State = ateapipb.ActorState_ACTOR_STATE_RUNNING
	placed.Status.WorkerAssignment = &ateapipb.WorkerAssignment{}
	stub := &stubSubstrateControl{actor: placed, templateErr: status.Error(codes.NotFound, "ActorTemplate team-a/mcp not found")}
	client := &grpcSubstrateControlClient{client: stub, atespace: "team-a"}
	executor := &SubstrateWorkspaceExecutor{control: client, now: time.Now}
	ref := WorkspaceRef{ID: "actor-1"}

	actor, err := client.GetActor(t.Context(), "actor-1")
	require.NoError(t, err)
	require.Empty(t, actor.TemplateUID, "a missing template leaves the Actor's identity unknown")
	require.Equal(t, substrateStatusRunning, actor.Status)

	// The identity is unknown, so a caller that pins a template UID is refused
	// rather than reattached or handed a new Actor.
	_, err = executor.Claim(t.Context(), ClaimRequest{
		ClaimName: "actor-1", CreateIfMissing: true,
		Template: TemplateRef{Namespace: "team-a", Name: "mcp", UID: "template-uid"},
	})
	require.True(t, IsKind(err, ErrorKindFailedPrecondition), "error = %v", err)

	// Delete is fenced by the Actor UID and must still remove the Actor.
	result, err := executor.Delete(t.Context(), DeleteRequest{Ref: ref, SkipScrub: true})
	require.NoError(t, err)
	require.True(t, result.Deleted)
	require.Len(t, stub.deletes, 1)
}
