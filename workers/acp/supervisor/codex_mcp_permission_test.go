package supervisor

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

// Exact structural envelopes from the pinned codex-acp 2.1.1 bundle's
// McpToolReporter and buildMcpPermissionRequest. The permission callback
// must retain the structured identity from the same prompt's preceding update.
func TestCodexMCPPermissionUsesPinnedStructuredIdentity(t *testing.T) {
	server, cfg, _ := newTestServer(t, "immediate")
	fence := cfg.Fence
	fence.RuntimeSessionUID = "permission-session"
	fence.RuntimeSessionGeneration = 1
	state := &sessionState{
		descriptor:  harnessv2.RuntimeSessionDescriptor{RuntimeSessionUID: fence.RuntimeSessionUID, Generation: 1},
		profile:     harnessv2.RuntimeProfile{ProviderKind: providerKindCodex},
		permissions: make(map[harnessv2.PermissionRequestID]permissionState),
		mcpProxy: &mcpProxySession{configuration: harnessv2.MCPPolicyConfiguration{ToolPolicy: harnessv2.MCPToolPolicy{
			AllowedToolNames: []string{"reply_in_conversation"},
			Tools:            []harnessv2.MCPToolDescriptor{{Name: "reply_in_conversation", Source: harnessv2.MCPToolSourceBrokeredBuiltin}},
		}}},
	}
	prompt := &promptState{request: testStartPromptRequest(t, cfg, fence)}
	_, err := server.mapRuntimeEvent(state, prompt, acp.PromptEvent{
		Type: acp.PromptEventUpdate, Timestamp: time.Now().UTC(),
		Update: &acp.SessionNotification{Update: json.RawMessage(`{"sessionUpdate":"tool_call","toolCallId":"call-1","kind":"execute","title":"mcp.orka.reply_in_conversation","status":"in_progress","rawInput":{"server":"orka","tool":"reply_in_conversation","arguments":{"content":"synthetic"}},"_meta":{"is_mcp_tool_call":true}}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	mapped, err := server.mapRuntimeEvent(state, prompt, acp.PromptEvent{
		Type: acp.PromptEventPermissionRequested, Timestamp: time.Now().UTC(),
		Permission: &acp.PermissionRequestEvent{RequestID: "permission-1", Request: acp.RequestPermissionRequest{
			ToolCall: json.RawMessage(`{"toolCallId":"call-1","kind":"execute","status":"pending"}`),
			Meta:     acp.Meta{"is_mcp_tool_approval": true},
			Options:  []acp.PermissionOption{{OptionID: "allow_once", Name: "Allow", Kind: "allow_once"}, {OptionID: "cancel", Name: "Cancel", Kind: "reject_once"}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if mapped.PermissionRequested.ToolName != "reply_in_conversation" {
		t.Fatalf("pinned Codex MCP permission identity = %q, want reply_in_conversation; empty identity causes frozen policy to reject before broker dispatch", mapped.PermissionRequested.ToolName)
	}
}

func TestCodexMCPPermissionIdentityFailsClosed(t *testing.T) {
	const reply = "reply_in_conversation"
	policy := harnessv2.MCPToolPolicy{
		AllowedToolNames: []string{"Read", reply, "run_validation"},
		Tools: []harnessv2.MCPToolDescriptor{
			{Name: "Read", Source: harnessv2.MCPToolSourceProviderNative},
			{Name: reply, Source: harnessv2.MCPToolSourceBrokeredBuiltin},
			{Name: "run_validation", Source: harnessv2.MCPToolSourceBrokeredCustom},
		},
	}
	for _, test := range []struct {
		name     string
		provider string
		update   string
		deny     bool
		want     string
		wantErr  bool
	}{
		{name: "registered builtin", update: `{"rawInput":{"server":"orka","tool":"reply_in_conversation"},"_meta":{"is_mcp_tool_call":true}}`, want: reply},
		{name: "registered custom", update: `{"rawInput":{"server":"orka","tool":"run_validation"},"_meta":{"is_mcp_tool_call":true}}`, want: "run_validation"},
		{name: "matching explicit identity", update: `{"name":"reply_in_conversation","rawInput":{"server":"orka","tool":"reply_in_conversation"},"_meta":{"is_mcp_tool_call":true}}`, want: reply},
		{name: "display title alone", update: `{"title":"mcp.orka.reply_in_conversation"}`},
		{name: "unmarked raw input", update: `{"rawInput":{"server":"orka","tool":"reply_in_conversation"}}`},
		{name: "false marker", update: `{"rawInput":{"server":"orka","tool":"reply_in_conversation"},"_meta":{"is_mcp_tool_call":false}}`},
		{name: "brokered explicit identity without marker", update: `{"name":"reply_in_conversation","rawInput":{"server":"other","tool":"reply_in_conversation"}}`, wantErr: true},
		{name: "brokered explicit identity with false marker", update: `{"name":"reply_in_conversation","rawInput":{"server":"other","tool":"reply_in_conversation"},"_meta":{"is_mcp_tool_call":false}}`, wantErr: true},
		{name: "brokered explicit identity with null marker", update: `{"name":"reply_in_conversation","_meta":{"is_mcp_tool_call":null}}`, wantErr: true},
		{name: "null marker is not a boolean", update: `{"_meta":{"is_mcp_tool_call":null}}`, wantErr: true},
		{name: "native explicit identity without marker", update: `{"name":"Read"}`, want: "Read"},
		{name: "native explicit identity with false marker", update: `{"name":"Read","_meta":{"is_mcp_tool_call":false}}`, want: "Read"},
		{name: "nonexecute MCP kind", update: `{"kind":"read","rawInput":{"server":"orka","tool":"reply_in_conversation"},"_meta":{"is_mcp_tool_call":true}}`, wantErr: true},
		{name: "empty MCP kind", update: `{"kind":"","rawInput":{"server":"orka","tool":"reply_in_conversation"},"_meta":{"is_mcp_tool_call":true}}`, wantErr: true},
		{name: "wrong server", update: `{"rawInput":{"server":"other","tool":"reply_in_conversation"},"_meta":{"is_mcp_tool_call":true}}`, wantErr: true},
		{name: "native name collision", update: `{"rawInput":{"server":"orka","tool":"Read"},"_meta":{"is_mcp_tool_call":true}}`, wantErr: true},
		{name: "unregistered tool", update: `{"rawInput":{"server":"orka","tool":"unknown"},"_meta":{"is_mcp_tool_call":true}}`, wantErr: true},
		{name: "explicit denial", update: `{"rawInput":{"server":"orka","tool":"reply_in_conversation"},"_meta":{"is_mcp_tool_call":true}}`, deny: true, wantErr: true},
		{name: "missing raw input", update: `{"title":"mcp.orka.reply_in_conversation","_meta":{"is_mcp_tool_call":true}}`, wantErr: true},
		{name: "wrong raw input type", update: `{"rawInput":"reply_in_conversation","_meta":{"is_mcp_tool_call":true}}`, wantErr: true},
		{name: "wrong marker type", update: `{"_meta":{"is_mcp_tool_call":"true"}}`, wantErr: true},
		{name: "conflicting explicit identity", update: `{"name":"Read","rawInput":{"server":"orka","tool":"reply_in_conversation"},"_meta":{"is_mcp_tool_call":true}}`, wantErr: true},
		{name: "Claude cannot use Codex envelope", provider: providerKindClaude, update: `{"rawInput":{"server":"orka","tool":"reply_in_conversation"},"_meta":{"is_mcp_tool_call":true}}`},
		{name: "other provider metadata remains opaque", provider: providerKindClaude, update: `{"_meta":{"is_mcp_tool_call":"provider-extension"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := test.provider
			if provider == "" {
				provider = providerKindCodex
			}
			var update map[string]any
			if err := json.Unmarshal([]byte(test.update), &update); err != nil {
				t.Fatal(err)
			}
			if _, hasKind := update["kind"]; !hasKind {
				update["kind"] = "execute"
			}
			update["sessionUpdate"] = "tool_call"
			update["toolCallId"] = "call-1"
			data, err := json.Marshal(update)
			if err != nil {
				t.Fatal(err)
			}
			frozen := policy
			if test.deny {
				frozen.DisallowedToolNames = []string{reply}
			}
			prompt := &promptState{}
			err = prompt.rememberToolCallName(&acp.SessionNotification{Update: data}, provider, frozen)
			if (err != nil) != test.wantErr {
				t.Fatalf("remember identity error = %v, want error %v", err, test.wantErr)
			}
			id, err := canonicalACPToolCallID("call-1")
			if err != nil {
				t.Fatal(err)
			}
			if got := prompt.toolCallNames[id]; got != test.want {
				t.Fatalf("remembered tool = %q, want %q", got, test.want)
			}
		})
	}
}

func TestCodexMCPPermissionIdentityCannotCrossCallsOrPrompts(t *testing.T) {
	server, cfg, _ := newTestServer(t, "immediate")
	fence := cfg.Fence
	fence.RuntimeSessionUID = "permission-session"
	fence.RuntimeSessionGeneration = 1
	policy := harnessv2.MCPToolPolicy{
		AllowedToolNames: []string{"reply_in_conversation", "run_validation"},
		Tools: []harnessv2.MCPToolDescriptor{
			{Name: "reply_in_conversation", Source: harnessv2.MCPToolSourceBrokeredBuiltin},
			{Name: "run_validation", Source: harnessv2.MCPToolSourceBrokeredCustom},
		},
	}
	original := &promptState{request: testStartPromptRequest(t, cfg, fence)}
	update := &acp.SessionNotification{Update: json.RawMessage(`{"sessionUpdate":"tool_call","toolCallId":"call-1","kind":"execute","rawInput":{"server":"orka","tool":"reply_in_conversation"},"_meta":{"is_mcp_tool_call":true}}`)}
	if err := original.rememberToolCallName(update, providerKindCodex, policy); err != nil {
		t.Fatal(err)
	}
	changed := &acp.SessionNotification{Update: json.RawMessage(`{"sessionUpdate":"tool_call","toolCallId":"call-1","kind":"execute","rawInput":{"server":"orka","tool":"run_validation"},"_meta":{"is_mcp_tool_call":true}}`)}
	if err := original.rememberToolCallName(changed, providerKindCodex, policy); err == nil {
		t.Fatal("same call ID changed its structured tool identity")
	}
	for _, test := range []struct {
		name   string
		prompt *promptState
		call   string
	}{
		{name: "different call", prompt: original, call: "call-2"},
		{name: "new prompt", prompt: &promptState{request: original.request}, call: "call-1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := &sessionState{
				descriptor: harnessv2.RuntimeSessionDescriptor{RuntimeSessionUID: fence.RuntimeSessionUID, Generation: 1},
				profile:    harnessv2.RuntimeProfile{ProviderKind: providerKindCodex}, permissions: make(map[harnessv2.PermissionRequestID]permissionState),
				mcpProxy: &mcpProxySession{configuration: harnessv2.MCPPolicyConfiguration{ToolPolicy: policy}},
			}
			call, err := json.Marshal(map[string]string{"toolCallId": test.call, "kind": "execute", "title": "mcp.orka.reply_in_conversation"})
			if err != nil {
				t.Fatal(err)
			}
			mapped, err := server.mapRuntimeEvent(state, test.prompt, acp.PromptEvent{
				Type: acp.PromptEventPermissionRequested, Timestamp: time.Now().UTC(),
				Permission: &acp.PermissionRequestEvent{RequestID: "permission-1", Request: acp.RequestPermissionRequest{
					ToolCall: call, Options: []acp.PermissionOption{{OptionID: "allow", Name: "Allow once", Kind: "allow_once"}},
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if mapped.PermissionRequested.ToolName != "" {
				t.Fatal("permission borrowed identity from another call or prompt")
			}
		})
	}
}

func TestCodexMCPPermissionRejectsUncorrelatedBrokeredName(t *testing.T) {
	for _, test := range []struct {
		name, provider, tool string
		wantErr              bool
	}{
		{name: "Codex brokered name needs preceding MCP identity", provider: providerKindCodex, tool: "reply_in_conversation", wantErr: true},
		{name: "Codex native identity is unchanged", provider: providerKindCodex, tool: "Read"},
		{name: "Claude explicit identity is unchanged", provider: providerKindClaude, tool: "reply_in_conversation"},
		{name: "Copilot explicit identity is unchanged", provider: providerKindCopilot, tool: "reply_in_conversation"},
		{name: "OpenCode explicit identity is unchanged", provider: providerKindOpencode, tool: "reply_in_conversation"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, cfg, _ := newTestServer(t, "immediate")
			fence := cfg.Fence
			fence.RuntimeSessionUID = "permission-session"
			fence.RuntimeSessionGeneration = 1
			policy := harnessv2.MCPToolPolicy{AllowedToolNames: []string{"Read", "reply_in_conversation"}, Tools: []harnessv2.MCPToolDescriptor{{Name: "Read", Source: harnessv2.MCPToolSourceProviderNative}, {Name: "reply_in_conversation", Source: harnessv2.MCPToolSourceBrokeredBuiltin}}}
			state := &sessionState{descriptor: harnessv2.RuntimeSessionDescriptor{RuntimeSessionUID: fence.RuntimeSessionUID, Generation: 1}, profile: harnessv2.RuntimeProfile{ProviderKind: test.provider}, permissions: make(map[harnessv2.PermissionRequestID]permissionState), mcpProxy: &mcpProxySession{configuration: harnessv2.MCPPolicyConfiguration{ToolPolicy: policy}}}
			prompt := &promptState{request: testStartPromptRequest(t, cfg, fence)}
			call, err := json.Marshal(map[string]string{"toolCallId": "call-1", "name": test.tool, "kind": "execute"})
			if err != nil {
				t.Fatal(err)
			}
			mapped, err := server.mapRuntimeEvent(state, prompt, acp.PromptEvent{Type: acp.PromptEventPermissionRequested, Timestamp: time.Now().UTC(), Permission: &acp.PermissionRequestEvent{RequestID: "permission-1", Request: acp.RequestPermissionRequest{ToolCall: call, Options: []acp.PermissionOption{{OptionID: "allow", Name: "Allow once", Kind: "allow_once"}}}}})
			if (err != nil) != test.wantErr {
				t.Fatalf("permission mapping error = %v, want error %v", err, test.wantErr)
			}
			if !test.wantErr && mapped.PermissionRequested.ToolName != test.tool {
				t.Fatalf("permission tool = %q, want %q", mapped.PermissionRequested.ToolName, test.tool)
			}
		})
	}
}

func TestCodexMCPPermissionValidatesCallbackMarker(t *testing.T) {
	for _, test := range []struct {
		name, provider, marker string
		wantErr                bool
	}{
		{name: "Codex ID-only callback", provider: providerKindCodex},
		{name: "Codex boolean marker", provider: providerKindCodex, marker: "true"},
		{name: "Codex string marker", provider: providerKindCodex, marker: `"true"`, wantErr: true},
		{name: "Codex null marker", provider: providerKindCodex, marker: "null", wantErr: true},
		{name: "Codex number marker", provider: providerKindCodex, marker: "1", wantErr: true},
		{name: "Codex object marker", provider: providerKindCodex, marker: "{}", wantErr: true},
		{name: "Codex array marker", provider: providerKindCodex, marker: "[]", wantErr: true},
		{name: "Claude metadata remains opaque", provider: providerKindClaude, marker: `"provider-extension"`},
		{name: "Copilot metadata remains opaque", provider: providerKindCopilot, marker: `"provider-extension"`},
		{name: "OpenCode metadata remains opaque", provider: providerKindOpencode, marker: `"provider-extension"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, cfg, _ := newTestServer(t, "immediate")
			fence := cfg.Fence
			fence.RuntimeSessionUID = "permission-session"
			fence.RuntimeSessionGeneration = 1
			policy := harnessv2.MCPToolPolicy{
				AllowedToolNames: []string{"reply_in_conversation"},
				Tools:            []harnessv2.MCPToolDescriptor{{Name: "reply_in_conversation", Source: harnessv2.MCPToolSourceBrokeredBuiltin}},
			}
			state := &sessionState{
				descriptor:  harnessv2.RuntimeSessionDescriptor{RuntimeSessionUID: fence.RuntimeSessionUID, Generation: 1},
				profile:     harnessv2.RuntimeProfile{ProviderKind: test.provider},
				permissions: make(map[harnessv2.PermissionRequestID]permissionState),
				mcpProxy:    &mcpProxySession{configuration: harnessv2.MCPPolicyConfiguration{ToolPolicy: policy}},
			}
			prompt := &promptState{request: testStartPromptRequest(t, cfg, fence)}
			_, err := server.mapRuntimeEvent(state, prompt, acp.PromptEvent{
				Type: acp.PromptEventUpdate, Timestamp: time.Now().UTC(),
				Update: &acp.SessionNotification{Update: json.RawMessage(`{"sessionUpdate":"tool_call","toolCallId":"call-1","name":"reply_in_conversation","kind":"execute","status":"in_progress","rawInput":{"server":"orka","tool":"reply_in_conversation"},"_meta":{"is_mcp_tool_call":true}}`)},
			})
			if err != nil {
				t.Fatal(err)
			}
			call := map[string]any{"toolCallId": "call-1", "kind": "execute"}
			if test.marker != "" {
				call["_meta"] = map[string]json.RawMessage{"is_mcp_tool_call": json.RawMessage(test.marker)}
			}
			payload, err := json.Marshal(call)
			if err != nil {
				t.Fatal(err)
			}
			mapped, err := server.mapRuntimeEvent(state, prompt, acp.PromptEvent{
				Type: acp.PromptEventPermissionRequested, Timestamp: time.Now().UTC(),
				Permission: &acp.PermissionRequestEvent{RequestID: "permission-1", Request: acp.RequestPermissionRequest{
					ToolCall: payload, Options: []acp.PermissionOption{{OptionID: "allow", Name: "Allow once", Kind: "allow_once"}},
				}},
			})
			if (err != nil) != test.wantErr {
				t.Fatalf("permission mapping error = %v, want error %v", err, test.wantErr)
			}
			if test.wantErr {
				if mapped != nil || len(state.permissions) != 0 || len(prompt.permissionRequestIDs) != 0 {
					t.Fatal("invalid callback marker recorded a pending permission")
				}
				return
			}
			if mapped.PermissionRequested.ToolName != "reply_in_conversation" || len(state.permissions) != 1 {
				t.Fatal("valid callback did not retain its correlated tool identity")
			}
		})
	}
}
