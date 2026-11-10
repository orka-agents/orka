/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/transactiontoken"
	"github.com/orka-agents/orka/internal/workerenv"
)

// useSubstrateMCPTestRouter points an executor's in-process router at a test
// server standing in for atenet-router.
func useSubstrateMCPTestRouter(t *testing.T, executor *ToolExecutor, serverURL string) {
	t.Helper()
	router, err := NewSubstrateMCPRouter(serverURL, executor.client)
	if err != nil {
		t.Fatalf("NewSubstrateMCPRouter() error = %v", err)
	}
	executor.SetSubstrateMCPRouter(router)
}

func substrateMCPTestTool() *corev1alpha1.Tool {
	const namespace = "tenant"
	return &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "lookup", Namespace: namespace, UID: "tool-uid", Generation: 2},
		Spec: corev1alpha1.ToolSpec{
			HTTP: &corev1alpha1.HTTPExecution{
				Headers:       map[string]string{"X-Static": "tool-static"},
				AuthSecretRef: &corev1alpha1.SecretKeySelector{Name: "mcp-auth", Key: "token"},
			},
			MCP: &corev1alpha1.MCPToolServer{SubstrateActor: &corev1alpha1.SubstrateMCPActor{
				TemplateRef: corev1alpha1.WorkspaceTemplateReference{Name: "mcp-template"},
			}},
		},
		Status: corev1alpha1.ToolStatus{
			Available: true,
			Endpoint:  SubstrateMCPProxyPath(namespace, "lookup"),
			Actor:     &corev1alpha1.ToolActorStatus{Provider: corev1alpha1.WorkspaceProviderSubstrate, ActorID: "actor-1.tools"},
			Conditions: []metav1.Condition{{
				Type: "Available", Status: metav1.ConditionTrue, ObservedGeneration: 2, Reason: "EndpointReachable",
			}},
		},
	}
}

type recordedMCPRequest struct {
	method string
	path   string
	query  string
	header http.Header
	body   map[string]any
}

func newRecordingMCPServer(t *testing.T) (*httptest.Server, func() []recordedMCPRequest) {
	t.Helper()
	var mu sync.Mutex
	var requests []recordedMCPRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record := recordedMCPRequest{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, header: r.Header.Clone()}
		if r.Method == http.MethodPost {
			_ = json.NewDecoder(r.Body).Decode(&record.body)
		}
		mu.Lock()
		requests = append(requests, record)
		mu.Unlock()
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch record.body["method"] {
		case mcpInitializeMethod:
			w.Header().Set(mcpSessionIDHeader, "session-1")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"initialize","result":{"protocolVersion":"2025-06-18"}}`))
		case mcpInitializedNotificationMethod:
			w.WriteHeader(http.StatusAccepted)
		default:
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"1","result":{"content":[{"type":"text","text":"proxied"}]}}`))
		}
	}))
	t.Cleanup(server.Close)
	return server, func() []recordedMCPRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]recordedMCPRequest(nil), requests...)
	}
}

func TestToolExecutorSubstrateMCPUsesAuthenticatedOrkaProxy(t *testing.T) {
	server, requests := newRecordingMCPServer(t)
	executor := &ToolExecutor{client: server.Client(), namespace: "tenant"}
	if err := executor.SetSubstrateMCPProxy(server.URL+"/", func() (string, error) { return "worker-sa-token", nil }); err != nil {
		t.Fatal(err)
	}
	executor.SetTransactionAuthority("task-txn-token", []string{"tools:call"})
	ctx := WithToolIdempotencyKey(context.Background(), "approval-1")

	result, err := executor.Execute(ctx, substrateMCPTestTool(), json.RawMessage(`{"q":"x"}`))
	if err != nil || result != "proxied" {
		t.Fatalf("Execute() = %q, %v", result, err)
	}
	got := requests()
	if len(got) != 4 {
		t.Fatalf("requests = %d, want initialize, initialized, tools/call and session DELETE", len(got))
	}
	for i, request := range got {
		if request.path != "/api/v1/tools/lookup/mcp" || request.query != "namespace=tenant" {
			t.Fatalf("request %d target = %s?%s", i, request.path, request.query)
		}
		if auth := request.header.Get("Authorization"); auth != "Bearer worker-sa-token" {
			t.Fatalf("request %d Authorization = %q, want the workload token", i, auth)
		}
		if got := request.header.Get(SubstrateMCPToolTxnTokenHeader); got != "task-txn-token" {
			t.Fatalf("request %d %s = %q", i, SubstrateMCPToolTxnTokenHeader, got)
		}
		// Every request, including session termination, names the Tool revision
		// whose credential authority the worker validated.
		tool := substrateMCPTestTool()
		if !SubstrateMCPToolBindingMatches(tool, request.header.Get(SubstrateMCPToolUIDHeader),
			request.header.Get(SubstrateMCPToolGenerationHeader), request.header.Get(SubstrateMCPToolActorHeader)) {
			t.Fatalf("request %d does not bind the validated Tool revision", i)
		}
		// The proxy resolves Tool credentials and routing; the worker never
		// sends them, and Txn-Token stays reserved for Orka API auth.
		for _, header := range []string{transactiontoken.HeaderName, "ate-target-actor", "X-Static"} {
			if value := request.header.Get(header); value != "" {
				t.Fatalf("request %d sent %s = %q", i, header, value)
			}
		}
	}
	if got[2].body["method"] != mcpToolsCallMethod || got[2].header.Get(toolIdempotencyKeyHeader) != "approval-1" {
		t.Fatalf("tools/call = %v idempotency=%q", got[2].body, got[2].header.Get(toolIdempotencyKeyHeader))
	}
	if got[0].header.Get(toolIdempotencyKeyHeader) != "" || got[3].method != http.MethodDelete || got[3].header.Get(mcpSessionIDHeader) != "session-1" {
		t.Fatalf("unexpected idempotency or session termination headers")
	}
}

func TestToolExecutorSubstrateMCPProxyKeepsTaskCredentialAuthority(t *testing.T) {
	server, requests := newRecordingMCPServer(t)
	executor := &ToolExecutor{client: server.Client(), namespace: "tenant"}
	if err := executor.SetSubstrateMCPProxy(server.URL, func() (string, error) { return "worker-sa-token", nil }); err != nil {
		t.Fatal(err)
	}
	executor.SetTransactionCredentialAuthority(true, false, "")
	if _, err := executor.Execute(context.Background(), substrateMCPTestTool(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("Execute() succeeded without credential authority")
	}
	if len(requests()) != 0 {
		t.Fatal("proxy was called without credential authority")
	}
}

func TestToolExecutorSubstrateMCPProxyDoesNotFollowRedirects(t *testing.T) {
	var redirected bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected = true
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	executor := &ToolExecutor{client: server.Client(), namespace: "tenant"}
	if err := executor.SetSubstrateMCPProxy(server.URL, func() (string, error) { return "worker-sa-token", nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := executor.Execute(context.Background(), substrateMCPTestTool(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("Execute() succeeded through a redirect")
	}
	if redirected {
		t.Fatal("workload token followed a redirect")
	}
}

func TestToolExecutorSubstrateMCPRouterInjectsToolCredentials(t *testing.T) {
	server, requests := newRecordingMCPServer(t)
	secretDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(secretDir, "mcp-auth"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secretDir, "mcp-auth", "token"), []byte("actor-credential"), 0o600); err != nil {
		t.Fatal(err)
	}
	executor := &ToolExecutor{client: server.Client(), namespace: "tenant", secretPath: secretDir}
	useSubstrateMCPTestRouter(t, executor, server.URL)
	if _, err := executor.Execute(context.Background(), substrateMCPTestTool(), json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	for i, request := range requests() {
		if request.path != SubstrateMCPDefaultPath || request.header.Get("ate-target-actor") != "tools/actor-1" {
			t.Fatalf("request %d path=%q target=%q", i, request.path, request.header.Get("ate-target-actor"))
		}
		if request.header.Get("Authorization") != "Bearer actor-credential" || request.header.Get("X-Static") != "tool-static" {
			t.Fatalf("request %d did not carry Tool credentials", i)
		}
	}
}

func TestToolExecutorSubstrateMCPFailsClosedWithoutTransport(t *testing.T) {
	t.Setenv(workerenv.ControllerURL, "")
	executor := NewToolExecutorForNamespace("tenant", nil, nil)
	_, err := executor.Execute(context.Background(), substrateMCPTestTool(), json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "Orka API MCP proxy") {
		t.Fatalf("Execute() error = %v, want missing proxy error", err)
	}
}

func TestToolExecutorSubstrateMCPRejectsReservedToolHeaders(t *testing.T) {
	server, requests := newRecordingMCPServer(t)
	for _, header := range []string{"ate-target-actor", "Host", "Txn-Token", "Mcp-Session-Id", "Idempotency-Key", "X-Orka-Tool-Txn-Token", "Proxy-Authorization"} {
		t.Run(header, func(t *testing.T) {
			executor := &ToolExecutor{client: server.Client(), namespace: "tenant"}
			useSubstrateMCPTestRouter(t, executor, server.URL)
			tool := substrateMCPTestTool()
			tool.Spec.HTTP.AuthSecretRef = nil
			tool.Spec.HTTP.Headers = map[string]string{header: "other/actor"}
			if _, err := executor.Execute(context.Background(), tool, json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "reserved header") {
				t.Fatalf("Execute() error = %v, want reserved header rejection", err)
			}
		})
	}
	if len(requests()) != 0 {
		t.Fatal("actor was called with a reserved Tool header")
	}
}

func TestSubstrateMCPActorTargetComesOnlyFromStatus(t *testing.T) {
	tool := substrateMCPTestTool()
	if target, err := SubstrateMCPActorTarget(tool); err != nil || target != "tools/actor-1" {
		t.Fatalf("SubstrateMCPActorTarget() = %q, %v", target, err)
	}
	for name, mutate := range map[string]func(*corev1alpha1.Tool){
		"missing actor":     func(tool *corev1alpha1.Tool) { tool.Status.Actor = nil },
		"unqualified actor": func(tool *corev1alpha1.Tool) { tool.Status.Actor.ActorID = "actor-1" },
		"other provider":    func(tool *corev1alpha1.Tool) { tool.Status.Actor.Provider = "other" },
		"invalid actor":     func(tool *corev1alpha1.Tool) { tool.Status.Actor.ActorID = "Actor/1.tools" },
		"not substrate":     func(tool *corev1alpha1.Tool) { tool.Spec.MCP = nil },
	} {
		t.Run(name, func(t *testing.T) {
			tool := substrateMCPTestTool()
			mutate(tool)
			if target, err := SubstrateMCPActorTarget(tool); err == nil {
				t.Fatalf("SubstrateMCPActorTarget() = %q, want error", target)
			}
		})
	}
}

func TestSubstrateMCPActorPathValidation(t *testing.T) {
	for path, valid := range map[string]bool{
		"": true, "/mcp": true, "/v1/mcp": true,
		"mcp": false, "//evil/mcp": false, "/mcp?x=1": false, "/mcp#x": false, "/a/../mcp": false, "/a/./mcp": false,
	} {
		tool := substrateMCPTestTool()
		tool.Spec.MCP.Path = path
		if _, err := SubstrateMCPActorPath(tool); (err == nil) != valid {
			t.Fatalf("SubstrateMCPActorPath(%q) error = %v, valid=%t", path, err, valid)
		}
	}
}

func TestSubstrateMCPProxyPathEscapesNames(t *testing.T) {
	got := SubstrateMCPProxyPath("team a", "tool/x")
	parsed, err := neturl.Parse(got)
	if err != nil || parsed.EscapedPath() != "/api/v1/tools/tool%2Fx/mcp" || parsed.Query().Get("namespace") != "team a" {
		t.Fatalf("SubstrateMCPProxyPath() = %q", got)
	}
}

func TestSubstrateMCPToolBindingRequiresExactRevision(t *testing.T) {
	tool := substrateMCPTestTool()
	tool.UID, tool.Generation = "tool-uid", 4
	header := http.Header{}
	SetSubstrateMCPToolBinding(header, tool)
	match := func(candidate *corev1alpha1.Tool) bool {
		return SubstrateMCPToolBindingMatches(candidate, header.Get(SubstrateMCPToolUIDHeader),
			header.Get(SubstrateMCPToolGenerationHeader), header.Get(SubstrateMCPToolActorHeader))
	}
	if !match(tool) {
		t.Fatal("binding does not match its own Tool")
	}
	for name, mutate := range map[string]func(*corev1alpha1.Tool){
		"recreated": func(t *corev1alpha1.Tool) { t.UID = "other-uid" },
		"spec":      func(t *corev1alpha1.Tool) { t.Generation++ },
		"actor":     func(t *corev1alpha1.Tool) { t.Status.Actor.ActorID = "actor-2.tools" },
		"no actor":  func(t *corev1alpha1.Tool) { t.Status.Actor = nil },
		"no uid":    func(t *corev1alpha1.Tool) { t.UID = "" },
	} {
		candidate := tool.DeepCopy()
		mutate(candidate)
		if match(candidate) {
			t.Fatalf("%s: binding matched a changed Tool", name)
		}
	}
}

func TestSubstrateMCPRequiresReadinessForCurrentToolGeneration(t *testing.T) {
	server, requests := newRecordingMCPServer(t)
	executor := &ToolExecutor{client: server.Client(), namespace: "tenant"}
	if err := executor.SetSubstrateMCPProxy(server.URL, func() (string, error) { return "worker-sa-token", nil }); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*corev1alpha1.Tool){
		// A spec update keeps the previous Actor in status until reconciliation.
		"stale generation": func(tool *corev1alpha1.Tool) { tool.Generation++ },
		"not available":    func(tool *corev1alpha1.Tool) { tool.Status.Conditions[0].Status = metav1.ConditionFalse },
		"no condition":     func(tool *corev1alpha1.Tool) { tool.Status.Conditions = nil },
	} {
		tool := substrateMCPTestTool()
		mutate(tool)
		if SubstrateMCPToolReadyForCurrentGeneration(tool) {
			t.Fatalf("%s: Tool reported ready", name)
		}
		if _, err := executor.Execute(context.Background(), tool, json.RawMessage(`{}`)); err == nil {
			t.Fatalf("%s: Execute() sent a request for an unreconciled Tool", name)
		}
	}
	if got := requests(); len(got) != 0 {
		t.Fatalf("unreconciled Tools reached the proxy %d times", len(got))
	}
}

// The manager's router client is shared and has no timeout. A call through it
// must still be bounded: by the Tool's timeout when it sets one, otherwise by
// the executor's default.
func TestToolExecutorSubstrateMCPRouterCallsHaveATimeout(t *testing.T) {
	for name, tc := range map[string]struct {
		timeout *metav1.Duration
		want    time.Duration
	}{
		"tool sets none": {want: defaultToolHTTPTimeout},
		"tool sets one":  {timeout: &metav1.Duration{Duration: 5 * time.Second}, want: 5 * time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			shared := &http.Client{}
			router, err := NewSubstrateMCPRouter("https://router.test", shared)
			if err != nil {
				t.Fatal(err)
			}
			executor := &ToolExecutor{client: &http.Client{}, namespace: "tenant"}
			executor.SetSubstrateMCPRouter(router)
			tool := substrateMCPTestTool()
			tool.Spec.HTTP.AuthSecretRef = nil
			tool.Spec.HTTP.Timeout = tc.timeout
			prepared, err := executor.prepareSubstrateMCPRequest(context.Background(), tool, map[string]any{"q": "x"})
			if err != nil {
				t.Fatalf("prepareSubstrateMCPRequest() error = %v", err)
			}
			client, err := toolHTTPClient(prepared.baseClient, prepared.httpConfig.Timeout, prepared.gatewayTLS, prepared.gateway, prepared.connection)
			if err != nil {
				t.Fatal(err)
			}
			if client.Timeout != tc.want {
				t.Fatalf("call timeout = %s, want %s", client.Timeout, tc.want)
			}
			if shared.Timeout != 0 {
				t.Fatalf("shared router client timeout = %s, want it left alone", shared.Timeout)
			}
		})
	}
}
