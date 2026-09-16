package supervisor

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

// These envelopes match the pinned codex-acp 1.1.7 mapper and its correlated
// MCP approval request. The display title is deliberately not authoritative.
func TestCodexMCPPermissionUsesCorrelatedStructuredIdentity(t *testing.T) {
	for _, test := range []struct {
		name             string
		updates          []string
		permission       string
		provider         string
		freshPrompt      bool
		nonMCPPermission bool
		wantName         string
		wantError        bool
	}{
		{
			name:     "authorized Orka MCP tool",
			updates:  []string{`{"sessionUpdate":"tool_call","toolCallId":"call/1","kind":"execute","title":"untrusted display","rawInput":{"server":"orka","tool":"web_search","arguments":{"query":"public news"}},"_meta":{"is_mcp_tool_call":true}}`},
			wantName: "web_search",
		},
		{
			name:      "invalid UTF8 tool call ID is rejected",
			updates:   []string{"{\"sessionUpdate\":\"tool_call\",\"toolCallId\":\"\xff\",\"rawInput\":{\"server\":\"orka\",\"tool\":\"web_search\"},\"_meta\":{\"is_mcp_tool_call\":true}}"},
			wantError: true,
		},
		{
			name:      "malformed update call ID cannot establish identity",
			updates:   []string{`{"sessionUpdate":"tool_call","toolCallId":"\ud800","rawInput":{"server":"orka","tool":"web_search"},"_meta":{"is_mcp_tool_call":true}}`},
			wantError: true,
		},
		{
			name:       "malformed permission ID cannot borrow replacement-character call",
			updates:    []string{`{"sessionUpdate":"tool_call","toolCallId":"\ufffd","rawInput":{"server":"orka","tool":"web_search"},"_meta":{"is_mcp_tool_call":true}}`},
			permission: `{"toolCallId":"\ud801","kind":"execute"}`,
			wantError:  true,
		},
		{
			name:       "legitimate replacement-character ID remains supported",
			updates:    []string{`{"sessionUpdate":"tool_call","toolCallId":"\ufffd","rawInput":{"server":"orka","tool":"web_search"},"_meta":{"is_mcp_tool_call":true}}`},
			permission: `{"toolCallId":"\ufffd","kind":"execute"}`,
			wantName:   "web_search",
		},
		{
			name:      "marked name-only update cannot establish authority",
			updates:   []string{`{"sessionUpdate":"tool_call","toolCallId":"call/1","name":"web_search","_meta":{"is_mcp_tool_call":true}}`},
			wantError: true,
		},
		{
			name:      "unmarked name-only update cannot seed MCP authority",
			updates:   []string{`{"sessionUpdate":"tool_call","toolCallId":"call/1","name":"web_search"}`},
			wantError: true,
		},
		{
			name: "marked partial update retains verified identity",
			updates: []string{
				`{"sessionUpdate":"tool_call","toolCallId":"call/1","rawInput":{"server":"orka","tool":"web_search"},"_meta":{"is_mcp_tool_call":true}}`,
				`{"sessionUpdate":"tool_call_update","toolCallId":"call/1","status":"in_progress","_meta":{"is_mcp_tool_call":true}}`,
			},
			wantName: "web_search",
		},
		{
			name: "partial progress retains identity",
			updates: []string{
				`{"sessionUpdate":"tool_call","toolCallId":"call/1","rawInput":{"server":"orka","tool":"web_search","arguments":{}},"_meta":{"is_mcp_tool_call":true}}`,
				`{"sessionUpdate":"tool_call_update","toolCallId":"call/1","status":"in_progress"}`,
			},
			wantName: "web_search",
		},
		{
			name: "unmarked completion confirms existing identity",
			updates: []string{
				`{"sessionUpdate":"tool_call","toolCallId":"call/1","kind":"execute","rawInput":{"server":"orka","tool":"web_search","arguments":{"query":"public news"}},"_meta":{"is_mcp_tool_call":true}}`,
				`{"sessionUpdate":"tool_call_update","toolCallId":"call/1","status":"completed","rawInput":{"server":"orka","tool":"web_search","arguments":{"query":"public news"}},"rawOutput":{"result":{"content":[]},"error":null}}`,
			},
			wantName: "web_search",
		},
		{
			name: "unmarked completion cannot change server",
			updates: []string{
				`{"sessionUpdate":"tool_call","toolCallId":"call/1","rawInput":{"server":"orka","tool":"web_search"},"_meta":{"is_mcp_tool_call":true}}`,
				`{"sessionUpdate":"tool_call_update","toolCallId":"call/1","status":"completed","rawInput":{"server":"other","tool":"web_search"}}`,
			},
			wantError: true,
		},
		{
			name:     "dotted tool on exact Orka server is permitted",
			updates:  []string{`{"sessionUpdate":"tool_call","toolCallId":"call/1","rawInput":{"server":"orka","tool":"web.search"},"_meta":{"is_mcp_tool_call":true}}`},
			wantName: "web.search",
		},
		{
			name:             "command elevation cannot borrow MCP identity",
			updates:          []string{`{"sessionUpdate":"tool_call","toolCallId":"call/1","rawInput":{"server":"orka","tool":"web_search"},"_meta":{"is_mcp_tool_call":true}}`},
			nonMCPPermission: true,
			wantError:        true,
		},
		{
			name: "unmarked replacement input cannot borrow MCP identity",
			updates: []string{
				`{"sessionUpdate":"tool_call","toolCallId":"call/1","rawInput":{"server":"orka","tool":"web_search"},"_meta":{"is_mcp_tool_call":true}}`,
				`{"sessionUpdate":"tool_call_update","toolCallId":"call/1","rawInput":{"command":"not authorized"}}`,
			},
			wantError: true,
		},
		{
			name:       "direct brokered name without a prior call is not authority",
			permission: `{"toolCallId":"call/1","name":"web_search","kind":"execute"}`,
			wantError:  true,
		},
		{
			name:      "malformed MCP marker is rejected",
			updates:   []string{`{"sessionUpdate":"tool_call","toolCallId":"call/1","rawInput":{"server":"orka","tool":"web_search"},"_meta":{"is_mcp_tool_call":"true"}}`},
			wantError: true,
		},
		{
			name:      "malformed Unicode cannot identify a tool",
			updates:   []string{`{"sessionUpdate":"tool_call","toolCallId":"call/1","rawInput":{"server":"orka","tool":"\ud800"},"_meta":{"is_mcp_tool_call":true}}`},
			wantError: true,
		},
		{
			name:    "display title cannot grant authority",
			updates: []string{`{"sessionUpdate":"tool_call","toolCallId":"call/1","kind":"execute","title":"mcp.orka.web_search"}`},
		},
		{
			name:    "unmarked raw input cannot grant authority",
			updates: []string{`{"sessionUpdate":"tool_call","toolCallId":"call/1","rawInput":{"server":"orka","tool":"web_search"}}`},
		},
		{
			name:     "other provider cannot borrow Codex metadata",
			provider: providerKindCopilot,
			updates:  []string{`{"sessionUpdate":"tool_call","toolCallId":"call/1","rawInput":{"server":"orka","tool":"web_search"},"_meta":{"is_mcp_tool_call":true}}`},
		},
		{
			name:      "foreign MCP server is rejected",
			updates:   []string{`{"sessionUpdate":"tool_call","toolCallId":"call/1","rawInput":{"server":"other","tool":"web_search"},"_meta":{"is_mcp_tool_call":true}}`},
			wantError: true,
		},
		{
			name:      "dotted foreign server cannot borrow an Orka tool",
			updates:   []string{`{"sessionUpdate":"tool_call","toolCallId":"call/1","rawInput":{"server":"orka.web","tool":"search"},"_meta":{"is_mcp_tool_call":true}}`},
			wantError: true,
		},
		{
			name:      "brokered identity cannot borrow native grant",
			updates:   []string{`{"sessionUpdate":"tool_call","toolCallId":"call/1","rawInput":{"server":"orka","tool":"Read"},"_meta":{"is_mcp_tool_call":true}}`},
			wantError: true,
		},
		{
			name:      "ungranted tool is rejected",
			updates:   []string{`{"sessionUpdate":"tool_call","toolCallId":"call/1","rawInput":{"server":"orka","tool":"web_fetch"},"_meta":{"is_mcp_tool_call":true}}`},
			wantError: true,
		},
		{
			name:      "malformed tool identity is rejected",
			updates:   []string{`{"sessionUpdate":"tool_call","toolCallId":"call/1","rawInput":{"server":"orka","tool":{}},"_meta":{"is_mcp_tool_call":true}}`},
			wantError: true,
		},
		{
			name:      "conflicting structured identity is rejected",
			updates:   []string{`{"sessionUpdate":"tool_call","toolCallId":"call/1","name":"Read","rawInput":{"server":"orka","tool":"web_search"},"_meta":{"is_mcp_tool_call":true}}`},
			wantError: true,
		},
		{
			name: "identity cannot change on a later update",
			updates: []string{
				`{"sessionUpdate":"tool_call","toolCallId":"call/1","rawInput":{"server":"orka","tool":"web_search"},"_meta":{"is_mcp_tool_call":true}}`,
				`{"sessionUpdate":"tool_call_update","toolCallId":"call/1","name":"Read"}`,
			},
			wantError: true,
		},
		{
			name:       "permission cannot change the correlated identity",
			updates:    []string{`{"sessionUpdate":"tool_call","toolCallId":"call/1","rawInput":{"server":"orka","tool":"web_search"},"_meta":{"is_mcp_tool_call":true}}`},
			permission: `{"toolCallId":"call/1","kind":"execute","name":"Read"}`,
			wantError:  true,
		},
		{
			name:        "identity is prompt scoped",
			updates:     []string{`{"sessionUpdate":"tool_call","toolCallId":"call/1","rawInput":{"server":"orka","tool":"web_search"},"_meta":{"is_mcp_tool_call":true}}`},
			freshPrompt: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, cfg, _ := newTestServer(t, "immediate")
			provider := test.provider
			if provider == "" {
				provider = providerKindCodex
			}
			state := &sessionState{
				descriptor:  harnessv2.RuntimeSessionDescriptor{RuntimeSessionUID: "permission-session", Generation: 1},
				profile:     harnessv2.RuntimeProfile{ProviderKind: provider},
				permissions: make(map[harnessv2.PermissionRequestID]permissionState),
				mcpProxy: &mcpProxySession{configuration: harnessv2.MCPPolicyConfiguration{ToolPolicy: harnessv2.MCPToolPolicy{
					AllowedToolNames: []string{"Read", "web_search", "web.search"},
					Tools: []harnessv2.MCPToolDescriptor{
						{Name: "Read", Source: harnessv2.MCPToolSourceProviderNative},
						{Name: "web.search", Source: harnessv2.MCPToolSourceBrokeredCustom},
						{Name: "web_search", Source: harnessv2.MCPToolSourceBrokeredBuiltin},
					},
				}}},
			}
			prompt := &promptState{request: testStartPromptRequest(t, cfg, cfg.Fence)}
			for _, update := range test.updates {
				_, err := server.mapRuntimeEvent(state, prompt, acp.PromptEvent{Type: acp.PromptEventUpdate, Timestamp: time.Now().UTC(), Update: &acp.SessionNotification{Update: json.RawMessage(update)}})
				if err != nil {
					if !test.wantError {
						t.Fatal(err)
					}
					if len(state.permissions) != 0 {
						t.Fatal("invalid update created permission authority")
					}
					return
				}
			}
			if test.freshPrompt {
				prompt = &promptState{request: prompt.request}
			}
			permission := test.permission
			if permission == "" {
				permission = `{"toolCallId":"call/1","kind":"execute","status":"pending"}`
			}
			mapped, err := server.mapRuntimeEvent(state, prompt, acp.PromptEvent{
				Type: acp.PromptEventPermissionRequested, Timestamp: time.Now().UTC(),
				Permission: &acp.PermissionRequestEvent{RequestID: "permission-1", Request: acp.RequestPermissionRequest{
					Meta:     acp.Meta{"is_mcp_tool_approval": !test.nonMCPPermission},
					ToolCall: json.RawMessage(permission), Options: []acp.PermissionOption{{OptionID: "allow", Name: "Allow", Kind: string(harnessv2.PermissionOptionAllowOnce)}},
				}},
			})
			if test.wantError {
				if err == nil || len(state.permissions) != 0 {
					t.Fatalf("invalid identity admitted: error=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if mapped.PermissionRequested.ToolName != test.wantName {
				t.Fatalf("permission tool = %q, want %q", mapped.PermissionRequested.ToolName, test.wantName)
			}
		})
	}
}

func TestCodexCorrelatedMCPPermissionHTTP(t *testing.T) {
	server, cfg, profile := newTestServer(t, "immediate")
	create := testCreateSessionRequest(t, cfg, profile)
	if response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg); response.Code != http.StatusCreated {
		t.Fatalf("create status = %d", response.Code)
	}
	now := time.Now().UTC()
	authorization, lease := buildTestMCPAuthorization(t, create.Metadata.Fence, now, "web_search", harnessv2.MCPToolEffectReadOnly, false)
	mutations := newBlockingPromptMutator()
	close(mutations.resolveRelease)
	server.mu.Lock()
	state := server.sessions[create.RuntimeSessionID]
	state.profile.ProviderKind = providerKindCodex
	state.prompt = &promptState{request: testStartPromptRequest(t, cfg, create.Metadata.Fence)}
	state.descriptor.State = harnessv2.RuntimeSessionStatePromptRunning
	state.promptMutations = mutations
	state.mcpProxy.mu.Lock()
	state.mcpProxy.configuration = authorization.Configuration()
	state.mcpProxy.authorization = &authorization
	state.mcpProxy.lease = lease
	state.mcpProxy.state = harnessv2.RuntimeSessionStatePromptRunning
	state.mcpProxy.mu.Unlock()
	server.mu.Unlock()
	_, err := server.mapRuntimeEvent(state, state.prompt, acp.PromptEvent{Type: acp.PromptEventUpdate, Timestamp: now, Update: &acp.SessionNotification{Update: json.RawMessage(`{"sessionUpdate":"tool_call","toolCallId":"call/1","title":"not authority","rawInput":{"server":"orka","tool":"web_search","arguments":{}},"_meta":{"is_mcp_tool_call":true}}`)}})
	if err == nil {
		_, err = server.mapRuntimeEvent(state, state.prompt, acp.PromptEvent{Type: acp.PromptEventPermissionRequested, Timestamp: now, Permission: &acp.PermissionRequestEvent{RequestID: "permission-1", Request: acp.RequestPermissionRequest{Meta: acp.Meta{"is_mcp_tool_approval": true}, ToolCall: json.RawMessage(`{"toolCallId":"call/1","kind":"execute","status":"pending"}`), Options: []acp.PermissionOption{{OptionID: "allow", Name: "Allow", Kind: string(harnessv2.PermissionOptionAllowOnce)}}}}})
	}
	if err != nil {
		t.Fatal(err)
	}
	request := harnessv2.ResolvePermissionRequest{Protocol: harnessv2.ProtocolVersion, Metadata: testMetadata(create.Metadata.Fence, "permission-operation-1", true), RequestID: "permission-1", Decision: harnessv2.PermissionDecision{Outcome: harnessv2.PermissionDecisionSelected, OptionID: "allow"}}
	sealRequest(t, &request.Metadata.RequestDigest, request)
	response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1/prompts/prompt-1/permissions/permission-1", request, cfg)
	if response.Code != http.StatusOK {
		t.Fatalf("correlated MCP permission status = %d: %s", response.Code, response.Body.String())
	}
	if mutations.resolveCalls.Load() != 1 {
		t.Fatal("permission did not reach the native ACP client exactly once")
	}
	_, err = server.mapRuntimeEvent(state, state.prompt, acp.PromptEvent{
		Type: acp.PromptEventUpdate, Timestamp: now.Add(time.Millisecond),
		Update: &acp.SessionNotification{Update: json.RawMessage(`{"sessionUpdate":"tool_call_update","toolCallId":"call/1","status":"completed","rawInput":{"server":"orka","tool":"web_search","arguments":{}},"rawOutput":{"result":{"content":[{"type":"text","text":"public result"}]},"error":null}}`)},
	})
	if err != nil {
		t.Fatalf("pinned Codex completion update was rejected after permission: %v", err)
	}
	state.mcpProxy.mu.Lock()
	approvalCount := len(state.mcpProxy.approvals)
	state.mcpProxy.mu.Unlock()
	if approvalCount != 0 {
		t.Fatal("native MCP confirmation created an unrequested Orka approval")
	}
	// The canonical control-plane projection does not expose native display text.
	if strings.Contains(response.Body.String(), "not authority") {
		t.Fatal("display title leaked into permission authority")
	}
}
