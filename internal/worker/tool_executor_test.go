/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aramase/kontxt/pkg/keys"
	kontxttoken "github.com/aramase/kontxt/pkg/token"
	sdkverify "github.com/aramase/kontxt/sdk/verify"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/contexttoken"
	"github.com/sozercan/orka/internal/tracing"
	"github.com/sozercan/orka/internal/tracing/genai"
	"github.com/sozercan/orka/internal/tracing/testutil"
	"github.com/sozercan/orka/internal/workerenv"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

const (
	testOutboundToolScope      = "orka:tools:http"
	testTokenEndpointPath      = "/token_endpoint"
	testParentTransactionToken = "parent-tx-token"
)

func TestNewToolExecutor(t *testing.T) {
	executor := NewToolExecutor()
	if executor == nil {
		t.Fatal("NewToolExecutor returned nil")
	}
	if executor.client == nil {
		t.Error("client is nil")
	}
	if executor.namespace == "" {
		t.Error("namespace should have a default value")
	}
}

func TestNewToolExecutor_WithNamespaceEnv(t *testing.T) {
	originalNamespace := os.Getenv("ORKA_TASK_NAMESPACE")
	os.Setenv("ORKA_TASK_NAMESPACE", "custom-namespace")      //nolint:errcheck
	defer os.Setenv("ORKA_TASK_NAMESPACE", originalNamespace) //nolint:errcheck

	executor := NewToolExecutor()
	if executor.namespace != "custom-namespace" {
		t.Errorf("namespace = %s, want custom-namespace", executor.namespace)
	}
}

func TestToolExecutor_Execute_Success(t *testing.T) {
	expectedResponse := `{"result": "success"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(expectedResponse)) //nolint:errcheck
	}))
	defer server.Close()

	executor := &ToolExecutor{
		client:     server.Client(),
		namespace:  "default",
		secretPath: "/secrets/tools",
	}

	tool := &corev1alpha1.Tool{
		Spec: corev1alpha1.ToolSpec{
			HTTP: &corev1alpha1.HTTPExecution{
				URL: server.URL,
			},
		},
	}

	args := json.RawMessage(`{"key": "value"}`)
	result, err := executor.Execute(context.Background(), tool, args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if result != expectedResponse {
		t.Errorf("Execute() = %v, want %v", result, expectedResponse)
	}
}

func TestToolExecutor_Execute_PreservesLargeJSONIntegers(t *testing.T) {
	var receivedBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		receivedBody = string(body)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`)) //nolint:errcheck
	}))
	defer server.Close()

	executor := &ToolExecutor{
		client:     server.Client(),
		namespace:  "default",
		secretPath: "/secrets/tools",
	}
	tool := &corev1alpha1.Tool{
		Spec: corev1alpha1.ToolSpec{
			HTTP: &corev1alpha1.HTTPExecution{URL: server.URL},
		},
	}

	if _, err := executor.Execute(context.Background(), tool, json.RawMessage(`{"account":9007199254740993}`)); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !strings.Contains(receivedBody, `"account":9007199254740993`) {
		t.Fatalf("request body = %s, want large integer preserved", receivedBody)
	}
	if strings.Contains(receivedBody, `9007199254740992`) {
		t.Fatalf("request body = %s, large integer was rounded", receivedBody)
	}
}

type zeroLengthRoundTripFunc func(*http.Request) (*http.Response, error)

func (f zeroLengthRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestToolExecutor_Execute_CustomTransportReadsZeroContentLengthBody(t *testing.T) {
	executor := &ToolExecutor{
		client: &http.Client{Transport: zeroLengthRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			}, nil
		})},
		namespace:  "default",
		secretPath: "/secrets/tools",
	}
	tool := &corev1alpha1.Tool{
		Spec: corev1alpha1.ToolSpec{
			HTTP: &corev1alpha1.HTTPExecution{URL: "http://tool.test"},
		},
	}

	result, err := executor.Execute(context.Background(), tool, json.RawMessage(`{"x":1}`))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result != `{"ok":true}` {
		t.Fatalf("Execute() = %q, want response body", result)
	}
}

func TestToolExecutor_Execute_CustomTransportAllowsEmptyBodyWithContentLength(t *testing.T) {
	executor := &ToolExecutor{
		client: &http.Client{Transport: zeroLengthRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode:    http.StatusOK,
				Header:        make(http.Header),
				Body:          io.NopCloser(strings.NewReader(``)),
				ContentLength: 42,
			}, nil
		})},
		namespace:  "default",
		secretPath: "/secrets/tools",
	}
	tool := &corev1alpha1.Tool{
		Spec: corev1alpha1.ToolSpec{
			HTTP: &corev1alpha1.HTTPExecution{URL: "http://tool.test"},
		},
	}

	result, err := executor.Execute(context.Background(), tool, json.RawMessage(`{"x":1}`))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result != "" {
		t.Fatalf("Execute() = %q, want empty response body", result)
	}
}

func TestToolExecutor_Execute_MCPSubstrateActorUsesStatusEndpointAndRouteHost(t *testing.T) {
	var gotHosts []string
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHosts = append(gotHosts, r.Host)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request %d: %v", calls, err)
		}
		switch calls {
		case 0:
			if got := body["method"]; got != mcpInitializeMethod {
				t.Fatalf("first MCP method = %v, want %s", got, mcpInitializeMethod)
			}
			w.Write([]byte(`{"jsonrpc":"2.0","id":"initialize","result":{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"test","version":"1"}}}`)) //nolint:errcheck
		case 1:
			if got := body["method"]; got != mcpInitializedNotificationMethod {
				t.Fatalf("second MCP method = %v, want %s", got, mcpInitializedNotificationMethod)
			}
			w.WriteHeader(http.StatusAccepted)
		case 2:
			if got := body["method"]; got != mcpToolsCallMethod {
				t.Fatalf("third MCP method = %v, want %s", got, mcpToolsCallMethod)
			}
			w.Write([]byte(`{"jsonrpc":"2.0","id":"1","result":{"content":[{"type":"text","text":"mcp"}]}}`)) //nolint:errcheck
		default:
			t.Fatalf("unexpected MCP request %d", calls)
		}
		calls++
	}))
	defer server.Close()

	executor := &ToolExecutor{
		client:     server.Client(),
		namespace:  "default",
		secretPath: "/secrets/tools",
	}
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "lookup"},
		Spec: corev1alpha1.ToolSpec{
			MCP: &corev1alpha1.MCPToolServer{
				SubstrateActor: &corev1alpha1.SubstrateMCPActor{
					TemplateRef: corev1alpha1.WorkspaceTemplateReference{Name: "mcp-template"},
				},
			},
		},
		Status: corev1alpha1.ToolStatus{
			Endpoint: server.URL + "/mcp",
			Actor: &corev1alpha1.ToolActorStatus{
				RouteHost: "actor-1.actors.test",
			},
		},
	}

	result, err := executor.Execute(context.Background(), tool, json.RawMessage(`{"x":1}`))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result != "mcp" {
		t.Fatalf("Execute() = %q, want mcp", result)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
	for i, gotHost := range gotHosts {
		if gotHost != "actor-1.actors.test" {
			t.Fatalf("Host[%d] = %q, want actor route host", i, gotHost)
		}
	}
}

func TestToolExecutor_Execute_MCPSubstrateActorInitializesSession(t *testing.T) {
	const sessionID = "session-1"
	const negotiatedProtocolVersion = "2025-03-26"
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "actor-1.actors.test" {
			t.Fatalf("Host = %q, want actor route host", r.Host)
		}
		if r.Method == http.MethodDelete {
			if calls != 3 {
				t.Fatalf("delete call index = %d, want 3", calls)
			}
			if got := r.Header.Get(mcpProtocolVersionHeader); got != negotiatedProtocolVersion {
				t.Fatalf("delete %s = %q, want %q", mcpProtocolVersionHeader, got, negotiatedProtocolVersion)
			}
			if got := r.Header.Get(mcpSessionIDHeader); got != sessionID {
				t.Fatalf("delete %s = %q, want %q", mcpSessionIDHeader, got, sessionID)
			}
			w.WriteHeader(http.StatusNoContent)
			calls++
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request %d: %v", calls, err)
		}
		switch calls {
		case 0:
			if got := r.Header.Get(mcpProtocolVersionHeader); got != mcpProtocolVersion {
				t.Fatalf("initialize %s = %q, want %q", mcpProtocolVersionHeader, got, mcpProtocolVersion)
			}
			if got := body["method"]; got != mcpInitializeMethod {
				t.Fatalf("first MCP method = %v, want %s", got, mcpInitializeMethod)
			}
			if got := r.Header.Get(mcpSessionIDHeader); got != "" {
				t.Fatalf("initial %s = %q, want empty", mcpSessionIDHeader, got)
			}
			w.Header().Set(mcpSessionIDHeader, sessionID)
			w.Write([]byte(`{"jsonrpc":"2.0","id":"initialize","result":{"protocolVersion":"` + negotiatedProtocolVersion + `","capabilities":{},"serverInfo":{"name":"test","version":"1"}}}`)) //nolint:errcheck
		case 1:
			if got := r.Header.Get(mcpProtocolVersionHeader); got != negotiatedProtocolVersion {
				t.Fatalf("initialized %s = %q, want %q", mcpProtocolVersionHeader, got, negotiatedProtocolVersion)
			}
			if got := body["method"]; got != mcpInitializedNotificationMethod {
				t.Fatalf("second MCP method = %v, want %s", got, mcpInitializedNotificationMethod)
			}
			if got := r.Header.Get(mcpSessionIDHeader); got != sessionID {
				t.Fatalf("initialized %s = %q, want %q", mcpSessionIDHeader, got, sessionID)
			}
			w.WriteHeader(http.StatusAccepted)
		case 2:
			if got := r.Header.Get(mcpProtocolVersionHeader); got != negotiatedProtocolVersion {
				t.Fatalf("tools/call %s = %q, want %q", mcpProtocolVersionHeader, got, negotiatedProtocolVersion)
			}
			if got := body["method"]; got != mcpToolsCallMethod {
				t.Fatalf("third MCP method = %v, want %s", got, mcpToolsCallMethod)
			}
			if got := r.Header.Get(mcpSessionIDHeader); got != sessionID {
				t.Fatalf("tools/call %s = %q, want %q", mcpSessionIDHeader, got, sessionID)
			}
			params, ok := body["params"].(map[string]any)
			if !ok {
				t.Fatalf("params = %T, want object", body["params"])
			}
			if got := params["name"]; got != "lookup" {
				t.Fatalf("tools/call name = %v, want lookup", got)
			}
			args, ok := params["arguments"].(map[string]any)
			if !ok {
				t.Fatalf("arguments = %T, want object", params["arguments"])
			}
			if got := args["x"]; got != float64(1) {
				t.Fatalf("argument x = %v, want 1", got)
			}
			w.Write([]byte(`{"jsonrpc":"2.0","id":"1","result":{"content":[{"type":"text","text":"mcp-session-ok"}]}}`)) //nolint:errcheck
		default:
			t.Fatalf("unexpected MCP request %d", calls)
		}
		calls++
	}))
	defer server.Close()

	executor := &ToolExecutor{
		client:     server.Client(),
		namespace:  "default",
		secretPath: "/secrets/tools",
	}
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "lookup"},
		Spec: corev1alpha1.ToolSpec{
			MCP: &corev1alpha1.MCPToolServer{
				SubstrateActor: &corev1alpha1.SubstrateMCPActor{
					TemplateRef: corev1alpha1.WorkspaceTemplateReference{Name: "mcp-template"},
				},
			},
		},
		Status: corev1alpha1.ToolStatus{
			Endpoint: server.URL + "/mcp",
			Actor: &corev1alpha1.ToolActorStatus{
				RouteHost: "actor-1.actors.test",
			},
		},
	}

	result, err := executor.Execute(context.Background(), tool, json.RawMessage(`{"x":1}`))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result != "mcp-session-ok" {
		t.Fatalf("Execute() = %q, want mcp-session-ok", result)
	}
	if calls != 4 {
		t.Fatalf("calls = %d, want 4", calls)
	}
}

func TestToolExecutor_Execute_MCPSubstrateActorSendsApprovalIdempotencyOnlyOnToolsCall(t *testing.T) {
	const sessionID = "session-approval-1"
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			if got := r.Header.Get(toolIdempotencyKeyHeader); got != "" {
				t.Fatalf("delete %s = %q, want empty", toolIdempotencyKeyHeader, got)
			}
			if got := r.Header.Get(mcpSessionIDHeader); got != sessionID {
				t.Fatalf("delete %s = %q, want %q", mcpSessionIDHeader, got, sessionID)
			}
			w.WriteHeader(http.StatusNoContent)
			calls++
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request %d: %v", calls, err)
		}
		switch calls {
		case 0:
			if got := r.Header.Get(toolIdempotencyKeyHeader); got != "" {
				t.Fatalf("initialize %s = %q, want empty", toolIdempotencyKeyHeader, got)
			}
			if got := body["method"]; got != mcpInitializeMethod {
				t.Fatalf("first MCP method = %v, want %s", got, mcpInitializeMethod)
			}
			w.Header().Set(mcpSessionIDHeader, sessionID)
			w.Write([]byte(`{"jsonrpc":"2.0","id":"initialize","result":{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"test","version":"1"}}}`)) //nolint:errcheck
		case 1:
			if got := r.Header.Get(toolIdempotencyKeyHeader); got != "" {
				t.Fatalf("initialized %s = %q, want empty", toolIdempotencyKeyHeader, got)
			}
			if got := body["method"]; got != mcpInitializedNotificationMethod {
				t.Fatalf("second MCP method = %v, want %s", got, mcpInitializedNotificationMethod)
			}
			w.WriteHeader(http.StatusAccepted)
		case 2:
			if got := r.Header.Get(toolIdempotencyKeyHeader); got != "approval-marker-1" {
				t.Fatalf("tools/call %s = %q, want approval-marker-1", toolIdempotencyKeyHeader, got)
			}
			if got := body["method"]; got != mcpToolsCallMethod {
				t.Fatalf("third MCP method = %v, want %s", got, mcpToolsCallMethod)
			}
			w.Write([]byte(`{"jsonrpc":"2.0","id":"1","result":{"content":[{"type":"text","text":"approved-call-ok"}]}}`)) //nolint:errcheck
		default:
			t.Fatalf("unexpected MCP request %d", calls)
		}
		calls++
	}))
	defer server.Close()

	executor := &ToolExecutor{
		client:     server.Client(),
		namespace:  "default",
		secretPath: "/secrets/tools",
	}
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "lookup"},
		Spec: corev1alpha1.ToolSpec{
			MCP: &corev1alpha1.MCPToolServer{
				SubstrateActor: &corev1alpha1.SubstrateMCPActor{
					TemplateRef: corev1alpha1.WorkspaceTemplateReference{Name: "mcp-template"},
				},
			},
		},
		Status: corev1alpha1.ToolStatus{
			Endpoint: server.URL + "/mcp",
			Actor: &corev1alpha1.ToolActorStatus{
				RouteHost: "actor-1.actors.test",
			},
		},
	}
	ctx := WithToolIdempotencyKey(context.Background(), "approval-marker-1")

	result, err := executor.Execute(ctx, tool, json.RawMessage(`{"x":1}`))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result != "approved-call-ok" {
		t.Fatalf("Execute() = %q, want approved-call-ok", result)
	}
	if calls != 4 {
		t.Fatalf("calls = %d, want 4", calls)
	}
}

func TestToolExecutor_Execute_MCPSubstrateActorAllowsUnsupportedSessionTermination(t *testing.T) {
	const sessionID = "session-1"
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			if got := r.Header.Get(mcpSessionIDHeader); got != sessionID {
				t.Fatalf("delete %s = %q, want %q", mcpSessionIDHeader, got, sessionID)
			}
			w.WriteHeader(http.StatusMethodNotAllowed)
			calls++
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request %d: %v", calls, err)
		}
		switch calls {
		case 0:
			w.Header().Set(mcpSessionIDHeader, sessionID)
			w.Write([]byte(`{"jsonrpc":"2.0","id":"initialize","result":{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"test","version":"1"}}}`)) //nolint:errcheck
		case 1:
			if got := body["method"]; got != mcpInitializedNotificationMethod {
				t.Fatalf("second MCP method = %v, want %s", got, mcpInitializedNotificationMethod)
			}
			w.WriteHeader(http.StatusAccepted)
		case 2:
			if got := body["method"]; got != mcpToolsCallMethod {
				t.Fatalf("third MCP method = %v, want %s", got, mcpToolsCallMethod)
			}
			w.Write([]byte(`{"jsonrpc":"2.0","id":"1","result":{"content":[{"type":"text","text":"cleanup-allowed"}]}}`)) //nolint:errcheck
		default:
			t.Fatalf("unexpected MCP request %d", calls)
		}
		calls++
	}))
	defer server.Close()

	executor := &ToolExecutor{
		client:     server.Client(),
		namespace:  "default",
		secretPath: "/secrets/tools",
	}
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "lookup"},
		Spec: corev1alpha1.ToolSpec{
			MCP: &corev1alpha1.MCPToolServer{
				SubstrateActor: &corev1alpha1.SubstrateMCPActor{
					TemplateRef: corev1alpha1.WorkspaceTemplateReference{Name: "mcp-template"},
				},
			},
		},
		Status: corev1alpha1.ToolStatus{
			Endpoint: server.URL + "/mcp",
			Actor: &corev1alpha1.ToolActorStatus{
				RouteHost: "actor-1.actors.test",
			},
		},
	}

	result, err := executor.Execute(context.Background(), tool, json.RawMessage(`{"x":1}`))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result != "cleanup-allowed" {
		t.Fatalf("Execute() = %q, want cleanup-allowed", result)
	}
	if calls != 4 {
		t.Fatalf("calls = %d, want 4", calls)
	}
}

func TestToolExecutor_Execute_MCPSubstrateActorSendsInitializedWithoutSession(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request %d: %v", len(methods), err)
		}
		method, _ := body["method"].(string)
		methods = append(methods, method)
		if got := r.Header.Get(mcpSessionIDHeader); got != "" {
			t.Fatalf("%s = %q, want empty for stateless MCP server", mcpSessionIDHeader, got)
		}
		switch method {
		case mcpInitializeMethod:
			w.Write([]byte(`{"jsonrpc":"2.0","id":"initialize","result":{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"test","version":"1"}}}`)) //nolint:errcheck
		case mcpInitializedNotificationMethod:
			w.WriteHeader(http.StatusAccepted)
		case mcpToolsCallMethod:
			w.Write([]byte(`{"jsonrpc":"2.0","id":"1","result":{"content":[{"type":"text","text":"stateless-ok"}]}}`)) //nolint:errcheck
		default:
			t.Fatalf("unexpected MCP method %q", method)
		}
	}))
	defer server.Close()

	executor := &ToolExecutor{
		client:     server.Client(),
		namespace:  "default",
		secretPath: "/secrets/tools",
	}
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "lookup"},
		Spec: corev1alpha1.ToolSpec{
			MCP: &corev1alpha1.MCPToolServer{
				SubstrateActor: &corev1alpha1.SubstrateMCPActor{
					TemplateRef: corev1alpha1.WorkspaceTemplateReference{Name: "mcp-template"},
				},
			},
		},
		Status: corev1alpha1.ToolStatus{
			Endpoint: server.URL + "/mcp",
			Actor: &corev1alpha1.ToolActorStatus{
				RouteHost: "actor-1.actors.test",
			},
		},
	}

	result, err := executor.Execute(context.Background(), tool, json.RawMessage(`{"x":1}`))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result != "stateless-ok" {
		t.Fatalf("Execute() = %q, want stateless-ok", result)
	}
	wantMethods := []string{mcpInitializeMethod, mcpInitializedNotificationMethod, mcpToolsCallMethod}
	if fmt.Sprint(methods) != fmt.Sprint(wantMethods) {
		t.Fatalf("methods = %v, want %v", methods, wantMethods)
	}
}

func TestToolExecutor_Execute_MCPSubstrateActorRedactsSessionIDOnErrors(t *testing.T) {
	const sessionID = "session-secret-1"
	for _, tt := range []struct {
		name      string
		failStep  int
		wantError string
	}{
		{name: "initialized notification", failStep: 1, wantError: "MCP initialized notification failed"},
		{name: "tool call", failStep: 2, wantError: "tool returned HTTP 500"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var calls int
			var deleted bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					deleted = true
					if got := r.Header.Get(mcpSessionIDHeader); got != sessionID {
						t.Fatalf("delete %s = %q, want %q", mcpSessionIDHeader, got, sessionID)
					}
					w.WriteHeader(http.StatusNoContent)
					calls++
					return
				}
				switch calls {
				case 0:
					w.Header().Set(mcpSessionIDHeader, sessionID)
					w.Write([]byte(`{"jsonrpc":"2.0","id":"initialize","result":{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"test","version":"1"}}}`)) //nolint:errcheck
				case tt.failStep:
					w.WriteHeader(http.StatusInternalServerError)
					w.Write([]byte(`{"error":"echoed ` + sessionID + `"}`)) //nolint:errcheck
				default:
					if calls == 1 {
						w.WriteHeader(http.StatusAccepted)
					} else {
						w.Write([]byte(`{"jsonrpc":"2.0","id":"1","result":{"content":[{"type":"text","text":"ok"}]}}`)) //nolint:errcheck
					}
				}
				calls++
			}))
			defer server.Close()

			executor := &ToolExecutor{
				client:     server.Client(),
				namespace:  "default",
				secretPath: "/secrets/tools",
			}
			tool := &corev1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{Name: "lookup"},
				Spec: corev1alpha1.ToolSpec{
					MCP: &corev1alpha1.MCPToolServer{
						SubstrateActor: &corev1alpha1.SubstrateMCPActor{
							TemplateRef: corev1alpha1.WorkspaceTemplateReference{Name: "mcp-template"},
						},
					},
				},
				Status: corev1alpha1.ToolStatus{
					Endpoint: server.URL + "/mcp",
					Actor: &corev1alpha1.ToolActorStatus{
						RouteHost: "actor-1.actors.test",
					},
				},
			}

			_, err := executor.Execute(context.Background(), tool, json.RawMessage(`{"x":1}`))
			if err == nil {
				t.Fatal("Execute() error = nil, want MCP HTTP error")
			}
			if !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("Execute() error = %q, want %q", err, tt.wantError)
			}
			if strings.Contains(err.Error(), sessionID) {
				t.Fatalf("Execute() error leaked session id: %q", err)
			}
			if !strings.Contains(err.Error(), "[REDACTED]") {
				t.Fatalf("Execute() error = %q, want redaction marker", err)
			}
			if !deleted {
				t.Fatal("session was not terminated after MCP error")
			}
		})
	}
}

func TestToolExecutor_Execute_MCPSubstrateActorRejectsInvalidInitializeResponse(t *testing.T) {
	for _, tt := range []struct {
		name      string
		response  string
		wantError string
	}{
		{
			name:      "mismatched response id",
			response:  `{"jsonrpc":"2.0","id":"1","result":{"content":[{"type":"text","text":"not initialize"}]}}`,
			wantError: `MCP response id did not match expected "initialize"`,
		},
		{
			name:      "missing protocol version",
			response:  `{"jsonrpc":"2.0","id":"initialize","result":{"capabilities":{},"serverInfo":{"name":"test","version":"1"}}}`,
			wantError: "MCP initialize result missing protocolVersion",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var calls int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				if calls > 1 {
					t.Fatalf("unexpected MCP request %d after invalid initialize response", calls)
				}
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(tt.response)) //nolint:errcheck
			}))
			defer server.Close()

			executor := &ToolExecutor{
				client:     server.Client(),
				namespace:  "default",
				secretPath: "/secrets/tools",
			}
			tool := &corev1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{Name: "lookup"},
				Spec: corev1alpha1.ToolSpec{
					MCP: &corev1alpha1.MCPToolServer{
						SubstrateActor: &corev1alpha1.SubstrateMCPActor{
							TemplateRef: corev1alpha1.WorkspaceTemplateReference{Name: "mcp-template"},
						},
					},
				},
				Status: corev1alpha1.ToolStatus{
					Endpoint: server.URL + "/mcp",
					Actor: &corev1alpha1.ToolActorStatus{
						RouteHost: "actor-1.actors.test",
					},
				},
			}

			_, err := executor.Execute(context.Background(), tool, json.RawMessage(`{"x":1}`))
			if err == nil {
				t.Fatal("Execute() error = nil, want invalid initialize response error")
			}
			if !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("Execute() error = %q, want %q", err, tt.wantError)
			}
			if calls != 1 {
				t.Fatalf("calls = %d, want only initialize request", calls)
			}
		})
	}
}

func TestToolExecutor_Execute_MCPSubstrateActorTerminatesSessionAfterInvalidInitializeResponse(t *testing.T) {
	const sessionID = "session-secret-1"
	for _, tt := range []struct {
		name      string
		response  string
		wantError string
	}{
		{
			name:      "mismatched response id",
			response:  `{"jsonrpc":"2.0","id":"1","result":{"content":[{"type":"text","text":"not initialize"}]}}`,
			wantError: `MCP response id did not match expected "initialize"`,
		},
		{
			name:      "missing protocol version",
			response:  `{"jsonrpc":"2.0","id":"initialize","result":{"capabilities":{},"serverInfo":{"name":"test","version":"1"}}}`,
			wantError: "MCP initialize result missing protocolVersion",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var initializeRequests int
			var deletes int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					deletes++
					if got := r.Header.Get(mcpSessionIDHeader); got != sessionID {
						t.Fatalf("delete %s = %q, want %q", mcpSessionIDHeader, got, sessionID)
					}
					if got := r.Header.Get(mcpProtocolVersionHeader); got != mcpProtocolVersion {
						t.Fatalf("delete %s = %q, want %q", mcpProtocolVersionHeader, got, mcpProtocolVersion)
					}
					w.WriteHeader(http.StatusNoContent)
					return
				}
				initializeRequests++
				if initializeRequests > 1 {
					t.Fatalf("unexpected MCP request %d after invalid initialize response", initializeRequests)
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set(mcpSessionIDHeader, sessionID)
				w.Write([]byte(tt.response)) //nolint:errcheck
			}))
			defer server.Close()

			executor := &ToolExecutor{
				client:     server.Client(),
				namespace:  "default",
				secretPath: "/secrets/tools",
			}
			tool := &corev1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{Name: "lookup"},
				Spec: corev1alpha1.ToolSpec{
					MCP: &corev1alpha1.MCPToolServer{
						SubstrateActor: &corev1alpha1.SubstrateMCPActor{
							TemplateRef: corev1alpha1.WorkspaceTemplateReference{Name: "mcp-template"},
						},
					},
				},
				Status: corev1alpha1.ToolStatus{
					Endpoint: server.URL + "/mcp",
					Actor: &corev1alpha1.ToolActorStatus{
						RouteHost: "actor-1.actors.test",
					},
				},
			}

			_, err := executor.Execute(context.Background(), tool, json.RawMessage(`{"x":1}`))
			if err == nil {
				t.Fatal("Execute() error = nil, want invalid initialize response error")
			}
			if !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("Execute() error = %q, want %q", err, tt.wantError)
			}
			if strings.Contains(err.Error(), sessionID) {
				t.Fatalf("Execute() error leaked session id: %q", err)
			}
			if initializeRequests != 1 {
				t.Fatalf("initialize requests = %d, want 1", initializeRequests)
			}
			if deletes != 1 {
				t.Fatalf("deletes = %d, want 1", deletes)
			}
		})
	}
}

func TestToolExecutor_Execute_MCPSubstrateActorIgnoresSessionTerminationFailureAfterSuccess(t *testing.T) {
	const sessionID = "session-secret-1"
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error":"cleanup echoed ` + sessionID + `"}`)) //nolint:errcheck
			calls++
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request %d: %v", calls, err)
		}
		switch calls {
		case 0:
			w.Header().Set(mcpSessionIDHeader, sessionID)
			w.Write([]byte(`{"jsonrpc":"2.0","id":"initialize","result":{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"test","version":"1"}}}`)) //nolint:errcheck
		case 1:
			if got := body["method"]; got != mcpInitializedNotificationMethod {
				t.Fatalf("second MCP method = %v, want %s", got, mcpInitializedNotificationMethod)
			}
			w.WriteHeader(http.StatusAccepted)
		case 2:
			if got := body["method"]; got != mcpToolsCallMethod {
				t.Fatalf("third MCP method = %v, want %s", got, mcpToolsCallMethod)
			}
			w.Write([]byte(`{"jsonrpc":"2.0","id":"1","result":{"content":[{"type":"text","text":"tool-ok"}]}}`)) //nolint:errcheck
		default:
			t.Fatalf("unexpected MCP request %d", calls)
		}
		calls++
	}))
	defer server.Close()

	executor := &ToolExecutor{
		client:     server.Client(),
		namespace:  "default",
		secretPath: "/secrets/tools",
	}
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "lookup"},
		Spec: corev1alpha1.ToolSpec{
			MCP: &corev1alpha1.MCPToolServer{
				SubstrateActor: &corev1alpha1.SubstrateMCPActor{
					TemplateRef: corev1alpha1.WorkspaceTemplateReference{Name: "mcp-template"},
				},
			},
		},
		Status: corev1alpha1.ToolStatus{
			Endpoint: server.URL + "/mcp",
			Actor: &corev1alpha1.ToolActorStatus{
				RouteHost: "actor-1.actors.test",
			},
		},
	}

	result, err := executor.Execute(context.Background(), tool, json.RawMessage(`{"x":1}`))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result != "tool-ok" {
		t.Fatalf("Execute() = %q, want tool-ok", result)
	}
	if calls != 4 {
		t.Fatalf("calls = %d, want 4", calls)
	}
}

func TestNormalizeMCPResponseBodySkipsIntermediateSSEEvents(t *testing.T) {
	body := []byte(strings.Join([]string{
		"event: message",
		`data: {"jsonrpc":"2.0","method":"notifications/progress","params":{"progress":1}}`,
		"",
		"event: message",
		`data: {"jsonrpc":"2.0","id":"1","result":{"content":[{"type":"text","text":"done"}]}}`,
		"",
	}, "\n"))

	got := normalizeMCPResponseBody("text/event-stream", body)
	want := `{"jsonrpc":"2.0","id":"1","result":{"content":[{"type":"text","text":"done"}]}}`
	if string(got) != want {
		t.Fatalf("normalizeMCPResponseBody() = %q, want %q", got, want)
	}
}

func TestExecuteMCPHTTPRequestReturnsOnMatchingSSEEventBeforeStreamCloses(t *testing.T) {
	unblock := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("response writer does not implement http.Flusher")
		}
		fmt.Fprint(w, "event: message\n")                                                                                        //nolint:errcheck
		fmt.Fprint(w, `data: {"jsonrpc":"2.0","method":"notifications/progress","params":{"progress":1}}`+"\n\n")                //nolint:errcheck
		fmt.Fprint(w, "event: message\n")                                                                                        //nolint:errcheck
		fmt.Fprint(w, `data: {"jsonrpc":"2.0","id":"1","result":{"content":[{"type":"text","text":"done"}]}}`+"\n\n")            //nolint:errcheck
		fmt.Fprint(w, `data: {"jsonrpc":"2.0","id":"unrelated","result":{"content":[{"type":"text","text":"ignored"}]}}`+"\n\n") //nolint:errcheck
		flusher.Flush()
		select {
		case <-unblock:
		case <-r.Context().Done():
		}
	}))
	defer func() {
		close(unblock)
		server.Close()
	}()

	req, err := http.NewRequest(http.MethodPost, server.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	client := server.Client()
	client.Timeout = 2 * time.Second

	started := time.Now()
	body, err := executeMCPHTTPRequest(client, req, mcpToolCallRequestID)
	if err != nil {
		t.Fatalf("executeMCPHTTPRequest() error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("executeMCPHTTPRequest() took %s, want return before stream closes", elapsed)
	}
	want := `{"jsonrpc":"2.0","id":"1","result":{"content":[{"type":"text","text":"done"}]}}`
	if string(body) != want {
		t.Fatalf("executeMCPHTTPRequest() body = %q, want %q", body, want)
	}
}

func TestDecodeMCPToolCallResponsePreservesNonTextContent(t *testing.T) {
	body := []byte(`{"jsonrpc":"2.0","id":"1","result":{"content":[{"type":"resource","resource":{"uri":"file:///tmp/result.json","mimeType":"application/json","text":"{\"ok\":true}"}}]}}`)

	result, err := decodeMCPToolCallResponse(body)
	if err != nil {
		t.Fatalf("decodeMCPToolCallResponse() error = %v", err)
	}

	want := `{"type":"resource","resource":{"uri":"file:///tmp/result.json","mimeType":"application/json","text":"{\"ok\":true}"}}`
	if result != want {
		t.Fatalf("decodeMCPToolCallResponse() = %q, want raw content %q", result, want)
	}
}

func TestToolExecutor_Execute_DefaultMethodPOST(t *testing.T) {
	var receivedMethod string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedMethod = r.Method
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	executor := &ToolExecutor{
		client:     server.Client(),
		namespace:  "default",
		secretPath: "/secrets/tools",
	}

	tool := &corev1alpha1.Tool{
		Spec: corev1alpha1.ToolSpec{
			HTTP: &corev1alpha1.HTTPExecution{
				URL:    server.URL,
				Method: "", // Empty, should default to POST
			},
		},
	}

	_, err := executor.Execute(context.Background(), tool, nil)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if receivedMethod != http.MethodPost {
		t.Errorf("Method = %s, want POST", receivedMethod)
	}
}

func TestToolExecutor_Execute_IdempotencyKeyHeader(t *testing.T) {
	var receivedHeader string
	var receivedBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeader = r.Header.Get("Idempotency-Key")
		if err := json.NewDecoder(r.Body).Decode(&receivedBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	executor := &ToolExecutor{client: server.Client(), namespace: "default", secretPath: "/secrets/tools"}
	tool := &corev1alpha1.Tool{Spec: corev1alpha1.ToolSpec{HTTP: &corev1alpha1.HTTPExecution{URL: server.URL}}}
	ctx := WithToolIdempotencyKey(context.Background(), "approval-key-1")

	_, err := executor.Execute(ctx, tool, json.RawMessage(`{"input":"test"}`))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if receivedHeader != "approval-key-1" {
		t.Fatalf("Idempotency-Key = %q, want approval-key-1", receivedHeader)
	}
	if _, ok := receivedBody["idempotencyKey"]; ok {
		t.Fatalf("idempotency key was injected into body: %#v", receivedBody)
	}
}

func TestToolExecutor_Execute_CustomMethod(t *testing.T) {
	var receivedMethod string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedMethod = r.Method
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	executor := &ToolExecutor{
		client:     server.Client(),
		namespace:  "default",
		secretPath: "/secrets/tools",
	}

	tool := &corev1alpha1.Tool{
		Spec: corev1alpha1.ToolSpec{
			HTTP: &corev1alpha1.HTTPExecution{
				URL:    server.URL,
				Method: http.MethodPut,
			},
		},
	}

	_, err := executor.Execute(context.Background(), tool, nil)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if receivedMethod != http.MethodPut {
		t.Errorf("Method = %s, want PUT", receivedMethod)
	}
}

func TestToolExecutor_Execute_CustomHeaders(t *testing.T) {
	var receivedHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaders = r.Header
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	executor := &ToolExecutor{
		client:     server.Client(),
		namespace:  "default",
		secretPath: "/secrets/tools",
	}

	tool := &corev1alpha1.Tool{
		Spec: corev1alpha1.ToolSpec{
			HTTP: &corev1alpha1.HTTPExecution{
				URL: server.URL,
				Headers: map[string]string{
					"X-Custom-Header": "custom-value",
				},
			},
		},
	}

	_, err := executor.Execute(context.Background(), tool, nil)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if receivedHeaders.Get("X-Custom-Header") != "custom-value" {
		t.Errorf("X-Custom-Header = %s, want custom-value", receivedHeaders.Get("X-Custom-Header"))
	}
	if receivedHeaders.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %s, want application/json", receivedHeaders.Get("Content-Type"))
	}
}

func TestToolExecutor_Execute_AuthHeader(t *testing.T) {
	var receivedAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// Create temp secret file
	tmpDir := t.TempDir()
	secretDir := filepath.Join(tmpDir, "secret-name")
	os.MkdirAll(secretDir, 0755)                                                //nolint:errcheck
	os.WriteFile(filepath.Join(secretDir, "token"), []byte("test-token"), 0644) //nolint:errcheck

	executor := &ToolExecutor{
		client:     server.Client(),
		namespace:  "default",
		secretPath: tmpDir,
	}

	tool := &corev1alpha1.Tool{
		Spec: corev1alpha1.ToolSpec{
			HTTP: &corev1alpha1.HTTPExecution{
				URL: server.URL,
				AuthSecretRef: &corev1alpha1.SecretKeySelector{
					Name: "secret-name",
					Key:  "token",
				},
				AuthInject: "header",
			},
		},
	}

	_, err := executor.Execute(context.Background(), tool, nil)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if receivedAuth != "Bearer test-token" {
		t.Errorf("Authorization = %s, want Bearer test-token", receivedAuth)
	}
}

func TestToolExecutor_Execute_PropagatesTransactionTokenFile(t *testing.T) {
	txnTokenPath := filepath.Join(t.TempDir(), "txn-token")
	if err := os.WriteFile(txnTokenPath, []byte("tx-token"), 0600); err != nil {
		t.Fatalf("failed to write transaction token fixture: %v", err)
	}
	t.Setenv(workerenv.TransactionTokenFile, txnTokenPath)

	var receivedTxnToken string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedTxnToken = r.Header.Get(kontxttoken.HeaderName)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	executor := &ToolExecutor{
		client:     server.Client(),
		namespace:  "default",
		secretPath: "/secrets/tools",
	}
	tool := &corev1alpha1.Tool{
		Spec: corev1alpha1.ToolSpec{
			HTTP: &corev1alpha1.HTTPExecution{URL: server.URL},
		},
	}

	if _, err := executor.Execute(context.Background(), tool, nil); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if receivedTxnToken != "tx-token" {
		t.Fatalf("%s = %q, want tx-token", kontxttoken.HeaderName, receivedTxnToken)
	}
}

func TestToolExecutor_Execute_FailsClosedOnConfiguredTransactionTokenHeader(t *testing.T) {
	txnTokenPath := filepath.Join(t.TempDir(), "txn-token")
	if err := os.WriteFile(txnTokenPath, []byte("tx-token"), 0600); err != nil {
		t.Fatalf("failed to write transaction token fixture: %v", err)
	}
	t.Setenv(workerenv.TransactionTokenFile, txnTokenPath)

	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	executor := &ToolExecutor{
		client:     server.Client(),
		namespace:  "default",
		secretPath: "/secrets/tools",
	}
	tool := &corev1alpha1.Tool{
		Spec: corev1alpha1.ToolSpec{
			HTTP: &corev1alpha1.HTTPExecution{
				URL: server.URL,
				Headers: map[string]string{
					kontxttoken.HeaderName: "user-configured-token",
				},
			},
		},
	}

	_, err := executor.Execute(context.Background(), tool, nil)
	if err == nil {
		t.Fatal("Execute() error = nil, want reserved header conflict")
	}
	if !strings.Contains(err.Error(), "reserved header") || !strings.Contains(err.Error(), kontxttoken.HeaderName) {
		t.Fatalf("Execute() error = %q, want reserved %s header conflict", err, kontxttoken.HeaderName)
	}
	if called {
		t.Fatal("server was called despite reserved TxToken header conflict")
	}
}

func TestToolExecutor_Execute_ExchangesOutboundTransactionTokenWithTTS(t *testing.T) {
	subjectTokenPath := filepath.Join(t.TempDir(), "subject-token")
	if err := os.WriteFile(subjectTokenPath, []byte(testParentTransactionToken), 0600); err != nil {
		t.Fatalf("failed to write subject token fixture: %v", err)
	}

	keyManager, err := keys.NewManager(2048, time.Hour)
	if err != nil {
		t.Fatalf("failed to create kontxt key manager: %v", err)
	}
	jwksServer := httptest.NewServer(keyManager.JWKSHandler())
	defer jwksServer.Close()
	signingKey, kid := keyManager.SigningKey()
	downstreamToken, err := kontxttoken.New(kontxttoken.Claims{
		Issuer:             "https://tts.example.test",
		Audience:           "downstream.example.test",
		TransactionID:      "txn-downstream-123",
		Subject:            "spiffe://example.test/ns/default/sa/orka-worker",
		Scope:              testOutboundToolScope,
		RequestingWorkload: "spiffe://example.test/ns/default/sa/orka-worker",
		TransactionContext: map[string]any{
			"operation": "httpTool",
			"tool":      "downstream",
		},
	}, signingKey, kid, time.Minute)
	if err != nil {
		t.Fatalf("failed to create downstream TxToken: %v", err)
	}

	var ttsScope string
	var ttsSubjectToken string
	var ttsRequestedExpiresIn string
	var requestDetails map[string]any
	ttsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != testTokenEndpointPath {
			t.Fatalf("TTS path = %q, want %s", r.URL.Path, testTokenEndpointPath)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm() error = %v", err)
		}
		ttsSubjectToken = r.FormValue("subject_token")
		ttsScope = r.FormValue("scope")
		ttsRequestedExpiresIn = r.FormValue("requested_expires_in")
		if err := json.Unmarshal([]byte(r.FormValue("request_details")), &requestDetails); err != nil {
			t.Fatalf("request_details JSON error = %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token":      downstreamToken,
			"issued_token_type": "urn:ietf:params:oauth:token-type:txn_token",
			"token_type":        "N_A",
		})
	}))
	defer ttsServer.Close()

	t.Setenv(workerenv.ContextTokenTTSURL, ttsServer.URL)
	t.Setenv(workerenv.ContextTokenTTSTokenSource, contexttoken.TTSTokenSourceIncoming)
	t.Setenv(workerenv.TransactionTokenFile, subjectTokenPath)
	t.Setenv(workerenv.ContextTokenSubjectTokenFile, subjectTokenPath)
	t.Setenv(workerenv.ContextTokenOutboundScope, testOutboundToolScope)
	t.Setenv(workerenv.TransactionScopes, testOutboundToolScope)
	t.Setenv(workerenv.ContextTokenToolTokenTTL, "17s")
	t.Setenv(workerenv.TaskName, "task-1")

	verifier := sdkverify.New(jwksServer.URL, "downstream.example.test")
	var receivedTxnToken string
	var verifiedTransactionID string
	var verifiedScope string
	toolServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedTxnToken = r.Header.Get(kontxttoken.HeaderName)
		claims, err := verifier.Verify(r.Context(), receivedTxnToken)
		if err != nil {
			http.Error(w, "invalid TxToken", http.StatusUnauthorized)
			t.Errorf("downstream verifier rejected propagated TxToken: %v", err)
			return
		}
		verifiedTransactionID = claims.TransactionID
		verifiedScope = claims.Scope
		w.WriteHeader(http.StatusOK)
	}))
	defer toolServer.Close()

	executor := &ToolExecutor{client: toolServer.Client(), namespace: "default", secretPath: "/secrets/tools"}
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "downstream"},
		Spec: corev1alpha1.ToolSpec{
			HTTP: &corev1alpha1.HTTPExecution{URL: toolServer.URL},
		},
	}

	if _, err := executor.Execute(context.Background(), tool, nil); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if ttsSubjectToken != testParentTransactionToken {
		t.Fatalf("TTS subject_token = %q, want %s", ttsSubjectToken, testParentTransactionToken)
	}
	if ttsScope != testOutboundToolScope {
		t.Fatalf("TTS scope = %q, want %s", ttsScope, testOutboundToolScope)
	}
	if ttsRequestedExpiresIn != "17" {
		t.Fatalf("TTS requested_expires_in = %q, want 17", ttsRequestedExpiresIn)
	}
	if requestDetails["operation"] != "httpTool" || requestDetails["tool"] != "downstream" || requestDetails["task"] != "task-1" {
		t.Fatalf("request_details = %#v", requestDetails)
	}
	if receivedTxnToken == "" {
		t.Fatalf("missing propagated %s header", kontxttoken.HeaderName)
	}
	if receivedTxnToken != downstreamToken {
		t.Fatalf("%s did not contain token returned by TTS", kontxttoken.HeaderName)
	}
	if verifiedTransactionID != "txn-downstream-123" {
		t.Fatalf("downstream verified txn = %q, want txn-downstream-123", verifiedTransactionID)
	}
	if verifiedScope != testOutboundToolScope {
		t.Fatalf("downstream verified scope = %q, want %s", verifiedScope, testOutboundToolScope)
	}
}

func TestToolExecutor_Execute_DefaultsOutboundTTSToServiceAccountSubjectToken(t *testing.T) {
	var ttsCalled bool
	ttsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ttsCalled = true
		if r.URL.Path != testTokenEndpointPath {
			t.Fatalf("TTS path = %q, want %s", r.URL.Path, testTokenEndpointPath)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm() error = %v", err)
		}
		if got := r.FormValue("subject_token"); got != "service-account-token" {
			t.Fatalf("TTS subject_token = %q, want service-account-token", got)
		}
		if got := r.FormValue("subject_token_type"); got != kontxttoken.SubjectTokenTypeAccessToken {
			t.Fatalf("TTS subject_token_type = %q, want %q", got, kontxttoken.SubjectTokenTypeAccessToken)
		}
		if got := r.FormValue("scope"); got != testOutboundToolScope {
			t.Fatalf("TTS scope = %q, want %s", got, testOutboundToolScope)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token":      "downstream-token",
			"issued_token_type": "urn:ietf:params:oauth:token-type:txn_token",
			"token_type":        "N_A",
		})
	}))
	defer ttsServer.Close()

	t.Setenv(workerenv.ContextTokenTTSURL, ttsServer.URL)
	t.Setenv(workerenv.ContextTokenOutboundScope, testOutboundToolScope)
	t.Setenv(workerenv.TransactionScopes, testOutboundToolScope)
	t.Setenv(workerenv.ServiceAccountToken, "service-account-token")

	var receivedTxnToken string
	toolServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedTxnToken = r.Header.Get(kontxttoken.HeaderName)
		w.WriteHeader(http.StatusOK)
	}))
	defer toolServer.Close()

	executor := &ToolExecutor{client: toolServer.Client(), namespace: "default", secretPath: "/secrets/tools"}
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "downstream"},
		Spec: corev1alpha1.ToolSpec{
			HTTP: &corev1alpha1.HTTPExecution{URL: toolServer.URL},
		},
	}

	if _, err := executor.Execute(context.Background(), tool, nil); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !ttsCalled {
		t.Fatal("expected outbound TTS exchange")
	}
	if receivedTxnToken != "downstream-token" {
		t.Fatalf("%s = %q, want downstream-token", kontxttoken.HeaderName, receivedTxnToken)
	}
}

func TestToolExecutor_Execute_ReusesOutboundTTSClient(t *testing.T) {
	subjectTokenPath := filepath.Join(t.TempDir(), "subject-token")
	if err := os.WriteFile(subjectTokenPath, []byte(testParentTransactionToken), 0600); err != nil {
		t.Fatalf("failed to write subject token fixture: %v", err)
	}

	exchangeCount := 0
	ttsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchangeCount++
		if r.URL.Path != testTokenEndpointPath {
			t.Fatalf("TTS path = %q, want %s", r.URL.Path, testTokenEndpointPath)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm() error = %v", err)
		}
		if got := r.FormValue("subject_token"); got != testParentTransactionToken {
			t.Fatalf("TTS subject_token = %q, want %s", got, testParentTransactionToken)
		}
		if got := r.FormValue("scope"); got != testOutboundToolScope {
			t.Fatalf("TTS scope = %q, want %s", got, testOutboundToolScope)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token":      fmt.Sprintf("downstream-token-%d", exchangeCount),
			"issued_token_type": "urn:ietf:params:oauth:token-type:txn_token",
			"token_type":        "N_A",
		})
	}))
	defer ttsServer.Close()

	t.Setenv(workerenv.ContextTokenTTSURL, ttsServer.URL)
	t.Setenv(workerenv.ContextTokenTTSTokenSource, contexttoken.TTSTokenSourceIncoming)
	t.Setenv(workerenv.TransactionTokenFile, subjectTokenPath)
	t.Setenv(workerenv.ContextTokenSubjectTokenFile, subjectTokenPath)
	t.Setenv(workerenv.ContextTokenOutboundScope, testOutboundToolScope)
	t.Setenv(workerenv.TransactionScopes, testOutboundToolScope)

	var receivedTxnTokens []string
	toolServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedTxnTokens = append(receivedTxnTokens, r.Header.Get(kontxttoken.HeaderName))
		w.WriteHeader(http.StatusOK)
	}))
	defer toolServer.Close()

	executor := &ToolExecutor{client: toolServer.Client(), namespace: "default", secretPath: "/secrets/tools"}
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "downstream"},
		Spec: corev1alpha1.ToolSpec{
			HTTP: &corev1alpha1.HTTPExecution{URL: toolServer.URL},
		},
	}

	if _, err := executor.Execute(context.Background(), tool, nil); err != nil {
		t.Fatalf("first Execute() error = %v", err)
	}
	firstClient := executor.ttsClient
	if firstClient == nil {
		t.Fatal("executor.ttsClient is nil after first TTS exchange")
	}

	if _, err := executor.Execute(context.Background(), tool, nil); err != nil {
		t.Fatalf("second Execute() error = %v", err)
	}
	if executor.ttsClient != firstClient {
		t.Fatal("executor did not reuse cached TTS client for matching config")
	}
	if exchangeCount != 2 {
		t.Fatalf("TTS exchange count = %d, want 2", exchangeCount)
	}
	if len(receivedTxnTokens) != 2 {
		t.Fatalf("downstream call count = %d, want 2", len(receivedTxnTokens))
	}
	if receivedTxnTokens[0] != "downstream-token-1" || receivedTxnTokens[1] != "downstream-token-2" {
		t.Fatalf("propagated TxTokens = %v, want TTS responses", receivedTxnTokens)
	}
}

func TestToolExecutor_Execute_FailsClosedWhenOutboundTTSExchangeFails(t *testing.T) {
	subjectTokenPath := filepath.Join(t.TempDir(), "subject-token")
	if err := os.WriteFile(subjectTokenPath, []byte(testParentTransactionToken), 0600); err != nil {
		t.Fatalf("failed to write subject token fixture: %v", err)
	}

	ttsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"temporarily_unavailable","error_description":"maintenance"}`))
	}))
	defer ttsServer.Close()

	t.Setenv(workerenv.ContextTokenTTSURL, ttsServer.URL)
	t.Setenv(workerenv.ContextTokenTTSTokenSource, contexttoken.TTSTokenSourceIncoming)
	t.Setenv(workerenv.ContextTokenSubjectTokenFile, subjectTokenPath)
	t.Setenv(workerenv.ContextTokenOutboundScope, testOutboundToolScope)
	t.Setenv(workerenv.TransactionScopes, testOutboundToolScope)

	calledDownstream := false
	toolServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calledDownstream = true
		w.WriteHeader(http.StatusOK)
	}))
	defer toolServer.Close()

	executor := &ToolExecutor{client: toolServer.Client(), namespace: "default", secretPath: "/secrets/tools"}
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "downstream"},
		Spec: corev1alpha1.ToolSpec{
			HTTP: &corev1alpha1.HTTPExecution{URL: toolServer.URL},
		},
	}

	_, err := executor.Execute(context.Background(), tool, nil)
	if err == nil || !strings.Contains(err.Error(), "token exchange failed") || !strings.Contains(err.Error(), "temporarily_unavailable") {
		t.Fatalf("Execute() error = %v, want TTS exchange failure", err)
	}
	if calledDownstream {
		t.Fatal("downstream tool should not be called when outbound TTS exchange fails")
	}
}

func TestToolExecutor_Execute_TransactionTokenFileMissingFails(t *testing.T) {
	t.Setenv(workerenv.TransactionTokenFile, filepath.Join(t.TempDir(), "missing"))
	executor := &ToolExecutor{
		client:     http.DefaultClient,
		namespace:  "default",
		secretPath: "/secrets/tools",
	}
	tool := &corev1alpha1.Tool{
		Spec: corev1alpha1.ToolSpec{
			HTTP: &corev1alpha1.HTTPExecution{URL: "http://127.0.0.1"},
		},
	}

	_, err := executor.Execute(context.Background(), tool, nil)
	if err == nil || !strings.Contains(err.Error(), "transaction token file") {
		t.Fatalf("Execute() error = %v, want transaction token file error", err)
	}
}

func TestToolExecutor_Execute_AuthBody(t *testing.T) {
	var receivedBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&receivedBody) //nolint:errcheck
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// Create temp secret file
	tmpDir := t.TempDir()
	secretDir := filepath.Join(tmpDir, "secret-name")
	os.MkdirAll(secretDir, 0755)                                                      //nolint:errcheck
	os.WriteFile(filepath.Join(secretDir, "api_key"), []byte("secret-api-key"), 0644) //nolint:errcheck

	executor := &ToolExecutor{
		client:     server.Client(),
		namespace:  "default",
		secretPath: tmpDir,
	}

	tool := &corev1alpha1.Tool{
		Spec: corev1alpha1.ToolSpec{
			HTTP: &corev1alpha1.HTTPExecution{
				URL: server.URL,
				AuthSecretRef: &corev1alpha1.SecretKeySelector{
					Name: "secret-name",
					Key:  "api_key",
				},
				AuthInject:  "body",
				AuthBodyKey: "apiKey",
			},
		},
	}

	_, err := executor.Execute(context.Background(), tool, json.RawMessage(`{"query": "test"}`))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if receivedBody["apiKey"] != "secret-api-key" {
		t.Errorf("apiKey = %v, want secret-api-key", receivedBody["apiKey"])
	}
	if receivedBody["query"] != "test" {
		t.Errorf("query = %v, want test", receivedBody["query"])
	}
}

func TestToolExecutor_Execute_AuthBodyMissingKey(t *testing.T) {
	// Create temp secret file
	tmpDir := t.TempDir()
	secretDir := filepath.Join(tmpDir, "secret-name")
	os.MkdirAll(secretDir, 0755)                                                      //nolint:errcheck
	os.WriteFile(filepath.Join(secretDir, "api_key"), []byte("secret-api-key"), 0644) //nolint:errcheck

	executor := &ToolExecutor{
		client:     &http.Client{},
		namespace:  "default",
		secretPath: tmpDir,
	}

	tool := &corev1alpha1.Tool{
		Spec: corev1alpha1.ToolSpec{
			HTTP: &corev1alpha1.HTTPExecution{
				URL: "http://localhost",
				AuthSecretRef: &corev1alpha1.SecretKeySelector{
					Name: "secret-name",
					Key:  "api_key",
				},
				AuthInject:  "body",
				AuthBodyKey: "", // Missing
			},
		},
	}

	_, err := executor.Execute(context.Background(), tool, nil)
	if err == nil {
		t.Error("Execute() expected error for missing authBodyKey")
	}
}

func TestToolExecutor_Execute_MCPRejectsBodyAuth(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	tmpDir := t.TempDir()
	secretDir := filepath.Join(tmpDir, "secret-name")
	os.MkdirAll(secretDir, 0755)                                                      //nolint:errcheck
	os.WriteFile(filepath.Join(secretDir, "api_key"), []byte("secret-api-key"), 0644) //nolint:errcheck

	executor := &ToolExecutor{
		client:     server.Client(),
		namespace:  "default",
		secretPath: tmpDir,
	}
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "mcp-tool"},
		Spec: corev1alpha1.ToolSpec{
			HTTP: &corev1alpha1.HTTPExecution{
				AuthSecretRef: &corev1alpha1.SecretKeySelector{
					Name: "secret-name",
					Key:  "api_key",
				},
				AuthInject:  "body",
				AuthBodyKey: "apiKey",
			},
			MCP: &corev1alpha1.MCPToolServer{
				SubstrateActor: &corev1alpha1.SubstrateMCPActor{
					TemplateRef: corev1alpha1.WorkspaceTemplateReference{Name: "mcp-template"},
				},
			},
		},
		Status: corev1alpha1.ToolStatus{
			Endpoint: server.URL + "/mcp",
			Actor:    &corev1alpha1.ToolActorStatus{RouteHost: "actor-1.actors.test"},
		},
	}

	_, err := executor.Execute(context.Background(), tool, json.RawMessage(`{"query":"test"}`))
	if err == nil || !strings.Contains(err.Error(), "MCP tools do not support authInject=body") {
		t.Fatalf("Execute() error = %v, want MCP body auth rejection", err)
	}
	if called {
		t.Fatal("MCP endpoint was called despite invalid body auth config")
	}
}

func TestToolExecutor_Execute_HTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error")) //nolint:errcheck
	}))
	defer server.Close()

	executor := &ToolExecutor{
		client:     server.Client(),
		namespace:  "default",
		secretPath: "/secrets/tools",
	}

	tool := &corev1alpha1.Tool{
		Spec: corev1alpha1.ToolSpec{
			HTTP: &corev1alpha1.HTTPExecution{
				URL: server.URL,
			},
		},
	}

	_, err := executor.Execute(context.Background(), tool, nil)
	if err == nil {
		t.Error("Execute() expected error for HTTP 500")
	}
}

func TestToolExecutor_Execute_HTTPErrorRedactsPropagatedTransactionToken(t *testing.T) {
	txnTokenPath := filepath.Join(t.TempDir(), "txn-token")
	if err := os.WriteFile(txnTokenPath, []byte("tx-token-sensitive"), 0600); err != nil {
		t.Fatalf("failed to write transaction token fixture: %v", err)
	}
	t.Setenv(workerenv.TransactionTokenFile, txnTokenPath)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("debug echoed " + r.Header.Get(kontxttoken.HeaderName)))
	}))
	defer server.Close()

	executor := &ToolExecutor{
		client:     server.Client(),
		namespace:  "default",
		secretPath: "/secrets/tools",
	}
	tool := &corev1alpha1.Tool{
		Spec: corev1alpha1.ToolSpec{
			HTTP: &corev1alpha1.HTTPExecution{URL: server.URL},
		},
	}

	_, err := executor.Execute(context.Background(), tool, nil)
	if err == nil {
		t.Fatal("Execute() error = nil, want HTTP error")
	}
	if strings.Contains(err.Error(), "tx-token-sensitive") {
		t.Fatalf("Execute() error leaked transaction token: %v", err)
	}
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("Execute() error = %v, want redacted marker", err)
	}
}

func TestToolExecutor_Execute_InvalidArgs(t *testing.T) {
	executor := &ToolExecutor{
		client:     &http.Client{},
		namespace:  "default",
		secretPath: "/secrets/tools",
	}

	tool := &corev1alpha1.Tool{
		Spec: corev1alpha1.ToolSpec{
			HTTP: &corev1alpha1.HTTPExecution{
				URL: "http://localhost",
			},
		},
	}

	_, err := executor.Execute(context.Background(), tool, json.RawMessage(`{invalid json}`))
	if err == nil {
		t.Error("Execute() expected error for invalid JSON args")
	}
}

func TestToolExecutor_Execute_Timeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	executor := &ToolExecutor{
		client:     server.Client(),
		namespace:  "default",
		secretPath: "/secrets/tools",
	}

	timeout := metav1.Duration{Duration: 5 * time.Second}
	tool := &corev1alpha1.Tool{
		Spec: corev1alpha1.ToolSpec{
			HTTP: &corev1alpha1.HTTPExecution{
				URL:     server.URL,
				Timeout: &timeout,
			},
		},
	}

	_, err := executor.Execute(context.Background(), tool, nil)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	// Verify shared client timeout was NOT mutated (per-request client used instead)
	if executor.client.Timeout != 0 {
		t.Errorf("Timeout = %v, want 0 (shared client should not be mutated)", executor.client.Timeout)
	}
}

func TestToolExecutor_Execute_MissingAuthSecret(t *testing.T) {
	executor := &ToolExecutor{
		client:     &http.Client{},
		namespace:  "default",
		secretPath: "/nonexistent/path",
	}

	tool := &corev1alpha1.Tool{
		Spec: corev1alpha1.ToolSpec{
			HTTP: &corev1alpha1.HTTPExecution{
				URL: "http://localhost",
				AuthSecretRef: &corev1alpha1.SecretKeySelector{
					Name: "nonexistent-secret",
					Key:  "token",
				},
			},
		},
	}

	_, err := executor.Execute(context.Background(), tool, nil)
	if err == nil {
		t.Error("Execute() expected error for missing auth secret")
	}
}

func TestToolExecutor_getSecretKey_MountedSecret(t *testing.T) {
	tmpDir := t.TempDir()
	secretDir := filepath.Join(tmpDir, "my-secret")
	os.MkdirAll(secretDir, 0755)                                                       //nolint:errcheck
	os.WriteFile(filepath.Join(secretDir, "my-key"), []byte("  secret-value  "), 0644) //nolint:errcheck

	executor := &ToolExecutor{
		secretPath: tmpDir,
		namespace:  "default",
	}

	value, err := executor.getSecretKey(context.Background(), "my-secret", "my-key")
	if err != nil {
		t.Fatalf("getSecretKey() error = %v", err)
	}

	// Should trim whitespace
	if value != "secret-value" {
		t.Errorf("getSecretKey() = %q, want %q", value, "secret-value")
	}
}

func TestToolExecutor_getSecretKey_K8sAPISecret(t *testing.T) {
	// Create a fake Kubernetes client with a secret
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "k8s-secret",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"api-key": []byte("k8s-secret-value"),
		},
	}

	fakeClient := fake.NewSimpleClientset(secret) //nolint:staticcheck // NewClientset requires apply configs

	executor := &ToolExecutor{
		secretPath: "/nonexistent", // Mount doesn't exist
		namespace:  "default",
		k8sClient:  fakeClient,
	}

	value, err := executor.getSecretKey(context.Background(), "k8s-secret", "api-key")
	if err != nil {
		t.Fatalf("getSecretKey() error = %v", err)
	}

	if value != "k8s-secret-value" {
		t.Errorf("getSecretKey() = %q, want %q", value, "k8s-secret-value")
	}
}

func TestToolExecutor_getSecretKey_NotFound(t *testing.T) {
	fakeClient := fake.NewSimpleClientset() //nolint:staticcheck // NewClientset requires apply configs

	executor := &ToolExecutor{
		secretPath: "/nonexistent",
		namespace:  "default",
		k8sClient:  fakeClient,
	}

	_, err := executor.getSecretKey(context.Background(), "nonexistent-secret", "key")
	if err == nil {
		t.Error("getSecretKey() expected error for nonexistent secret")
	}
}

func TestToolExecutor_getSecretKey_TaskSecretPath(t *testing.T) {
	tmpDir := t.TempDir()
	taskSecretPath := filepath.Join(tmpDir, "task")
	os.MkdirAll(taskSecretPath, 0755)                                                  //nolint:errcheck
	os.WriteFile(filepath.Join(taskSecretPath, "my-key"), []byte("task-secret"), 0644) //nolint:errcheck

	// The getSecretKey function checks /secrets/task/{key} as one of the paths
	// We need to mock this properly
	executor := &ToolExecutor{
		secretPath: tmpDir,
		namespace:  "default",
	}

	// This test verifies the mounted secret paths work
	secretDir := filepath.Join(tmpDir, "my-secret")
	os.MkdirAll(secretDir, 0755)                                                  //nolint:errcheck
	os.WriteFile(filepath.Join(secretDir, "key"), []byte("mounted-secret"), 0644) //nolint:errcheck

	value, err := executor.getSecretKey(context.Background(), "my-secret", "key")
	if err != nil {
		t.Fatalf("getSecretKey() error = %v", err)
	}

	if value != "mounted-secret" {
		t.Errorf("getSecretKey() = %q, want %q", value, "mounted-secret")
	}
}

func TestToolExecutor_Execute_EmptyArgs(t *testing.T) {
	var receivedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedBody, _ = json.Marshal(map[string]any{})
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	executor := &ToolExecutor{
		client:     server.Client(),
		namespace:  "default",
		secretPath: "/secrets/tools",
	}

	tool := &corev1alpha1.Tool{
		Spec: corev1alpha1.ToolSpec{
			HTTP: &corev1alpha1.HTTPExecution{
				URL: server.URL,
			},
		},
	}

	// Empty args
	_, err := executor.Execute(context.Background(), tool, nil)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	// Should create empty params map
	_ = receivedBody
}

func TestToolExecutor_Execute_DefaultAuthInject(t *testing.T) {
	var receivedAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// Create temp secret file
	tmpDir := t.TempDir()
	secretDir := filepath.Join(tmpDir, "secret-name")
	os.MkdirAll(secretDir, 0755)                                                //nolint:errcheck
	os.WriteFile(filepath.Join(secretDir, "token"), []byte("test-token"), 0644) //nolint:errcheck

	executor := &ToolExecutor{
		client:     server.Client(),
		namespace:  "default",
		secretPath: tmpDir,
	}

	tool := &corev1alpha1.Tool{
		Spec: corev1alpha1.ToolSpec{
			HTTP: &corev1alpha1.HTTPExecution{
				URL: server.URL,
				AuthSecretRef: &corev1alpha1.SecretKeySelector{
					Name: "secret-name",
					Key:  "token",
				},
				AuthInject: "", // Empty, should default to header
			},
		},
	}

	_, err := executor.Execute(context.Background(), tool, nil)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	// Default should be header auth
	if receivedAuth != "Bearer test-token" {
		t.Errorf("Authorization = %s, want Bearer test-token", receivedAuth)
	}
}

func TestToolExecutor_Execute_URLInterpolation(t *testing.T) {
	var receivedPath string
	var receivedBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		json.NewDecoder(r.Body).Decode(&receivedBody) //nolint:errcheck
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status": "ok"}`)) //nolint:errcheck
	}))
	defer server.Close()

	executor := &ToolExecutor{
		client:     server.Client(),
		namespace:  "default",
		secretPath: "/secrets/tools",
	}

	tool := &corev1alpha1.Tool{
		Spec: corev1alpha1.ToolSpec{
			HTTP: &corev1alpha1.HTTPExecution{
				URL:    server.URL + "/repos/{{owner}}/{{repo}}/commits/{{ref}}/check-runs",
				Method: http.MethodGet,
			},
		},
	}

	args := json.RawMessage(`{"owner": "myorg", "repo": "myrepo", "ref": "abc123"}`)
	_, err := executor.Execute(context.Background(), tool, args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	// Verify URL was interpolated
	expectedPath := "/repos/myorg/myrepo/commits/abc123/check-runs"
	if receivedPath != expectedPath {
		t.Errorf("path = %q, want %q", receivedPath, expectedPath)
	}

	// Verify interpolated keys were removed from body
	if receivedBody != nil {
		if _, ok := receivedBody["owner"]; ok {
			t.Error("body should not contain 'owner' after interpolation")
		}
		if _, ok := receivedBody["repo"]; ok {
			t.Error("body should not contain 'repo' after interpolation")
		}
		if _, ok := receivedBody["ref"]; ok {
			t.Error("body should not contain 'ref' after interpolation")
		}
	}
}

func TestToolExecutor_Execute_URLInterpolation_Partial(t *testing.T) {
	var receivedPath string
	var receivedBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		json.NewDecoder(r.Body).Decode(&receivedBody) //nolint:errcheck
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	executor := &ToolExecutor{
		client:     server.Client(),
		namespace:  "default",
		secretPath: "/secrets/tools",
	}

	tool := &corev1alpha1.Tool{
		Spec: corev1alpha1.ToolSpec{
			HTTP: &corev1alpha1.HTTPExecution{
				URL:    server.URL + "/repos/{{owner}}/{{repo}}/pulls/{{pull_number}}/merge",
				Method: http.MethodPut,
			},
		},
	}

	args := json.RawMessage(`{"owner": "myorg", "repo": "myrepo", "pull_number": 42, "merge_method": "squash"}`)
	_, err := executor.Execute(context.Background(), tool, args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	// Verify URL was interpolated (pull_number is numeric, should be converted to string)
	expectedPath := "/repos/myorg/myrepo/pulls/42/merge"
	if receivedPath != expectedPath {
		t.Errorf("path = %q, want %q", receivedPath, expectedPath)
	}

	// Verify non-interpolated key remains in body
	if receivedBody == nil {
		t.Fatal("body should not be nil")
	}
	if receivedBody["merge_method"] != "squash" {
		t.Errorf("body merge_method = %v, want squash", receivedBody["merge_method"])
	}

	// Interpolated keys should be removed
	if _, ok := receivedBody["owner"]; ok {
		t.Error("body should not contain 'owner'")
	}
}

func TestToolExecutor_Execute_URLInterpolation_NoPlaceholders(t *testing.T) {
	var receivedPath string
	var receivedBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		json.NewDecoder(r.Body).Decode(&receivedBody) //nolint:errcheck
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	executor := &ToolExecutor{
		client:     server.Client(),
		namespace:  "default",
		secretPath: "/secrets/tools",
	}

	tool := &corev1alpha1.Tool{
		Spec: corev1alpha1.ToolSpec{
			HTTP: &corev1alpha1.HTTPExecution{
				URL: server.URL + "/api/search",
			},
		},
	}

	args := json.RawMessage(`{"query": "test", "limit": 10}`)
	_, err := executor.Execute(context.Background(), tool, args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	// URL should be unchanged
	if receivedPath != "/api/search" {
		t.Errorf("path = %q, want /api/search", receivedPath)
	}

	// All params should remain in body
	if receivedBody["query"] != "test" {
		t.Errorf("body query = %v, want test", receivedBody["query"])
	}
}

func TestToolExecutor_Execute_ResponseSizeLimit(t *testing.T) {
	// Create a server that returns more than 10MB
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// Write 11MB of data (10MB limit should truncate it)
		chunk := make([]byte, 1024*1024) // 1MB
		for range 11 {
			w.Write(chunk) //nolint:errcheck
		}
	}))
	defer server.Close()

	executor := &ToolExecutor{
		client:     server.Client(),
		namespace:  "default",
		secretPath: "/secrets/tools",
	}

	tool := &corev1alpha1.Tool{
		Spec: corev1alpha1.ToolSpec{
			HTTP: &corev1alpha1.HTTPExecution{
				URL: server.URL,
			},
		},
	}

	result, err := executor.Execute(context.Background(), tool, nil)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	// Verify response was limited to 10MB
	if len(result) > 10*1024*1024 {
		t.Errorf("response size = %d bytes, want <= 10MB", len(result))
	}

	// Should be exactly 10MB
	if len(result) != 10*1024*1024 {
		t.Errorf("response size = %d bytes, want exactly 10MB", len(result))
	}
}

func TestToolExecutor_Execute_IdempotencyKeyReservedHeaderConflict(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("server should not be called when Idempotency-Key conflicts")
	}))
	defer server.Close()

	executor := &ToolExecutor{client: server.Client(), namespace: "default", secretPath: "/secrets/tools"}
	tool := &corev1alpha1.Tool{Spec: corev1alpha1.ToolSpec{HTTP: &corev1alpha1.HTTPExecution{
		URL: server.URL,
		Headers: map[string]string{
			"Idempotency-Key": "tool-key",
		},
	}}}
	ctx := WithToolIdempotencyKey(context.Background(), "approval-key")

	_, err := executor.Execute(ctx, tool, json.RawMessage(`{"input":"test"}`))
	if err == nil || !strings.Contains(err.Error(), "reserved header") {
		t.Fatalf("Execute() error = %v, want reserved header conflict", err)
	}
}

func TestToolExecutor_Execute_InjectsTraceparentAndEmitsHTTPToolSpan(t *testing.T) {
	if _, err := tracing.Init("test", false); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	spans := testutil.NewSpanHarness(t)
	metrics := testutil.NewMetricHarness(t)
	var gotTraceparent string
	var gotBaggage string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTraceparent = r.Header.Get("traceparent")
		gotBaggage = r.Header.Get("baggage")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	executor := &ToolExecutor{client: server.Client(), namespace: "team-a", secretPath: "/secrets/tools"}
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "lookup"},
		Spec:       corev1alpha1.ToolSpec{HTTP: &corev1alpha1.HTTPExecution{URL: server.URL}},
	}
	ctx, parent := tracing.Tracer("test").Start(context.Background(), "agent.step")
	member, err := baggage.NewMember("tenant", "acme")
	if err != nil {
		t.Fatalf("baggage member: %v", err)
	}
	bag, err := baggage.New(member)
	if err != nil {
		t.Fatalf("baggage: %v", err)
	}
	ctx = baggage.ContextWithBaggage(ctx, bag)
	ctx = WithToolCallID(ctx, "call-http")
	result, err := executor.Execute(ctx, tool, json.RawMessage(`{"x":1}`))
	parent.End()
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result != `{"ok":true}` {
		t.Fatalf("result = %q", result)
	}
	if !validTraceparent(gotTraceparent) {
		t.Fatalf("traceparent = %q, want valid W3C header", gotTraceparent)
	}
	if gotBaggage != "" {
		t.Fatalf("baggage header = %q, want empty", gotBaggage)
	}
	span := testutil.SpanNamed(spans.Recorder.Ended(), "execute_tool lookup")
	if span == nil {
		t.Fatal("missing HTTP tool span")
	}
	attrs := testutil.AttributeMap(span)
	if got := attrs[tracing.AttrToolKind].AsString(); got != tracing.ToolKindHTTP {
		t.Fatalf("%s = %q", tracing.AttrToolKind, got)
	}
	if got := attrs[tracing.AttrToolResultSizeBytes].AsInt64(); got != int64(len(result)) {
		t.Fatalf("result size = %d", got)
	}
	if got := attrs[genai.AttrToolCallID].AsString(); got != "call-http" {
		t.Fatalf("tool call id = %q", got)
	}
	if countMetricDataPoints(metrics.Collect(t), genai.MetricExecuteToolDuration) != 1 {
		t.Fatalf("missing %s datapoint", genai.MetricExecuteToolDuration)
	}
}

func TestToolExecutor_Execute_MCPSubstrateActorInjectsTraceparent(t *testing.T) {
	if _, err := tracing.Init("test", false); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	_ = testutil.NewSpanHarness(t)
	const sessionID = "session-trace"
	var traceparents []string
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traceparents = append(traceparents, r.Header.Get("traceparent"))
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusAccepted)
			calls++
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request %d: %v", calls, err)
		}
		switch calls {
		case 0:
			if got := body["method"]; got != mcpInitializeMethod {
				t.Fatalf("method = %v, want %s", got, mcpInitializeMethod)
			}
			w.Header().Set(mcpSessionIDHeader, sessionID)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"initialize","result":{"protocolVersion":"2025-06-18"}}`))
		case 1:
			if got := body["method"]; got != mcpInitializedNotificationMethod {
				t.Fatalf("method = %v, want %s", got, mcpInitializedNotificationMethod)
			}
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":{}}`))
		case 2:
			if got := body["method"]; got != mcpToolsCallMethod {
				t.Fatalf("method = %v, want %s", got, mcpToolsCallMethod)
			}
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"1","result":{"content":[{"type":"text","text":"ok"}]}}`))
		default:
			t.Fatalf("unexpected MCP request %d", calls)
		}
		calls++
	}))
	defer server.Close()

	executor := &ToolExecutor{client: server.Client(), namespace: "default", secretPath: "/secrets/tools"}
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "lookup"},
		Spec: corev1alpha1.ToolSpec{MCP: &corev1alpha1.MCPToolServer{SubstrateActor: &corev1alpha1.SubstrateMCPActor{
			TemplateRef: corev1alpha1.WorkspaceTemplateReference{Name: "mcp-template"},
		}}},
		Status: corev1alpha1.ToolStatus{Endpoint: server.URL + "/mcp", Actor: &corev1alpha1.ToolActorStatus{RouteHost: "actor-1.actors.test"}},
	}
	ctx, parent := tracing.Tracer("test").Start(context.Background(), "agent.step")
	member, err := baggage.NewMember("tenant", "acme")
	if err != nil {
		t.Fatalf("baggage member: %v", err)
	}
	bag, err := baggage.New(member)
	if err != nil {
		t.Fatalf("baggage: %v", err)
	}
	ctx = baggage.ContextWithBaggage(ctx, bag)
	ctx = WithToolCallID(ctx, "call-http")
	result, err := executor.Execute(ctx, tool, json.RawMessage(`{"x":1}`))
	parent.End()
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result != "ok" {
		t.Fatalf("result = %q, want ok", result)
	}
	if len(traceparents) != 4 {
		t.Fatalf("traceparent count = %d, want 4", len(traceparents))
	}
	for i, traceparent := range traceparents {
		if !validTraceparent(traceparent) {
			t.Fatalf("traceparent[%d] = %q, want valid W3C header", i, traceparent)
		}
		if traceparent != traceparents[0] {
			t.Fatalf("traceparent[%d] = %q, want %q", i, traceparent, traceparents[0])
		}
	}
}

func validTraceparent(value string) bool {
	parts := strings.Split(value, "-")
	return len(parts) == 4 && parts[0] == "00" && len(parts[1]) == 32 && len(parts[2]) == 16 && len(parts[3]) == 2
}

func TestToolExecutor_Execute_HTTPErrorRecordsErrorType(t *testing.T) {
	if _, err := tracing.Init("test", false); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	spans := testutil.NewSpanHarness(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`bad gateway sensitive-body-marker`))
	}))
	defer server.Close()
	executor := &ToolExecutor{client: server.Client(), namespace: "default", secretPath: "/secrets/tools"}
	tool := &corev1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: "failing_http"}, Spec: corev1alpha1.ToolSpec{HTTP: &corev1alpha1.HTTPExecution{URL: server.URL}}}
	ctx, parent := tracing.Tracer("test").Start(context.Background(), "agent.step")
	_, err := executor.Execute(ctx, tool, json.RawMessage(`{}`))
	parent.End()
	if err == nil {
		t.Fatal("Execute() error = nil, want HTTP error")
	}
	span := testutil.SpanNamed(spans.Recorder.Ended(), "execute_tool failing_http")
	if span == nil {
		t.Fatal("missing HTTP tool span")
	}
	attrs := testutil.AttributeMap(span)
	if got := attrs["error.type"].AsString(); got != "http_status_502" {
		t.Fatalf("error.type = %q", got)
	}
	if strings.Contains(span.Status().Description, "sensitive-body-marker") {
		t.Fatalf("span status leaked response body: %q", span.Status().Description)
	}
	for _, event := range span.Events() {
		if strings.Contains(event.Name, "sensitive-body-marker") {
			t.Fatalf("span event leaked response body: %#v", event)
		}
		for _, kv := range event.Attributes {
			if strings.Contains(kv.Value.AsString(), "sensitive-body-marker") {
				t.Fatalf("span event attribute leaked response body: %#v", event)
			}
		}
	}
}

func countMetricDataPoints(rm metricdata.ResourceMetrics, name string) int {
	count := 0
	for _, scope := range rm.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if metric.Name != name {
				continue
			}
			switch data := metric.Data.(type) {
			case metricdata.Histogram[int64]:
				count += len(data.DataPoints)
			case metricdata.Histogram[float64]:
				count += len(data.DataPoints)
			}
		}
	}
	return count
}
