package kube

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

func TestReclaimPromptAttemptsScopesApprovalEffectsToTask(t *testing.T) {
	for _, test := range []struct {
		name        string
		taskUID     string
		aggregate   string
		changedHint *string
		blocked     bool
	}{
		{name: "later Task sharing session", taskUID: "task-later", aggregate: "runtime-session-a"},
		{name: "later Task with forged own hint", taskUID: "task-later", aggregate: "runtime-session-a", changedHint: new("task-earlier")},
		{name: "own approval", taskUID: "task-earlier", aggregate: "runtime-session-a", blocked: true},
		{name: "own approval with forged later hint", taskUID: "task-earlier", aggregate: "runtime-session-a", changedHint: new("task-later"), blocked: true},
		{name: "own approval with removed hint", taskUID: "task-earlier", aggregate: "runtime-session-a", changedHint: new(""), blocked: true},
		{name: "legacy effect without Task label", aggregate: "runtime-session-a", blocked: true},
		{name: "legacy effect with forged later hint", aggregate: "runtime-session-a", changedHint: new("task-later"), blocked: true},
		{name: "legacy own hint without projected session", aggregate: "unprojected-session", changedHint: new("task-earlier"), blocked: true},
		{name: "own approval without projected session", taskUID: "task-earlier", aggregate: "unprojected-session", blocked: true},
		{name: "own unprojected approval with forged hint", taskUID: "task-earlier", aggregate: "unprojected-session", changedHint: new("task-later"), blocked: true},
	} {
		for _, state := range []store.ExternalEffectState{store.ExternalEffectPending, store.ExternalEffectInFlight} {
			t.Run(test.name+"/"+string(state), func(t *testing.T) {
				ctx := t.Context()
				kubeStore, kubeClient, fence := newPromptAttemptReclaimStore(t)
				attempt := createFailedPromptAttempt(t, ctx, kubeStore, fence, "task-earlier", 1, "prompt-earlier")
				projectionID := enqueueTaskTerminalProjection(t, ctx, kubeStore, fence, attempt, true)
				request := approvalDiscoveryRequest(fence)
				request.ApprovalTaskUID = test.taskUID
				request.Identity.AggregateID = test.aggregate
				effect, err := kubeStore.ReserveExternalEffect(ctx, request)
				require.NoError(t, err)
				if state == store.ExternalEffectInFlight {
					expires := testNow.Add(time.Minute)
					effect, err = kubeStore.TransitionExternalEffect(ctx, store.ExternalEffectTransition{
						ID: effect.ID, Fence: fence, ExpectedVersion: effect.Version, ExpectedState: store.ExternalEffectPending,
						NewState: store.ExternalEffectInFlight, RequestDigest: effect.RequestDigest,
						LeaseOwner: "approval-owner", LeaseExpiresAt: &expires, UpdatedAt: testNow,
					})
					require.NoError(t, err)
				}
				if test.changedHint != nil {
					object := approvalDiscoveryObject(t, kubeClient, request.Identity)
					before := object.DeepCopy()
					if *test.changedHint == "" {
						delete(object.Labels, corev1alpha1.ControlRecordTaskUIDLabel)
					} else {
						object.Labels[corev1alpha1.ControlRecordTaskUIDLabel] = *test.changedHint
					}
					require.NoError(t, kubeClient.Patch(ctx, object, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})))
					require.Equal(t, before.Spec, approvalDiscoveryObject(t, kubeClient, request.Identity).Spec)
				}
				taskName := createDeletingTaskForPromptAttempt(t, ctx, kubeClient, attempt)

				deleted, err := kubeStore.ReclaimPromptAttempts(ctx, store.ReclaimPromptAttemptsRequest{
					Namespace: attempt.Key.Namespace, TaskName: taskName, TaskUID: attempt.Key.TaskUID,
					Mode: store.PromptAttemptReclamationProjected, FinalPromptAttemptID: attempt.ID,
					TerminalProjectionID: projectionID, Fence: fence,
					RelatedExternalEffectAggregateIDs: []string{"runtime-session-a"},
				})
				if test.blocked {
					require.ErrorIs(t, err, store.ErrNotReady)
					require.Zero(t, deleted)
					_, err = kubeStore.GetPromptAttempt(ctx, attempt.ID)
					require.NoError(t, err)
				} else {
					require.NoError(t, err)
					require.Equal(t, 1, deleted)
					_, err = kubeStore.GetPromptAttempt(ctx, attempt.ID)
					require.ErrorIs(t, err, store.ErrNotFound)
				}
				preserved, err := kubeStore.GetExternalEffect(ctx, effect.ID)
				require.NoError(t, err)
				require.Equal(t, effect, preserved, "Task reclamation must preserve the approval receipt")
			})
		}
	}
}
