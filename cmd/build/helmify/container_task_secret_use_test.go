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

func TestStaticChartContainerTaskSecretUse(t *testing.T) {
	t.Run("off by default", func(t *testing.T) {
		deployment, err := helmTemplateStaticChart(t, "--show-only", "templates/deployment.yaml")
		require.NoError(t, err, "%s", deployment)
		require.NotContains(t, deployment, "--container-task-secret-use")

		_, err = helmTemplateStaticChart(t, "--show-only", "templates/container-task-secret-use-policy.yaml")
		require.Error(t, err, "the policy must not render unless enabled")

		rbac, err := helmTemplateStaticChart(t, "--show-only", "templates/rbac.yaml")
		require.NoError(t, err, "%s", rbac)
		require.NotContains(t, rbac, "validatingadmissionpolicybindings")
	})

	t.Run("enabled renders the flag, the canonical policy, and its read grant", func(t *testing.T) {
		enabled := []string{"--set", "controller.containerTaskSecretUse.enabled=true"}
		deployment, err := helmTemplateStaticChart(t, append(enabled, "--show-only", "templates/deployment.yaml")...)
		require.NoError(t, err, "%s", deployment)
		require.Contains(t, deployment, "--container-task-secret-use-authorization-enabled=true")
		require.Contains(t, deployment, `"--container-task-secret-use-policy-name=test-orka-container-task-secret-use"`)

		rendered, err := helmTemplateStaticChart(t,
			append(enabled, "--show-only", "templates/container-task-secret-use-policy.yaml")...)
		require.NoError(t, err, "%s", rendered)
		canonical, err := os.ReadFile(filepath.Join("..", "..", "..", "config",
			"container-task-secret-use-policy", "container_task_secret_use_policy.yaml"))
		require.NoError(t, err)
		controllerUser := "system:serviceaccount:orka-test:test-orka"
		require.Contains(t, rendered, controllerUser, "the scheduled-run exemption names this release's controller")
		want := agentUsePoliciesByResource(t, strings.NewReplacer(
			"ORKA_NAMESPACE", "orka-test", "CONTROLLER_SA", "test-orka").Replace(string(canonical)))
		got := agentUsePoliciesByResource(t, rendered)
		require.ElementsMatch(t, []string{"tasks"}, keys(want))
		require.Equal(t, want, got, "the chart must enforce exactly the canonical policy")
		require.Contains(t, rendered, "name: test-orka-container-task-secret-use")

		bindings := 0
		for document := range strings.SplitSeq(rendered, "\n---") {
			var meta metav1.TypeMeta
			if yaml.Unmarshal([]byte(document), &meta) != nil || meta.Kind != "ValidatingAdmissionPolicyBinding" {
				continue
			}
			var binding admissionregistrationv1.ValidatingAdmissionPolicyBinding
			require.NoError(t, yaml.UnmarshalStrict([]byte(document), &binding))
			require.Equal(t, "test-orka-container-task-secret-use", binding.Spec.PolicyName)
			require.Equal(t, []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny},
				binding.Spec.ValidationActions)
			require.NotNil(t, binding.Spec.MatchResources)
			require.Equal(t, &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "orka-test"}},
				binding.Spec.MatchResources.NamespaceSelector)
			bindings++
		}
		require.Equal(t, 1, bindings)

		rbac, err := helmTemplateStaticChart(t, append(enabled, "--show-only", "templates/rbac.yaml")...)
		require.NoError(t, err, "%s", rbac)
		require.Contains(t, rbac, `resources: ["validatingadmissionpolicies", "validatingadmissionpolicybindings"]`+
			"\n    resourceNames: [\"test-orka-container-task-secret-use\"]\n    verbs: [\"get\"]")
	})

	t.Run("Orka's own identities never hold use on Secrets", func(t *testing.T) {
		rbac, err := helmTemplateStaticChart(t,
			"--set", "controller.containerTaskSecretUse.enabled=true", "--show-only", "templates/rbac.yaml")
		require.NoError(t, err, "%s", rbac)
		for document := range strings.SplitSeq(rbac, "\n---") {
			var role struct {
				Kind  string `json:"kind"`
				Rules []struct {
					APIGroups []string `json:"apiGroups"`
					Resources []string `json:"resources"`
					Verbs     []string `json:"verbs"`
				} `json:"rules"`
			}
			if yaml.Unmarshal([]byte(document), &role) != nil || (role.Kind != "Role" && role.Kind != "ClusterRole") {
				continue
			}
			for _, rule := range role.Rules {
				coreSecrets := false
				for _, group := range rule.APIGroups {
					for _, resource := range rule.Resources {
						if (group == "" || group == "*") && (resource == "secrets" || resource == "*") {
							coreSecrets = true
						}
					}
				}
				if coreSecrets {
					require.NotContains(t, rule.Verbs, "use", "%s", document)
					require.NotContains(t, rule.Verbs, "*", "%s", document)
				}
			}
		}
	})
}
