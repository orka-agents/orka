package applicationexec

import (
	"context"
	"fmt"
	"testing"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func rejectedCreateFixture(t *testing.T) (*fixture, *run, *pv.ApplicationAPIIdentity) {
	t.Helper()
	f := newFixture(t)
	r := f.newRun()
	key := resourceKey(pv.Original, "actor-object")
	target := pv.ApplicationActionTarget{APIVersion: "v1", Resource: "configmaps", Name: "rejected-create",
		MustBeAbsent: true, OwnerUID: string(r.object.UID)}
	r.state.Effects = append(r.state.Effects, effect{Key: key, APIVersion: "v1", Kind: "ConfigMap",
		Namespace: namespaceName(r, pv.Original), Name: target.Name, Digest: pv.Digest([]byte("admitted-create")),
		Attempted: true, Actor: true, Rejected: true, Failure: "create-rejected"})
	r.state.Actions[actionSlot(pv.Original, "observe-0", 0)] = actionReceipt{Side: pv.Original, Role: "observe-0",
		EffectKey: key, Target: target, Outcome: &pv.ApplicationActionObservation{Target: target,
			APIVersion: target.APIVersion, Resource: target.Resource, Name: target.Name, StatusCode: 403}}
	return f, r, &pv.ApplicationAPIIdentity{APIVersion: "v1", Resource: "configmaps", Name: target.Name,
		Result: pv.ApplicationAPIResult{Exists: false}}
}

func TestRejectedCreateReservationAllowsProvenAbsence(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 405, 409, 422, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f, r, identity := rejectedCreateFixture(t)
			r.state.Actions[actionSlot(pv.Original, "observe-0", 0)].Outcome.StatusCode = status
			require.NoError(t, f.backend.validateAPIIdentity(t.Context(), r, pv.Original, identity))
			require.Empty(t, r.effect(resourceKey(pv.Original, "actor-object")).UID)
			require.True(t, r.effect(resourceKey(pv.Original, "actor-object")).Rejected)
		})
	}
}

func TestAbsentActorTargetNeverExcusesUncertainOrCreatedEffects(t *testing.T) {
	for _, which := range []string{"unknown", "timeout", "successful", "update", "not-started", "no-outcome",
		"wrong-side", "wrong-effect", "wrong-target", "returned-uid", "not-actor", "not-attempted", "not-dispatched"} {
		t.Run(which, func(t *testing.T) {
			f, r, identity := rejectedCreateFixture(t)
			entry := r.effect(resourceKey(pv.Original, "actor-object"))
			slot := actionSlot(pv.Original, "observe-0", 0)
			action := r.state.Actions[slot]
			switch which {
			case "unknown":
				entry.Rejected, entry.Failure = false, "create-outcome-unknown"
			case "timeout":
				action.Outcome.StatusCode = 504
			case "successful":
				entry.UID, entry.Rejected, entry.Failure = "created-uid", false, ""
				action.Outcome.StatusCode, action.Outcome.UID = 201, "created-uid"
			case "update":
				action.Target.MustBeAbsent, action.Target.UID, action.Target.ResourceVersion = false, "original-uid", "123"
				action.Outcome.Target = action.Target
			case "not-started":
				action.NotStarted = true
			case "no-outcome":
				action.Outcome = nil
			case "wrong-side":
				action.Side = pv.Patched
			case "wrong-effect":
				action.EffectKey = "another/effect"
			case "wrong-target":
				action.Outcome.Target.Name = "another-target"
			case "returned-uid":
				action.Outcome.UID, action.Outcome.ResourceVersion = "unclassified-uid", "456"
			case "not-actor":
				entry.Actor = false
			case "not-attempted":
				entry.Attempted = false
			case "not-dispatched":
				entry.Failure = "not-dispatched"
			}
			r.state.Actions[slot] = action
			require.ErrorIs(t, f.backend.validateAPIIdentity(t.Context(), r, pv.Original, identity), contract.ErrConflict)
		})
	}
}

func TestRejectedCreateAbsenceRechecksLiveIdentity(t *testing.T) {
	f, r, identity := rejectedCreateFixture(t)
	replacement := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: identity.Name,
		Namespace: namespaceName(r, pv.Original), UID: "unowned-replacement"}}
	require.NoError(t, f.target.Create(t.Context(), replacement))
	require.ErrorIs(t, f.backend.validateAPIIdentity(t.Context(), r, pv.Original, identity), contract.ErrConflict)
	identity.Result.Exists, identity.UID = true, string(replacement.UID)
	require.ErrorIs(t, f.backend.validateAPIIdentity(t.Context(), r, pv.Original, identity), contract.ErrConflict)
	require.Empty(t, f.deleted)
}

func TestRejectedCreateReadFailureIsNotAbsence(t *testing.T) {
	f, r, identity := rejectedCreateFixture(t)
	f.backend.targetReader = fake.NewClientBuilder().WithScheme(f.scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return apierrors.NewServiceUnavailable("fixture read unavailable")
		},
	}).Build()
	require.ErrorIs(t, f.backend.validateAPIIdentity(t.Context(), r, pv.Original, identity), contract.ErrUnavailable)
}
