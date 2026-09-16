package supervisor

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func TestProviderProfilesDisableUpdatesAndUsePrivateHomes(t *testing.T) {
	paths := acp.SessionPaths{Home: "/sessions/private/home"}
	request := harnessv2.CreateRuntimeSessionRequest{}
	proxy := ProviderProxyBinding{BaseURL: "http://127.0.0.1:43210/_orka/provider/session", Credential: "local-session-credential"}

	codex, err := providerProfile(providerKindCodex, "gpt-test", harnessv2.WorkspaceIntentRead)
	if err != nil {
		t.Fatal(err)
	}
	codexEnv, err := codex.EnvironmentForSession(request, paths, proxy)
	if err != nil {
		t.Fatal(err)
	}
	if codexEnv["NO_BROWSER"] != "1" || codexEnv["CODEX_HOME"] != "/sessions/private/home/.codex" || !strings.Contains(codexEnv["CODEX_CONFIG"], proxy.BaseURL) || codexEnv["CODEX_API_KEY"] != proxy.Credential {
		t.Fatalf("unexpected Codex environment: %#v", codexEnv)
	}
	var codexConfig map[string]any
	if err := json.Unmarshal([]byte(codexEnv["CODEX_CONFIG"]), &codexConfig); err != nil {
		t.Fatal(err)
	}
	if codexConfig["model"] != "gpt-test" {
		t.Fatalf("Codex model config = %#v, want gpt-test", codexConfig["model"])
	}
	assertCodexWebSocketTransportsDisabled(t, codexConfig)
	if strings.Contains(strings.Join(codex.Args, " "), "npx") {
		t.Fatalf("Codex runtime uses a download-on-start command: %v", codex.Args)
	}

	claude, err := providerProfile("claude", "claude-test", harnessv2.WorkspaceIntentWrite)
	if err != nil {
		t.Fatal(err)
	}
	claudeEnv, err := claude.EnvironmentForSession(request, paths, proxy)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"DISABLE_UPDATES", "DISABLE_AUTOUPDATER", "DISABLE_INSTALLATION_CHECKS"} {
		if claudeEnv[name] != "1" {
			t.Fatalf("%s = %q", name, claudeEnv[name])
		}
	}
	if claudeEnv["CLAUDE_CONFIG_DIR"] != "/sessions/private/home/.claude" {
		t.Fatalf("unexpected Claude config dir: %q", claudeEnv["CLAUDE_CONFIG_DIR"])
	}
	if claudeEnv["ANTHROPIC_BASE_URL"] != proxy.BaseURL || claudeEnv["ANTHROPIC_API_KEY"] != proxy.Credential ||
		claudeEnv["ANTHROPIC_MODEL"] != "claude-test" {
		t.Fatalf("unexpected Claude proxy environment: %#v", claudeEnv)
	}

	copilot, err := providerProfile(providerKindCopilot, "copilot-test", harnessv2.WorkspaceIntentRead)
	if err != nil {
		t.Fatal(err)
	}
	copilotEnv, err := copilot.EnvironmentForSession(request, paths, proxy)
	if err != nil {
		t.Fatal(err)
	}
	if copilotEnv["COPILOT_AUTO_UPDATE"] != "false" || !containsArg(copilot.Args, "--no-auto-update") || !containsArg(copilot.Args, "--disable-builtin-mcps") {
		t.Fatalf("Copilot update/tool hardening missing: env=%#v args=%v", copilotEnv, copilot.Args)
	}
	if copilotEnv["COPILOT_PROVIDER_BASE_URL"] != proxy.BaseURL || copilotEnv["COPILOT_PROVIDER_BEARER_TOKEN"] != proxy.Credential {
		t.Fatalf("unexpected Copilot proxy environment: %#v", copilotEnv)
	}
	wantAdapterName, wantAdapterDigest, err := copilotAdapterIdentity(runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	if copilot.AdapterName != wantAdapterName || copilot.AdapterDigest != wantAdapterDigest {
		t.Fatalf("Copilot adapter identity = %s/%s, want %s/%s", copilot.AdapterName, copilot.AdapterDigest, wantAdapterName, wantAdapterDigest)
	}
}

const testProjectionAgentUID = "agent-uid"

func TestCodexProviderSessionProjection(t *testing.T) {
	paths := acp.SessionPaths{Home: "/sessions/private/home"}
	proxy := ProviderProxyBinding{BaseURL: "http://127.0.0.1:43210/_orka/provider/session", Credential: "test-auth-token"}
	codex, err := providerProfile(providerKindCodex, "gpt-test", harnessv2.WorkspaceIntentRead)
	if err != nil {
		t.Fatal(err)
	}
	request := testProviderProjectionRequest(t, providerKindCodex, "gpt-test", "codex system", "high", nil, nil, true)
	projection, err := codex.ProjectSession(request, paths, proxy)
	if err != nil {
		t.Fatal(err)
	}
	environment, err := codex.EnvironmentForSession(request, paths, proxy)
	if err != nil {
		t.Fatal(err)
	}
	maps.Copy(environment, projection.Environment)
	if environment["NO_BROWSER"] != "1" || environment["CODEX_HOME"] != "/sessions/private/home/.codex" ||
		!strings.Contains(environment["CODEX_CONFIG"], proxy.BaseURL) || environment["CODEX_API_KEY"] != proxy.Credential {
		t.Fatalf("unexpected Codex environment: %#v", environment)
	}
	var config map[string]any
	if err := json.Unmarshal([]byte(environment["CODEX_CONFIG"]), &config); err != nil {
		t.Fatal(err)
	}
	if config["model"] != "gpt-test" || config["developer_instructions"] != "codex system" || config["model_reasoning_effort"] != "high" {
		t.Fatalf("Codex config = %#v", config)
	}
	assertCodexWebSocketTransportsDisabled(t, config)
	if strings.Contains(strings.Join(codex.Args, " "), "npx") {
		t.Fatalf("Codex runtime uses a download-on-start command: %v", codex.Args)
	}
}

func TestCodexProviderSessionProjectionReadOnlySurface(t *testing.T) {
	paths := acp.SessionPaths{Home: "/sessions/private/home"}
	proxy := ProviderProxyBinding{BaseURL: "http://127.0.0.1:43210/_orka/provider/session", Credential: "test-auth-token"}
	codex, err := providerProfile(providerKindCodex, "gpt-test", harnessv2.WorkspaceIntentRead)
	if err != nil {
		t.Fatal(err)
	}
	surface := []string{providerToolGlob, providerToolGrep, providerToolRead}
	request := testProviderProjectionRequest(t, providerKindCodex, "gpt-test", "", "", surface, nil, false)
	projection, err := codex.ProjectSession(request, paths, proxy)
	if err != nil {
		t.Fatalf("read-only codex projection error = %v", err)
	}
	environment, err := codex.EnvironmentForSession(request, paths, proxy)
	if err != nil {
		t.Fatal(err)
	}
	maps.Copy(environment, projection.Environment)
	// Read-only sessions keep the orka-external agent mode: Codex's own
	// sandbox needs unprivileged user namespaces the runtime Pod forbids, so
	// the RuntimeSession boundary enforces the read-only surface instead.
	if environment["INITIAL_AGENT_MODE"] != codexAgentModeOrkaExternal {
		t.Fatalf("INITIAL_AGENT_MODE = %q, want orka-external", environment["INITIAL_AGENT_MODE"])
	}
	var config map[string]any
	if err := json.Unmarshal([]byte(environment["CODEX_CONFIG"]), &config); err != nil {
		t.Fatal(err)
	}
	if config["web_search"] != "disabled" {
		t.Fatal("read-only Codex session retained an ungranted native web tool")
	}
	if !strings.Contains(environment["CODEX_CONFIG"], proxy.BaseURL) || environment["CODEX_API_KEY"] != proxy.Credential {
		t.Fatalf("unexpected Codex environment: %#v", environment)
	}

	rejected := testProviderProjectionRequest(t, providerKindCodex, "gpt-test", "", "", []string{providerToolGlob, providerToolRead, providerToolWrite}, nil, false)
	if _, err := codex.ProjectSession(rejected, paths, proxy); err == nil {
		t.Fatal("restricted codex projection with Write was accepted")
	}

	writeIntent := testProviderProjectionRequest(t, providerKindCodex, "gpt-test", "", "", surface, nil, false)
	writeIntent.Profile.WorkspaceIntent = harnessv2.WorkspaceIntentWrite
	if _, err := codex.ProjectSession(writeIntent, paths, proxy); err == nil {
		t.Fatal("write-intent restricted codex projection was accepted")
	}
}

func TestCodexProviderSessionProjectionWithBrokeredMessaging(t *testing.T) {
	paths := acp.SessionPaths{Home: "/sessions/private/home"}
	proxy := ProviderProxyBinding{BaseURL: "http://127.0.0.1:43210/_orka/provider/session", Credential: "test-auth-token"}
	for _, tt := range []struct {
		name       string
		allowed    []string
		disallowed []string
		allowBash  bool
		wantError  bool
	}{
		{name: "native defaults with messaging", allowed: acp.BuiltInRuntimeNativeToolNames("codex"), allowBash: true},
		{name: "messaging without native grants", allowed: []string{}, allowBash: true, wantError: true},
		{name: "native deny", allowed: acp.BuiltInRuntimeNativeToolNames("codex"), disallowed: []string{providerToolWrite}, allowBash: true, wantError: true},
		{name: "bash denied", allowed: acp.BuiltInRuntimeNativeToolNames("codex"), wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			allowed := append(slices.Clone(tt.allowed), "send_message", "check_messages")
			request := testProviderProjectionRequest(t, providerKindCodex, "gpt-test", "", "", allowed, tt.disallowed, tt.allowBash)
			request.Profile.WorkspaceIntent = harnessv2.WorkspaceIntentWrite
			_, err := codexSessionProjection(request, paths, proxy, "gpt-test")
			if (err != nil) != tt.wantError {
				t.Fatalf("Codex projection error = %v, want error %t", err, tt.wantError)
			}
		})
	}
}

func TestClaudeProviderSessionProjection(t *testing.T) {
	paths := acp.SessionPaths{Home: "/sessions/private/home"}
	proxy := ProviderProxyBinding{BaseURL: "http://127.0.0.1:43210/_orka/provider/session", Credential: "test-auth-token"}
	claude, err := providerProfile(providerKindClaude, "claude-test", harnessv2.WorkspaceIntentWrite)
	if err != nil {
		t.Fatal(err)
	}
	request := testProviderProjectionRequest(t, providerKindClaude, "claude-test", "claude system", "max", []string{providerToolRead, providerToolWebFetch}, []string{providerToolBash}, false)
	projection, err := claude.ProjectSession(request, paths, proxy)
	if err != nil {
		t.Fatal(err)
	}
	environment, err := claude.EnvironmentForSession(request, paths, proxy)
	if err != nil {
		t.Fatal(err)
	}
	maps.Copy(environment, projection.Environment)
	for _, name := range []string{"DISABLE_UPDATES", "DISABLE_AUTOUPDATER", "DISABLE_INSTALLATION_CHECKS"} {
		if environment[name] != "1" {
			t.Fatalf("%s = %q", name, environment[name])
		}
	}
	if environment["CLAUDE_CONFIG_DIR"] != "/sessions/private/home/.claude" || environment["ANTHROPIC_BASE_URL"] != proxy.BaseURL ||
		environment["ANTHROPIC_API_KEY"] != proxy.Credential || environment["ANTHROPIC_MODEL"] != "claude-test" {
		t.Fatalf("unexpected Claude environment: %#v", environment)
	}
	if projection.NewSessionMeta["systemPrompt"] != "claude system" {
		t.Fatalf("Claude system prompt metadata = %#v", projection.NewSessionMeta)
	}
	claudeCode := projection.NewSessionMeta["claudeCode"].(map[string]any)
	options := claudeCode["options"].(map[string]any)
	if options["maxTurns"] != int32(7) || options["effort"] != "max" || !slices.Equal(options["tools"].([]string), []string{providerToolRead, providerToolWebFetch}) ||
		!slices.Contains(options["disallowedTools"].([]string), providerToolBash) {
		t.Fatalf("Claude options = %#v", options)
	}
}

func TestClaudeProviderEffortProjection(t *testing.T) {
	for _, tt := range []struct {
		name   string
		model  string
		effort string
	}{
		{name: "gateway model default", model: "claude-haiku-4.5"},
		{name: "canonical non-effort model", model: "claude-haiku-4-5"},
		{name: "recognized effort model default", model: "claude-sonnet-4-6"},
		{name: "explicit medium", model: "claude-sonnet-4-6", effort: "medium"},
		{name: "explicit max", model: "claude-opus-4-6", effort: "max"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			paths := acp.SessionPaths{Home: "/sessions/private/home"}
			proxy := ProviderProxyBinding{BaseURL: "http://127.0.0.1:43210/_orka/provider/session", Credential: "test-auth-token"}
			claude, err := providerProfile(providerKindClaude, tt.model, harnessv2.WorkspaceIntentRead)
			if err != nil {
				t.Fatal(err)
			}
			request := testProviderProjectionRequest(t, providerKindClaude, tt.model, "", tt.effort, nil, nil, true)
			projection, err := claude.ProjectSession(request, paths, proxy)
			if err != nil {
				t.Fatal(err)
			}
			environment, err := claude.EnvironmentForSession(request, paths, proxy)
			if err != nil {
				t.Fatal(err)
			}
			maps.Copy(environment, projection.Environment)
			if environment["ANTHROPIC_MODEL"] != tt.model {
				t.Fatalf("model changed to %q", environment["ANTHROPIC_MODEL"])
			}
			options := projection.NewSessionMeta["claudeCode"].(map[string]any)["options"].(map[string]any)
			if tt.effort == "" {
				if _, ok := options["effort"]; ok || environment["CLAUDE_CODE_EFFORT_LEVEL"] != "unset" {
					t.Fatal("unspecified effort must suppress the CLI default without adding SDK effort")
				}
			} else {
				if _, ok := environment["CLAUDE_CODE_EFFORT_LEVEL"]; ok || options["effort"] != tt.effort {
					t.Fatalf("explicit effort %q must reach the SDK without an environment override", tt.effort)
				}
			}
		})
	}
}

func TestCopilotProviderSessionProjection(t *testing.T) {
	paths := acp.SessionPaths{Home: "/sessions/private/home"}
	proxy := ProviderProxyBinding{BaseURL: "http://127.0.0.1:43210/_orka/provider/session", Credential: "test-auth-token"}
	copilot, err := providerProfile(providerKindCopilot, "copilot-test", harnessv2.WorkspaceIntentRead)
	if err != nil {
		t.Fatal(err)
	}
	request := testProviderProjectionRequest(t, providerKindCopilot, "copilot-test", "", "", []string{providerToolRead, providerToolGrep}, nil, false)
	projection, err := copilot.ProjectSession(request, paths, proxy)
	if err != nil {
		t.Fatal(err)
	}
	environment, err := copilot.EnvironmentForSession(request, paths, proxy)
	if err != nil {
		t.Fatal(err)
	}
	maps.Copy(environment, projection.Environment)
	args := append(append([]string(nil), copilot.Args...), projection.AdditionalArgs...)
	if environment["COPILOT_AUTO_UPDATE"] != "false" || !containsArg(args, "--no-auto-update") || !containsArg(args, "--disable-builtin-mcps") {
		t.Fatalf("Copilot update/tool hardening missing: env=%#v args=%v", environment, args)
	}
	if environment["COPILOT_PROVIDER_BASE_URL"] != proxy.BaseURL || environment["COPILOT_PROVIDER_BEARER_TOKEN"] != proxy.Credential {
		t.Fatalf("unexpected Copilot proxy environment: %#v", environment)
	}
	excluded := strings.Split(strings.TrimPrefix(projection.AdditionalArgs[0], "--excluded-tools="), ",")
	if !slices.Contains(excluded, "bash") || !slices.Contains(excluded, "create") || !slices.Contains(excluded, "web_search") ||
		!slices.Contains(excluded, "edit") || !slices.Contains(excluded, "str_replace_editor") || !slices.Contains(excluded, "apply_patch") ||
		!slices.Contains(excluded, "list_agents") || !slices.Contains(excluded, "read_agent") || !slices.Contains(excluded, "write_agent") ||
		slices.Contains(excluded, "view") || slices.Contains(excluded, "grep") || slices.Contains(excluded, "rg") {
		t.Fatalf("Copilot excluded tools = %v", excluded)
	}
	editRequest := testProviderProjectionRequest(
		t, providerKindCopilot, "copilot-test", "", "",
		[]string{providerToolEdit, providerToolWrite}, nil, false,
	)
	editProjection, err := copilot.ProjectSession(editRequest, paths, proxy)
	if err != nil {
		t.Fatal(err)
	}
	editExcluded := strings.Split(strings.TrimPrefix(editProjection.AdditionalArgs[0], "--excluded-tools="), ",")
	for _, allowedID := range []string{"edit", "str_replace_editor", "apply_patch", "create"} {
		if slices.Contains(editExcluded, allowedID) {
			t.Fatalf("Copilot excluded authorized tool alias %q: %v", allowedID, editExcluded)
		}
	}
	wantAdapterName, wantAdapterDigest, err := copilotAdapterIdentity(runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	if copilot.AdapterName != wantAdapterName || copilot.AdapterDigest != wantAdapterDigest {
		t.Fatalf("Copilot adapter identity = %s/%s, want %s/%s", copilot.AdapterName, copilot.AdapterDigest, wantAdapterName, wantAdapterDigest)
	}
}

func TestCopilotUnrestrictedProjectionKeepsPermanentExclusions(t *testing.T) {
	paths := acp.SessionPaths{Home: "/sessions/private/home"}
	proxy := ProviderProxyBinding{BaseURL: "http://127.0.0.1:43210/_orka/provider/session", Credential: "test-auth-token"}
	copilot, err := providerProfile(providerKindCopilot, "copilot-test", harnessv2.WorkspaceIntentRead)
	if err != nil {
		t.Fatal(err)
	}
	request := testProviderProjectionRequest(t, providerKindCopilot, "copilot-test", "", "", nil, nil, true)
	projection, err := copilot.ProjectSession(request, paths, proxy)
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.AdditionalArgs) != 3 || !slices.Contains(projection.AdditionalArgs, "--allow-tool=shell") ||
		!slices.Contains(projection.AdditionalArgs, "--allow-tool=write") {
		t.Fatalf("Copilot unrestricted projection args = %v", projection.AdditionalArgs)
	}
	excluded := strings.Split(strings.TrimPrefix(projection.AdditionalArgs[0], "--excluded-tools="), ",")
	for _, excludedID := range copilotAlwaysExcludedToolIDs {
		if !slices.Contains(excluded, excludedID) {
			t.Fatalf("Copilot unrestricted projection omitted permanent exclusion %q: %v", excludedID, excluded)
		}
	}
}

func TestCopilotProviderSessionProjectionWithBrokeredMessaging(t *testing.T) {
	paths := acp.SessionPaths{Home: "/sessions/private/home"}
	proxy := ProviderProxyBinding{BaseURL: "http://127.0.0.1:43210/_orka/provider/session", Credential: "test-auth-token"}
	for _, tt := range []struct {
		name       string
		disallowed []string
		allowBash  bool
		wantError  bool
	}{
		{name: "native defaults with messaging", allowBash: true},
		{name: "native deny retains unsupported web search", disallowed: []string{providerToolWrite}, allowBash: true, wantError: true},
		{name: "bash denied retains unsupported web search", wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			allowed := append(acp.BuiltInRuntimeNativeToolNames("copilot"), "send_message", "check_messages")
			request := testProviderProjectionRequest(t, providerKindCopilot, "copilot-test", "", "", allowed, tt.disallowed, tt.allowBash)
			request.Profile.WorkspaceIntent = harnessv2.WorkspaceIntentWrite
			projection, err := copilotSessionProjection(request, paths, proxy, "copilot-test")
			if tt.wantError {
				if err == nil || !strings.Contains(err.Error(), providerToolWebSearch) {
					t.Fatalf("restricted Copilot projection error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := []string{
				"--excluded-tools=" + strings.Join(copilotAlwaysExcludedToolIDs, ","),
				"--allow-tool=shell", "--allow-tool=write", "--allow-tool=orka",
			}
			if !slices.Equal(projection.AdditionalArgs, want) {
				t.Fatalf("delegation changed native Copilot exclusions: %v, want %v", projection.AdditionalArgs, want)
			}
		})
	}
}

func TestCopilotPermissionRulesPreserveRestrictedPolicies(t *testing.T) {
	for _, tt := range []struct {
		name      string
		allowed   []string
		allowBash bool
		wantRules []string
	}{
		{name: "deny all", allowed: []string{}, wantRules: []string{}},
		{name: "native read", allowed: []string{providerToolRead}, wantRules: []string{}},
		{name: "native write", allowed: []string{providerToolWrite}, wantRules: []string{"--allow-tool=write"}},
		{name: "native edit", allowed: []string{providerToolEdit}, wantRules: []string{"--allow-tool=write"}},
		{name: "native shell", allowed: []string{providerToolBash}, allowBash: true, wantRules: []string{"--allow-tool=shell"}},
		{
			name: "read and brokered messaging", allowed: []string{providerToolRead, "check_messages", "send_message"},
			wantRules: []string{"--allow-tool=orka"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			request := testProviderProjectionRequest(t, providerKindCopilot, "copilot-test", "", "", tt.allowed, nil, tt.allowBash)
			projection, err := copilotSessionProjection(request, acp.SessionPaths{}, ProviderProxyBinding{}, "copilot-test")
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(projection.AdditionalArgs[1:], tt.wantRules) {
				t.Fatalf("Copilot permission rules = %v, want %v", projection.AdditionalArgs[1:], tt.wantRules)
			}
		})
	}
}

func TestProviderSessionProjectionFailsClosed(t *testing.T) {
	paths := acp.SessionPaths{Home: "/sessions/private/home"}
	proxy := ProviderProxyBinding{BaseURL: "http://127.0.0.1:43210/_orka/provider/session", Credential: "test-auth-token"}

	codex, err := providerProfile(providerKindCodex, "gpt-test", harnessv2.WorkspaceIntentRead)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codex.ProjectSession(testProviderProjectionRequest(t, providerKindCodex, "gpt-test", "", "", []string{providerToolRead}, nil, true), paths, proxy); err == nil || !strings.Contains(err.Error(), "cannot exactly enforce") {
		t.Fatalf("Codex restricted policy error = %v", err)
	}

	copilot, err := providerProfile(providerKindCopilot, "copilot-test", harnessv2.WorkspaceIntentRead)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := copilot.ProjectSession(testProviderProjectionRequest(t, providerKindCopilot, "copilot-test", "unsupported", "", nil, nil, true), paths, proxy); err == nil || !strings.Contains(err.Error(), "systemPrompt") {
		t.Fatalf("Copilot system prompt error = %v", err)
	}
	if _, err := copilot.ProjectSession(testProviderProjectionRequest(t, providerKindCopilot, "copilot-test", "", "", []string{providerToolWebSearch}, nil, true), paths, proxy); err == nil || !strings.Contains(err.Error(), providerToolWebSearch) {
		t.Fatalf("Copilot WebSearch policy error = %v", err)
	}
	largeCodexPrompt := strings.Repeat("\u0001", 17<<10)
	if _, err := codex.ProjectSession(testProviderProjectionRequest(t, providerKindCodex, "gpt-test", largeCodexPrompt, "", nil, nil, true), paths, proxy); err == nil || !strings.Contains(err.Error(), "safe environment limit") {
		t.Fatalf("Codex oversized session configuration error = %v", err)
	}
}

func TestProviderSessionPolicyDistinguishesOmittedAndExplicitEmptyToolPolicies(t *testing.T) {
	tests := []struct {
		provider string
		model    string
	}{
		{provider: providerKindCodex, model: "gpt-test"},
		{provider: providerKindClaude, model: "claude-test"},
		{provider: providerKindCopilot, model: "copilot-test"},
	}
	for _, test := range tests {
		t.Run(test.provider, func(t *testing.T) {
			omitted := testProviderProjectionRequest(t, test.provider, test.model, "", "", nil, nil, true)
			policy, err := providerSessionPolicy(omitted, test.provider, test.model)
			if err != nil {
				t.Fatal(err)
			}
			if !policy.unrestricted {
				t.Fatal("omitted provider-native tool policy was not unrestricted")
			}

			explicitEmpty := testProviderProjectionRequest(t, test.provider, test.model, "", "", []string{}, nil, true)
			policy, err = providerSessionPolicy(explicitEmpty, test.provider, test.model)
			if err != nil {
				t.Fatal(err)
			}
			if policy.unrestricted {
				t.Fatal("explicit-empty provider-native tool policy was unrestricted")
			}

			emptyDisallowed := testProviderProjectionRequest(t, test.provider, test.model, "", "", nil, []string{}, true)
			policy, err = providerSessionPolicy(emptyDisallowed, test.provider, test.model)
			if err != nil {
				t.Fatal(err)
			}
			if !policy.unrestricted {
				t.Fatal("explicit-empty disallowlist was not treated as deny-none")
			}
		})
	}
}

func TestCopilotAdapterIdentity(t *testing.T) {
	tests := []struct {
		goarch     string
		wantName   string
		wantDigest string
	}{
		{goarch: "amd64", wantName: "copilot-cli-linux-amd64", wantDigest: "sha256:" + acp.CopilotCLILinuxX64SHA256},
		{goarch: "arm64", wantName: "copilot-cli-linux-arm64", wantDigest: "sha256:" + acp.CopilotCLILinuxARM64SHA256},
	}
	for _, test := range tests {
		t.Run(test.goarch, func(t *testing.T) {
			name, digest, err := copilotAdapterIdentity(test.goarch)
			if err != nil {
				t.Fatal(err)
			}
			if name != test.wantName || digest != test.wantDigest {
				t.Fatalf("identity = %s/%s, want %s/%s", name, digest, test.wantName, test.wantDigest)
			}
		})
	}
	if _, _, err := copilotAdapterIdentity("riscv64"); err == nil {
		t.Fatal("unsupported Copilot architecture unexpectedly accepted")
	}
}

func TestCodexProviderProfileUsesExternalRuntimeSandbox(t *testing.T) {
	paths := acp.SessionPaths{Home: "/sessions/private/home"}
	proxy := ProviderProxyBinding{BaseURL: "http://127.0.0.1:43210/_orka/provider/session", Credential: "local-session-credential"}
	for _, intent := range []harnessv2.WorkspaceIntent{harnessv2.WorkspaceIntentRead, harnessv2.WorkspaceIntentWrite} {
		t.Run(string(intent), func(t *testing.T) {
			profile, err := providerProfile(providerKindCodex, "gpt-test", intent)
			if err != nil {
				t.Fatal(err)
			}
			environment, err := profile.EnvironmentForSession(harnessv2.CreateRuntimeSessionRequest{}, paths, proxy)
			if err != nil {
				t.Fatal(err)
			}
			if got := environment["INITIAL_AGENT_MODE"]; got != codexAgentModeOrkaExternal {
				t.Fatalf("INITIAL_AGENT_MODE = %q, want orka-external", got)
			}
		})
	}
}

func TestLoadConfigFromEnv(t *testing.T) {
	dir := t.TempDir()
	controllerToken := filepath.Join(dir, "controller-token")
	capabilitySecret := filepath.Join(dir, "capability-secret")
	providerToken := filepath.Join(dir, "provider-token")
	for path, value := range map[string]string{
		controllerToken: strings.Repeat("t", 32), capabilitySecret: strings.Repeat("s", 32), providerToken: "provider-capability",
	} {
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	values := map[string]string{
		EnvPodUID: "pod-uid", EnvSupervisorBootID: "boot", EnvControllerEpoch: "1", EnvRuntimePoolUID: "pool-uid",
		EnvRuntimePoolGeneration: "1", EnvProvider: providerKindCodex, EnvModel: "gpt-test", EnvWorkspaceIntent: "read",
		EnvAgentConfigurationDigest: testDigest("agent"), EnvToolPolicyDigest: testDigest("tool"),
		EnvApprovalPolicyDigest: testDigest("approval"), EnvMCPConfigurationDigest: testDigest("mcp"),
		EnvProxyCredentialRole: "provider", EnvProxyCredentialScope: "model:gpt-test", EnvResourceClass: "standard",
		EnvControllerTokenFile: controllerToken, EnvCapabilitySecretFile: capabilitySecret, EnvProviderTokenFile: providerToken,
		EnvMCPBrokerURL: "http://orka-controller.orka-system.svc:8080", EnvTrustNamespace: "default",
		EnvSessionBaseDir: filepath.Join(dir, "sessions"), EnvFirstSessionUID: "20000", EnvLastSessionUID: "20010", EnvSessionGID: "20000",
		EnvE2EPromptWriteAmbiguity: testE2EPromptWriteAmbiguityMarker,
	}
	for name, value := range values {
		t.Setenv(name, value)
	}
	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider.Kind != providerKindCodex || cfg.Capabilities.ACPVersion != harnessv2.ACPProfileV1 || cfg.Fence.RuntimeProfileDigest == "" ||
		cfg.Fence.RuntimeInstanceID != "pod-uid.boot" || cfg.Fence.SupervisorBootID != "boot" {
		t.Fatalf("unexpected config: %#v", cfg)
	}
	if !cfg.Capabilities.SupportsAgentSessionConfiguration {
		t.Fatal("supervisor did not advertise Agent session configuration support")
	}
	if cfg.E2EPromptWriteAmbiguityMarker != testE2EPromptWriteAmbiguityMarker {
		t.Fatalf("E2E prompt write ambiguity marker = %q", cfg.E2EPromptWriteAmbiguityMarker)
	}
	if cfg.Capabilities.AdapterDigests["codex-acp"] != "sha256:"+acp.CodexACPTarSHA256 ||
		cfg.Capabilities.AdapterDigests["codex-acp-orka-patch"] != "sha256:"+acp.CodexACPOrkaPatchSHA256 ||
		cfg.Capabilities.AdapterDigests["codex-acp-orka-dist"] != "sha256:"+acp.CodexACPOrkaDistSHA256 ||
		cfg.Provider.AdapterName != "codex-acp-orka-dist" || cfg.Provider.AdapterDigest != "sha256:"+acp.CodexACPOrkaDistSHA256 {
		t.Fatalf("unexpected adapter digests: capabilities=%#v provider=%#v", cfg.Capabilities.AdapterDigests, cfg.Provider)
	}
	if cfg.Capabilities.Limits.MaxUpdateEventsPerSecond != runtimeMaxUpdateEventsPerSecond {
		t.Fatalf("max update events per second = %d, want %d", cfg.Capabilities.Limits.MaxUpdateEventsPerSecond, runtimeMaxUpdateEventsPerSecond)
	}
	if cfg.ProviderProxy.UpstreamBaseURL != "http://vekil.vekil-system.svc:1337/v1" {
		t.Fatalf("unexpected provider proxy base URL: %q", cfg.ProviderProxy.UpstreamBaseURL)
	}
	if cfg.ProviderProxy.UpstreamBearerToken != "provider-capability" {
		t.Fatal("provider proxy token was not loaded from the supervisor-only file")
	}
	t.Run("dedicated durable workspace", func(t *testing.T) {
		t.Setenv(EnvDurableWorkspaceDir, filepath.Join(dir, "durable"))
		t.Setenv(EnvDurableWorkspaceKey, "workspace")
		dedicated, err := LoadConfigFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if dedicated.DurableWorkspaceKey != "workspace" ||
			dedicated.Capabilities.Limits.MaxResidentSessions != 1 || dedicated.Capabilities.Limits.MaxConcurrentPrompts != 1 {
			t.Fatal("stable durable workspace did not load as a dedicated single-session runtime")
		}
	})

	t.Setenv(EnvProvider, providerKindCopilot)
	copilotCfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	wantAdapterName, wantAdapterDigest, err := copilotAdapterIdentity(runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	if copilotCfg.Provider.Kind != providerKindCopilot || copilotCfg.Provider.AdapterName != wantAdapterName ||
		copilotCfg.Provider.AdapterDigest != wantAdapterDigest {
		t.Fatalf("unexpected Copilot provider: %#v", copilotCfg.Provider)
	}
	if copilotCfg.Capabilities.AdapterDigests["copilot-cli-linux-amd64"] != "sha256:"+acp.CopilotCLILinuxX64SHA256 ||
		copilotCfg.Capabilities.AdapterDigests["copilot-cli-linux-arm64"] != "sha256:"+acp.CopilotCLILinuxARM64SHA256 {
		t.Fatalf("unexpected Copilot adapter digests: %#v", copilotCfg.Capabilities.AdapterDigests)
	}

	t.Setenv(EnvProvider, providerKindAgentKit)
	t.Setenv(EnvAgentKitAdapterDigest, testAgentKitAdapterDigest)
	agentKitCfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if agentKitCfg.Provider.Kind != providerKindAgentKit || agentKitCfg.Provider.AdapterName != agentKitAdapterName ||
		agentKitCfg.Provider.AdapterDigest != testAgentKitAdapterDigest ||
		len(agentKitCfg.Capabilities.AdapterDigests) != 1 ||
		agentKitCfg.Capabilities.AdapterDigests[agentKitAdapterName] != testAgentKitAdapterDigest {
		t.Fatalf("unexpected AgentKit provider: provider=%#v capabilities=%#v", agentKitCfg.Provider, agentKitCfg.Capabilities)
	}
	if agentKitCfg.ProviderProxy.UpstreamBaseURL != "http://vekil.vekil-system.svc:1337/v1" {
		t.Fatalf("AgentKit provider proxy base URL = %q", agentKitCfg.ProviderProxy.UpstreamBaseURL)
	}
	if agentKitCfg.Capabilities.SupportsAgentSessionConfiguration ||
		agentKitCfg.Capabilities.Provider.SupportsImages || agentKitCfg.Capabilities.Provider.SupportsAudio ||
		agentKitCfg.Capabilities.Provider.SupportsEmbeddedResources ||
		agentKitCfg.Capabilities.Provider.SupportsPermissions || !agentKitCfg.Capabilities.Provider.SupportsTools {
		t.Fatalf("unexpected AgentKit provider capabilities: %#v", agentKitCfg.Capabilities.Provider)
	}
}

func TestLoadConfigFromEnvBootstrapSecrets(t *testing.T) {
	dir := t.TempDir()
	values := map[string]string{
		EnvPodUID: "actor:orka-acp-actor", EnvSupervisorBootID: "boot", EnvControllerEpoch: "1", EnvRuntimePoolUID: "pool-uid",
		EnvRuntimePoolGeneration: "1", EnvProvider: providerKindCodex, EnvModel: "gpt-test", EnvWorkspaceIntent: "read",
		EnvAgentConfigurationDigest: testDigest("agent"), EnvToolPolicyDigest: testDigest("tool"),
		EnvApprovalPolicyDigest: testDigest("approval"), EnvMCPConfigurationDigest: testDigest("mcp"),
		EnvProxyCredentialRole: "provider", EnvProxyCredentialScope: "model:gpt-test", EnvResourceClass: "standard",
		EnvControllerTokenBootstrap: strings.Repeat("t", 32), EnvCapabilitySecretBootstrap: strings.Repeat("s", 32),
		EnvProviderTokenBootstrap: "provider-capability",
		EnvMCPBrokerURL:           "http://orka-controller.orka-system.svc:8080", EnvTrustNamespace: "default",
		EnvSessionBaseDir: filepath.Join(dir, "sessions"), EnvFirstSessionUID: "20000", EnvLastSessionUID: "20010", EnvSessionGID: "20000",
	}
	for name, value := range values {
		t.Setenv(name, value)
	}
	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ControllerBearerToken != strings.Repeat("t", 32) || string(cfg.CapabilitySecret) != strings.Repeat("s", 32) ||
		cfg.ProviderProxy.UpstreamBearerToken != "provider-capability" {
		t.Fatal("bootstrap secrets were not loaded into the supervisor config")
	}
	if cfg.Fence.RuntimeInstanceID != "actor:orka-acp-actor.boot" {
		t.Fatalf("instance ID = %q, want actor-scoped identity", cfg.Fence.RuntimeInstanceID)
	}
	for _, name := range []string{EnvControllerTokenBootstrap, EnvCapabilitySecretBootstrap, EnvProviderTokenBootstrap} {
		if value, present := os.LookupEnv(name); present && value != "" {
			t.Fatalf("read-once bootstrap env %s survived config load", name)
		}
	}

	// The file variable always wins over the bootstrap variable.
	controllerToken := filepath.Join(dir, "controller-token")
	if err := os.WriteFile(controllerToken, []byte(strings.Repeat("f", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvControllerTokenFile, controllerToken)
	t.Setenv(EnvControllerTokenBootstrap, strings.Repeat("x", 32))
	t.Setenv(EnvCapabilitySecretBootstrap, strings.Repeat("s", 32))
	t.Setenv(EnvProviderTokenBootstrap, "provider-capability")
	fileCfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if fileCfg.ControllerBearerToken != strings.Repeat("f", 32) {
		t.Fatal("mounted secret file did not take precedence over the bootstrap env")
	}
	if value, present := os.LookupEnv(EnvControllerTokenBootstrap); present && value != "" {
		t.Fatal("unused bootstrap secret survived file-backed config load")
	}

	// Neither the file nor the bootstrap variable fails closed.
	t.Setenv(EnvControllerTokenFile, "")
	t.Setenv(EnvControllerTokenBootstrap, "")
	t.Setenv(EnvCapabilitySecretBootstrap, strings.Repeat("s", 32))
	t.Setenv(EnvProviderTokenBootstrap, "provider-capability")
	if _, err := LoadConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "read-once bootstrap secret") {
		t.Fatalf("missing controller secret error = %v, want fail-closed bootstrap message", err)
	}
}

func TestDefaultProtocolLimitsUseProviderSpecificUpdateRates(t *testing.T) {
	tests := []struct {
		provider string
		want     int
	}{
		{provider: providerKindCodex, want: runtimeMaxUpdateEventsPerSecond},
		{provider: providerKindClaude, want: runtimeMaxUpdateEventsPerSecond},
		{provider: providerKindCopilot, want: runtimeMaxUpdateEventsPerSecond},
		{provider: providerKindOpencode, want: runtimeMaxUpdateEventsPerSecond},
		{provider: providerKindAgentKit, want: runtimeMaxUpdateEventsPerSecond},
	}
	for _, test := range tests {
		t.Run(test.provider, func(t *testing.T) {
			if got := defaultProtocolLimits(test.provider).MaxUpdateEventsPerSecond; got != test.want {
				t.Fatalf("MaxUpdateEventsPerSecond = %d, want %d", got, test.want)
			}
		})
	}
}

func TestProviderUpstreamBaseURLPreservesProviderSemantics(t *testing.T) {
	if got := providerUpstreamBaseURL(providerKindCodex, "http://vekil:1337"); got != "http://vekil:1337/v1" {
		t.Fatalf("Codex upstream base URL = %q", got)
	}
	if got := providerUpstreamBaseURL(providerKindCopilot, "http://vekil:1337/v1"); got != "http://vekil:1337/v1" {
		t.Fatalf("Copilot upstream base URL = %q", got)
	}
	if got := providerUpstreamBaseURL(providerKindAgentKit, "http://vekil:1337"); got != "http://vekil:1337/v1" {
		t.Fatalf("AgentKit upstream base URL = %q", got)
	}
	if got := providerUpstreamBaseURL("claude", "http://vekil:1337/"); got != "http://vekil:1337" {
		t.Fatalf("Claude upstream base URL = %q", got)
	}
}

func testProviderProjectionRequest(
	t *testing.T,
	provider string,
	model string,
	systemPrompt string,
	reasoningEffort string,
	allowed []string,
	disallowed []string,
	allowBash bool,
) harnessv2.CreateRuntimeSessionRequest {
	t.Helper()
	allowed = slices.Clone(allowed)
	disallowed = slices.Clone(disallowed)
	slices.Sort(allowed)
	slices.Sort(disallowed)
	toolPolicy := harnessv2.MCPToolPolicy{
		AllowedToolNames: allowed, DisallowedToolNames: disallowed, AllowBash: allowBash,
	}
	for _, name := range allowed {
		if !toolPolicy.Allows(name) {
			continue
		}
		descriptor := harnessv2.MCPToolDescriptor{
			Name: name, Description: "provider native", Source: harnessv2.MCPToolSourceProviderNative,
			Effect: harnessv2.MCPToolEffectReadOnly,
		}
		if _, ok := canonicalProviderNativeToolName(name); !ok {
			switch name {
			case "send_message", "check_messages":
				descriptor.Source = harnessv2.MCPToolSourceBrokeredBuiltin
				descriptor.InputSchema = json.RawMessage(`{"type":"object"}`)
			default:
				t.Fatalf("unknown projection test tool %q", name)
			}
		}
		toolPolicy.Tools = append(toolPolicy.Tools, descriptor)
	}
	slices.SortFunc(toolPolicy.Tools, func(a, b harnessv2.MCPToolDescriptor) int { return strings.Compare(a.Name, b.Name) })
	var err error
	toolPolicy.DescriptorDigest, err = harnessv2.CanonicalMCPToolDescriptorDigest(toolPolicy.Tools)
	if err != nil {
		t.Fatal(err)
	}
	approval := harnessv2.MCPApprovalPolicy{}
	configuration := harnessv2.AgentSessionConfiguration{
		AgentUID: testProjectionAgentUID, AgentGeneration: 1, ProviderKind: provider, Model: model,
		MaxTurns: 7, ReasoningEffort: reasoningEffort, SystemPrompt: systemPrompt,
	}
	agentDigest, err := harnessv2.CanonicalAgentConfigurationDigest(configuration)
	if err != nil {
		t.Fatal(err)
	}
	toolDigest, err := harnessv2.CanonicalRuntimeToolPolicyDigest(allowed, disallowed, allowBash)
	if err != nil {
		t.Fatal(err)
	}
	approvalDigest, err := harnessv2.CanonicalMCPApprovalPolicyDigest(approval)
	if err != nil {
		t.Fatal(err)
	}
	mcpDigest, err := harnessv2.CanonicalMCPConfigurationDigest(allowed)
	if err != nil {
		t.Fatal(err)
	}
	profile := harnessv2.RuntimeProfile{
		ACPProfile: harnessv2.ACPProfileV1, AdapterDigests: map[string]string{"adapter": testDigest("adapter")},
		ProviderKind: provider, Model: model,
		AgentConfigurationDigest: agentDigest, ToolPolicyDigest: toolDigest, ApprovalPolicyDigest: approvalDigest,
		MCPConfigurationDigest: mcpDigest, WorkspaceIntent: harnessv2.WorkspaceIntentRead,
		ProxyCredentialRole: "provider", ProxyCredentialScope: "model:" + model, ResourceClass: "standard",
	}
	return harnessv2.CreateRuntimeSessionRequest{
		Profile: profile, AgentConfiguration: &configuration,
		MCPConfiguration: harnessv2.MCPPolicyConfiguration{
			ToolPolicyDigest: toolDigest, ApprovalPolicyDigest: approvalDigest, MCPConfigurationDigest: mcpDigest,
			ToolPolicy: toolPolicy, ApprovalPolicy: approval,
		},
	}
}

func containsArg(args []string, want string) bool {
	return slices.Contains(args, want)
}

// assertCodexWebSocketTransportsDisabled proves the session config selects
// the custom HTTPS-only provider instead of Codex's built-in "openai"
// provider, whose Responses WebSocket attempt the proxy rejects with 403 and
// whose fallback warning would leak into the agent's first message.
func assertCodexWebSocketTransportsDisabled(t *testing.T, config map[string]any) {
	t.Helper()
	if config["model_provider"] != codexProviderID {
		t.Fatalf("Codex model_provider = %#v, want %q", config["model_provider"], codexProviderID)
	}
	if _, ok := config["openai_base_url"]; ok {
		t.Fatalf("Codex config still selects the built-in openai provider: %#v", config)
	}
	providers, _ := config["model_providers"].(map[string]any)
	provider, _ := providers[codexProviderID].(map[string]any)
	if provider["wire_api"] != "responses" || provider["env_key"] != "CODEX_API_KEY" || provider["base_url"] == "" {
		t.Fatalf("Codex provider definition = %#v", provider)
	}
}

func TestPrepareCodexHomeDisablesResponsesWebSockets(t *testing.T) {
	paths := acp.SessionPaths{Home: t.TempDir()}
	if err := prepareCodexHome(paths); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(paths.Home, ".codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	config := string(data)
	if !strings.Contains(config, "check_for_update_on_startup = false") {
		t.Fatalf("config.toml lacks the update opt-out:\n%s", config)
	}
}

// This digest pins the pre-systemPrompt configuration bytes for a synthetic
// session, so absent/empty prompts cannot silently change the default profile.
func TestOpenCodeSystemPromptEmptyConfigurationStable(t *testing.T) {
	paths := acp.SessionPaths{Config: "/sessions/private/xdg/config", Home: "/sessions/private/home", Workspace: "/sessions/private/workspace"}
	proxy := ProviderProxyBinding{BaseURL: "http://127.0.0.1:43210/_orka/provider/session/v1", Credential: "synthetic-prompt-test-capability"}
	profile, err := providerProfile(providerKindOpencode, "openai/gpt-test", harnessv2.WorkspaceIntentRead, testOpenCodeModelLimits())
	if err != nil {
		t.Fatal(err)
	}
	request := testProviderProjectionRequest(t, providerKindOpencode, "openai/gpt-test", "", "", []string{providerToolRead, providerToolGlob, "send_message"}, nil, false)
	var baseline map[string]string
	for _, tt := range []struct {
		name          string
		configuration *harnessv2.AgentSessionConfiguration
	}{
		{name: "nil"},
		{name: "empty", configuration: &harnessv2.AgentSessionConfiguration{}},
		{name: "configured without prompt", configuration: request.AgentConfiguration},
	} {
		t.Run(tt.name, func(t *testing.T) {
			request.AgentConfiguration = tt.configuration
			environment, err := profile.EnvironmentForSession(request, paths, proxy)
			if err != nil {
				t.Fatal(err)
			}
			got := fmt.Sprintf("%x", sha256.Sum256([]byte(environment["OPENCODE_CONFIG_CONTENT"])))
			if got != "2bf597d8d310b94159e1653f0c6c4250e1140e9c93fe5ac5941dea1994a89f63" {
				t.Fatalf("prompt-free config digest = %s, want original configuration bytes", got)
			}
			if baseline == nil {
				baseline = environment
			} else if !maps.Equal(baseline, environment) {
				t.Fatal("empty AgentConfiguration changed the session environment")
			}
		})
	}
}

func TestOpenCodeSystemPromptLiteralPrimaryAgents(t *testing.T) {
	paths := acp.SessionPaths{Config: "/sessions/private/xdg/config", Home: "/sessions/private/home", Workspace: "/sessions/private/workspace"}
	proxy := ProviderProxyBinding{BaseURL: "http://127.0.0.1:43210/_orka/provider/session/v1", Credential: "synthetic-prompt-test-capability"}
	profile, err := providerProfile(providerKindOpencode, "openai/gpt-test", harnessv2.WorkspaceIntentRead, testOpenCodeModelLimits())
	if err != nil {
		t.Fatal(err)
	}
	allowed := []string{providerToolRead, providerToolGlob, "send_message"}
	request := testProviderProjectionRequest(t, providerKindOpencode, "openai/gpt-test", "", "", allowed, nil, false)
	baseline, err := profile.EnvironmentForSession(request, paths, proxy)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORKA_SYNTHETIC_PROMPT_VAR", "must-not-expand")
	for _, tt := range []struct {
		name   string
		prompt string
	}{
		{
			name: "JSON newlines Unicode and literal escapes",
			prompt: "  Follow these instructions literally.\n{\"message\": \"你好 🌍\", \"items\": [1, 2]}\n" +
				"Keep <>&, tabs\tand CRLF\r\n" + `C:\synthetic\notes $ORKA_SYNTHETIC_PROMPT_VAR ${ORKA_SYNTHETIC_PROMPT_VAR} \u007benv:ORKA_SYNTHETIC_PROMPT_VAR}` + "\n  ",
		},
		{name: "whitespace is nonempty", prompt: " \n\t "},
		{name: "maximum JSON encoded prompt", prompt: strings.Repeat("x", acp.MaxOpenCodeSystemPromptEncodedBytes-2)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			request := testProviderProjectionRequest(t, providerKindOpencode, "openai/gpt-test", tt.prompt, "", allowed, nil, false)
			request.Profile.ModelLimits = testOpenCodeModelLimits()
			projection, err := profile.ProjectSession(request, paths, proxy)
			if err != nil {
				t.Fatal(err)
			}
			if len(projection.AdditionalArgs) != 0 || len(projection.Environment) != 0 || len(projection.NewSessionMeta) != 0 {
				t.Fatal("OpenCode system prompt escaped its isolated config projection")
			}
			environment, err := profile.EnvironmentForSession(request, paths, proxy)
			if err != nil {
				t.Fatal(err)
			}
			config := decodeOpenCodeSystemPromptConfig(t, environment)
			if config["default_agent"] != "build" {
				t.Fatal("OpenCode system prompt does not select the build agent by default")
			}
			agents := config["agent"].(map[string]any)
			if len(agents) != 3 || agents["title"].(map[string]any)["disable"] != true {
				t.Fatal("OpenCode prompt changed the agent set or enabled title inference")
			}
			for _, name := range []string{"build", "plan"} {
				agent, ok := agents[name].(map[string]any)
				if !ok || len(agent) != 2 || agent["mode"] != "primary" || agent["prompt"] != tt.prompt {
					t.Fatalf("OpenCode %s agent did not receive the exact literal primary prompt", name)
				}
				delete(agents, name)
			}
			delete(config, "default_agent")
			withoutPrompt, err := json.Marshal(config)
			if err != nil {
				t.Fatal(err)
			}
			// Every permission, provider, model limit, MCP and instruction setting
			// must remain byte-identical after removing only the prompt additions.
			environment["OPENCODE_CONFIG_CONTENT"] = string(withoutPrompt)
			if !maps.Equal(baseline, environment) {
				t.Fatal("adding a literal prompt changed unrelated configuration or environment")
			}
		})
	}
}

func TestOpenCodeSystemPromptRejectedAtBothBoundaries(t *testing.T) {
	paths := acp.SessionPaths{Workspace: "/sessions/private/workspace"}
	proxy := ProviderProxyBinding{BaseURL: "http://127.0.0.1:43210/_orka/provider/session/v1", Credential: "synthetic-prompt-test-capability"}
	profile, err := providerProfile(providerKindOpencode, "openai/gpt-test", harnessv2.WorkspaceIntentRead, testOpenCodeModelLimits())
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name    string
		prompt  string
		wantErr string
	}{
		{name: "env", prompt: "Never expand {env:ORKA_OPENCODE_PROVIDER_TOKEN}", wantErr: "configuration substitutions"},
		{name: "file", prompt: "Never read {file:/synthetic/private/config}", wantErr: "configuration substitutions"},
		{name: "env case and spacing", prompt: "Nested { EnV : SYNTHETIC_TOKEN }", wantErr: "configuration substitutions"},
		{name: "file newlines", prompt: "Nested {\nFiLe\t:/synthetic/private/config}", wantErr: "configuration substitutions"},
		{name: "JSON quoted substitution", prompt: `{"note":"{env:SYNTHETIC_TOKEN}"}`, wantErr: "configuration substitutions"},
		{name: "oversized plain text", prompt: strings.Repeat("x", acp.MaxOpenCodeSystemPromptEncodedBytes-1), wantErr: "encoded limit"},
		{name: "oversized escaped text", prompt: strings.Repeat("\n", acp.MaxOpenCodeSystemPromptEncodedBytes/2), wantErr: "encoded limit"},
		{name: "oversized HTML escaped text", prompt: strings.Repeat("<", acp.MaxOpenCodeSystemPromptEncodedBytes/6+1), wantErr: "encoded limit"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// Bind the forbidden text into a valid digest: rejection must not rely
			// on a stale AgentConfiguration digest masking the unsafe prompt.
			request := testProviderProjectionRequest(t, providerKindOpencode, "openai/gpt-test", tt.prompt, "", nil, nil, false)
			request.Profile.ModelLimits = testOpenCodeModelLimits()
			t.Run("projection", func(t *testing.T) {
				_, err := profile.ProjectSession(request, paths, proxy)
				assertOpenCodeSystemPromptError(t, err, tt.wantErr, tt.prompt, proxy.Credential)
			})
			t.Run("environment without projection", func(t *testing.T) {
				environment, err := profile.EnvironmentForSession(request, paths, proxy)
				assertOpenCodeSystemPromptError(t, err, tt.wantErr, tt.prompt, proxy.Credential)
				if environment != nil {
					t.Fatal("rejected system prompt returned a partial session environment")
				}
			})
		})
	}
}

func TestOpenCodeSystemPromptFinalConfigByteLimit(t *testing.T) {
	paths := acp.SessionPaths{Workspace: "/sessions/private/workspace"}
	proxy := ProviderProxyBinding{BaseURL: "http://127.0.0.1:43210/_orka/provider/session/v1", Credential: "synthetic-prompt-test-capability"}
	profile, err := providerProfile(providerKindOpencode, "openai/gpt-test", harnessv2.WorkspaceIntentRead, testOpenCodeModelLimits())
	if err != nil {
		t.Fatal(err)
	}
	prompt := strings.Repeat("x", acp.MaxOpenCodeSystemPromptEncodedBytes-2)
	request := testProviderProjectionRequest(t, providerKindOpencode, "openai/gpt-test", prompt, "", nil, nil, false)
	environment, err := profile.EnvironmentForSession(request, paths, proxy)
	if err != nil {
		t.Fatal(err)
	}
	// Exercise the final serialized size, including both prompt copies and
	// unrelated config overhead, without creating files or enormous credentials.
	padding := acp.MaxOpenCodeConfigEnvironmentBytes - len(environment["OPENCODE_CONFIG_CONTENT"])
	if padding < 0 {
		t.Fatal("ordinary maximum prompt already exceeds the environment limit")
	}
	paths.Workspace += strings.Repeat("w", padding)
	environment, err = profile.EnvironmentForSession(request, paths, proxy)
	if err != nil {
		t.Fatal(err)
	}
	if len(environment["OPENCODE_CONFIG_CONTENT"]) != acp.MaxOpenCodeConfigEnvironmentBytes {
		t.Fatal("boundary fixture did not reach the exact configuration byte limit")
	}
	paths.Workspace += "w"
	environment, err = profile.EnvironmentForSession(request, paths, proxy)
	assertOpenCodeSystemPromptError(t, err, "safe environment limit", prompt, proxy.Credential)
	if environment != nil {
		t.Fatal("oversized configuration returned a partial session environment")
	}

	// Do not introduce a new limit for legacy prompt-free configurations.
	paths.Workspace += strings.Repeat("w", acp.MaxOpenCodeConfigEnvironmentBytes)
	request.AgentConfiguration.SystemPrompt = ""
	environment, err = profile.EnvironmentForSession(request, paths, proxy)
	if err != nil {
		t.Fatal(err)
	}
	if len(environment["OPENCODE_CONFIG_CONTENT"]) <= acp.MaxOpenCodeConfigEnvironmentBytes {
		t.Fatal("prompt-free fixture did not exercise the legacy unbounded behavior")
	}
}

func decodeOpenCodeSystemPromptConfig(t *testing.T, environment map[string]string) map[string]any {
	t.Helper()
	var config map[string]any
	if err := json.Unmarshal([]byte(environment["OPENCODE_CONFIG_CONTENT"]), &config); err != nil {
		t.Fatal("OpenCode system prompt configuration is not valid JSON")
	}
	return config
}

func assertOpenCodeSystemPromptError(t *testing.T, err error, want, prompt, credential string) {
	t.Helper()
	if err == nil {
		t.Fatalf("unsafe system prompt accepted; want %s rejection", want)
	}
	if strings.Contains(err.Error(), prompt) || strings.Contains(err.Error(), credential) {
		t.Fatal("system prompt error disclosed input text or proxy capability")
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("system prompt failed for an unrelated reason; want %s rejection", want)
	}
}

func TestOpenCodeSystemPromptPreservesRestrictions(t *testing.T) {
	paths := acp.SessionPaths{Config: "/sessions/private/xdg/config", Workspace: "/sessions/private/workspace"}
	proxy := ProviderProxyBinding{BaseURL: "http://127.0.0.1:43210/_orka/provider/session/v1", Credential: "synthetic-prompt-test-capability"}
	nativeAndBrokered := []string{providerToolBash, providerToolRead, providerToolGrep, providerToolEdit, providerToolWrite, "send_message", "check_messages"}
	for _, tt := range []struct {
		name         string
		intent       harnessv2.WorkspaceIntent
		allowed      []string
		disallowed   []string
		allowBash    bool
		wantRead     bool
		wantGrep     string
		wantMutation string
	}{
		{name: "read intent", intent: harnessv2.WorkspaceIntentRead, allowed: nativeAndBrokered, allowBash: true, wantRead: true, wantGrep: openCodePermissionDeny, wantMutation: openCodePermissionDeny},
		{name: "write without Bash", intent: harnessv2.WorkspaceIntentWrite, allowed: nativeAndBrokered, wantRead: true, wantGrep: openCodePermissionAllow, wantMutation: openCodePermissionAllow},
		{name: "denied mutation alias", intent: harnessv2.WorkspaceIntentWrite, allowed: nativeAndBrokered, disallowed: []string{providerToolWrite}, wantRead: true, wantGrep: openCodePermissionAllow, wantMutation: openCodePermissionDeny},
		{name: "broker only", intent: harnessv2.WorkspaceIntentWrite, allowed: []string{"send_message", "check_messages"}, allowBash: true, wantGrep: openCodePermissionDeny, wantMutation: openCodePermissionDeny},
	} {
		t.Run(tt.name, func(t *testing.T) {
			profile, err := providerProfile(providerKindOpencode, "openai/gpt-test", tt.intent, testOpenCodeModelLimits())
			if err != nil {
				t.Fatal(err)
			}
			request := testProviderProjectionRequest(t, providerKindOpencode, "openai/gpt-test", "Literal instructions do not grant tools or access to secrets.", "", tt.allowed, tt.disallowed, tt.allowBash)
			request.Profile.ModelLimits = testOpenCodeModelLimits()
			request.Profile.WorkspaceIntent = tt.intent
			if _, err := profile.ProjectSession(request, paths, proxy); err != nil {
				t.Fatal(err)
			}
			environment, err := profile.EnvironmentForSession(request, paths, proxy)
			if err != nil {
				t.Fatal(err)
			}
			assertOpenCodeSystemPromptIsolation(t, profile.Args, environment, proxy.Credential)
			config := decodeOpenCodeSystemPromptConfig(t, environment)
			permissions := config["permission"].(map[string]any)
			for _, name := range []string{"*", "bash", "external_directory", "task", "skill", "question", "webfetch", "websearch", "lsp", "list", "doom_loop", "todowrite"} {
				if permissions[name] != openCodePermissionDeny {
					t.Fatalf("system prompt relaxed %s permission", name)
				}
			}
			for _, name := range []string{"apply_patch", "edit", "write"} {
				if permissions[name] != tt.wantMutation {
					t.Fatalf("system prompt changed %s mutation permission", name)
				}
			}
			if permissions["grep"] != tt.wantGrep {
				t.Fatal("system prompt changed grep content-access restrictions")
			}
			if tt.wantRead {
				read, ok := permissions["read"].(map[string]any)
				if !ok || read["*"] != openCodePermissionAllow || read["*.env"] != openCodePermissionDeny || read["*.env.*"] != openCodePermissionDeny || read["*.env.example"] != openCodePermissionAllow {
					t.Fatal("system prompt changed secret-file read restrictions")
				}
			} else if permissions["read"] != openCodePermissionDeny || permissions["glob"] != openCodePermissionDeny {
				t.Fatal("broker-only system prompt acquired native file tools")
			}
			for _, name := range []string{"send_message", "check_messages"} {
				if permissions["orka_"+name] != openCodePermissionAllow || permissions[name] != nil {
					t.Fatal("system prompt changed broker-only tool namespacing")
				}
			}
			if containsOpenCodePermissionAction(permissions, "ask") || len(config["mcp"].(map[string]any)) != 0 {
				t.Fatal("system prompt introduced unsupported approvals or ungoverned MCP servers")
			}
			assertOpenCodeSystemPromptProviderRestrictions(t, config, proxy.BaseURL)
		})
	}
}

func assertOpenCodeSystemPromptIsolation(t *testing.T, args []string, environment map[string]string, credential string) {
	t.Helper()
	if !slices.Contains(args, "--pure") || environment["OPENCODE_PURE"] != "1" || environment["OPENCODE_DISABLE_PROJECT_CONFIG"] != "1" || environment["OPENCODE_AUTH_CONTENT"] != "{}" {
		t.Fatal("system prompt weakened native config isolation")
	}
	if strings.Contains(environment["OPENCODE_CONFIG_CONTENT"], credential) {
		t.Fatal("system prompt configuration embeds the proxy capability")
	}
}

func assertOpenCodeSystemPromptProviderRestrictions(t *testing.T, config map[string]any, baseURL string) {
	t.Helper()
	provider := config["provider"].(map[string]any)[openCodeProviderID].(map[string]any)
	model := provider["models"].(map[string]any)["openai/gpt-test"].(map[string]any)
	limits := model["limit"].(map[string]any)
	if limits["context"] != float64(32768) || limits["output"] != float64(4096) || config["model"] != "orka/openai/gpt-test" || config["small_model"] != "orka/openai/gpt-test" {
		t.Fatal("system prompt changed immutable model selection or token limits")
	}
	options := provider["options"].(map[string]any)
	if options["apiKey"] != "{env:"+openCodeProviderEnvName+"}" || options["baseURL"] != baseURL {
		t.Fatal("system prompt changed the isolated provider proxy settings")
	}
}

func TestOpenCodeSystemPromptDoesNotAuthorizeUnsupportedControls(t *testing.T) {
	paths := acp.SessionPaths{Workspace: "/sessions/private/workspace"}
	proxy := ProviderProxyBinding{BaseURL: "http://127.0.0.1:43210/_orka/provider/session/v1", Credential: "synthetic-prompt-test-capability"}
	profile, err := providerProfile(providerKindOpencode, "openai/gpt-test", harnessv2.WorkspaceIntentRead, testOpenCodeModelLimits())
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{providerToolWebFetch, providerToolWebSearch} {
		t.Run(tool, func(t *testing.T) {
			request := testProviderProjectionRequest(t, providerKindOpencode, "openai/gpt-test", "A literal synthetic prompt.", "", []string{tool}, nil, false)
			request.Profile.ModelLimits = testOpenCodeModelLimits()
			if _, err := profile.ProjectSession(request, paths, proxy); err == nil || !strings.Contains(err.Error(), "provider-native tool") {
				t.Fatal("system prompt bypassed unsupported native tool rejection")
			}
		})
	}
	request := testProviderProjectionRequest(t, providerKindOpencode, "openai/gpt-test", "A literal synthetic prompt.", "", nil, nil, false)
	request.Profile.ModelLimits = testOpenCodeModelLimits()
	request.AgentConfiguration.ReasoningEffort = "high"
	if _, err := profile.ProjectSession(request, paths, proxy); err == nil || !strings.Contains(err.Error(), "reasoning effort") {
		t.Fatal("system prompt bypassed reasoning effort rejection")
	}
	request.AgentConfiguration.ReasoningEffort = ""
	request.AgentConfiguration.SystemPrompt += " Unbound changes."
	if _, err := profile.ProjectSession(request, paths, proxy); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatal("system prompt bypassed immutable Agent configuration validation")
	}
	for _, limits := range []*harnessv2.ModelTokenLimits{nil, {Context: 32768, Output: 0}} {
		if _, err := openCodeSessionConfig("openai/gpt-test", limits, harnessv2.WorkspaceIntentRead, request, paths, proxy); err == nil || !strings.Contains(err.Error(), "model token limits") {
			t.Fatal("system prompt bypassed required model-token limit validation")
		}
	}
}
