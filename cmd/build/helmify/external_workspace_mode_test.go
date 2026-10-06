package main

import (
	"strings"
	"testing"
)

func TestStaticChartExternalWorkspaceDispatchAndMCPFlags(t *testing.T) {
	for _, test := range []struct {
		name          string
		args          []string
		dispatch, mcp bool
	}{
		{name: "external dispatch", args: []string{
			"--set", "controller.executionWorkspace.dispatchEnabled=true",
		}, dispatch: true},
		{name: "MCP support", args: []string{
			"--set", "controller.substrate.mcpToolsEnabled=true",
			"--set-string", "controller.substrate.apiCredentials.existingSecret=substrate-control",
			"--set-string", "controller.substrate.apiCredentials.bearerTokenKey=token",
			"--set-string", "controller.substrate.apiCredentials.caKey=ca",
		}, mcp: true},
		{name: "default"},
	} {
		t.Run(test.name, func(t *testing.T) {
			rendered := requireHelmRender(t, append(test.args, "--show-only", "templates/deployment.yaml")...)
			for _, flag := range []string{"--acp-workspace-dispatch-enabled=true", "--enable-workspace-provider-api=true"} {
				if strings.Contains(rendered, flag) != test.dispatch {
					t.Errorf("flag %s disagrees with generic dispatch", flag)
				}
			}
			if strings.Contains(rendered, "--substrate-mcp-tools-enabled=true") != test.mcp {
				t.Error("MCP enablement flag disagrees with values")
			}
			for _, obsolete := range []string{
				"--agent-sandbox-enabled", "--substrate-enabled",
				"--substrate-direct-egress-enabled", "--enable-fake-workspace-provider",
			} {
				if strings.Contains(rendered, obsolete) {
					t.Errorf("Core renders retired flag %s", obsolete)
				}
			}
		})
	}
}

func TestStaticChartRejectsExternalWorkspaceDispatchInHarnessV1(t *testing.T) {
	rendered, err := helmTemplateStaticChart(t,
		"--set-string", "controller.mode=harness-v1",
		"--set", "controller.executionWorkspace.dispatchEnabled=true",
		"--set-string", "harnessV1.image.digest=sha256:"+strings.Repeat("1", 64),
		"--set-string", "harnessV1.auth.existingSecret=harness-wrapper-auth",
		"--set-string", "harnessV1.tls.existingSecret=harness-wrapper-tls")
	if err == nil || !strings.Contains(rendered, "controller.executionWorkspace.dispatchEnabled requires harness-v2") {
		t.Fatalf("harness-v1 dispatch render = %v: %s", err, rendered)
	}
}

func TestStaticChartDoesNotGrantNativeProviderAuthority(t *testing.T) {
	rendered := requireHelmRender(t, "--show-only", "templates/rbac.yaml")
	for _, group := range []string{
		"agents.x-k8s.io", "extensions.agents.x-k8s.io", "acp.workspace.orka.ai", "fake.workspace.orka.ai",
	} {
		if strings.Contains(rendered, "apiGroups: [\""+group+"\"]") {
			t.Errorf("Core retains native provider permission %s", group)
		}
	}
}
