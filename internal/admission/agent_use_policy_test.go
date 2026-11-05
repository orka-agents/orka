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
	"sigs.k8s.io/yaml"
)

// agentUseGrants allows `use` only on the listed namespace/name Agents and
// records every check the policy makes.
type agentUseGrants struct {
	allowed map[string]bool
	checked []string
}

func (g *agentUseGrants) Authorize(_ context.Context, attributes authorizer.Attributes) (authorizer.Decision, string, error) {
	key := attributes.GetNamespace() + "/" + attributes.GetName()
	g.checked = append(g.checked, key)
	if attributes.GetVerb() == "use" && attributes.GetAPIGroup() == "core.orka.ai" &&
		attributes.GetResource() == "agents" && g.allowed[key] {
		return authorizer.DecisionAllow, "", nil
	}
	return authorizer.DecisionNoOpinion, "", nil
}

func loadAgentUsePolicies(t *testing.T) map[string]*admissionregistrationv1.ValidatingAdmissionPolicy {
	t.Helper()
	documents, err := splitYAMLDocuments(filepath.Join("..", "..", "config", "agent-use-policy", "agent_use_policy.yaml"))
	require.NoError(t, err)
	policies := map[string]*admissionregistrationv1.ValidatingAdmissionPolicy{}
	bindings := map[string]bool{}
	for _, document := range documents {
		var meta metav1.TypeMeta
		require.NoError(t, yaml.Unmarshal(document, &meta))
		switch meta.Kind {
		case "ValidatingAdmissionPolicy":
			policy := &admissionregistrationv1.ValidatingAdmissionPolicy{}
			require.NoError(t, yaml.UnmarshalStrict(document, policy))
			policies[policy.Spec.MatchConstraints.ResourceRules[0].Resources[0]] = policy
		case "ValidatingAdmissionPolicyBinding":
			binding := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
			require.NoError(t, yaml.UnmarshalStrict(document, binding))
			require.Equal(t, []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny}, binding.Spec.ValidationActions)
			bindings[binding.Spec.PolicyName] = true
		}
	}
	for _, policy := range policies {
		require.True(t, bindings[policy.Name], "policy %s has no Deny binding", policy.Name)
		require.Equal(t, admissionregistrationv1.Fail, *policy.Spec.FailurePolicy)
	}
	return policies
}

// admitsAgentUse evaluates a shipped policy as kube-apiserver does.
func admitsAgentUse(
	t *testing.T,
	policy *admissionregistrationv1.ValidatingAdmissionPolicy,
	gvk schema.GroupVersionKind,
	resource string,
	grants *agentUseGrants,
	object, oldObject map[string]any,
	override ...authorizer.UnconditionalAuthorizer,
) bool {
	t.Helper()
	compiler, err := admissioncel.NewCompositedCompiler(environment.MustBaseEnvSet(environment.DefaultCompatibilityVersion()))
	require.NoError(t, err)
	expressions := make([]admissioncel.ExpressionAccessor, 0, len(policy.Spec.Validations))
	for _, validation := range policy.Spec.Validations {
		expressions = append(expressions, &validating.ValidationCondition{Expression: validation.Expression})
	}
	validations := compiler.CompileCondition(expressions,
		admissioncel.OptionalVariableDeclarations{HasAuthorizer: true}, environment.NewExpressions)
	require.Empty(t, validations.CompilationErrors())

	gvr := gvk.GroupVersion().WithResource(resource)
	current := &unstructured.Unstructured{Object: object}
	operation := apiserveradmission.Create
	var previous runtime.Object
	if oldObject != nil {
		operation = apiserveradmission.Update
		previous = &unstructured.Unstructured{Object: oldObject}
	}
	attributes := apiserveradmission.NewAttributesRecord(current, previous, gvk, admissionTestNamespace, current.GetName(),
		gvr, "", operation, nil, false, &user.DefaultInfo{Name: "alice@example.com"})
	versioned := &apiserveradmission.VersionedAttributes{
		Attributes: attributes, VersionedKind: gvk,
		VersionedObject:    apiserveradmission.NewLazyObject(current),
		VersionedOldObject: apiserveradmission.NewLazyObject(previous),
	}
	request := admissioncel.CreateAdmissionRequest(attributes, metav1.GroupVersionResource(gvr), metav1.GroupVersionKind(gvk))
	var authz authorizer.UnconditionalAuthorizer = grants
	if len(override) > 0 {
		authz = override[0]
	}
	results, _, err := validations.ForInput(compiler.CreateContext(context.Background()), versioned, request,
		admissioncel.OptionalVariableBindings{Authorizer: authz}, nil, celconfig.RuntimeCELCostBudget)
	require.NoError(t, err)
	for _, result := range results {
		require.NoError(t, result.Error)
		if result.EvalResult.Value() != true {
			return false
		}
	}
	return true
}

func agentUseObject(kind, apiVersion string, spec map[string]any) map[string]any {
	return map[string]any{
		"apiVersion": apiVersion, "kind": kind,
		"metadata": map[string]any{"name": "work", "namespace": admissionTestNamespace},
		"spec":     spec,
	}
}

func TestAgentUsePolicyTasks(t *testing.T) {
	policy := loadAgentUsePolicies(t)["tasks"]
	require.NotNil(t, policy)
	gvk := schema.GroupVersionKind{Group: "core.orka.ai", Version: "v1alpha1", Kind: "Task"}
	task := func(agentRef map[string]any) map[string]any {
		spec := map[string]any{"type": "agent", "prompt": "hi"}
		if agentRef != nil {
			spec["agentRef"] = agentRef
		}
		return agentUseObject("Task", "core.orka.ai/v1alpha1", spec)
	}
	local := map[string]any{"name": "reviewer"}

	granted := &agentUseGrants{allowed: map[string]bool{admissionTestNamespace + "/reviewer": true}}
	require.True(t, admitsAgentUse(t, policy, gvk, "tasks", granted, task(local), nil))

	denied := &agentUseGrants{}
	require.False(t, admitsAgentUse(t, policy, gvk, "tasks", denied, task(local), nil), "creating a Task needs use on its Agent")
	require.Equal(t, []string{admissionTestNamespace + "/reviewer"}, denied.checked)

	require.True(t, admitsAgentUse(t, policy, gvk, "tasks", &agentUseGrants{}, task(nil), nil), "a Task without an Agent needs no grant")

	unchanged := &agentUseGrants{}
	require.True(t, admitsAgentUse(t, policy, gvk, "tasks", unchanged, task(local), task(local)),
		"an edit that keeps the Agent needs no new grant")
	require.Empty(t, unchanged.checked)

	remote := &agentUseGrants{allowed: map[string]bool{"agents-team/reviewer": true}}
	require.True(t, admitsAgentUse(t, policy, gvk, "tasks", remote,
		task(map[string]any{"name": "reviewer", "namespace": "agents-team"}), nil),
		"a cross-namespace Agent is checked in its own namespace")
	require.Equal(t, []string{"agents-team/reviewer"}, remote.checked)
}

func TestAgentUsePolicyRepositoryMonitorChecksOnlyChangedRoles(t *testing.T) {
	policy := loadAgentUsePolicies(t)["repositorymonitors"]
	require.NotNil(t, policy)
	gvk := schema.GroupVersionKind{Group: "core.orka.ai", Version: "v1alpha1", Kind: "RepositoryMonitor"}
	monitor := func(agents map[string]any) map[string]any {
		return agentUseObject("RepositoryMonitor", "core.orka.ai/v1alpha1",
			map[string]any{"repoURL": "https://github.com/example/repo", "agents": agents})
	}
	before := monitor(map[string]any{"reviewer": map[string]any{"name": "reviewer"}})
	after := monitor(map[string]any{
		"reviewer": map[string]any{"name": "reviewer"},
		"repairer": map[string]any{"name": "repairer"},
	})

	grants := &agentUseGrants{allowed: map[string]bool{admissionTestNamespace + "/repairer": true}}
	require.True(t, admitsAgentUse(t, policy, gvk, "repositorymonitors", grants, after, before))
	require.Equal(t, []string{admissionTestNamespace + "/repairer"}, grants.checked, "only the added role is checked")

	require.False(t, admitsAgentUse(t, policy, gvk, "repositorymonitors", &agentUseGrants{}, after, before))
	require.True(t, admitsAgentUse(t, policy, gvk, "repositorymonitors", &agentUseGrants{}, monitor(map[string]any{}), nil))
}

func TestAgentUsePolicyRepositoryScanAndGatewayBinding(t *testing.T) {
	policies := loadAgentUsePolicies(t)
	scanKind := schema.GroupVersionKind{Group: "core.orka.ai", Version: "v1alpha1", Kind: "RepositoryScan"}
	scan := agentUseObject("RepositoryScan", "core.orka.ai/v1alpha1", map[string]any{
		"repoURL":          "https://github.com/example/repo",
		"analysisAgentRef": map[string]any{"name": "analyst"},
		"patchAgentRef":    map[string]any{"name": "patcher"},
	})
	analystOnly := &agentUseGrants{allowed: map[string]bool{admissionTestNamespace + "/analyst": true}}
	require.False(t, admitsAgentUse(t, policies["repositoryscans"], scanKind, "repositoryscans", analystOnly, scan, nil),
		"the patch Agent needs its own grant")
	both := &agentUseGrants{allowed: map[string]bool{
		admissionTestNamespace + "/analyst": true, admissionTestNamespace + "/patcher": true,
	}}
	require.True(t, admitsAgentUse(t, policies["repositoryscans"], scanKind, "repositoryscans", both, scan, nil))

	bindingKind := schema.GroupVersionKind{Group: "gateway.orka.ai", Version: "v1alpha1", Kind: "GatewayBinding"}
	binding := agentUseObject("GatewayBinding", "gateway.orka.ai/v1alpha1", map[string]any{
		"agentRef": map[string]any{"name": "support"},
	})
	require.False(t, admitsAgentUse(t, policies["gatewaybindings"], bindingKind, "gatewaybindings", &agentUseGrants{}, binding, nil))
	support := &agentUseGrants{allowed: map[string]bool{admissionTestNamespace + "/support": true}}
	require.True(t, admitsAgentUse(t, policies["gatewaybindings"], bindingKind, "gatewaybindings", support, binding, nil))
}
