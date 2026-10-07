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
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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
	got, err := connectorBackedTools(context.Background(), c, "default", customTools)
	if err != nil {
		t.Fatal(err)
	}
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
	named := &corev1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: tool}}
	return executeConnectorToolViaController(context.Background(), s.server.Client(), named, args, callID, key)
}

func TestConnectorToolProxyTimeoutFollowsTheTool(t *testing.T) {
	if got := connectorToolProxyTimeout(nil); got != connectorToolCallTimeout {
		t.Fatalf("no tool = %v, want the default", got)
	}
	slow := &corev1alpha1.Tool{Spec: corev1alpha1.ToolSpec{HTTP: &corev1alpha1.HTTPExecution{
		Timeout: &metav1.Duration{Duration: 8 * time.Minute},
	}}}
	if got := connectorToolProxyTimeout(slow); got != 8*time.Minute+connectorToolSettlementMargin {
		t.Fatalf("slow tool = %v, want the tool timeout plus the settlement margin", got)
	}
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
		`"uid":"conn-uid","generation":2,"grantSequence":1,"mode":"readOnly"},` +
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

func TestExecuteConnectorToolTransportFailureIsNotAttempted(t *testing.T) {
	stub := newConnectorControllerStub(t)
	// The controller is unreachable: nothing was executed and the local
	// approval must not be consumed, so an exact retry can go back through
	// the controller's claim and effect ledger.
	stub.server.Close()
	_, err := stub.call("gh_search", json.RawMessage(`{"q":"x"}`), "call-1", "ap-1")
	if err == nil || worker.ToolRequestWasAttempted(err) {
		t.Fatalf("err = %v, want a non-attempted transport failure", err)
	}
}

func TestConnectorBackedToolsHonorFrozenDigestsAsUpperBound(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	// The policy moved out of connection mode after dispatch; the frozen
	// digests still route the tool to the controller, which refuses it.
	direct := &corev1alpha1.OutboundAccessPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "github-conn", Namespace: "default"},
		Spec:       corev1alpha1.OutboundAccessPolicySpec{Direct: &corev1alpha1.DirectOutboundAccess{}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(direct).Build()
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "gh_search", Namespace: "default"},
		Spec: corev1alpha1.ToolSpec{HTTP: &corev1alpha1.HTTPExecution{
			URL:                     "https://api.github.example.test/search",
			OutboundAccessPolicyRef: &corev1alpha1.LocalObjectReference{Name: "github-conn"},
		}},
	}
	t.Setenv(workerenv.ConnectorToolDigests, `{"gh_search":"digest-a"}`)
	got, err := connectorBackedTools(context.Background(), c, "default", map[string]*corev1alpha1.Tool{"gh_search": tool})
	if err != nil {
		t.Fatal(err)
	}
	if !got["gh_search"] {
		t.Fatalf("a tool frozen as connector-backed must stay routed to the controller: %v", got)
	}
	t.Setenv(workerenv.ConnectorToolDigests, "")
	got, err = connectorBackedTools(context.Background(), c, "default", map[string]*corev1alpha1.Tool{"gh_search": tool})
	if err != nil {
		t.Fatal(err)
	}
	if got["gh_search"] {
		t.Fatalf("without a frozen digest the live direct policy runs locally: %v", got)
	}
}

// A transient policy read failure at startup is retried and then fails the
// worker, instead of marking the tool connector-backed without the policy
// spec its approval target needs.
func TestConnectorBackedToolsRetriesThenFailsOnPolicyReadErrors(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	policy := connectorTestPolicy("github-conn", corev1alpha1.OutboundAccessPolicySpec{
		Connection: &corev1alpha1.ConnectionOutboundAccess{ProviderRef: corev1alpha1.LocalObjectReference{Name: "github"}},
	})
	var reads atomic.Int32
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(policy).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(
			ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption,
		) error {
			if _, ok := obj.(*corev1alpha1.OutboundAccessPolicy); ok && reads.Add(1) < 3 {
				return errors.New("transient")
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	}).Build()
	previous := connectorPolicyReadBackoff
	connectorPolicyReadBackoff = time.Millisecond
	t.Cleanup(func() { connectorPolicyReadBackoff = previous })
	tools := map[string]*corev1alpha1.Tool{"gh_search": connectorTestTool("gh_search", "github-conn")}
	got, err := connectorBackedTools(context.Background(), c, "default", tools)
	if err != nil || !got["gh_search"] || connectorToolPolicies["gh_search"].Connection == nil {
		t.Fatalf("after two transient failures: got %v err = %v policy = %+v", got, err, connectorToolPolicies["gh_search"])
	}
	reads.Store(-1000)
	if _, err := connectorBackedTools(context.Background(), c, "default", tools); err == nil {
		t.Fatal("a policy that stays unreadable must fail startup")
	}
}

// A Tool the Job dispatched as connector-backed is never silently dropped
// at startup: a transient read is retried, and one that keeps failing fails
// the worker instead of running with a reduced tool set.
func TestLoadCustomToolsKeepsFrozenConnectorTools(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	tool := connectorTestTool("gh_search", "github-conn")
	policy := connectorTestPolicy("github-conn", corev1alpha1.OutboundAccessPolicySpec{
		Connection: &corev1alpha1.ConnectionOutboundAccess{ProviderRef: corev1alpha1.LocalObjectReference{Name: "github"}},
	})
	var reads atomic.Int32
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tool, policy).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(
			ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption,
		) error {
			if _, ok := obj.(*corev1alpha1.Tool); ok && reads.Add(1) < 3 {
				return errors.New("transient")
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	}).Build()
	previous := connectorPolicyReadBackoff
	connectorPolicyReadBackoff = time.Millisecond
	t.Cleanup(func() { connectorPolicyReadBackoff = previous })
	t.Setenv(workerenv.ConnectorToolDigests, `{"gh_search":"digest-a"}`)
	loaded, err := loadCustomTools(context.Background(), c, "default", []string{"gh_search"})
	if err != nil || loaded["gh_search"] == nil {
		t.Fatalf("frozen tool after transient failures: %v err = %v", loaded, err)
	}
	reads.Store(-1000)
	if _, err := loadCustomTools(context.Background(), c, "default", []string{"gh_search"}); err == nil {
		t.Fatal("a frozen connector tool that stays unreadable must fail startup")
	}
	// A frozen tool that was deleted after dispatch fails startup as well.
	reads.Store(0)
	t.Setenv(workerenv.ConnectorToolDigests, `{"gh_missing":"digest-b"}`)
	if _, err := loadCustomTools(context.Background(), c, "default", []string{"gh_missing"}); err == nil {
		t.Fatal("a frozen connector tool that no longer exists must fail startup")
	}
	reads.Store(-1000)
	// A tool the Job did not freeze is still skipped with a warning.
	t.Setenv(workerenv.ConnectorToolDigests, "")
	loaded, err = loadCustomTools(context.Background(), c, "default", []string{"gh_search"})
	if err != nil || loaded["gh_search"] != nil {
		t.Fatalf("unfrozen tool: %v err = %v, want skipped", loaded, err)
	}
}

// TestExecuteConnectorToolDoesNotFollowRedirectsOrProxies covers the default
// client: a redirect from the controller URL is not followed (the worker's
// ServiceAccount token never reaches another host), and inherited proxy
// settings are ignored.
func TestExecuteConnectorToolDoesNotFollowRedirectsOrProxies(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.Add(1)
		_, _ = w.Write([]byte(`{"result":"leaked"}`))
	}))
	defer other.Close()
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer controller.Close()
	t.Setenv(workerenv.ControllerURL, controller.URL)
	t.Setenv(workerenv.TaskNamespace, "default")
	t.Setenv(workerenv.TaskName, "task-a")
	t.Setenv(workerenv.ServiceAccountTokenPath, "")
	t.Setenv(workerenv.ServiceAccountToken, "sa-token")
	tool := &corev1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: "gh_search"}}
	if _, err := executeConnectorToolViaController(context.Background(), nil, tool, nil, "", ""); err == nil {
		t.Fatal("a redirect from the controller must not succeed")
	}
	if elsewhere.Load() != 0 {
		t.Fatalf("the redirect target was called %d times; the token must stay with the controller", elsewhere.Load())
	}
	httpClient := controllerHTTPClient()
	if transport, ok := httpClient.Transport.(*http.Transport); !ok || transport.Proxy != nil {
		t.Fatalf("transport = %#v, want inherited proxies disabled", httpClient.Transport)
	}
}

// TestConnectorApprovalTargetTreatsEmptyArgumentsAsTheEmptyObject covers an
// approval-required connector tool called with no arguments: the controller
// executes and digests the call as `{}`, so the worker's approval target is
// built from the same bytes and the approval matches.
func TestConnectorApprovalTargetTreatsEmptyArgumentsAsTheEmptyObject(t *testing.T) {
	connector := &corev1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{
		Name: "gh_write", Annotations: map[string]string{connectorBackedToolAnnotation: connectorBackedToolValue},
	}}
	for _, empty := range []json.RawMessage{nil, json.RawMessage(""), json.RawMessage("  ")} {
		got, err := approvalTargetArguments(empty, connector)
		if err != nil || string(got) != "{}" {
			t.Fatalf("connector target args for %q = %q err = %v, want {}", empty, got, err)
		}
	}
	// A tool the worker executes itself keeps its existing target.
	local := &corev1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: "local"}}
	if got, err := approvalTargetArguments(nil, local); err != nil || len(got) != 0 {
		t.Fatalf("local target args = %q err = %v, want unchanged", got, err)
	}
}

// TestDecodeConnectorToolResponseReadsTheErrorEnvelope covers the API's
// shared error envelope: the worker shows the controller's message rather
// than the raw JSON body, and a 502 is still reported as attempted.
func TestDecodeConnectorToolResponseReadsTheErrorEnvelope(t *testing.T) {
	envelope := []byte(`{"error":{"code":502,"message":"connector tool \"gh_write\": upstream refused"}}`)
	_, err := decodeConnectorToolResponse("gh_write", http.StatusBadGateway, envelope)
	var attemptedErr worker.ToolRequestAttemptedError
	if err == nil || !errors.As(err, &attemptedErr) ||
		!strings.Contains(err.Error(), "upstream refused") || strings.Contains(err.Error(), `"code"`) {
		t.Fatalf("envelope 502 err = %v, want the attempted message", err)
	}
	_, err = decodeConnectorToolResponse("gh_write", http.StatusFailedDependency, []byte(`{"error":"no connection"}`))
	if err == nil || errors.As(err, &attemptedErr) || !strings.Contains(err.Error(), "no connection") {
		t.Fatalf("flat 424 err = %v, want the unattempted message", err)
	}
}
