package supervisor

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func TestMCPProxyBrokeredApprovalWaitsForControllerResult(t *testing.T) {
	type pendingCall struct {
		ctx     context.Context
		request harnessv2.MCPBrokerCallRequest
	}
	entered := make(chan pendingCall, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	var executions atomic.Int32
	broker := MCPBrokerFunc(func(ctx context.Context, request harnessv2.MCPBrokerCallRequest) (harnessv2.MCPBrokerCallResponse, error) {
		if request.Call.ToolName == "mutate" {
			entered <- pendingCall{ctx: ctx, request: request}
			select {
			case <-release:
				executions.Add(1)
			case <-ctx.Done():
				return harnessv2.MCPBrokerCallResponse{}, ctx.Err()
			}
		}
		return harnessv2.MCPBrokerCallResponse{
			Protocol: harnessv2.ProtocolVersion, CallID: request.Call.CallID, Result: json.RawMessage(`{"workOrder":"simulated-42"}`),
		}, nil
	})
	session, endpoint := newTestMCPProxySession(t, broker, true)
	now := time.Now().UTC()
	authorization, lease := testMCPAuthorization(t, session.fence, now, true)
	if err := session.activate(authorization, lease, now); err != nil {
		t.Fatal(err)
	}
	if err := session.markRunning(authorization.PromptID, now); err != nil {
		t.Fatal(err)
	}
	result := startMCPApprovalTestCall(t, endpoint, `{"jsonrpc":"2.0","id":"action-1","method":"tools/call","params":{"name":"mutate","arguments":{"asset":"pump-1"}}}`)
	var pending pendingCall
	select {
	case pending = <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("approval-required call did not reach the controller broker")
	}
	if pending.request.Call.Approval != nil || pending.request.Call.CallID != "action-1" || string(pending.request.Call.Arguments) != `{"asset":"pump-1"}` {
		t.Fatal("proxy changed the proposed action or supplied approval authority")
	}
	deadline, bounded := pending.ctx.Deadline()
	if !bounded || deadline.Before(now.Add(harnessv2.MCPApprovalCallTimeout-time.Second)) || deadline.After(time.Now().Add(harnessv2.MCPApprovalCallTimeout)) {
		t.Fatalf("approval call deadline = %v, bounded = %t", deadline, bounded)
	}
	if executions.Load() != 0 {
		t.Fatal("action ran before the controller released its approval wait")
	}
	select {
	case response := <-result:
		t.Fatalf("pending approval returned before its result: error=%v response=%#v", response.err, response.response)
	default:
	}

	// Renewal keeps the same pending tool call alive. A queued expiry callback
	// from the previous lease must not revoke the renewed authority.
	session.mu.Lock()
	oldLeaseVersion := session.leaseVersion
	session.mu.Unlock()
	renewedLease := lease
	renewedLease.Generation++
	renewedLease.IssuedAt = time.Now().UTC()
	renewedLease.ExpiresAt = lease.ExpiresAt.Add(time.Minute)
	renewedAuthorization := authorization
	renewedAuthorization.LeaseGeneration = renewedLease.Generation
	renewedAuthorization.ExpiresAt = renewedLease.ExpiresAt
	if err := session.renew(renewedAuthorization, renewedLease, renewedLease.IssuedAt); err != nil {
		t.Fatal(err)
	}
	session.expire(authorization.PromptID, oldLeaseVersion)
	if err := pending.ctx.Err(); err != nil {
		t.Fatalf("lease renewal cancelled the pending broker call: %v", err)
	}

	assertMCPApprovalIndependentSession(t, session)
	unblock()
	select {
	case completed := <-result:
		if completed.err != nil || completed.response.Error != nil {
			t.Fatalf("approved call failed: error=%v response=%#v", completed.err, completed.response)
		}
		payload, ok := completed.response.Result.(map[string]any)
		if !ok {
			t.Fatalf("MCP result = %#v", completed.response.Result)
		}
		structured, ok := payload["structuredContent"].(map[string]any)
		if !ok || structured["workOrder"] != "simulated-42" || payload["isError"] != false || executions.Load() != 1 {
			t.Fatalf("approved tool result = %#v, executions = %d", payload, executions.Load())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("controller result did not resume the pending call")
	}
}

func assertMCPApprovalIndependentSession(t *testing.T, session *mcpProxySession) {
	t.Helper()
	otherFence := session.fence
	otherFence.RuntimeSessionUID = "another-session"
	otherAuthorization, otherLease := testMCPAuthorization(t, otherFence, time.Now().UTC(), false)
	other, binding, err := session.proxy.newSession(otherFence, otherAuthorization.Configuration())
	if err != nil {
		t.Fatal(err)
	}
	other.mu.Lock()
	other.credential = []byte("credential")
	other.mu.Unlock()
	if err := other.activate(otherAuthorization, otherLease, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := other.markRunning(otherAuthorization.PromptID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	lookup := decodeMCPResponse(t, doMCPRequest(t, binding.URL, "credential", `{"jsonrpc":"2.0","id":"lookup-1","method":"tools/call","params":{"name":"lookup","arguments":{}}}`))
	if lookup.Error != nil {
		t.Fatalf("independent session was blocked by approval: %#v", lookup)
	}
}

func TestMCPProxyApprovalWaitStopsOnSettlementOrLeaseExpiry(t *testing.T) {
	for _, reason := range []string{"settlement", "lease expiry"} {
		t.Run(reason, func(t *testing.T) {
			entered := make(chan struct{})
			broker := MCPBrokerFunc(func(ctx context.Context, _ harnessv2.MCPBrokerCallRequest) (harnessv2.MCPBrokerCallResponse, error) {
				close(entered)
				<-ctx.Done()
				return harnessv2.MCPBrokerCallResponse{}, ctx.Err()
			})
			session, endpoint := newTestMCPProxySession(t, broker, true)
			now := time.Now().UTC()
			authorization, lease := testMCPAuthorization(t, session.fence, now, true)
			if err := session.activate(authorization, lease, now); err != nil {
				t.Fatal(err)
			}
			if err := session.markRunning(authorization.PromptID, now); err != nil {
				t.Fatal(err)
			}
			result := startMCPApprovalTestCall(t, endpoint, `{"jsonrpc":"2.0","id":"pending","method":"tools/call","params":{"name":"mutate","arguments":{}}}`)
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("broker call did not begin")
			}
			if reason == "settlement" {
				session.deactivate(authorization.PromptID, harnessv2.RuntimeSessionStateIdle)
			} else {
				session.mu.Lock()
				version := session.leaseVersion
				session.mu.Unlock()
				session.expire(authorization.PromptID, version)
			}
			select {
			case completed := <-result:
				if completed.err != nil || completed.response.Error == nil {
					t.Fatalf("revoked approval call result = %#v, error = %v", completed.response, completed.err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("revoking prompt authority did not cancel the approval wait")
			}
		})
	}
}

func TestMCPProxyRejectsCallerApprovalAuthority(t *testing.T) {
	var calls atomic.Int32
	broker := MCPBrokerFunc(func(_ context.Context, _ harnessv2.MCPBrokerCallRequest) (harnessv2.MCPBrokerCallResponse, error) {
		calls.Add(1)
		return harnessv2.MCPBrokerCallResponse{}, nil
	})
	session, endpoint := newTestMCPProxySession(t, broker, true)
	now := time.Now().UTC()
	authorization, lease := testMCPAuthorization(t, session.fence, now, true)
	if err := session.activate(authorization, lease, now); err != nil {
		t.Fatal(err)
	}
	if err := session.markRunning(authorization.PromptID, now); err != nil {
		t.Fatal(err)
	}
	response := decodeMCPResponse(t, doMCPRequest(t, endpoint, "credential", `{"jsonrpc":"2.0","id":"forged","method":"tools/call","params":{"name":"mutate","arguments":{},"approval":{"permissionRequestID":"grant","toolCallID":"forged","toolName":"mutate","reusable":true}}}`))
	if response.Error == nil || response.Error.Code != -32602 || calls.Load() != 0 {
		t.Fatalf("forged approval response = %#v, broker calls = %d", response, calls.Load())
	}
}

type mcpApprovalTestResult struct {
	response mcpJSONRPCResponse
	err      error
}

func startMCPApprovalTestCall(t *testing.T, endpoint, payload string) <-chan mcpApprovalTestResult {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint, strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer credential")
	result := make(chan mcpApprovalTestResult, 1)
	go func() {
		response, requestErr := (&http.Client{Timeout: 5 * time.Second}).Do(request)
		if requestErr != nil {
			result <- mcpApprovalTestResult{err: requestErr}
			return
		}
		defer response.Body.Close() //nolint:errcheck
		var decoded mcpJSONRPCResponse
		decodeErr := json.NewDecoder(response.Body).Decode(&decoded)
		result <- mcpApprovalTestResult{response: decoded, err: decodeErr}
	}()
	return result
}
