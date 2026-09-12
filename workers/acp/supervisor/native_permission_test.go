package supervisor

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

//nolint:gocyclo // Keep the endpoint permission matrix and its setup together.
func TestResolvePermissionEnforcesNativeToolPolicy(t *testing.T) {
	for _, test := range []struct {
		name            string
		toolName        string
		kind            harnessv2.PermissionOptionKind
		disallowed      bool
		bashDisabled    bool
		brokered        bool
		requireApproval bool
		inactive        string
		wantAllowed     bool
	}{
		{name: "native allow once", toolName: providerToolBash, wantAllowed: true},
		{name: "native allow always denied", toolName: providerToolBash, kind: harnessv2.PermissionOptionAllowAlways},
		{name: "unknown native name", toolName: "unknown"},
		{name: "title alone cannot authorize native tool"},
		{name: "noncanonical native name", toolName: " Bash "},
		{name: "disallowed native tool", toolName: providerToolBash, disallowed: true},
		{name: "disabled Bash", toolName: providerToolBash, bashDisabled: true},
		{name: "brokered tool without additional approval", toolName: providerToolBash, brokered: true, wantAllowed: true},
		{name: "brokered reusable permission needs approval", toolName: providerToolBash, brokered: true, kind: harnessv2.PermissionOptionAllowAlways},
		{name: "brokered approval preserved", toolName: providerToolBash, brokered: true, requireApproval: true, wantAllowed: true},
		{name: "brokered reusable approval preserved", toolName: providerToolBash, brokered: true, requireApproval: true, kind: harnessv2.PermissionOptionAllowAlways, wantAllowed: true},
		{name: "expired prompt", toolName: providerToolBash, inactive: "authorization"},
		{name: "expired lease", toolName: providerToolBash, inactive: "lease"},
		{name: "expired permission", toolName: providerToolBash, inactive: "permission"},
		{name: "different prompt", toolName: providerToolBash, inactive: "prompt"},
		{name: "revoked prompt", toolName: providerToolBash, inactive: "revoked"},
		{name: "missing proxy", toolName: providerToolBash, inactive: "missing"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, cfg, profile := newTestServer(t, "immediate")
			create := testCreateSessionRequest(t, cfg, profile)
			created := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg)
			if created.Code != http.StatusCreated {
				t.Fatalf("create status = %d body=%s", created.Code, created.Body.String())
			}
			prompt := testStartPromptRequest(t, cfg, create.Metadata.Fence)
			now := time.Now().UTC()
			authorization, lease := buildTestMCPAuthorization(t, create.Metadata.Fence, now, providerToolBash, harnessv2.MCPToolEffectConsequential, test.requireApproval)
			policy := &authorization.ToolPolicy
			if !test.brokered {
				policy.Tools[0].Source = harnessv2.MCPToolSourceProviderNative
				policy.Tools[0].InputSchema = nil
			}
			if test.disallowed {
				policy.DisallowedToolNames = []string{providerToolBash}
				policy.Tools = nil
			}
			if test.bashDisabled {
				policy.AllowBash = false
				policy.Tools = nil
			}
			var err error
			policy.DescriptorDigest, err = harnessv2.CanonicalMCPToolDescriptorDigest(policy.Tools)
			if err != nil {
				t.Fatal(err)
			}
			authorization.ToolPolicyDigest, err = harnessv2.CanonicalRuntimeToolPolicyDigest(policy.AllowedToolNames, policy.DisallowedToolNames, policy.AllowBash)
			if err != nil {
				t.Fatal(err)
			}
			server.mu.Lock()
			state := server.sessions[create.RuntimeSessionID]
			proxy := state.mcpProxy
			server.mu.Unlock()
			proxy.mu.Lock()
			proxy.configuration = authorization.Configuration()
			proxy.mu.Unlock()
			if err := proxy.activate(authorization, lease, now); err != nil {
				t.Fatal(err)
			}
			if err := proxy.markRunning(authorization.PromptID, now); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { proxy.revoke(harnessv2.RuntimeSessionStateIdle) })
			optionKind := test.kind
			if optionKind == "" {
				optionKind = harnessv2.PermissionOptionAllowOnce
			}
			toolCallID, err := canonicalACPToolCallID("native-call-1")
			if err != nil {
				t.Fatal(err)
			}
			permission := permissionState{
				requestID: "permission-1", toolCallID: toolCallID, toolName: test.toolName, title: providerToolBash,
				requestedAt: now, expiresAt: now.Add(time.Minute),
				options: map[string]harnessv2.PermissionOptionKind{"allow": optionKind},
			}
			proxy.mu.Lock()
			switch test.inactive {
			case "authorization":
				proxy.authorization.ExpiresAt = now.Add(-time.Second)
			case "lease":
				proxy.lease.ExpiresAt = now.Add(-time.Second)
			case "permission":
				permission.expiresAt = now.Add(-time.Second)
			case "prompt":
				proxy.authorization.PromptID = "another-prompt"
			case "revoked":
				proxy.revokeLocked(harnessv2.RuntimeSessionStateCancelling)
			}
			proxy.mu.Unlock()
			mutations := newBlockingPromptMutator()
			close(mutations.resolveRelease)
			server.mu.Lock()
			state.prompt = &promptState{request: prompt}
			state.descriptor.State = harnessv2.RuntimeSessionStatePromptRunning
			state.promptMutations = mutations
			state.mcpProxy = proxy
			if test.inactive == "missing" {
				state.mcpProxy = nil
			}
			state.permissions[permission.requestID] = permission
			server.mu.Unlock()

			request := harnessv2.ResolvePermissionRequest{
				Protocol: harnessv2.ProtocolVersion, Metadata: testMetadata(create.Metadata.Fence, "permission-operation-1", true),
				RequestID: permission.requestID, Decision: harnessv2.PermissionDecision{Outcome: harnessv2.PermissionDecisionSelected, OptionID: "allow"},
			}
			sealRequest(t, &request.Metadata.RequestDigest, request)
			path := "/v2/runtime-sessions/session-1/prompts/" + string(request.Metadata.PromptID) + "/permissions/permission-1"
			response := performMutation(t, server.Handler(), http.MethodPut, path, request, cfg)
			wantStatus, wantCalls := http.StatusForbidden, int32(0)
			if test.wantAllowed {
				wantStatus, wantCalls = http.StatusOK, 1
			}
			if response.Code != wantStatus || mutations.resolveCalls.Load() != wantCalls {
				t.Fatalf("resolution status=%d calls=%d, want status=%d calls=%d; body=%s", response.Code, mutations.resolveCalls.Load(), wantStatus, wantCalls, response.Body.String())
			}
			proxy.mu.Lock()
			grants := proxy.approvals[providerToolBash]
			proxy.mu.Unlock()
			if !test.requireApproval && len(grants) != 0 {
				t.Fatal("permission created unrequested brokered MCP approval evidence")
			}
			if test.wantAllowed && test.requireApproval && (len(grants) != 1 || grants[0].evidence.Reusable != (optionKind == harnessv2.PermissionOptionAllowAlways)) {
				t.Fatalf("brokered approval evidence = %#v", grants)
			}
			if test.wantAllowed {
				replayed := performMutation(t, server.Handler(), http.MethodPut, path, request, cfg)
				var result harnessv2.PermissionResolutionResponse
				if err := json.Unmarshal(replayed.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if replayed.Code != http.StatusOK || result.State != harnessv2.PermissionResolutionAlreadyResolved || mutations.resolveCalls.Load() != 1 {
					t.Fatalf("duplicate resolution status=%d result=%#v calls=%d", replayed.Code, result, mutations.resolveCalls.Load())
				}
			}
		})
	}
}
