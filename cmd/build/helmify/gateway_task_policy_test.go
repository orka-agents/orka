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

// The raw policy's admission tests cover foreground finalization and forbidden
// mutations. Helm must render the same rules with its release-specific trust
// identities and watched namespace, or those guarantees do not reach Helm users.
func TestStaticChartGatewayTaskProtectionMatchesCanonicalPolicy(t *testing.T) {
	canonical, err := os.ReadFile(filepath.Join("..", "..", "..", "config", "admission", "gateway_task_protection.yaml"))
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
				"--show-only", "templates/gateway-task-admission-policy.yaml",
			}, tc.args...)
			rendered, err := helmTemplateStaticChartForRelease(t, tc.release, tc.namespace, args...)
			require.NoError(t, err, "%s", rendered)
			var actual admissionregistrationv1.ValidatingAdmissionPolicy
			require.NoError(t, yaml.UnmarshalStrict([]byte(requireRenderedDocument(t, rendered,
				"kind: ValidatingAdmissionPolicy\n")), &actual))
			require.Equal(t, tc.fullname+"-gateway-task-protection", actual.Name)

			replacements := strings.NewReplacer(
				"ORKA_NAMESPACE", tc.namespace,
				"CONTROLLER_SA", tc.controllerAccount,
				"AI_WORKER_SA", tc.fullname+"-ai-worker",
				"VENDOR_WORKER_SA", tc.fullname+"-vendor-worker",
				"CONTAINER_WORKER_SA", tc.fullname+"-container-worker",
				"startsWith('gateway.orka.ai/')", "startsWith('gateway.orka.ai/"+tc.namespace+"/')",
			)
			var expected admissionregistrationv1.ValidatingAdmissionPolicy
			require.NoError(t, yaml.UnmarshalStrict([]byte(replacements.Replace(canonicalPolicy)), &expected))
			require.Equal(t, expected.Spec, actual.Spec, "Helm must preserve the canonical Gateway admission rules")

			var binding admissionregistrationv1.ValidatingAdmissionPolicyBinding
			require.NoError(t, yaml.UnmarshalStrict([]byte(requireRenderedDocument(t, rendered,
				"kind: ValidatingAdmissionPolicyBinding\n")), &binding))
			require.Equal(t, actual.Name, binding.Spec.PolicyName)
			require.Equal(t,
				[]admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny}, binding.Spec.ValidationActions)
		})
	}
}
