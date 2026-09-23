package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func TestMCPProxyHoldsOnlyGateCancellationErrors(t *testing.T) {
	for _, outcome := range []string{"gate cancelled", "broker failure", "completed response", "invalid response"} {
		t.Run(outcome, func(t *testing.T) {
			started := make(chan struct{})
			broker := MCPBrokerFunc(func(ctx context.Context, request harnessv2.MCPBrokerCallRequest) (harnessv2.MCPBrokerCallResponse, error) {
				close(started)
				<-ctx.Done()
				switch outcome {
				case "gate cancelled":
					return harnessv2.MCPBrokerCallResponse{}, ctx.Err()
				case "broker failure":
					return harnessv2.MCPBrokerCallResponse{}, errors.New("independent broker failure")
				case "invalid response":
					return harnessv2.MCPBrokerCallResponse{CallID: "wrong-call"}, nil
				default:
					return harnessv2.MCPBrokerCallResponse{Protocol: harnessv2.ProtocolVersion,
						CallID: request.Call.CallID, Result: json.RawMessage(`{"code":"approval_cancelled"}`), IsError: true}, nil
				}
			})
			session, endpoint := newTestMCPProxySession(t, broker, false)
			authorization := activateCancellationTestMCP(t, session, false)
			release, held := make(chan struct{}), make(chan struct{}, 1)
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			cause := &promptGateCancellation{wait: func(ctx context.Context) {
				held <- struct{}{}
				select {
				case <-release:
				case <-ctx.Done():
				}
			}}
			response := serveCancellationTestMCP(session, endpoint, t.Context(), "action", "lookup")
			awaitSignal(t, started, "MCP request did not reach the broker")
			session.deactivateWithCause(authorization.PromptID, harnessv2.RuntimeSessionStateCancelling, cause)
			if outcome == "gate cancelled" {
				awaitSignal(t, held, "revocation error was not held for cancellation")
				select {
				case <-response:
					t.Fatal("gate error reached the child before cancellation settled")
				default:
				}
				unblock()
			}
			got := awaitCancellationTestMCP(t, response)
			if outcome == "completed response" {
				result, ok := got.Result.(map[string]any)
				if got.Error != nil || !ok || result["isError"] != true {
					t.Fatalf("definitive broker response changed: %#v", got)
				}
			} else if got.Error == nil || got.Error.Code != -32002 {
				t.Fatalf("broker failure changed: %#v", got)
			}
			if outcome != "gate cancelled" && len(held) != 0 {
				t.Fatal("an independent result was hidden by gate cancellation")
			}
		})
	}
}

func TestMCPProxyCancelledAdmissionStaysBoundedAndRevoked(t *testing.T) {
	var calls atomic.Int32
	session, endpoint := newTestMCPProxySession(t, MCPBrokerFunc(func(context.Context, harnessv2.MCPBrokerCallRequest) (harnessv2.MCPBrokerCallResponse, error) {
		calls.Add(1)
		return harnessv2.MCPBrokerCallResponse{}, errors.New("revoked call reached broker")
	}), false)
	authorization := activateCancellationTestMCP(t, session, false)
	held := make(chan struct{}, defaultMCPMaxSessionCalls)
	cause := &promptGateCancellation{wait: func(ctx context.Context) {
		held <- struct{}{}
		<-ctx.Done()
	}}
	session.deactivateWithCause(authorization.PromptID, harnessv2.RuntimeSessionStateCancelling, cause)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	responses := make([]<-chan *httptest.ResponseRecorder, 0, defaultMCPMaxSessionCalls)
	for index := range defaultMCPMaxSessionCalls {
		responses = append(responses, serveCancellationTestMCP(session, endpoint, ctx, strings.Repeat("x", index+1), "lookup"))
		awaitSignal(t, held, "revoked request was not held")
	}
	saturated := awaitCancellationTestMCP(t, serveCancellationTestMCP(session, endpoint, t.Context(), "saturated", "lookup"))
	if saturated.Error == nil || saturated.Error.Code != -32003 || calls.Load() != 0 {
		t.Fatalf("revoked admission escaped its bound: %#v, calls=%d", saturated, calls.Load())
	}
	cancel()
	for _, response := range responses {
		got := awaitCancellationTestMCP(t, response)
		if got.Error == nil || got.Error.Code != -32001 {
			t.Fatalf("revoked request was admitted: %#v", got)
		}
	}
	if len(session.calls) != 0 || calls.Load() != 0 {
		t.Fatal("downstream disconnect left a held request or admitted a tool")
	}
}

func activateCancellationTestMCP(t *testing.T, session *mcpProxySession, approval bool) harnessv2.PromptMCPAuthorization {
	t.Helper()
	now := time.Now().UTC()
	authorization, lease := testMCPAuthorization(t, session.fence, now, approval)
	if err := session.activate(t.Context(), authorization, lease, now); err != nil {
		t.Fatal(err)
	}
	if err := session.markRunning(authorization.PromptID, now); err != nil {
		t.Fatal(err)
	}
	return authorization
}

func serveCancellationTestMCP(session *mcpProxySession, endpoint string, ctx context.Context, id, tool string) <-chan *httptest.ResponseRecorder {
	payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": map[string]any{}}})
	request := httptest.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(payload)))
	request.RemoteAddr = "127.0.0.1:45678"
	request.Header.Set("Authorization", "Bearer credential")
	return serveMutationAsync(http.HandlerFunc(session.proxy.serveHTTP), request)
}

func awaitCancellationTestMCP(t *testing.T, responses <-chan *httptest.ResponseRecorder) mcpJSONRPCResponse {
	t.Helper()
	select {
	case response := <-responses:
		return decodeMCPResponse(t, response.Result())
	case <-time.After(5 * time.Second):
		t.Fatal("MCP request did not finish")
		return mcpJSONRPCResponse{}
	}
}
