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
	})
}
