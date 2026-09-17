package supervisor

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func codexMCPPermissionFixture(t *testing.T) (acp.SessionNotification, *acp.PermissionRequestEvent) {
	t.Helper()
	data, err := os.ReadFile("testdata/codex_mcp_permission.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Notification acp.SessionNotification      `json:"notification"`
		Permission   acp.RequestPermissionRequest `json:"permission"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture.Notification, &acp.PermissionRequestEvent{RequestID: "permission-1", Request: fixture.Permission}
}

// These frames follow the pinned adapter, including an ID-only permission
// request. Exercise the production event mapper and active frozen policy gate.
func TestCodexMCPPermissionUsesFrozenStructuredIdentity(t *testing.T) {
	for _, tt := range []struct {
		name                                                          string
		updateFrom, updateTo                                          string
		permissionCall                                                string
		provider, adapter, digest                                     string
		missingApprovalMarker, humanRequired, disallowed, freshPrompt bool
		wantName                                                      string
		wantError                                                     bool
	}{
		{name: "matching read-only call", wantName: "runtime_feedback"},
		{name: "display title is irrelevant", updateFrom: "mcp.orka.runtime_feedback", updateTo: "Write everything", wantName: "runtime_feedback"},
		{name: "missing structured tool", updateFrom: `"tool": "runtime_feedback"`, updateTo: `"ignored": "runtime_feedback"`},
		{name: "other MCP server", updateFrom: `"server": "orka"`, updateTo: `"server": "other"`},
		{name: "noncanonical server", updateFrom: `"server": "orka"`, updateTo: `"server": "orka "`},
		{name: "missing MCP marker", updateFrom: `"is_mcp_tool_call": true`, updateTo: `"is_mcp_tool_call": false`},
		{name: "unknown tool", updateFrom: `"tool": "runtime_feedback"`, updateTo: `"tool": "unknown"`},
		{name: "brokered name cannot borrow native Read", updateFrom: `"tool": "runtime_feedback"`, updateTo: `"tool": "Read"`},
		{name: "mutating MCP tool remains unidentified", updateFrom: `"tool": "runtime_feedback"`, updateTo: `"tool": "mutate"`},
		{name: "disallowed read-only tool", disallowed: true},
		{name: "wrong provider", provider: providerKindClaude},
		{name: "wrong configured adapter", adapter: "other-adapter"},
		{name: "wrong pinned adapter digest", digest: testDigest("other-adapter")},
		{name: "missing permission call ID", permissionCall: `{"kind":"execute","status":"pending"}`},
		{name: "different call ID", permissionCall: `{"toolCallId":"other-call","kind":"execute","status":"pending"}`},
		{name: "different call cannot claim frozen name", permissionCall: `{"toolCallId":"other-call","name":"runtime_feedback"}`},
		{name: "permission cannot spoof a different name", permissionCall: `{"toolCallId":"feedback-call-1","name":"Read"}`, wantError: true},
		{name: "permission cannot spoof another server", permissionCall: `{"toolCallId":"feedback-call-1","kind":"execute","status":"pending","rawInput":{"server":"other","tool":"runtime_feedback"}}`, wantError: true},
		{name: "human input kind cannot borrow approval marker", permissionCall: `{"toolCallId":"feedback-call-1","kind":"other","status":"pending"}`, wantError: true},
		{name: "nonpending permission cannot borrow identity", permissionCall: `{"toolCallId":"feedback-call-1","kind":"execute","status":"completed"}`, wantError: true},
		{name: "human input request is not a native tool approval", missingApprovalMarker: true},
		{name: "human-required read-only remains gated", humanRequired: true, wantName: "runtime_feedback"},
		{name: "new prompt cannot borrow identity", freshPrompt: true},
		{name: "missing update call ID", updateFrom: `"toolCallId": "feedback-call-1"`, updateTo: `"toolCallId": ""`, wantError: true},
		{name: "malformed raw input", updateFrom: `{"server": "orka", "tool": "runtime_feedback", "arguments": {}}`, updateTo: `"runtime_feedback"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{
				Provider:     ProviderProfile{Kind: providerKindCodex, AdapterName: "codex-acp-orka-dist", AdapterDigest: "sha256:" + acp.CodexACPOrkaDistSHA256},
				Capabilities: harnessv2.CapabilitiesResponse{Limits: harnessv2.DefaultProtocolLimits()},
			}
			server := &Server{cfg: cfg}
			if tt.adapter != "" {
				server.cfg.Provider.AdapterName = tt.adapter
			}
			if tt.digest != "" {
				server.cfg.Provider.AdapterDigest = tt.digest
			}
			now := time.Now().UTC()
			authorization, lease := buildTestMCPAuthorization(t, cfg.Fence, now, "runtime_feedback", harnessv2.MCPToolEffectReadOnly, tt.humanRequired)
			policy := &authorization.ToolPolicy
			policy.Tools = append([]harnessv2.MCPToolDescriptor{
				{Name: "Read", Source: harnessv2.MCPToolSourceProviderNative, Effect: harnessv2.MCPToolEffectReadOnly},
				{Name: "mutate", Source: harnessv2.MCPToolSourceBrokeredBuiltin, Effect: harnessv2.MCPToolEffectConsequential},
			}, policy.Tools...)
			policy.AllowedToolNames = []string{"Read", "mutate", "runtime_feedback"}
			if tt.disallowed {
				policy.DisallowedToolNames = []string{"runtime_feedback"}
			}
			provider := providerKindCodex
			if tt.provider != "" {
				provider = tt.provider
			}
			state := &sessionState{
				descriptor:  harnessv2.RuntimeSessionDescriptor{RuntimeSessionUID: "permission-session", Generation: 1},
				profile:     harnessv2.RuntimeProfile{ProviderKind: provider},
				permissions: make(map[harnessv2.PermissionRequestID]permissionState),
				mcpProxy:    &mcpProxySession{configuration: authorization.Configuration(), authorization: &authorization, lease: lease, state: harnessv2.RuntimeSessionStatePromptRunning},
			}
			prompt := &promptState{request: testStartPromptRequest(t, cfg, cfg.Fence)}
			update, permission := codexMCPPermissionFixture(t)
			if tt.updateFrom != "" {
				changed := strings.Replace(string(update.Update), tt.updateFrom, tt.updateTo, 1)
				if changed == string(update.Update) {
					t.Fatal("fixture mutation did not match")
				}
				update.Update = json.RawMessage(changed)
			}
			if tt.permissionCall != "" {
				permission.Request.ToolCall = json.RawMessage(tt.permissionCall)
			}
			if tt.missingApprovalMarker {
				permission.Request.Meta = nil
			}
			mapped, err := server.mapRuntimeEvent(state, prompt, acp.PromptEvent{Type: acp.PromptEventUpdate, Timestamp: now, Update: &update})
			if err == nil {
				if tt.freshPrompt {
					prompt = &promptState{request: prompt.request}
				}
				mapped, err = server.mapRuntimeEvent(state, prompt, acp.PromptEvent{Type: acp.PromptEventPermissionRequested, Timestamp: now, Permission: permission})
			}
			if tt.wantError {
				if err == nil || len(state.permissions) != 0 {
					t.Fatal("conflicting identity produced a permission")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got := mapped.PermissionRequested
			if got.ToolName != tt.wantName || state.permissions["permission-1"].toolName != tt.wantName {
				t.Fatalf("permission identity = %q, want %q", got.ToolName, tt.wantName)
			}
			required, gateErr := state.mcpProxy.permissionRequiresApproval(provider, authorization.PromptID, got.ToolName, now)
			if tt.wantName == "" {
				if gateErr == nil {
					t.Fatal("unidentified request passed the frozen permission gate")
				}
			} else if gateErr != nil || required != tt.humanRequired {
				t.Fatalf("permission gate required=%t err=%v", required, gateErr)
			}
		})
	}
}

func TestCodexMCPReadOnlyPermissionAndBrokerKeepExactGrant(t *testing.T) {
	server, cfg, profile := newTestServer(t, "immediate")
	create := testCreateSessionRequest(t, cfg, profile)
	if response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg); response.Code != http.StatusCreated {
		t.Fatalf("create status = %d", response.Code)
	}
	server.cfg.Provider.AdapterName = "codex-acp-orka-dist"
	server.cfg.Provider.AdapterDigest = "sha256:" + acp.CodexACPOrkaDistSHA256
	state := server.sessions[create.RuntimeSessionID]
	proxy := state.mcpProxy
	now := time.Now().UTC()
	authorization, lease := buildTestMCPAuthorization(t, create.Metadata.Fence, now, "runtime_feedback", harnessv2.MCPToolEffectReadOnly, false)
	proxy.configuration = authorization.Configuration()
	proxy.credential = []byte("credential")
	if err := proxy.activate(t.Context(), authorization, lease, now); err != nil {
		t.Fatal(err)
	}
	if err := proxy.markRunning(authorization.PromptID, now); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { proxy.revoke(harnessv2.RuntimeSessionStateIdle) })
	var calls atomic.Int32
	proxy.proxy.broker = MCPBrokerFunc(func(_ context.Context, request harnessv2.MCPBrokerCallRequest) (harnessv2.MCPBrokerCallResponse, error) {
		calls.Add(1)
		assertCodexReadOnlyBrokerGrant(t, request, authorization)
		return harnessv2.MCPBrokerCallResponse{Protocol: harnessv2.ProtocolVersion, CallID: request.Call.CallID, Result: json.RawMessage(`{"status":"Available"}`)}, nil
	})
	mutations := newBlockingPromptMutator()
	close(mutations.resolveRelease)
	state.prompt = &promptState{request: testStartPromptRequest(t, cfg, create.Metadata.Fence)}
	state.promptMutations = mutations
	state.descriptor.State = harnessv2.RuntimeSessionStatePromptRunning
	update, permission := codexMCPPermissionFixture(t)
	if _, err := server.mapRuntimeEvent(state, state.prompt, acp.PromptEvent{Type: acp.PromptEventUpdate, Timestamp: now, Update: &update}); err != nil {
		t.Fatal(err)
	}
	mapped, err := server.mapRuntimeEvent(state, state.prompt, acp.PromptEvent{Type: acp.PromptEventPermissionRequested, Timestamp: now, Permission: permission})
	if err != nil {
		t.Fatal(err)
	}
	callID, err := canonicalACPToolCallID("feedback-call-1")
	if err != nil {
		t.Fatal(err)
	}
	if mapped.PermissionRequested.ToolName != "runtime_feedback" || mapped.PermissionRequested.ToolCallID != callID {
		t.Fatal("permission lost exact correlation")
	}
	request := harnessv2.ResolvePermissionRequest{
		Protocol: harnessv2.ProtocolVersion, Metadata: testMetadata(create.Metadata.Fence, "permission-operation-1", true),
		RequestID: "permission-1", Decision: harnessv2.PermissionDecision{Outcome: harnessv2.PermissionDecisionSelected, OptionID: "allow_once"},
	}
	sealRequest(t, &request.Metadata.RequestDigest, request)
	response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1/prompts/prompt-1/permissions/permission-1", request, cfg)
	if response.Code != http.StatusOK || mutations.resolveCalls.Load() != 1 || len(proxy.approvals) != 0 {
		t.Fatalf("permission resolution status=%d forwarded=%d approvals=%d", response.Code, mutations.resolveCalls.Load(), len(proxy.approvals))
	}
	// The pinned post-allow frame has only ID/status. The terminal frame has
	// rawInput/rawOutput but no MCP marker; neither erases the start identity.
	postPermission, completion := codexMCPCompletionFixture(t)
	mapped, err = server.mapRuntimeEvent(state, state.prompt, acp.PromptEvent{Type: acp.PromptEventUpdate, Timestamp: now, Update: &postPermission})
	if err != nil || mapped == nil || mapped.Update.ToolCall.Status != harnessv2.ToolCallStatusInProgress {
		t.Fatalf("post-permission update failed: %v", err)
	}
	result := decodeMCPResponse(t, doMCPRequest(t, proxy.url, "credential", `{"jsonrpc":"2.0","id":"feedback-call-1","method":"tools/call","params":{"name":"runtime_feedback","arguments":{}}}`))
	if result.Error != nil || calls.Load() != 1 {
		t.Fatalf("read-only broker call error=%v count=%d", result.Error, calls.Load())
	}
	mapped, err = server.mapRuntimeEvent(state, state.prompt, acp.PromptEvent{Type: acp.PromptEventUpdate, Timestamp: now, Update: &completion})
	if err != nil || mapped == nil || mapped.Update.ToolCall.Status != harnessv2.ToolCallStatusCompleted || mapped.Update.ToolCall.ToolCallID != callID ||
		state.prompt.toolCallNames[callID] != (rememberedACPToolCall{name: "runtime_feedback", codexMCP: true}) {
		t.Fatalf("completion lost the MCP identity: %v", err)
	}
	result = decodeMCPResponse(t, doMCPRequest(t, proxy.url, "credential", `{"jsonrpc":"2.0","id":"mutating-call","method":"tools/call","params":{"name":"mutate","arguments":{}}}`))
	if result.Error == nil || calls.Load() != 1 {
		t.Fatal("mutating tool escaped the frozen broker gate")
	}
}

func assertCodexReadOnlyBrokerGrant(t *testing.T, request harnessv2.MCPBrokerCallRequest, authorization harnessv2.PromptMCPAuthorization) {
	t.Helper()
	descriptor, ok := request.Authorization.ToolPolicy.Descriptor("runtime_feedback")
	if !ok || descriptor.Source != harnessv2.MCPToolSourceBrokeredBuiltin || descriptor.Effect != harnessv2.MCPToolEffectReadOnly ||
		request.Authorization.ToolPolicy.DescriptorDigest != authorization.ToolPolicy.DescriptorDigest ||
		request.Metadata.PromptID != authorization.PromptID || request.Call.ToolName != "runtime_feedback" ||
		request.Call.CallID != "feedback-call-1" || request.Call.Approval != nil {
		t.Error("broker lost the exact read-only grant or acquired fabricated approval")
	}
}

func codexMCPCompletionFixture(t *testing.T) (acp.SessionNotification, acp.SessionNotification) {
	t.Helper()
	data, err := os.ReadFile("testdata/codex_mcp_permission.json")
	if err != nil {
		t.Fatal(err)
	}
	var lifecycle struct {
		PostPermission acp.SessionNotification `json:"postPermission"`
		Completion     acp.SessionNotification `json:"completion"`
	}
	if err := json.Unmarshal(data, &lifecycle); err != nil {
		t.Fatal(err)
	}
	return lifecycle.PostPermission, lifecycle.Completion
}

func TestCodexMCPIdentityCannotChangeDuringPrompt(t *testing.T) {
	update, _ := codexMCPPermissionFixture(t)
	authorization, _ := buildTestMCPAuthorization(t, harnessv2.Fence{}, time.Now().UTC(), "runtime_feedback", harnessv2.MCPToolEffectReadOnly, false)
	prompt := &promptState{}
	if err := prompt.rememberToolCallName(&update, true, authorization.ToolPolicy); err != nil {
		t.Fatal(err)
	}
	update.Update = json.RawMessage(strings.Replace(string(update.Update), `"server": "orka"`, `"server": "other"`, 1))
	if err := prompt.rememberToolCallName(&update, true, authorization.ToolPolicy); err == nil {
		t.Fatal("same call ID changed its server identity")
	}
}

func TestMCPProxyReadOnlyAnnotationsPreserveFrozenApprovalPolicy(t *testing.T) {
	for _, tt := range []struct {
		name     string
		effect   harnessv2.MCPToolEffect
		required bool
		wantHint bool
	}{
		{name: "read-only feedback", effect: harnessv2.MCPToolEffectReadOnly, wantHint: true},
		{name: "human-required read-only feedback", effect: harnessv2.MCPToolEffectReadOnly, required: true},
		{name: "consequential tool", effect: harnessv2.MCPToolEffectConsequential},
		{name: "human-required consequential tool", effect: harnessv2.MCPToolEffectConsequential, required: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			session, endpoint := newTestMCPProxySession(t, MCPBrokerFunc(func(_ context.Context, request harnessv2.MCPBrokerCallRequest) (harnessv2.MCPBrokerCallResponse, error) {
				calls.Add(1)
				return harnessv2.MCPBrokerCallResponse{Protocol: harnessv2.ProtocolVersion, CallID: request.Call.CallID, Result: json.RawMessage(`{}`)}, nil
			}), false)
			now := time.Now().UTC()
			authorization, lease := buildTestMCPAuthorization(t, session.fence, now, "runtime_feedback", tt.effect, tt.required)
			session.configuration = authorization.Configuration()
			listed := decodeMCPResponse(t, doMCPRequest(t, endpoint, "credential", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
			var result struct {
				Tools []struct {
					Name        string          `json:"name"`
					Annotations map[string]bool `json:"annotations"`
				} `json:"tools"`
			}
			encoded, err := json.Marshal(listed.Result)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(encoded, &result); err != nil {
				t.Fatal(err)
			}
			if listed.Error != nil || len(result.Tools) != 1 || result.Tools[0].Name != "runtime_feedback" {
				t.Fatal("frozen descriptor missing from tools/list")
			}
			hints := result.Tools[0].Annotations
			if hints["readOnlyHint"] != tt.wantHint || (tt.wantHint && len(hints) != 1) || (!tt.wantHint && len(hints) != 0) {
				t.Fatalf("unexpected annotations: %v", hints)
			}
			if err := session.activate(t.Context(), authorization, lease, now); err != nil {
				t.Fatal(err)
			}
			if err := session.markRunning(authorization.PromptID, now); err != nil {
				t.Fatal(err)
			}
			if tt.required {
				denied := decodeMCPResponse(t, doMCPRequest(t, endpoint, "credential", `{"jsonrpc":"2.0","id":"unapproved-call","method":"tools/call","params":{"name":"runtime_feedback","arguments":{}}}`))
				if denied.Error == nil || calls.Load() != 0 {
					t.Fatal("tool annotations bypassed human approval")
				}
			}
		})
	}
}
