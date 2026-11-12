package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	"sigs.k8s.io/yaml"
)

func TestStaticChartAgentIdentityBindPolicyMatchesCanonicalPolicy(t *testing.T) {
	rendered, err := helmTemplateStaticChart(t, "--show-only", "templates/agent-identity-bind-policy.yaml")
	require.NoError(t, err, "%s", rendered)
	canonical, err := os.ReadFile(filepath.Join("..", "..", "..", "config", "policy", "agent_identity_bind_policy.yaml"))
	require.NoError(t, err)
	want := agentUsePoliciesByResource(t, string(canonical))
	require.Contains(t, want, "agents")
	require.Equal(t, want, agentUsePoliciesByResource(t, rendered), "the chart must enforce the canonical bind policy")
	require.Contains(t, rendered, "kind: ValidatingAdmissionPolicyBinding")
}

func TestStaticChartAgentIdentityFlags(t *testing.T) {
	deployment, err := helmTemplateStaticChart(t, "--show-only", "templates/deployment.yaml")
	require.NoError(t, err, "%s", deployment)
	require.NotContains(t, deployment, "--agent-identity-entra")

	deployment, err = helmTemplateStaticChart(t,
		"--set-string", "controller.agentIdentity.entra.tenantID=11111111-1111-1111-1111-111111111111",
		"--set-string", "controller.agentIdentity.entra.blueprintClientID=22222222-2222-2222-2222-222222222222",
		"--show-only", "templates/deployment.yaml")
	require.NoError(t, err, "%s", deployment)
	require.Contains(t, deployment, `"--agent-identity-entra-tenant-id=11111111-1111-1111-1111-111111111111"`)
	require.Contains(t, deployment, `"--agent-identity-entra-blueprint-client-id=22222222-2222-2222-2222-222222222222"`)

	// One value alone would crash-loop the controller, so rendering fails.
	out, err := helmTemplateStaticChart(t,
		"--set-string", "controller.agentIdentity.entra.tenantID=11111111-1111-1111-1111-111111111111",
		"--show-only", "templates/deployment.yaml")
	require.Error(t, err, "%s", out)
	require.Contains(t, out+err.Error(), "must be set together")
}

func TestStaticChartMountsTheAgentIdentityToken(t *testing.T) {
	deployment, err := helmTemplateStaticChart(t, "--show-only", "templates/deployment.yaml")
	require.NoError(t, err, "%s", deployment)
	require.NotContains(t, deployment, "agent-identity-token")

	deployment, err = helmTemplateStaticChart(t,
		"--set-string", "controller.agentIdentity.entra.tenantID=11111111-1111-1111-1111-111111111111",
		"--set-string", "controller.agentIdentity.entra.blueprintClientID=22222222-2222-2222-2222-222222222222",
		"--set-string", "controller.agentIdentity.entra.authority=https://login.microsoftonline.us",
		"--show-only", "templates/deployment.yaml")
	require.NoError(t, err, "%s", deployment)
	require.Contains(t, deployment, "audience: api://AzureADTokenExchange")
	require.Contains(t, deployment, "mountPath: /var/run/secrets/orka.ai/agent-identity")
	require.Contains(t, deployment, `"--agent-identity-entra-authority=https://login.microsoftonline.us"`)
}

// The raw policy's admission tests cover which workloads may run as the
// controller ServiceAccount. Helm must render the same rules, with its release
// namespace and ServiceAccount, whenever it projects the blueprint's token.
func TestStaticChartProtectsTheControllerServiceAccountWithAgentIdentities(t *testing.T) {
	const template = "templates/controller-serviceaccount-admission-policy.yaml"
	rendered, err := helmTemplateStaticChartForRelease(t, "orka", "orka-system", "--show-only", template)
	require.Error(t, err, "without an Entra issuer the chart must not render the policy: %s", rendered)

	canonical, err := os.ReadFile(
		filepath.Join("..", "..", "..", "config", "admission", "controller_serviceaccount_protection.yaml"))
	require.NoError(t, err)
	canonicalPolicy, _, found := strings.Cut(string(canonical), "\n---\n")
	require.True(t, found)
	for _, tc := range []struct {
		name, release, namespace, fullname, controllerAccount string
		args                                                  []string
	}{
		{name: "default names", release: "orka", namespace: "orka-system", fullname: "orka", controllerAccount: "orka"},
		{
			name: "separate release and custom controller", release: "secondary", namespace: "controllers",
			fullname: "secondary-orka", controllerAccount: "custom-controller",
			args: []string{"--set-string", "serviceAccount.name=custom-controller"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{
				"--set-string", "controller.agentIdentity.entra.tenantID=11111111-1111-1111-1111-111111111111",
				"--set-string", "controller.agentIdentity.entra.blueprintClientID=22222222-2222-2222-2222-222222222222",
				"--show-only", template,
			}, tc.args...)
			rendered, err := helmTemplateStaticChartForRelease(t, tc.release, tc.namespace, args...)
			require.NoError(t, err, "%s", rendered)
			var actual admissionregistrationv1.ValidatingAdmissionPolicy
			require.NoError(t, yaml.UnmarshalStrict([]byte(requireRenderedDocument(t, rendered,
				"kind: ValidatingAdmissionPolicy\n")), &actual))
			require.Equal(t, tc.fullname+"-controller-serviceaccount-protection", actual.Name)
			var expected admissionregistrationv1.ValidatingAdmissionPolicy
			require.NoError(t, yaml.UnmarshalStrict([]byte(strings.NewReplacer(
				"ORKA_NAMESPACE", tc.namespace, "CONTROLLER_SA", tc.controllerAccount,
				"CONTROLLER_DEPLOYMENT", tc.fullname+"-controller",
			).Replace(canonicalPolicy)), &expected))
			require.Equal(t, expected.Spec, actual.Spec, "Helm must preserve the canonical controller ServiceAccount rules")

			var binding admissionregistrationv1.ValidatingAdmissionPolicyBinding
			require.NoError(t, yaml.UnmarshalStrict([]byte(requireRenderedDocument(t, rendered,
				"kind: ValidatingAdmissionPolicyBinding\n")), &binding))
			require.Equal(t, actual.Name, binding.Spec.PolicyName)
			require.Equal(t,
				[]admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny}, binding.Spec.ValidationActions)
		})
	}
}
