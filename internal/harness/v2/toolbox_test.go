package v2

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const testToolboxDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func testToolboxProfile(toolboxes []RuntimeToolbox) RuntimeProfile {
	return RuntimeProfile{
		ACPProfile:               ACPProfileV1,
		AdapterDigests:           map[string]string{"codex-acp": testToolboxDigest},
		ProviderKind:             "codex",
		Model:                    "gpt-test",
		AgentConfigurationDigest: testToolboxDigest,
		ToolPolicyDigest:         testToolboxDigest,
		ApprovalPolicyDigest:     testToolboxDigest,
		MCPConfigurationDigest:   testToolboxDigest,
		WorkspaceIntent:          WorkspaceIntentRead,
		ProxyCredentialRole:      "provider-inference",
		ProxyCredentialScope:     "model:gpt-test",
		ResourceClass:            "standard",
		Toolboxes:                toolboxes,
	}
}

func TestValidateRuntimeToolboxImage(t *testing.T) {
	valid := []string{
		"registry.example.com/tools/yq-jq@" + testToolboxDigest,
		"ghcr.io/my-org/tools@" + testToolboxDigest,
		"localhost:5001/tools/yq@" + testToolboxDigest,
		"registry.example.com:5000/a/b/c@" + testToolboxDigest,
	}
	for _, image := range valid {
		if err := ValidateRuntimeToolboxImage(image); err != nil {
			t.Errorf("%s: unexpected error %v", image, err)
		}
	}
	invalid := map[string]string{
		"tag only":              "registry.example.com/tools/yq:1",
		"tag and digest":        "registry.example.com/tools/yq:1@" + testToolboxDigest,
		"no registry host":      "yq-jq@" + testToolboxDigest,
		"library shorthand":     "library/yq@" + testToolboxDigest,
		"short digest":          "registry.example.com/tools/yq@sha256:abc",
		"uppercase repository":  "registry.example.com/Tools/yq@" + testToolboxDigest,
		"empty":                 "",
		"whitespace":            "registry.example.com/tools/yq @" + testToolboxDigest,
		"missing repository":    "registry.example.com@" + testToolboxDigest,
		"too long":              "registry.example.com/" + strings.Repeat("a", 600) + "@" + testToolboxDigest,
		"sha512":                "registry.example.com/tools/yq@sha512:" + strings.Repeat("a", 128),
		"trailing slash before": "registry.example.com/tools/@" + testToolboxDigest,
		"repeated periods":      "registry.example.com/tools/a..b@" + testToolboxDigest,
		"double hyphen host":    "registry..example.com/tools/yq@" + testToolboxDigest,
	}
	for name, image := range invalid {
		if err := ValidateRuntimeToolboxImage(image); err == nil {
			t.Errorf("%s (%q): expected error", name, image)
		}
	}
}

func TestValidateRuntimeToolboxMountPath(t *testing.T) {
	valid := []string{"/opt/yq-jq", "/opt/tools", "/opt/a.b_c-d", RuntimeToolboxHomebrewMountPath}
	for _, mountPath := range valid {
		if err := ValidateRuntimeToolboxMountPath(mountPath); err != nil {
			t.Errorf("%s: unexpected error %v", mountPath, err)
		}
	}
	invalid := []string{
		"", "opt/tools", "/opt", "/opt/", "/opt/tools/", "/opt//tools", "/opt/tools/bin", "/opt/..", "/opt/.", "/opt/../etc",
		"/usr/local/tools", "/home/linuxbrew", "/home/linuxbrew/.linuxbrew/bin", "/opt/to:ols", "/opt/to\x00ols", "/opt/to\nols",
		"/opt/-tools", "/opt/" + strings.Repeat("a", 129),
		"/opt/codex", "/opt/codex-acp", "/opt/claude", "/opt/claude-agent-acp", "/opt/copilot", "/opt/opencode", "/opt/ripgrep", "/opt/yarn-v1.22.22",
	}
	for _, mountPath := range invalid {
		if err := ValidateRuntimeToolboxMountPath(mountPath); err == nil {
			t.Errorf("%q: expected error", mountPath)
		}
	}
}

func TestRuntimeToolboxValidatePathEntries(t *testing.T) {
	base := RuntimeToolbox{Image: "registry.example.com/tools/yq@" + testToolboxDigest, MountPath: "/opt/yq"}
	valid := [][]string{nil, {}, {"bin"}, {"bin", "sbin"}, {"usr/bin"}, {"a", "b", "c", "d", "e", "f", "g", "h"}}
	for _, entries := range valid {
		toolbox := base
		toolbox.PathEntries = entries
		if err := toolbox.Validate(); err != nil {
			t.Errorf("%v: unexpected error %v", entries, err)
		}
	}
	invalid := [][]string{
		{"/bin"}, {"../bin"}, {"bin/../sbin"}, {"."}, {""}, {"bin/"}, {"bin//sbin"}, {"./bin"}, {"bi:n"}, {"bin,legacy"}, {"bin$cache"}, {"bin cache"}, {"bin`id`"}, {"bin\\x"}, {"bin'x"}, {"bin", "bin"},
		{"a", "b", "c", "d", "e", "f", "g", "h", "i"}, {strings.Repeat("a", 257)}, {"bi\x00n"},
	}
	for _, entries := range invalid {
		toolbox := base
		toolbox.PathEntries = entries
		if err := toolbox.Validate(); err == nil {
			t.Errorf("%q: expected error", entries)
		}
	}
}

func TestValidateRuntimeToolboxesLimitsAndOverlap(t *testing.T) {
	make := func(mountPath string) RuntimeToolbox {
		return RuntimeToolbox{Image: "registry.example.com/tools/x@" + testToolboxDigest, MountPath: mountPath, PathEntries: []string{"bin"}}
	}
	if err := ValidateRuntimeToolboxes(nil); err != nil {
		t.Fatalf("nil list: %v", err)
	}
	if err := ValidateRuntimeToolboxes([]RuntimeToolbox{make("/opt/a"), make("/opt/b"), make("/opt/c"), make(RuntimeToolboxHomebrewMountPath)}); err != nil {
		t.Fatalf("four distinct toolboxes: %v", err)
	}
	if err := ValidateRuntimeToolboxes([]RuntimeToolbox{make("/opt/a"), make("/opt/b"), make("/opt/c"), make("/opt/d"), make("/opt/e")}); err == nil {
		t.Fatal("five toolboxes: expected error")
	}
	if err := ValidateRuntimeToolboxes([]RuntimeToolbox{make("/opt/a"), make("/opt/a")}); err == nil {
		t.Fatal("duplicate mount path: expected error")
	}
	// Nested paths cannot pass the mountPath shape rule, but the overlap
	// check must stay independent of it.
	if !runtimeToolboxMountPathsOverlap("/opt/a", "/opt/a/bin") || !runtimeToolboxMountPathsOverlap("/opt/a/bin", "/opt/a") {
		t.Fatal("nested mount paths must overlap")
	}
	if runtimeToolboxMountPathsOverlap("/opt/a", "/opt/ab") {
		t.Fatal("sibling prefix must not overlap")
	}
}

func TestRuntimeProfileToolboxDigestIsOrderAndFieldSensitive(t *testing.T) {
	toolbox := RuntimeToolbox{Image: "registry.example.com/tools/yq@" + testToolboxDigest, MountPath: "/opt/yq", PathEntries: []string{"bin", "sbin"}}
	other := RuntimeToolbox{Image: "registry.example.com/tools/jq@" + testToolboxDigest, MountPath: "/opt/jq", PathEntries: []string{"bin"}}
	base := testToolboxProfile([]RuntimeToolbox{toolbox, other})
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	baseDigest, err := CanonicalProfileDigest(base)
	if err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(*RuntimeProfile){
		"image": func(p *RuntimeProfile) {
			p.Toolboxes[0].Image = "registry.example.com/tools/yq@sha256:" + strings.Repeat("f", 64)
		},
		"mountPath":     func(p *RuntimeProfile) { p.Toolboxes[0].MountPath = "/opt/yq2" },
		"pathEntries":   func(p *RuntimeProfile) { p.Toolboxes[0].PathEntries = []string{"bin"} },
		"entry order":   func(p *RuntimeProfile) { p.Toolboxes[0].PathEntries = []string{"sbin", "bin"} },
		"toolbox order": func(p *RuntimeProfile) { p.Toolboxes[0], p.Toolboxes[1] = p.Toolboxes[1], p.Toolboxes[0] },
		"removed":       func(p *RuntimeProfile) { p.Toolboxes = p.Toolboxes[:1] },
	}
	for name, mutate := range mutations {
		changed := base
		changed.Toolboxes = CloneRuntimeToolboxes(base.Toolboxes)
		mutate(&changed)
		digest, err := CanonicalProfileDigest(changed)
		if err != nil {
			t.Fatal(err)
		}
		if digest == baseDigest {
			t.Errorf("%s: profile digest did not change", name)
		}
	}
	// A pure copy keeps the digest, proving the mutations above are the only
	// source of change.
	same := base
	same.Toolboxes = CloneRuntimeToolboxes(base.Toolboxes)
	if digest, _ := CanonicalProfileDigest(same); digest != baseDigest {
		t.Fatal("cloned profile digest changed")
	}
}

// TestRuntimeProfileWithoutToolboxesIsUnchanged is the no-change golden test:
// a profile without toolboxes serializes and digests exactly like the shape
// that existed before the field was added.
func TestRuntimeProfileWithoutToolboxesIsUnchanged(t *testing.T) {
	legacy := testToolboxProfile(nil)
	legacyJSON, err := CanonicalValue(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(legacyJSON), "toolboxes") {
		t.Fatalf("profile without toolboxes must not serialize a toolboxes key: %s", legacyJSON)
	}
	empty := testToolboxProfile([]RuntimeToolbox{})
	emptyJSON, err := CanonicalValue(empty)
	if err != nil {
		t.Fatal(err)
	}
	if string(emptyJSON) != string(legacyJSON) {
		t.Fatalf("empty toolbox list must serialize exactly like nil:\n%s\n%s", emptyJSON, legacyJSON)
	}
	type preToolboxProfile struct {
		ACPProfile               string            `json:"acpProfile"`
		AdapterDigests           map[string]string `json:"adapterDigests"`
		ProviderKind             string            `json:"providerKind"`
		Model                    string            `json:"model"`
		ModelLimits              *ModelTokenLimits `json:"modelLimits,omitempty"`
		AgentConfigurationDigest string            `json:"agentConfigurationDigest"`
		ToolPolicyDigest         string            `json:"toolPolicyDigest"`
		ApprovalPolicyDigest     string            `json:"approvalPolicyDigest"`
		MCPConfigurationDigest   string            `json:"mcpConfigurationDigest"`
		WorkspaceIntent          WorkspaceIntent   `json:"workspaceIntent"`
		ProxyCredentialRole      string            `json:"proxyCredentialRole"`
		ProxyCredentialScope     string            `json:"proxyCredentialScope"`
		ResourceClass            string            `json:"resourceClass"`
	}
	golden, err := CanonicalProfileDigest(preToolboxProfile{
		ACPProfile: legacy.ACPProfile, AdapterDigests: legacy.AdapterDigests, ProviderKind: legacy.ProviderKind,
		Model: legacy.Model, ModelLimits: legacy.ModelLimits, AgentConfigurationDigest: legacy.AgentConfigurationDigest,
		ToolPolicyDigest: legacy.ToolPolicyDigest, ApprovalPolicyDigest: legacy.ApprovalPolicyDigest,
		MCPConfigurationDigest: legacy.MCPConfigurationDigest, WorkspaceIntent: legacy.WorkspaceIntent,
		ProxyCredentialRole: legacy.ProxyCredentialRole, ProxyCredentialScope: legacy.ProxyCredentialScope,
		ResourceClass: legacy.ResourceClass,
	})
	if err != nil {
		t.Fatal(err)
	}
	current, err := CanonicalProfileDigest(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if current != golden {
		t.Fatalf("profile digest without toolboxes changed: %s != %s", current, golden)
	}
}

func TestRuntimeToolboxChildPath(t *testing.T) {
	toolboxes := []RuntimeToolbox{
		{Image: "registry.example.com/tools/yq@" + testToolboxDigest, MountPath: "/opt/yq", PathEntries: []string{"bin", "sbin"}},
		{Image: "registry.example.com/tools/brew@" + testToolboxDigest, MountPath: RuntimeToolboxHomebrewMountPath, PathEntries: []string{"bin"}},
		{Image: "registry.example.com/tools/none@" + testToolboxDigest, MountPath: "/opt/none"},
	}
	got := RuntimeToolboxChildPath("/usr/local/bin:/usr/bin:/bin", toolboxes)
	want := "/usr/local/bin:/usr/bin:/bin:/opt/yq/bin:/opt/yq/sbin:/home/linuxbrew/.linuxbrew/bin"
	if got != want {
		t.Fatalf("child PATH = %q, want %q", got, want)
	}
	if RuntimeToolboxChildPath("/usr/bin", nil) != "/usr/bin" {
		t.Fatal("no toolboxes must leave the base PATH unchanged")
	}
}

// TestReservedOptNamesCoverRuntimeImages scans every built-in runtime
// Dockerfile for /opt/<name> folders and fails when one is missing from the
// reserved list, so a new runtime folder cannot be shadowed by a toolbox.
func TestReservedOptNamesCoverRuntimeImages(t *testing.T) {
	root := filepath.Join("..", "..", "..", "workers", "acp", "images")
	matches, err := filepath.Glob(filepath.Join(root, "*", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatalf("no runtime Dockerfiles found under %s", root)
	}
	reserved := map[string]struct{}{}
	for _, name := range RuntimeToolboxReservedOptNames {
		reserved[name] = struct{}{}
	}
	pattern := regexp.MustCompile(`/opt/([A-Za-z0-9][A-Za-z0-9._-]*)`)
	for _, dockerfile := range matches {
		provider := filepath.Base(filepath.Dir(dockerfile))
		switch provider {
		case "codex", "claude", "copilot", "opencode":
		default:
			// Composition images (AgentKit, Foundry) are not built-in
			// toolbox-capable runtimes.
			continue
		}
		data, err := os.ReadFile(dockerfile)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range pattern.FindAllStringSubmatch(string(data), -1) {
			if _, ok := reserved[match[1]]; !ok {
				t.Errorf("%s uses /opt/%s, which is missing from RuntimeToolboxReservedOptNames", dockerfile, match[1])
			}
		}
	}
}
