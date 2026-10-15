package supervisor

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func TestHyperlightOnlyCommandPolicy(t *testing.T) {
	for _, tt := range []struct {
		name      string
		allowBash bool
		tools     []harnessv2.MCPToolDescriptor
		want      bool
	}{
		{name: "ordinary read-only policy"},
		{name: "ordinary native commands", allowBash: true},
		{
			name:  "runtime-local sandbox without native Bash",
			tools: []harnessv2.MCPToolDescriptor{{Name: acp.SandboxExecToolName, Source: harnessv2.MCPToolSourceRuntimeLocal}},
			want:  true,
		},
		{
			name:      "sandbox with native Bash is not strict",
			allowBash: true,
			tools:     []harnessv2.MCPToolDescriptor{{Name: acp.SandboxExecToolName, Source: harnessv2.MCPToolSourceRuntimeLocal}},
		},
		{
			name:  "brokered name is not a runtime-local sandbox",
			tools: []harnessv2.MCPToolDescriptor{{Name: acp.SandboxExecToolName, Source: harnessv2.MCPToolSourceBrokeredBuiltin}},
		},
		{
			name:  "custom name is not a runtime-local sandbox",
			tools: []harnessv2.MCPToolDescriptor{{Name: acp.SandboxExecToolName, Source: harnessv2.MCPToolSourceBrokeredCustom}},
		},
		{
			name:  "native name is not a runtime-local sandbox",
			tools: []harnessv2.MCPToolDescriptor{{Name: acp.SandboxExecToolName, Source: harnessv2.MCPToolSourceProviderNative}},
		},
		{
			name:  "another runtime-local tool is not a sandbox",
			tools: []harnessv2.MCPToolDescriptor{{Name: "another_tool", Source: harnessv2.MCPToolSourceRuntimeLocal}},
		},
		{
			name:  "sandbox descriptor is matched exactly",
			tools: []harnessv2.MCPToolDescriptor{{Name: "Sandbox_Exec", Source: harnessv2.MCPToolSourceRuntimeLocal}},
		},
		{
			name: "sandbox is found alongside other grants",
			tools: []harnessv2.MCPToolDescriptor{
				{Name: providerToolRead, Source: harnessv2.MCPToolSourceProviderNative},
				{Name: "web_fetch", Source: harnessv2.MCPToolSourceBrokeredBuiltin},
				{Name: acp.SandboxExecToolName, Source: harnessv2.MCPToolSourceRuntimeLocal},
			},
			want: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			request := harnessv2.CreateRuntimeSessionRequest{
				MCPConfiguration: harnessv2.MCPPolicyConfiguration{
					ToolPolicy: harnessv2.MCPToolPolicy{
						AllowedToolNames: []string{acp.SandboxExecToolName},
						AllowBash:        tt.allowBash,
						Tools:            tt.tools,
					},
				},
			}
			for _, intent := range []harnessv2.WorkspaceIntent{harnessv2.WorkspaceIntentRead, harnessv2.WorkspaceIntentWrite} {
				request.Profile.WorkspaceIntent = intent
				if got := hyperlightOnlyCommandPolicy(request); got != tt.want {
					t.Errorf("intent %s: strict policy = %t, want %t", intent, got, tt.want)
				}
			}
		})
	}
}

func TestClaudeHyperlightCommandOptions(t *testing.T) {
	request := testHyperlightCommandPolicyRequest()
	before := request.MCPConfiguration.ToolPolicy
	options := claudeHyperlightCommandOptions(request)
	want := map[string]any{
		"strictMcpConfig": true,
		"settings":        map[string]any{"disableAllHooks": true},
	}
	if !reflect.DeepEqual(options, want) {
		t.Fatalf("Claude SDK overlay = %#v, want %#v", options, want)
	}
	if !reflect.DeepEqual(request.MCPConfiguration.ToolPolicy, before) {
		t.Fatal("provider overlay changed frozen policy")
	}

	// These belong to the existing Orka projection or the trusted ACP adapter.
	// In particular, settingSources must keep loading project CLAUDE.md and
	// SDK hooks/canUseTool must not be replaced by filesystem-hook controls.
	existing := map[string]any{
		"maxTurns":        int32(7),
		"effort":          "max",
		"tools":           []string{providerToolRead, providerToolWrite},
		"disallowedTools": []string{providerToolBash},
		"settingSources":  []string{"user", "project", "local"},
		"hooks":           map[string]any{"PostToolUse": "trusted adapter callback"},
		"canUseTool":      "trusted permission callback",
	}
	original := maps.Clone(existing)
	maps.Copy(existing, options)
	for name, value := range original {
		if !reflect.DeepEqual(existing[name], value) {
			t.Errorf("SDK overlay changed %s", name)
		}
	}
	for _, name := range []string{"disableAllHooks", "settingSources", "hooks", "canUseTool", "managedSettings", "mcpServers"} {
		if _, found := options[name]; found {
			t.Errorf("SDK overlay unexpectedly sets %s", name)
		}
	}

	// No shared map can carry options between independent sessions.
	options["settings"].(map[string]any)["disableAllHooks"] = false
	if !reflect.DeepEqual(claudeHyperlightCommandOptions(request), want) {
		t.Fatal("SDK overlay reused mutable options from a previous session")
	}
}

func TestClaudeHyperlightCommandOptionsLeaveOtherSessionsUnchanged(t *testing.T) {
	request := testHyperlightCommandPolicyRequest()
	request.MCPConfiguration.ToolPolicy.AllowBash = true
	if got := claudeHyperlightCommandOptions(request); got != nil {
		t.Fatalf("sandbox with native Bash received strict options: %#v", got)
	}
	request.MCPConfiguration.ToolPolicy.AllowBash = false
	request.MCPConfiguration.ToolPolicy.Tools = nil
	if got := claudeHyperlightCommandOptions(request); got != nil {
		t.Fatalf("ordinary read-only session received strict options: %#v", got)
	}
}

func testHyperlightCommandPolicyRequest() harnessv2.CreateRuntimeSessionRequest {
	return harnessv2.CreateRuntimeSessionRequest{
		MCPConfiguration: harnessv2.MCPPolicyConfiguration{
			ToolPolicy: harnessv2.MCPToolPolicy{
				AllowBash: false,
				Tools: []harnessv2.MCPToolDescriptor{{
					Name: acp.SandboxExecToolName, Source: harnessv2.MCPToolSourceRuntimeLocal,
				}},
			},
		},
	}
}

func TestValidateCopilotHyperlightCommandPolicy(t *testing.T) {
	request := testHyperlightCommandPolicyRequest()
	err := validateCopilotHyperlightCommandPolicy(request)
	if err == nil || !strings.Contains(err.Error(), acp.CopilotCLIVersion) ||
		!strings.Contains(err.Error(), "cannot enforce Hyperlight-only commands") {
		t.Fatalf("strict Copilot session must fail closed with its pinned version: %v", err)
	}
	request.MCPConfiguration.ToolPolicy.AllowBash = true
	if err := validateCopilotHyperlightCommandPolicy(request); err != nil {
		t.Fatalf("sandbox with native Bash must remain supported: %v", err)
	}
	request.MCPConfiguration.ToolPolicy.AllowBash = false
	request.MCPConfiguration.ToolPolicy.Tools = nil
	if err := validateCopilotHyperlightCommandPolicy(request); err != nil {
		t.Fatalf("ordinary read-only session must remain supported: %v", err)
	}
}

func TestSandboxExecSessionsWaitForTheLongestCall(t *testing.T) {
	milliseconds := strconv.FormatInt(acp.SandboxExecClientTimeout.Milliseconds(), 10)
	for _, granted := range []bool{true, false} {
		t.Run(fmt.Sprintf("granted=%t", granted), func(t *testing.T) {
			grant := func(tools ...string) []string {
				if granted {
					return append(tools, acp.SandboxExecToolName)
				}
				return tools
			}

			claude, err := claudeSessionProjection(
				testProviderProjectionRequest(t, providerKindClaude, "claude-test", "", "high", grant(providerToolRead), nil, false),
				acp.SessionPaths{}, ProviderProxyBinding{}, "claude-test")
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"MCP_TOOL_TIMEOUT", "CLAUDE_CODE_MCP_TOOL_IDLE_TIMEOUT"} {
				if got, want := claude.Environment[name], map[bool]string{true: milliseconds}[granted]; got != want {
					t.Errorf("Claude %s = %q, want %q", name, got, want)
				}
			}

			codex, err := codexSessionProjection(
				testProviderProjectionRequest(t, providerKindCodex, "gpt-test", "", "", grant(acp.BuiltInRuntimeNativeToolNames(providerKindCodex)...), nil, true),
				acp.SessionPaths{}, ProviderProxyBinding{}, "gpt-test")
			if err != nil {
				t.Fatal(err)
			}
			if got, want := codex.Environment["ORKA_CODEX_MCP_TOOL_TIMEOUT_SEC"], map[bool]string{true: "1260"}[granted]; got != want {
				t.Errorf("Codex tool timeout = %q, want %q", got, want)
			}

			data, err := openCodeSessionConfig("openai/gpt-test", testOpenCodeModelLimits(), harnessv2.WorkspaceIntentRead,
				testProviderProjectionRequest(t, providerKindOpencode, "openai/gpt-test", "", "", grant(), nil, false),
				acp.SessionPaths{}, ProviderProxyBinding{})
			if err != nil {
				t.Fatal(err)
			}
			var config struct {
				Experimental *struct {
					MCPTimeout int64 `json:"mcp_timeout"`
				} `json:"experimental"`
			}
			if err := json.Unmarshal(data, &config); err != nil {
				t.Fatal(err)
			}
			if granted && (config.Experimental == nil || config.Experimental.MCPTimeout != acp.SandboxExecClientTimeout.Milliseconds()) {
				t.Errorf("OpenCode experimental = %+v, want mcp_timeout %s", config.Experimental, milliseconds)
			}
			if !granted && config.Experimental != nil {
				t.Errorf("OpenCode without sandbox_exec changed its MCP timeout: %+v", config.Experimental)
			}
		})
	}
}
