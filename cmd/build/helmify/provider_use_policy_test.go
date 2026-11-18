package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

func TestStaticChartProviderUseAuthorization(t *testing.T) {
	t.Run("off by default", func(t *testing.T) {
		deployment, err := helmTemplateStaticChart(t, "--show-only", "templates/deployment.yaml")
		require.NoError(t, err, "%s", deployment)
		require.NotContains(t, deployment, "--provider-use-authorization-enabled")

		_, err = helmTemplateStaticChart(t, "--show-only", "templates/provider-use-policy.yaml")
		require.Error(t, err, "the policies must not render unless enabled")
	})

	t.Run("enabled renders the flag and the canonical policies", func(t *testing.T) {
		deployment, err := helmTemplateStaticChart(t,
			"--set", "controller.providerUseAuthorization.enabled=true", "--show-only", "templates/deployment.yaml")
		require.NoError(t, err, "%s", deployment)
		require.Contains(t, deployment, "--provider-use-authorization-enabled=true")
		require.NotContains(t, deployment, "--agent-use-authorization-enabled", "the two settings are independent")

		rendered, err := helmTemplateStaticChart(t,
			"--set", "controller.providerUseAuthorization.enabled=true", "--show-only", "templates/provider-use-policy.yaml")
		require.NoError(t, err, "%s", rendered)
		canonicalPath := filepath.Join("..", "..", "..", "config", "provider-use-policy", "provider_use_policy.yaml")
		canonical, err := os.ReadFile(canonicalPath)
		require.NoError(t, err)

		want := agentUsePoliciesByResource(t, string(canonical))
		got := agentUsePoliciesByResource(t, rendered)
		require.ElementsMatch(t, []string{"tasks", "agents"}, keys(want))
		require.Equal(t, want, got, "the chart must enforce exactly the canonical policies")
		require.Contains(t, rendered, "name: test-orka-task-provider-use")

		bindings := 0
		for document := range strings.SplitSeq(rendered, "\n---") {
			var meta metav1.TypeMeta
			if yaml.Unmarshal([]byte(document), &meta) != nil || meta.Kind != "ValidatingAdmissionPolicyBinding" {
				continue
			}
			var binding admissionregistrationv1.ValidatingAdmissionPolicyBinding
			require.NoError(t, yaml.UnmarshalStrict([]byte(document), &binding))
			require.NotNil(t, binding.Spec.MatchResources, "binding %s must be scoped to the release", binding.Name)
			require.Equal(t, &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "orka-test"}},
				binding.Spec.MatchResources.NamespaceSelector,
				"another installation that leaves Provider use off must not be enforced by this one")
			bindings++
		}
		require.Equal(t, 2, bindings)
	})

	t.Run("Orka's own identities may use every Provider", func(t *testing.T) {
		rbac, err := helmTemplateStaticChart(t, "--show-only", "templates/rbac.yaml")
		require.NoError(t, err, "%s", rbac)
		require.Contains(t, rbac, `resources: ["agents", "providers"]`+"\n    verbs: [\"use\"]")
		require.Equal(t, 2, strings.Count(rbac, `resources: ["providers"]`+"\n    verbs: [\"use\"]"),
			"both full worker profiles create Tasks and Agents that may name Providers")
	})
}
