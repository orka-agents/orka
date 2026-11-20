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
	apiserveradmission "k8s.io/apiserver/pkg/admission"
	admissioncel "k8s.io/apiserver/pkg/admission/plugin/cel"
	"k8s.io/apiserver/pkg/admission/plugin/policy/validating"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	"k8s.io/apiserver/pkg/cel/environment"
	"sigs.k8s.io/yaml"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// fixedAuthorizer answers every check the same way.
type fixedAuthorizer bool

func (a fixedAuthorizer) Authorize(context.Context, authorizer.Attributes) (authorizer.Decision, string, error) {
	if a {
		return authorizer.DecisionAllow, "", nil
	}
	return authorizer.DecisionNoOpinion, "", nil
}

// The shipped AgentHost policy, evaluated as kube-apiserver does: only the
// controller creates a host fed by a Connection, and such a host needs no
// template use grant.
func TestAgentHostPolicyReservesConnectionHostsToTheController(t *testing.T) {
	documents, err := splitYAMLDocuments(filepath.Join("..", "..", "config", "policy", "workspace_class_use_policy.yaml"))
	require.NoError(t, err)
	var policy *admissionregistrationv1.ValidatingAdmissionPolicy
	for _, document := range documents {
		candidate := &admissionregistrationv1.ValidatingAdmissionPolicy{}
		if yaml.Unmarshal(document, candidate) == nil && candidate.Kind == "ValidatingAdmissionPolicy" &&
			candidate.Name == "agenthost-template-use.orka.ai" {
			policy = candidate
		}
	}
	require.NotNil(t, policy, "the AgentHost policy is missing")

	compiler, err := admissioncel.NewCompositedCompiler(environment.MustBaseEnvSet(environment.DefaultCompatibilityVersion()))
	require.NoError(t, err)
	options := admissioncel.OptionalVariableDeclarations{HasAuthorizer: true}
	expressions := make([]admissioncel.ExpressionAccessor, 0, len(policy.Spec.Validations))
	for _, validation := range policy.Spec.Validations {
		expressions = append(expressions, &validating.ValidationCondition{Expression: validation.Expression})
	}
	validations := compiler.CompileCondition(expressions, options, environment.NewExpressions)
	require.Empty(t, validations.CompilationErrors())

	kind := corev1alpha1.GroupVersion.WithKind("AgentHost")
	resource := corev1alpha1.GroupVersion.WithResource("agenthosts")
	admits := func(username string, granted bool, spec map[string]any) bool {
		t.Helper()
		host := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": corev1alpha1.GroupVersion.String(), "kind": "AgentHost",
			"metadata": map[string]any{"name": "gh-alice", "namespace": admissionTestNamespace},
			"spec":     spec,
		}}
		attributes := apiserveradmission.NewAttributesRecord(host, nil, kind, admissionTestNamespace, "gh-alice",
			resource, "", apiserveradmission.Create, nil, false, &user.DefaultInfo{Name: username})
		versioned := &apiserveradmission.VersionedAttributes{
			Attributes: attributes, VersionedKind: kind,
			VersionedObject:    apiserveradmission.NewLazyObject(host),
			VersionedOldObject: apiserveradmission.NewLazyObject(nil),
		}
		request := admissioncel.CreateAdmissionRequest(attributes, metav1.GroupVersionResource(resource), metav1.GroupVersionKind(kind))
		results, _, err := validations.ForInput(compiler.CreateContext(context.Background()), versioned, request,
			admissioncel.OptionalVariableBindings{Authorizer: fixedAuthorizer(granted)}, nil, celconfig.RuntimeCELCostBudget)
		require.NoError(t, err)
		for _, result := range results {
			require.NoError(t, result.Error)
			if result.EvalResult.Value() != true {
				return false
			}
		}
		return true
	}
	const controller = "system:serviceaccount:orka-system:orka-controller-manager"
	fromConnection := map[string]any{
		"templateRef":     map[string]any{"name": "copilotd"},
		"githubTokenFrom": map[string]any{"connectionRef": map[string]any{"name": "github-alice"}},
	}
	fromSecret := map[string]any{
		"templateRef":          map[string]any{"name": "copilotd"},
		"githubTokenSecretRef": map[string]any{"name": "alice-github"},
	}
	require.True(t, admits(controller, false, fromConnection), "the controller needs no template use grant")
	require.False(t, admits("alice@example.com", true, fromConnection), "a person cannot point a host at a Connection")
	require.True(t, admits("alice@example.com", true, fromSecret))
	require.False(t, admits("alice@example.com", false, fromSecret), "a Secret-fed host still needs template use")
}
