/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package admission

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func loadProviderUsePolicies(t *testing.T) map[string]*admissionregistrationv1.ValidatingAdmissionPolicy {
	t.Helper()
	return loadUsePolicies(t, filepath.Join("..", "..", "config", "provider-use-policy", "provider_use_policy.yaml"))
}

func providerUseGrants(allowed ...string) *agentUseGrants {
	grants := &agentUseGrants{allowed: map[string]bool{}, resource: "providers"}
	for _, key := range allowed {
		grants.allowed[key] = true
	}
	return grants
}

func TestProviderUsePolicyTasks(t *testing.T) {
	policies := loadProviderUsePolicies(t)
	require.Len(t, policies, 2)
	policy := policies["tasks"]
	require.NotNil(t, policy)
	gvk := schema.GroupVersionKind{Group: "core.orka.ai", Version: "v1alpha1", Kind: "Task"}
	task := func(providerRef map[string]any) map[string]any {
		ai := map[string]any{"prompt": "hi"}
		if providerRef != nil {
			ai["providerRef"] = providerRef
		}
		return agentUseObject("Task", "core.orka.ai/v1alpha1", map[string]any{"type": "ai", "ai": ai})
	}
	local := map[string]any{"name": "anthropic"}
	localKey := admissionTestNamespace + "/anthropic"

	require.True(t, admitsAgentUse(t, policy, gvk, "tasks", providerUseGrants(localKey), task(local), nil))
	denied := providerUseGrants()
	require.False(t, admitsAgentUse(t, policy, gvk, "tasks", denied, task(local), nil), "creating a Task needs use on its Provider")
	require.Equal(t, []string{localKey}, denied.checked)

	require.True(t, admitsAgentUse(t, policy, gvk, "tasks", providerUseGrants(), task(nil), nil), "a Task without a Provider needs no grant")
	require.True(t, admitsAgentUse(t, policy, gvk, "tasks", providerUseGrants(),
		agentUseObject("Task", "core.orka.ai/v1alpha1", map[string]any{"type": "container", "image": "busybox"}), nil))
	require.True(t, admitsAgentUse(t, policy, gvk, "tasks", providerUseGrants(localKey),
		task(map[string]any{"name": "anthropic", "namespace": ""}), nil), "an empty namespace resolves in the Task namespace")

	unchanged := providerUseGrants()
	require.True(t, admitsAgentUse(t, policy, gvk, "tasks", unchanged, task(local), task(local)), "an edit that keeps the Provider needs no new grant")
	require.Empty(t, unchanged.checked)

	remote := providerUseGrants("models/anthropic")
	require.True(t, admitsAgentUse(t, policy, gvk, "tasks", remote, task(map[string]any{"name": "anthropic", "namespace": "models"}), nil))
	require.Equal(t, []string{"models/anthropic"}, remote.checked, "a cross-namespace Provider is checked in its own namespace")

	agentsOnly := &agentUseGrants{allowed: map[string]bool{localKey: true}}
	require.False(t, admitsAgentUse(t, policy, gvk, "tasks", agentsOnly, task(local), nil), "use on an Agent of the same name is not use on the Provider")
}

func TestProviderUsePolicyAgents(t *testing.T) {
	policy := loadProviderUsePolicies(t)["agents"]
	require.NotNil(t, policy)
	gvk := schema.GroupVersionKind{Group: "core.orka.ai", Version: "v1alpha1", Kind: "Agent"}
	agent := func(primary map[string]any, fallbacks ...string) map[string]any {
		spec := map[string]any{}
		if primary != nil {
			spec["providerRef"] = primary
		}
		if len(fallbacks) > 0 {
			list := make([]any, 0, len(fallbacks))
			for _, name := range fallbacks {
				list = append(list, map[string]any{"providerRef": name})
			}
			spec["model"] = map[string]any{"name": "model", "fallbacks": list}
		}
		return agentUseObject("Agent", "core.orka.ai/v1alpha1", spec)
	}
	primary := map[string]any{"name": "primary"}
	primaryKey, backupKey := admissionTestNamespace+"/primary", admissionTestNamespace+"/backup"

	require.True(t, admitsAgentUse(t, policy, gvk, "agents", providerUseGrants(), agent(nil), nil), "an Agent without Providers needs no grant")
	require.True(t, admitsAgentUse(t, policy, gvk, "agents", providerUseGrants(primaryKey, backupKey), agent(primary, "backup"), nil))
	require.False(t, admitsAgentUse(t, policy, gvk, "agents", providerUseGrants(primaryKey), agent(primary, "backup"), nil),
		"each fallback needs its own grant")
	require.False(t, admitsAgentUse(t, policy, gvk, "agents", providerUseGrants(backupKey), agent(primary, "backup"), nil))

	kept := providerUseGrants()
	require.True(t, admitsAgentUse(t, policy, gvk, "agents", kept, agent(map[string]any{"name": "backup"}, "primary"), agent(primary, "backup")),
		"an edit that keeps the Agent's Providers, in any role, needs no new grant")
	require.Empty(t, kept.checked)

	added := providerUseGrants(admissionTestNamespace + "/other")
	require.True(t, admitsAgentUse(t, policy, gvk, "agents", added, agent(primary, "other"), agent(primary)))
	require.Equal(t, []string{admissionTestNamespace + "/other"}, added.checked, "only the added fallback is checked")

	remote := providerUseGrants("models/primary")
	require.True(t, admitsAgentUse(t, policy, gvk, "agents", remote, agent(map[string]any{"name": "primary", "namespace": "models"}), agent(primary)),
		"the same name in another namespace is a different Provider")
	require.Equal(t, []string{"models/primary"}, remote.checked)
}
