package applicationexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestScalingAdmissionProbesRejectVersionAndReplicaSetBypasses(t *testing.T) {
	for _, bypass := range []string{"none", "v1/target", "v1/maximum", "v1/replicaset", "v2/target", "v2/maximum", "v2/replicaset", "scale/maximum"} {
		t.Run(bypass, func(t *testing.T) {
			f := newFixture(t)
			deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
				Name: "replica-target", Namespace: "owned", UID: "deployment-uid"}}
			var calls []string
			f.backend.target = fake.NewClientBuilder().WithScheme(f.scheme).WithInterceptorFuncs(interceptor.Funcs{
				Create: func(_ context.Context, _ client.WithWatch, object client.Object, options ...client.CreateOption) error {
					opts := (&client.CreateOptions{}).ApplyOptions(options)
					require.Equal(t, []string{metav1.DryRunAll}, opts.DryRun)
					require.Equal(t, metav1.FieldValidationStrict, opts.FieldValidation)
					require.Equal(t, deployment.Namespace, object.GetNamespace())
					version, name, kind, maximum := hpaProbeIdentity(t, object)
					probe := version + "/allowed"
					switch {
					case name != deployment.Name:
						probe = version + "/target"
					case kind != "Deployment":
						require.Equal(t, "ReplicaSet", kind)
						probe = version + "/replicaset"
					case maximum != 4:
						probe = version + "/maximum"
					}
					calls = append(calls, probe)
					if probe == version+"/allowed" || probe == bypass {
						return nil
					}
					return apierrors.NewForbidden(schema.GroupResource{Group: "autoscaling", Resource: "horizontalpodautoscalers"},
						object.GetName(), errors.New("application replica fixture boundary"))
				},
			}).Build()
			kube := kubernetesfake.NewClientset()
			kube.PrependReactor("get", "deployments", func(action kubetesting.Action) (bool, runtime.Object, error) {
				require.Equal(t, "scale", action.GetSubresource())
				return true, &autoscalingv1.Scale{ObjectMeta: deployment.ObjectMeta, Spec: autoscalingv1.ScaleSpec{Replicas: 0}}, nil
			})
			kube.PrependReactor("update", "deployments", func(action kubetesting.Action) (bool, runtime.Object, error) {
				update := action.(kubetesting.UpdateActionImpl)
				require.Equal(t, "scale", action.GetSubresource())
				require.Equal(t, []string{metav1.DryRunAll}, update.GetUpdateOptions().DryRun)
				require.Equal(t, metav1.FieldValidationStrict, update.GetUpdateOptions().FieldValidation)
				scale := update.GetObject().(*autoscalingv1.Scale)
				require.Equal(t, deployment.UID, scale.UID)
				if scale.Spec.Replicas == 1 {
					calls = append(calls, "scale/allowed")
					return true, scale, nil
				}
				require.Equal(t, int32(5), scale.Spec.Replicas)
				calls = append(calls, "scale/maximum")
				if bypass == "scale/maximum" {
					return true, scale, nil
				}
				return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments/scale"},
					deployment.Name, errors.New("application replica fixture boundary"))
			})
			f.backend.targetKube = kube
			err := f.backend.probeScalingAdmission(t.Context(), deployment, 4)
			if bypass != "none" {
				require.ErrorIs(t, err, ErrIsolation)
				require.Contains(t, calls, bypass)
				return
			}
			require.NoError(t, err)
			require.Equal(t, []string{"v1/allowed", "v1/target", "v1/maximum", "v1/replicaset",
				"v2/allowed", "v2/target", "v2/maximum", "v2/replicaset", "scale/allowed", "scale/maximum"}, calls)
		})
	}
}

func hpaProbeIdentity(t *testing.T, object client.Object) (string, string, string, int32) {
	t.Helper()
	switch hpa := object.(type) {
	case *autoscalingv1.HorizontalPodAutoscaler:
		require.Equal(t, "apps/v1", hpa.Spec.ScaleTargetRef.APIVersion)
		require.Equal(t, int32(1), *hpa.Spec.MinReplicas)
		return "v1", hpa.Spec.ScaleTargetRef.Name, hpa.Spec.ScaleTargetRef.Kind, hpa.Spec.MaxReplicas
	case *autoscalingv2.HorizontalPodAutoscaler:
		require.Equal(t, "apps/v1", hpa.Spec.ScaleTargetRef.APIVersion)
		require.Equal(t, int32(1), *hpa.Spec.MinReplicas)
		return "v2", hpa.Spec.ScaleTargetRef.Name, hpa.Spec.ScaleTargetRef.Kind, hpa.Spec.MaxReplicas
	default:
		t.Fatalf("unexpected probe type %T", object)
		return "", "", "", 0
	}
}

func renderedReplicaFixture(t *testing.T, f *fixture, r *run) *appsv1.Deployment {
	t.Helper()
	fixture := &pv.ApplicationFixture{ID: "replica-target", Kind: "replica", Profile: "bounded"}
	profile := &pv.ApplicationFixtureProfile{ID: "bounded", Image: f.config.ToolchainImage, ImageID: f.config.ToolchainImageID,
		Args:          []string{"/usr/bin/sleep", "3600"},
		ReplicaTarget: &pv.ApplicationReplicaTargetProfile{Name: fixture.ID, InitialReplicas: 0, MaxReplicas: 4}}
	deployment, err := f.backend.replicaDeployment(r, pv.Original, fixture, profile, string(r.state.Task.UID))
	require.NoError(t, err)
	return deployment
}

func TestReplicaDeploymentRendererForServerDryRun(t *testing.T) {
	f := newFixture(t)
	r := f.newRun()
	deployment := renderedReplicaFixture(t, f, r)
	require.Zero(t, *deployment.Spec.Replicas)
	require.Equal(t, corev1.RestartPolicyAlways, deployment.Spec.Template.Spec.RestartPolicy)
	require.Nil(t, deployment.Spec.Template.Spec.ActiveDeadlineSeconds)
	require.Len(t, deployment.Spec.Template.Spec.InitContainers, 1)
	require.True(t, replicaRunDeadline(r, deployment.Spec.Template.Spec))
	content, err := json.Marshal(deployment)
	require.NoError(t, err)
	require.NotContains(t, string(content), "activeDeadlineSeconds")
	// This is the production renderer, not server validation. The parent can
	// submit this logged zero-replica object using --dry-run=server.
	t.Logf("APPLICATION_REPLICA_DEPLOYMENT=%s", content)
}

func TestReplicaLedgerRetainsAbsoluteDeadlineWithoutPodDeadline(t *testing.T) {
	f := newFixture(t)
	r := f.newRun()
	require.NoError(t, f.backend.ensureClaim(t.Context(), r))
	require.NoError(t, f.backend.ensureNamespace(t.Context(), r, pv.Original))
	deployment := renderedReplicaFixture(t, f, r)
	deadline := r.state.Deadline
	key := resourceKey(pv.Original, "replica-deployment")
	created, err := f.backend.ensureObject(t.Context(), r, key, deployment)
	require.NoError(t, err)
	require.NotEmpty(t, created.GetUID())
	require.Zero(t, r.effect(key).DeadlineSeconds)
	reloaded, err := f.backend.load(t.Context(), r.object.Name)
	require.NoError(t, err)
	require.Equal(t, deadline, reloaded.state.Deadline)
	require.Zero(t, reloaded.effect(key).DeadlineSeconds)
	resumed := renderedReplicaFixture(t, f, reloaded)
	require.Equal(t, deployment.Spec.Template, resumed.Spec.Template)
	again, err := f.backend.ensureObject(t.Context(), reloaded, key, resumed)
	require.NoError(t, err)
	require.Equal(t, created.GetUID(), again.GetUID())
}

func TestReplicaDeadlineMissingExpiredOrChangedCannotCreateEffect(t *testing.T) {
	for _, change := range []string{"missing", "expired", "extended", "shortened", "duplicate", "projected", "pod-field"} {
		t.Run(change, func(t *testing.T) {
			f := newFixture(t)
			r := f.newRun()
			deployment := renderedReplicaFixture(t, f, r)
			spec := &deployment.Spec.Template.Spec
			environment := &spec.Containers[0].Env
			for index := range *environment {
				variable := &(*environment)[index]
				if variable.Name != "ORKA_APPLICATION_DEADLINE" {
					continue
				}
				switch change {
				case "missing":
					*environment = append((*environment)[:index], (*environment)[index+1:]...)
				case "expired":
					variable.Value = time.Now().Add(-time.Minute).Format(time.RFC3339Nano)
				case "extended":
					variable.Value = r.state.Deadline.Add(time.Minute).Format(time.RFC3339Nano)
				case "shortened":
					variable.Value = r.state.Deadline.Add(-time.Second).Format(time.RFC3339Nano)
				case "duplicate":
					*environment = append(*environment, *variable)
				case "projected":
					variable.ValueFrom = &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}
				case "pod-field":
					spec.ActiveDeadlineSeconds = new(int64(60))
				}
				break
			}
			before := len(f.created)
			_, err := f.backend.ensureObject(t.Context(), r, resourceKey(pv.Original, "replica-deployment"), deployment)
			require.ErrorIs(t, err, contract.ErrConflict)
			require.Len(t, f.created, before)
			require.Nil(t, r.effect(resourceKey(pv.Original, "replica-deployment")))
		})
	}
}

func TestReplicaDescendantDeadlineMustMatchLiveTemplate(t *testing.T) {
	f := newFixture(t)
	r := f.newRun()
	template := renderedReplicaFixture(t, f, r).Spec.Template.Spec
	for index, change := range []func(*corev1.PodSpec){
		func(spec *corev1.PodSpec) { spec.Containers[0].Env = nil },
		func(spec *corev1.PodSpec) { spec.ActiveDeadlineSeconds = new(int64(60)) },
		func(spec *corev1.PodSpec) {
			for i := range spec.Containers[0].Env {
				if spec.Containers[0].Env[i].Name == "ORKA_APPLICATION_DEADLINE" {
					spec.Containers[0].Env[i].Value = time.Now().Add(-time.Minute).Format(time.RFC3339Nano)
				}
			}

		},
	} {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			changed := *template.DeepCopy()
			change(&changed)
			require.False(t, f.backend.replicaPodMatches(template, changed))
			require.False(t, f.backend.replicaPodMatches(changed, changed))
		})
	}
}

func TestReplicaIdentityCannotInheritClusterAuthority(t *testing.T) {
	f := newFixture(t)
	r := f.newRun()
	role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "unapproved-replica-access", UID: "unapproved-role"},
		Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"create"}}}}
	require.NoError(t, f.target.Create(t.Context(), role))
	require.NoError(t, f.target.Create(t.Context(), &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: role.Name, UID: "unapproved-binding"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
		Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Namespace: namespaceName(r, pv.Original),
			Name: "orka-replica"}}}))
	require.ErrorIs(t, f.backend.auditAuthorization(t.Context(), r), ErrIsolation)
}
