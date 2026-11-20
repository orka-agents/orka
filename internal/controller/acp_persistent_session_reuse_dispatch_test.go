/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	storekube "github.com/orka-agents/orka/internal/store/kube"
	"github.com/orka-agents/orka/internal/store/sqlite"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

const (
	reuseDispatchSession      = "reuse-dispatch-session"
	reuseDispatchSessionUID   = "reuse-dispatch-session-uid"
	reuseDispatchWorkspace    = "reuse-dispatch-workspace"
	reuseDispatchWorkspaceUID = "reuse-dispatch-workspace-uid"
)

// persistentReuseDispatchFixture runs Claude Session turns with the flag on
// through reserveTask and executeReservedTask, against a class-backed
// Session workspace on its dedicated single-session RuntimePool, the
// Kubernetes-authoritative composite control store, and a fake supervisor.
type persistentReuseDispatchFixture struct {
	kube         client.WithWatch
	scheme       *runtime.Scheme
	controlStore *storekube.Store
	sqlite       *sealedTestStore
	epochs       *ControllerEpochManager
	dispatcher   *ACPDispatcher
	runtime      *warmClaudeRuntime
	agent        *corev1alpha1.Agent
	images       ACPRuntimeImages
	plan         ACPRuntimePlan
	pool         *corev1alpha1.RuntimePool
	instance     harnessv2.RuntimeInstanceID
	// probeOnDelete models the RuntimePool controller re-probing the
	// supervisor inside the deletion call; the live controller publishes the
	// freed slot only on its next probe.
	probeOnDelete atomic.Bool
	// quiescent is what the supervisor reports for its resident session.
	quiescent atomic.Bool
}

func reuseDispatchTask(name string, uid types.UID) *corev1alpha1.Task {
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name, UID: uid, Labels: map[string]string{}},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent, Prompt: "question from " + name, AgentRuntime: &corev1alpha1.AgentRuntimeSpec{},
			AgentRef:   &corev1alpha1.AgentReference{Name: "agent"},
			SessionRef: &corev1alpha1.SessionReference{Name: reuseDispatchSession, Create: true, Append: true},
			Execution: &corev1alpha1.ExecutionSpec{Workspace: &corev1alpha1.ExecutionWorkspaceSpec{
				ClassRef: &corev1alpha1.WorkspaceClassReference{Name: "acp-class"}, ReusePolicy: corev1alpha1.WorkspaceReusePolicySession,
			}},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending, Attempts: 1, Execution: &corev1alpha1.TaskExecutionStatus{
			State: corev1alpha1.TaskExecutionStateQueued, Attempt: 1, PromptID: "prompt-" + string(uid) + "-1",
			RequestDigest: testControlDigestForDispatcher(name + "-request"), ControllerEpoch: 1,
		}},
	}
}

func newPersistentReuseDispatchFixture(t *testing.T, ctx context.Context) *persistentReuseDispatchFixture {
	t.Helper()
	f := &persistentReuseDispatchFixture{runtime: &warmClaudeRuntime{}}
	f.quiescent.Store(true)
	f.probeOnDelete.Store(true)
	f.scheme = runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1alpha1.AddToScheme, corev1.AddToScheme, coordinationv1.AddToScheme, workspacev1alpha1.AddToScheme} {
		require.NoError(t, add(f.scheme))
	}
	f.agent = &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "agent", UID: types.UID("agent-uid"), Generation: 1},
		Spec: corev1alpha1.AgentSpec{
			Model:   &corev1alpha1.ModelConfig{Name: acpTestModel},
			Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeClaude, ContractVersion: new(corev1alpha1.AgentRuntimeContractHarnessV2)},
		},
	}
	image := "docker.io/example/acp@sha256:" + strings.Repeat("a", 64)
	f.images = ACPRuntimeImages{Codex: image, Claude: image}
	probe := reuseDispatchTask("probe", "00000000-0000-0000-0000-000000000000")
	plan := frozenACPDispatcherPlanForTest(t, probe, f.agent, f.images)
	binding, err := resolveTestACPWorkspaceBinding(t, probe, reuseDispatchSessionUID)
	require.NoError(t, err)
	plan, err = applyACPWorkspaceBindingToPlan(plan, binding)
	require.NoError(t, err)
	f.plan = plan
	runtimeUID := externalNativeRuntimeUID(&workspacev1alpha1.WorkloadRequest{
		Sequence: 1, Key: workspacev1alpha1.AllocationKey{WorkspaceUID: reuseDispatchWorkspaceUID},
		Runtime: &workspacev1alpha1.RuntimeWorkload{PoolBinding: workspacev1alpha1.ImmutableObjectBinding{UID: "pool-uid"}},
	})
	f.instance = harnessv2.RuntimeInstanceID(runtimePoolRuntimeInstanceID(runtimeUID, "boot-id"))
	var kube client.WithWatch
	server := newDispatcherRuntimeServerWithOptions(t, plan.Profile, plan.Digest, dispatcherRuntimeServerOptions{
		quiescent: f.quiescent.Load, supportsRebase: true, runtimeInstanceID: f.instance,
		onPrompt: func(request harnessv2.StartPromptRequest) {
			f.runtime.mu.Lock()
			f.runtime.prompts = append(f.runtime.prompts, request)
			f.runtime.mu.Unlock()
		},
		onDelete: func(request harnessv2.DeleteRuntimeSessionRequest) {
			f.runtime.mu.Lock()
			f.runtime.deletes = append(f.runtime.deletes, request)
			f.runtime.mu.Unlock()
			if f.probeOnDelete.Load() {
				f.setPool(ctx, t, kube, setRuntimePoolAccepting)
			}
		},
	}, func(request harnessv2.CreateRuntimeSessionRequest) {
		f.runtime.mu.Lock()
		f.runtime.creates = append(f.runtime.creates, request)
		f.runtime.mu.Unlock()
	})
	t.Cleanup(server.Close)
	parsed, err := url.Parse(server.URL)
	require.NoError(t, err)
	f.pool = &corev1alpha1.RuntimePool{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: plan.PoolName, UID: types.UID("pool-uid"), Generation: 1,
			Labels:      map[string]string{acpExecutionWorkspaceLinkLabel: reuseDispatchWorkspace},
			Annotations: map[string]string{acpExecutionWorkspaceUIDAnnotation: reuseDispatchWorkspaceUID, externalRuntimeServedAnnotation: "served-record"},
		},
		Spec: corev1alpha1.RuntimePoolSpec{
			RuntimeNamespace: "orka-runtimes", Runtime: corev1alpha1.RuntimePoolRuntimeSpec{Image: plan.Image, Profile: RuntimePoolProfileFromPlan(plan)},
			DesiredReplicas: 1, ExecutionWorkspace: testExternalPoolWorkspaceSpec(plan.Workspace, reuseDispatchWorkspace, reuseDispatchWorkspaceUID),
		},
		Status: corev1alpha1.RuntimePoolStatus{
			ActiveInstance: &corev1alpha1.RuntimePoolActiveInstanceStatus{
				PodNamespace: "orka-runtimes", PodName: "runtime-pod", PodAddress: parsed.Hostname(),
				PodUID: string(runtimeUID), BootID: "boot-id", RuntimeInstanceID: string(f.instance), ControllerEpoch: 1,
				ProtocolVersion: corev1alpha1.RuntimePoolProtocolHarnessV2, ProfileDigest: string(plan.Digest),
				ProfileDigestSchemaVersion: strconv.FormatUint(uint64(harnessv2.ProfileDigestSchemaVersion), 10),
			},
		},
	}
	setRuntimePoolAccepting(f.pool)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: "orka-runtimes", Name: runtimePoolChildName(runtimePoolResourceName(f.pool.Namespace, f.pool.Name), "auth-e1-"+strings.Repeat("a", 24)),
		UID: "workspace-auth-uid", Labels: map[string]string{
			runtimePoolManagedByLabel: runtimePoolManagedByLabelValue, runtimePoolApplicationLabel: runtimePoolApplicationLabelValue,
			runtimePoolKeyLabel: runtimePoolKey(f.pool.Namespace, f.pool.Name), runtimePoolNameLabel: f.pool.Name,
			runtimePoolNamespaceLabel: f.pool.Namespace, runtimePoolUIDLabel: string(f.pool.UID),
			runtimePoolNetworkRoleLabel: "provider-client", runtimePoolAuthLabel: booleanTrueValue, runtimePoolCredentialEpochLabel: "1",
		},
	}, Immutable: new(true), Data: map[string][]byte{
		runtimePoolControllerTokenKey: []byte(strings.Repeat("t", 32)), runtimePoolCapabilitySecretKey: []byte(strings.Repeat("s", 32)),
	}}
	f.pool.Annotations[runtimePoolPrivateAuthSecretBindingAnnotation(1)] = secret.Name + "/" + string(secret.UID)
	bootstrapPort, err := strconv.ParseInt(parsed.Port(), 10, 32)
	require.NoError(t, err)
	workspace := testAdmittedNativeDispatchWorkspace(t, plan.Workspace, f.pool, server.URL, int32(bootstrapPort))
	workspace.Spec.SessionRef = &workspacev1alpha1.ObjectIdentityReference{Name: reuseDispatchSession, UID: reuseDispatchSessionUID}
	workspace.Annotations[acpWorkspaceSuspendAfterDetachAnnotation] = "5m"
	workspace.Annotations[acpWorkspaceDetachActionAnnotation] = string(workspacev1alpha1.WorkspaceOnDetachSuspend)
	workspace.Annotations[acpWorkspaceDurableSessionCommittedAnnotation] = "1"
	worker := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "orka-runtimes", Name: "deadline-worker", UID: "deadline-worker-uid"}}
	kube = withControllerEpochLeaseUIDs(t, fake.NewClientBuilder().WithScheme(f.scheme).
		WithStatusSubresource(&corev1alpha1.Task{}, &corev1alpha1.ControllerEpoch{}, &corev1alpha1.PromptAttempt{},
			&corev1alpha1.RuntimeSessionControl{}, &corev1alpha1.RuntimePool{}, &workspacev1alpha1.ExecutionWorkspace{}).
		WithObjects(f.pool, secret, f.agent, workspace, worker,
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", UID: "default-namespace-uid"}}).Build())
	f.kube = kube
	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "reuse-dispatch.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	f.sqlite = sealedStore(t, sqlite.NewStore(db, "reuse-dispatch-test"))
	f.controlStore, err = storekube.NewComposite(kube, "default", f.sqlite, storekube.WithAPIReader(kube))
	require.NoError(t, err)
	f.epochs = NewControllerEpochManager(f.controlStore, "reuse-dispatch-controller").WithStorage(storagev1.StateContract, f.sqlite)
	epochCtx, cancelEpochs := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.epochs.Start(epochCtx) }()
	t.Cleanup(func() {
		cancelEpochs()
		<-done
	})
	_, err = f.epochs.CurrentFence(ctx)
	require.NoError(t, err)
	observer := &RuntimePoolReconciler{Client: kube, APIReader: kube}
	current := f.currentPool(ctx, t)
	require.NoError(t, observer.bindExternalRuntimeInstanceEvidence(ctx, current, workspace))
	continuity, err := NewACPSessionContinuity(ACPSessionContinuityConfig{
		SessionControls: f.controlStore, Transcripts: f.sqlite, GatewayEvents: f.sqlite,
		Publications: f.controlStore, BranchClaims: f.controlStore, Lineages: f.sqlite,
		NewSessionUID: func() (string, error) { return reuseDispatchSessionUID, nil },
	})
	require.NoError(t, err)
	f.dispatcher = &ACPDispatcher{
		Client: kube, APIReader: kube, Store: f.controlStore, ResultStore: f.sqlite, EventStore: f.sqlite, PlanStore: f.sqlite,
		Snapshots: f.sqlite, Epochs: f.epochs, Sessions: continuity, PersistentSessionEnabled: true,
	}
	return f
}

func (f *persistentReuseDispatchFixture) currentPool(ctx context.Context, t *testing.T) *corev1alpha1.RuntimePool {
	t.Helper()
	pool := &corev1alpha1.RuntimePool{}
	require.NoError(t, f.kube.Get(ctx, client.ObjectKeyFromObject(f.pool), pool))
	return pool
}

func (f *persistentReuseDispatchFixture) setPool(ctx context.Context, t *testing.T, kube client.Client, mutate func(*corev1alpha1.RuntimePool)) {
	t.Helper()
	pool := &corev1alpha1.RuntimePool{}
	require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(f.pool), pool))
	mutate(pool)
	require.NoError(t, kube.Status().Update(ctx, pool))
}

// attach models the queue's warm attach of the Session workspace by task at
// epoch: the exclusive attachment and its Lease, the provider's
// acknowledgement, and, for a warm handoff, the attach evidence.
func (f *persistentReuseDispatchFixture) attach(ctx context.Context, t *testing.T, task *corev1alpha1.Task, epoch int64, lastSettled *corev1alpha1.Task) {
	t.Helper()
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	require.NoError(t, f.kube.Get(ctx, types.NamespacedName{Namespace: "default", Name: reuseDispatchWorkspace}, workspace))
	workspace.Spec.AttachmentEpoch = epoch
	workspace.Spec.Attachment = &workspacev1alpha1.ExecutionWorkspaceAttachment{
		TaskRef: workspacev1alpha1.ObjectIdentityReference{Name: task.Name, UID: task.UID}, Epoch: epoch,
		ExpiresAt: metav1.NewTime(time.Now().Add(time.Hour)),
	}
	delete(workspace.Annotations, acpWorkspaceAttachedFromWarmAnnotation)
	if lastSettled != nil {
		workspace.Annotations[acpWorkspaceAttachedFromWarmAnnotation] = formatACPWorkspaceAttachedFromWarm(
			task.UID, epoch, time.Now().Add(5*time.Minute).UTC().Format(time.RFC3339Nano), string(lastSettled.UID))
	}
	require.NoError(t, f.kube.Update(ctx, workspace))
	require.NoError(t, f.kube.Get(ctx, client.ObjectKeyFromObject(workspace), workspace))
	if workspace.Spec.CoreAdmission.AdmittedGeneration != workspace.Generation {
		// Core re-admits the attach generation.
		workspace.Spec.CoreAdmission.AdmittedGeneration = workspace.Generation
		require.NoError(t, f.kube.Update(ctx, workspace))
		require.NoError(t, f.kube.Get(ctx, client.ObjectKeyFromObject(workspace), workspace))
	}
	// The provider acknowledges the enforced epoch.
	workspace.Status.ObservedGeneration = workspace.Generation
	workspace.Status.State = workspacev1alpha1.ExecutionWorkspaceStateAttached
	workspace.Status.AttachedEpoch = epoch
	for _, condition := range []string{string(workspacev1alpha1.ConditionWorkspaceAdmitted), string(workspacev1alpha1.ConditionWorkspaceAttached)} {
		meta.SetStatusCondition(&workspace.Status.Conditions, metav1.Condition{
			Type: condition, Status: metav1.ConditionTrue, Reason: "Ready", ObservedGeneration: workspace.Generation,
		})
	}
	require.NoError(t, f.kube.Status().Update(ctx, workspace))
	holder := string(task.UID)
	lease := &coordinationv1.Lease{}
	key := types.NamespacedName{Namespace: "default", Name: attachmentLeaseName(reuseDispatchWorkspace)}
	if err := f.kube.Get(ctx, key, lease); err != nil {
		lease = &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}}
		require.NoError(t, f.kube.Create(ctx, lease))
	}
	lease.Annotations = map[string]string{workspaceAttachmentLeaseEpochAnnotation: strconv.FormatInt(epoch, 10)}
	lease.Spec.HolderIdentity = &holder
	lease.Spec.LeaseDurationSeconds = new(int32(3600))
	lease.Spec.RenewTime = &metav1.MicroTime{Time: time.Now()}
	require.NoError(t, f.kube.Update(ctx, lease))
}

// queueTurn creates, binds and queues a Session turn attached at epoch, as
// the Task queue does; a non-empty handoff queues it on the held path.
func (f *persistentReuseDispatchFixture) queueTurn(ctx context.Context, t *testing.T, name string, uid types.UID, epoch int64, handoff string) *corev1alpha1.Task {
	t.Helper()
	task := reuseDispatchTask(name, uid)
	task.Labels[acpRuntimeTaskPoolLabel] = f.plan.PoolName
	task.Labels[acpExecutionWorkspaceLinkLabel] = reuseDispatchWorkspace
	task.Annotations = map[string]string{
		acpExecutionWorkspaceUIDAnnotation: reuseDispatchWorkspaceUID,
		acpTaskAttachmentEpochAnnotation:   strconv.FormatInt(epoch, 10),
	}
	if handoff != "" {
		task.Annotations[acpTaskPersistentSessionHandoffAnnotation] = handoff
	}
	task.Status.Execution.RuntimePoolName = f.plan.PoolName
	task.Status.Execution.RuntimePoolUID = string(f.pool.UID)
	status := task.Status
	require.NoError(t, f.kube.Create(ctx, task))
	task.Status = status
	require.NoError(t, f.kube.Status().Update(ctx, task))
	task = f.bind(ctx, t, task)
	key := store.PromptAttemptKey{Namespace: task.Namespace, TaskUID: string(task.UID), Attempt: 1, PromptID: task.Status.Execution.PromptID}
	attemptID, err := key.CanonicalID()
	require.NoError(t, err)
	fence, err := f.epochs.CurrentFence(ctx)
	require.NoError(t, err)
	_, err = f.controlStore.CreatePromptAttempt(ctx, boundPromptAttemptForTest(&store.PromptAttempt{
		ID: attemptID, Key: key, RequestDigest: task.Status.Execution.RequestDigest,
		BindingDigest: task.Status.AgentExecutionBinding.BindingDigest, SnapshotDigest: task.Status.AgentExecutionBinding.Snapshot.Digest,
		ExecutionState: store.PromptExecutionQueued, DeliveryState: store.PromptDeliveryNotRequested,
	}), fence)
	require.NoError(t, err)
	return task
}

// bind freezes the Task's execution as the Task controller does, with the
// Session manager a session-reused execution workspace needs.
func (f *persistentReuseDispatchFixture) bind(ctx context.Context, t *testing.T, task *corev1alpha1.Task) *corev1alpha1.Task {
	t.Helper()
	binder := &TaskReconciler{
		Client: f.kube, APIReader: f.kube, Scheme: f.scheme, DurableControlStore: f.controlStore,
		AgentExecutionSnapshots: f.sqlite, ACPRuntimeImages: f.images, ControllerEpochManager: f.epochs,
		SessionManager: NewSessionManager(f.sqlite),
	}
	installTestACPWorkspaceClass(t, binder)
	// Establish the Session's identity first, so its RuntimePool is the one
	// the fixture created for that identity.
	sessionUID, err := binder.ensureACPWorkspaceSessionUID(ctx, task, reuseDispatchSessionUID)
	require.NoError(t, err)
	require.Equal(t, reuseDispatchSessionUID, sessionUID)
	bound := bindACPQueueTaskForTest(t, ctx, binder, task, f.agent)
	verified, err := binder.loadVerifiedBoundExecution(ctx, bound, bound.Status.AgentExecutionBinding)
	require.NoError(t, err)
	requestDigest, err := acpBoundTaskRequestDigest(verified, bound.Status.Execution.Attempt, bound.Status.Execution.PromptID)
	require.NoError(t, err)
	current := f.task(ctx, t, bound)
	current.Status.Execution.RequestDigest = requestDigest
	require.NoError(t, f.kube.Status().Update(ctx, current))
	return f.task(ctx, t, current)
}

func (f *persistentReuseDispatchFixture) task(ctx context.Context, t *testing.T, task *corev1alpha1.Task) *corev1alpha1.Task {
	t.Helper()
	current := &corev1alpha1.Task{}
	require.NoError(t, f.kube.Get(ctx, client.ObjectKeyFromObject(task), current))
	return current
}

func (f *persistentReuseDispatchFixture) resident(ctx context.Context, t *testing.T) *store.SessionResidentRuntime {
	t.Helper()
	control, err := f.controlStore.GetSessionControl(ctx, "default", reuseDispatchSession)
	require.NoError(t, err)
	return control.ResidentRuntime
}

// keepFirstTurn runs turn 1 through the real dispatch path and returns it
// with the record it kept.
func (f *persistentReuseDispatchFixture) keepFirstTurn(ctx context.Context, t *testing.T) (*corev1alpha1.Task, *store.SessionResidentRuntime) {
	t.Helper()
	turn1 := f.queueTurn(ctx, t, "reuse-turn-1", "12121212-0000-0000-0000-000000000001", 1, "")
	f.attach(ctx, t, turn1, 1, nil)
	runQueuedTurn(t, ctx, f.dispatcher, turn1)
	settled := f.task(ctx, t, turn1)
	require.Equal(t, corev1alpha1.TaskPhaseSucceeded, settled.Status.Phase, "turn 1: %s", settled.Status.Message)
	record := f.resident(ctx, t)
	require.NotNil(t, record, "turn 1 must keep its RuntimeSession")
	require.Equal(t, store.SessionResidentRuntimeResident, record.State)
	creates, deletes, prompts := f.runtime.counts()
	require.Equal(t, []int{1, 0, 1}, []int{creates, deletes, prompts})
	// The kept session fills the pool's only resident slot.
	f.setPool(ctx, t, f.kube, setRuntimePoolAtOwnCapacity)
	return settled, record
}

// The live r9b defect: turn 2 claimed turn 1's record, but the runtime gate
// compared the supervisor's session ID with one built from a runtime fence
// that carried no RuntimeSession yet, so every warm turn missed.
func TestACPPersistentSessionSecondTurnReusesTheKeptSessionThroughDispatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	f := newPersistentReuseDispatchFixture(t, ctx)
	turn1, record := f.keepFirstTurn(ctx, t)

	turn2 := f.queueTurn(ctx, t, "reuse-turn-2", "12121212-0000-0000-0000-000000000002", 2, record.HandoffID)
	f.attach(ctx, t, turn2, 2, turn1)
	runQueuedTurn(t, ctx, f.dispatcher, turn2)

	settled := f.task(ctx, t, turn2)
	require.Equal(t, corev1alpha1.TaskPhaseSucceeded, settled.Status.Phase, "turn 2: %s", settled.Status.Message)
	creates, deletes, prompts := f.runtime.counts()
	require.Equal(t, 1, creates, "turn 2 must not create a RuntimeSession")
	require.Equal(t, 0, deletes, "turn 2 must not retire the kept RuntimeSession")
	require.Equal(t, 2, prompts)
	require.Equal(t, uint64(record.RuntimeSessionGeneration), uint64(settled.Status.Execution.RuntimeSessionGeneration))
	require.Equal(t, record.RuntimeSessionUID, settled.Status.Execution.RuntimeSessionUID)

	f.runtime.mu.Lock()
	defer f.runtime.mu.Unlock()
	prompt := f.runtime.prompts[1]
	require.Equal(t, string(turn2.UID), string(prompt.Metadata.TaskUID), "the prompt is turn 2's own")
	require.Equal(t, harnessv2.RuntimeSessionUID(record.RuntimeSessionUID), prompt.Metadata.Fence.RuntimeSessionUID)
	require.Equal(t, uint64(record.RuntimeSessionGeneration), prompt.Metadata.Fence.RuntimeSessionGeneration,
		"the prompt goes to the existing session's generation")
	require.Equal(t, f.runtime.creates[0].RuntimeSessionID, harnessv2.RuntimeSessionID(runtimeSessionID(prompt.Metadata.Fence)))
}

// A turn whose claimed record the gate refuses falls back to a fresh
// RuntimeSession in the same dispatch, as the flag-off path does: it retires
// the resident session and, while the pool still publishes that session's
// slot (the RuntimePool controller has not re-probed yet), takes the slot the
// runtime itself shows free, instead of requeueing for the next probe.
func TestACPPersistentSessionGateMissCreatesTheFreshSessionInTheSamePass(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	f := newPersistentReuseDispatchFixture(t, ctx)
	turn1, record := f.keepFirstTurn(ctx, t)
	// The provider process tree is still busy, so the gate refuses reuse;
	// and the pool controller publishes nothing until its next probe.
	f.quiescent.Store(false)
	f.probeOnDelete.Store(false)

	turn2 := f.queueTurn(ctx, t, "reuse-turn-2", "12121212-0000-0000-0000-000000000002", 2, record.HandoffID)
	f.attach(ctx, t, turn2, 2, turn1)
	runQueuedTurn(t, ctx, f.dispatcher, turn2)

	settled := f.task(ctx, t, turn2)
	require.Equal(t, corev1alpha1.TaskPhaseSucceeded, settled.Status.Phase,
		"turn 2 must finish in one dispatch (state %s: %s)", settled.Status.Execution.State, settled.Status.Execution.Message)
	require.Equal(t, acpPersistentReasonRuntimeSessionNotIdle, settled.Annotations[acpTaskPersistentSessionRefusedAnnotation])
	creates, deletes, prompts := f.runtime.counts()
	require.Equal(t, []int{2, 1, 2}, []int{creates, deletes, prompts}, "retire the kept session, create a fresh one, send the prompt")

	f.runtime.mu.Lock()
	defer f.runtime.mu.Unlock()
	require.Equal(t, uint64(record.RuntimeSessionGeneration), f.runtime.deletes[0].Metadata.Fence.RuntimeSessionGeneration)
	require.Greater(t, f.runtime.creates[1].Metadata.Fence.RuntimeSessionGeneration, uint64(record.RuntimeSessionGeneration))
	require.Equal(t, f.runtime.creates[1].Metadata.Fence.RuntimeSessionGeneration, f.runtime.prompts[1].Metadata.Fence.RuntimeSessionGeneration)
	require.Equal(t, string(turn2.UID), string(f.runtime.prompts[1].Metadata.TaskUID))
}
