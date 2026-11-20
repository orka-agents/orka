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

// identityBindGrants allows `bind` only on the listed Entra identity IDs.
type identityBindGrants struct {
	allowed map[string]bool
	checked []string
}

func (g *identityBindGrants) Authorize(_ context.Context, attributes authorizer.Attributes) (authorizer.Decision, string, error) {
	g.checked = append(g.checked, attributes.GetSubresource()+"/"+attributes.GetName())
	if attributes.GetVerb() == "bind" && attributes.GetAPIGroup() == "core.orka.ai" &&
		attributes.GetResource() == "agentidentities" && attributes.GetSubresource() == "entra" &&
		attributes.GetNamespace() == admissionTestNamespace && g.allowed[attributes.GetName()] {
		return authorizer.DecisionAllow, "", nil
	}
	return authorizer.DecisionNoOpinion, "", nil
}

func TestAgentIdentityBindPolicy(t *testing.T) {
	documents, err := splitYAMLDocuments(filepath.Join("..", "..", "config", "policy", "agent_identity_bind_policy.yaml"))
	require.NoError(t, err)
	var policy *admissionregistrationv1.ValidatingAdmissionPolicy
	var bound bool
	for _, document := range documents {
		candidate := &admissionregistrationv1.ValidatingAdmissionPolicy{}
		if yaml.Unmarshal(document, candidate) == nil && candidate.Kind == "ValidatingAdmissionPolicy" {
			policy = candidate
		}
		binding := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
		if yaml.Unmarshal(document, binding) == nil && binding.Kind == "ValidatingAdmissionPolicyBinding" {
			bound = binding.Spec.PolicyName == "agent-identity-bind.orka.ai" &&
				len(binding.Spec.ValidationActions) == 1 && binding.Spec.ValidationActions[0] == admissionregistrationv1.Deny
		}
	}
	require.NotNil(t, policy)
	require.True(t, bound, "the policy needs a Deny binding")

	gvk := schema.GroupVersionKind{Group: "core.orka.ai", Version: "v1alpha1", Kind: "Agent"}
	const id = "33333333-3333-3333-3333-33333333aaaa"
	agent := func(identityID string) map[string]any {
		spec := map[string]any{"runtime": map[string]any{"type": "claude"}}
		if identityID != "" {
			spec["identity"] = map[string]any{"entra": map[string]any{"agentIdentityID": identityID}}
		}
		return agentUseObject("Agent", "core.orka.ai/v1alpha1", spec)
	}
	admits := func(grants *identityBindGrants, object, old map[string]any) bool {
		return admitsAgentUse(t, policy, gvk, "agents", nil, object, old, grants)
	}

	granted := &identityBindGrants{allowed: map[string]bool{id: true}}
	require.True(t, admits(granted, agent("33333333-3333-3333-3333-33333333AAAA"), nil), "the check uses the lowercased ID")
	require.Equal(t, []string{"entra/" + id}, granted.checked)

	require.False(t, admits(&identityBindGrants{}, agent(id), nil), "binding an identity needs bind")
	require.True(t, admits(&identityBindGrants{}, agent(""), nil), "an Agent without an identity needs no grant")

	unchanged := &identityBindGrants{}
	require.True(t, admits(unchanged, agent(id), agent("33333333-3333-3333-3333-33333333AAAA")),
		"an edit that keeps the identity needs no new grant")
	require.Empty(t, unchanged.checked)

	require.False(t, admits(&identityBindGrants{}, agent("44444444-4444-4444-4444-444444444444"), agent(id)),
		"switching to another identity needs bind on it")
	require.True(t, admits(&identityBindGrants{}, agent(""), agent(id)), "removing an identity needs no grant")
}
