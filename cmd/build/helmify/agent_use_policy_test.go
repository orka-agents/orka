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

type agentUsePolicySpecs map[string]admissionregistrationv1.ValidatingAdmissionPolicySpec

func agentUsePoliciesByResource(t *testing.T, manifest string) agentUsePolicySpecs {
	t.Helper()
	policies := agentUsePolicySpecs{}
	for document := range strings.SplitSeq(manifest, "\n---") {
		var meta metav1.TypeMeta
		if yaml.Unmarshal([]byte(document), &meta) != nil || meta.Kind != "ValidatingAdmissionPolicy" {
			continue
		}
		var policy admissionregistrationv1.ValidatingAdmissionPolicy
		require.NoError(t, yaml.UnmarshalStrict([]byte(document), &policy))
		policies[policy.Spec.MatchConstraints.ResourceRules[0].Resources[0]] = policy.Spec
	}
	return policies
}

func TestStaticChartAgentUseAuthorization(t *testing.T) {
	t.Run("off by default", func(t *testing.T) {
		deployment, err := helmTemplateStaticChart(t, "--show-only", "templates/deployment.yaml")
		require.NoError(t, err, "%s", deployment)
		require.NotContains(t, deployment, "--agent-use-authorization-enabled")

		_, err = helmTemplateStaticChart(t, "--show-only", "templates/agent-use-policy.yaml")
		require.Error(t, err, "the policies must not render unless enabled")
	})

	t.Run("enabled renders the flag and the canonical policies", func(t *testing.T) {
		deployment, err := helmTemplateStaticChart(t,
			"--set", "controller.agentUseAuthorization.enabled=true", "--show-only", "templates/deployment.yaml")
		require.NoError(t, err, "%s", deployment)
		require.Contains(t, deployment, "--agent-use-authorization-enabled=true")

		rendered, err := helmTemplateStaticChart(t,
			"--set", "controller.agentUseAuthorization.enabled=true", "--show-only", "templates/agent-use-policy.yaml")
		require.NoError(t, err, "%s", rendered)
		canonical, err := os.ReadFile(filepath.Join("..", "..", "..", "config", "agent-use-policy", "agent_use_policy.yaml"))
		require.NoError(t, err)

		want := agentUsePoliciesByResource(t, string(canonical))
		got := agentUsePoliciesByResource(t, rendered)
		require.ElementsMatch(t, []string{"tasks", "repositoryscans", "repositorymonitors", "gatewaybindings"}, keys(want))
		require.Equal(t, want, got, "the chart must enforce exactly the canonical policies")
		require.Equal(t, 4, strings.Count(rendered, "kind: ValidatingAdmissionPolicyBinding"))
		require.Contains(t, rendered, "name: test-orka-task-agent-use")
	})
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	return out
}
