/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/worker"
	"github.com/orka-agents/orka/internal/workerenv"
)

func connectorTestPolicy(name string, spec corev1alpha1.OutboundAccessPolicySpec) *corev1alpha1.OutboundAccessPolicy {
	return &corev1alpha1.OutboundAccessPolicy{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Spec: spec}
}

func connectorTestTool(name, policy string) *corev1alpha1.Tool {
	spec := corev1alpha1.ToolSpec{HTTP: &corev1alpha1.HTTPExecution{URL: "https://api.example.test"}}
	if policy != "" {
		spec.HTTP.OutboundAccessPolicyRef = &corev1alpha1.LocalObjectReference{Name: policy}
	}
	return &corev1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Spec: spec}
}

func TestConnectorBackedTools(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	connectionPolicy := connectorTestPolicy("github-conn", corev1alpha1.OutboundAccessPolicySpec{
		Connection: &corev1alpha1.ConnectionOutboundAccess{ProviderRef: corev1alpha1.LocalObjectReference{Name: "github"}},
	})
	directPolicy := connectorTestPolicy("direct", corev1alpha1.OutboundAccessPolicySpec{
		Direct: &corev1alpha1.DirectOutboundAccess{},
	})
	customTools := map[string]*corev1alpha1.Tool{
		"gh_search":  connectorTestTool("gh_search", "github-conn"),
		"direct":     connectorTestTool("direct", "direct"),
		"unreadable": connectorTestTool("unreadable", "missing-policy"),
		"plain":      connectorTestTool("plain", ""),
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(connectionPolicy, directPolicy).Build()
	got := connectorBackedTools(context.Background(), c, "default", customTools)
	if !got["gh_search"] || got["direct"] || got["plain"] {
		t.Fatalf("connector-backed = %v", got)
	}
	if !got["unreadable"] {
		t.Fatal("a tool whose policy cannot be read must be treated as connector-backed")
	}
}

type connectorControllerStub struct {
	server    *httptest.Server
	responses map[string]func(http.ResponseWriter)
	path      string
	auth      string
	body      connectorToolRequest
}

func newConnectorControllerStub(t *testing.T) *connectorControllerStub {
	t.Helper()
	stub := &connectorControllerStub{responses: map[string]func(http.ResponseWriter){}}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.path = r.URL.Path
		stub.auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&stub.body)
		w.Header().Set("Content-Type", "application/json")
		if respond, ok := stub.responses[r.URL.Path]; ok {
			respond(w)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"result": "42 open pull requests"})
	}))
	t.Cleanup(stub.server.Close)
	t.Setenv(workerenv.ControllerURL, stub.server.URL)
	t.Setenv(workerenv.TaskNamespace, "default")
	t.Setenv(workerenv.TaskName, "task-a")
	t.Setenv(workerenv.ServiceAccountTokenPath, "")
	t.Setenv(workerenv.ServiceAccountToken, "sa-token")
	return stub
}

func (s *connectorControllerStub) respond(tool string, status int, payload map[string]string) {
	s.responses["/internal/v1/tasks/default/task-a/connector-tools/"+tool] = func(w http.ResponseWriter) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(payload)
	}
}

func (s *connectorControllerStub) call(tool string, args json.RawMessage, callID, key string) (string, error) {
	return executeConnectorToolViaController(context.Background(), s.server.Client(), tool, args, callID, key)
}

func TestExecuteConnectorToolViaController(t *testing.T) {
	stub := newConnectorControllerStub(t)

	result, err := stub.call("gh_search", json.RawMessage(`{"q":"orka"}`), "call-1", "idem-1")
	if err != nil {
		t.Fatal(err)
	}
	wantPath := "/internal/v1/tasks/default/task-a/connector-tools/gh_search"
	if result != "42 open pull requests" || stub.path != wantPath || stub.auth != "Bearer sa-token" {
		t.Fatalf("result = %q path = %q auth = %q", result, stub.path, stub.auth)
	}
	if string(stub.body.Arguments) != `{"q":"orka"}` || stub.body.CallID != "call-1" ||
		stub.body.IdempotencyKey != "idem-1" {
		t.Fatalf("request body = %+v", stub.body)
	}

	stub.respond("gh_write", http.StatusBadGateway, map[string]string{"error": "provider returned 500"})
	_, err = stub.call("gh_write", nil, "", "")
	if err == nil || !strings.Contains(err.Error(), "provider returned 500") || !worker.ToolRequestWasAttempted(err) {
		t.Fatalf("502 err = %v, want attempted execution error", err)
	}
	if _, ok := errors.AsType[worker.ToolExecutionError](err); !ok {
		t.Fatalf("502 must unwrap to ToolExecutionError: %v", err)
	}
	if string(stub.body.Arguments) != `{}` {
		t.Fatalf("nil arguments must be sent as an empty object, got %s", stub.body.Arguments)
	}

	stub.respond("gh_denied", http.StatusFailedDependency, map[string]string{"error": "the requester has no connection"})
	_, err = stub.call("gh_denied", nil, "", "")
	if err == nil || !strings.Contains(err.Error(), "no connection") || worker.ToolRequestWasAttempted(err) {
		t.Fatalf("424 err = %v, want a non-attempted precondition failure", err)
	}

	stub.responses["/internal/v1/tasks/default/task-a/connector-tools/gh_html"] = func(w http.ResponseWriter) {
		_, _ = w.Write([]byte("<html>"))
	}
	if _, err := stub.call("gh_html", nil, "", ""); err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("html err = %v", err)
	}

	t.Setenv(workerenv.ServiceAccountToken, "")
	_, err = stub.call("gh_search", nil, "", "")
	if err == nil || !strings.Contains(err.Error(), "service account token") {
		t.Fatalf("missing token err = %v", err)
	}
	t.Setenv(workerenv.ControllerURL, "")
	if _, err := stub.call("gh_search", nil, "", ""); err == nil || !strings.Contains(err.Error(), "controller URL") {
		t.Fatalf("missing controller err = %v", err)
	}
}

func TestParseConnectionBindings(t *testing.T) {
	if got := parseConnectionBindings(""); len(got) != 0 {
		t.Fatalf("empty env = %+v, want no bindings", got)
	}
	if got := parseConnectionBindings("{not json"); len(got) != 0 {
		t.Fatalf("unreadable env = %+v, want no bindings (fail closed at the controller)", got)
	}
	got := parseConnectionBindings(`[` +
		`{"policyName":"github-conn","provider":"github","connectionName":"github-abc",` +
		`"uid":"conn-uid","generation":2,"mode":"readOnly"},` +
		`{"policyName":"","uid":"ignored"}]`)
	if len(got) != 1 || got["github-conn"].UID != "conn-uid" || got["github-conn"].Generation != 2 {
		t.Fatalf("bindings = %+v", got)
	}
}

func TestExecuteConnectorToolRetriesUnavailableController(t *testing.T) {
	stub := newConnectorControllerStub(t)
	previous := connectorToolRetryBackoff
	connectorToolRetryBackoff = time.Millisecond
	t.Cleanup(func() { connectorToolRetryBackoff = previous })
	var calls int
	stub.responses["/internal/v1/tasks/default/task-a/connector-tools/gh_search"] = func(w http.ResponseWriter) {
		calls++
		if calls < 3 {
			// The Task's Job identity is not published yet.
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "task job identity is not yet published"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"result": "ready now"})
	}
	result, err := stub.call("gh_search", json.RawMessage(`{"q":"x"}`), "call-1", "")
	if err != nil || result != "ready now" || calls != 3 {
		t.Fatalf("result = %q err = %v calls = %d, want the call retried until the controller could judge it",
			result, err, calls)
	}

	calls = 0
	stub.responses["/internal/v1/tasks/default/task-a/connector-tools/gh_search"] = func(w http.ResponseWriter) {
		calls++
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "still unavailable"})
	}
	_, err = stub.call("gh_search", json.RawMessage(`{"q":"x"}`), "call-2", "")
	if err == nil || calls != connectorToolUnavailableRetries+1 {
		t.Fatalf("err = %v calls = %d, want a bounded number of attempts", err, calls)
	}
}

func TestExecuteConnectorToolReportsOversizedControllerResponse(t *testing.T) {
	stub := newConnectorControllerStub(t)
	stub.responses["/internal/v1/tasks/default/task-a/connector-tools/gh_search"] = func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":"`))
		chunk := bytes.Repeat([]byte("a"), 1<<20)
		for written := 0; written <= connectorToolResponseLimit; written += len(chunk) {
			_, _ = w.Write(chunk)
		}
		_, _ = w.Write([]byte(`"}`))
	}
	_, err := stub.call("gh_search", json.RawMessage(`{"q":"x"}`), "call-1", "")
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("err = %v, want an explicit size error rather than a truncated document", err)
	}
	if !worker.ToolRequestWasAttempted(err) {
		t.Fatal("a response that arrived means the request was attempted")
	}
}
