package main

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	distributionref "github.com/distribution/reference"
	"sigs.k8s.io/yaml"
)

// Use only the cluster-specific settings from the installation guide. The usual
// test helper supplies image digests and would hide missing release defaults.
func renderInstallationDefaults(t *testing.T, overrides ...string) (string, error) {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is required for chart render tests")
	}
	args := []string{
		"template", "orka", "static", "--namespace", "orka-install-test",
		"--show-only", "templates/deployment.yaml",
		"--show-only", "templates/publisher-deployment.yaml",
		"--show-only", "templates/serviceaccount.yaml",
		"--show-only", "templates/gateway-task-admission-policy.yaml",
	}
	output, err := exec.Command(helm, append(args, overrides...)...).CombinedOutput()
	return string(output), err
}

func TestStaticChartUsesReleaseTagsByDefault(t *testing.T) {
	chart, err := os.ReadFile("static/Chart.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var metadata struct {
		AppVersion string `json:"appVersion"`
	}
	if err := yaml.Unmarshal(chart, &metadata); err != nil {
		t.Fatal(err)
	}
	version := strings.TrimPrefix(metadata.AppVersion, "v")
	rendered, err := renderInstallationDefaults(t)
	if err != nil {
		t.Fatalf("installation defaults do not render: %v\n%s", err, rendered)
	}
	for _, image := range []string{
		"orka", "orka/workspace-publisher", "orka/ai-worker", "orka/general-worker",
		"orka/acp-codex-runtime", "orka/acp-claude-runtime", "orka/acp-copilot-runtime", "orka/acp-opencode-runtime",
	} {
		want := "ghcr.io/orka-agents/" + image + ":" + version
		if !regexp.MustCompile(regexp.QuoteMeta(want) + `["\s]`).MatchString(rendered) {
			t.Errorf("installation defaults do not select release image %s", want)
		}
	}
	for _, want := range []string{
		"--watch-namespace=orka-install-test",
		"namespace: orka-install-test",
		"gateway.orka.ai/orka-install-test/",
		"--controller-mode=harness-v2",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("installation defaults are missing %q", want)
		}
	}
	for _, forbidden := range []string{"--acp-provider-proxy-", "vekil"} {
		if strings.Contains(rendered, forbidden) {
			t.Errorf("installation preconfigured a model gateway: %q", forbidden)
		}
	}
}

func TestStaticChartDigestOverridesTakePrecedence(t *testing.T) {
	digest := "sha256:" + strings.Repeat("1", 64)
	overrides := make([]string, 0, 24)
	for _, setting := range []string{"controller.image", "publisher.image", "workers.ai.image", "workers.general.image"} {
		overrides = append(overrides, "--set-string", setting+".digest="+digest, "--set-string", setting+".tag=ignored-tag")
	}
	for _, provider := range []string{"codex", "claude", "copilot", "opencode"} {
		overrides = append(overrides, "--set-string",
			"controller.acpRuntime."+provider+"Image=registry.example/"+provider+"@"+digest)
	}
	rendered, err := renderInstallationDefaults(t, overrides...)
	if err != nil {
		t.Fatalf("digest overrides do not render: %v\n%s", err, rendered)
	}
	for _, image := range []string{
		"ghcr.io/orka-agents/orka", "ghcr.io/orka-agents/orka/workspace-publisher",
		"ghcr.io/orka-agents/orka/ai-worker", "ghcr.io/orka-agents/orka/general-worker",
		"registry.example/codex", "registry.example/claude", "registry.example/copilot", "registry.example/opencode",
	} {
		if !strings.Contains(rendered, image+"@"+digest) {
			t.Errorf("digest override is missing for %s", image)
		}
	}
	if strings.Contains(rendered, "ignored-tag") {
		t.Error("a tag overrode an explicit digest")
	}
}

func TestStaticChartRejectsInvalidImageOverrides(t *testing.T) {
	for _, override := range []string{
		"controller.image.digest=not-a-digest",
		"publisher.image.digest=not-a-digest",
		"workers.ai.image.digest=not-a-digest",
		"controller.image.tag=invalid/tag",
		"controller.acpRuntime.codexImage=registry.example/codex",
		"controller.acpRuntime.claudeImage=registry.example/claude@sha256:broken",
	} {
		t.Run(override, func(t *testing.T) {
			if _, err := renderInstallationDefaults(t, "--set-string", override); err == nil {
				t.Error("chart accepted an invalid image override")
			}
		})
	}
}

func TestStaticChartRuntimeImagesMatchCanonicalParser(t *testing.T) {
	digest := "sha256:" + strings.Repeat("1", 64)
	fallbackRegistry := "registry_internal.example.com/"
	for _, ref := range []string{
		"ghcr.io/orka-agents/runtime:v0.2.0",
		"registry.example:5000/orka/runtime:v1",
		"registry:5000/runtime:v1", "localhost/runtime:v1",
		"[2001:db8::1]:5000/orka/runtime:v1",
		"docker.io/library/runtime:v1", "GHCR.IO/orka/runtime:Tag_1",
		"registry.example/orka__agents/runtime-name:v1",
		"ghcr.io/orka/runtime@" + digest, "ghcr.io/orka/runtime:v1@" + digest,
		fallbackRegistry + "orka/runtime:v1", fallbackRegistry + "orka/runtime@" + digest,
		fallbackRegistry + strings.Repeat("a", 255-len(fallbackRegistry)) + ":v1",
		fallbackRegistry + strings.Repeat("a", 256-len(fallbackRegistry)) + ":v1",
		"acme/runtime:v1", "runtime:v1", "acme/runtime@" + digest,
		"localhost:v1", "localhost@" + digest, "ghcr.io:v1", "registry_internal.example.com:v1",
		"ghcr.io/orka-agents/Runtime:v1", "ghcr.io/orka//runtime:v1",
		"ghcr.io/orka/.runtime:v1", "ghcr.io/orka/runtime..name:v1",
		"docker.io/runtime:v1", "index.docker.io/library/runtime:v1",
		"ghcr.io/orka/runtime:v1@sha256:broken",
		"ghcr.io/" + strings.Repeat("a", 255) + ":v1",
		"ghcr.io/" + strings.Repeat("a", 256) + ":v1",
	} {
		t.Run(ref, func(t *testing.T) {
			_, parseErr := distributionref.ParseNamed(ref)
			_, renderErr := renderInstallationDefaults(t, "--set-string", "controller.acpRuntime.codexImage="+ref)
			if (parseErr == nil) != (renderErr == nil) {
				t.Fatalf("runtime image validation differs: controller parser = %v; Helm = %v", parseErr, renderErr)
			}
		})
	}
}

func TestStaticChartCanDisableOneRuntime(t *testing.T) {
	rendered, err := renderInstallationDefaults(t, "--set-string", "controller.acpRuntime.codexImage=")
	if err != nil {
		t.Fatalf("empty runtime override does not render: %v\n%s", err, rendered)
	}
	if strings.Contains(rendered, "--acp-codex-runtime-image=") {
		t.Error("chart configured a disabled runtime")
	}
	if !strings.Contains(rendered, "--acp-claude-runtime-image=") {
		t.Error("disabling Codex also disabled Claude")
	}
}
