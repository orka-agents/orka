package controller

import (
	"context"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	enforcementv1 "github.com/orka-agents/orka/api/enforcement/v1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	kubestore "github.com/orka-agents/orka/internal/store/kube"
	"github.com/orka-agents/orka/internal/store/sqlite"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func enforcedACPDispatchFixture(
	t *testing.T,
	options dispatcherRuntimeServerOptions,
	onCreate func(harnessv2.CreateRuntimeSessionRequest),
) (*ACPDispatcher, *corev1alpha1.Task, *recordingEnforcementCoordinator) {
	t.Helper()
	d, task, target, _, coordinator := acpEnforcementFixture(t)
	scheme := newTestScheme()
	require.NoError(t, coordinationv1.AddToScheme(scheme))
	task.CreationTimestamp = metav1.Now()
	task.Spec.Prompt = "respond briefly"
	task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{}
	task.Spec.AgentRef = &corev1alpha1.AgentReference{Name: "agent"}
	task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "session", Create: true, Append: true}
	task.Status = corev1alpha1.TaskStatus{
		Phase: corev1alpha1.TaskPhasePending, Attempts: 1,
		Execution: &corev1alpha1.TaskExecutionStatus{
			State: corev1alpha1.TaskExecutionStateQueued, Attempt: 1, PromptID: "prompt-" + string(task.UID) + "-1",
			RequestDigest: testControlDigestForDispatcher("enforced-dispatch"), ControllerEpoch: 1,
		},
	}
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Namespace: task.Namespace, Name: "agent", UID: "agent-uid", Generation: 1},
		Spec: corev1alpha1.AgentSpec{
			Model: &corev1alpha1.ModelConfig{Name: acpTestModel},
			Runtime: &corev1alpha1.AgentCLIRuntime{
				Type: corev1alpha1.AgentRuntimeCodex, ContractVersion: new(corev1alpha1.AgentRuntimeContractHarnessV2),
			},
		},
	}
	images := ACPRuntimeImages{Codex: "docker.io/example/acp@sha256:" + strings.Repeat("a", 64)}
	plan := frozenACPDispatcherPlanForTest(t, task, agent, images)
	pool := target.pool
	pool.Name = plan.PoolName
	pool.Spec.Runtime = corev1alpha1.RuntimePoolRuntimeSpec{Image: plan.Image, Profile: RuntimePoolProfileFromPlan(plan)}
	pool.Status.ControllerEpoch = 1
	active := pool.Status.ActiveInstance
	pool.Spec.RuntimeNamespace = active.PodNamespace
	active.ControllerEpoch, active.RuntimeInstanceID, active.BootID = 1, "pod-uid.boot-id", "boot-id"
	active.ProfileDigest = string(plan.Digest)
	active.ProtocolVersion = corev1alpha1.RuntimePoolProtocolHarnessV2
	active.ProfileDigestSchemaVersion = strconv.FormatUint(uint64(harnessv2.ProfileDigestSchemaVersion), 10)
	server := newDispatcherRuntimeServerForPoolWithOptions(t, plan.Profile, plan.Digest, string(pool.UID), options, onCreate)
	t.Cleanup(server.Close)
	address, err := url.Parse(server.URL)
	require.NoError(t, err)
	active.PodAddress = address.Host
	task.Labels = map[string]string{acpRuntimeTaskPoolLabel: plan.PoolName}
	task.Status.Execution.RuntimePoolName, task.Status.Execution.RuntimePoolUID = plan.PoolName, string(pool.UID)
	pod, node := &corev1.Pod{}, &corev1.Node{}
	require.NoError(t, d.Client.Get(t.Context(), client.ObjectKey{Namespace: active.PodNamespace, Name: active.PodName}, pod))
	require.NoError(t, d.Client.Get(t.Context(), client.ObjectKey{Name: "node"}, node))
	pod.Spec.Containers[0].Image = plan.Image
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: pool.Spec.RuntimeNamespace, Name: "pool-auth-e1", Labels: map[string]string{
			runtimePoolAuthLabel: "true", runtimePoolUIDLabel: string(pool.UID),
		}},
		Data: map[string][]byte{runtimePoolControllerTokenKey: []byte(strings.Repeat("t", 32)), runtimePoolCapabilitySecretKey: []byte(strings.Repeat("s", 32))},
	}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: task.Namespace, UID: "namespace-uid"}}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(
		&corev1alpha1.Task{}, &corev1alpha1.RuntimePool{}, &corev1alpha1.ControllerEpoch{},
		&corev1alpha1.PromptAttempt{}, &corev1alpha1.RuntimeSessionControl{},
		&corev1alpha1.BranchClaim{}, &corev1alpha1.Publication{}, &corev1alpha1.ExternalEffect{}, &corev1.Pod{},
	).WithObjects(task, pool, pod, node, secret, namespace, agent).Build()
	kubeClient = withControllerEpochLeaseUIDs(t, kubeClient)
	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "dispatch.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	persistence := sqlite.NewStore(db, "enforced-dispatch")
	control, err := kubestore.NewComposite(kubeClient, "orka-system", persistence, kubestore.WithAPIReader(kubeClient))
	require.NoError(t, err)
	epochs := NewControllerEpochManager(control, "controller-test").WithMirror(persistence)
	epochCtx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- epochs.Start(epochCtx) }()
	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-done)
	})
	fence, err := epochs.CurrentFence(t.Context())
	require.NoError(t, err)
	task = prepareBoundACPDispatcherTaskWithStoresForTest(t, t.Context(), kubeClient, scheme, control, persistence, task, agent, images)
	_, err = control.CreatePromptAttempt(t.Context(), boundPromptAttemptForTest(&store.PromptAttempt{
		Key:           store.PromptAttemptKey{Namespace: task.Namespace, TaskUID: string(task.UID), Attempt: 1, PromptID: task.Status.Execution.PromptID},
		RequestDigest: task.Status.Execution.RequestDigest, BindingDigest: task.Status.AgentExecutionBinding.BindingDigest,
		SnapshotDigest: task.Status.AgentExecutionBinding.Snapshot.Digest,
	}), fence)
	require.NoError(t, err)
	continuity, err := NewACPSessionContinuity(ACPSessionContinuityConfig{
		SessionControls: control, Transcripts: persistence, Publications: control, BranchClaims: control, Lineages: persistence,
	})
	require.NoError(t, err)
	return &ACPDispatcher{
		Client: kubeClient, APIReader: kubeClient, Store: control, ResultStore: persistence, Snapshots: persistence,
		Epochs: epochs, Sessions: continuity, Enforcement: coordinator,
	}, task, coordinator
}

func TestACPEnforcementDispatchPreparesVerifiesLaunchesAndCompletes(t *testing.T) {
	var prepared, launches, prompts atomic.Int32
	var coordinator *recordingEnforcementCoordinator
	d, task, configured := enforcedACPDispatchFixture(t, dispatcherRuntimeServerOptions{
		onLaunch: func(harnessv2.LaunchRuntimeSessionRequest) {
			if coordinator.ensureCalls == 0 {
				t.Error("launch preceded provider verification")
			}
			launches.Add(1)
		},
		onPrompt: func(harnessv2.StartPromptRequest) { prompts.Add(1) },
	}, func(request harnessv2.CreateRuntimeSessionRequest) {
		if !request.PrepareOnly {
			t.Error("protected dispatch bypassed preparation")
		}
		prepared.Add(1)
	})
	coordinator = configured
	dispatchQueuedTask(t.Context(), t, d, task)
	current := &corev1alpha1.Task{}
	require.NoError(t, d.Client.Get(t.Context(), client.ObjectKeyFromObject(task), current))
	require.Equal(t, corev1alpha1.TaskPhaseSucceeded, current.Status.Phase, "execution=%#v ensures=%d prepared=%d", current.Status.Execution, coordinator.ensureCalls, prepared.Load())
	require.EqualValues(t, 1, prepared.Load())
	require.EqualValues(t, 1, launches.Load())
	require.EqualValues(t, 1, prompts.Load())
	require.False(t, current.Status.Execution.RuntimeSessionRecreationPending)
	require.Zero(t, coordinator.retireCalls)
}

func TestACPEnforcementDispatchRefusalRetiresUnlaunchedReadSession(t *testing.T) {
	var deleted, launches, prompts atomic.Int32
	d, task, coordinator := enforcedACPDispatchFixture(t, dispatcherRuntimeServerOptions{
		onDelete: func(harnessv2.DeleteRuntimeSessionRequest) { deleted.Add(1) },
		onLaunch: func(harnessv2.LaunchRuntimeSessionRequest) { launches.Add(1) },
		onPrompt: func(harnessv2.StartPromptRequest) { prompts.Add(1) },
	}, nil)
	coordinator.outcome = &enforcementv1.EnsureResponse{Result: &enforcementv1.EnsureResponse_Refused{Refused: &enforcementv1.Refused{Reason: "unsupported"}}}
	dispatchQueuedTask(t.Context(), t, d, task)
	current := &corev1alpha1.Task{}
	require.NoError(t, d.Client.Get(t.Context(), client.ObjectKeyFromObject(task), current))
	require.Equal(t, corev1alpha1.TaskPhaseFailed, current.Status.Phase, "execution=%#v ensures=%d", current.Status.Execution, coordinator.ensureCalls)
	require.EqualValues(t, 1, deleted.Load())
	require.Zero(t, launches.Load())
	require.Zero(t, prompts.Load())
	require.Zero(t, coordinator.retireCalls)
}

func TestACPEnforcementDispatchUncertainAppliedLaunchRecoversWithoutRelaunch(t *testing.T) {
	var launches, prompts atomic.Int32
	d, task, coordinator := enforcedACPDispatchFixture(t, dispatcherRuntimeServerOptions{
		onLaunch:             func(harnessv2.LaunchRuntimeSessionRequest) { launches.Add(1) },
		onPrompt:             func(harnessv2.StartPromptRequest) { prompts.Add(1) },
		launchFailureApplied: true,
		launchFailure: &harnessv2.ErrorResponse{
			Protocol: harnessv2.ProtocolVersion, Code: harnessv2.ErrorCodeRateLimited, Message: "response uncertain", Retryable: true,
		},
	}, nil)
	dispatchQueuedTask(t.Context(), t, d, task)
	current := &corev1alpha1.Task{}
	require.NoError(t, d.Client.Get(t.Context(), client.ObjectKeyFromObject(task), current))
	require.Equal(t, corev1alpha1.TaskExecutionStateReserved, current.Status.Execution.State)
	require.True(t, current.Status.Execution.RuntimeSessionRecreationPending)
	require.Zero(t, prompts.Load())
	dispatchQueuedTask(t.Context(), t, d, current)
	require.NoError(t, d.Client.Get(t.Context(), client.ObjectKeyFromObject(task), current))
	require.Equal(t, corev1alpha1.TaskPhaseSucceeded, current.Status.Phase, "%#v", current.Status.Execution)
	require.EqualValues(t, 1, launches.Load())
	require.EqualValues(t, 1, prompts.Load())
	require.Equal(t, 2, coordinator.ensureCalls)
	require.Zero(t, coordinator.retireCalls)
}

func TestACPEnforcementDispatchCapacityRetryReverifiesBeforePrompt(t *testing.T) {
	var launches, prompts atomic.Int32
	var coordinator *recordingEnforcementCoordinator
	d, task, configured := enforcedACPDispatchFixture(t, dispatcherRuntimeServerOptions{
		onLaunch: func(harnessv2.LaunchRuntimeSessionRequest) { launches.Add(1) },
		rejectPrompt: func(harnessv2.StartPromptRequest) *harnessv2.ErrorResponse {
			if prompts.Add(1) == 1 {
				return &harnessv2.ErrorResponse{
					Protocol: harnessv2.ProtocolVersion, Code: harnessv2.ErrorCodeRateLimited, Message: "at capacity", Retryable: true,
				}

			}
			if coordinator.ensureCalls < 2 {
				t.Error("capacity retry bypassed current enforcement verification")
			}
			return nil
		},
	}, nil)
	coordinator = configured
	dispatchQueuedTask(t.Context(), t, d, task)
	current := &corev1alpha1.Task{}
	require.NoError(t, d.Client.Get(t.Context(), client.ObjectKeyFromObject(task), current))
	require.Equal(t, corev1alpha1.TaskExecutionStateReserved, current.Status.Execution.State)
	dispatchQueuedTask(t.Context(), t, d, current)
	require.NoError(t, d.Client.Get(t.Context(), client.ObjectKeyFromObject(task), current))
	require.Equal(t, corev1alpha1.TaskPhaseSucceeded, current.Status.Phase, "%#v", current.Status.Execution)
	require.EqualValues(t, 1, launches.Load())
	require.EqualValues(t, 2, prompts.Load())
	require.Equal(t, 2, coordinator.ensureCalls)
	require.Zero(t, coordinator.retireCalls)
}

func TestACPEnforcementDispatchRefusalAfterUncertainLaunchRetiresPendingReadSession(t *testing.T) {
	var launches, deleted, prompts atomic.Int32
	d, task, coordinator := enforcedACPDispatchFixture(t, dispatcherRuntimeServerOptions{
		onLaunch:             func(harnessv2.LaunchRuntimeSessionRequest) { launches.Add(1) },
		onDelete:             func(harnessv2.DeleteRuntimeSessionRequest) { deleted.Add(1) },
		onPrompt:             func(harnessv2.StartPromptRequest) { prompts.Add(1) },
		launchFailureApplied: true,
		launchFailure: &harnessv2.ErrorResponse{
			Protocol: harnessv2.ProtocolVersion, Code: harnessv2.ErrorCodeRateLimited, Message: "response uncertain", Retryable: true,
		},
	}, nil)
	dispatchQueuedTask(t.Context(), t, d, task)
	current := &corev1alpha1.Task{}
	require.NoError(t, d.Client.Get(t.Context(), client.ObjectKeyFromObject(task), current))
	require.True(t, current.Status.Execution.RuntimeSessionRecreationPending)
	coordinator.outcome = &enforcementv1.EnsureResponse{Result: &enforcementv1.EnsureResponse_Refused{Refused: &enforcementv1.Refused{Reason: "unsupported"}}}
	dispatchQueuedTask(t.Context(), t, d, current)
	require.NoError(t, d.Client.Get(t.Context(), client.ObjectKeyFromObject(task), current))
	require.Equal(t, corev1alpha1.TaskPhaseFailed, current.Status.Phase)
	require.EqualValues(t, 1, launches.Load())
	require.EqualValues(t, 1, deleted.Load())
	require.Zero(t, prompts.Load())
	require.Zero(t, coordinator.retireCalls)
}

func TestACPEnforcementDispatchMissingProviderRefusesBeforePreparation(t *testing.T) {
	var prepared atomic.Int32
	d, task, coordinator := enforcedACPDispatchFixture(t, dispatcherRuntimeServerOptions{}, func(harnessv2.CreateRuntimeSessionRequest) {
		prepared.Add(1)
	})
	d.Enforcement = nil
	dispatchQueuedTask(t.Context(), t, d, task)
	current := &corev1alpha1.Task{}
	require.NoError(t, d.Client.Get(t.Context(), client.ObjectKeyFromObject(task), current))
	require.Equal(t, corev1alpha1.TaskPhaseFailed, current.Status.Phase)
	require.Zero(t, prepared.Load())
	require.Zero(t, coordinator.ensureCalls)
}
