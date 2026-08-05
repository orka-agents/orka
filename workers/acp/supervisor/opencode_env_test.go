package supervisor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const testVekilV1URL = "http://vekil:1337" + "/v1"

func testOpenCodeModelLimits() *harnessv2.ModelTokenLimits {
	return &harnessv2.ModelTokenLimits{Context: 32768, Output: 4096}
}

//nolint:gocyclo // Table-driven policy coverage intentionally exercises the complete config surface.
func TestOpenCodeProviderProfileIsImmutableAndGoverned(t *testing.T) {
	paths := acp.SessionPaths{Config: "/sessions/private/xdg/config", Home: "/sessions/private/home", Workspace: "/sessions/private/workspace"}
	proxy := ProviderProxyBinding{BaseURL: "http://127.0.0.1:43210/_orka/provider/session/v1", Credential: "placeholder"}
	request := harnessv2.CreateRuntimeSessionRequest{
		MCPConfiguration: harnessv2.MCPPolicyConfiguration{
			ToolPolicy: harnessv2.MCPToolPolicy{
				AllowedToolNames: []string{
					"APPLY_PATCH", "BASH", "EDIT", "GLOB", "GREP", "QUESTION", "READ",
					"SKILL", "TASK", "TODOWRITE", "WEBFETCH", "WEBSEARCH", "WRITE",
					"danger.tool", "web_search",
				},
				DisallowedToolNames: []string{"DANGER.TOOL", "gReP"},
				AllowBash:           true,
				Tools: []harnessv2.MCPToolDescriptor{
					{Name: "READ", Source: harnessv2.MCPToolSourceProviderNative},
					{Name: "danger.tool", Source: harnessv2.MCPToolSourceBrokeredBuiltin},
					{Name: "web_search", Source: harnessv2.MCPToolSourceBrokeredBuiltin},
				},
			},
		},
	}

	for _, test := range []struct {
		intent    harnessv2.WorkspaceIntent
		editRule  string
		writeRule string
		bashRule  string
	}{
		{intent: harnessv2.WorkspaceIntentRead, editRule: openCodePermissionDeny, writeRule: openCodePermissionDeny, bashRule: openCodePermissionDeny},
		{intent: harnessv2.WorkspaceIntentWrite, editRule: openCodePermissionAllow, writeRule: openCodePermissionAllow, bashRule: openCodePermissionAllow},
	} {
		t.Run(string(test.intent), func(t *testing.T) {
			profile, err := providerProfile(providerKindOpencode, "openai/gpt-test", test.intent, testOpenCodeModelLimits())
			if err != nil {
				t.Fatal(err)
			}
			if profile.Command != "/opt/opencode/bin/opencode" || !containsArg(profile.Args, "--pure") || !containsArg(profile.Args, "acp") || !containsArg(profile.Args, "--no-mdns") {
				t.Fatalf("OpenCode does not use the hardened native ACP command: command=%q args=%v", profile.Command, profile.Args)
			}
			digests := providerAdapterDigests(providerKindOpencode)
			if got := digests[profile.AdapterName]; got != profile.AdapterDigest {
				t.Fatalf("OpenCode adapter digest = %q, advertised %q", profile.AdapterDigest, got)
			}
			if digests["opencode-ripgrep-linux-amd64"] != "sha256:"+acp.OpenCodeRipgrepLinuxX64BinarySHA256 ||
				digests["opencode-ripgrep-linux-arm64"] != "sha256:"+acp.OpenCodeRipgrepLinuxARM64BinarySHA256 {
				t.Fatalf("OpenCode ripgrep digests = %#v", digests)
			}

			environment, err := profile.EnvironmentForSession(request, paths, proxy)
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{
				"OPENCODE_DISABLE_AUTOUPDATE",
				"OPENCODE_DISABLE_CLAUDE_CODE",
				"OPENCODE_DISABLE_DEFAULT_PLUGINS",
				"OPENCODE_DISABLE_EMBEDDED_WEB_UI",
				"OPENCODE_DISABLE_EXTERNAL_SKILLS",
				"OPENCODE_DISABLE_LSP_DOWNLOAD",
				"OPENCODE_DISABLE_MODELS_FETCH",
				"OPENCODE_DISABLE_PROJECT_CONFIG",
				"OPENCODE_EXPERIMENTAL_DISABLE_FILEWATCHER",
				"OPENCODE_PURE",
			} {
				if environment[name] != "1" {
					t.Fatalf("%s = %q, want 1", name, environment[name])
				}
			}
			if environment["OPENCODE_CONFIG_DIR"] != "/sessions/private/xdg/config/opencode" {
				t.Fatalf("OPENCODE_CONFIG_DIR = %q", environment["OPENCODE_CONFIG_DIR"])
			}
			if environment[openCodeProviderEnvName] != proxy.Credential {
				t.Fatal("OpenCode did not receive the per-session provider credential")
			}
			if environment["OPENCODE_AUTH_CONTENT"] != "{}" {
				t.Fatalf("OPENCODE_AUTH_CONTENT = %q, want isolated empty auth store", environment["OPENCODE_AUTH_CONTENT"])
			}
			if password := environment["OPENCODE_SERVER_PASSWORD"]; len(password) != 64 || password == proxy.Credential {
				t.Fatalf("OpenCode server password was not independently derived: %q", password)
			}
			if strings.Contains(environment["OPENCODE_CONFIG_CONTENT"], proxy.Credential) {
				t.Fatal("OpenCode config content embeds the per-session provider credential")
			}

			var config map[string]any
			if err := json.Unmarshal([]byte(environment["OPENCODE_CONFIG_CONTENT"]), &config); err != nil {
				t.Fatal(err)
			}
			if config["model"] != "orka/openai/gpt-test" || config["small_model"] != "orka/openai/gpt-test" || config["autoupdate"] != false || config["share"] != "disabled" {
				t.Fatalf("unexpected OpenCode root config: %#v", config)
			}
			if config["lsp"] != false || config["formatter"] != false || config["snapshot"] != false {
				t.Fatalf("OpenCode local discovery/state features are not disabled: %#v", config)
			}
			instructions := config["instructions"].([]any)
			if len(instructions) != 2 || instructions[0] != openCodeRootInstructionPath || instructions[1] != "/sessions/private/workspace/AGENTS.md" {
				t.Fatalf("OpenCode instructions = %#v", instructions)
			}
			enabledProviders := config["enabled_providers"].([]any)
			if len(enabledProviders) != 1 || enabledProviders[0] != openCodeProviderID {
				t.Fatalf("OpenCode enabled providers = %#v", enabledProviders)
			}

			provider := config["provider"].(map[string]any)[openCodeProviderID].(map[string]any)
			if provider["npm"] != "@ai-sdk/openai-compatible" {
				t.Fatalf("unexpected OpenCode provider: %#v", provider)
			}
			whitelist := provider["whitelist"].([]any)
			if len(whitelist) != 1 || whitelist[0] != "openai/gpt-test" {
				t.Fatalf("OpenCode provider whitelist = %#v, want selected model only", whitelist)
			}
			if _, exists := provider["api"]; exists {
				t.Fatalf("OpenCode provider unexpectedly configures api: %#v", provider)
			}
			options := provider["options"].(map[string]any)
			if options["apiKey"] != "{env:"+openCodeProviderEnvName+"}" || options["baseURL"] != proxy.BaseURL {
				t.Fatalf("unexpected OpenCode provider options: %#v", options)
			}
			model := provider["models"].(map[string]any)["openai/gpt-test"].(map[string]any)
			limit := model["limit"].(map[string]any)
			if limit["context"] != float64(32768) || limit["output"] != float64(4096) {
				t.Fatalf("OpenCode model limits = %#v", limit)
			}
			for _, field := range []string{"tool_call", "attachment", "modalities"} {
				if _, exists := model[field]; exists {
					t.Fatalf("OpenCode model override contains unreviewed %s metadata: %#v", field, model)
				}
			}

			permissions := config["permission"].(map[string]any)
			if permissions["*"] != openCodePermissionDeny || permissions["external_directory"] != openCodePermissionDeny ||
				permissions["edit"] != test.editRule || permissions["write"] != test.writeRule || permissions["apply_patch"] != test.writeRule || permissions["bash"] != test.bashRule {
				t.Fatalf("unexpected OpenCode permissions: %#v", permissions)
			}
			for _, name := range []string{"task", "skill", "question", "lsp", "doom_loop"} {
				if permissions[name] != openCodePermissionDeny {
					t.Fatalf("OpenCode permission %q = %#v, want deny", name, permissions[name])
				}
			}
			if permissions["glob"] != openCodePermissionAllow {
				t.Fatalf("OpenCode glob permission = %#v, want allow", permissions["glob"])
			}
			for _, name := range []string{"grep", "list", "todowrite", "webfetch", "websearch"} {
				if permissions[name] != openCodePermissionDeny {
					t.Fatalf("OpenCode permission %q = %#v, want deny from tool policy", name, permissions[name])
				}
			}
			if permissions["orka_web_search"] != openCodePermissionAllow || permissions["orka_danger_tool"] != openCodePermissionDeny {
				t.Fatalf("unexpected brokered MCP permissions: %#v", permissions)
			}
			if _, exists := permissions["orka_READ"]; exists {
				t.Fatalf("provider-native descriptor received brokered MCP permission: %#v", permissions)
			}
			readRules := permissions["read"].(map[string]any)
			if readRules["*"] != openCodePermissionAllow || readRules["*.env"] != openCodePermissionDeny || readRules["*.env.*"] != openCodePermissionDeny || readRules["*.env.example"] != openCodePermissionAllow {
				t.Fatalf("unexpected OpenCode sensitive read rules: %#v", readRules)
			}
			if containsOpenCodePermissionAction(permissions, "ask") {
				t.Fatal("OpenCode permissions contain unsupported ask policy")
			}
		})
	}
}

func TestOpenCodeProviderSessionProjectionValidatesImmutableAgentConfiguration(t *testing.T) {
	profile, err := providerProfile(
		providerKindOpencode,
		"openai/gpt-test",
		harnessv2.WorkspaceIntentWrite,
		testOpenCodeModelLimits(),
	)
	if err != nil {
		t.Fatal(err)
	}
	paths := acp.SessionPaths{Workspace: "/sessions/private/workspace"}
	proxy := ProviderProxyBinding{BaseURL: "http://127.0.0.1:43210/_orka/provider/session/v1", Credential: "placeholder"}
	request := testProviderProjectionRequest(
		t,
		providerKindOpencode,
		"openai/gpt-test",
		"",
		"",
		nil,
		nil,
		true,
	)
	request.Profile.ModelLimits = testOpenCodeModelLimits()
	if _, err := profile.ProjectSession(request, paths, proxy); err != nil {
		t.Fatalf("valid OpenCode session projection error = %v", err)
	}

	request = testProviderProjectionRequest(
		t,
		providerKindOpencode,
		"openai/gpt-test",
		"must not be ignored",
		"",
		nil,
		nil,
		true,
	)
	request.Profile.ModelLimits = testOpenCodeModelLimits()
	if _, err := profile.ProjectSession(request, paths, proxy); err == nil || !strings.Contains(err.Error(), "systemPrompt") {
		t.Fatalf("OpenCode systemPrompt projection error = %v, want rejection", err)
	}

	request.AgentConfiguration = nil
	if _, err := profile.ProjectSession(request, paths, proxy); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("missing OpenCode Agent configuration error = %v, want rejection", err)
	}
}

func TestOpenCodeProviderProfileRejectsConfigSubstitutionModel(t *testing.T) {
	for _, model := range []string{"{env:ORKA_OPENCODE_PROVIDER_TOKEN}", "{file:/proc/self/environ}"} {
		if _, err := providerProfile(providerKindOpencode, model, harnessv2.WorkspaceIntentWrite, testOpenCodeModelLimits()); err == nil ||
			!strings.Contains(err.Error(), "substitution braces") {
			t.Fatalf("providerProfile(%q) error = %v, want substitution rejection", model, err)
		}
	}
}

func TestPrepareOpenCodeConfigPreventsDependencyInstall(t *testing.T) {
	paths := acp.SessionPaths{Config: filepath.Join(t.TempDir(), "xdg", "config")}
	profile, err := providerProfile(providerKindOpencode, "openai/gpt-test", harnessv2.WorkspaceIntentRead, testOpenCodeModelLimits())
	if err != nil {
		t.Fatal(err)
	}
	if profile.PrepareSession == nil {
		t.Fatal("OpenCode profile does not prepare its private config directory")
	}
	if err := profile.PrepareSession(paths); err != nil {
		t.Fatal(err)
	}

	configDir := filepath.Join(paths.Config, "opencode")
	nodeModules := filepath.Join(configDir, "node_modules")
	info, err := os.Stat(nodeModules)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("node_modules mode = %v, want private directory", info.Mode())
	}
	if _, err := os.Stat(filepath.Join(nodeModules, "@opencode-ai", "plugin")); !os.IsNotExist(err) {
		t.Fatalf("OpenCode plugin code must not be vendored, stat error = %v", err)
	}

	var packageJSON map[string]any
	packageData, err := os.ReadFile(filepath.Join(configDir, "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(packageData, &packageJSON); err != nil {
		t.Fatal(err)
	}
	dependencies := packageJSON["dependencies"].(map[string]any)
	if dependencies["@opencode-ai/plugin"] != acp.OpenCodeVersion {
		t.Fatalf("package.json dependencies = %#v", dependencies)
	}

	var lockJSON map[string]any
	lockData, err := os.ReadFile(filepath.Join(configDir, "package-lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(lockData, &lockJSON); err != nil {
		t.Fatal(err)
	}
	root := lockJSON["packages"].(map[string]any)[""].(map[string]any)
	locked := root["dependencies"].(map[string]any)
	if locked["@opencode-ai/plugin"] != acp.OpenCodeVersion {
		t.Fatalf("package-lock.json root dependencies = %#v", locked)
	}
}

func TestOpenCodeBrokeredPermissionsUseDisjointNativeNamespace(t *testing.T) {
	policy := harnessv2.MCPToolPolicy{
		AllowedToolNames: []string{"bash"}, AllowBash: true,
		Tools: []harnessv2.MCPToolDescriptor{{
			Name: "bash", Source: harnessv2.MCPToolSourceBrokeredBuiltin, Effect: harnessv2.MCPToolEffectConsequential,
		}},
	}
	permissions, err := openCodeBrokeredPermissions(policy)
	if err != nil {
		t.Fatal(err)
	}
	if !permissions["orka_bash"] {
		t.Fatalf("brokered Bash permission = %#v, want namespaced allow", permissions)
	}
	if _, exists := permissions["bash"]; exists {
		t.Fatalf("brokered permission overwrote native Bash key: %#v", permissions)
	}
}

func TestOpenCodeSessionConfigRejectsCollidingBrokeredPermissionNames(t *testing.T) {
	paths := acp.SessionPaths{Workspace: "/sessions/private/workspace"}
	proxy := ProviderProxyBinding{BaseURL: "http://127.0.0.1:43210/_orka/provider/session", Credential: "placeholder"}
	tools := []harnessv2.MCPToolDescriptor{
		{Name: "web.search", Source: harnessv2.MCPToolSourceBrokeredCustom, Effect: harnessv2.MCPToolEffectReadOnly, DefinitionDigest: "sha256:" + strings.Repeat("a", 64), InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "web_search", Source: harnessv2.MCPToolSourceBrokeredBuiltin, Effect: harnessv2.MCPToolEffectReadOnly, InputSchema: json.RawMessage(`{"type":"object"}`)},
	}
	slices.SortFunc(tools, func(a, b harnessv2.MCPToolDescriptor) int { return strings.Compare(a.Name, b.Name) })
	descriptorDigest, err := harnessv2.CanonicalMCPToolDescriptorDigest(tools)
	if err != nil {
		t.Fatal(err)
	}
	policy := harnessv2.MCPToolPolicy{
		AllowedToolNames: []string{"web.search", "web_search"},
		Tools:            tools, DescriptorDigest: descriptorDigest,
	}
	request := harnessv2.CreateRuntimeSessionRequest{MCPConfiguration: harnessv2.MCPPolicyConfiguration{ToolPolicy: policy}}
	_, err = openCodeSessionConfig("openai/gpt-test", testOpenCodeModelLimits(), harnessv2.WorkspaceIntentWrite, request, paths, proxy)
	if err == nil || !strings.Contains(err.Error(), "collide in OpenCode permission name") {
		t.Fatalf("openCodeSessionConfig() error = %v, want sanitized permission collision rejection", err)
	}
}

func TestOpenCodeReadIntentDeniesGrepContentAccess(t *testing.T) {
	paths := acp.SessionPaths{Workspace: "/sessions/private/workspace"}
	proxy := ProviderProxyBinding{BaseURL: "http://127.0.0.1:43210/_orka/provider/session", Credential: "placeholder"}
	request := harnessv2.CreateRuntimeSessionRequest{MCPConfiguration: harnessv2.MCPPolicyConfiguration{
		ToolPolicy: harnessv2.MCPToolPolicy{AllowedToolNames: []string{"Grep", "Read"}},
	}}

	readConfig, err := openCodeSessionConfig("openai/gpt-test", testOpenCodeModelLimits(), harnessv2.WorkspaceIntentRead, request, paths, proxy)
	if err != nil {
		t.Fatal(err)
	}
	writeConfig, err := openCodeSessionConfig("openai/gpt-test", testOpenCodeModelLimits(), harnessv2.WorkspaceIntentWrite, request, paths, proxy)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"read": readConfig, "write": writeConfig} {
		var config map[string]any
		if err := json.Unmarshal(data, &config); err != nil {
			t.Fatal(err)
		}
		got := config["permission"].(map[string]any)["grep"]
		want := any(openCodePermissionAllow)
		if name == "read" {
			want = openCodePermissionDeny
		}
		if got != want {
			t.Fatalf("%s-intent grep permission = %#v, want %#v", name, got, want)
		}
	}
}

func TestOpenCodeMutationPermissionAliasesAreFailClosed(t *testing.T) {
	paths := acp.SessionPaths{Workspace: "/sessions/private/workspace"}
	proxy := ProviderProxyBinding{BaseURL: "http://127.0.0.1:43210/_orka/provider/session/v1", Credential: "placeholder"}
	for _, test := range []struct {
		name       string
		intent     harnessv2.WorkspaceIntent
		allowed    []string
		disallowed []string
		want       string
	}{
		{name: "gpt_apply_patch", intent: harnessv2.WorkspaceIntentWrite, allowed: []string{"APPLY_PATCH"}, want: openCodePermissionAllow},
		{name: "edit_alias_enables_gpt_patch", intent: harnessv2.WorkspaceIntentWrite, allowed: []string{"EdIt"}, want: openCodePermissionAllow},
		{name: "disallowed_write_alias_closes_group", intent: harnessv2.WorkspaceIntentWrite, allowed: []string{"apply_patch", "EDIT"}, disallowed: []string{"WrItE"}, want: openCodePermissionDeny},
		{name: "read_intent_closes_group", intent: harnessv2.WorkspaceIntentRead, allowed: []string{"apply_patch"}, want: openCodePermissionDeny},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := harnessv2.CreateRuntimeSessionRequest{
				MCPConfiguration: harnessv2.MCPPolicyConfiguration{
					ToolPolicy: harnessv2.MCPToolPolicy{
						AllowedToolNames:    test.allowed,
						DisallowedToolNames: test.disallowed,
					},
				},
			}
			content, err := openCodeSessionConfig("openai/gpt-test", testOpenCodeModelLimits(), test.intent, request, paths, proxy)
			if err != nil {
				t.Fatal(err)
			}
			var config map[string]any
			if err := json.Unmarshal(content, &config); err != nil {
				t.Fatal(err)
			}
			permissions := config["permission"].(map[string]any)
			for _, name := range []string{"apply_patch", "edit", "write"} {
				if permissions[name] != test.want {
					t.Fatalf("mutation permission %q = %#v, want %q; all=%#v", name, permissions[name], test.want, permissions)
				}
			}
		})
	}
}

func containsOpenCodePermissionAction(value any, action string) bool {
	switch typed := value.(type) {
	case string:
		return typed == action
	case map[string]any:
		for _, nested := range typed {
			if containsOpenCodePermissionAction(nested, action) {
				return true
			}
		}
	}
	return false
}

func TestOpenCodeSessionConfigRequiresProviderModel(t *testing.T) {
	_, err := openCodeSessionConfig(
		"gpt-test",
		testOpenCodeModelLimits(),
		harnessv2.WorkspaceIntentRead,
		harnessv2.CreateRuntimeSessionRequest{},
		acp.SessionPaths{Workspace: "/sessions/private/workspace"},
		ProviderProxyBinding{BaseURL: "http://127.0.0.1:43210/v1", Credential: "placeholder"},
	)
	if err == nil || !strings.Contains(err.Error(), "provider/model") {
		t.Fatalf("openCodeSessionConfig() error = %v, want provider/model rejection", err)
	}
}

func TestOpenCodeDefaultProtocolLimitUsesStandardUpdateRate(t *testing.T) {
	if got := defaultProtocolLimits(providerKindOpencode).MaxUpdateEventsPerSecond; got != harnessv2.DefaultMaxUpdateEventsPerSecond {
		t.Fatalf("MaxUpdateEventsPerSecond = %d, want %d", got, harnessv2.DefaultMaxUpdateEventsPerSecond)
	}
}

func TestOpenCodeProviderUpstreamBaseURLPreservesOpenAISemantics(t *testing.T) {
	if got := providerUpstreamBaseURL(providerKindOpencode, "http://vekil:1337"); got != testVekilV1URL {
		t.Fatalf("OpenCode upstream base URL = %q", got)
	}
}
