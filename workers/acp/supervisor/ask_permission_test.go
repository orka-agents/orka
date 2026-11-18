package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

// The permission tool calls below were recorded from the pinned runtimes in
// sessions launched to ask. None carries a structured tool name.
const (
	copilotAskShell = `{"toolCallId":"call_resp_3","title":"Create empty file","kind":"execute","status":"pending","rawInput":{"command":"touch created.txt","commands":["touch created.txt"]}}`
	copilotAskWrite = `{"toolCallId":"call_resp_5","title":"Create file","kind":"edit","status":"pending","rawInput":{"fileName":"/workspace/created.txt","diff":"--- a/dev/null\n+++ b/created.txt\n@@ -0,0 +1 @@\n+hi\n"},"locations":[{"path":"/workspace/created.txt"}]}`
	copilotAskPath  = `{"toolCallId":"call_resp_7","title":"Access paths outside trusted directories","kind":"read","status":"pending","rawInput":{"path":"/etc/hosts"},"locations":[{"path":"/etc/hosts"}]}`
	copilotAskFetch = `{"toolCallId":"call_resp_8","title":"Fetch content from this URL.","kind":"fetch","status":"pending","rawInput":{"url":"https://example.com"}}`
	codexAskCommand = `{"toolCallId":"call_2","title":"Run command","rawInput":{"command":"touch created.txt","cwd":"/workspace"},"kind":"execute","status":"pending"}`
	codexAskPatch   = `{"toolCallId":"call_3","title":"Edit files","locations":[{"path":"/workspace/added.txt"}],"kind":"edit","status":"pending"}`
	codexAskNetwork = `{"toolCallId":"call_4","title":"https network access to example.com","rawInput":{"command":"curl https://example.com","cwd":"/workspace","url":"https://example.com"},"kind":"execute","status":"pending"}`
	codexAskMCP     = `{"toolCallId":"call_5","kind":"execute","status":"pending"}`
	openCodeAskBash = `{"toolCallId":"call_bash_ls","title":"ls","kind":"execute","status":"pending","locations":[],"rawInput":{"command":"ls"}}`
	openCodeAskEdit = `{"toolCallId":"call_write","title":"/workspace/notes.txt","kind":"edit","status":"pending","locations":[{"path":"/workspace/notes.txt"}],"rawInput":{"filepath":"/workspace/notes.txt","diff":"--- a\n+++ b\n"}}`
)

func askConfiguration(t *testing.T, provider string, allowed []string, allowBash bool) harnessv2.MCPPolicyConfiguration {
	t.Helper()
	configuration := testProviderProjectionRequest(t, provider, "model", "", "", allowed, nil, allowBash).MCPConfiguration
	configuration.NativePermissions = harnessv2.NativePermissionsAsk
	return configuration
}

func openCodeAskConfiguration(allowed []string, allowBash bool) harnessv2.MCPPolicyConfiguration {
	return harnessv2.MCPPolicyConfiguration{
		ToolPolicy:        harnessv2.MCPToolPolicy{AllowedToolNames: allowed, AllowBash: allowBash},
		NativePermissions: harnessv2.NativePermissionsAsk,
	}
}

func TestAskPermissionToolNameUsesOnlyStructuredFields(t *testing.T) {
	unrestrictedCopilot := askConfiguration(t, providerKindCopilot, nil, true)
	writeOnlyCopilot := askConfiguration(t, providerKindCopilot, []string{providerToolRead, providerToolWrite}, false)
	readOnlyCopilot := askConfiguration(t, providerKindCopilot, []string{providerToolRead}, false)
	unrestrictedCodex := askConfiguration(t, providerKindCodex, nil, true)
	readOnlyCodex := askConfiguration(t, providerKindCodex, []string{providerToolGlob, providerToolGrep, providerToolRead}, false)
	openCode := openCodeAskConfiguration([]string{"apply_patch", "bash", "edit", "glob", "grep", "read", "write"}, true)
	openCodeWriteOnly := openCodeAskConfiguration([]string{"read", "write"}, false)
	preapproved := unrestrictedCopilot
	preapproved.NativePermissions = harnessv2.NativePermissionsPreapproved

	for _, test := range []struct {
		name          string
		provider      string
		configuration harnessv2.MCPPolicyConfiguration
		toolCall      string
		want          string
	}{
		{name: "copilot write with edit granted", provider: providerKindCopilot, configuration: unrestrictedCopilot, toolCall: copilotAskWrite, want: providerToolEdit},
		{name: "copilot write with only create kept", provider: providerKindCopilot, configuration: writeOnlyCopilot, toolCall: copilotAskWrite, want: providerToolWrite},
		{name: "copilot write with no write tool kept", provider: providerKindCopilot, configuration: readOnlyCopilot, toolCall: copilotAskWrite},
		{name: "copilot outside path read", provider: providerKindCopilot, configuration: unrestrictedCopilot, toolCall: copilotAskPath},
		{name: "copilot fetch", provider: providerKindCopilot, configuration: unrestrictedCopilot, toolCall: copilotAskFetch},
		{name: "copilot write with an extra key", provider: providerKindCopilot, configuration: unrestrictedCopilot,
			toolCall: `{"kind":"edit","rawInput":{"fileName":"/workspace/a","diff":"","command":"rm -rf /"}}`},
		{name: "copilot title alone", provider: providerKindCopilot, configuration: unrestrictedCopilot, toolCall: `{"kind":"edit","title":"Edit"}`},
		{name: "pre-approved session is never classified", provider: providerKindCopilot, configuration: preapproved, toolCall: copilotAskWrite},
		{name: "named request is left to its name", provider: providerKindCopilot, configuration: unrestrictedCopilot,
			toolCall: `{"name":"create","kind":"edit","rawInput":{"fileName":"/workspace/a","diff":""}}`},
		{name: "request with metadata", provider: providerKindCopilot, configuration: unrestrictedCopilot,
			toolCall: `{"kind":"edit","rawInput":{"fileName":"/workspace/a","diff":""},"_meta":{"tool":"Edit"}}`},
		{name: "codex command", provider: providerKindCodex, configuration: unrestrictedCodex, toolCall: codexAskCommand, want: providerToolBash},
		{name: "codex command without cwd", provider: providerKindCodex, configuration: unrestrictedCodex,
			toolCall: `{"kind":"execute","rawInput":{"command":"ls"}}`, want: providerToolBash},
		{name: "codex file change", provider: providerKindCodex, configuration: unrestrictedCodex, toolCall: codexAskPatch, want: providerToolEdit},
		{name: "codex network request", provider: providerKindCodex, configuration: unrestrictedCodex, toolCall: codexAskNetwork},
		{name: "codex MCP approval", provider: providerKindCodex, configuration: unrestrictedCodex, toolCall: codexAskMCP},
		{name: "codex additional permissions", provider: providerKindCodex, configuration: unrestrictedCodex,
			toolCall: `{"kind":"execute","rawInput":{"command":"ls","cwd":"/workspace","additionalPermissions":{}}}`},
		{name: "codex file change without files", provider: providerKindCodex, configuration: unrestrictedCodex, toolCall: `{"kind":"edit"}`},
		{name: "codex read-only session launches pre-approved", provider: providerKindCodex, configuration: readOnlyCodex, toolCall: codexAskCommand},
		{name: "opencode bash", provider: providerKindOpencode, configuration: openCode, toolCall: openCodeAskBash, want: openCodeToolBash},
		{name: "opencode edit group", provider: providerKindOpencode, configuration: openCode, toolCall: openCodeAskEdit, want: openCodeToolEdit},
		{name: "opencode edit group through write", provider: providerKindOpencode, configuration: openCodeWriteOnly, toolCall: openCodeAskEdit, want: openCodeToolWrite},
		{name: "opencode bash not granted", provider: providerKindOpencode, configuration: openCodeWriteOnly, toolCall: openCodeAskBash},
		{name: "opencode empty command", provider: providerKindOpencode, configuration: openCode, toolCall: `{"kind":"execute","rawInput":{"command":" "}}`},
		{name: "claude requests carry names", provider: providerKindClaude, configuration: askConfiguration(t, providerKindClaude, nil, true), toolCall: codexAskCommand},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := askPermissionToolName(test.provider, test.configuration, json.RawMessage(test.toolCall)); got != test.want {
				t.Fatalf("askPermissionToolName() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestAskSessionNamesPermissionRequestsBeforeThePolicyDecides(t *testing.T) {
	for _, test := range []struct {
		name string
		mode harnessv2.NativePermissionMode
		want string
	}{
		{name: "ask session", mode: harnessv2.NativePermissionsAsk, want: providerToolEdit},
		{name: "pre-approved session", mode: harnessv2.NativePermissionsPreapproved},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, state, prompt := newLiveTestPrompt(t)
			configuration := askConfiguration(t, providerKindCopilot, nil, true)
			configuration.NativePermissions = test.mode
			state.profile.ProviderKind = providerKindCopilot
			state.mcpProxy = &mcpProxySession{configuration: configuration}
			state.permissions = map[harnessv2.PermissionRequestID]permissionState{}
			event, err := server.mapRuntimeEvent(state, prompt, acp.PromptEvent{
				Type: acp.PromptEventPermissionRequested, Timestamp: time.Now().UTC(),
				Permission: &acp.PermissionRequestEvent{RequestID: "permission-1", Request: acp.RequestPermissionRequest{
					ToolCall: json.RawMessage(copilotAskWrite),
					Options:  []acp.PermissionOption{{OptionID: "allow_once", Name: "Allow once", Kind: "allow_once"}},
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if event == nil || event.PermissionRequested == nil || event.PermissionRequested.ToolName != test.want {
				t.Fatalf("permission event = %#v, want tool %q", event, test.want)
			}
			if got := state.permissions["permission-1"].toolName; got != test.want {
				t.Fatalf("pending permission tool = %q, want %q", got, test.want)
			}
		})
	}
}

var (
	askTestPaths = acp.SessionPaths{Home: "/sessions/private/home", Workspace: "/sessions/private/workspace"}
	askTestProxy = ProviderProxyBinding{BaseURL: "http://127.0.0.1:43210/_orka/provider/session", Credential: "placeholder"}
)

func askRequest(request harnessv2.CreateRuntimeSessionRequest) harnessv2.CreateRuntimeSessionRequest {
	request.MCPConfiguration.NativePermissions = harnessv2.NativePermissionsAsk
	return request
}

func TestClaudeAskSessionNamesToolsAndAddsAskRules(t *testing.T) {
	request := testProviderProjectionRequest(t, providerKindClaude, "claude-test", "", "", nil, nil, true)
	policyOptions := claudeProjectionOptions(t, request, askTestPaths, askTestProxy)
	if _, ok := policyOptions["tools"]; ok {
		t.Fatalf("pre-approved unrestricted Claude session names tools: %#v", policyOptions)
	}
	if _, ok := policyOptions["settings"]; ok {
		t.Fatalf("pre-approved Claude session gained settings: %#v", policyOptions)
	}
	askOptions := claudeProjectionOptions(t, askRequest(request), askTestPaths, askTestProxy)
	if !slices.Equal(askOptions["tools"].([]string), providerNativeToolNames) || len(askOptions["disallowedTools"].([]string)) != 0 {
		t.Fatalf("ask Claude tools = %#v disallowed = %#v", askOptions["tools"], askOptions["disallowedTools"])
	}
	rules := askOptions["settings"].(map[string]any)["permissions"].(map[string]any)["ask"].([]string)
	if !slices.Equal(rules, []string{providerToolBash, providerToolEdit, providerToolWrite}) {
		t.Fatalf("ask Claude rules = %v", rules)
	}
	restricted := askRequest(testProviderProjectionRequest(t, providerKindClaude, "claude-test", "", "", []string{providerToolGlob, providerToolRead}, nil, false))
	restrictedOptions := claudeProjectionOptions(t, restricted, askTestPaths, askTestProxy)
	if !slices.Equal(restrictedOptions["tools"].([]string), []string{providerToolGlob, providerToolRead}) {
		t.Fatalf("restricted ask Claude tools = %#v", restrictedOptions["tools"])
	}
}

func TestCopilotAskSessionDropsPreApprovalRules(t *testing.T) {
	allowed := append(acp.BuiltInRuntimeNativeToolNames("copilot"), "send_message")
	request := testProviderProjectionRequest(t, providerKindCopilot, "copilot-test", "", "", allowed, nil, true)
	policy, err := copilotSessionProjection(request, askTestPaths, askTestProxy, "copilot-test")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(policy.AdditionalArgs, "--allow-tool=shell") || !slices.Contains(policy.AdditionalArgs, "--allow-tool=write") {
		t.Fatalf("pre-approved Copilot args = %v", policy.AdditionalArgs)
	}
	asking, err := copilotSessionProjection(askRequest(request), askTestPaths, askTestProxy, "copilot-test")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{policy.AdditionalArgs[0], "--allow-tool=orka"}
	if !slices.Equal(asking.AdditionalArgs, want) {
		t.Fatalf("ask Copilot args = %v, want %v", asking.AdditionalArgs, want)
	}
}

func TestOpenCodeAskSessionAsksForActingPermissions(t *testing.T) {
	request := harnessv2.CreateRuntimeSessionRequest{MCPConfiguration: harnessv2.MCPPolicyConfiguration{
		ToolPolicy: harnessv2.MCPToolPolicy{AllowedToolNames: []string{"apply_patch", "bash", "edit", "glob", "grep", "read", "write"}, AllowBash: true},
	}}
	for _, intent := range []harnessv2.WorkspaceIntent{harnessv2.WorkspaceIntentWrite, harnessv2.WorkspaceIntentRead} {
		policyPermissions := openCodeProjectionPermissions(t, intent, request, askTestPaths, askTestProxy)
		askPermissions := openCodeProjectionPermissions(t, intent, askRequest(request), askTestPaths, askTestProxy)
		for _, name := range []string{openCodeToolBash, openCodeToolEdit, openCodeToolWrite, openCodeToolApplyPatch} {
			want, wantAsk := any(openCodePermissionAllow), any(openCodePermissionAsk)
			if intent == harnessv2.WorkspaceIntentRead {
				want, wantAsk = openCodePermissionDeny, openCodePermissionDeny
			}
			if policyPermissions[name] != want || askPermissions[name] != wantAsk {
				t.Fatalf("%s %s permission = %#v / %#v, want %#v / %#v", intent, name, policyPermissions[name], askPermissions[name], want, wantAsk)
			}
		}
		if intent == harnessv2.WorkspaceIntentWrite && (askPermissions[openCodeToolGlob] != openCodePermissionAllow || askPermissions[openCodeToolGrep] != openCodePermissionAllow) {
			t.Fatalf("ask OpenCode read tools = %#v", askPermissions)
		}
	}
}

func TestCodexAskSessionLaunchesTheAskingAgentMode(t *testing.T) {
	request := testProviderProjectionRequest(t, providerKindCodex, "gpt-test", "", "", nil, nil, true)
	policy, err := codexSessionProjection(request, askTestPaths, askTestProxy, "gpt-test")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := policy.Environment["INITIAL_AGENT_MODE"]; ok || policy.RequiredModeID != "" || strings.Contains(policy.Environment["CODEX_CONFIG"], "features") {
		t.Fatalf("pre-approved Codex projection changed: %#v", policy)
	}
	asking, err := codexSessionProjection(askRequest(request), askTestPaths, askTestProxy, "gpt-test")
	if err != nil {
		t.Fatal(err)
	}
	if asking.Environment["INITIAL_AGENT_MODE"] != codexAgentModeOrkaExternalAsk || asking.RequiredModeID != codexAgentModeOrkaExternalAsk {
		t.Fatalf("ask Codex projection = %#v", asking)
	}
	var config map[string]any
	if err := json.Unmarshal([]byte(asking.Environment["CODEX_CONFIG"]), &config); err != nil {
		t.Fatal(err)
	}
	if features, _ := config["features"].(map[string]any); features["unified_exec_tty"] != false {
		t.Fatalf("ask Codex config = %#v", config)
	}
	restored := askRequest(request)
	restored.NativeRestore = &harnessv2.NativeSessionRestore{}
	restoredProjection, err := codexSessionProjection(restored, askTestPaths, askTestProxy, "gpt-test")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(restoredProjection.Environment["CODEX_CONFIG"], `"approval_policy":"untrusted"`) {
		t.Fatalf("ask Codex restore fallback = %s", restoredProjection.Environment["CODEX_CONFIG"])
	}
	readOnly := testProviderProjectionRequest(t, providerKindCodex, "gpt-test", "", "", []string{providerToolGlob, providerToolGrep, providerToolRead}, nil, false)
	readOnlyPolicy, err := codexSessionProjection(readOnly, askTestPaths, askTestProxy, "gpt-test")
	if err != nil {
		t.Fatal(err)
	}
	readOnlyAsk, err := codexSessionProjection(askRequest(readOnly), askTestPaths, askTestProxy, "gpt-test")
	if err != nil {
		t.Fatal(err)
	}
	if readOnlyAsk.RequiredModeID != "" || readOnlyAsk.Environment["CODEX_CONFIG"] != readOnlyPolicy.Environment["CODEX_CONFIG"] {
		t.Fatalf("read-only ask Codex projection = %#v, want the pre-approved launch", readOnlyAsk)
	}
}

func claudeProjectionOptions(t *testing.T, request harnessv2.CreateRuntimeSessionRequest, paths acp.SessionPaths, proxy ProviderProxyBinding) map[string]any {
	t.Helper()
	projection, err := claudeSessionProjection(request, paths, proxy, "claude-test")
	if err != nil {
		t.Fatal(err)
	}
	return projection.NewSessionMeta["claudeCode"].(map[string]any)["options"].(map[string]any)
}

func openCodeProjectionPermissions(t *testing.T, intent harnessv2.WorkspaceIntent, request harnessv2.CreateRuntimeSessionRequest, paths acp.SessionPaths, proxy ProviderProxyBinding) map[string]any {
	t.Helper()
	content, err := openCodeSessionConfig("openai/gpt-test", testOpenCodeModelLimits(), intent, request, paths, proxy)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(content, &config); err != nil {
		t.Fatal(err)
	}
	return config["permission"].(map[string]any)
}

func TestBuiltInProvidersAdvertiseNativePermissionAsk(t *testing.T) {
	for _, provider := range []string{providerKindCodex, providerKindClaude, providerKindCopilot, providerKindOpencode} {
		if !providerCapabilities(provider, "model").SupportsNativePermissionAsk {
			t.Fatalf("%s does not advertise native permission ask", provider)
		}
	}
	for _, provider := range []string{providerKindAgentKit, providerKindFoundry} {
		if providerCapabilities(provider, "model").SupportsNativePermissionAsk {
			t.Fatalf("%s advertises native permission ask", provider)
		}
	}
}

func TestCreateSessionRefusesAskWithoutTheCapability(t *testing.T) {
	server, cfg, profile := newTestServer(t, "immediate")
	server.cfg.Capabilities.Provider.SupportsNativePermissionAsk = false
	create := testCreateSessionRequest(t, cfg, profile)
	create.MCPConfiguration.NativePermissions = harnessv2.NativePermissionsAsk
	create.Metadata.RequestDigest = ""
	sealRequest(t, &create.Metadata.RequestDigest, create)
	response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "asks before each native tool") {
		t.Fatalf("create status = %d body=%s", response.Code, response.Body.String())
	}
}

func TestMCPProxyRefusesAPromptWithAnotherNativePermissionMode(t *testing.T) {
	proxy, err := newMCPProxy(MCPBrokerFunc(func(context.Context, harnessv2.MCPBrokerCallRequest) (harnessv2.MCPBrokerCallResponse, error) {
		return harnessv2.MCPBrokerCallResponse{}, errors.New("no broker calls are expected")
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = proxy.close(ctx)
	})
	fence := harnessv2.Fence{
		RuntimeInstanceID: "runtime-instance", SupervisorBootID: "boot", ControllerEpoch: 2,
		RuntimePoolUID: "pool-uid", RuntimePoolGeneration: 4,
		RuntimeSessionUID: "session-uid", RuntimeSessionGeneration: 3,
		RuntimeProfileDigest:       harnessv2.ProfileDigest(testDigest("profile")),
		ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion,
	}
	now := time.Now().UTC()
	authorization, lease := testMCPAuthorization(t, fence, now, false)
	sessionConfiguration := authorization.Configuration()
	sessionConfiguration.NativePermissions = harnessv2.NativePermissionsAsk
	session, _, err := proxy.newSession(fence, sessionConfiguration)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(session.close)
	if err := session.activate(t.Context(), authorization, lease, now); err == nil {
		t.Fatal("an ask session admitted a pre-approved prompt")
	}
	authorization.NativePermissions = harnessv2.NativePermissionsAsk
	if err := session.activate(t.Context(), authorization, lease, now); err != nil {
		t.Fatalf("ask prompt activation: %v", err)
	}
}
