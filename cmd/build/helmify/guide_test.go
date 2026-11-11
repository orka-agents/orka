package main

import (
	"strings"
	"testing"
)

func guideChartArgs(extra ...string) []string {
	args := []string{
		"--set", "controller.acpRuntime.toolboxes.enabled=true",
		"--set", "controller.acpRuntime.toolboxes.allowedRegistries[0]=registry.example.com",
		"--set", "controller.acpRuntime.toolboxes.allowedRegistries[1]=ghcr.io/orka-agents",
		"--set", "guide.enabled=true",
		"--set-string", "guide.model=claude-test",
		"--set-string", "guide.docs.image.digest=sha256:" + strings.Repeat("a", 64),
	}
	return append(append(args, extra...), "--show-only", "templates/deployment.yaml")
}

func TestStaticChartRendersGuideFlags(t *testing.T) {
	t.Run("off by default", func(t *testing.T) {
		output, err := helmTemplateStaticChart(t, "--show-only", "templates/deployment.yaml")
		if err != nil {
			t.Fatalf("helm template: %v\n%s", err, output)
		}
		if strings.Contains(output, "--guide-") {
			t.Fatalf("default render unexpectedly enabled the guide:\n%s", output)
		}
	})

	t.Run("enabled", func(t *testing.T) {
		output, err := helmTemplateStaticChart(t, guideChartArgs(
			"--set", "guide.runtime=codex",
			"--set", "guide.maxTurns=12",
		)...)
		if err != nil {
			t.Fatalf("helm template: %v\n%s", err, output)
		}
		for _, want := range []string{
			"- --guide-enabled=true",
			`- "--guide-runtime=codex"`,
			`- "--guide-model=claude-test"`,
			"- --guide-max-turns=12",
			`- "--guide-docs-image=ghcr.io/orka-agents/orka/guide-docs@sha256:` + strings.Repeat("a", 64) + `"`,
			`- "--guide-owner-deployment=test-orka-controller"`,
		} {
			if !strings.Contains(output, want) {
				t.Fatalf("render is missing %s:\n%s", want, output)
			}
		}
	})

	t.Run("reuse-values from a release without guide values", func(t *testing.T) {
		output, err := helmTemplateStaticChart(t, "--set", "guide=null", "--show-only", "templates/deployment.yaml")
		if err != nil {
			t.Fatalf("helm template: %v\n%s", err, output)
		}
		if strings.Contains(output, "--guide-") {
			t.Fatalf("missing guide values enabled the guide:\n%s", output)
		}
	})

	for _, tt := range []struct {
		name  string
		extra []string
		want  string
	}{
		{"opencode", []string{"--set", "guide.runtime=opencode"}, `guide.runtime "opencode" is not supported`},
		{"runtime image unset", []string{"--set-string", "controller.acpRuntime.claudeImage="}, "needs controller.acpRuntime.claudeImage"},
		{"no model", []string{"--set-string", "guide.model= "}, "guide.model is required"},
		{"zero turns", []string{"--set", "guide.maxTurns=0"}, "guide.maxTurns must be"},
		{"too many turns", []string{"--set", "guide.maxTurns=1001"}, "guide.maxTurns must be"},
		{"fractional turns", []string{"--set", "guide.maxTurns=2.5"}, "guide.maxTurns must be"},
		{"provider proxy off", []string{"--set", "providerProxy.enabled=false"}, "needs providerProxy.enabled=true"},
		{"no digest", []string{"--set-string", "guide.docs.image.digest="}, "guide.docs.image.digest must be a sha256 digest"},
		{"tag as digest", []string{"--set-string", "guide.docs.image.digest=v1.2.3"}, "guide.docs.image.digest must be a sha256 digest"},
		{"no repository", []string{"--set-string", "guide.docs.image.repository="}, "guide.docs.image.repository is required"},
		{"toolboxes off", []string{"--set", "controller.acpRuntime.toolboxes.enabled=false"}, "needs controller.acpRuntime.toolboxes.enabled=true"},
		{"registry not allowed", []string{"--set-string", "guide.docs.image.repository=ghcr.io/someone/guide-docs"}, "is not covered by controller.acpRuntime.toolboxes.allowedRegistries"},
		{"partial segment", []string{"--set-string", "guide.docs.image.repository=ghcr.io/orka-agents-mirror/guide-docs"}, "is not covered by"},
	} {
		t.Run("rejects "+tt.name, func(t *testing.T) {
			output, err := helmTemplateStaticChart(t, guideChartArgs(tt.extra...)...)
			if err == nil || !strings.Contains(output, tt.want) {
				t.Fatalf("helm render error = %v, want %q:\n%s", err, tt.want, output)
			}
		})
	}
}
