package main

import (
	"strings"
	"testing"
)

func guideChartArgs(extra ...string) []string {
	args := make([]string, 0, 14+len(extra))
	args = append(args,
		"--set", "controller.acpRuntime.toolboxes.enabled=true",
		"--set", "controller.acpRuntime.toolboxes.allowedRegistries[0]=registry.example.com",
		"--set", "controller.acpRuntime.toolboxes.allowedRegistries[1]=ghcr.io/orka-agents",
		"--set", "guide.enabled=true",
		"--set-string", "guide.model=claude-test",
		"--set-string", "guide.docs.image.digest=sha256:"+strings.Repeat("a", 64),
	)
	return append(append(args, extra...), "--show-only", "templates/deployment.yaml")
}

func TestStaticChartRendersGuideFlags(t *testing.T) {

	// With the guide off, only the owner Deployment is passed, so the
	// controller can still recognize and delete a guide Agent it created.
	onlyOwner := func(t *testing.T, output string) {
		t.Helper()
		withoutOwner := strings.ReplaceAll(output, `- "--guide-owner-deployment=test-orka-controller"`+"\n", "")
		if withoutOwner == output || strings.Contains(withoutOwner, "--guide-") {
			t.Fatalf("a render with the guide off must pass only --guide-owner-deployment:\n%s", output)
		}
	}

	t.Run("off by default", func(t *testing.T) {
		output, err := helmTemplateStaticChart(t, "--show-only", "templates/deployment.yaml")
		if err != nil {
			t.Fatalf("helm template: %v\n%s", err, output)
		}
		onlyOwner(t, output)
	})

	t.Run("enabled", func(t *testing.T) {
		output, err := helmTemplateStaticChart(t, guideChartArgs(
			"--set", "guide.runtime=copilot",
			"--set", "guide.maxTurns=12",
		)...)
		if err != nil {
			t.Fatalf("helm template: %v\n%s", err, output)
		}
		for _, want := range []string{
			"- --guide-enabled=true",
			`- "--guide-runtime=copilot"`,
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

	t.Run("turns with leading zeros render as decimal", func(t *testing.T) {
		for value, want := range map[string]string{"030": "30", "08": "8"} {
			output, err := helmTemplateStaticChart(t, guideChartArgs("--set-string", "guide.maxTurns="+value)...)
			if err != nil {
				t.Fatalf("helm template: %v\n%s", err, output)
			}
			if !strings.Contains(output, "- --guide-max-turns="+want+"\n") {
				t.Fatalf("guide.maxTurns=%s must render --guide-max-turns=%s:\n%s", value, want, output)
			}
		}
	})

	t.Run("registry with a port", func(t *testing.T) {
		output, err := helmTemplateStaticChart(t, guideChartArgs(
			"--set", "controller.acpRuntime.toolboxes.allowedRegistries[2]=localhost:5000",
			"--set-string", "guide.docs.image.repository=localhost:5000/orka/guide-docs",
		)...)
		if err != nil {
			t.Fatalf("helm template: %v\n%s", err, output)
		}
		if !strings.Contains(output, `"--guide-docs-image=localhost:5000/orka/guide-docs@sha256:`) {
			t.Fatalf("render is missing the local registry image:\n%s", output)
		}
	})

	t.Run("Docker Hub namespace", func(t *testing.T) {
		output, err := helmTemplateStaticChart(t, guideChartArgs(
			"--set", "controller.acpRuntime.toolboxes.allowedRegistries[2]=docker.io/acme",
			"--set-string", "guide.docs.image.repository=docker.io/acme/guide-docs",
		)...)
		if err != nil {
			t.Fatalf("helm template: %v\n%s", err, output)
		}
		if !strings.Contains(output, `"--guide-docs-image=docker.io/acme/guide-docs@sha256:`) {
			t.Fatalf("render is missing the Docker Hub image:\n%s", output)
		}
	})

	t.Run("reuse-values from a release without guide values", func(t *testing.T) {
		output, err := helmTemplateStaticChart(t, "--set", "guide=null", "--show-only", "templates/deployment.yaml")
		if err != nil {
			t.Fatalf("helm template: %v\n%s", err, output)
		}
		onlyOwner(t, output)
	})

	set := func(key, value string) []string { return []string{"--set", key + "=" + value} }
	str := func(key, value string) []string { return []string{"--set-string", key + "=" + value} }
	const (
		repository      = "guide.docs.image.repository"
		digest          = "guide.docs.image.digest"
		toolboxes       = "controller.acpRuntime.toolboxes.enabled"
		notCovered      = "is not covered by controller.acpRuntime.toolboxes.allowedRegistries"
		notCanonical    = "must be <registry host>/<lowercase path>"
		notHubCanonical = "is not canonical: use docker.io/<namespace>/<name>"
		notSHA256       = "guide.docs.image.digest must be a sha256 digest"
		badTurns        = "guide.maxTurns must be"
	)
	for _, tt := range []struct {
		name  string
		extra []string
		want  string
	}{
		{"opencode", set("guide.runtime", "opencode"), `guide.runtime "opencode" is not supported`},
		{"codex", set("guide.runtime", "codex"), `guide.runtime "codex" is not supported`},
		{"runtime image unset", str("controller.acpRuntime.claudeImage", ""), "needs controller.acpRuntime.claudeImage"},
		{"no model", str("guide.model", " "), "guide.model is required"},
		{"zero turns", set("guide.maxTurns", "0"), badTurns},
		{"too many turns", set("guide.maxTurns", "1001"), badTurns},
		{"fractional turns", set("guide.maxTurns", "2.5"), badTurns},
		{"provider proxy off", set("providerProxy.enabled", "false"), "needs providerProxy.enabled=true"},
		{"no digest", str(digest, ""), notSHA256},
		{"tag as digest", str(digest, "v1.2.3"), notSHA256},
		{"no repository", str(repository, ""), "guide.docs.image.repository is required"},
		{"toolboxes off", set(toolboxes, "false"), "needs " + toolboxes + "=true"},
		{"registry not allowed", str(repository, "ghcr.io/someone/guide-docs"), notCovered},
		{"partial segment", str(repository, "ghcr.io/orka-agents-mirror/guide-docs"), notCovered},
		{"no registry host", str(repository, "myregistry/guide-docs"), notCanonical},
		{"host only", str(repository, "ghcr.io"), notCanonical},
		{"uppercase path", str(repository, "ghcr.io/orka-agents/Guide-Docs"), notCanonical},
		{"tag in repository", str(repository, "ghcr.io/orka-agents/guide-docs:v1"), notCanonical},
		{"legacy Docker Hub host", str(repository, "index.docker.io/acme/guide-docs"), notHubCanonical},
		{"Docker Hub name without namespace", str(repository, "docker.io/guide-docs"), notHubCanonical},
	} {
		t.Run("rejects "+tt.name, func(t *testing.T) {
			output, err := helmTemplateStaticChart(t, guideChartArgs(tt.extra...)...)
			if err == nil || !strings.Contains(output, tt.want) {
				t.Fatalf("helm render error = %v, want %q:\n%s", err, tt.want, output)
			}
		})
	}
}
