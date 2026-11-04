package controller

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

// A Session that does not exist cannot appear by waiting when the Task asks
// not to create it, so the Task fails at once instead of at its deadline.
func TestACPDispatcherFailsCreateFalseTaskForMissingSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	creates, prompts := 0, 0
	fixture := newTaskScopedCreateConflictFixtureForRuntime(t, ctx, "missing-session", "88888888-8888-8888-8888-888888888888", corev1alpha1.AgentRuntimeCodex,
		func(profile harnessv2.RuntimeProfile, digest harnessv2.ProfileDigest, _ *client.Client) *httptest.Server {
			options := dispatcherRuntimeServerOptions{onPrompt: func(harnessv2.StartPromptRequest) { prompts++ }}
			return newDispatcherRuntimeServerWithOptions(t, profile, digest, options, func(harnessv2.CreateRuntimeSessionRequest) { creates++ })
		}, func(task *corev1alpha1.Task) {
			task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "never-created", Create: false, Append: true}
		})
	defer fixture.stop()
	persistence := fixture.dispatcher.ResultStore.(*sqlite.Store)
	fixture.dispatcher.EventStore = persistence
	continuity, err := NewACPSessionContinuity(ACPSessionContinuityConfig{
		SessionControls: persistence, Transcripts: persistence, Publications: persistence, BranchClaims: persistence,
	})
	require.NoError(t, err)
	fixture.dispatcher.Sessions = continuity

	reserved, target, err := fixture.dispatcher.reserveTask(ctx, fixture.task)
	require.NoError(t, err)
	require.NotNil(t, reserved)
	require.NoError(t, fixture.dispatcher.executeReservedTask(ctx, reserved, target))

	current := fixture.currentTask(t, ctx)
	require.Equal(t, corev1alpha1.TaskPhaseFailed, current.Status.Phase, "message: %s", current.Status.Message)
	require.Equal(t, corev1alpha1.TaskExecutionReason("SessionNotFound"), current.Status.Execution.Reason)
	require.Equal(t, "session never-created does not exist and sessionRef.create is false", current.Status.Execution.Message)
	attempt, err := persistence.GetPromptAttempt(ctx, fixture.attemptID)
	require.NoError(t, err)
	require.Equal(t, store.PromptExecutionFailed, attempt.ExecutionState)
	require.Empty(t, attempt.SessionUID)
	_, err = persistence.GetSessionControl(ctx, "default", "never-created")
	require.ErrorIs(t, err, store.ErrNotFound, "a create=false Task must not create the Session")
	require.Zero(t, creates)
	require.Zero(t, prompts)
}
