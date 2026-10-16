package controller

import (
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

type generationCommitFailureStore struct {
	store.DurableControlStore
	failures int
}

func (s *generationCommitFailureStore) CommitSessionRuntimeGeneration(ctx context.Context, request store.CommitSessionRuntimeGenerationRequest) (*store.SessionControl, error) {
	if s.failures > 0 {
		s.failures--
		return nil, errors.New("injected generation commit failure")
	}
	return s.DurableControlStore.CommitSessionRuntimeGeneration(ctx, request)
}

// A failure between RuntimeSession creation and prompt submission requeues
// the Task with the provider process preserved. The retry reuses a process
// that never received a prompt, so the toolbox note is still due; it stops
// being due only once a prompt is accepted.
func TestACPDispatcherToolboxNoteSurvivesPreSubmissionRetry(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	creates, prompts := 0, 0
	fixture := newTaskScopedCreateConflictFixture(t, ctx, "toolbox-note-retry", "88888888-8888-8888-8888-888888888888",
		func(profile harnessv2.RuntimeProfile, digest harnessv2.ProfileDigest, _ *client.Client) *httptest.Server {
			return newDispatcherRuntimeServerWithOptions(t, profile, digest, dispatcherRuntimeServerOptions{
				onPrompt: func(harnessv2.StartPromptRequest) { prompts++ },
			}, func(harnessv2.CreateRuntimeSessionRequest) { creates++ })
		}, func(task *corev1alpha1.Task) {
			task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "toolbox-note-retry", Create: true, Append: true}
		})
	defer fixture.stop()
	persistence := fixture.dispatcher.ResultStore.(*sqlite.Store)
	failure := &generationCommitFailureStore{DurableControlStore: persistence, failures: 1}
	fixture.dispatcher.EventStore = persistence
	continuity, err := NewACPSessionContinuity(ACPSessionContinuityConfig{
		SessionControls: failure, Transcripts: persistence, Publications: persistence, BranchClaims: persistence,
	})
	require.NoError(t, err)
	fixture.dispatcher.Sessions = continuity

	reserved, target, err := fixture.dispatcher.reserveTask(ctx, fixture.task)
	require.NoError(t, err)
	require.NotNil(t, reserved)
	require.NoError(t, fixture.dispatcher.executeReservedTask(ctx, reserved, target))
	require.Equal(t, 1, creates)
	require.Zero(t, prompts, "the injected commit failure happens before prompt submission")
	current := fixture.currentTask(t, ctx)
	binding := fixture.dispatcher.currentRuntimeSessionBinding(current.Status.Execution.RuntimeSessionUID)
	require.NotNil(t, binding)
	require.False(t, binding.PromptDelivered, "a created but never-prompted process has not received the note")

	reserved, target, err = fixture.dispatcher.reserveTask(ctx, current)
	require.NoError(t, err)
	require.NotNil(t, reserved)
	retry := &acpTaskSession{Binding: *binding, Reused: true}
	require.True(t, acpToolboxNoteDue(retry), "a reused never-prompted process still gets the note")
	require.NoError(t, fixture.dispatcher.executeReservedTask(ctx, reserved, target))
	require.Equal(t, 1, creates, "the retry must reuse the preserved process")
	require.Equal(t, 1, prompts)
	current = fixture.currentTask(t, ctx)
	binding = fixture.dispatcher.currentRuntimeSessionBinding(current.Status.Execution.RuntimeSessionUID)
	require.NotNil(t, binding)
	require.True(t, binding.PromptDelivered, "an accepted prompt delivers the note")
	require.False(t, acpToolboxNoteDue(&acpTaskSession{Binding: *binding, Reused: true}))
}

// Continuation turns reuse a live binding with its delivery record, so the
// note is not repeated; any replacement process starts without it.
func TestACPToolboxNoteFollowsRuntimeSessionPlanning(t *testing.T) {
	profileDigest := harnessv2.ProfileDigest("sha256:1111111111111111111111111111111111111111111111111111111111111111")
	rotatedDigest := harnessv2.ProfileDigest("sha256:2222222222222222222222222222222222222222222222222222222222222222")
	mcpDigest := "sha256:3333333333333333333333333333333333333333333333333333333333333333"
	control := store.SessionControl{SessionUID: "session-uid", Availability: store.SessionAvailable}
	delivered := &ACPRuntimeSessionBinding{
		SessionUID: "session-uid", Generation: 3, ProfileDigest: profileDigest, MCPDigest: mcpDigest,
		RuntimeInstanceID: "instance", SupervisorBootID: "boot", PromptDelivered: true,
	}
	require.True(t, acpToolboxNoteDue(nil), "standalone Tasks always start a provider conversation")

	plan, err := PlanACPRuntimeSession(control, delivered, profileDigest, mcpDigest, "instance", "boot")
	require.NoError(t, err)
	require.False(t, plan.Recreate)
	require.False(t, acpToolboxNoteDue(&acpTaskSession{Binding: plan.Binding, Reused: true}), "a continuation turn must not repeat the note")

	for name, replan := range map[string]func() (ACPRuntimeSessionPlan, error){
		"profile rotated": func() (ACPRuntimeSessionPlan, error) {
			return PlanACPRuntimeSession(control, delivered, rotatedDigest, mcpDigest, "instance", "boot")
		},
		"runtime lost": func() (ACPRuntimeSessionPlan, error) {
			return PlanACPRuntimeSession(control, delivered, profileDigest, mcpDigest, "instance", "boot-2")
		},
		"initial": func() (ACPRuntimeSessionPlan, error) {
			return PlanACPRuntimeSession(control, nil, profileDigest, mcpDigest, "instance", "boot")
		},
	} {
		plan, err := replan()
		require.NoError(t, err, name)
		require.True(t, plan.Recreate, name)
		require.True(t, acpToolboxNoteDue(&acpTaskSession{Binding: plan.Binding}), "%s: a new process gets the note", name)
	}
}

// A Reserved Session Task whose interrupted preparation still holds an open
// SessionTurn is failed with ToolboxUnavailable only after the turn is
// recovered and finalized, so the next Session Task is not blocked on the
// mutation lease.
func TestACPDispatcherToolboxUnavailableFinalizesRecoveredSessionTurn(t *testing.T) {
	ctx := context.Background()
	controlStore, fence, closeStore := newACPSessionTestStore(t, filepath.Join(t.TempDir(), "toolbox-turn.db"))
	defer closeStore()
	continuity := newACPSessionTestContinuity(t, controlStore, ACPBootstrapLimits{})
	taskUID := types.UID("66666666-6666-6666-6666-666666666666")
	promptID := "prompt-" + string(taskUID) + "-1"
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "toolbox-turn", UID: taskUID},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent, Prompt: "toolbox pool fails during session preparation",
			SessionRef: &corev1alpha1.SessionReference{Name: "toolbox-turn", Create: true, Append: true},
		},
		Status: corev1alpha1.TaskStatus{Attempts: 1, Execution: &corev1alpha1.TaskExecutionStatus{
			State: corev1alpha1.TaskExecutionStateReserved, Attempt: 1, PromptID: promptID,
			RequestDigest: testControlDigestForDispatcher("toolbox-turn"), ControllerEpoch: fence.Epoch,
		}},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task.DeepCopy()).Build()
	key := store.PromptAttemptKey{Namespace: task.Namespace, TaskUID: string(task.UID), Attempt: 1, PromptID: promptID}
	attempt, err := controlStore.CreatePromptAttempt(ctx, boundPromptAttemptForTest(&store.PromptAttempt{Key: key, RequestDigest: task.Status.Execution.RequestDigest}), fence)
	require.NoError(t, err)
	_, err = controlStore.TransitionPromptAttemptExecution(ctx, store.PromptAttemptExecutionTransition{
		ID: attempt.ID, Fence: fence, ExpectedVersion: attempt.Version, ExpectedState: attempt.ExecutionState,
		NewState: store.PromptExecutionReserved, OperationID: "reserve-toolbox-turn",
		OperationDigest: testControlDigestForDispatcher("reserve-toolbox-turn"), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	dispatcher := &ACPDispatcher{
		Client: kubeClient, APIReader: kubeClient, Store: controlStore, Sessions: continuity,
		runtimeSessions: make(map[string]ACPRuntimeSessionBinding),
	}
	opened, err := dispatcher.prepareTaskSession(
		ctx, task, fence, harnessv2.ProfileDigest(testControlDigestForDispatcher("profile-toolbox-turn")),
		testControlDigestForDispatcher("mcp-toolbox-turn"),
		harnessv2.RuntimeInstanceID("runtime-instance"), harnessv2.SupervisorBootID("boot-id"),
		acpSessionLineageIdentity{}, false,
	)
	require.NoError(t, err)
	require.NotNil(t, opened)
	require.NotNil(t, opened.Turn)

	pool := &corev1alpha1.RuntimePool{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "pool", Generation: 1}}
	meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
		Type: corev1alpha1.RuntimePoolConditionRolloutReady, Status: metav1.ConditionFalse, ObservedGeneration: 1,
		Reason: corev1alpha1.RuntimePoolReasonToolboxUnavailable, Message: acpToolboxUnavailablePrefix + "TOOLBOX_IMAGE_PULL: test",
	})
	require.NoError(t, dispatcher.settleRuntimePoolToolboxUnavailableForPool(ctx, task, attempt.ID, fence, pool))

	turn, err := controlStore.GetSessionTurn(ctx, opened.Turn.Turn.ID)
	require.NoError(t, err)
	require.Equal(t, store.SessionTurnFinalized, turn.State)
	control, err := controlStore.GetSessionControl(ctx, task.Namespace, task.Spec.SessionRef.Name)
	require.NoError(t, err)
	require.Nil(t, control.Lease, "the toolbox failure must not leak the Session mutation lease")
	settled, err := controlStore.GetPromptAttempt(ctx, attempt.ID)
	require.NoError(t, err)
	require.Equal(t, store.PromptExecutionFailed, settled.ExecutionState)
	current := &corev1alpha1.Task{}
	require.NoError(t, kubeClient.Get(ctx, client.ObjectKeyFromObject(task), current))
	require.Equal(t, corev1alpha1.TaskPhaseFailed, current.Status.Phase)
	require.Equal(t, corev1alpha1.TaskExecutionReason(corev1alpha1.RuntimePoolReasonToolboxUnavailable), current.Status.Execution.Reason)
}
