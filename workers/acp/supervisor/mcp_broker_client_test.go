package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func TestControllerMCPBrokerClientPreservesLongCallerDeadline(t *testing.T) {
	client := newTestControllerMCPBrokerClient(t)
	configuredTransport := client.client.Transport

	callerDeadline := time.Now().Add(10 * time.Minute)
	observedDeadline := make(chan time.Time, 1)
	client.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		deadline, ok := request.Context().Deadline()
		if !ok {
			t.Fatal("broker request context has no caller deadline")
		}
		observedDeadline <- deadline
		return testMCPBrokerHTTPResponse(t, request, "call-1"), nil
	})

	ctx, cancel := context.WithDeadline(context.Background(), callerDeadline)
	defer cancel()
	if _, err := client.Call(ctx, testControllerMCPBrokerCallRequest(t, "call-1")); err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	deadline := <-observedDeadline
	if !deadline.Equal(callerDeadline) {
		t.Fatalf("broker request deadline = %s, want caller deadline %s", deadline, callerDeadline)
	}

	if client.client.Timeout != 0 {
		t.Fatalf("MCP broker client timeout = %s, want no client-wide timeout", client.client.Timeout)
	}
	transport, ok := configuredTransport.(*http.Transport)
	if !ok {
		t.Fatalf("MCP broker transport type = %T, want *http.Transport", configuredTransport)
	}
	if transport.DialContext == nil {
		t.Fatal("MCP broker transport has no bounded dialer")
	}
	if transport.TLSHandshakeTimeout <= 0 {
		t.Fatalf("MCP broker TLS handshake timeout = %s, want a positive bound", transport.TLSHandshakeTimeout)
	}
	if transport.ResponseHeaderTimeout != 0 {
		t.Fatalf("MCP broker response-header timeout = %s, want the caller context to remain authoritative", transport.ResponseHeaderTimeout)
	}
	if transport.MaxResponseHeaderBytes <= 0 {
		t.Fatalf("MCP broker max response-header bytes = %d, want a positive bound", transport.MaxResponseHeaderBytes)
	}
}

func TestControllerMCPBrokerClientContextCancellationStopsTransport(t *testing.T) {
	client := newTestControllerMCPBrokerClient(t)
	started := make(chan struct{})
	cancellation := make(chan error, 1)
	client.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		close(started)
		<-request.Context().Done()
		cancellation <- request.Context().Err()
		return nil, request.Context().Err()
	})

	ctx, cancel := context.WithCancel(context.Background())
	request := testControllerMCPBrokerCallRequest(t, "call-cancel")
	result := make(chan error, 1)
	go func() {
		_, err := client.Call(ctx, request)
		result <- err
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("broker transport did not start")
	}
	cancelledAt := time.Now()
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("Call() succeeded after context cancellation")
		}
		if observed := <-cancellation; observed != context.Canceled {
			t.Fatalf("broker transport context error = %v, want context.Canceled", observed)
		}
		if elapsed := time.Since(cancelledAt); elapsed > time.Second {
			t.Fatalf("Call() returned %s after cancellation, want prompt termination", elapsed)
		}
	case <-time.After(time.Second):
		t.Fatal("Call() did not terminate promptly after context cancellation")
	}
}

func newTestControllerMCPBrokerClient(t *testing.T) *controllerMCPBrokerClient {
	t.Helper()
	broker, err := NewControllerMCPBrokerClient(
		"https://controller.example.test",
		"default",
		strings.Repeat("b", 32),
		[]byte(strings.Repeat("s", harnessv2.MinCapabilitySecretBytes)),
	)
	if err != nil {
		t.Fatal(err)
	}
	client, ok := broker.(*controllerMCPBrokerClient)
	if !ok {
		t.Fatalf("MCP broker type = %T, want *controllerMCPBrokerClient", broker)
	}
	return client
}

func testControllerMCPBrokerCallRequest(t *testing.T, callID string) harnessv2.MCPBrokerCallRequest {
	t.Helper()
	now := time.Now().UTC()
	fence := harnessv2.Fence{
		RuntimeInstanceID: "runtime-instance", SupervisorBootID: "boot", ControllerEpoch: 2,
		RuntimePoolUID: "pool-uid", RuntimePoolGeneration: 4,
		RuntimeSessionUID: "session-uid", RuntimeSessionGeneration: 3,
		RuntimeProfileDigest:       harnessv2.ProfileDigest(testDigest("profile")),
		ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion,
	}
	authorization, lease := testMCPAuthorization(t, fence, now, false)
	return harnessv2.MCPBrokerCallRequest{
		Protocol:     harnessv2.ProtocolVersion,
		SessionState: harnessv2.RuntimeSessionStatePromptRunning,
		Metadata: harnessv2.MutationMetadata{
			Fence: fence, TaskUID: authorization.TaskUID, TaskAttempt: authorization.TaskAttempt,
			PromptID: authorization.PromptID, OperationID: harnessv2.OperationID("mcp-" + callID),
			RequestDigestSchemaVersion: harnessv2.RequestDigestSchemaVersion,
			ExpiresAt:                  now.Add(30 * time.Second),
		},
		Lease: lease, Authorization: authorization,
		Call: harnessv2.MCPToolCall{
			CallID: callID, ToolName: "lookup", Arguments: json.RawMessage(`{"value":"x"}`),
		},
	}
}

func testMCPBrokerHTTPResponse(t *testing.T, request *http.Request, callID string) *http.Response {
	t.Helper()
	body, err := json.Marshal(harnessv2.MCPBrokerCallResponse{
		Protocol: harnessv2.ProtocolVersion,
		CallID:   callID,
		Result:   json.RawMessage(`{"ok":true}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(body)),
		Request:    request,
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
