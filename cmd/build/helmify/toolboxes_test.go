package main

import (
	"strings"
	"testing"
)

func TestStaticChartRendersToolboxFlags(t *testing.T) {
	t.Run("off by default", func(t *testing.T) {
		output, err := helmTemplateStaticChart(t, "--show-only", "templates/deployment.yaml")
		if err != nil {
			t.Fatalf("helm template: %v\n%s", err, output)
		}
		if strings.Contains(output, "--acp-toolbox") {
			t.Fatalf("default render unexpectedly enabled toolboxes:\n%s", output)
		}
	})

	t.Run("enabled with registries, pull secrets, node selector and mount method", func(t *testing.T) {
		output, err := helmTemplateStaticChart(t,
			"--set", "controller.acpRuntime.toolboxes.enabled=true",
			"--set", "controller.acpRuntime.toolboxes.allowedRegistries[0]=registry.example.com/tools",
			"--set", "controller.acpRuntime.toolboxes.allowedRegistries[1]=ghcr.io/my-org",
			"--set", "controller.acpRuntime.toolboxes.imagePullSecrets[0]=toolbox-pull",
			"--set-string", "controller.acpRuntime.toolboxes.nodeSelector.kubernetes\\.io/arch=amd64",
			"--set", "controller.acpRuntime.toolboxes.mountMethod=imageVolume",
			"--show-only", "templates/deployment.yaml",
		)
		if err != nil {
			t.Fatalf("helm template: %v\n%s", err, output)
		}
		for _, want := range []string{
			"--acp-toolboxes-enabled=true",
			`"--acp-toolbox-allowed-registries=registry.example.com/tools,ghcr.io/my-org"`,
			`"--acp-toolbox-image-pull-secrets=toolbox-pull"`,
			`"--acp-toolbox-node-selector=kubernetes.io/arch=amd64"`,
			"--acp-toolbox-mount-method=imageVolume",
		} {
			if !strings.Contains(output, want) {
				t.Fatalf("render is missing %s:\n%s", want, output)
			}
		}
	})
}
