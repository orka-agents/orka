/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"
	"unicode/utf16"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	workerexecutor "github.com/orka-agents/orka/internal/worker"
)

type substrateMCPProxyFixture struct {
	server    *Server
	kube      client.Client
	clientset *kubefake.Clientset
	user      authenticationv1.UserInfo

	mu       sync.Mutex
	reviews  []authorizationv1.ResourceAttributes
	deny     func(authorizationv1.ResourceAttributes) bool
	upstream []*http.Request
	bodies   []string
	// respond replaces the Actor's default response when set.
	respond http.HandlerFunc
}

// substrateMCPProxyToolCall is the tools/call the fixture Tool admits.
const substrateMCPProxyToolCall = `{"jsonrpc":"2.0","id":"1","method":"tools/call","params":{"name":"lookup","arguments":{"q":"x"}}}`

func substrateMCPProxyTool() *corev1alpha1.Tool {
	return &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "lookup", Namespace: "default", UID: "tool-uid", Generation: 3},
		Spec: corev1alpha1.ToolSpec{
			Description: "lookup",
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
			Endpoint:  workerexecutor.SubstrateMCPProxyPath("default", "lookup"),
			Actor:     &corev1alpha1.ToolActorStatus{Provider: corev1alpha1.WorkspaceProviderSubstrate, ActorID: "actor-1.tools"},
			Conditions: []metav1.Condition{{
				Type: "Available", Status: metav1.ConditionTrue, ObservedGeneration: 3, Reason: "EndpointReachable",
			}},
		},
	}
}

func newSubstrateMCPProxyFixture(t *testing.T, objects ...client.Object) *substrateMCPProxyFixture {
	t.Helper()
	f := &substrateMCPProxyFixture{user: authenticationv1.UserInfo{
		Username: "system:serviceaccount:default:ai-worker", UID: "worker-uid",
		Groups: []string{"system:serviceaccounts", "system:serviceaccounts:default"},
	}}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.upstream = append(f.upstream, r.Clone(context.Background()))
		f.bodies = append(f.bodies, string(body))
		respond := f.respond
		f.mu.Unlock()
		if respond != nil {
			respond(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "session-1")
		w.Header().Set("Set-Cookie", "actor=secret")
		w.Header().Set("X-Actor-Internal", "leak")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":"1","result":{}}`)
	}))
	t.Cleanup(upstream.Close)

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1alpha1.AddToScheme, corev1.AddToScheme, authenticationv1.AddToScheme} {
		require.NoError(t, add(scheme))
	}
	if len(objects) == 0 {
		objects = []client.Object{substrateMCPProxyTool()}
	}
	objects = append(objects, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "mcp-auth", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte("actor-credential")},
	})
	f.kube = fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if review, ok := obj.(*authenticationv1.TokenReview); ok {
				review.Status = authenticationv1.TokenReviewStatus{Authenticated: true, User: f.user}
				return nil
			}
			return c.Create(ctx, obj, opts...)
		},
	}).Build()
	f.clientset = kubefake.NewClientset()
	f.clientset.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview)
		attributes := *review.Spec.ResourceAttributes
		f.mu.Lock()
		f.reviews = append(f.reviews, attributes)
		deny := f.deny
		f.mu.Unlock()
		review.Status.Allowed = deny == nil || !deny(attributes)
		return true, review, nil
	})
	router, err := workerexecutor.NewSubstrateMCPRouter(upstream.URL, upstream.Client())
	require.NoError(t, err)
	f.server = NewServer(f.kube, nil, ServerConfig{
		EnforceNamespaceIsolation: true, Clientset: f.clientset, APIReader: f.kube,
		SubstrateMCPProxy: SubstrateMCPProxyConfig{Router: router},
	})
	return f
}

func (f *substrateMCPProxyFixture) do(t *testing.T, method, path string, header http.Header, body string) *http.Response {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	for key, values := range header {
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}
	if request.Header.Get("Authorization") == "" {
		request.Header.Set("Authorization", "Bearer worker-sa-token-"+t.Name())
	}
	if request.Header.Get(workerexecutor.SubstrateMCPToolUIDHeader) == "" {
		workerexecutor.SetSubstrateMCPToolBinding(request.Header, substrateMCPProxyTool())
	}
	response, err := f.server.app.Test(request, fiber.TestConfig{Timeout: 10 * time.Second, FailOnTimeout: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = response.Body.Close() })
	return response
}

func (f *substrateMCPProxyFixture) upstreamCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.upstream)
}

func TestSubstrateMCPProxyForwardsOnlySafeHeadersToStatusTarget(t *testing.T) {
	f := newSubstrateMCPProxyFixture(t)
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	header.Set("Accept", "application/json, text/event-stream")
	header.Set("MCP-Protocol-Version", "2025-06-18")
	header.Set("Mcp-Session-Id", "session-1")
	header.Set("Idempotency-Key", "approval-1")
	header.Set("Cookie", "orka=session")
	header.Set("X-Forwarded-For", "10.0.0.1")
	header.Set("ate-target-actor", "victims/other-actor")
	header.Set("X-Static", "caller-override")
	header.Set(workerexecutor.SubstrateMCPToolTxnTokenHeader, "task-txn-token")
	response := f.do(t, http.MethodPost, "/api/v1/tools/lookup/mcp?namespace=default", header, substrateMCPProxyToolCall)
	body, _ := io.ReadAll(response.Body)
	require.Equal(t, http.StatusOK, response.StatusCode, string(body))
	require.JSONEq(t, `{"jsonrpc":"2.0","id":"1","result":{}}`, string(body))
	require.Equal(t, "session-1", response.Header.Get("Mcp-Session-Id"))
	require.Empty(t, response.Header.Get("Set-Cookie"))
	require.Empty(t, response.Header.Get("X-Actor-Internal"))

	require.Equal(t, 1, f.upstreamCalls())
	upstream := f.upstream[0]
	require.Equal(t, "/mcp", upstream.URL.Path)
	require.Empty(t, upstream.URL.RawQuery)
	require.Equal(t, "tools/actor-1", upstream.Header.Get("ate-target-actor"), "target must come from Tool status")
	require.Equal(t, "Bearer actor-credential", upstream.Header.Get("Authorization"), "caller credential must never reach the actor")
	require.Equal(t, "task-txn-token", upstream.Header.Get("Txn-Token"))
	require.Equal(t, "tool-static", upstream.Header.Get("X-Static"))
	require.Equal(t, "session-1", upstream.Header.Get("Mcp-Session-Id"))
	require.Equal(t, "approval-1", upstream.Header.Get("Idempotency-Key"))
	require.Equal(t, "2025-06-18", upstream.Header.Get("MCP-Protocol-Version"))
	for _, dropped := range []string{"Cookie", "X-Forwarded-For", workerexecutor.SubstrateMCPToolTxnTokenHeader} {
		require.Empty(t, upstream.Header.Get(dropped), dropped)
	}
	require.JSONEq(t, substrateMCPProxyToolCall, f.bodies[0])
	require.ElementsMatch(t, []authorizationv1.ResourceAttributes{
		{Namespace: "default", Verb: "get", Group: corev1alpha1.GroupVersion.Group, Resource: "tools", Name: "lookup"},
		{Namespace: "default", Verb: "get", Resource: "secrets", Name: "mcp-auth"},
	}, f.reviews)
}

func TestSubstrateMCPProxyDeleteForwardsSessionTermination(t *testing.T) {
	f := newSubstrateMCPProxyFixture(t)
	header := http.Header{}
	header.Set("Mcp-Session-Id", "session-1")
	response := f.do(t, http.MethodDelete, "/api/v1/tools/lookup/mcp?namespace=default", header, "")
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, 1, f.upstreamCalls())
	require.Equal(t, http.MethodDelete, f.upstream[0].Method)
	require.Equal(t, "session-1", f.upstream[0].Header.Get("Mcp-Session-Id"))
}

func TestSubstrateMCPProxyDeniesBeforeContactingActor(t *testing.T) {
	notAvailable := substrateMCPProxyTool()
	notAvailable.Status.Available = false
	// Status still names the previous Actor after a spec update.
	staleGeneration := substrateMCPProxyTool()
	staleGeneration.Generation = 4
	notSubstrate := substrateMCPProxyTool()
	notSubstrate.Spec.MCP = nil
	notSubstrate.Spec.HTTP.URL = "https://example.com"
	unqualified := substrateMCPProxyTool()
	unqualified.Status.Actor.ActorID = "actor-1"
	withPolicy := substrateMCPProxyTool()
	withPolicy.Spec.HTTP.OutboundAccessPolicyRef = &corev1alpha1.LocalObjectReference{Name: "policy"}
	reservedHeader := substrateMCPProxyTool()
	reservedHeader.Spec.HTTP.Headers = map[string]string{"ate-target-actor": "victims/other"}

	for _, tc := range []struct {
		name   string
		tool   *corev1alpha1.Tool
		path   string
		deny   func(authorizationv1.ResourceAttributes) bool
		ctype  string
		status int
	}{
		{name: "missing tool RBAC", deny: func(a authorizationv1.ResourceAttributes) bool { return a.Resource == "tools" }, status: http.StatusForbidden},
		{name: "missing credential RBAC", deny: func(a authorizationv1.ResourceAttributes) bool { return a.Resource == "secrets" }, status: http.StatusForbidden},
		{name: "cross namespace", path: "/api/v1/tools/lookup/mcp?namespace=other", status: http.StatusForbidden},
		{name: "unknown tool", path: "/api/v1/tools/missing/mcp?namespace=default", status: http.StatusNotFound},
		{name: "unavailable", tool: notAvailable, status: http.StatusServiceUnavailable},
		{name: "stale generation", tool: staleGeneration, status: http.StatusServiceUnavailable},
		{name: "not substrate", tool: notSubstrate, status: http.StatusBadRequest},
		{name: "unqualified actor status", tool: unqualified, status: http.StatusServiceUnavailable},
		{name: "outbound access policy", tool: withPolicy, status: http.StatusBadRequest},
		{name: "reserved tool header", tool: reservedHeader, status: http.StatusBadRequest},
		{name: "non-json body", ctype: "text/plain", status: http.StatusUnsupportedMediaType},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var objects []client.Object
			if tc.tool != nil {
				objects = append(objects, tc.tool)
			}
			f := newSubstrateMCPProxyFixture(t, objects...)
			f.deny = tc.deny
			path := tc.path
			if path == "" {
				path = "/api/v1/tools/lookup/mcp?namespace=default"
			}
			ctype := tc.ctype
			if ctype == "" {
				ctype = "application/json"
			}
			header := http.Header{}
			header.Set("Content-Type", ctype)
			response := f.do(t, http.MethodPost, path, header, `{}`)
			body, _ := io.ReadAll(response.Body)
			require.Equal(t, tc.status, response.StatusCode, string(body))
			require.Zero(t, f.upstreamCalls(), "denied request reached the actor")
			require.NotContains(t, string(body), "actor-credential")
		})
	}
}

func TestSubstrateMCPProxyUnavailableWithoutRouter(t *testing.T) {
	f := newSubstrateMCPProxyFixture(t)
	f.server.handlers.substrateMCPRouter = nil
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	response := f.do(t, http.MethodPost, "/api/v1/tools/lookup/mcp?namespace=default", header, `{}`)
	require.Equal(t, http.StatusServiceUnavailable, response.StatusCode)
	require.Zero(t, f.upstreamCalls())
}

// contextTokenProxyApp exercises the handler's own identity checks with an
// already-authenticated identity, as set by the Orka auth middleware.
func contextTokenProxyApp(f *substrateMCPProxyFixture, cfg ContextTokenAuthorizationConfig, ui *UserInfo) *fiber.App {
	f.server.handlers.contextTokenAuthorization = cfg
	app := fiber.New(fiber.Config{ErrorHandler: customErrorHandler})
	app.Post("/api/v1/tools/:name/mcp", func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, ui)
		return c.Next()
	}, f.server.handlers.ProxySubstrateMCPTool)
	return app
}

func TestSubstrateMCPProxyIdentityAndContextTokenScopes(t *testing.T) {
	enforce := ContextTokenAuthorizationConfig{
		Mode: ContextTokenAuthorizationModeEnforce, ToolUseScopes: []string{"tools:use"},
		SecretCredentialReadScopeList: []string{"secrets:credentials"},
	}
	token := func(scopes []string, transactionContext map[string]any) *UserInfo {
		return &UserInfo{AuthType: AuthTypeContextToken, Namespace: "default", ContextToken: &ContextToken{Scopes: scopes, TransactionContext: transactionContext}}
	}
	for _, tc := range []struct {
		name   string
		cfg    ContextTokenAuthorizationConfig
		ui     *UserInfo
		status int
	}{
		{name: "oidc identity", cfg: enforce, ui: &UserInfo{AuthType: AuthTypeOIDC, Username: "person", Namespace: "default"}, status: http.StatusForbidden},
		{name: "audit mode", cfg: ContextTokenAuthorizationConfig{Mode: ContextTokenAuthorizationModeAudit}, ui: token([]string{"tools:use", "secrets:credentials"}, nil), status: http.StatusForbidden},
		{name: "missing tool scope", cfg: enforce, ui: token([]string{"secrets:credentials"}, nil), status: http.StatusForbidden},
		{name: "tool not allowed", cfg: enforce, ui: token([]string{"tools:use", "secrets:credentials"}, map[string]any{"allowedTools": []any{"other"}}), status: http.StatusForbidden},
		{name: "namespace mismatch", cfg: enforce, ui: token([]string{"tools:use", "secrets:credentials"}, map[string]any{"namespace": "other"}), status: http.StatusForbidden},
		{name: "missing credential scope", cfg: enforce, ui: token([]string{"tools:use"}, nil), status: http.StatusForbidden},
		{name: "credential secret mismatch", cfg: enforce, ui: token([]string{"tools:use", "secrets:credentials"}, map[string]any{"secret": "other"}), status: http.StatusForbidden},
		{name: "allowed", cfg: enforce, ui: token([]string{"tools:use", "secrets:credentials"}, map[string]any{"allowedTools": []any{"lookup"}, "secret": "mcp-auth"}), status: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSubstrateMCPProxyFixture(t)
			app := contextTokenProxyApp(f, tc.cfg, tc.ui)
			request := httptest.NewRequest(http.MethodPost, "/api/v1/tools/lookup/mcp?namespace=default", strings.NewReader(substrateMCPProxyToolCall))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer caller-token")
			workerexecutor.SetSubstrateMCPToolBinding(request.Header, substrateMCPProxyTool())
			response, err := app.Test(request, fiber.TestConfig{Timeout: 10 * time.Second, FailOnTimeout: true})
			require.NoError(t, err)
			defer response.Body.Close() //nolint:errcheck
			body, _ := io.ReadAll(response.Body)
			require.Equal(t, tc.status, response.StatusCode, string(body))
			if tc.status == http.StatusOK {
				require.Equal(t, 1, f.upstreamCalls())
				require.Equal(t, "Bearer actor-credential", f.upstream[0].Header.Get("Authorization"))
				require.Empty(t, f.reviews, "transaction tokens are not reinterpreted as Kubernetes identities")
			} else {
				require.Zero(t, f.upstreamCalls())
			}
		})
	}
}

func TestSubstrateMCPProxyRefusesToolChangedSinceCallerValidated(t *testing.T) {
	for name, mutate := range map[string]func(http.Header){
		"missing binding":    func(h http.Header) { h.Del(workerexecutor.SubstrateMCPToolUIDHeader) },
		"recreated tool":     func(h http.Header) { h.Set(workerexecutor.SubstrateMCPToolUIDHeader, "previous-uid") },
		"changed credential": func(h http.Header) { h.Set(workerexecutor.SubstrateMCPToolGenerationHeader, "2") },
		"replaced actor":     func(h http.Header) { h.Set(workerexecutor.SubstrateMCPToolActorHeader, "actor-0.tools") },
	} {
		t.Run(name, func(t *testing.T) {
			f := newSubstrateMCPProxyFixture(t)
			request := httptest.NewRequest(http.MethodPost, "/api/v1/tools/lookup/mcp?namespace=default", strings.NewReader(`{}`))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer worker-sa-token-"+t.Name())
			workerexecutor.SetSubstrateMCPToolBinding(request.Header, substrateMCPProxyTool())
			mutate(request.Header)
			response, err := f.server.app.Test(request, fiber.TestConfig{Timeout: 10 * time.Second, FailOnTimeout: true})
			require.NoError(t, err)
			defer response.Body.Close() //nolint:errcheck
			require.Equal(t, http.StatusPreconditionFailed, response.StatusCode)
			require.Zero(t, f.upstreamCalls(), "a changed Tool must not reach the Actor")
			for _, review := range f.reviews {
				require.NotEqual(t, "secrets", review.Resource, "credential use is checked only for the validated Tool")
			}
		})
	}
}

func TestSubstrateMCPProxyAdmitsOnlyTheToolsOwnCall(t *testing.T) {
	call := func(params string) string {
		return `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":` + params + `}`
	}
	for _, tc := range []struct {
		name      string
		body      string
		status    int
		forwarded string
	}{
		{name: "own callable", body: call(`{"name":"lookup","arguments":{"n":12345678901234567890}}`), status: http.StatusOK,
			forwarded: call(`{"arguments":{"n":12345678901234567890},"name":"lookup"}`)},
		{name: "initialize", body: `{"jsonrpc":"2.0","id":"initialize","method":"initialize","params":{"protocolVersion":"2025-06-18"}}`, status: http.StatusOK,
			forwarded: `{"jsonrpc":"2.0","id":"initialize","method":"initialize","params":{"protocolVersion":"2025-06-18"}}`},
		{name: "initialized notification", body: `{"jsonrpc":"2.0","method":"notifications/initialized"}`, status: http.StatusOK,
			forwarded: `{"jsonrpc":"2.0","method":"notifications/initialized"}`},
		// The Actor may read the first duplicate; only the checked one is sent.
		{name: "duplicate name keeps the checked one", body: call(`{"name":"admin","name":"lookup"}`), status: http.StatusOK,
			forwarded: call(`{"name":"lookup"}`)},
		{name: "another callable on the same actor", body: call(`{"name":"admin","arguments":{}}`), status: http.StatusForbidden},
		{name: "duplicate name ending on another callable", body: call(`{"name":"lookup","name":"admin"}`), status: http.StatusForbidden},
		{name: "differently cased name", body: call(`{"name":"lookup","Name":"admin"}`), status: http.StatusBadRequest},
		{name: "missing name", body: call(`{"arguments":{}}`), status: http.StatusForbidden},
		{name: "missing params", body: `{"jsonrpc":"2.0","id":7,"method":"tools/call"}`, status: http.StatusForbidden},
		{name: "non-object params", body: call(`["lookup"]`), status: http.StatusBadRequest},
		{name: "tools list", body: `{"jsonrpc":"2.0","id":7,"method":"tools/list"}`, status: http.StatusForbidden},
		{name: "resource read", body: `{"jsonrpc":"2.0","id":7,"method":"resources/read","params":{"uri":"file:///etc/passwd"}}`, status: http.StatusForbidden},
		{name: "batch", body: `[` + call(`{"name":"lookup"}`) + `,` + call(`{"name":"admin"}`) + `]`, status: http.StatusBadRequest},
		{name: "differently cased method", body: `{"jsonrpc":"2.0","id":7,"method":"initialize","Method":"tools/call","params":{"name":"admin"}}`, status: http.StatusBadRequest},
		{name: "duplicate method ending on a refused one", body: `{"jsonrpc":"2.0","id":7,"method":"initialize","method":"tools/list"}`, status: http.StatusForbidden},
		{name: "client response", body: `{"jsonrpc":"2.0","id":7,"result":{}}`, status: http.StatusBadRequest},
		{name: "wrong protocol", body: `{"jsonrpc":"1.0","id":7,"method":"initialize"}`, status: http.StatusBadRequest},
		{name: "structured id", body: `{"jsonrpc":"2.0","id":{"a":1},"method":"initialize"}`, status: http.StatusBadRequest},
		{name: "trailing message", body: call(`{"name":"lookup"}`) + call(`{"name":"admin"}`), status: http.StatusBadRequest},
		{name: "empty object", body: `{}`, status: http.StatusBadRequest},
		{name: "null", body: `null`, status: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSubstrateMCPProxyFixture(t)
			header := http.Header{}
			header.Set("Content-Type", "application/json")
			response := f.do(t, http.MethodPost, "/api/v1/tools/lookup/mcp?namespace=default", header, tc.body)
			body, _ := io.ReadAll(response.Body)
			require.Equal(t, tc.status, response.StatusCode, string(body))
			if tc.status != http.StatusOK {
				require.Zero(t, f.upstreamCalls(), "a refused message reached the actor")
				return
			}
			require.Equal(t, 1, f.upstreamCalls())
			require.Equal(t, tc.forwarded, f.bodies[0], "the actor must receive the checked message, not the caller's bytes")
		})
	}
}

// A transaction token limited to one Tool must not reach another callable that
// the same Actor happens to serve.
func TestSubstrateMCPProxyTokenLimitedToAToolCannotCallAnotherCallable(t *testing.T) {
	f := newSubstrateMCPProxyFixture(t)
	app := contextTokenProxyApp(f, ContextTokenAuthorizationConfig{
		Mode: ContextTokenAuthorizationModeEnforce, ToolUseScopes: []string{"tools:use"},
		SecretCredentialReadScopeList: []string{"secrets:credentials"},
	}, &UserInfo{AuthType: AuthTypeContextToken, Namespace: "default", ContextToken: &ContextToken{
		Scopes:             []string{"tools:use", "secrets:credentials"},
		TransactionContext: map[string]any{"allowedTools": []any{"lookup"}, "secret": "mcp-auth"},
	}})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/tools/lookup/mcp?namespace=default",
		strings.NewReader(`{"jsonrpc":"2.0","id":"1","method":"tools/call","params":{"name":"admin","arguments":{}}}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer caller-token")
	workerexecutor.SetSubstrateMCPToolBinding(request.Header, substrateMCPProxyTool())
	response, err := app.Test(request, fiber.TestConfig{Timeout: 10 * time.Second, FailOnTimeout: true})
	require.NoError(t, err)
	defer response.Body.Close() //nolint:errcheck
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	require.Zero(t, f.upstreamCalls(), "the Tool's credential was used for a callable the token does not name")
}

func TestSubstrateMCPProxyRedactsInjectedValuesTheActorEchoes(t *testing.T) {
	tool := substrateMCPProxyTool()
	tool.Spec.HTTP.Headers = map[string]string{"X-Static": "tool-static", "X-Api-Token": "Bearer header-secret"}
	for _, tc := range []struct {
		name        string
		contentType string
		status      int
		chunks      []string
		want        string
	}{
		{
			name: "json result", contentType: "application/json", status: http.StatusOK,
			chunks: []string{`{"jsonrpc":"2.0","id":"1","result":{"content":[{"type":"text","text":"auth=Bearer actor-credential key=header-secret txn=task-txn-token ok=tool-static"}]}}`},
			want:   `{"jsonrpc":"2.0","id":"1","result":{"content":[{"type":"text","text":"auth=Bearer [REDACTED] key=[REDACTED] txn=[REDACTED] ok=tool-static"}]}}`,
		},
		{
			name: "error body", contentType: "text/plain", status: http.StatusInternalServerError,
			chunks: []string{"upstream rejected Authorization: Bearer actor-credential"},
			want:   "upstream rejected Authorization: Bearer [REDACTED]",
		},
		{
			name: "event stream split inside the value", contentType: "text/event-stream", status: http.StatusOK,
			chunks: []string{
				"event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n",
				"event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":\"1\",\"result\":{\"echo\":\"actor-cre",
				"dential and Bearer header-sec",
				"ret\"}}\n\n",
			},
			want: "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n" +
				"event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":\"1\",\"result\":{\"echo\":\"[REDACTED] and [REDACTED]\"}}\n\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSubstrateMCPProxyFixture(t, tool)
			f.respond = func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				w.Header().Set("Mcp-Session-Id", "session-actor-credential")
				w.WriteHeader(tc.status)
				for _, chunk := range tc.chunks {
					_, _ = io.WriteString(w, chunk)
					w.(http.Flusher).Flush()
					time.Sleep(20 * time.Millisecond)
				}
			}
			header := http.Header{}
			header.Set("Content-Type", "application/json")
			header.Set(workerexecutor.SubstrateMCPToolTxnTokenHeader, "task-txn-token")
			response := f.do(t, http.MethodPost, "/api/v1/tools/lookup/mcp?namespace=default", header, substrateMCPProxyToolCall)
			body, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.Equal(t, tc.status, response.StatusCode)
			require.Equal(t, tc.want, string(body))
			require.Equal(t, "session-[REDACTED]", response.Header.Get("Mcp-Session-Id"))
			require.Equal(t, "Bearer actor-credential", f.upstream[0].Header.Get("Authorization"), "the actor still receives the credential")
		})
	}
}

// substrateMCPProxyTestStream builds a stream over the secrets the production
// path would collect: the first as the Tool credential, the rest as sensitive
// Tool headers.
func substrateMCPProxyTestStream(body io.Reader, secrets ...string) *substrateMCPProxyStream {
	headers := map[string]string{}
	for i, secret := range secrets[1:] {
		headers[fmt.Sprintf("X-Token-%d", i)] = secret
	}
	tool := &corev1alpha1.Tool{Spec: corev1alpha1.ToolSpec{HTTP: &corev1alpha1.HTTPExecution{Headers: headers}}}
	return newSubstrateMCPProxyStream(io.NopCloser(body), func() {}, substrateMCPProxyInjectedSecrets(tool, secrets[0], ""))
}

func TestSubstrateMCPProxyStreamRedactsValuesSplitAtEveryByte(t *testing.T) {
	const body = "x abcdefgh y bcd z abcdefgh\nabcdefg abcdefgh"
	const want = "x [REDACTED] y [REDACTED] z [REDACTED]\n[REDACTED]defg [REDACTED]"
	for name, reader := range map[string]io.Reader{
		"one byte per read": iotest.OneByteReader(strings.NewReader(body)),
		"half reads":        iotest.HalfReader(strings.NewReader(body)),
		"data with EOF":     iotest.DataErrReader(strings.NewReader(body)),
		"whole body":        strings.NewReader(body),
	} {
		t.Run(name, func(t *testing.T) {
			// "abc" begins "abcdefgh" and "bcd" sits inside it: the longer
			// value must win, even while only its beginning has arrived.
			got, err := io.ReadAll(substrateMCPProxyTestStream(reader, "abc", "abcdefgh", "bcd"))
			require.NoError(t, err)
			require.Equal(t, want, string(got))
		})
	}
}

// A JSON serializer may write any character of a string as an escape, and the
// caller decodes it back, so an escaped credential is still the credential.
func TestSubstrateMCPProxyStreamRedactsJSONEscapedValues(t *testing.T) {
	const secret = "pre/fix+p\"a\\ss<w>ord=\u00e9\U0001F511"
	goEncoded, err := json.Marshal(secret)
	require.NoError(t, err)
	var plain bytes.Buffer
	encoder := json.NewEncoder(&plain)
	encoder.SetEscapeHTML(false)
	require.NoError(t, encoder.Encode(secret))
	everyUnit := ""
	for _, unit := range utf16.Encode([]rune(secret)) {
		everyUnit += fmt.Sprintf(`\u%04X`, unit)
	}
	for name, encoded := range map[string]string{
		"go default":      strings.Trim(string(goEncoded), `"`),
		"go plain":        strings.Trim(strings.TrimSpace(plain.String()), `"`),
		"escaped solidus": `pre\/fix+p\"a\\ss<w>ord=\u00e9\ud83d\udd11`,
		"escaped plus":    `pre/fix\u002Bp\"a\\ss\u003cw\u003eord\u003d\u00E9\uD83D\uDD11`,
		"every character": everyUnit,
	} {
		t.Run(name, func(t *testing.T) {
			body := `{"echo":"` + encoded + `","tail":"\u00e9 \/ kept"}`
			var decoded map[string]string
			require.NoError(t, json.Unmarshal([]byte(body), &decoded))
			require.Equal(t, secret, decoded["echo"], "the case must spell the secret once decoded")
			for readers, reader := range map[string]io.Reader{
				"whole": strings.NewReader(body), "one byte per read": iotest.OneByteReader(strings.NewReader(body)),
			} {
				got, err := io.ReadAll(substrateMCPProxyTestStream(reader, secret))
				require.NoError(t, err, readers)
				require.Equal(t, `{"echo":"[REDACTED]","tail":"\u00e9 \/ kept"}`, string(got), readers)
			}
		})
	}
	// Outside JSON the value appears as it is, backslash included.
	got, err := io.ReadAll(substrateMCPProxyTestStream(iotest.OneByteReader(strings.NewReader("auth "+secret+" end\\")), secret))
	require.NoError(t, err)
	require.Equal(t, "auth [REDACTED] end\\", string(got))
}

// Text that only begins like a secret, or like an escape, passes through whole.
func TestSubstrateMCPProxyStreamKeepsTextThatOnlyResemblesAValue(t *testing.T) {
	for _, body := range []string{
		`actor-credentia`, `actor-cred\`, `actor-cred\u00`, `actor-cred\u0065x`, `\ud83d`, `\ud83d\u0041`, `a\qb actor-credentiaL`, `actor-\u0063redentiaX`,
	} {
		for readers, reader := range map[string]io.Reader{
			"whole": strings.NewReader(body), "one byte per read": iotest.OneByteReader(strings.NewReader(body)),
		} {
			got, err := io.ReadAll(substrateMCPProxyTestStream(reader, "actor-credential"))
			require.NoError(t, err, body+" "+readers)
			require.Equal(t, body, string(got), readers)
		}
	}
}

// A backslash in a secret can be read two ways at every backslash the Actor
// sends. Matching must not retry the same position along each combination.
func TestSubstrateMCPProxyStreamMatchesBackslashRunsInBoundedTime(t *testing.T) {
	secret := strings.Repeat(`\`, 64) + "x"
	body := strings.Repeat(`\`, 512) + "y " + strings.Repeat(`\\`, 64) + "x " + secret
	done := make(chan string, 1)
	go func() {
		got, _ := io.ReadAll(substrateMCPProxyTestStream(strings.NewReader(body), secret))
		done <- string(got)
	}()
	select {
	case got := <-done:
		require.Equal(t, strings.Repeat(`\`, 512)+"y [REDACTED] [REDACTED]", got)
	case <-time.After(20 * time.Second):
		t.Fatal("matching a run of backslashes did not finish")
	}
}

// An SSE event must reach the caller when the Actor sends it, even when it ends
// in bytes that could begin a secret.
func TestSubstrateMCPProxyStreamReleasesCompleteLinesWithoutWaiting(t *testing.T) {
	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close() })
	stream := substrateMCPProxyTestStream(reader, strings.Repeat("s", 64))
	const event = "event: message\ndata: {\"progress\":\"sss\"}\n\ndata: sss\n\n"
	go func() { _, _ = io.WriteString(writer, event) }()
	got := make(chan string, 1)
	go func() {
		buffer := make([]byte, 0, len(event))
		chunk := make([]byte, 256)
		for len(buffer) < len(event) {
			n, err := stream.Read(chunk)
			buffer = append(buffer, chunk[:n]...)
			if err != nil {
				break
			}
		}
		got <- string(buffer)
	}()
	select {
	case value := <-got:
		require.Equal(t, event, value)
	case <-time.After(5 * time.Second):
		t.Fatal("the event was held back until the Actor sent more data")
	}
}

func TestSubstrateMCPProxyStreamStillFailsPastItsBound(t *testing.T) {
	for name, secrets := range map[string][]string{"redacting": {"secret-value"}, "plain": {""}} {
		t.Run(name, func(t *testing.T) {
			stream := substrateMCPProxyTestStream(strings.NewReader("0123456789"), secrets...)
			stream.remaining = 4
			got, err := io.ReadAll(stream)
			require.ErrorContains(t, err, "exceeds the proxy limit")
			require.Equal(t, "0123", string(got))
		})
	}
}
