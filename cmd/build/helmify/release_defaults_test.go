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

// Do not inject image defaults here: source charts must reject missing images
// rather than silently selecting an older published release.
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

func developmentImageArgs() []string {
	args := make([]string, 0, 8)
	for _, setting := range []string{"controller.image", "publisher.image", "workers.ai.image", "workers.general.image"} {
		args = append(args, "--set-string", setting+".tag=source-test")
	}
	return args
}

func TestStaticChartDevelopmentDefaultsRequireImages(t *testing.T) {
	chart, err := os.ReadFile("static/Chart.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var metadata struct {
		Version    string `json:"version"`
		AppVersion string `json:"appVersion"`
	}
	if err := yaml.Unmarshal(chart, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.AppVersion != "v0.0.0-dev" {
		t.Skip("release preparation supplies release image defaults")
	}
	if metadata.Version != "0.0.0-dev" || metadata.AppVersion != "v0.0.0-dev" {
		t.Fatalf("source chart claims a release version: %#v", metadata)
	}
	values, err := os.ReadFile("static/values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if got := len(regexp.MustCompile(`(?m)^\s+tag: ""$`).FindAll(values, -1)); got != 5 {
		t.Fatalf("source chart has %d unset image tags, want all five", got)
	}
	for _, provider := range []string{"codex", "claude", "copilot", "opencode"} {
		if !strings.Contains(string(values), provider+`Image: ""`) {
			t.Errorf("source chart supplied an image default for %s", provider)
		}
	}
	const missingImageMessage = "development charts require images built from the same checkout"
	if rendered, err := renderInstallationDefaults(t); err == nil || !strings.Contains(rendered, missingImageMessage) {
		t.Fatalf("source chart did not reject missing images: %v\n%s", err, rendered)
	}
	for _, setting := range []string{"controller.image", "publisher.image", "workers.ai.image", "workers.general.image"} {
		t.Run(setting, func(t *testing.T) {
			args := append(developmentImageArgs(), "--set-string", setting+".tag=")
			if _, err := renderInstallationDefaults(t, args...); err == nil {
				t.Error("source chart accepted a missing image tag and digest")
			}
		})
	}
	rendered, err := renderInstallationDefaults(t, developmentImageArgs()...)
	if err != nil {
		t.Fatalf("source-matched image overrides do not render: %v\n%s", err, rendered)
	}
	for _, image := range []string{"orka", "orka/workspace-publisher", "orka/ai-worker", "orka/general-worker"} {
		if !strings.Contains(rendered, "ghcr.io/orka-agents/"+image+":source-test") {
			t.Errorf("source chart did not use the supplied image for %s", image)
		}
	}
	for _, provider := range []string{"codex", "claude", "copilot", "opencode"} {
		if strings.Contains(rendered, "--acp-"+provider+"-runtime-image=") {
			t.Errorf("source chart enabled %s without a supplied image", provider)
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
	if version == "0.0.0-dev" {
		t.Skip("development charts require explicit source-matched images")
	}
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
			if _, err := renderInstallationDefaults(t, append(developmentImageArgs(), "--set-string", override)...); err == nil {
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
			args := append(developmentImageArgs(), "--set-string", "controller.acpRuntime.codexImage="+ref)
			_, renderErr := renderInstallationDefaults(t, args...)
			if (parseErr == nil) != (renderErr == nil) {
				t.Fatalf("runtime image validation differs: controller parser = %v; Helm = %v", parseErr, renderErr)
			}
		})
	}
}

func TestStaticChartCanDisableOneRuntime(t *testing.T) {
	rendered, err := renderInstallationDefaults(t, append(developmentImageArgs(),
		"--set-string", "controller.acpRuntime.claudeImage=registry.example/claude:source-test",
		"--set-string", "controller.acpRuntime.codexImage=")...)
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
