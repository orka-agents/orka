/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
)

const warmClaudeSession = "warm-claude-session"

// warmClaudeRuntime records what the fake supervisor received.
type warmClaudeRuntime struct {
	mu      sync.Mutex
	creates []harnessv2.CreateRuntimeSessionRequest
	deletes []harnessv2.DeleteRuntimeSessionRequest
	prompts []harnessv2.StartPromptRequest
}

func (r *warmClaudeRuntime) counts() (creates, deletes, prompts int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.creates), len(r.deletes), len(r.prompts)
}

// setRuntimePoolAtOwnCapacity records what the live RuntimePool controller
// published while the Session's only resident session was live on its
// dedicated single-session pool: Serving, admission Closed for capacity.
func setRuntimePoolAtOwnCapacity(pool *corev1alpha1.RuntimePool) {
	pool.Status.ObservedGeneration = pool.Generation
	pool.Status.Lifecycle = corev1alpha1.RuntimePoolLifecycleServing
	pool.Status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
	pool.Status.Message = "runtime pool is at configured capacity"
	pool.Status.Capacity.MaxResidentSessions, pool.Status.Capacity.MaxRunningPrompts = 1, 1
	pool.Status.Capacity.ResidentSessions, pool.Status.Capacity.RunningPrompts = 1, 0
	meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
		Type: corev1alpha1.RuntimePoolConditionAdmissionReady, Status: metav1.ConditionFalse, ObservedGeneration: pool.Generation,
		Reason: corev1alpha1.RuntimePoolReasonAtCapacity, Message: pool.Status.Message,
	})
}

// setRuntimePoolAccepting is the pool controller's next probe once the
// resident session is gone.
func setRuntimePoolAccepting(pool *corev1alpha1.RuntimePool) {
	pool.Status.ObservedGeneration = pool.Generation
	pool.Status.Lifecycle = corev1alpha1.RuntimePoolLifecycleServing
	pool.Status.AdmissionState = corev1alpha1.RuntimePoolAdmissionAccepting
	pool.Status.Message = "one exact runtime instance is ready"
	pool.Status.Capacity.MaxResidentSessions, pool.Status.Capacity.MaxRunningPrompts = 1, 1
	pool.Status.Capacity.ResidentSessions, pool.Status.Capacity.RunningPrompts = 0, 0
	meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
		Type: corev1alpha1.RuntimePoolConditionAdmissionReady, Status: metav1.ConditionTrue, ObservedGeneration: pool.Generation,
		Reason: "Serving", Message: pool.Status.Message,
	})
}

func updateTestRuntimePoolStatus(ctx context.Context, t *testing.T, kubeClient client.Client, name string, mutate func(*corev1alpha1.RuntimePool)) {
	t.Helper()
	pool := &corev1alpha1.RuntimePool{}
	require.NoError(t, kubeClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: name}, pool))
	mutate(pool)
	require.NoError(t, kubeClient.Status().Update(ctx, pool))
}

// addBoundSessionTurnTask creates and binds the next Task of the fixture's
// Session, with its queued PromptAttempt.
func addBoundSessionTurnTask(t *testing.T, ctx context.Context, fixture *taskScopedCreateConflictFixture, name string, uid types.UID, prompt string) *corev1alpha1.Task {
	t.Helper()
	agent := &corev1alpha1.Agent{}
	require.NoError(t, fixture.kubeClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "agent"}, agent))
	first := fixture.task
	promptID := "prompt-" + string(uid) + "-1"
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name, UID: uid, Labels: map[string]string{acpRuntimeTaskPoolLabel: first.Labels[acpRuntimeTaskPoolLabel]}},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent, Prompt: prompt, AgentRuntime: &corev1alpha1.AgentRuntimeSpec{},
			AgentRef: &corev1alpha1.AgentReference{Name: "agent"}, SessionRef: first.Spec.SessionRef.DeepCopy(),
		},
	}
	require.NoError(t, fixture.kubeClient.Create(ctx, task))
	task.Status = corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending, Attempts: 1, Execution: &corev1alpha1.TaskExecutionStatus{
		State: corev1alpha1.TaskExecutionStateQueued, Attempt: 1, PromptID: promptID,
		RuntimePoolName: first.Status.Execution.RuntimePoolName, RuntimePoolUID: first.Status.Execution.RuntimePoolUID,
		RequestDigest: testControlDigestForDispatcher(name + "-request"), ControllerEpoch: 1,
	}}
	require.NoError(t, fixture.kubeClient.Status().Update(ctx, task))
	persistence := fixture.dispatcher.ResultStore.(*sealedTestStore)
	image := "docker.io/example/acp@sha256:" + strings.Repeat("a", 64)
	task = prepareBoundACPDispatcherTaskForTest(t, ctx, fixture.kubeClient, fixture.kubeClient.Scheme(), persistence, task, agent,
		ACPRuntimeImages{Codex: image, Claude: image})
	key := store.PromptAttemptKey{Namespace: task.Namespace, TaskUID: string(task.UID), Attempt: 1, PromptID: promptID}
	attemptID, err := key.CanonicalID()
	require.NoError(t, err)
	fence, err := fixture.dispatcher.Epochs.CurrentFence(ctx)
	require.NoError(t, err)
	_, err = persistence.CreatePromptAttempt(ctx, boundPromptAttemptForTest(&store.PromptAttempt{
		ID: attemptID, Key: key, RequestDigest: task.Status.Execution.RequestDigest,
		BindingDigest: task.Status.AgentExecutionBinding.BindingDigest, SnapshotDigest: task.Status.AgentExecutionBinding.Snapshot.Digest,
		ExecutionState: store.PromptExecutionQueued, DeliveryState: store.PromptDeliveryNotRequested,
	}), fence)
	require.NoError(t, err)
	return task
}

func runQueuedTurn(t *testing.T, ctx context.Context, dispatcher *ACPDispatcher, task *corev1alpha1.Task) {
	t.Helper()
	reserved, target, err := dispatcher.reserveTask(ctx, task)
	require.NoError(t, err)
	require.NotNil(t, reserved, "the turn must reserve its RuntimePool")
	require.NoError(t, dispatcher.executeReservedTask(ctx, reserved, target))
}

type warmClaudeSessionFixture struct {
	*taskScopedCreateConflictFixture
	runtime     *warmClaudeRuntime
	persistence *sealedTestStore
	poolName    string
	first       *corev1alpha1.Task
}

// newWarmClaudeSessionFixture runs a Claude Session's first turn through
// reserveTask and executeReservedTask with the flag on. That turn leaves its
// read-intent RuntimeSession resident, so the Session's single-session pool
// then reports Serving with admission Closed at capacity, and the reuse path
// never consults the in-memory binding. deleteFails makes the supervisor
// refuse every RuntimeSession deletion.
func newWarmClaudeSessionFixture(t *testing.T, ctx context.Context, deleteFails bool) *warmClaudeSessionFixture {
	t.Helper()
	runtime := &warmClaudeRuntime{}
	f := &warmClaudeSessionFixture{runtime: runtime}
	var kube *client.Client
	f.taskScopedCreateConflictFixture = newTaskScopedCreateConflictFixtureForRuntime(t, ctx, "warm-claude-t1", "99999999-0000-0000-0000-000000000001", corev1alpha1.AgentRuntimeClaude,
		func(profile harnessv2.RuntimeProfile, digest harnessv2.ProfileDigest, kubeClient *client.Client) *httptest.Server {
			kube = kubeClient
			options := dispatcherRuntimeServerOptions{
				onPrompt: func(request harnessv2.StartPromptRequest) {
					runtime.mu.Lock()
					runtime.prompts = append(runtime.prompts, request)
					runtime.mu.Unlock()
				},
				onDelete: func(request harnessv2.DeleteRuntimeSessionRequest) {
					runtime.mu.Lock()
					runtime.deletes = append(runtime.deletes, request)
					runtime.mu.Unlock()
					if deleteFails {
						return
					}
					// The pool controller re-probes the supervisor and reopens
					// admission once its only resident session is gone.
					updateTestRuntimePoolStatus(ctx, t, *kube, f.poolName, setRuntimePoolAccepting)
				},
			}
			if deleteFails {
				options.deleteFailure = func() *harnessv2.ErrorResponse {
					return &harnessv2.ErrorResponse{Protocol: harnessv2.ProtocolVersion, Code: harnessv2.ErrorCodeDigestConflict, Message: "deletion refused by the test runtime"}
				}
			}
			return newDispatcherRuntimeServerWithOptions(t, profile, digest, options, func(request harnessv2.CreateRuntimeSessionRequest) {
				runtime.mu.Lock()
				runtime.creates = append(runtime.creates, request)
				runtime.mu.Unlock()
			})
		}, func(task *corev1alpha1.Task) {
			task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: warmClaudeSession, Create: true, Append: true}
		})
	t.Cleanup(f.stop)
	f.poolName = f.task.Status.Execution.RuntimePoolName
	f.persistence = f.dispatcher.ResultStore.(*sealedTestStore)
	f.dispatcher.EventStore = f.persistence
	f.dispatcher.PlanStore = f.persistence
	f.dispatcher.PersistentSessionEnabled = true
	continuity, err := NewACPSessionContinuity(ACPSessionContinuityConfig{
		SessionControls: f.persistence, Transcripts: f.persistence, Publications: f.persistence, BranchClaims: f.persistence,
	})
	require.NoError(t, err)
	f.dispatcher.Sessions = continuity

	runQueuedTurn(t, ctx, f.dispatcher, f.task)
	f.first = f.currentTask(t, ctx)
	require.Equal(t, corev1alpha1.TaskPhaseSucceeded, f.first.Status.Phase, "turn 1: %s", f.first.Status.Message)
	creates, deletes, prompts := runtime.counts()
	require.Equal(t, []int{1, 0, 1}, []int{creates, deletes, prompts}, "turn 1 creates and prompts its session and keeps it resident")
	updateTestRuntimePoolStatus(ctx, t, f.kubeClient, f.poolName, setRuntimePoolAtOwnCapacity)
	f.dispatcher.forgetRuntimeSessionBinding(f.first.Status.Execution.RuntimeSessionUID)
	return f
}

// ADR 0059's fallback must always reach a fresh session that receives its
// prompt. This replays the live kind run: after the fixture's first turn, the
// second turn's reuse misses (no record) and plans a fresh session. It must
// retire the resident session, take the slot, create its session and send its
// own prompt, all within one dispatch.
func TestACPFreshSessionAfterReuseMissRetiresTheResidentSessionAndSendsThePrompt(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	fixture := newWarmClaudeSessionFixture(t, ctx, false)
	runtime := fixture.runtime

	second := addBoundSessionTurnTask(t, ctx, fixture.taskScopedCreateConflictFixture, "warm-claude-t2", "99999999-0000-0000-0000-000000000002", "the second question")
	runQueuedTurn(t, ctx, fixture.dispatcher, second)

	current := &corev1alpha1.Task{}
	require.NoError(t, fixture.kubeClient.Get(ctx, client.ObjectKeyFromObject(second), current))
	creates, deletes, prompts := runtime.counts()
	require.Equal(t, 2, creates, "turn 2 must reach a fresh CreateRuntimeSession (state %s: %s)", current.Status.Execution.State, current.Status.Execution.Message)
	require.Equal(t, 1, deletes, "turn 2 retires turn 1's resident session first")
	require.Equal(t, 2, prompts, "turn 2 must send its prompt")
	require.Equal(t, corev1alpha1.TaskPhaseSucceeded, current.Status.Phase, "turn 2: %s", current.Status.Message)

	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	require.Equal(t, uint64(1), runtime.deletes[0].Metadata.Fence.RuntimeSessionGeneration, "the retired session is turn 1's")
	require.Equal(t, uint64(2), runtime.creates[1].Metadata.Fence.RuntimeSessionGeneration, "the fresh session is above it")
	require.Equal(t, string(second.UID), string(runtime.prompts[1].Metadata.TaskUID), "the prompt is turn 2's own")
}

// A turn requeued before submission keeps its Session lease and open
// SessionTurn for its retry, with its PromptAttempt unbound. When its deadline
// then settles it before runtime admission, the settlement must finalize that
// turn and release the lease; otherwise the Session can never take another
// turn or be deleted (the live stuck Session).
func TestACPRequeuedTurnThatTimesOutReleasesItsSessionLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	fixture := newWarmClaudeSessionFixture(t, ctx, true)

	second := addBoundSessionTurnTask(t, ctx, fixture.taskScopedCreateConflictFixture, "warm-claude-t2", "99999999-0000-0000-0000-000000000002", "the second question")
	runQueuedTurn(t, ctx, fixture.dispatcher, second)
	current := &corev1alpha1.Task{}
	require.NoError(t, fixture.kubeClient.Get(ctx, client.ObjectKeyFromObject(second), current))
	require.Equal(t, corev1alpha1.TaskExecutionStateReserved, current.Status.Execution.State, "turn 2 is requeued before submission")
	control, err := fixture.persistence.GetSessionControl(ctx, "default", warmClaudeSession)
	require.NoError(t, err)
	require.NotNil(t, control.Lease)
	require.Equal(t, string(second.UID), control.Lease.TaskUID, "the requeued turn keeps its lease for the retry")

	// Its deadline passes while it waits.
	current.Spec.Timeout = &metav1.Duration{Duration: time.Second}
	if current.Annotations == nil {
		current.Annotations = map[string]string{}
	}
	current.Annotations[acpRuntimeQueuedAtAnnotation] = time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	require.NoError(t, fixture.kubeClient.Update(ctx, current))
	if !current.CreationTimestamp.IsZero() {
		time.Sleep(time.Until(current.CreationTimestamp.Add(1100 * time.Millisecond)))
	}
	reserved, _, err := fixture.dispatcher.reserveTask(ctx, current)
	require.NoError(t, err)
	require.Nil(t, reserved, "the deadline settles the turn before runtime admission")

	require.NoError(t, fixture.kubeClient.Get(ctx, client.ObjectKeyFromObject(second), current))
	require.Equal(t, corev1alpha1.TaskPhaseCancelled, current.Status.Phase, "turn 2: %s", current.Status.Message)
	control, err = fixture.persistence.GetSessionControl(ctx, "default", warmClaudeSession)
	require.NoError(t, err)
	require.Nil(t, control.Lease, "the settlement released the cancelled turn's Session lease")
	_, _, prompts := fixture.runtime.counts()
	require.Equal(t, 1, prompts, "the cancelled turn never sent a prompt")
}
