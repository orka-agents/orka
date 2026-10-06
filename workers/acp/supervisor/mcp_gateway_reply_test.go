package supervisor

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/tools"
	"github.com/stretchr/testify/require"
)

func TestMCPGatewayReplyFrozenVisibilityAndSealedIdentity(t *testing.T) {
	for _, selected := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary frozen policy", true: "gateway frozen policy"}[selected], func(t *testing.T) {
			calls := make(chan harnessv2.MCPBrokerCallRequest, 3)
			proxy, err := newMCPProxy(MCPBrokerFunc(func(_ context.Context, r harnessv2.MCPBrokerCallRequest) (harnessv2.MCPBrokerCallResponse, error) {
				calls <- r
				return harnessv2.MCPBrokerCallResponse{Protocol: harnessv2.ProtocolVersion, CallID: r.Call.CallID, Result: json.RawMessage(`{"success":true,"data":{"deliveryID":"gdm-receipt","status":"Pending","created":true}}`)}, nil
			}))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, proxy.close(context.Background())) })
			fence := harnessv2.Fence{RuntimeInstanceID: "runtime-instance", SupervisorBootID: "boot", ControllerEpoch: 2, RuntimePoolUID: "pool-uid", RuntimePoolGeneration: 4, RuntimeSessionUID: "session-uid", RuntimeSessionGeneration: 3, RuntimeProfileDigest: harnessv2.ProfileDigest(testDigest("profile")), ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion}
			now := time.Now().UTC()
			name := "lookup"
			if selected {
				name = "reply_in_conversation"
			}
			authorization, lease := buildTestMCPAuthorization(t, fence, now, name, harnessv2.MCPToolEffectConsequential, false)
			if selected {
				tool := tools.NewReplyInConversationTool()
				authorization.ToolPolicy.Tools[0].Description = tool.Description()
				authorization.ToolPolicy.Tools[0].InputSchema = tool.Parameters()
				authorization.ToolPolicy.DescriptorDigest, err = harnessv2.CanonicalMCPToolDescriptorDigest(authorization.ToolPolicy.Tools)
				require.NoError(t, err)
			}
			session, binding, err := proxy.newSession(fence, authorization.Configuration())
			require.NoError(t, err)
			session.mu.Lock()
			session.credential = []byte("credential")
			session.mu.Unlock()
			for _, active := range []bool{false, true} {
				if active {
					require.NoError(t, session.activate(t.Context(), authorization, lease, now))
					require.NoError(t, session.markRunning(authorization.PromptID, now.Add(time.Millisecond)))
				}
				response := decodeMCPResponse(t, doMCPRequest(t, binding.URL, "credential", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
				require.Nil(t, response.Error)
				encoded, err := json.Marshal(response.Result)
				require.NoError(t, err)
				if selected {
					require.Contains(t, string(encoded), "reply_in_conversation")
					require.Contains(t, string(encoded), `"additionalProperties":false`)
				} else {
					require.NotContains(t, string(encoded), "reply_in_conversation")
				}
			}
			response := decodeMCPResponse(t, doMCPRequest(t, binding.URL, "credential", `{"jsonrpc":"2.0","id":"provider-call","method":"tools/call","params":{"name":"reply_in_conversation","arguments":{"content":"working"}}}`))
			if !selected {
				require.NotNil(t, response.Error)
				require.Empty(t, calls)
				return
			}
			require.Nil(t, response.Error)
			forwarded := <-calls
			require.NotEqual(t, "provider-call", string(forwarded.Metadata.OperationID))
			require.Equal(t, authorization.TaskUID, forwarded.Metadata.TaskUID)
			require.Equal(t, fence, forwarded.Metadata.Fence)
			require.Equal(t, harnessv2.MCPToolEffectConsequential, forwarded.Authorization.ToolPolicy.Tools[0].Effect)
		})
	}
}
