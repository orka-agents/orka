package controller

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/artifactcap"
	"github.com/orka-agents/orka/internal/store"
)

func TestACPTaskCleanupScopesApprovalEffectsToTask(t *testing.T) {
	for _, test := range []struct {
		name               string
		owner              string
		changedHint        *string
		unprojectedSession bool
		blocked            bool
	}{
		{name: "later Task sharing session", owner: "later-task"},
		{name: "later Task with forged own hint", owner: "later-task", changedHint: new("own")},
		{name: "own approval", owner: "own", blocked: true},
		{name: "own approval with forged later hint", owner: "own", changedHint: new("later-task"), blocked: true},
		{name: "own approval with removed hint", owner: "own", changedHint: new(""), blocked: true},
		{name: "legacy effect without Task label", blocked: true},
		{name: "legacy effect with forged later hint", changedHint: new("later-task"), blocked: true},
		{name: "own approval without projected session", owner: "own", unprojectedSession: true, blocked: true},
		{name: "own unprojected approval with forged hint", owner: "own", changedHint: new("later-task"), unprojectedSession: true, blocked: true},
		{name: "own unprojected approval with removed hint", owner: "own", changedHint: new(""), unprojectedSession: true, blocked: true},
		{name: "legacy own hint without projected session", changedHint: new("own"), unprojectedSession: true, blocked: true},
	} {
		for _, state := range []store.ExternalEffectState{store.ExternalEffectPending, store.ExternalEffectInFlight} {
			t.Run(test.name+"/"+string(state), func(t *testing.T) {
				task, _ := promptAttemptReclaimFinalizerFixture(t)
				task.Spec.Workspace = nil
				task.Status.Execution.RuntimeSessionUID = "shared-session"
				if test.unprojectedSession {
					task.Status.Execution.RuntimeSessionUID = ""
					task.Status.Execution.RuntimeSessionGeneration = 0
				}
				owner := test.owner
				if owner == "own" {
					owner = string(task.UID)
				}
				effect := &corev1alpha1.ExternalEffect{
					ObjectMeta: metav1.ObjectMeta{Namespace: task.Namespace, Name: "approval-effect", UID: "approval-effect-uid",
						Labels: map[string]string{}},
					Spec: corev1alpha1.ExternalEffectSpec{
						ID: "approval-effect", Kind: acpMCPToolEffectKind, AggregateID: "shared-session", ApprovalTaskUID: owner,
					},
					Status: corev1alpha1.ExternalEffectStatus{State: corev1alpha1.ExternalEffectControlState(state)},
				}
				if owner != "" {
					effect.Labels[corev1alpha1.ControlRecordTaskUIDLabel] = owner
				}
				if state == store.ExternalEffectInFlight {
					expires := metav1.NewTime(time.Now().Add(time.Minute))
					effect.Status.LeaseOwner = "approval-owner"
					effect.Status.LeaseExpiresAt = &expires
					effect.Status.Attempts = 1
				}
				reconciler := newUnitReconciler(newTestScheme(), effect)
				key := client.ObjectKeyFromObject(effect)
				require.NoError(t, reconciler.Get(t.Context(), key, effect))
				if test.changedHint != nil {
					before := effect.DeepCopy()
					if effect.Labels == nil {
						effect.Labels = map[string]string{}
					}
					switch *test.changedHint {
					case "":
						delete(effect.Labels, corev1alpha1.ControlRecordTaskUIDLabel)
					case "own":
						effect.Labels[corev1alpha1.ControlRecordTaskUIDLabel] = string(task.UID)
					default:
						effect.Labels[corev1alpha1.ControlRecordTaskUIDLabel] = *test.changedHint
					}
					require.NoError(t, reconciler.Patch(t.Context(), effect,
						client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})))
					require.NoError(t, reconciler.Get(t.Context(), key, effect))
					require.Equal(t, before.UID, effect.UID)
					require.NotEqual(t, before.ResourceVersion, effect.ResourceVersion, "exercise a persisted metadata patch")
					require.Equal(t, before.Spec, effect.Spec, "discovery metadata must not alter the immutable Task binding")
					require.Equal(t, before.Status, effect.Status, "discovery metadata must not settle the action")
				}
				unsettled, err := reconciler.acpTaskHasUnsettledExternalEffects(t.Context(), task, "")
				require.NoError(t, err)
				require.Equal(t, test.blocked, unsettled, "cleanup readiness must use the approval's immutable Task binding")

				identities, ready, err := reconciler.acpArtifactRetirementIdentities(t.Context(), task)
				require.NoError(t, err)
				require.Equal(t, !test.blocked, ready, "artifact retirement must use the same Task boundary")
				if ready {
					require.Equal(t, []artifactcap.Identity{{Namespace: task.Namespace, TaskID: string(task.UID)}}, identities)
				} else {
					require.Empty(t, identities)
				}
			})
		}
	}
}
