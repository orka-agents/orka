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
	canonical, err := os.ReadFile(filepath.Join(
		"..", "..", "..", "config", "admission", "ai_worker_identity_protection.yaml"))
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
			require.NoError(t, yaml.UnmarshalStrict([]byte(strings.NewReplacer(
				"ORKA_NAMESPACE", tc.namespace, "AI_WORKER_SA", tc.fullname+"-ai-worker",
			).Replace(canonicalPolicy)), &expected))
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

// The policy is on by default and when an older release's reused values lack
// the setting; operators can turn it off explicitly, and only with a boolean.
func TestStaticChartAIWorkerIdentityProtectionOptOut(t *testing.T) {
	const policyKind = "kind: ValidatingAdmissionPolicy\nmetadata:\n  name: test-orka-ai-worker-identity-protection"
	for _, tc := range []struct {
		name string
		args []string
		want bool
	}{
		{name: "default", want: true},
		{
			name: "reused values without the setting",
			args: []string{"--set", "workers.ai.identityProtection=null"}, want: true,
		},
		{name: "explicitly enabled", args: []string{"--set", "workers.ai.identityProtection=true"}, want: true},
		{name: "opted out", args: []string{"--set", "workers.ai.identityProtection=false"}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rendered := requireHelmRender(t, tc.args...)
			require.Equal(t, tc.want, strings.Contains(rendered, policyKind), "policy rendered")
			require.Equal(t, tc.want, strings.Contains(rendered, "policyName: test-orka-ai-worker-identity-protection"),
				"binding rendered")
		})
	}
	output, err := helmTemplateStaticChart(t, "--set-string", "workers.ai.identityProtection=false")
	require.Error(t, err, "a string value must not silently disable the policy")
	require.Contains(t, output, "workers.ai.identityProtection must be true or false")
}
