package supervisor

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func TestClaudePermissionUsesStructuredToolIdentity(t *testing.T) {
	for _, tt := range []struct {
		name        string
		update      string
		permission  string
		freshPrompt bool
		wantName    string
		wantError   bool
	}{
		{
			name:       "write identity from preceding Claude update",
			update:     `{"sessionUpdate":"tool_call","toolCallId":"call/1","title":"Write arithmetic.py","_meta":{"claudeCode":{"toolName":"Write"}}}`,
			permission: `{"toolCallId":"call/1","title":"Write arithmetic.py"}`,
			wantName:   "Write",
		},
		{
			name:       "validation identity from preceding Claude update",
			update:     `{"sessionUpdate":"tool_call","toolCallId":"call/1","title":"Run validation","_meta":{"claudeCode":{"toolName":"mcp__orka__run_validation"}}}`,
			permission: `{"toolCallId":"call/1","title":"Run validation"}`,
			wantName:   "run_validation",
		},
		{
			name:       "identity carried by permission metadata",
			permission: `{"toolCallId":"call/1","title":"Write arithmetic.py","_meta":{"claudeCode":{"toolName":"Write"}}}`,
			wantName:   "Write",
		},
		{
			name:       "display title does not grant a tool",
			permission: `{"toolCallId":"call/1","title":"Write"}`,
		},
		{
			name:       "conflicting structured identities",
			permission: `{"toolCallId":"call/1","name":"Read","_meta":{"claudeCode":{"toolName":"Write"}}}`,
			wantError:  true,
		},
		{
			name:       "permission cannot change the recorded tool",
			update:     `{"sessionUpdate":"tool_call","toolCallId":"call/1","_meta":{"claudeCode":{"toolName":"Read"}}}`,
			permission: `{"toolCallId":"call/1","name":"Write"}`,
			wantError:  true,
		},
		{
			name:       "other MCP server cannot borrow Orka grant",
			permission: `{"toolCallId":"call/1","name":"mcp__other__run_validation"}`,
			wantName:   "mcp__other__run_validation",
		},
		{
			name:       "brokered alias cannot borrow native grant",
			permission: `{"toolCallId":"call/1","name":"mcp__orka__Write"}`,
			wantName:   "mcp__orka__Write",
		},
		{
			name:        "a new prompt cannot borrow an earlier tool identity",
			update:      `{"sessionUpdate":"tool_call","toolCallId":"call/1","_meta":{"claudeCode":{"toolName":"Write"}}}`,
			permission:  `{"toolCallId":"call/1","title":"Write"}`,
			freshPrompt: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server, cfg, _ := newTestServer(t, "immediate")
			fence := cfg.Fence
			fence.RuntimeSessionUID = "permission-session"
			fence.RuntimeSessionGeneration = 1
			state := &sessionState{
				descriptor:  harnessv2.RuntimeSessionDescriptor{RuntimeSessionUID: fence.RuntimeSessionUID, Generation: 1},
				profile:     harnessv2.RuntimeProfile{ProviderKind: providerKindClaude},
				permissions: make(map[harnessv2.PermissionRequestID]permissionState),
				mcpProxy: &mcpProxySession{configuration: harnessv2.MCPPolicyConfiguration{ToolPolicy: harnessv2.MCPToolPolicy{
					AllowedToolNames: []string{"Write", "run_validation"},
					Tools: []harnessv2.MCPToolDescriptor{
						{Name: "Write", Source: harnessv2.MCPToolSourceProviderNative},
						{Name: "run_validation", Source: harnessv2.MCPToolSourceBrokeredBuiltin},
					},
				}}},
			}
			prompt := &promptState{request: testStartPromptRequest(t, cfg, fence)}
			if tt.update != "" {
				_, err := server.mapRuntimeEvent(state, prompt, acp.PromptEvent{
					Type: acp.PromptEventUpdate, Timestamp: time.Now().UTC(),
					Update: &acp.SessionNotification{Update: json.RawMessage(tt.update)},
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			if tt.freshPrompt {
				prompt = &promptState{request: prompt.request}
			}
			mapped, err := server.mapRuntimeEvent(state, prompt, acp.PromptEvent{
				Type: acp.PromptEventPermissionRequested, Timestamp: time.Now().UTC(),
				Permission: &acp.PermissionRequestEvent{
					RequestID: "permission-1",
					Request: acp.RequestPermissionRequest{
						ToolCall: json.RawMessage(tt.permission),
						Options:  []acp.PermissionOption{{OptionID: "allow", Name: "Allow", Kind: string(harnessv2.PermissionOptionAllowOnce)}},
					},
				},
			})
			if tt.wantError {
				if err == nil || len(state.permissions) != 0 {
					t.Fatalf("conflicting identity produced a pending permission: error=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if mapped.PermissionRequested.ToolName != tt.wantName || state.permissions["permission-1"].toolName != tt.wantName {
				t.Fatalf("permission tool identity = %q, want %q", mapped.PermissionRequested.ToolName, tt.wantName)
			}
		})
	}
}

func TestPermissionResolutionUsesFrozenToolAuthority(t *testing.T) {
	for _, tt := range []struct {
		name       string
		tool       string
		native     bool
		implicit   bool
		denyAll    bool
		required   bool
		expired    bool
		wrongName  bool
		always     bool
		wantStatus int
	}{
		{name: "native write", tool: "Write", native: true, wantStatus: http.StatusOK},
		{name: "implicit native write", tool: "Write", native: true, implicit: true, wantStatus: http.StatusOK},
		{name: "explicit native deny all", tool: "Write", native: true, denyAll: true, wantStatus: http.StatusForbidden},
		{name: "brokered validation", tool: "run_validation", wantStatus: http.StatusOK},
		{name: "brokered approval", tool: "mutate", required: true, wantStatus: http.StatusOK},
		{name: "expired prompt authority", tool: "Write", native: true, expired: true, wantStatus: http.StatusForbidden},
		{name: "tool outside frozen grant", tool: "Write", native: true, wrongName: true, wantStatus: http.StatusForbidden},
		{name: "native allow always is not a one shot grant", tool: "Write", native: true, always: true, wantStatus: http.StatusForbidden},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server, cfg, profile := newTestServer(t, "immediate")
			create := testCreateSessionRequest(t, cfg, profile)
			created := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg)
			if created.Code != http.StatusCreated {
				t.Fatalf("session creation status = %d", created.Code)
			}
			now := time.Now().UTC()
			authorization, lease := buildTestMCPAuthorization(t, create.Metadata.Fence, now, tt.tool, harnessv2.MCPToolEffectConsequential, tt.required)
			if tt.native {
				authorization.ToolPolicy.Tools[0].Source = harnessv2.MCPToolSourceProviderNative
				authorization.ToolPolicy.Tools[0].InputSchema = nil
			}
			if tt.implicit || tt.denyAll {
				authorization.ToolPolicy.AllowedToolNames = nil
				authorization.ToolPolicy.Tools = nil
				if tt.denyAll {
					authorization.ToolPolicy.AllowedToolNames = []string{}
				}
			}
			if tt.expired {
				lease.ExpiresAt = now.Add(-time.Second)
			}
			mutations := newBlockingPromptMutator()
			close(mutations.resolveRelease)
			optionKind := harnessv2.PermissionOptionAllowOnce
			if tt.always {
				optionKind = harnessv2.PermissionOptionAllowAlways
			}
			name := tt.tool
			if tt.wrongName {
				name = "ungranted_tool"
			}
			server.mu.Lock()
			state := server.sessions[create.RuntimeSessionID]
			state.profile.ProviderKind = providerKindClaude
			state.prompt = &promptState{request: testStartPromptRequest(t, cfg, create.Metadata.Fence)}
			state.descriptor.State = harnessv2.RuntimeSessionStatePromptRunning
			state.promptMutations = mutations
			state.permissions["permission-1"] = permissionState{
				requestID: "permission-1", toolCallID: "call-1", toolName: name,
				requestedAt: now, expiresAt: now.Add(time.Minute),
				options: map[string]harnessv2.PermissionOptionKind{"allow": optionKind},
			}
			state.mcpProxy.mu.Lock()
			state.mcpProxy.authorization = &authorization
			state.mcpProxy.lease = lease
			state.mcpProxy.state = harnessv2.RuntimeSessionStatePromptRunning
			state.mcpProxy.mu.Unlock()
			server.mu.Unlock()
			request := harnessv2.ResolvePermissionRequest{
				Protocol:  harnessv2.ProtocolVersion,
				Metadata:  testMetadata(create.Metadata.Fence, "permission-operation-1", true),
				RequestID: "permission-1",
				Decision:  harnessv2.PermissionDecision{Outcome: harnessv2.PermissionDecisionSelected, OptionID: "allow"},
			}
			sealRequest(t, &request.Metadata.RequestDigest, request)
			response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1/prompts/prompt-1/permissions/permission-1", request, cfg)
			if response.Code != tt.wantStatus {
				t.Fatalf("permission response = %d, want %d: %s", response.Code, tt.wantStatus, response.Body.String())
			}
			wantCalls := int32(0)
			if tt.wantStatus == http.StatusOK {
				wantCalls = 1
			}
			if got := mutations.resolveCalls.Load(); got != wantCalls {
				t.Fatalf("forwarded permissions = %d, want %d", got, wantCalls)
			}
			state.mcpProxy.mu.Lock()
			approvalCount := len(state.mcpProxy.approvals)
			state.mcpProxy.mu.Unlock()
			if tt.required != (approvalCount == 1) {
				t.Fatalf("MCP approval records = %d, approval required = %t", approvalCount, tt.required)
			}
		})
	}
}
