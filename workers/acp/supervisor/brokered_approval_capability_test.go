package supervisor

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func TestProviderCapabilitiesDefaultToUnqualifiedBrokeredApprovals(t *testing.T) {
	for _, provider := range []string{providerKindAgentKit, providerKindFoundry, providerKindCodex, providerKindClaude, providerKindCopilot, providerKindOpencode} {
		capabilities := providerCapabilities(provider, "test-model")
		if capabilities.SupportsBrokeredToolApprovals {
			t.Fatalf("%s advertised brokered approvals without exact-profile qualification", provider)
		}
		if (provider == providerKindAgentKit || provider == providerKindFoundry) && capabilities.SupportsPermissions {
			t.Fatalf("%s brokered approvals advertised native ACP permissions", provider)
		}
	}
}

func TestSupervisorSessionRequiresBrokeredApprovalCapability(t *testing.T) {
	for _, supported := range []bool{false, true} {
		name := "missing capability"
		if supported {
			name = "qualified brokered approval"
		}
		t.Run(name, func(t *testing.T) {
			cfg, profile := newTestConfigWithUpstream(t, "immediate", "http://127.0.0.1:1", strings.Repeat("p", 32))
			profile.ProviderKind = providerKindAgentKit
			profile.AgentConfigurationDigest = testDigest("fixed-agentkit-config")
			fence := cfg.Fence
			fence.RuntimeSessionUID, fence.RuntimeSessionGeneration = "approval-session", 1
			authorization, _ := testMCPAuthorization(t, fence, time.Now().UTC(), true)
			profile.ToolPolicyDigest = authorization.ToolPolicyDigest
			profile.ApprovalPolicyDigest = authorization.ApprovalPolicyDigest
			profile.MCPConfigurationDigest = authorization.MCPConfigurationDigest
			digest, err := harnessv2.CanonicalProfileDigest(profile)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Fence.RuntimeProfileDigest = digest
			cfg.Capabilities.RuntimeProfileDigest = digest
			cfg.Capabilities.SupportsAgentSessionConfiguration = false
			cfg.Capabilities.Provider = providerCapabilities(providerKindAgentKit, profile.Model)
			cfg.Capabilities.Provider.SupportsBrokeredToolApprovals = supported
			// Native ACP permission support cannot substitute for the broker gate.
			cfg.Capabilities.Provider.SupportsPermissions = !supported
			cfg.Provider.Kind = providerKindAgentKit
			cfg.Provider.ProjectSession = func(request harnessv2.CreateRuntimeSessionRequest, _ acp.SessionPaths, _ ProviderProxyBinding) (ProviderSessionProjection, error) {
				return agentKitSessionProjection(request, profile.Model)
			}
			cfg.ProviderProxy.ProviderKind = providerKindAgentKit
			server, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				_ = server.Close(ctx)
			})
			request := testCreateSessionRequest(t, cfg, profile)
			request.AgentConfiguration = nil
			request.MCPConfiguration = authorization.Configuration()
			sealRequest(t, &request.Metadata.RequestDigest, request)
			response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", request, cfg)
			if supported {
				if response.Code != http.StatusCreated {
					t.Fatalf("qualified approval session returned %d: %s", response.Code, response.Body.String())
				}
			} else if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "does not support controller-owned brokered tool approvals") {
				t.Fatalf("unqualified approval session returned %d: %s", response.Code, response.Body.String())
			}
		})
	}
}
