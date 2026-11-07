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

// internal/admission tests the canonical policy. Helm must render the same
// rules with its release's AI worker ServiceAccount, for both worker profiles.
func TestStaticChartAIWorkerIdentityProtectionMatchesCanonicalPolicy(t *testing.T) {
	canonical, err := os.ReadFile(filepath.Join("..", "..", "..", "config", "admission", "ai_worker_identity_protection.yaml"))
	require.NoError(t, err)
	canonicalPolicy, _, found := strings.Cut(string(canonical), "\n---\n")
	require.True(t, found)

	for _, tc := range []struct {
		name, release, namespace, fullname string
		args                               []string
	}{
		{name: "default profile", release: "orka", namespace: "orka-system", fullname: "orka"},
		{
			name: "restricted profile", release: "secondary", namespace: "controllers", fullname: "secondary-orka",
			args: []string{"--set-string", "workers.ai.rbacProfile=restricted"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"--show-only", "templates/ai-worker-identity-policy.yaml"}, tc.args...)
			rendered, err := helmTemplateStaticChartForRelease(t, tc.release, tc.namespace, args...)
			require.NoError(t, err, "%s", rendered)
			var actual admissionregistrationv1.ValidatingAdmissionPolicy
			require.NoError(t, yaml.UnmarshalStrict([]byte(requireRenderedDocument(t, rendered,
				"kind: ValidatingAdmissionPolicy\n")), &actual))
			require.Equal(t, tc.fullname+"-ai-worker-identity-protection", actual.Name)

			var expected admissionregistrationv1.ValidatingAdmissionPolicy
			require.NoError(t, yaml.UnmarshalStrict([]byte(strings.ReplaceAll(canonicalPolicy,
				"AI_WORKER_SA", tc.fullname+"-ai-worker")), &expected))
			require.Equal(t, expected.Spec, actual.Spec, "Helm must preserve the canonical AI worker identity rules")

			var binding admissionregistrationv1.ValidatingAdmissionPolicyBinding
			require.NoError(t, yaml.UnmarshalStrict([]byte(requireRenderedDocument(t, rendered,
				"kind: ValidatingAdmissionPolicyBinding\n")), &binding))
			require.Equal(t, actual.Name, binding.Spec.PolicyName)
			require.Equal(t,
				[]admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny}, binding.Spec.ValidationActions)
		})
	}
}
