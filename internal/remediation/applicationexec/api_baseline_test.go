package applicationexec

import (
	"context"
	"testing"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	"github.com/stretchr/testify/require"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestNormalBaselineUsesCreateResponseNotLaterControllerStatus(t *testing.T) {
	f := newFixture(t)
	r := f.newRun()
	require.NoError(t, f.backend.ensureClaim(t.Context(), r))
	require.NoError(t, f.backend.ensureNamespace(t.Context(), r, pv.Original))
	initialVersion := ""
	f.backend.target = interceptor.NewClient(f.target.(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.CreateOption) error {
			if err := c.Create(ctx, object, options...); err != nil {
				return err
			}
			if ingress, ok := object.(*networkingv1.Ingress); ok && ingress.Name == "control" {
				initialVersion = ingress.ResourceVersion
				changed := ingress.DeepCopy()
				changed.Status.LoadBalancer.Ingress = []networkingv1.IngressLoadBalancerIngress{{IP: "198.51.100.10"}}
				return c.Status().Update(ctx, changed)
			}
			return nil
		},
	})
	require.NoError(t, f.backend.ensureFixtures(t.Context(), r, pv.Original, "resource", r.state.Request.Checks.Application.Resources))
	baseline, err := f.backend.inputAPIBaseline(r, pv.Original, "control")
	require.NoError(t, err)
	require.NotNil(t, baseline)
	require.Equal(t, initialVersion, baseline.ResourceVersion)
	require.True(t, baseline.Result.Exists)
	current := &networkingv1.Ingress{}
	require.NoError(t, f.target.Get(t.Context(), client.ObjectKey{Namespace: namespaceName(r, pv.Original), Name: "control"}, current))
	require.NotEqual(t, current.ResourceVersion, baseline.ResourceVersion)
	reloaded, err := f.backend.load(t.Context(), r.object.Name)
	require.NoError(t, err)
	require.NoError(t, f.backend.ensureFixtures(t.Context(), reloaded, pv.Original, "resource", reloaded.state.Request.Checks.Application.Resources))
	resumed, err := f.backend.inputAPIBaseline(reloaded, pv.Original, "control")
	require.NoError(t, err)
	require.Equal(t, baseline, resumed, "resuming must not manufacture a new baseline from current status")
	input := pv.ApplicationWorkerInput{Side: pv.Original, CheckID: "control", APIBaseline: baseline}
	observation := pv.Observation{Application: &pv.ApplicationObservation{APIBaseline: baseline}}
	require.NoError(t, validateNormalAPIBaseline(reloaded, input, observation))
	changed := *baseline
	changed.ResourceVersion = current.ResourceVersion
	observation.Application.APIBaseline = &changed
	require.ErrorIs(t, validateNormalAPIBaseline(reloaded, input, observation), contract.ErrConflict)
}

func TestNormalActorBaselineRequiresItsDurableOwnedOutcome(t *testing.T) {
	f := newFixture(t)
	installPolicyFixture(t, f)
	r := f.newRun()
	check := r.state.Request.Checks.Checks[1]
	initial := &pv.ApplicationAPIIdentity{APIVersion: "fixtures.orka.ai/v1", Resource: "probes",
		Name: check.Application.Kubernetes.Name, Pointer: "/status/ready", UID: "created-by-actor", ResourceVersion: "1",
		Result: pv.ApplicationAPIResult{Exists: true}}
	effectKey := resourceKey(pv.Original, "actor-normal")
	r.state.Effects = append(r.state.Effects, effect{Key: effectKey, UID: "created-by-actor", Attempted: true, Actor: true})
	slot := actionSlot(pv.Original, "observe-1", 0)
	r.state.Actions[slot] = actionReceipt{Side: pv.Original, Role: "observe-1", EffectKey: effectKey,
		Outcome: &pv.ApplicationActionObservation{StatusCode: 201, UID: initial.UID, ResourceVersion: initial.ResourceVersion, InitialAPI: initial}}
	input := pv.ApplicationWorkerInput{Side: pv.Original, CheckID: check.ID}
	observation := pv.Observation{Application: &pv.ApplicationObservation{APIBaseline: initial}}
	require.NoError(t, validateNormalAPIBaseline(r, input, observation))
	changed := *initial
	changed.ResourceVersion = "2"
	observation.Application.APIBaseline = &changed
	require.ErrorIs(t, validateNormalAPIBaseline(r, input, observation), contract.ErrConflict)
	observation.Application.APIBaseline = initial
	r.effect(effectKey).UID = "replacement"
	require.ErrorIs(t, validateNormalAPIBaseline(r, input, observation), contract.ErrConflict)
	r.effect(effectKey).UID = "created-by-actor"
	action := r.state.Actions[slot]
	action.Outcome = nil
	r.state.Actions[slot] = action
	require.ErrorIs(t, validateNormalAPIBaseline(r, input, observation), contract.ErrConflict)
}

func TestApplicationSourcePVCUsesPrivateArchiveSubdirectory(t *testing.T) {
	f := newFixture(t)
	r := f.newRun()
	for _, test := range []struct {
		side, mode, expectedPath, expectedSubPath string
	}{
		{pv.Original, "prepare", "/source", ""},
		{pv.Original, "build", "/source/source.tar", "archives/original.tar"},
		{pv.Patched, "build", "/source/source.tar", "archives/patched.tar"},
	} {
		job, err := f.backend.job(r, test.side, "source-mount", jobOptions{role: test.mode, mode: test.mode,
			image: f.config.WorkerImage, taskUID: string(r.state.Task.UID), serviceAccount: "worker", source: true})
		require.NoError(t, err)
		found := false
		for _, mount := range job.Spec.Template.Spec.Containers[0].VolumeMounts {
			if mount.Name == "source" {
				found = true
				require.Equal(t, test.expectedPath, mount.MountPath)
				require.Equal(t, test.expectedSubPath, mount.SubPath)
				require.Equal(t, test.mode == "build", mount.ReadOnly)
			}
		}
		require.True(t, found)
	}
}

func TestTypedCreateBaselineRestoresOnlyRegisteredTypeMeta(t *testing.T) {
	f := newFixture(t)
	r := f.newRun()
	response := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{
		Name: "control", Namespace: namespaceName(r, pv.Original), UID: "create-response-uid", ResourceVersion: "17"}}
	require.Empty(t, response.APIVersion)
	require.Empty(t, response.Kind)
	baselines, err := f.backend.apiBaselinesFromCreate(r, resourceKey(pv.Original, "resource-1"), response)
	require.NoError(t, err)
	require.Len(t, baselines, 1)
	initial := baselines[resourceKey(pv.Original, "control")].Identity
	require.Equal(t, "networking.k8s.io/v1", initial.APIVersion)
	require.Equal(t, "ingresses", initial.Resource)
	require.Equal(t, "/status/loadBalancer/ingress", initial.Pointer)
	require.Equal(t, "create-response-uid", initial.UID)
	require.Equal(t, "17", initial.ResourceVersion)
	require.True(t, initial.Result.Exists)
	require.False(t, initial.Result.Found)
	require.Empty(t, response.APIVersion, "normalization must not mutate the received object")

	for name, change := range map[string]func(*networkingv1.Ingress){
		"missing-uid":    func(value *networkingv1.Ingress) { value.UID = "" },
		"missing-rv":     func(value *networkingv1.Ingress) { value.ResourceVersion = "" },
		"wrong-version":  func(value *networkingv1.Ingress) { value.APIVersion, value.Kind = "apps/v1", "Ingress" },
		"wrong-kind":     func(value *networkingv1.Ingress) { value.APIVersion, value.Kind = "networking.k8s.io/v1", "Service" },
		"incomplete-gvk": func(value *networkingv1.Ingress) { value.Kind = "Ingress" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := response.DeepCopy()
			change(changed)
			_, err := f.backend.apiBaselinesFromCreate(r, resourceKey(pv.Original, "resource-1"), changed)
			require.ErrorIs(t, err, contract.ErrConflict)
		})
	}
}
