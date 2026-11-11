package applicationexec

import (
	"strings"
	"testing"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestScalingAdmissionPinsExactNamespaceDeploymentUIDAndBounds(t *testing.T) {
	f := newFixture(t)
	r := f.newRun()
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "replica-target",
		Namespace: namespaceName(r, pv.Original), UID: "deployment-uid"}}
	policies, bindings := scalingPolicies(r, pv.Original, deployment, 4)
	require.Len(t, policies, 2)
	require.Len(t, bindings, 2)
	for index, policy := range policies {
		require.Equal(t, admissionv1.Fail, *policy.Spec.FailurePolicy)
		require.Equal(t, "apps/v1", policy.Spec.ParamKind.APIVersion)
		require.Equal(t, "Deployment", policy.Spec.ParamKind.Kind)
		require.Equal(t, deployment.Namespace, policy.Spec.MatchConstraints.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"])
		require.Equal(t, string(r.object.UID), policy.Spec.MatchConstraints.NamespaceSelector.MatchLabels[runLabel])
		require.Equal(t, admissionv1.Equivalent, *policy.Spec.MatchConstraints.MatchPolicy)
		require.Len(t, policy.Spec.Validations, 1)
		expression := policy.Spec.Validations[0].Expression
		require.Contains(t, expression, "deployment-uid")
		require.Contains(t, expression, "replica-target")
		require.Contains(t, expression, deployment.Namespace)
		require.Contains(t, expression, "<= 4")
		for _, rule := range policy.Spec.MatchConstraints.ResourceRules {
			for _, name := range append(append([]string{}, rule.APIGroups...), rule.Resources...) {
				require.NotContains(t, name, "*")
			}
		}
		require.Equal(t, []admissionv1.ValidationAction{admissionv1.Deny}, bindings[index].Spec.ValidationActions)
		require.Equal(t, policy.Name, bindings[index].Spec.PolicyName)
		require.Equal(t, deployment.Namespace, bindings[index].Spec.ParamRef.Namespace)
		require.Equal(t, deployment.Name, bindings[index].Spec.ParamRef.Name)
		require.Equal(t, admissionv1.DenyAction, *bindings[index].Spec.ParamRef.ParameterNotFoundAction)
		require.Equal(t, admissionv1.Equivalent, *bindings[index].Spec.MatchResources.MatchPolicy)
	}
	require.Contains(t, policies[0].Spec.Validations[0].Expression, "scaleTargetRef.kind == 'Deployment'")
	require.Contains(t, policies[1].Spec.Validations[0].Expression, "object.spec.replicas >= 0")
	require.Empty(t, policies[0].OwnerReferences, "cross-cluster owner references cannot be used for admission cleanup")
}

func TestReplicaProfileHasNoModelTemplateOrCredentialSurface(t *testing.T) {
	profile := pv.ApplicationFixtureProfile{ID: "bounded", Image: "example/fixture@" + pv.Digest([]byte("image")),
		ImageID: pv.Digest([]byte("resolved")), Args: []string{"/usr/bin/sleep", "3600"},
		ReplicaTarget: &pv.ApplicationReplicaTargetProfile{Name: "target", InitialReplicas: 0, MaxReplicas: 4}}
	policy := &pv.ApplicationPolicy{Version: pv.ApplicationProtocolVersion, Fixtures: []pv.ApplicationFixtureProfile{profile}}
	plan := &pv.ApplicationPlan{Fixtures: []pv.ApplicationFixture{{ID: "target", Kind: "replica", Profile: "bounded"}}}
	fixture, admitted, err := replicaProfile(plan, policy)
	require.NoError(t, err)
	require.NotNil(t, fixture)
	require.Equal(t, int32(0), admitted.ReplicaTarget.InitialReplicas)
	for _, change := range []func(*pv.ApplicationFixtureProfile){
		func(value *pv.ApplicationFixtureProfile) { value.ReplicaTarget.MaxReplicas = 5 },
		func(value *pv.ApplicationFixtureProfile) { value.ReplicaTarget.Name = "another-target" },
		func(value *pv.ApplicationFixtureProfile) { value.ConfigKeys = []string{"script"} },
		func(value *pv.ApplicationFixtureProfile) {
			value.SecretFiles = []pv.ApplicationFixtureSecretFile{{ID: "key", SecretName: "secret", Key: "value"}}
		},
	} {
		current := profile
		current.ReplicaTarget = &pv.ApplicationReplicaTargetProfile{Name: "target", InitialReplicas: 0, MaxReplicas: 4}
		change(&current)
		_, _, err := replicaProfile(plan, &pv.ApplicationPolicy{Fixtures: []pv.ApplicationFixtureProfile{current}})
		require.ErrorIs(t, err, contract.ErrInvalid)
	}
}

func TestScalingAuthorityWithheldFromInitialNamespaceRole(t *testing.T) {
	f := newFixture(t)
	installPolicyFixture(t, f)
	plan := f.request.Checks.Application
	plan.Permissions = append(plan.Permissions,
		pv.ApplicationPermission{APIGroup: "apps", Resources: []string{"deployments/scale"}, Verbs: []string{"get", "patch", "update"}},
		pv.ApplicationPermission{APIGroup: "autoscaling", Resources: []string{"horizontalpodautoscalers"}, Verbs: []string{"get", "list", "watch", "create", "update"}})
	rules, err := subjectRules(plan, f.request.Checks.Checks, f.request.ApplicationPolicy)
	require.NoError(t, err)
	for _, rule := range rules {
		for _, resource := range rule.Resources {
			require.False(t, strings.Contains(resource, "scale") || resource == "horizontalpodautoscalers")
		}
	}
}

func TestReplicaPodTemplateCannotGainPrivilegeOrChangeImage(t *testing.T) {
	f := newFixture(t)
	r := f.newRun()
	profile := &pv.ApplicationFixtureProfile{ReplicaTarget: &pv.ApplicationReplicaTargetProfile{Name: "target", InitialReplicas: 0, MaxReplicas: 4}}
	job, err := f.backend.job(r, pv.Original, "target", jobOptions{role: "replica", mode: "fixture",
		image: f.config.WorkerImage, taskUID: string(r.state.Task.UID), serviceAccount: "replica", fixtureProfile: profile})
	require.NoError(t, err)
	spec := *job.Spec.Template.Spec.DeepCopy()
	spec.RestartPolicy = "Always"
	require.Nil(t, spec.ActiveDeadlineSeconds)
	require.True(t, f.backend.replicaPodMatches(spec, spec))
	changed := *spec.DeepCopy()
	changed.Containers[0].Image = "unadmitted/image:latest"
	require.False(t, f.backend.replicaPodMatches(spec, changed))
	changed = *spec.DeepCopy()
	changed.Containers[0].SecurityContext.Privileged = new(true)
	require.False(t, f.backend.replicaPodMatches(spec, changed))
	changed = *spec.DeepCopy()
	changed.ServiceAccountName = "orka-subject"
	require.False(t, f.backend.replicaPodMatches(spec, changed))
}

func TestScalingRulesPreserveFrozenHPANames(t *testing.T) {
	for _, names := range [][]string{nil, {"only-hpa", "second-hpa"}} {
		plan := &pv.ApplicationPlan{Permissions: []pv.ApplicationPermission{
			{APIGroup: "autoscaling", Resources: []string{"horizontalpodautoscalers"}, Verbs: []string{"get", "update"}, ResourceNames: names},
			{APIGroup: "apps", Resources: []string{"deployments/scale"}, Verbs: []string{"patch"}},
		}}
		rules, err := scalingRules(plan, "replica-target")
		require.NoError(t, err)
		require.Len(t, rules, 3)
		require.Equal(t, names, rules[0].ResourceNames)
		require.Equal(t, names, rules[1].ResourceNames)
		require.Equal(t, []string{"replica-target"}, rules[2].ResourceNames)
		if len(names) != 0 {
			names[0] = "changed-after-render"
			require.Equal(t, "only-hpa", rules[0].ResourceNames[0])
		}
	}
	plan := &pv.ApplicationPlan{Permissions: []pv.ApplicationPermission{
		{APIGroup: "apps", Resources: []string{"deployments/scale"}, Verbs: []string{"update"}, ResourceNames: []string{"other-deployment"}},
	}}
	_, err := scalingRules(plan, "replica-target")
	require.ErrorIs(t, err, contract.ErrInvalid)
}
