package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	ateapipb "github.com/orka-agents/orka/internal/substratepb"
	"github.com/orka-agents/orka/internal/workspace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// This fixture implements only current upstream RPCs. It has no synthetic
// atomic Suspend/Resume receipts or fork-only ActorSnapshot methods.
type nativeRuntimeTestAPI struct {
	*nativeTemplateTestAPI
	kube                                         client.Client
	actors                                       map[string]*ateapipb.Actor
	workers                                      map[string]*ateapipb.Worker
	tags                                         map[string]*ateapipb.Tag
	data                                         map[string]string
	tagData                                      map[string]string
	creates, resumes, suspends, deletes, updates int
	lostCreate, lostSuspend, lostTag             bool
	afterTag                                     func(*ateapipb.Actor)
	deleteWithLivePod                            bool
}

func (a *nativeRuntimeTestAPI) GetActor(_ context.Context, req *ateapipb.GetActorRequest, _ ...grpc.CallOption) (*ateapipb.Actor, error) {
	if actor := a.actors[req.GetActor().GetName()]; actor != nil {
		return proto.Clone(actor).(*ateapipb.Actor), nil
	}
	return nil, status.Error(codes.NotFound, "actor absent")
}

func (a *nativeRuntimeTestAPI) CreateActor(_ context.Context, req *ateapipb.CreateActorRequest, _ ...grpc.CallOption) (*ateapipb.Actor, error) {
	actor := proto.Clone(req.GetActor()).(*ateapipb.Actor)
	name := actor.GetMetadata().GetName()
	if a.actors[name] != nil {
		return nil, status.Error(codes.AlreadyExists, "actor exists")
	}
	template := a.templates[actor.GetActorTemplate().GetAtespace()+"/"+actor.GetActorTemplate().GetName()]
	if template == nil {
		return nil, status.Error(codes.NotFound, "template absent")
	}
	a.creates++
	actor.Metadata.Uid, actor.Metadata.Version = fmt.Sprintf("actor-uid-%d", a.creates), 1
	actor.Status = &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_SUSPENDED}
	if tagName := actor.GetSourceTag().GetName(); tagName != "" {
		tag := a.tags[tagName]
		if tag == nil || tag.GetStatus().GetActorTemplateUid() != template.GetMetadata().GetUid() {
			return nil, status.Error(codes.FailedPrecondition, "source template UID mismatch")
		}
		actor.Status.ExternalSnapshot = proto.Clone(tag.GetStatus().GetSnapshot()).(*ateapipb.ExternalSnapshot)
		actor.Status.CurrentActorTemplateUid = template.GetMetadata().GetUid()
		a.data[name] = a.tagData[tagName]
	}
	a.actors[name] = actor
	if a.lostCreate {
		a.lostCreate = false
		return nil, status.Error(codes.Unavailable, "create response lost")
	}
	return proto.Clone(actor).(*ateapipb.Actor), nil
}

func (a *nativeRuntimeTestAPI) UpdateActor(_ context.Context, req *ateapipb.UpdateActorRequest, _ ...grpc.CallOption) (*ateapipb.Actor, error) {
	update := req.GetActor()
	actor := a.actors[update.GetMetadata().GetName()]
	if actor == nil || actor.GetMetadata().GetUid() != update.GetMetadata().GetUid() || actor.GetMetadata().GetVersion() != update.GetMetadata().GetVersion() {
		return nil, status.Error(codes.Aborted, "UID/version conflict")
	}
	if actor.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_SUSPENDED {
		return nil, status.Error(codes.FailedPrecondition, "restore update requires suspended Actor")
	}
	a.updates++
	actor.ActorTemplate = proto.Clone(update.GetActorTemplate()).(*ateapipb.ObjectRef)
	actor.Metadata.Version++
	return proto.Clone(actor).(*ateapipb.Actor), nil
}

func (a *nativeRuntimeTestAPI) ResumeActor(ctx context.Context, req *ateapipb.ResumeActorRequest, _ ...grpc.CallOption) (*ateapipb.ResumeActorResponse, error) {
	actor := a.actors[req.GetActor().GetName()]
	if actor == nil {
		return nil, status.Error(codes.NotFound, "actor absent")
	}
	if actor.GetStatus().GetState() == ateapipb.ActorState_ACTOR_STATE_RUNNING {
		return &ateapipb.ResumeActorResponse{Actor: proto.Clone(actor).(*ateapipb.Actor)}, nil
	}
	if actor.SourceTag != nil && req.GetBoot() {
		return nil, status.Error(codes.FailedPrecondition, "boot would bypass preserved data")
	}
	a.resumes++
	workerName := fmt.Sprintf("native-worker-%d", a.resumes)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: substrateTestWorkerNamespace, Name: workerName, UID: types.UID(workerName + "-pod-uid"), Labels: map[string]string{substrateWorkerPoolLabel: substrateTestWorkerPoolName}}}
	if err := a.kube.Create(ctx, pod); err != nil {
		return nil, err
	}
	worker := &ateapipb.Worker{Metadata: &ateapipb.ResourceMetadata{Name: workerName, Uid: workerName + "-uid", Version: 1}, WorkerNamespace: pod.Namespace, WorkerPool: substrateTestWorkerPoolName, WorkerPod: pod.Name, WorkerPodUid: string(pod.UID), Ip: "10.99.0.5", Status: &ateapipb.WorkerStatus{State: ateapipb.WorkerState_WORKER_STATE_ACTIVE, Capacity: &ateapipb.WorkerResources{Actors: 1}}}
	a.workers[workerName] = worker
	actor.Metadata.Version++
	actor.Status.State = ateapipb.ActorState_ACTOR_STATE_RUNNING
	actor.Status.CurrentActorTemplateUid = a.templates[actor.GetActorTemplate().GetAtespace()+"/"+actor.GetActorTemplate().GetName()].GetMetadata().GetUid()
	actor.Status.WorkerAssignment = &ateapipb.WorkerAssignment{Worker: &ateapipb.ObjectRef{Name: workerName}, WorkerNamespace: pod.Namespace, WorkerPool: substrateTestWorkerPoolName, WorkerPod: pod.Name, WorkerPodUid: string(pod.UID), WorkerPodIp: worker.Ip}
	return &ateapipb.ResumeActorResponse{Actor: proto.Clone(actor).(*ateapipb.Actor), Resumed: true}, nil
}

func (a *nativeRuntimeTestAPI) SuspendActor(_ context.Context, req *ateapipb.SuspendActorRequest, _ ...grpc.CallOption) (*ateapipb.SuspendActorResponse, error) {
	actor := a.actors[req.GetActor().GetName()]
	if actor == nil {
		return nil, status.Error(codes.NotFound, "actor absent")
	}
	a.suspends++
	actor.Metadata.Version++
	actor.Status.State, actor.Status.WorkerAssignment = ateapipb.ActorState_ACTOR_STATE_SUSPENDED, nil
	actor.Status.ExternalSnapshot = &ateapipb.ExternalSnapshot{SnapshotUri: fmt.Sprintf("s3://test/data-%d", a.suspends), ContentScope: ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA}
	if a.lostSuspend {
		a.lostSuspend = false
		return nil, status.Error(codes.Unavailable, "suspend response lost")
	}
	return &ateapipb.SuspendActorResponse{Actor: proto.Clone(actor).(*ateapipb.Actor)}, nil
}

func (a *nativeRuntimeTestAPI) DeleteActor(ctx context.Context, req *ateapipb.DeleteActorRequest, _ ...grpc.CallOption) (*ateapipb.Actor, error) {
	actor := a.actors[req.GetActor().GetName()]
	if actor == nil {
		return nil, status.Error(codes.NotFound, "actor absent")
	}
	if !req.GetAnyState() {
		return nil, status.Error(codes.InvalidArgument, "cleanup must not checkpoint")
	}
	for _, worker := range a.workers {
		if worker.GetStatus().GetState() != ateapipb.WorkerState_WORKER_STATE_DRAINING {
			continue
		}
		if err := a.kube.Get(ctx, types.NamespacedName{Namespace: worker.GetWorkerNamespace(), Name: worker.GetWorkerPod()}, &corev1.Pod{}); err == nil {
			a.deleteWithLivePod = true
		}
	}
	a.deletes++
	delete(a.actors, actor.GetMetadata().GetName())
	delete(a.data, actor.GetMetadata().GetName())
	return proto.Clone(actor).(*ateapipb.Actor), nil
}

func (a *nativeRuntimeTestAPI) GetWorker(_ context.Context, req *ateapipb.GetWorkerRequest, _ ...grpc.CallOption) (*ateapipb.Worker, error) {
	if worker := a.workers[req.GetWorker().GetName()]; worker != nil {
		return proto.Clone(worker).(*ateapipb.Worker), nil
	}
	return nil, status.Error(codes.NotFound, "worker absent")
}

func (a *nativeRuntimeTestAPI) DrainWorker(_ context.Context, req *ateapipb.DrainWorkerRequest, _ ...grpc.CallOption) (*ateapipb.Worker, error) {
	worker := a.workers[req.GetWorker().GetName()]
	if worker == nil {
		return nil, status.Error(codes.NotFound, "worker absent")
	}
	worker.Status.State = ateapipb.WorkerState_WORKER_STATE_DRAINING
	return proto.Clone(worker).(*ateapipb.Worker), nil
}

func (a *nativeRuntimeTestAPI) ListWorkerActorAssignments(_ context.Context, req *ateapipb.ListWorkerActorAssignmentsRequest, _ ...grpc.CallOption) (*ateapipb.ListWorkerActorAssignmentsResponse, error) {
	response := &ateapipb.ListWorkerActorAssignmentsResponse{}
	for _, actor := range a.actors {
		if actor.GetStatus().GetWorkerAssignment().GetWorker().GetName() == req.GetWorker().GetName() {
			response.ActorAssignments = append(response.ActorAssignments, &ateapipb.ActorAssignment{Actor: &ateapipb.ObjectRef{Atespace: actor.GetMetadata().GetAtespace(), Name: actor.GetMetadata().GetName()}, ActorUid: actor.GetMetadata().GetUid()})
		}
	}
	return response, nil
}

func (a *nativeRuntimeTestAPI) GetTag(_ context.Context, req *ateapipb.GetTagRequest, _ ...grpc.CallOption) (*ateapipb.Tag, error) {
	if tag := a.tags[req.GetTag().GetName()]; tag != nil {
		return proto.Clone(tag).(*ateapipb.Tag), nil
	}
	return nil, status.Error(codes.NotFound, "tag absent")
}

func (a *nativeRuntimeTestAPI) CreateTag(_ context.Context, req *ateapipb.CreateTagRequest, _ ...grpc.CallOption) (*ateapipb.Tag, error) {
	tag := proto.Clone(req.GetTag()).(*ateapipb.Tag)
	if a.tags[tag.GetMetadata().GetName()] != nil {
		return nil, status.Error(codes.AlreadyExists, "tag exists")
	}
	actor := a.actors[tag.GetSourceActor().GetName()]
	if actor == nil || actor.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_SUSPENDED {
		return nil, status.Error(codes.FailedPrecondition, "source must be suspended")
	}
	tag.Metadata.Uid, tag.Metadata.Version = "tag-uid-"+tag.Metadata.Name, 1
	tag.Status = &ateapipb.TagStatus{SourceActorUid: actor.GetMetadata().GetUid(), ActorTemplateUid: actor.GetStatus().GetCurrentActorTemplateUid(), Snapshot: proto.Clone(actor.GetStatus().GetExternalSnapshot()).(*ateapipb.ExternalSnapshot)}
	tag.Status.Snapshot.SnapshotUri = "s3://test/tags/" + tag.Metadata.Name
	a.tags[tag.Metadata.Name], a.tagData[tag.Metadata.Name] = tag, a.data[actor.GetMetadata().GetName()]
	if a.afterTag != nil {
		a.afterTag(actor)
	}
	if a.lostTag {
		a.lostTag = false
		return nil, status.Error(codes.Unavailable, "tag response lost")
	}
	return proto.Clone(tag).(*ateapipb.Tag), nil
}

func (a *nativeRuntimeTestAPI) DeleteTag(_ context.Context, req *ateapipb.DeleteTagRequest, _ ...grpc.CallOption) (*ateapipb.Tag, error) {
	tag := a.tags[req.GetTag().GetName()]
	if tag == nil {
		return nil, status.Error(codes.NotFound, "tag absent")
	}
	delete(a.tags, tag.GetMetadata().GetName())
	delete(a.tagData, tag.GetMetadata().GetName())
	return tag, nil
}

type nativeRuntimeTestHarness struct {
	r          *RuntimePoolReconciler
	pool       *corev1alpha1.RuntimePool
	api        *nativeRuntimeTestAPI
	supervisor *fakeRuntimePoolSupervisorClient
	draining   map[string]bool
	seeds      []harnessv2.CredentialBootstrapRequest
}

func newNativeRuntimeTestHarness(t *testing.T) *nativeRuntimeTestHarness {
	t.Helper()
	h := &nativeRuntimeTestHarness{supervisor: &fakeRuntimePoolSupervisorClient{}, draining: map[string]bool{}}
	h.r, h.pool = runtimePoolSubstrateTestReconciler(t, h.supervisor, &fakeSubstrateActorControl{})
	h.r.SubstrateTemplates, h.r.SubstrateActorControlFactory = nil, nil
	h.r.SubstrateConfig.DirectEgressEnabled = true
	h.r.ControllerNamespace = "orka-system"
	base, err := nativeSubstrateRuntimeTemplate(nativeSubstrateTestRender(t, h.r, "public-nonce"))
	if err != nil {
		t.Fatal(err)
	}
	base.Metadata = &ateapipb.ResourceMetadata{Atespace: substrateTestTemplateNamespace, Name: substrateTestBaseTemplateName, Uid: "base-template-uid", Version: 1}
	base.Volumes = nil // the infrastructure template does not own runtime identity projections
	h.api = &nativeRuntimeTestAPI{nativeTemplateTestAPI: &nativeTemplateTestAPI{templates: map[string]*ateapipb.ActorTemplate{substrateTestTemplateNamespace + "/" + substrateTestBaseTemplateName: base}}, kube: h.r.Client, actors: map[string]*ateapipb.Actor{}, workers: map[string]*ateapipb.Worker{}, tags: map[string]*ateapipb.Tag{}, data: map[string]string{}, tagData: map[string]string{}}
	h.r.SubstrateNativeClientFactory = func(SubstrateConfig) (*workspace.SubstrateNativeClient, error) {
		return &workspace.SubstrateNativeClient{Control: h.api}, nil
	}
	h.r.SubstrateCredentialSeeder = func(_ context.Context, _, _ string, _ []byte, request harnessv2.CredentialBootstrapRequest) error {
		h.seeds = append(h.seeds, request)
		return request.Validate()
	}
	h.r.Scheme.AddKnownTypeWithName(substrateActorTemplateGVK.GroupVersion().WithKind("WorkerPool"), &unstructured.Unstructured{})
	h.r.Scheme.AddKnownTypeWithName(substrateActorTemplateGVK.GroupVersion().WithKind("WorkerPoolList"), &unstructured.UnstructuredList{})
	workerPool := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"replicas": int64(1)}}}
	workerPool.SetAPIVersion("ate.dev/v1alpha1")
	workerPool.SetKind("WorkerPool")
	workerPool.SetNamespace(substrateTestWorkerNamespace)
	workerPool.SetName(substrateTestWorkerPoolName)
	workerPool.SetLabels(map[string]string{"orka.ai/worker-pool": "test"})
	if err := h.r.Create(t.Context(), workerPool); err != nil {
		t.Fatal(err)
	}
	pool := runtimePoolTestGetPool(t, h.r, h.pool)
	pool.Spec.ExecutionWorkspace.Substrate.SuspendMode = "DataOnly"
	if err := h.r.Update(t.Context(), &pool); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *nativeRuntimeTestHarness) record(t *testing.T) *substrateNativeState {
	t.Helper()
	pool := runtimePoolTestGetPool(t, h.r, h.pool)
	_, record, err := h.r.readNativeSubstrateState(t.Context(), &pool)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func (h *nativeRuntimeTestHarness) step(t *testing.T) {
	t.Helper()
	pool := runtimePoolTestGetPool(t, h.r, h.pool)
	record := h.record(t)
	actorName := ""
	if record != nil && record.Attempt != nil {
		actorName = record.Attempt.Name
		if actor := h.api.actors[actorName]; actor != nil && actor.GetStatus().GetState() == ateapipb.ActorState_ACTOR_STATE_RUNNING {
			cfg, err := h.r.runtimePoolConfigForDrain(&pool)
			if err != nil {
				t.Fatal(err)
			}
			template, err := h.r.substrateTemplates().Get(t.Context(), record.Atespace, runtimePoolSubstrateTemplateName(cfg.baseName))
			if err != nil {
				t.Fatal(err)
			}
			validationPool, validationConfig, _, err := h.r.substrateRuntimePoolDeployedValidationTarget(t.Context(), &pool, cfg, template)
			if err != nil {
				t.Fatal(err)
			}
			route := substrateActorRouteHost(workspace.SubstrateActorKey(record.Atespace, actorName), h.r.SubstrateConfig.ActorDNSSuffix)
			pod := substrateSyntheticInstancePod(validationPool, validationConfig, nativeSubstrateRuntimeActorView(actor), actorName, route)
			h.supervisor.probe = runtimePoolValidProbe(validationPool, pod, "boot-"+actor.GetMetadata().GetUid(), h.draining[actorName])
			h.supervisor.probe.Status.Fence.ControllerEpoch = uint64(validationConfig.controllerEpoch)
		}
	}
	before := h.supervisor.drainCalls
	runtimePoolReconcile(t, h.r, h.pool)
	if h.supervisor.drainCalls > before {
		h.draining[actorName] = true
	}
}

func (h *nativeRuntimeTestHarness) until(t *testing.T, done func(*corev1alpha1.RuntimePool, *substrateNativeState) bool) {
	t.Helper()
	for range 70 {
		h.step(t)
		pool := runtimePoolTestGetPool(t, h.r, h.pool)
		if done(&pool, h.record(t)) {
			return
		}
	}
	pool := runtimePoolTestGetPool(t, h.r, h.pool)
	record := h.record(t)
	t.Fatalf("native lifecycle did not settle: lifecycle=%s message=%s journal=%+v", pool.Status.Lifecycle, pool.Status.Message, record)
}

func nativeTestServing(pool *corev1alpha1.RuntimePool, _ *substrateNativeState) bool {
	return pool.Status.Lifecycle == corev1alpha1.RuntimePoolLifecycleServing && pool.Status.AdmissionState == corev1alpha1.RuntimePoolAdmissionAccepting
}
func nativeTestSuspended(pool *corev1alpha1.RuntimePool, record *substrateNativeState) bool {
	return record != nil && record.Phase == substrateNativeSuspended && pool.Status.Lifecycle == corev1alpha1.RuntimePoolLifecycleStopped && runtimePoolWorkspaceSuspendConsentRecorded(pool)
}

func TestNativeSubstrateSuspendContinuePreservesDataAndRotatesCredentials(t *testing.T) {
	h := newNativeRuntimeTestHarness(t)
	h.api.lostCreate, h.api.lostSuspend, h.api.lostTag = true, true, true
	h.until(t, nativeTestServing)
	first := h.record(t)
	h.api.data[first.Attempt.Name] = "committed workspace change"
	substrateSuspendTestPoolIntent(t, h.r, h.pool, true)
	h.until(t, nativeTestSuspended)
	suspended := h.record(t)
	if len(h.api.actors) != 0 || len(h.api.tags) != 1 || h.api.tagData[suspended.Checkpoint.Name] != "committed workspace change" || h.api.deleteWithLivePod || h.api.suspends != 1 {
		t.Fatal("suspension lost data, replayed a mutation, or completed before workload absence")
	}
	if err := h.r.Get(t.Context(), types.NamespacedName{Namespace: first.Attempt.Worker.Namespace, Name: first.Attempt.Worker.Pod}, &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatal("suspended runtime kept its worker Pod")
	}
	// Reconstruct the reconciler while preserving only Kubernetes/provider data.
	old := h.r
	h.r = &RuntimePoolReconciler{
		Client: old.Client, APIReader: old.APIReader, Scheme: old.Scheme,
		RuntimeNamespace: old.RuntimeNamespace, ControllerNamespace: old.ControllerNamespace,
		ControllerAPIURL: old.ControllerAPIURL, ControllerAPIPort: old.ControllerAPIPort,
		ControllerEpoch: old.ControllerEpoch, AllowedImages: old.AllowedImages,
		WorkspaceArtifactMaxBytes: old.WorkspaceArtifactMaxBytes,
		ProviderProxy:             old.ProviderProxy, SubstrateEnabled: old.SubstrateEnabled,
		SubstrateConfig: old.SubstrateConfig, SubstrateNativeClientFactory: old.SubstrateNativeClientFactory,
		SubstrateCredentialSeeder: old.SubstrateCredentialSeeder, SupervisorClient: old.SupervisorClient,
		Rand: old.Rand, Now: old.Now,
	}
	substrateSuspendTestPoolIntent(t, h.r, h.pool, false)
	h.until(t, nativeTestServing)
	resumed := h.record(t)
	if resumed.Attempt.UID == first.Attempt.UID || resumed.Attempt.Worker.PodUID == first.Attempt.Worker.PodUID || resumed.Attempt.BootID == first.Attempt.BootID || h.api.data[resumed.Attempt.Name] != "committed workspace change" || h.api.updates != 1 {
		t.Fatal("continuation did not cold-boot preserved data under fresh runtime identities")
	}
	if len(h.seeds) != 2 || h.seeds[0].ControllerToken == h.seeds[1].ControllerToken || h.seeds[0].CapabilitySecret == h.seeds[1].CapabilitySecret {
		t.Fatal("continuation reused prior boot credentials")
	}
}

func TestNativeSubstrateTagSourceRacePreservesDataWithoutConsent(t *testing.T) {
	h := newNativeRuntimeTestHarness(t)
	h.until(t, nativeTestServing)
	h.api.afterTag = func(actor *ateapipb.Actor) { actor.Metadata.Version++ }
	substrateSuspendTestPoolIntent(t, h.r, h.pool, true)
	h.until(t, func(_ *corev1alpha1.RuntimePool, record *substrateNativeState) bool {
		return record != nil && record.Phase == substrateNativeFailed
	})
	pool := runtimePoolTestGetPool(t, h.r, h.pool)
	if runtimePoolWorkspaceSuspendConsentRecorded(&pool) || len(h.api.tags) != 1 || len(h.api.actors) != 1 || h.api.deletes != 0 {
		t.Fatal("source mutation was admitted or destroyed checkpoint recovery data")
	}
}

func TestNativeSubstrateActorReplacementIsNeverMutated(t *testing.T) {
	h := newNativeRuntimeTestHarness(t)
	h.until(t, nativeTestServing)
	record := h.record(t)
	h.api.actors[record.Attempt.Name].Metadata.Uid = "foreign-uid"
	beforeResume, beforeSuspend, beforeDelete := h.api.resumes, h.api.suspends, h.api.deletes
	substrateSuspendTestPoolIntent(t, h.r, h.pool, true)
	for range 3 {
		h.step(t)
	}
	pool := runtimePoolTestGetPool(t, h.r, h.pool)
	if pool.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed || h.api.resumes != beforeResume || h.api.suspends != beforeSuspend || h.api.deletes != beforeDelete {
		t.Fatal("replacement native Actor was admitted or mutated")
	}
}

func TestNativeSubstrateJournalLossBlocksProvisioningAndCleanup(t *testing.T) {
	for _, phase := range []string{"starting", "serving", "suspended"} {
		for _, action := range []string{"continue", "stop", "delete"} {
			t.Run(phase+"/"+action, func(t *testing.T) {
				h := newNativeRuntimeTestHarness(t)
				if phase == "starting" {
					h.until(t, func(pool *corev1alpha1.RuntimePool, record *substrateNativeState) bool {
						return pool.Annotations[substrateNativeJournalAnnotation] == substrateNativeJournalRequired && record.Attempt == nil
					})
					if h.api.creates != 0 {
						t.Fatal("provider operation preceded the journal recovery barrier")
					}
				} else {
					h.until(t, nativeTestServing)
					if phase == "suspended" {
						substrateSuspendTestPoolIntent(t, h.r, h.pool, true)
						h.until(t, nativeTestSuspended)
					}
				}
				record := h.record(t)
				pool := runtimePoolTestGetPool(t, h.r, h.pool)
				cm := h.r.substrateNativeStateObject(&pool)
				if err := h.r.Delete(t.Context(), cm); err != nil {
					t.Fatal(err)
				}
				if action == "delete" {
					if err := h.r.Delete(t.Context(), &pool); err != nil {
						t.Fatal(err)
					}
				} else {
					pool.Spec.DesiredReplicas = 0
					if action == "continue" {
						pool.Spec.DesiredReplicas = 1
					}
					if err := h.r.Update(t.Context(), &pool); err != nil {
						t.Fatal(err)
					}
				}
				assertNativeSubstrateMissingJournalBlocked(t, h, phase == "suspended")
				if record.Attempt != nil && record.Attempt.Worker != nil {
					fence := record.Attempt.Worker
					if err := h.r.Get(t.Context(), types.NamespacedName{Namespace: fence.Namespace, Name: fence.Pod}, &corev1.Pod{}); err != nil {
						t.Fatalf("journal loss deleted the unproven workload: %v", err)
					}
				}
			})
		}
	}
}

func TestNativeSubstrateMissingJournalRejectsLegacyLifecycle(t *testing.T) {
	for _, evidence := range []string{
		"active-instance", substrateActorBootedAnnotation, substrateActorTemplateFenceAnnotation,
		substrateActorWorkerPlacementAnnotation, substrateActorWorkerPodFenceAnnotation,
		substrateActorSuspendAcceptedAnnotation, substrateActorSnapshotDigestAnnotation,
		substrateNativeCheckpointConsent, substrateNativeDataProtection,
	} {
		t.Run(evidence, func(t *testing.T) {
			h := newNativeRuntimeTestHarness(t)
			pool := runtimePoolTestGetPool(t, h.r, h.pool)
			if evidence == "active-instance" {
				pool.Status.ActiveInstance = &corev1alpha1.RuntimePoolActiveInstanceStatus{PodUID: "prior-runtime"}
				if err := h.r.Status().Update(t.Context(), &pool); err != nil {
					t.Fatal(err)
				}
			} else {
				if pool.Annotations == nil {
					pool.Annotations = map[string]string{}
				}
				pool.Annotations[evidence] = "prior-lifecycle"
				if err := h.r.Update(t.Context(), &pool); err != nil {
					t.Fatal(err)
				}
			}
			assertNativeSubstrateMissingJournalBlocked(t, h, false)
			// Status no longer carries the only evidence in the active-instance
			// case. The durable rejection must still survive later deletion.
			pool = runtimePoolTestGetPool(t, h.r, h.pool)
			if pool.Status.ActiveInstance != nil || pool.Annotations[substrateNativeJournalAnnotation] != substrateNativeJournalRequired {
				t.Fatal("missing-journal failure did not preserve the recovery requirement")
			}
			if err := h.r.Delete(t.Context(), &pool); err != nil {
				t.Fatal(err)
			}
			assertNativeSubstrateMissingJournalBlocked(t, h, false)
		})
	}
}

func assertNativeSubstrateMissingJournalBlocked(t *testing.T, h *nativeRuntimeTestHarness, previouslyStopped bool) {
	t.Helper()
	h.r.SubstrateNativeClientFactory = func(SubstrateConfig) (*workspace.SubstrateNativeClient, error) {
		t.Fatal("missing lifecycle journal reached the native provider")
		return nil, nil
	}
	for range 3 {
		_, err := h.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(h.pool)})
		if err != nil && !errors.Is(err, errNativeSubstrateJournalMissing) {
			t.Fatal(err)
		}
		pool := runtimePoolTestGetPool(t, h.r, h.pool)
		if pool.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed || !slices.Contains(pool.Finalizers, runtimePoolFinalizer) {
			t.Fatal("journal loss admitted work or completed unproven cleanup")
		}
		if !previouslyStopped && pool.Status.Lifecycle == corev1alpha1.RuntimePoolLifecycleStopped {
			t.Fatal("journal loss falsely reported the runtime stopped")
		}
		if err := h.r.Get(t.Context(), client.ObjectKeyFromObject(h.r.substrateNativeStateObject(&pool)), &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
			t.Fatalf("missing journal was recreated: %v", err)
		}
	}
}

func TestNativeSubstrateMissingJournalUsesUncachedPoolMarker(t *testing.T) {
	h := newNativeRuntimeTestHarness(t)
	h.step(t) // create the journal without invoking the provider
	stale := runtimePoolTestGetPool(t, h.r, h.pool)
	h.step(t) // commit the recovery marker
	if err := h.r.Delete(t.Context(), h.r.substrateNativeStateObject(&stale)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.r.readNativeSubstrateState(t.Context(), &stale); !errors.Is(err, errNativeSubstrateJournalMissing) {
		t.Fatalf("a stale pool read bypassed the journal requirement: %v", err)
	}
}

func TestNativeSubstrateFreshStoppedPoolNeedsNoJournal(t *testing.T) {
	h := newNativeRuntimeTestHarness(t)
	pool := runtimePoolTestGetPool(t, h.r, h.pool)
	pool.Spec.DesiredReplicas = 0
	if err := h.r.Update(t.Context(), &pool); err != nil {
		t.Fatal(err)
	}
	h.step(t)
	pool = runtimePoolTestGetPool(t, h.r, h.pool)
	if pool.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleStopped || h.record(t) != nil || h.api.creates != 0 {
		t.Fatal("fresh zero-replica pool unnecessarily acquired a native runtime")
	}
	nativeDeletePool(t, h, &pool)
}

func TestNativeSubstrateDeletesUnstartedPoolWithoutControlCredentials(t *testing.T) {
	for _, test := range []struct {
		name  string
		steps int
	}{
		{name: "before journal"},
		{name: "unmarked journal", steps: 1},
		{name: "required journal", steps: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newNativeRuntimeTestHarness(t)
			for range test.steps {
				h.step(t)
			}
			if record := h.record(t); record != nil && record.Attempt != nil {
				t.Fatal("fixture allocated a provider attempt before deletion")
			}
			clientCalls := 0
			h.r.SubstrateNativeClientFactory = func(SubstrateConfig) (*workspace.SubstrateNativeClient, error) {
				clientCalls++
				return nil, errors.New("control credential file is unavailable")
			}
			pool := runtimePoolTestGetPool(t, h.r, h.pool)
			if !slices.Contains(pool.Finalizers, runtimePoolFinalizer) {
				pool.Finalizers = append(pool.Finalizers, runtimePoolFinalizer)
				if err := h.r.Update(t.Context(), &pool); err != nil {
					t.Fatal(err)
				}
			}
			nativeDeletePool(t, h, &pool)
			if clientCalls != 0 || h.api.creates != 0 {
				t.Fatal("empty pool cleanup unnecessarily contacted the native provider")
			}
			if err := h.r.Get(t.Context(), client.ObjectKeyFromObject(h.r.substrateNativeStateObject(&pool)), &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
				t.Fatalf("empty pool cleanup retained its journal: %v", err)
			}
		})
	}
}

func TestNativeSubstrateControlFailurePreservesActivePool(t *testing.T) {
	h := newNativeRuntimeTestHarness(t)
	h.until(t, nativeTestServing)
	attempt := h.record(t).Attempt
	controlErr := errors.New("control credential file is unavailable")
	h.r.SubstrateNativeClientFactory = func(SubstrateConfig) (*workspace.SubstrateNativeClient, error) {
		return nil, controlErr
	}
	pool := runtimePoolTestGetPool(t, h.r, h.pool)
	if err := h.r.Delete(t.Context(), &pool); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		_, err := h.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&pool)})
		if err != nil && !errors.Is(err, controlErr) {
			t.Fatal(err)
		}
	}
	pool = runtimePoolTestGetPool(t, h.r, h.pool)
	record := h.record(t)
	if record.Attempt == nil || record.Attempt.UID != attempt.UID || h.api.deletes != 0 ||
		pool.Status.Lifecycle == corev1alpha1.RuntimePoolLifecycleStopped || !slices.Contains(pool.Finalizers, runtimePoolFinalizer) {
		t.Fatal("control authentication failure discarded the existing workload or its cleanup records")
	}
	if err := h.r.Get(t.Context(), types.NamespacedName{Namespace: attempt.Worker.Namespace, Name: attempt.Worker.Pod}, &corev1.Pod{}); err != nil {
		t.Fatalf("control authentication failure removed the active worker: %v", err)
	}
}

func TestNativeSubstrateUnfencedBootBlocksCleanup(t *testing.T) {
	for _, providerState := range []string{"missing", "suspended"} {
		for _, action := range []string{"stop", "delete"} {
			t.Run(providerState+"/"+action, func(t *testing.T) {
				h := newNativeRuntimeTestHarness(t)
				h.until(t, func(_ *corev1alpha1.RuntimePool, record *substrateNativeState) bool {
					return record != nil && record.Attempt != nil && record.Attempt.BootRequested &&
						record.Attempt.Worker == nil && h.api.resumes == 1
				})
				name := h.record(t).Attempt.Name
				actor := h.api.actors[name]
				assignment := actor.GetStatus().GetWorkerAssignment()
				workerPod := types.NamespacedName{Namespace: assignment.GetWorkerNamespace(), Name: assignment.GetWorkerPod()}
				if providerState == "missing" {
					delete(h.api.actors, name)
				} else {
					actor.Status.State = ateapipb.ActorState_ACTOR_STATE_SUSPENDED
					actor.Status.WorkerAssignment = nil
				}
				pool := runtimePoolTestGetPool(t, h.r, h.pool)
				if action == "delete" {
					if err := h.r.Delete(t.Context(), &pool); err != nil {
						t.Fatal(err)
					}
				} else {
					pool.Spec.DesiredReplicas = 0
					if err := h.r.Update(t.Context(), &pool); err != nil {
						t.Fatal(err)
					}
				}
				for range 6 {
					h.step(t)
					pool = runtimePoolTestGetPool(t, h.r, h.pool)
					record := h.record(t)
					if record == nil || record.Attempt == nil || record.Attempt.Name != name || record.Attempt.WorkloadAbsent {
						t.Fatal("cleanup discarded an unfenced boot or asserted workload absence")
					}
					if pool.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed ||
						pool.Status.Lifecycle == corev1alpha1.RuntimePoolLifecycleStopped ||
						!slices.Contains(pool.Finalizers, runtimePoolFinalizer) {
						t.Fatal("cleanup admitted work, reported stopped, or released the pool finalizer")
					}
				}
				if h.api.resumes != 1 || h.api.deletes != 0 {
					t.Fatal("cleanup replayed an uncertain boot or deleted its provider identity")
				}
				if err := h.r.Get(t.Context(), workerPod, &corev1.Pod{}); err != nil {
					t.Fatalf("cleanup touched the worker without a recorded fence: %v", err)
				}
			})
		}
	}
}
