package main

import (
	"strings"
	"testing"
)

func TestStaticChartValidatesAgentSandboxControllerMode(t *testing.T) {
	t.Run("rejects harness v1", func(t *testing.T) {
		digest := "sha256:" + strings.Repeat("1", 64)
		output, err := helmTemplateStaticChart(t,
			"--set-string", "controller.mode=harness-v1",
			"--set", "controller.agentSandbox.enabled=true",
			"--set-string", "harnessV1.image.digest="+digest,
			"--set-string", "harnessV1.auth.existingSecret=harness-wrapper-auth",
			"--set-string", "harnessV1.tls.existingSecret=harness-wrapper-tls",
		)
		const want = "controller.agentSandbox.enabled is unsupported when controller.mode=harness-v1"
		if err == nil || !strings.Contains(output, want) {
			t.Fatalf("helm render error = %v, want %q:\n%s", err, want, output)
		}
	})

	t.Run("allows harness v2", func(t *testing.T) {
		output, err := helmTemplateStaticChart(t,
			"--set", "controller.agentSandbox.enabled=true",
			"--show-only", "templates/deployment.yaml",
		)
		if err != nil {
			t.Fatalf("helm template rejected harness-v2 agent sandbox: %v\n%s", err, output)
		}
		if !strings.Contains(output, "--agent-sandbox-enabled=true") {
			t.Fatalf("harness-v2 render is missing the agent sandbox flag:\n%s", output)
		}
		if !strings.Contains(output, "--acp-workspace-dispatch-enabled=true") {
			t.Fatalf("harness-v2 render is missing the workspace dispatch flag:\n%s", output)
		}
	})
}

func TestStaticChartGrantsAgentSandboxRuntimeRBAC(t *testing.T) {
	output, err := helmTemplateStaticChart(t,
		"--set", "controller.agentSandbox.enabled=true",
		"--show-only", "templates/rbac.yaml",
	)
	if err != nil {
		t.Fatalf("helm template rejected agent sandbox RBAC: %v\n%s", err, output)
	}
	start := strings.Index(output, "# Runtime child resources are confined to the v2 installation's runtime namespace.")
	if start < 0 {
		t.Fatalf("rendered RBAC is missing the runtime Role:\n%s", output)
	}
	runtimeRole := output[start:]
	if end := strings.Index(runtimeRole, "\n---"); end >= 0 {
		runtimeRole = runtimeRole[:end]
	}
	for _, want := range []string{
		`apiGroups: ["extensions.agents.x-k8s.io"]`,
		`resources: ["sandboxclaims", "sandboxtemplates", "sandboxwarmpools"]`,
		`verbs: ["create", "delete", "get", "list", "patch", "update", "watch"]`,
		`apiGroups: ["agents.x-k8s.io"]`,
		`resources: ["sandboxes"]`,
	} {
		if !strings.Contains(runtimeRole, want) {
			t.Fatalf("runtime Role is missing %q:\n%s", want, runtimeRole)
		}
	}
}

func TestStaticChartEnablesWorkspaceDispatchForSubstrate(t *testing.T) {
	output, err := helmTemplateStaticChart(t,
		"--set", "controller.substrate.enabled=true",
		"--show-only", "templates/deployment.yaml",
	)
	if err != nil {
		t.Fatalf("helm template rejected harness-v2 Substrate: %v\n%s", err, output)
	}
	for _, want := range []string{"--acp-workspace-dispatch-enabled=true", "--substrate-enabled=true"} {
		if !strings.Contains(output, want) {
			t.Fatalf("harness-v2 render is missing %q:\n%s", want, output)
		}
	}
}

func TestStaticChartPreservesSubstrateCleanupConfigWhenDisabled(t *testing.T) {
	output, err := helmTemplateStaticChart(t,
		"--set", "controller.substrate.enabled=false",
		"--set-string", "controller.substrate.apiEndpoint=cleanup-api.example.test:443",
		"--set-string", "controller.substrate.apiCAFile=/var/run/substrate/ca.crt",
		"--set-string", "controller.substrate.routerUrl=https://cleanup-router.example.test",
		"--set-string", "controller.substrate.actorDnsSuffix=Actors.Example.Test",
		"--show-only", "templates/deployment.yaml",
	)
	if err != nil {
		t.Fatalf("helm template rejected disabled Substrate cleanup config: %v\n%s", err, output)
	}
	for _, want := range []string{
		"--substrate-api-endpoint=cleanup-api.example.test:443",
		"--substrate-api-ca-file=/var/run/substrate/ca.crt",
		"--substrate-router-url=https://cleanup-router.example.test",
		"--substrate-actor-dns-suffix=Actors.Example.Test",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("disabled Substrate render is missing cleanup setting %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "--substrate-enabled=true") {
		t.Fatalf("disabled Substrate render unexpectedly enables admission:\n%s", output)
	}
}
