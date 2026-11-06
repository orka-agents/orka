package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
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
