/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package admission

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	apiserveradmission "k8s.io/apiserver/pkg/admission"
	admissioncel "k8s.io/apiserver/pkg/admission/plugin/cel"
	"k8s.io/apiserver/pkg/admission/plugin/policy/validating"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	"k8s.io/apiserver/pkg/cel/environment"
)

const (
	secretUseTestUser       = "alice@example.com"
	secretUseControllerUser = "system:serviceaccount:ORKA_NAMESPACE:CONTROLLER_SA"
	secretUseAIWorkerUser   = "system:serviceaccount:" + admissionTestNamespace + ":orka-ai-worker"
)

// secretUseGrants allows `use` only on the listed Secrets for the listed
// users, and records every check the policy makes.
type secretUseGrants struct {
	allowed map[string]map[string]bool
	checked []string
}

func (g *secretUseGrants) Authorize(_ context.Context, attributes authorizer.Attributes) (authorizer.Decision, string, error) {
	key := attributes.GetNamespace() + "/" + attributes.GetName()
	g.checked = append(g.checked, key)
	if attributes.GetVerb() == "use" && attributes.GetAPIGroup() == "" && attributes.GetResource() == "secrets" &&
		g.allowed[attributes.GetUser().GetName()][key] {
		return authorizer.DecisionAllow, "", nil
	}
	return authorizer.DecisionNoOpinion, "", nil
}

func grantSecretUse(username string, secrets ...string) *secretUseGrants {
	grants := &secretUseGrants{allowed: map[string]map[string]bool{username: {}}}
	for _, name := range secrets {
		grants.allowed[username][admissionTestNamespace+"/"+name] = true
	}
	return grants
}

func loadContainerTaskSecretUsePolicy(t *testing.T) *admissionregistrationv1.ValidatingAdmissionPolicy {
	t.Helper()
	policies := loadUsePolicies(t, filepath.Join("..", "..", "config", "container-task-secret-use-policy", "container_task_secret_use_policy.yaml"))
	require.Len(t, policies, 1)
	policy := policies["tasks"]
	require.NotNil(t, policy)
	return policy
}

// admitsSecretUse evaluates the policy, match conditions included, as
// kube-apiserver does for a write by username.
func admitsSecretUse(
	t *testing.T,
	policy *admissionregistrationv1.ValidatingAdmissionPolicy,
	username string,
	grants *secretUseGrants,
	object, oldObject map[string]any,
) bool {
	t.Helper()
	gvk := schema.GroupVersionKind{Group: "core.orka.ai", Version: "v1alpha1", Kind: "Task"}
	compiler, err := admissioncel.NewCompositedCompiler(environment.MustBaseEnvSet(environment.DefaultCompatibilityVersion()))
	require.NoError(t, err)
	options := admissioncel.OptionalVariableDeclarations{HasAuthorizer: true}
	for _, variable := range policy.Spec.Variables {
		compiled := compiler.CompileAndStoreVariable(&validating.Variable{Name: variable.Name, Expression: variable.Expression},
			options, environment.NewExpressions)
		require.Nil(t, compiled.Error, "variable %s", variable.Name)
	}
	compile := func(expressions []string) admissioncel.ConditionEvaluator {
		accessors := make([]admissioncel.ExpressionAccessor, 0, len(expressions))
		for _, expression := range expressions {
			accessors = append(accessors, &validating.ValidationCondition{Expression: expression})
		}
		evaluator := compiler.CompileCondition(accessors, options, environment.NewExpressions)
		require.Empty(t, evaluator.CompilationErrors())
		return evaluator
	}
	matchExpressions := make([]string, 0, len(policy.Spec.MatchConditions))
	validationExpressions := make([]string, 0, len(policy.Spec.Validations))
	for _, condition := range policy.Spec.MatchConditions {
		matchExpressions = append(matchExpressions, condition.Expression)
	}
	for _, validation := range policy.Spec.Validations {
		validationExpressions = append(validationExpressions, validation.Expression)
	}

	gvr := gvk.GroupVersion().WithResource("tasks")
	current := &unstructured.Unstructured{Object: object}
	operation := apiserveradmission.Create
	var previous runtime.Object
	if oldObject != nil {
		operation = apiserveradmission.Update
		previous = &unstructured.Unstructured{Object: oldObject}
	}
	attributes := apiserveradmission.NewAttributesRecord(current, previous, gvk, admissionTestNamespace, current.GetName(),
		gvr, "", operation, nil, false, &user.DefaultInfo{Name: username})
	versioned := &apiserveradmission.VersionedAttributes{
		Attributes: attributes, VersionedKind: gvk,
		VersionedObject:    apiserveradmission.NewLazyObject(current),
		VersionedOldObject: apiserveradmission.NewLazyObject(previous),
	}
	request := admissioncel.CreateAdmissionRequest(attributes, metav1.GroupVersionResource(gvr), metav1.GroupVersionKind(gvk))
	evaluate := func(evaluator admissioncel.ConditionEvaluator) bool {
		results, _, err := evaluator.ForInput(compiler.CreateContext(context.Background()), versioned, request,
			admissioncel.OptionalVariableBindings{Authorizer: grants}, nil, celconfig.RuntimeCELCostBudget)
		require.NoError(t, err)
		for _, result := range results {
			require.NoError(t, result.Error)
			if result.EvalResult.Value() != true {
				return false
			}
		}
		return true
	}
	if !evaluate(compile(matchExpressions)) {
		return true
	}
	return evaluate(compile(validationExpressions))
}

func secretUseTask(taskType string, secretRef map[string]any, env []any, objectLabels map[string]any) map[string]any {
	spec := map[string]any{"type": taskType, "image": "busybox"}
	if secretRef != nil {
		spec["secretRef"] = secretRef
	}
	if env != nil {
		spec["env"] = env
	}
	metadata := map[string]any{"name": "nightly", "namespace": admissionTestNamespace}
	if objectLabels != nil {
		metadata["labels"] = objectLabels
	}
	return map[string]any{"apiVersion": "core.orka.ai/v1alpha1", "kind": "Task", "metadata": metadata, "spec": spec}
}

func secretKeyEnv(name, secret, key string) map[string]any {
	return map[string]any{"name": name, "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": secret, "key": key}}}
}

func TestContainerTaskSecretUsePolicyRequiresUseOnEverySecret(t *testing.T) {
	policy := loadContainerTaskSecretUsePolicy(t)
	bot := map[string]any{"name": "bot-key"}
	env := []any{
		map[string]any{"name": "PLAIN", "value": "x"},
		secretKeyEnv("API_TOKEN", "api-token", "token"),
		map[string]any{"name": "POD", "valueFrom": map[string]any{"fieldRef": map[string]any{"fieldPath": "metadata.name"}}},
	}

	require.True(t, admitsSecretUse(t, policy, secretUseTestUser, grantSecretUse(secretUseTestUser, "bot-key", "api-token"),
		secretUseTask("container", bot, env, nil), nil))

	partial := grantSecretUse(secretUseTestUser, "bot-key")
	require.False(t, admitsSecretUse(t, policy, secretUseTestUser, partial, secretUseTask("container", bot, env, nil), nil),
		"use on one Secret does not cover an env Secret")
	require.Contains(t, partial.checked, admissionTestNamespace+"/api-token")

	require.False(t, admitsSecretUse(t, policy, secretUseTestUser, grantSecretUse(secretUseTestUser),
		secretUseTask("container", bot, nil, nil), nil), "secretRef needs use")
	require.False(t, admitsSecretUse(t, policy, secretUseTestUser, grantSecretUse(secretUseTestUser),
		secretUseTask("container", nil, []any{secretKeyEnv("A", "api-token", "token")}, nil), nil), "env secretKeyRef needs use")

	none := grantSecretUse(secretUseTestUser)
	require.True(t, admitsSecretUse(t, policy, secretUseTestUser, none,
		secretUseTask("container", nil, []any{map[string]any{"name": "PLAIN", "value": "x"}}, nil), nil),
		"a container Task without Secrets needs no grant")
	require.Empty(t, none.checked)
}

func TestContainerTaskSecretUsePolicyAppliesOnlyToContainerTasks(t *testing.T) {
	policy := loadContainerTaskSecretUsePolicy(t)
	grants := grantSecretUse(secretUseTestUser)
	require.True(t, admitsSecretUse(t, policy, secretUseTestUser, grants,
		secretUseTask("ai", map[string]any{"name": "bot-key"}, nil, nil), nil), "other Task types keep their own Secret rules")
	require.Empty(t, grants.checked)
}

func TestContainerTaskSecretUsePolicyRefusesOtherNamespaces(t *testing.T) {
	policy := loadContainerTaskSecretUsePolicy(t)
	grants := grantSecretUse(secretUseTestUser, "bot-key")
	require.False(t, admitsSecretUse(t, policy, secretUseTestUser, grants,
		secretUseTask("container", map[string]any{"name": "bot-key", "namespace": "elsewhere"}, nil, nil), nil))
	require.True(t, admitsSecretUse(t, policy, secretUseTestUser, grants,
		secretUseTask("container", map[string]any{"name": "bot-key", "namespace": admissionTestNamespace}, nil, nil), nil))
	require.True(t, admitsSecretUse(t, policy, secretUseTestUser, grants,
		secretUseTask("container", map[string]any{"name": "bot-key", "namespace": ""}, nil, nil), nil))
}

func TestContainerTaskSecretUsePolicyRefusesOrkaIdentities(t *testing.T) {
	policy := loadContainerTaskSecretUsePolicy(t)
	task := secretUseTask("container", map[string]any{"name": "bot-key"}, nil, nil)
	require.False(t, admitsSecretUse(t, policy, secretUseAIWorkerUser, grantSecretUse(secretUseTestUser, "bot-key"), task, nil),
		"a Task an agent writes with its worker identity is refused")
	require.False(t, admitsSecretUse(t, policy, secretUseControllerUser, grantSecretUse(secretUseTestUser, "bot-key"), task, nil),
		"a Task the controller writes for the API, chat, or tools is refused")
}

func TestContainerTaskSecretUsePolicyAdmitsControllerScheduledRuns(t *testing.T) {
	policy := loadContainerTaskSecretUsePolicy(t)
	run := secretUseTask("container", map[string]any{"name": "bot-key"}, nil, map[string]any{"orka.ai/scheduled-run": "true"})
	require.True(t, admitsSecretUse(t, policy, secretUseControllerUser, grantSecretUse(secretUseTestUser), run, nil),
		"the controller writes scheduled runs; the job builder checks them against their scheduled Task")
	require.False(t, admitsSecretUse(t, policy, secretUseTestUser, grantSecretUse(secretUseTestUser), run, nil),
		"only the controller may write a run that skips the check")
	require.False(t, admitsSecretUse(t, policy, secretUseAIWorkerUser, grantSecretUse(secretUseTestUser), run, nil))
}

func TestContainerTaskSecretUsePolicyChecksEverySpecChange(t *testing.T) {
	policy := loadContainerTaskSecretUsePolicy(t)
	old := secretUseTask("container", map[string]any{"name": "bot-key"}, nil, nil)

	relabelled := secretUseTask("container", map[string]any{"name": "bot-key"}, nil, map[string]any{"example": "relabel"})
	unchanged := grantSecretUse(secretUseControllerUser)
	require.True(t, admitsSecretUse(t, policy, secretUseControllerUser, unchanged, relabelled, old),
		"a metadata-only update, such as the controller's own, needs no grant")
	require.Empty(t, unchanged.checked)

	reimaged := secretUseTask("container", map[string]any{"name": "bot-key"}, nil, nil)
	reimaged["spec"].(map[string]any)["image"] = "attacker/image"
	require.False(t, admitsSecretUse(t, policy, secretUseTestUser, grantSecretUse(secretUseTestUser), reimaged, old),
		"changing the code that would receive the Secret needs use, even with the same Secrets")
	require.True(t, admitsSecretUse(t, policy, secretUseTestUser, grantSecretUse(secretUseTestUser, "bot-key"), reimaged, old))

	added := secretUseTask("container", map[string]any{"name": "bot-key"}, []any{secretKeyEnv("B", "other", "k")}, nil)
	require.False(t, admitsSecretUse(t, policy, secretUseTestUser, grantSecretUse(secretUseTestUser, "bot-key"), added, old),
		"an update that adds a Secret needs use on it")

	run := secretUseTask("container", map[string]any{"name": "bot-key"}, nil, map[string]any{"orka.ai/scheduled-run": "true"})
	reimagedRun := secretUseTask("container", map[string]any{"name": "bot-key"}, nil, map[string]any{"orka.ai/scheduled-run": "true"})
	reimagedRun["spec"].(map[string]any)["image"] = "attacker/image"
	require.False(t, admitsSecretUse(t, policy, secretUseControllerUser, grantSecretUse(secretUseTestUser), reimagedRun, run),
		"the scheduled-run exemption covers only the controller creating a run")

	agentTask := secretUseTask("ai", map[string]any{"name": "bot-key"}, nil, nil)
	require.False(t, admitsSecretUse(t, policy, secretUseTestUser, grantSecretUse(secretUseTestUser), old, agentTask),
		"turning another Task type into a container Task that names Secrets needs use")
}

func TestContainerTaskSecretUsePolicyProtectsProvenance(t *testing.T) {
	policy := loadContainerTaskSecretUsePolicy(t)
	run := secretUseTask("container", map[string]any{"name": "bot-key"}, nil, map[string]any{"orka.ai/scheduled-run": "true"})
	root := secretUseTask("container", map[string]any{"name": "bot-key"}, nil, nil)

	require.False(t, admitsSecretUse(t, policy, secretUseTestUser, grantSecretUse(secretUseTestUser), root, run),
		"dropping the scheduled-run label would change which creation time the controller checks")
	require.False(t, admitsSecretUse(t, policy, secretUseTestUser, grantSecretUse(secretUseTestUser), run, root),
		"adding the scheduled-run label would let a Task be judged by another Task")
	withAnnotation := secretUseTask("container", map[string]any{"name": "bot-key"}, nil, nil)
	withAnnotation["metadata"].(map[string]any)["annotations"] = map[string]any{"orka.ai/requested-by-source": "api"}
	require.False(t, admitsSecretUse(t, policy, secretUseTestUser, grantSecretUse(secretUseTestUser), root, withAnnotation),
		"removing API provenance needs use")

	relabelled := secretUseTask("container", map[string]any{"name": "bot-key"}, nil,
		map[string]any{"orka.ai/scheduled-run": "true", "example": "relabel"})
	require.True(t, admitsSecretUse(t, policy, secretUseControllerUser, grantSecretUse(secretUseTestUser), relabelled, run),
		"other metadata stays free to change")
	require.True(t, admitsSecretUse(t, policy, secretUseTestUser, grantSecretUse(secretUseTestUser, "bot-key"), root, run),
		"a holder of use may change provenance")
}

func TestContainerTaskSecretUsePolicyProtectsOwners(t *testing.T) {
	policy := loadContainerTaskSecretUsePolicy(t)
	owned := func(owner string) map[string]any {
		task := secretUseTask("container", map[string]any{"name": "bot-key"}, nil, map[string]any{"orka.ai/scheduled-run": "true"})
		task["metadata"].(map[string]any)["ownerReferences"] = []any{map[string]any{
			"apiVersion": "core.orka.ai/v1alpha1", "kind": "Task", "name": owner, "uid": owner + "-uid", "controller": true,
		}}
		return task
	}
	require.False(t, admitsSecretUse(t, policy, secretUseTestUser, grantSecretUse(secretUseTestUser), owned("other"), owned("nightly")),
		"re-pointing a run at another scheduled Task needs use")
	require.True(t, admitsSecretUse(t, policy, secretUseControllerUser, grantSecretUse(secretUseTestUser), owned("nightly"), owned("nightly")))
}
