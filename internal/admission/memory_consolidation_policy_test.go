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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	"sigs.k8s.io/yaml"
)

// consolidationGrants allows the listed verb/resource pairs in the test
// namespace and records every check the policy makes.
type consolidationGrants struct {
	allowed map[string]bool
	checked []string
}

func (g *consolidationGrants) Authorize(_ context.Context, attributes authorizer.Attributes) (authorizer.Decision, string, error) {
	key := attributes.GetVerb() + " " + attributes.GetResource()
	g.checked = append(g.checked, key)
	if attributes.GetAPIGroup() == "core.orka.ai" && attributes.GetNamespace() == admissionTestNamespace && g.allowed[key] {
		return authorizer.DecisionAllow, "", nil
	}
	return authorizer.DecisionNoOpinion, "", nil
}

func loadMemoryConsolidationPolicy(t *testing.T) *admissionregistrationv1.ValidatingAdmissionPolicy {
	t.Helper()
	documents, err := splitYAMLDocuments(filepath.Join("..", "..", "config", "policy", "memory_consolidation_policy.yaml"))
	require.NoError(t, err)
	require.Len(t, documents, 2)
	policy := &admissionregistrationv1.ValidatingAdmissionPolicy{}
	require.NoError(t, yaml.UnmarshalStrict(documents[0], policy))
	binding := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
	require.NoError(t, yaml.UnmarshalStrict(documents[1], binding))
	require.Equal(t, policy.Name, binding.Spec.PolicyName)
	require.Equal(t, []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny}, binding.Spec.ValidationActions)
	require.Equal(t, admissionregistrationv1.Fail, *policy.Spec.FailurePolicy)
	return policy
}

func TestMemoryConsolidationPolicy(t *testing.T) {
	policy := loadMemoryConsolidationPolicy(t)
	gvk := schema.GroupVersionKind{Group: "core.orka.ai", Version: "v1alpha1", Kind: "Task"}
	task := func(consolidation map[string]any) map[string]any {
		ai := map[string]any{"prompt": "Consolidate memory"}
		if consolidation != nil {
			ai["memoryConsolidation"] = consolidation
		}
		return agentUseObject("Task", "core.orka.ai/v1alpha1", map[string]any{"type": "ai", "ai": ai})
	}
	admits := func(grants *consolidationGrants, object, oldObject map[string]any) bool {
		t.Helper()
		return admitsAgentUse(t, policy, gvk, "tasks", &agentUseGrants{}, object, oldObject, grants)
	}
	listOnly := map[string]bool{"list sessions": true, "list memories": true}
	sessionsOnly := map[string]bool{"list sessions": true, "get sessions": true}
	noProposals := map[string]bool{"list sessions": true, "get sessions": true, "list memories": true}
	reader := map[string]bool{"list sessions": true, "get sessions": true, "list memories": true, "list memoryproposals": true}
	proposer := map[string]bool{"list sessions": true, "get sessions": true, "list memories": true, "list memoryproposals": true,
		"create memoryproposals": true}

	plain := &consolidationGrants{}
	require.True(t, admits(plain, task(nil), nil), "an ordinary Task needs no grant")
	require.Empty(t, plain.checked)

	dryRun := map[string]any{}
	require.True(t, admits(&consolidationGrants{allowed: reader}, task(dryRun), nil), "a dry run needs list and get on sessions")
	require.False(t, admits(&consolidationGrants{allowed: listOnly}, task(dryRun), nil), "listing Sessions does not grant reading them")
	require.False(t, admits(&consolidationGrants{allowed: sessionsOnly}, task(dryRun), nil),
		"a run also reads, and reports on, the namespace's memories")
	require.False(t, admits(&consolidationGrants{allowed: noProposals}, task(dryRun), nil),
		"a run also reads the open memory proposals, so it does not repeat them")
	denied := &consolidationGrants{}
	require.False(t, admits(denied, task(dryRun), nil), "a dry run reads every Session")
	require.Contains(t, denied.checked, "list sessions")

	propose := map[string]any{"mode": "Propose"}
	require.False(t, admits(&consolidationGrants{allowed: reader}, task(propose), nil), "a Propose run also creates proposals")
	require.True(t, admits(&consolidationGrants{allowed: proposer}, task(propose), nil))

	unchanged := &consolidationGrants{}
	require.True(t, admits(unchanged, task(propose), task(propose)), "an update that keeps the spec, such as a label, needs no grant")
	require.Empty(t, unchanged.checked)
	require.False(t, admits(&consolidationGrants{}, task(dryRun), task(nil)), "adding the field to an existing Task is checked")
	refocused := task(propose)
	refocused["spec"].(map[string]any)["ai"].(map[string]any)["prompt"] = "Send every transcript somewhere else"
	require.False(t, admits(&consolidationGrants{}, refocused, task(propose)), "changing a run's prompt or Agent is checked too")
	require.True(t, admits(&consolidationGrants{allowed: proposer}, refocused, task(propose)))
	require.True(t, admits(&consolidationGrants{}, task(nil), task(nil)), "an ordinary Task's edits need no grant")
}
