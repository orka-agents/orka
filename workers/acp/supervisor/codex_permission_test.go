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

const (
	codexDelegateInput  = `{"server":"orka","tool":"delegate_task","arguments":{"task":"inspect the repository"}}`
	codexDelegateUpdate = `{"sessionUpdate":"tool_call","toolCallId":"call/1","kind":"execute",` +
		`"title":"mcp.orka.delegate_task","status":"pending","rawInput":` + codexDelegateInput + `,"_meta":{"is_mcp_tool_call":true}}`
	codexIDOnlyPermission = `{"toolCallId":"call/1","kind":"execute","status":"pending"}`
)

func TestCodexPermissionUsesStructuredMCPToolIdentity(t *testing.T) {
	for _, tt := range []struct {
		name        string
		updates     []string
		provider    string
		permission  string
		freshPrompt bool
		wantName    string
	}{
		{name: "real Codex MCP update and ID-only permission", updates: []string{codexDelegateUpdate}, wantName: "delegate_task"},
		{
			name:     "misleading display title is irrelevant",
			updates:  []string{strings.Replace(codexDelegateUpdate, "mcp.orka.delegate_task", "mcp.other.Read", 1)},
			wantName: "delegate_task",
		},
		{
			name:     "matching explicit identity",
			updates:  []string{strings.Replace(codexDelegateUpdate, `"kind":"execute"`, `"kind":"execute","name":"delegate_task"`, 1)},
			wantName: "delegate_task",
		},
		{
			name: "ID-only follow-up updates retain identity",
			updates: []string{
				codexDelegateUpdate,
				`{"sessionUpdate":"tool_call_update","toolCallId":"call/1"}`,
				`{"sessionUpdate":"tool_call_update","toolCallId":"call/1","status":"in_progress"}`,
			},
			wantName: "delegate_task",
		},
		{name: "no preceding update"},
		{name: "permission title cannot grant", permission: `{"toolCallId":"call/1","title":"mcp.orka.delegate_task"}`},
		{name: "update title cannot grant", updates: []string{`{"sessionUpdate":"tool_call","toolCallId":"call/1","kind":"execute","title":"mcp.orka.delegate_task"}`}},
		{name: "missing MCP marker", updates: []string{strings.Replace(codexDelegateUpdate, `,"_meta":{"is_mcp_tool_call":true}`, "", 1)}},
		{name: "false MCP marker", updates: []string{strings.Replace(codexDelegateUpdate, `"is_mcp_tool_call":true`, `"is_mcp_tool_call":false`, 1)}},
		{name: "Claude does not infer Codex identity", provider: providerKindClaude, updates: []string{codexDelegateUpdate}},
		{name: "Copilot does not infer Codex identity", provider: providerKindCopilot, updates: []string{codexDelegateUpdate}},
		{name: "OpenCode does not infer Codex identity", provider: providerKindOpencode, updates: []string{codexDelegateUpdate}},
		{name: "different call cannot borrow identity", updates: []string{codexDelegateUpdate}, permission: `{"toolCallId":"call/2","kind":"execute","status":"pending"}`},
		{name: "fresh prompt cannot borrow identity", updates: []string{codexDelegateUpdate}, freshPrompt: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server, state, prompt := newCodexPermissionState(t)
			if tt.provider != "" {
				state.profile.ProviderKind = tt.provider
			}
			for _, update := range tt.updates {
				if _, err := server.mapRuntimeEvent(state, prompt, codexToolUpdateEvent(update)); err != nil {
					t.Fatal(err)
				}
			}
			if tt.freshPrompt {
				prompt = &promptState{request: prompt.request}
				prompt.request.Metadata.PromptID = "prompt-2"
			}
			permission := tt.permission
			if permission == "" {
				permission = codexIDOnlyPermission
			}
			mapped, err := server.mapRuntimeEvent(state, prompt, codexPermissionEvent(permission))
			if err != nil {
				t.Fatal(err)
			}
			if mapped == nil || mapped.Type != harnessv2.EventPermissionRequested || mapped.PermissionRequested == nil {
				t.Fatalf("missing permission event: %+v", mapped)
			}
			pending, ok := state.permissions["permission-1"]
			if !ok || mapped.PermissionRequested.ToolName != tt.wantName || pending.toolName != tt.wantName {
				t.Fatalf("permission identity = %q, pending = %q (exists %t), want %q", mapped.PermissionRequested.ToolName, pending.toolName, ok, tt.wantName)
			}
			if pending.toolCallID == "" || pending.toolCallID != mapped.PermissionRequested.ToolCallID {
				t.Fatalf("pending and emitted tool call IDs differ: %q != %q", pending.toolCallID, mapped.PermissionRequested.ToolCallID)
			}
		})
	}
}

func TestCodexPermissionRejectsUntrustedMCPIdentity(t *testing.T) {
	for _, tt := range []struct {
		name          string
		input         string
		update        string
		permission    string
		rememberFirst bool
		policy        func(*harnessv2.MCPToolPolicy)
	}{
		{name: "foreign server", input: `{"server":"other","tool":"delegate_task"}`},
		{name: "server prefix is not Orka", input: `{"server":"orka.foo","tool":"delegate_task"}`},
		{name: "server is exact", input: `{"server":"orka ","tool":"delegate_task"}`},
		{name: "missing raw input", update: strings.Replace(codexDelegateUpdate, `,"rawInput":`+codexDelegateInput, "", 1)},
		{name: "null raw input", input: `null`},
		{name: "array raw input", input: `[]`},
		{name: "string raw input", input: `"delegate_task"`},
		{name: "missing server", input: `{"tool":"delegate_task"}`},
		{name: "non-string server", input: `{"server":1,"tool":"delegate_task"}`},
		{name: "missing tool", input: `{"server":"orka"}`},
		{name: "non-string tool", input: `{"server":"orka","tool":{}}`},
		{name: "noncanonical tool", input: `{"server":"orka","tool":"delegate_task "}`},
		{name: "unknown tool", input: `{"server":"orka","tool":"unknown_tool"}`},
		{name: "native tool cannot borrow broker identity", input: `{"server":"orka","tool":"Read"}`},
		{name: "nonexecute kind", update: strings.Replace(codexDelegateUpdate, `"kind":"execute"`, `"kind":"read"`, 1)},
		{name: "missing kind", update: strings.Replace(codexDelegateUpdate, `"kind":"execute",`, "", 1)},
		{name: "malformed MCP marker", update: strings.Replace(codexDelegateUpdate, `"is_mcp_tool_call":true`, `"is_mcp_tool_call":"true"`, 1)},
		{name: "conflicting explicit name", update: strings.Replace(codexDelegateUpdate, `"kind":"execute"`, `"kind":"execute","name":"Read"`, 1)},
		{name: "conflicting metadata name", update: strings.Replace(codexDelegateUpdate, `"is_mcp_tool_call":true`, `"is_mcp_tool_call":true,"claudeCode":{"toolName":"Read"}`, 1)},
		{
			name:          "later MCP update cannot change identity",
			rememberFirst: true,
			update:        strings.Replace(codexDelegateUpdate, `"sessionUpdate":"tool_call"`, `"sessionUpdate":"tool_call_update"`, 1),
			input:         `{"server":"orka","tool":"run_validation"}`,
		},
		{
			name:          "later explicit update cannot change identity",
			rememberFirst: true,
			update:        `{"sessionUpdate":"tool_call_update","toolCallId":"call/1","name":"Read"}`,
		},
		{
			name:       "permission cannot change remembered identity",
			permission: `{"toolCallId":"call/1","name":"Read","kind":"execute","status":"pending"}`,
		},
		{
			name: "native-only descriptor with matching name",
			policy: func(policy *harnessv2.MCPToolPolicy) {
				policy.Tools[1].Source = harnessv2.MCPToolSourceProviderNative
			},
		},
		{
			name: "disallowed broker tool",
			policy: func(policy *harnessv2.MCPToolPolicy) {
				policy.DisallowedToolNames = append(policy.DisallowedToolNames, "delegate_task")
			},
		},
		{
			name: "descriptor outside allowlist",
			policy: func(policy *harnessv2.MCPToolPolicy) {
				policy.AllowedToolNames = []string{"Read", "run_validation"}
			},
		},
		{
			name: "allowed name without descriptor",
			policy: func(policy *harnessv2.MCPToolPolicy) {
				policy.Tools = append(policy.Tools[:1], policy.Tools[2:]...)
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server, state, prompt := newCodexPermissionState(t)
			if tt.policy != nil {
				tt.policy(&state.mcpProxy.configuration.ToolPolicy)
			}
			if tt.rememberFirst {
				if _, err := server.mapRuntimeEvent(state, prompt, codexToolUpdateEvent(codexDelegateUpdate)); err != nil {
					t.Fatal(err)
				}
			}
			update := tt.update
			if update == "" {
				update = codexDelegateUpdate
			}
			if tt.input != "" {
				update = strings.Replace(update, codexDelegateInput, tt.input, 1)
			}
			_, err := server.mapRuntimeEvent(state, prompt, codexToolUpdateEvent(update))
			if err != nil {
				assertCodexPermissionUngranted(t, state, nil, err)
				return
			}
			permission := tt.permission
			if permission == "" {
				permission = codexIDOnlyPermission
			}
			mapped, err := server.mapRuntimeEvent(state, prompt, codexPermissionEvent(permission))
			assertCodexPermissionUngranted(t, state, mapped, err)
		})
	}
}

func TestCodexPermissionResolveBrokeredCall(t *testing.T) {
	for _, tt := range []struct {
		name        string
		option      string
		titleOnly   bool
		revoked     bool
		wantStatus  int
		wantResolve int32
	}{
		{name: "allow once with native tools restricted", option: "allow", wantStatus: http.StatusOK, wantResolve: 1},
		{name: "reusable approval denied", option: "always", wantStatus: http.StatusForbidden},
		{name: "title alone cannot be approved", option: "allow", titleOnly: true, wantStatus: http.StatusForbidden},
		{name: "remembered identity cannot bypass revocation", option: "allow", revoked: true, wantStatus: http.StatusForbidden},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server, cfg, profile := newTestServer(t, "immediate")
			create := testCreateSessionRequest(t, cfg, profile)
			created := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg)
			if created.Code != http.StatusCreated {
				t.Fatalf("create status = %d: %s", created.Code, created.Body.String())
			}
			now := time.Now().UTC()
			authorization, lease := buildTestMCPAuthorization(t, create.Metadata.Fence, now, "delegate_task", harnessv2.MCPToolEffectConsequential, true)
			authorization.ToolPolicy.AllowBash = false
			authorization.ToolPolicy.DisallowedToolNames = []string{providerToolBash}
			var err error
			authorization.ToolPolicyDigest, err = harnessv2.CanonicalRuntimeToolPolicyDigest(
				authorization.ToolPolicy.AllowedToolNames, authorization.ToolPolicy.DisallowedToolNames, authorization.ToolPolicy.AllowBash,
			)
			if err != nil {
				t.Fatal(err)
			}
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

			permission := codexIDOnlyPermission
			if tt.titleOnly {
				permission = `{"toolCallId":"call/1","title":"mcp.orka.delegate_task"}`
			} else if _, err := server.mapRuntimeEvent(state, state.prompt, codexToolUpdateEvent(codexDelegateUpdate)); err != nil {
				t.Fatal(err)
			}
			if _, err := server.mapRuntimeEvent(state, state.prompt, codexPermissionEvent(permission)); err != nil {
				t.Fatal(err)
			}
			if tt.revoked {
				state.mcpProxy.revoke(harnessv2.RuntimeSessionStateCancelling)
			}
			request := harnessv2.ResolvePermissionRequest{
				Protocol:  harnessv2.ProtocolVersion,
				Metadata:  testMetadata(create.Metadata.Fence, "permission-operation-1", true),
				RequestID: "permission-1",
				Decision:  harnessv2.PermissionDecision{Outcome: harnessv2.PermissionDecisionSelected, OptionID: tt.option},
			}
			sealRequest(t, &request.Metadata.RequestDigest, request)
			response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1/prompts/prompt-1/permissions/permission-1", request, cfg)
			if response.Code != tt.wantStatus {
				t.Fatalf("resolve status = %d, want %d: %s", response.Code, tt.wantStatus, response.Body.String())
			}
			if got := mutations.resolveCalls.Load(); got != tt.wantResolve {
				t.Fatalf("forwarded permissions = %d, want %d", got, tt.wantResolve)
			}
		})
	}
}

func newCodexPermissionState(t *testing.T) (*Server, *sessionState, *promptState) {
	t.Helper()
	server, cfg, _ := newTestServer(t, "immediate")
	fence := cfg.Fence
	fence.RuntimeSessionUID = "permission-session"
	fence.RuntimeSessionGeneration = 1
	state := &sessionState{
		descriptor:  harnessv2.RuntimeSessionDescriptor{RuntimeSessionUID: fence.RuntimeSessionUID, Generation: 1},
		profile:     harnessv2.RuntimeProfile{ProviderKind: providerKindCodex},
		permissions: make(map[harnessv2.PermissionRequestID]permissionState),
		mcpProxy: &mcpProxySession{configuration: harnessv2.MCPPolicyConfiguration{ToolPolicy: harnessv2.MCPToolPolicy{
			AllowedToolNames:    []string{"Read", "delegate_task", "run_validation"},
			DisallowedToolNames: []string{providerToolBash},
			Tools: []harnessv2.MCPToolDescriptor{
				{Name: "Read", Source: harnessv2.MCPToolSourceProviderNative},
				{Name: "delegate_task", Source: harnessv2.MCPToolSourceBrokeredBuiltin},
				{Name: "run_validation", Source: harnessv2.MCPToolSourceBrokeredBuiltin},
			},
		}}},
	}
	return server, state, &promptState{request: testStartPromptRequest(t, cfg, fence)}
}

func codexToolUpdateEvent(raw string) acp.PromptEvent {
	return acp.PromptEvent{
		Type: acp.PromptEventUpdate, Timestamp: time.Now().UTC(),
		Update: &acp.SessionNotification{Update: json.RawMessage(raw)},
	}
}

func codexPermissionEvent(raw string) acp.PromptEvent {
	return acp.PromptEvent{
		Type: acp.PromptEventPermissionRequested, Timestamp: time.Now().UTC(),
		Permission: &acp.PermissionRequestEvent{
			RequestID: "permission-1",
			Request: acp.RequestPermissionRequest{
				ToolCall: json.RawMessage(raw),
				Options: []acp.PermissionOption{
					{OptionID: "allow", Name: "Allow once", Kind: string(harnessv2.PermissionOptionAllowOnce)},
					{OptionID: "always", Name: "Allow always", Kind: string(harnessv2.PermissionOptionAllowAlways)},
				},
			},
		},
	}
}

func assertCodexPermissionUngranted(t *testing.T, state *sessionState, mapped *harnessv2.Event, err error) {
	t.Helper()
	if err != nil {
		if len(state.permissions) != 0 {
			t.Fatalf("rejected identity left pending permissions: %v", err)
		}
		return
	}
	if mapped == nil || mapped.PermissionRequested == nil {
		t.Fatalf("missing permission event: %+v", mapped)
	}
	pending, ok := state.permissions["permission-1"]
	if !ok || pending.toolName != mapped.PermissionRequested.ToolName {
		t.Fatalf("pending identity does not match emitted permission: %+v", mapped.PermissionRequested)
	}
	// This fixture disables implicit native grants, so every grant needs a descriptor.
	if _, allowed := state.mcpProxy.configuration.ToolPolicy.Descriptor(pending.toolName); allowed {
		t.Fatalf("untrusted identity queued an authorized permission for %q", pending.toolName)
	}
}
