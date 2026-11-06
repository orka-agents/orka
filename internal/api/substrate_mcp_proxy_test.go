/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
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
	"github.com/orka-agents/orka/internal/tracing/testutil"
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
	// routerContexts records each router request's trace identity.
	routerContexts []trace.SpanContext
}

type substrateMCPTraceRecordingTransport struct {
	base http.RoundTripper
	f    *substrateMCPProxyFixture
}

func (t substrateMCPTraceRecordingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.f.mu.Lock()
	t.f.routerContexts = append(t.f.routerContexts, trace.SpanContextFromContext(request.Context()))
	t.f.mu.Unlock()
	return t.base.RoundTrip(request)
}

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
		f.mu.Unlock()
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
	routerClient := upstream.Client()
	routerClient.Transport = substrateMCPTraceRecordingTransport{base: routerClient.Transport, f: f}
	router, err := workerexecutor.NewSubstrateMCPRouter(upstream.URL, routerClient)
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
	response := f.do(t, http.MethodPost, "/api/v1/tools/lookup/mcp?namespace=default", header, `{"jsonrpc":"2.0","id":"1","method":"tools/call"}`)
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
	require.Equal(t, `{"jsonrpc":"2.0","id":"1","method":"tools/call"}`, f.bodies[0])
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
			request := httptest.NewRequest(http.MethodPost, "/api/v1/tools/lookup/mcp?namespace=default", strings.NewReader(`{}`))
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

func TestSubstrateMCPProxyCarriesCallerTraceToRouter(t *testing.T) {
	testutil.NewSpanHarness(t)
	previous := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTextMapPropagator(previous) })
	f := newSubstrateMCPProxyFixture(t)
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	header.Set("traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	response := f.do(t, http.MethodPost, "/api/v1/tools/lookup/mcp?namespace=default", header, `{"jsonrpc":"2.0","id":"1","method":"tools/call"}`)
	require.Equal(t, http.StatusOK, response.StatusCode)
	f.mu.Lock()
	defer f.mu.Unlock()
	require.Len(t, f.routerContexts, 1)
	require.Equal(t, "0af7651916cd43dd8448eb211c80319c", f.routerContexts[0].TraceID().String(),
		"the routed Actor call must join the caller's trace")
}
