package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	"sigs.k8s.io/yaml"
)

// The raw policy's admission tests cover reserved Lease ownership. Helm must
// render the same rules with its release identity and namespace, or those
// guarantees do not reach Helm users.
func TestStaticChartACPWorkspaceLeaseProtectionMatchesCanonicalPolicy(t *testing.T) {
	canonical, err := os.ReadFile(
		filepath.Join("..", "..", "..", "config", "admission", "acp_workspace_lease_protection.yaml"))
	require.NoError(t, err)
	canonicalPolicy, _, found := strings.Cut(string(canonical), "\n---\n")
	require.True(t, found)

	for _, tc := range []struct {
		name              string
		release           string
		namespace         string
		fullname          string
		controllerAccount string
		args              []string
	}{
		{
			name: "default names", release: "orka", namespace: "orka-system",
			fullname: "orka", controllerAccount: "orka",
		},
		{
			name: "separate release and custom controller", release: "secondary", namespace: "controllers",
			fullname: "secondary-orka", controllerAccount: "custom-controller",
			args: []string{"--set-string", "serviceAccount.name=custom-controller"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{
				"--show-only", "templates/acp-workspace-lease-admission-policy.yaml",
			}, tc.args...)
			rendered, err := helmTemplateStaticChartForRelease(t, tc.release, tc.namespace, args...)
			require.NoError(t, err, "%s", rendered)
			var actual admissionregistrationv1.ValidatingAdmissionPolicy
			require.NoError(t, yaml.UnmarshalStrict([]byte(requireRenderedDocument(t, rendered,
				"kind: ValidatingAdmissionPolicy\n")), &actual))
			require.Equal(t, tc.fullname+"-acp-workspace-lease-protection", actual.Name)

			replacements := strings.NewReplacer(
				"ORKA_NAMESPACE", tc.namespace,
				"CONTROLLER_SA", tc.controllerAccount,
			)
			var expected admissionregistrationv1.ValidatingAdmissionPolicy
			require.NoError(t, yaml.UnmarshalStrict([]byte(replacements.Replace(canonicalPolicy)), &expected))
			require.Equal(t, expected.Spec, actual.Spec, "Helm must preserve the canonical ACP workspace Lease rules")

			var binding admissionregistrationv1.ValidatingAdmissionPolicyBinding
			require.NoError(t, yaml.UnmarshalStrict([]byte(requireRenderedDocument(t, rendered,
				"kind: ValidatingAdmissionPolicyBinding\n")), &binding))
			require.Equal(t, actual.Name, binding.Spec.PolicyName)
			require.Equal(t,
				[]admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny}, binding.Spec.ValidationActions)
		})
	}
}

func TestStaticChartACPWorkspaceLeaseProtectionIsHarnessV2Only(t *testing.T) {
	digest := "sha256:" + strings.Repeat("3", 64)
	rendered := requireHelmRender(t,
		"--set-string", "controller.mode=harness-v1",
		"--set-string", "harnessV1.image.digest="+digest,
		"--set-string", "harnessV1.auth.existingSecret=harness-wrapper-auth",
		"--set-string", "harnessV1.tls.existingSecret=harness-wrapper-tls",
	)
	require.NotContains(t, rendered, "acp-workspace-lease-protection")
}

// AKS refuses to stop a cluster while any fail-closed webhook intercepts
// Leases, so Lease guards must stay admission policies.
func TestSharedAdmissionWebhooksDoNotInterceptLeases(t *testing.T) {
	manifest, err := os.ReadFile(
		filepath.Join("..", "..", "..", "config", "orka-admission-webhooks", "validating_webhook.yaml"))
	require.NoError(t, err)
	var configuration admissionregistrationv1.ValidatingWebhookConfiguration
	require.NoError(t, yaml.Unmarshal(manifest, &configuration))
	require.NotEmpty(t, configuration.Webhooks)
	for _, webhook := range configuration.Webhooks {
		for _, rule := range webhook.Rules {
			require.False(t, slices.Contains(rule.Resources, "leases"), "%s intercepts Leases", webhook.Name)
		}
	}
}
