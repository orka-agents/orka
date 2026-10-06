/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/connectors"
	"github.com/orka-agents/orka/internal/controller"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

const (
	connectorTestNamespace = "tenant"
	connectorTestIssuer    = "https://issuer.example.test"
	connectorCallbackBase  = "https://orka.example.test"
)

type connectorTestHarness struct {
	t   *testing.T
	app *fiber.App
	// handlers lets a test replace the uncached reader to model informer lag.
	handlers *Handlers
	client   client.Client
	store    *sqlite.Store
	identity *UserInfo
	oauth    *httptest.Server
	tokens   int
	// skipFinalizer strips the custody finalizer from new Connections, as if
	// they were created outside the API, to prove consent refuses them.
	skipFinalizer bool
	// statusFailure, when set and true, makes Connection status updates fail
	// to model a transient API server error during completion.
	statusFailure *atomic.Bool
	// grantScope overrides the scope the token fixture reports; revoked
	// records every token the fixture was asked to revoke.
	grantScope string
	revoked    []string
	// omitScope leaves the scope field out of the token response entirely,
	// which RFC 6749 reads as "as requested"; grantScope " " is an explicit
	// empty grant instead.
	omitScope bool
	// omitRefreshToken issues an access token only; expiresIn overrides the
	// fixture lifetime.
	omitRefreshToken bool
	expiresIn        int
	// now is the handlers' clock.
	now time.Time
	// createConflict makes the next Connection create report AlreadyExists
	// after the object was created, as a concurrent request would.
	createConflict *atomic.Bool
	// statusConflictOnce makes the next Connection status update fail with a
	// conflict after applying a concurrent mode change to the stored object.
	statusConflictOnce *atomic.Bool
	statusConflictMode string
	// tokenSuffix makes each exchange issue distinct token values.
	tokenSuffix string
	// contextTokenAuthorization, when set, enables scope enforcement.
	contextTokenAuthorization ContextTokenAuthorizationConfig
}

func acceptedTestProvider() *corev1alpha1.ConnectorProvider {
	provider := &corev1alpha1.ConnectorProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "github", Namespace: connectorTestNamespace, Generation: 1, UID: "provider-uid"},
		Spec: corev1alpha1.ConnectorProviderSpec{
			DisplayName: "GitHub",
			OAuth: corev1alpha1.ConnectorOAuthConfig{
				AuthorizeURL:    "https://provider.example.test/authorize",
				TokenURL:        "https://provider.example.test/token",
				RevocationURL:   "https://provider.example.test/revoke",
				ClientID:        "client-id",
				ClientSecretRef: corev1alpha1.SecretKeySelector{Name: "github-oauth", Key: "clientSecret"},
				Scopes:          corev1alpha1.ConnectorScopes{Read: []string{"read:user"}, Write: []string{"repo"}},
			},
			Tools: []corev1alpha1.ConnectorTool{
				{Name: "list_pull_requests", Class: corev1alpha1.ConnectorToolClassRead, Source: corev1alpha1.ConnectorToolSourceBuiltin},
				{Name: "create_pull_request", Class: corev1alpha1.ConnectorToolClassWrite, Source: corev1alpha1.ConnectorToolSourceBuiltin},
			},
		},
	}
	provider.Status.ObservedGeneration = 1
	provider.Status.Conditions = []metav1.Condition{
		{Type: corev1alpha1.ConnectorProviderConditionAccepted, Status: metav1.ConditionTrue, Reason: "Accepted", ObservedGeneration: 1},
		{Type: corev1alpha1.ConnectorProviderConditionResolvedRefs, Status: metav1.ConditionTrue, Reason: "ResolvedRefs", ObservedGeneration: 1},
	}
	return provider
}

func newConnectorTestHarness(t *testing.T, objects ...runtime.Object) *connectorTestHarness {
	t.Helper()
	return buildConnectorTestHarness(t, ContextTokenAuthorizationConfig{}, objects...)
}

// newConnectorTestHarnessWithContextTokens enforces context-token scopes with
// the default connector scope names.
func newConnectorTestHarnessWithContextTokens(t *testing.T, objects ...runtime.Object) *connectorTestHarness {
	t.Helper()
	authz, err := NewContextTokenAuthorizationConfig(ContextTokenAuthorizationConfigOptions{Mode: ContextTokenAuthorizationModeEnforce})
	if err != nil {
		t.Fatal(err)
	}
	return buildConnectorTestHarness(t, authz, objects...)
}

func buildConnectorTestHarness(t *testing.T, authz ContextTokenAuthorizationConfig, objects ...runtime.Object) *connectorTestHarness {
	t.Helper()
	harness := &connectorTestHarness{t: t, contextTokenAuthorization: authz}
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	objects = append(objects, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "github-oauth", Namespace: connectorTestNamespace},
		Data:       map[string][]byte{"clientSecret": []byte("client-secret-value")},
	})
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...).
		WithStatusSubresource(&corev1alpha1.Connection{}, &corev1alpha1.ConnectorProvider{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, subResource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if harness.statusFailure != nil && harness.statusFailure.Load() {
					return errors.New("transient status failure")
				}
				if connection, ok := obj.(*corev1alpha1.Connection); ok && harness.statusConflictOnce != nil && harness.statusConflictOnce.Swap(false) {
					// A PUT moved the mode between the completion fence and
					// this status write.
					stored := &corev1alpha1.Connection{}
					if err := c.Get(ctx, client.ObjectKeyFromObject(connection), stored); err != nil {
						return err
					}
					stored.Spec.Mode = harness.statusConflictMode
					if err := c.Update(ctx, stored); err != nil {
						return err
					}
					return apierrors.NewConflict(schema.GroupResource{Group: corev1alpha1.GroupVersion.Group, Resource: "connections"}, connection.Name, errors.New("the object has been modified"))
				}
				return c.SubResource(subResource).Update(ctx, obj, opts...)
			},
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if obj.GetUID() == "" {
					obj.SetUID(types.UID("uid-" + obj.GetName()))
				}
				if _, isConnection := obj.(*corev1alpha1.Connection); isConnection && harness.skipFinalizer {
					// Model a Connection created outside the API, before the controller protects it.
					obj.SetFinalizers(nil)
				}
				if err := c.Create(ctx, obj, opts...); err != nil {
					return err
				}
				if _, isConnection := obj.(*corev1alpha1.Connection); isConnection && harness.createConflict != nil && harness.createConflict.Swap(false) {
					return apierrors.NewAlreadyExists(schema.GroupResource{Group: corev1alpha1.GroupVersion.Group, Resource: "connections"}, obj.GetName())
				}
				return nil
			},
		}).Build()

	db, err := sqlite.NewDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	sqliteStore := sqlite.NewStore(db, ":memory:")
	cipher, err := sqlite.NewAgentExecutionSnapshotCipher(bytes.Repeat([]byte{0x42}, sqlite.AgentExecutionSnapshotKeyBytes))
	if err != nil {
		t.Fatal(err)
	}
	if err := sqliteStore.SetAgentExecutionSnapshotCipher(cipher); err != nil {
		t.Fatal(err)
	}

	h := harness
	h.client, h.store = fakeClient, sqliteStore
	h.oauth = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/revoke" {
			h.revoked = append(h.revoked, r.PostForm.Get("token"))
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path != "/token" {
			http.NotFound(w, r)
			return
		}
		if r.PostForm.Get("code") != "good-code" || r.PostForm.Get("code_verifier") == "" {
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "bad_verification_code"})
			return
		}
		h.tokens++
		scope := "read:user"
		if h.grantScope != "" {
			scope = h.grantScope
		}
		expiresIn := 3600
		if h.expiresIn != 0 {
			expiresIn = h.expiresIn
		}
		payload := map[string]any{
			"access_token": "gho_secret_access" + h.tokenSuffix, "refresh_token": "ghr_secret_refresh" + h.tokenSuffix, "token_type": "bearer",
			"expires_in": expiresIn, "scope": scope,
		}
		if h.omitScope {
			delete(payload, "scope")
		}
		if h.omitRefreshToken {
			delete(payload, "refresh_token")
		}
		_ = json.NewEncoder(w).Encode(payload)
	}))
	t.Cleanup(h.oauth.Close)
	addr := h.oauth.Listener.Addr().String()
	oauthClient := connectors.NewOAuthClient(connectors.OAuthClientOptions{HTTPClient: &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // #nosec G402 -- local test fixture
		},
	}})

	handlers := NewHandlers(HandlersConfig{
		Client:                    fakeClient,
		APIReader:                 fakeClient,
		WatchNamespace:            connectorTestNamespace,
		ContextTokenAuthorization: harness.contextTokenAuthorization,
		Connectors: ConnectorConfig{
			Enabled:         true,
			CallbackBaseURL: connectorCallbackBase,
			StateKey:        bytes.Repeat([]byte{9}, connectors.MinStateKeyBytes),
			Credentials:     sqliteStore,
			Consents:        sqliteStore,
			OAuth:           oauthClient,
			Now: func() time.Time {
				if h.now.IsZero() {
					return time.Now()
				}
				return h.now
			},
		},
	})
	h.handlers = handlers
	h.identity = &UserInfo{AuthType: AuthTypeOIDC, Username: "alice", Subject: "alice", Issuer: connectorTestIssuer, Namespace: connectorTestNamespace}
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		if h.identity != nil {
			c.Locals(UserInfoContextKey, h.identity)
		}
		return c.Next()
	})
	app.Get(connectors.CallbackPath, handlers.ConnectionCallback)
	app.Get("/api/v1/connectors", handlers.ListConnectors)
	app.Get("/api/v1/connections", handlers.ListConnections)
	app.Post("/api/v1/connections", handlers.CreateConnection)
	app.Get("/api/v1/connections/:name", handlers.GetConnection)
	app.Put("/api/v1/connections/:name", handlers.UpdateConnection)
	app.Delete("/api/v1/connections/:name", handlers.DeleteConnection)
	app.Post("/api/v1/connections/:name/authorize", handlers.AuthorizeConnection)
	app.Post("/api/v1/connections/:name/complete", handlers.CompleteConnection)
	h.app = app
	return h
}

// completionFromLocation extracts the one-time completion token from the
// callback redirect's URL fragment.
func completionFromLocation(t *testing.T, location string) string {
	t.Helper()
	parsed, err := url.Parse(location)
	if err != nil {
		t.Fatal(err)
	}
	values, err := url.ParseQuery(parsed.Fragment)
	if err != nil {
		t.Fatal(err)
	}
	return values.Get("completion")
}

// consentAndCallback runs the browser half of a link and returns the redirect
// location.
func (h *connectorTestHarness) consentAndCallback(created ConnectionAuthorizeResponse) string {
	h.t.Helper()
	query := stateFromAuthorizeURL(h.t, created.AuthorizeURL)
	return h.callback(url.Values{"code": {"good-code"}, "state": {query.Get("state")}})
}

func (h *connectorTestHarness) complete(name, completion string) (*http.Response, []byte) {
	h.t.Helper()
	return h.do(http.MethodPost, "/api/v1/connections/"+name+"/complete", map[string]string{"completion": completion})
}

func (h *connectorTestHarness) link(created ConnectionAuthorizeResponse) {
	h.t.Helper()
	location := h.consentAndCallback(created)
	resp, raw := h.complete(created.Connection.Name, completionFromLocation(h.t, location))
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("complete status = %d body = %s", resp.StatusCode, raw)
	}
}

func (h *connectorTestHarness) do(method, path string, body any) (*http.Response, []byte) {
	h.t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			h.t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	if err != nil {
		h.t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, raw
}

func (h *connectorTestHarness) create(mode string) ConnectionAuthorizeResponse {
	h.t.Helper()
	resp, raw := h.do(http.MethodPost, "/api/v1/connections", map[string]string{"provider": "github", "mode": mode})
	if resp.StatusCode != http.StatusCreated {
		h.t.Fatalf("create status = %d body = %s", resp.StatusCode, raw)
	}
	var result ConnectionAuthorizeResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		h.t.Fatal(err)
	}
	return result
}

func stateFromAuthorizeURL(t *testing.T, raw string) url.Values {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Query()
}

func (h *connectorTestHarness) callback(query url.Values) string {
	h.t.Helper()
	resp, raw := h.do(http.MethodGet, connectors.CallbackPath+"?"+query.Encode(), nil)
	if resp.StatusCode != http.StatusSeeOther {
		h.t.Fatalf("callback status = %d body = %s", resp.StatusCode, raw)
	}
	return resp.Header.Get("Location")
}

func TestConnectionConsentFlow(t *testing.T) {
	h := newConnectorTestHarness(t, acceptedTestProvider())

	created := h.create("")
	if created.Connection.Provider != "github" || created.Connection.Mode != corev1alpha1.ConnectionModeReadOnly || created.Connection.State != corev1alpha1.ConnectionStatePending {
		t.Fatalf("created = %+v", created.Connection)
	}
	query := stateFromAuthorizeURL(t, created.AuthorizeURL)
	if query.Get("scope") != "read:user" || query.Get("code_challenge_method") != "S256" || query.Get("redirect_uri") != connectorCallbackBase+connectors.CallbackPath {
		t.Fatalf("authorize query = %v", query)
	}
	stored := &corev1alpha1.Connection{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: created.Connection.Name}, stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.Subject.Subject != "alice" || stored.Spec.Subject.Issuer != connectorTestIssuer || stored.Labels[ConnectionProviderLabel] != "github" || stored.Labels[ConnectionSubjectDigestLabel] == "" {
		t.Fatalf("stored connection = %+v", stored)
	}

	// The browser half: the callback parks the tokens and hands back a
	// completion token in the fragment; nothing is committed yet.
	location := h.callback(url.Values{"code": {"good-code"}, "state": {query.Get("state")}})
	if !strings.HasPrefix(location, connectorCallbackBase+"/settings/connectors?") || !strings.Contains(location, "status=pending") || !strings.Contains(location, "connection="+created.Connection.Name) || !strings.Contains(location, "namespace="+created.Connection.Namespace) {
		t.Fatalf("location = %q", location)
	}
	if strings.Contains(location, "gho_") || strings.Contains(location, "good-code") {
		t.Fatalf("redirect leaked material: %q", location)
	}
	if h.tokens != 1 {
		t.Fatalf("token endpoint calls = %d", h.tokens)
	}
	completion := completionFromLocation(t, location)
	if completion == "" {
		t.Fatalf("redirect fragment lacks a completion token: %q", location)
	}
	if ref, _ := connectors.CredentialRef(stored); ref.ConnectionUID != "" {
		if _, err := h.store.GetConnectorCredential(context.Background(), ref); err == nil {
			t.Fatal("callback must not commit custody before the owner completes")
		}
	}
	resp, raw := h.do(http.MethodGet, "/api/v1/connections/"+created.Connection.Name, nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), `"state":"Pending"`) {
		t.Fatalf("pending connection = %d %s", resp.StatusCode, raw)
	}

	// The owner commits with the one-time token.
	resp, raw = h.complete(created.Connection.Name, completion)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("complete = %d %s", resp.StatusCode, raw)
	}
	assertConnectionLinked(t, h, created.Connection.Name)

	// The completion token is single-use.
	resp, _ = h.complete(created.Connection.Name, completion)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("replayed completion = %d, want 409", resp.StatusCode)
	}

	// The state is single-use.
	location = h.callback(url.Values{"code": {"good-code"}, "state": {query.Get("state")}})
	if !strings.Contains(location, "reason=consent_expired") {
		t.Fatalf("replayed state location = %q", location)
	}
}

// assertConnectionLinked checks the API view, the Ready condition, and the
// sealed custody row after a successful callback, and that no token material
// escaped into the response.
func assertConnectionLinked(t *testing.T, h *connectorTestHarness, name string) {
	t.Helper()
	resp, raw := h.do(http.MethodGet, "/api/v1/connections/"+name, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d body = %s", resp.StatusCode, raw)
	}
	var got ConnectionResponse
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.State != corev1alpha1.ConnectionStateReady || !got.Ready || got.LinkedAt == nil || got.ExpiresAt == nil || len(got.GrantedScopes) == 0 {
		t.Fatalf("linked connection = %+v", got)
	}
	if strings.Contains(string(raw), "gho_") || strings.Contains(string(raw), "ghr_") {
		t.Fatalf("response leaked token material: %s", raw)
	}
	stored := &corev1alpha1.Connection{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: name}, stored); err != nil {
		t.Fatal(err)
	}
	ready := meta.FindStatusCondition(stored.Status.Conditions, corev1alpha1.ConnectionConditionReady)
	if ready == nil || ready.Status != metav1.ConditionTrue {
		t.Fatalf("Ready condition = %#v", ready)
	}
	ref, err := connectors.CredentialRef(stored)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := h.store.GetConnectorCredential(context.Background(), ref)
	if err != nil || credential.AccessToken != "gho_secret_access" || credential.RefreshToken != "ghr_secret_refresh" {
		t.Fatalf("credential = %+v err = %v", credential, err)
	}
}

func TestConnectionOwnershipModeAndDisconnect(t *testing.T) {
	h := newConnectorTestHarness(t, acceptedTestProvider())
	created := h.create("")
	h.link(created)
	var resp *http.Response
	var raw []byte

	// Listing shows the connection; another person sees nothing.
	resp, raw = h.do(http.MethodGet, "/api/v1/connections", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), created.Connection.Name) {
		t.Fatalf("list = %d %s", resp.StatusCode, raw)
	}
	h.identity = &UserInfo{AuthType: AuthTypeOIDC, Username: "bob", Subject: "bob", Issuer: connectorTestIssuer, Namespace: connectorTestNamespace}
	resp, raw = h.do(http.MethodGet, "/api/v1/connections", nil)
	if resp.StatusCode != http.StatusOK || strings.Contains(string(raw), created.Connection.Name) {
		t.Fatalf("bob list = %d %s", resp.StatusCode, raw)
	}
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/connections/" + created.Connection.Name},
		{http.MethodPut, "/api/v1/connections/" + created.Connection.Name},
		{http.MethodDelete, "/api/v1/connections/" + created.Connection.Name},
		{http.MethodPost, "/api/v1/connections/" + created.Connection.Name + "/authorize"},
		{http.MethodPost, "/api/v1/connections/" + created.Connection.Name + "/complete"},
	} {
		var body any
		switch route.method {
		case http.MethodPut:
			body = map[string]string{"mode": "readWrite"}
		case http.MethodPost:
			body = map[string]string{"completion": "nonce.c2ln"}
		}
		resp, _ := h.do(route.method, route.path, body)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s %s as bob = %d, want 404", route.method, route.path, resp.StatusCode)
		}
	}

	// Widening to readWrite starts a new consent with the write scope.
	h.identity = &UserInfo{AuthType: AuthTypeOIDC, Username: "alice", Subject: "alice", Issuer: connectorTestIssuer, Namespace: connectorTestNamespace}
	resp, raw = h.do(http.MethodPut, "/api/v1/connections/"+created.Connection.Name, map[string]string{"mode": "readWrite"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update = %d %s", resp.StatusCode, raw)
	}
	var updated ConnectionAuthorizeResponse
	if err := json.Unmarshal(raw, &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Connection.Mode != corev1alpha1.ConnectionModeReadWrite || updated.AuthorizeURL == "" {
		t.Fatalf("updated = %+v", updated)
	}
	// The stale read-only conditions must not advertise a usable readWrite link.
	if updated.Connection.Ready || updated.Connection.State != corev1alpha1.ConnectionStatePending {
		t.Fatalf("widened response must be pending until consent completes: %+v", updated.Connection)
	}
	if scope := stateFromAuthorizeURL(t, updated.AuthorizeURL).Get("scope"); scope != "read:user repo" {
		t.Fatalf("readWrite scope = %q", scope)
	}
	// Narrowing needs no consent.
	resp, raw = h.do(http.MethodPut, "/api/v1/connections/"+created.Connection.Name, map[string]string{"mode": "readOnly"})
	_ = json.Unmarshal(raw, &updated)
	if resp.StatusCode != http.StatusOK || updated.AuthorizeURL != "" || updated.Connection.Mode != corev1alpha1.ConnectionModeReadOnly {
		t.Fatalf("narrow = %d %+v", resp.StatusCode, updated)
	}

	// Re-authorize and disconnect.
	resp, raw = h.do(http.MethodPost, "/api/v1/connections/"+created.Connection.Name+"/authorize", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), "authorizeURL") {
		t.Fatalf("authorize = %d %s", resp.StatusCode, raw)
	}
	resp, _ = h.do(http.MethodDelete, "/api/v1/connections/"+created.Connection.Name, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d", resp.StatusCode)
	}
	// The custody finalizer holds the object until the controller finishes.
	stored := &corev1alpha1.Connection{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: created.Connection.Name}, stored); err != nil {
		t.Fatal(err)
	}
	if stored.DeletionTimestamp.IsZero() {
		t.Fatal("delete must mark the connection for deletion")
	}
	stored.Finalizers = nil
	if err := h.client.Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	resp, _ = h.do(http.MethodGet, "/api/v1/connections/"+created.Connection.Name, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("after delete = %d", resp.StatusCode)
	}
}

func TestConnectionCallbackRejections(t *testing.T) {
	h := newConnectorTestHarness(t, acceptedTestProvider())
	created := h.create("readOnly")
	state := stateFromAuthorizeURL(t, created.AuthorizeURL).Get("state")

	location := h.callback(url.Values{"code": {"good-code"}, "state": {"forged.c2lnbmF0dXJl"}})
	if !strings.Contains(location, "reason=invalid_state") {
		t.Fatalf("forged state location = %q", location)
	}
	location = h.callback(url.Values{"code": {"good-code"}})
	if !strings.Contains(location, "reason=invalid_state") {
		t.Fatalf("missing state location = %q", location)
	}

	// Provider denial consumes the consent.
	location = h.callback(url.Values{"error": {"access_denied"}, "state": {state}})
	if !strings.Contains(location, "reason=access_denied") || !strings.Contains(location, "connection="+created.Connection.Name) {
		t.Fatalf("denied location = %q", location)
	}
	location = h.callback(url.Values{"code": {"good-code"}, "state": {state}})
	if !strings.Contains(location, "reason=consent_expired") {
		t.Fatalf("consumed state location = %q", location)
	}

	// A bad code fails the exchange and stores nothing.
	created = h.create("readOnly")
	state = stateFromAuthorizeURL(t, created.AuthorizeURL).Get("state")
	location = h.callback(url.Values{"code": {"bad-code"}, "state": {state}})
	if !strings.Contains(location, "reason=exchange_failed") {
		t.Fatalf("bad code location = %q", location)
	}
	stored := &corev1alpha1.Connection{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: created.Connection.Name}, stored); err != nil {
		t.Fatal(err)
	}
	if ref, _ := connectors.CredentialRef(stored); ref.ConnectionUID != "" {
		if _, err := h.store.GetConnectorCredential(context.Background(), ref); err == nil {
			t.Fatal("failed exchange must not persist a credential")
		}
	}

	// A consent whose Connection was replaced (different UID) is rejected.
	created = h.create("readOnly")
	state = stateFromAuthorizeURL(t, created.AuthorizeURL).Get("state")
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: created.Connection.Name}, stored); err != nil {
		t.Fatal(err)
	}
	stored.Finalizers = nil
	if err := h.client.Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Delete(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	replacement := stored.DeepCopy()
	replacement.ResourceVersion = ""
	replacement.UID = "uid-replacement"
	replacement.Finalizers = []string{controller.ConnectionCustodyFinalizer}
	if err := h.client.Create(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	location = h.callback(url.Values{"code": {"good-code"}, "state": {state}})
	if !strings.Contains(location, "reason=connection_mismatch") {
		t.Fatalf("replaced connection location = %q", location)
	}
	if h.tokens != 0 {
		t.Fatalf("token endpoint must not be called on rejected callbacks, calls = %d", h.tokens)
	}
}

func TestConnectionHandlersRefuseNonHumanIdentities(t *testing.T) {
	h := newConnectorTestHarness(t, acceptedTestProvider())
	for name, identity := range map[string]*UserInfo{
		"service account":      {AuthType: AuthTypeTokenReview, Username: "system:serviceaccount:tenant:bot", Namespace: connectorTestNamespace},
		"oidc without subject": {AuthType: AuthTypeOIDC, Username: "x", Issuer: connectorTestIssuer, Namespace: connectorTestNamespace},
		"oidc without issuer":  {AuthType: AuthTypeOIDC, Username: "x", Subject: "x", Namespace: connectorTestNamespace},
	} {
		h.identity = identity
		resp, raw := h.do(http.MethodPost, "/api/v1/connections", map[string]string{"provider": "github"})
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s: status = %d body = %s", name, resp.StatusCode, raw)
		}
		resp, _ = h.do(http.MethodGet, "/api/v1/connectors", nil)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s: connectors status = %d", name, resp.StatusCode)
		}
	}
	h.identity = nil
	if resp, _ := h.do(http.MethodGet, "/api/v1/connections", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d", resp.StatusCode)
	}
	// Context-token identities are people too.
	h.identity = &UserInfo{AuthType: AuthTypeContextToken, Username: "carol", Subject: "carol", Issuer: connectorTestIssuer, Namespace: connectorTestNamespace}
	if resp, raw := h.do(http.MethodPost, "/api/v1/connections", map[string]string{"provider": "github"}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("context token status = %d body = %s", resp.StatusCode, raw)
	}
}

func TestConnectionCreateValidation(t *testing.T) {
	h := newConnectorTestHarness(t, acceptedTestProvider())
	cases := []struct {
		name string
		body string
		want int
	}{
		{"subject in body", `{"provider":"github","subject":{"subject":"mallory"}}`, http.StatusBadRequest},
		{"spec in body", `{"provider":"github","spec":{}}`, http.StatusBadRequest},
		{"trailing value", `{"provider":"github"} {"subject":"mallory"}`, http.StatusBadRequest},
		{"trailing garbage", `{"provider":"github"} x`, http.StatusBadRequest},
		{"trailing whitespace ok", "{\"provider\":\"github\"}\n\t ", http.StatusCreated},
		{"missing provider", `{}`, http.StatusBadRequest},
		{"bad mode", `{"provider":"github","mode":"admin"}`, http.StatusBadRequest},
		{"unknown provider", `{"provider":"gmail"}`, http.StatusNotFound},
		{"oversized", `{"provider":"` + strings.Repeat("a", maxConnectionRequestBytes) + `"}`, http.StatusRequestEntityTooLarge},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/connections", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			resp, err := h.app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != tt.want {
				raw, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d body = %s, want %d", resp.StatusCode, raw, tt.want)
			}
		})
	}

	// An unaccepted provider cannot be linked.
	pending := acceptedTestProvider()
	pending.Name = "gmail"
	pending.Status.Conditions = nil
	if err := h.client.Create(context.Background(), pending); err != nil {
		t.Fatal(err)
	}
	if resp, _ := h.do(http.MethodPost, "/api/v1/connections", map[string]string{"provider": "gmail"}); resp.StatusCode != http.StatusConflict {
		t.Fatalf("unaccepted provider status = %d", resp.StatusCode)
	}

	// Creating twice reuses the same Connection.
	first := h.create("readOnly")
	second := h.create("readWrite")
	if first.Connection.Name != second.Connection.Name || second.Connection.Mode != corev1alpha1.ConnectionModeReadWrite {
		t.Fatalf("first = %+v second = %+v", first.Connection, second.Connection)
	}
	list := &corev1alpha1.ConnectionList{}
	if err := h.client.List(context.Background(), list, client.InNamespace(connectorTestNamespace)); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("connections = %d, want 1", len(list.Items))
	}

	// The catalog lists providers with readiness and tool classes.
	resp, raw := h.do(http.MethodGet, "/api/v1/connectors", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("connectors = %d %s", resp.StatusCode, raw)
	}
	var catalog struct{ Items []ConnectorProviderResponse }
	if err := json.Unmarshal(raw, &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Items) != 2 {
		t.Fatalf("catalog = %+v", catalog.Items)
	}
	for _, item := range catalog.Items {
		if item.Name == "github" && (!item.Ready || item.DisplayName != "GitHub" || len(item.Tools) != 2 || item.Tools[1].Class != "write") {
			t.Fatalf("github catalog entry = %+v", item)
		}
		if item.Name == "gmail" && item.Ready {
			t.Fatalf("gmail must not be ready: %+v", item)
		}
	}
	if strings.Contains(string(raw), "client-secret-value") {
		t.Fatalf("catalog leaked the client secret: %s", raw)
	}
}

func TestConnectionHandlersDisabled(t *testing.T) {
	handlers := NewHandlers(HandlersConfig{Connectors: ConnectorConfig{Enabled: false}})
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, &UserInfo{AuthType: AuthTypeOIDC, Subject: "alice", Issuer: connectorTestIssuer})
		return c.Next()
	})
	app.Get("/api/v1/connections", handlers.ListConnections)
	app.Get(connectors.CallbackPath, handlers.ConnectionCallback)
	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/v1/connections", nil))
	if err != nil || resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("disabled list = %d %v", resp.StatusCode, err)
	}
	resp, err = app.Test(httptest.NewRequest(http.MethodGet, connectors.CallbackPath+"?state=x", nil))
	if err != nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("disabled callback = %d %v", resp.StatusCode, err)
	}
}

func TestValidateConnectorConfig(t *testing.T) {
	sqliteStore := sqlite.NewStore(nil, "")
	oauth := connectors.NewOAuthClient(connectors.OAuthClientOptions{})
	valid := ConnectorConfig{Enabled: true, CallbackBaseURL: "https://orka.example.test", StateKey: bytes.Repeat([]byte{1}, 32), Credentials: sqliteStore, Consents: sqliteStore, OAuth: oauth}
	if err := ValidateConnectorConfig(valid); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if err := ValidateConnectorConfig(ConnectorConfig{}); err != nil {
		t.Fatalf("disabled config rejected: %v", err)
	}
	for name, mutate := range map[string]func(*ConnectorConfig){
		"empty url":   func(c *ConnectorConfig) { c.CallbackBaseURL = "" },
		"http public": func(c *ConnectorConfig) { c.CallbackBaseURL = "http://orka.example.test" },
		"query":       func(c *ConnectorConfig) { c.CallbackBaseURL = "https://orka.example.test/?x=1" },
		"path":        func(c *ConnectorConfig) { c.CallbackBaseURL = "https://orka.example.test/prefix" },
		"userinfo":    func(c *ConnectorConfig) { c.CallbackBaseURL = "https://u@orka.example.test" },
		"ftp":         func(c *ConnectorConfig) { c.CallbackBaseURL = "ftp://orka.example.test" },
		"short key":   func(c *ConnectorConfig) { c.StateKey = []byte("short") },
		"no store":    func(c *ConnectorConfig) { c.Credentials = nil },
		"no consents": func(c *ConnectorConfig) { c.Consents = nil },
		"no oauth":    func(c *ConnectorConfig) { c.OAuth = nil },
	} {
		cfg := valid
		mutate(&cfg)
		if err := ValidateConnectorConfig(cfg); err == nil {
			t.Fatalf("%s must be rejected", name)
		}
	}
	local := valid
	local.CallbackBaseURL = "http://localhost:8080"
	if err := ValidateConnectorConfig(local); err != nil {
		t.Fatalf("localhost http rejected: %v", err)
	}
	if got := local.redirectURI(); got != "http://localhost:8080"+connectors.CallbackPath {
		t.Fatalf("redirectURI = %q", got)
	}
	_ = store.ErrNotFound
}

// TestConnectionCompletionRequiresOwnerAndToken covers the forwarded-link
// attack: an attacker starts a link and sends the authorize URL to a victim,
// who approves with their own provider account. The victim's browser then
// holds the completion token but is not the owner, and the attacker owns the
// Connection but never sees the token. Neither can finish the link.
func TestConnectionCompletionRequiresOwnerAndToken(t *testing.T) {
	h := newConnectorTestHarness(t, acceptedTestProvider())
	attacker := &UserInfo{AuthType: AuthTypeOIDC, Username: "mallory", Subject: "mallory", Issuer: connectorTestIssuer, Namespace: connectorTestNamespace}
	victim := &UserInfo{AuthType: AuthTypeOIDC, Username: "alice", Subject: "alice", Issuer: connectorTestIssuer, Namespace: connectorTestNamespace}

	h.identity = attacker
	created := h.create("")
	// The victim's browser completes consent (the callback has no Orka identity).
	h.identity = nil
	location := h.consentAndCallback(created)
	completion := completionFromLocation(t, location)

	// The victim, holding the token, is not the owner: the Connection is invisible.
	h.identity = victim
	if resp, _ := h.complete(created.Connection.Name, completion); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("victim completion = %d, want 404", resp.StatusCode)
	}
	// The attacker owns the Connection but has no token.
	h.identity = attacker
	for name, token := range map[string]string{"missing": "", "forged": "forged.Zm9yZ2Vk", "guessed nonce": "nonce.YWJj"} {
		resp, _ := h.complete(created.Connection.Name, token)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("attacker completion with %s token = %d, want 400", name, resp.StatusCode)
		}
	}
	stored := &corev1alpha1.Connection{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: created.Connection.Name}, stored); err != nil {
		t.Fatal(err)
	}
	if stored.Status.State == corev1alpha1.ConnectionStateReady {
		t.Fatal("no party may have linked the victim's account")
	}
	ref, err := connectors.CredentialRef(stored)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.GetConnectorCredential(context.Background(), ref); err == nil {
		t.Fatal("victim credentials must never reach custody")
	}
	// Even with the token, the attacker cannot commit into a *different* Connection they own.
	other := acceptedTestProvider()
	other.Name = "gmail"
	if err := h.client.Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	resp, raw := h.do(http.MethodPost, "/api/v1/connections", map[string]string{"provider": "gmail"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("second provider create = %d %s", resp.StatusCode, raw)
	}
	var second ConnectionAuthorizeResponse
	_ = json.Unmarshal(raw, &second)
	if resp, _ := h.complete(second.Connection.Name, completion); resp.StatusCode != http.StatusConflict {
		t.Fatalf("cross-connection completion = %d, want 409", resp.StatusCode)
	}
	// The token was consumed by that attempt, so the legitimate owner path is
	// also closed now; a fresh consent is required.
	if resp, _ := h.complete(created.Connection.Name, completion); resp.StatusCode != http.StatusConflict {
		t.Fatalf("consumed completion = %d, want 409", resp.StatusCode)
	}
}

func TestConnectionCompletionRequiresFinalizerAndRefusesTombstones(t *testing.T) {
	h := newConnectorTestHarness(t, acceptedTestProvider())

	// A Connection without the finalizer cannot even start consent.
	h.skipFinalizer = true
	resp, raw := h.do(http.MethodPost, "/api/v1/connections", map[string]string{"provider": "github"})
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(raw), "not yet protected") {
		t.Fatalf("consent without finalizer = %d %s, want 409", resp.StatusCode, raw)
	}
	h.skipFinalizer = false

	// A Connection that loses the finalizer after consent started is refused
	// at the callback, before any code exchange, and at completion.
	h.identity = &UserInfo{AuthType: AuthTypeOIDC, Username: "carol", Subject: "carol", Issuer: connectorTestIssuer, Namespace: connectorTestNamespace}
	created := h.create("")
	unprotected := &corev1alpha1.Connection{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: created.Connection.Name}, unprotected); err != nil {
		t.Fatal(err)
	}
	unprotected.Finalizers = nil
	if err := h.client.Update(context.Background(), unprotected); err != nil {
		t.Fatal(err)
	}
	location := h.consentAndCallback(created)
	if !strings.Contains(location, "reason=connection_unprotected") || strings.Contains(location, "completion=") {
		t.Fatalf("callback without finalizer location = %q", location)
	}
	if h.tokens != 0 {
		t.Fatalf("no code may be exchanged for an unprotected connection, calls = %d", h.tokens)
	}
	if resp, raw := h.complete(created.Connection.Name, "nonce.c2ln"); resp.StatusCode != http.StatusConflict || !strings.Contains(string(raw), "not yet protected") {
		t.Fatalf("completion without finalizer = %d %s, want 409", resp.StatusCode, raw)
	}

	// Simulate a disconnect that raced the completion: custody was deleted and
	// the UID tombstoned before the owner committed. A different person gets a
	// fresh, finalizer-protected Connection.
	h.identity = &UserInfo{AuthType: AuthTypeOIDC, Username: "bob", Subject: "bob", Issuer: connectorTestIssuer, Namespace: connectorTestNamespace}
	created = h.create("")
	location = h.consentAndCallback(created)
	completion := completionFromLocation(t, location)
	stored := &corev1alpha1.Connection{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: created.Connection.Name}, stored); err != nil {
		t.Fatal(err)
	}
	if err := h.store.DeleteConnectorCredential(context.Background(), string(stored.UID)); err != nil {
		t.Fatal(err)
	}
	if resp, raw := h.complete(created.Connection.Name, completion); resp.StatusCode != http.StatusConflict || !strings.Contains(string(raw), "disconnected") {
		t.Fatalf("completion after tombstone = %d %s, want 409", resp.StatusCode, raw)
	}
	ref, err := connectors.CredentialRef(stored)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.GetConnectorCredential(context.Background(), ref); err == nil {
		t.Fatal("tombstoned UID must not regain custody")
	}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: created.Connection.Name}, stored); err != nil {
		t.Fatal(err)
	}
	if stored.Status.State == corev1alpha1.ConnectionStateReady {
		t.Fatal("a refused completion must not mark the connection Ready")
	}

	// A callback arriving after the disconnect cannot even park tokens.
	resp, raw = h.do(http.MethodPost, "/api/v1/connections/"+created.Connection.Name+"/authorize", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("re-authorize = %d %s", resp.StatusCode, raw)
	}
	var reauthorized ConnectionAuthorizeResponse
	_ = json.Unmarshal(raw, &reauthorized)
	location = h.consentAndCallback(reauthorized)
	if !strings.Contains(location, "reason=disconnected") || strings.Contains(location, "completion=") {
		t.Fatalf("callback after tombstone location = %q", location)
	}
}

func TestConnectionCompletionRejectsStaleModeAndIsRetryable(t *testing.T) {
	h := newConnectorTestHarness(t, acceptedTestProvider())
	created := h.create("readOnly")
	location := h.consentAndCallback(created)
	completion := completionFromLocation(t, location)

	// Widen the mode after consent started: the parked read-only token must
	// not mark the readWrite generation Ready.
	resp, raw := h.do(http.MethodPut, "/api/v1/connections/"+created.Connection.Name, map[string]string{"mode": "readWrite"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("widen = %d %s", resp.StatusCode, raw)
	}
	resp, raw = h.complete(created.Connection.Name, completion)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(raw), "mode changed") {
		t.Fatalf("stale-mode completion = %d %s, want 409", resp.StatusCode, raw)
	}
	// The discarded tokens are left to expire, never revoked.
	if len(h.revoked) != 0 {
		t.Fatalf("a discarded completion must not revoke tokens nobody committed, revoked = %v", h.revoked)
	}
	stored := &corev1alpha1.Connection{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: created.Connection.Name}, stored); err != nil {
		t.Fatal(err)
	}
	if stored.Status.State == corev1alpha1.ConnectionStateReady {
		t.Fatal("a stale-mode completion must not link")
	}
	// The stale completion was discarded, not left for later.
	if resp, _ := h.complete(created.Connection.Name, completion); resp.StatusCode != http.StatusConflict {
		t.Fatalf("discarded completion = %d, want 409", resp.StatusCode)
	}

	// A completion whose status update fails transiently stays retryable.
	var fail atomic.Bool
	h.statusFailure = &fail
	h.grantScope = "read:user repo"
	second := h.create("readWrite")
	location = h.consentAndCallback(second)
	completion = completionFromLocation(t, location)
	fail.Store(true)
	if resp, raw := h.complete(second.Connection.Name, completion); resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("failed status update = %d %s, want 500", resp.StatusCode, raw)
	}
	fail.Store(false)
	if resp, raw := h.complete(second.Connection.Name, completion); resp.StatusCode != http.StatusOK {
		t.Fatalf("retry after transient failure = %d %s, want 200", resp.StatusCode, raw)
	}
	assertConnectionLinked(t, h, second.Connection.Name)
	if resp, _ := h.complete(second.Connection.Name, completion); resp.StatusCode != http.StatusConflict {
		t.Fatal("a committed completion must be consumed")
	}
}

func TestCompletionLocksSerializePerKey(t *testing.T) {
	locks := newCompletionLocks()
	unlockA := locks.lock("a")
	released := make(chan struct{})
	go func() {
		unlock := locks.lock("a")
		unlock()
		close(released)
	}()
	select {
	case <-released:
		t.Fatal("a second holder of the same key must wait")
	case <-time.After(50 * time.Millisecond):
	}
	unlockB := locks.lock("b")
	unlockB()
	unlockA()
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("the waiter must proceed once the key is released")
	}
	// Entries are dropped once the last holder releases them, so account
	// churn does not grow the map for the lifetime of the process.
	if size := locks.size(); size != 0 {
		t.Fatalf("lock map size = %d after release, want 0", size)
	}
}

func TestConnectionUpdateRequiresModeAndRetriesConsent(t *testing.T) {
	h := newConnectorTestHarness(t, acceptedTestProvider())
	created := h.create("readOnly")
	h.link(created)

	resp, raw := h.do(http.MethodPut, "/api/v1/connections/"+created.Connection.Name, map[string]string{})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(raw), "mode is required") {
		t.Fatalf("empty mode = %d %s, want 400", resp.StatusCode, raw)
	}
	stored := &corev1alpha1.Connection{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: created.Connection.Name}, stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.Mode != corev1alpha1.ConnectionModeReadOnly {
		t.Fatal("an update without a mode must change nothing")
	}

	// Widening returns an authorize URL, and so does retrying it while the
	// write scopes are still not granted.
	for attempt := range 2 {
		resp, raw = h.do(http.MethodPut, "/api/v1/connections/"+created.Connection.Name, map[string]string{"mode": "readWrite"})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("widen attempt %d = %d %s", attempt, resp.StatusCode, raw)
		}
		var updated ConnectionAuthorizeResponse
		_ = json.Unmarshal(raw, &updated)
		if updated.AuthorizeURL == "" || stateFromAuthorizeURL(t, updated.AuthorizeURL).Get("scope") != "read:user repo" {
			t.Fatalf("widen attempt %d must restart consent for the write scope: %+v", attempt, updated)
		}
	}
}

func TestConnectionCallbackRefusesPartialScopeGrants(t *testing.T) {
	h := newConnectorTestHarness(t, acceptedTestProvider())
	created := h.create("readWrite")
	// The person declines the write permission at the provider.
	h.grantScope = "read:user"
	location := h.consentAndCallback(created)
	if !strings.Contains(location, "reason=scopes_denied") || strings.Contains(location, "completion=") {
		t.Fatalf("partial grant location = %q", location)
	}
	if len(h.revoked) != 0 {
		t.Fatalf("a refused partial grant must not revoke tokens nobody committed, revoked = %v", h.revoked)
	}

	// An explicitly empty scope is a grant of nothing, not "as requested".
	h.grantScope = " "
	created = h.create("readWrite")
	location = h.consentAndCallback(created)
	if !strings.Contains(location, "reason=scopes_denied") {
		t.Fatalf("explicit empty grant location = %q", location)
	}

	// A provider that omits the scope field granted what was requested.
	h.grantScope = ""
	h.omitScope = true
	h.revoked = nil
	created = h.create("readWrite")
	location = h.consentAndCallback(created)
	if !strings.Contains(location, "status=pending") {
		t.Fatalf("no-scope grant location = %q", location)
	}
	resp, raw := h.complete(created.Connection.Name, completionFromLocation(t, location))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("complete = %d %s", resp.StatusCode, raw)
	}
	stored := &corev1alpha1.Connection{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: created.Connection.Name}, stored); err != nil {
		t.Fatal(err)
	}
	if strings.Join(stored.Status.GrantedScopes, " ") != "read:user repo" {
		t.Fatalf("granted scopes = %v, want the requested set assumed", stored.Status.GrantedScopes)
	}
}

func TestConnectionCallbackRevokesWhenDisconnectedMidExchange(t *testing.T) {
	h := newConnectorTestHarness(t, acceptedTestProvider())
	created := h.create("readOnly")
	stored := &corev1alpha1.Connection{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: created.Connection.Name}, stored); err != nil {
		t.Fatal(err)
	}
	// Disconnect finalizes while the code exchange is in flight.
	if err := h.store.DeleteConnectorCredential(context.Background(), string(stored.UID)); err != nil {
		t.Fatal(err)
	}
	location := h.consentAndCallback(created)
	if !strings.Contains(location, "reason=disconnected") {
		t.Fatalf("location = %q", location)
	}
	if len(h.revoked) != 0 {
		t.Fatalf("tokens issued for a disconnected link are left to expire, revoked = %v", h.revoked)
	}
}

func TestConnectionRoutesEnforceContextTokenScopes(t *testing.T) {
	h := newConnectorTestHarnessWithContextTokens(t, acceptedTestProvider())
	token := func(scopes ...string) *UserInfo {
		return &UserInfo{
			AuthType: AuthTypeContextToken, Username: "alice", Subject: "alice", Issuer: connectorTestIssuer, Namespace: connectorTestNamespace,
			ContextToken: &ContextToken{Subject: "alice", Issuer: connectorTestIssuer, Scopes: scopes},
		}
	}
	h.identity = token("orka:tasks:get")
	if resp, _ := h.do(http.MethodGet, "/api/v1/connections", nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unrelated scope list = %d, want 403", resp.StatusCode)
	}
	if resp, _ := h.do(http.MethodPost, "/api/v1/connections", map[string]string{"provider": "github"}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unrelated scope create = %d, want 403", resp.StatusCode)
	}
	h.identity = token(ContextTokenScopeConnectorsRead)
	if resp, _ := h.do(http.MethodGet, "/api/v1/connections", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("read scope list = %d, want 200", resp.StatusCode)
	}
	if resp, _ := h.do(http.MethodPost, "/api/v1/connections", map[string]string{"provider": "github"}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("read scope create = %d, want 403", resp.StatusCode)
	}
	h.identity = token(ContextTokenScopeConnectorsManage)
	if resp, raw := h.do(http.MethodPost, "/api/v1/connections", map[string]string{"provider": "github"}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("manage scope create = %d %s, want 201", resp.StatusCode, raw)
	}
	if resp, _ := h.do(http.MethodGet, "/api/v1/connections", nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("manage scope list = %d, want 403 (read scope required)", resp.StatusCode)
	}
}

func TestConnectionCallbackRefusesChangedProvider(t *testing.T) {
	h := newConnectorTestHarness(t, acceptedTestProvider())
	created := h.create("readOnly")
	// The provider's OAuth client is rotated while the person is at the
	// provider: the code must not be exchanged with the new client.
	provider := &corev1alpha1.ConnectorProvider{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: "github"}, provider); err != nil {
		t.Fatal(err)
	}
	provider.Spec.OAuth.ClientID = "rotated-client"
	if err := h.client.Update(context.Background(), provider); err != nil {
		t.Fatal(err)
	}
	location := h.consentAndCallback(created)
	if !strings.Contains(location, "reason=provider_changed") || strings.Contains(location, "completion=") {
		t.Fatalf("callback after provider change location = %q", location)
	}
	if h.tokens != 0 {
		t.Fatal("no code exchange may happen against a changed provider")
	}
}

func TestConnectionCompletionRefusesChangedProvider(t *testing.T) {
	h := newConnectorTestHarness(t, acceptedTestProvider())
	created := h.create("readOnly")
	location := h.consentAndCallback(created)
	completion := completionFromLocation(t, location)
	provider := &corev1alpha1.ConnectorProvider{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: "github"}, provider); err != nil {
		t.Fatal(err)
	}
	provider.Spec.OAuth.TokenURL = "https://provider.example.test/other-token"
	if err := h.client.Update(context.Background(), provider); err != nil {
		t.Fatal(err)
	}
	resp, raw := h.complete(created.Connection.Name, completion)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(raw), "provider changed") {
		t.Fatalf("completion after provider change = %d %s, want 409", resp.StatusCode, raw)
	}
	// The parked tokens belong to the old client: discarded, not sent to the new one.
	if len(h.revoked) != 0 {
		t.Fatalf("tokens must not be revoked against a different authority, revoked = %v", h.revoked)
	}
	if resp, _ := h.complete(created.Connection.Name, completion); resp.StatusCode != http.StatusConflict {
		t.Fatalf("discarded completion = %d, want 409", resp.StatusCode)
	}
	stored := &corev1alpha1.Connection{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: created.Connection.Name}, stored); err != nil {
		t.Fatal(err)
	}
	if stored.Status.State == corev1alpha1.ConnectionStateReady || stored.Status.Consent != nil {
		t.Fatalf("a completion from a changed provider must not link: %+v", stored.Status)
	}
}

func TestConnectionAuthorizeRefusesDeletingConnection(t *testing.T) {
	h := newConnectorTestHarness(t, acceptedTestProvider())
	created := h.create("readOnly")
	stored := &corev1alpha1.Connection{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: created.Connection.Name}, stored); err != nil {
		t.Fatal(err)
	}
	// The custody finalizer holds the object in Terminating until the
	// controller finalizes it; no new consent may start meanwhile.
	if err := h.client.Delete(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	resp, raw := h.do(http.MethodPost, "/api/v1/connections/"+created.Connection.Name+"/authorize", nil)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(raw), "being deleted") {
		t.Fatalf("authorize on deleting connection = %d %s, want 409", resp.StatusCode, raw)
	}
	resp, raw = h.do(http.MethodPut, "/api/v1/connections/"+created.Connection.Name, map[string]string{"mode": "readWrite"})
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(raw), "being deleted") {
		t.Fatalf("widen on deleting connection = %d %s, want 409", resp.StatusCode, raw)
	}
}

func TestConnectionLinkRecordsConsentAuthority(t *testing.T) {
	h := newConnectorTestHarness(t, acceptedTestProvider())
	created := h.create("readOnly")
	h.link(created)
	stored := &corev1alpha1.Connection{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: created.Connection.Name}, stored); err != nil {
		t.Fatal(err)
	}
	provider := &corev1alpha1.ConnectorProvider{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: "github"}, provider); err != nil {
		t.Fatal(err)
	}
	if !connectors.ConsentMatchesProvider(stored, provider) || !connectors.ConnectionLinked(stored) {
		t.Fatalf("a committed consent must record the provider authority and be linked: %+v", stored.Status)
	}
	// The sealed credential carries the issuing client's digest (what refresh
	// and revocation authenticate), independent of status.
	ref, _ := connectors.CredentialRef(stored)
	credential, err := h.store.GetConnectorCredential(context.Background(), ref)
	if err != nil || credential.AuthorityDigest != connectors.ProviderIssuerDigest(provider) {
		t.Fatalf("stored credential authority = %q err = %v", credential.AuthorityDigest, err)
	}
	// A rotated client makes the next mode change ask for consent again even
	// though the granted scopes already cover the mode.
	provider.Spec.OAuth.ClientID = "rotated-client"
	if err := h.client.Update(context.Background(), provider); err != nil {
		t.Fatal(err)
	}
	resp, raw := h.do(http.MethodPut, "/api/v1/connections/"+created.Connection.Name, map[string]string{"mode": "readOnly"})
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), "authorizeURL") {
		t.Fatalf("update after provider rotation = %d %s, want a new authorize URL", resp.StatusCode, raw)
	}
}

// TestConnectionStaleModeDiscardKeepsCommittedCredential covers a completion
// whose credential entered custody but whose status write failed: a later
// mode change discards the parked row without revoking the committed tokens.
func TestConnectionStaleModeDiscardKeepsCommittedCredential(t *testing.T) {
	h := newConnectorTestHarness(t, acceptedTestProvider())
	var fail atomic.Bool
	h.statusFailure = &fail
	created := h.create("readOnly")
	location := h.consentAndCallback(created)
	completion := completionFromLocation(t, location)
	fail.Store(true)
	if resp, raw := h.complete(created.Connection.Name, completion); resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("failed status update = %d %s, want 500", resp.StatusCode, raw)
	}
	fail.Store(false)
	// The credential is committed; the completion row is still parked.
	if resp, raw := h.do(http.MethodPut, "/api/v1/connections/"+created.Connection.Name, map[string]string{"mode": "readWrite"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("widen = %d %s", resp.StatusCode, raw)
	}
	h.revoked = nil
	// Custody already holds this grant, so the retry records the link from
	// it against the current (wider) mode, which the granted scopes do not
	// cover: the link is Pending until the person consents again, and
	// nothing is discarded or revoked.
	if resp, raw := h.complete(created.Connection.Name, completion); resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), `"state":"Pending"`) {
		t.Fatalf("committed completion after a mode change = %d %s, want 200 and Pending", resp.StatusCode, raw)
	}
	if len(h.revoked) != 0 {
		t.Fatalf("committed tokens must not be revoked when their parked row is discarded, revoked = %v", h.revoked)
	}
	stored := &corev1alpha1.Connection{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: created.Connection.Name}, stored); err != nil {
		t.Fatal(err)
	}
	ref, _ := connectors.CredentialRef(stored)
	if _, err := h.store.GetConnectorCredential(context.Background(), ref); err != nil {
		t.Fatalf("committed credential must remain in custody: %v", err)
	}
}

// TestConnectionPartialGrantKeepsReissuedCommittedToken covers a provider that
// re-issues the committed long-lived access token during a widening consent
// the person then declines: the refusal must not revoke the live token.
func TestConnectionPartialGrantKeepsReissuedCommittedToken(t *testing.T) {
	h := newConnectorTestHarness(t, acceptedTestProvider())
	created := h.create("readOnly")
	h.link(created)
	h.revoked = nil
	// The fixture always issues gho_secret_access / ghr_secret_refresh: the
	// widening exchange re-issues exactly the committed material.
	if resp, raw := h.do(http.MethodPut, "/api/v1/connections/"+created.Connection.Name, map[string]string{"mode": "readWrite"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("widen = %d %s", resp.StatusCode, raw)
	}
	var widened ConnectionAuthorizeResponse
	if resp, raw := h.do(http.MethodPost, "/api/v1/connections/"+created.Connection.Name+"/authorize", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("authorize = %d %s", resp.StatusCode, raw)
	} else if err := json.Unmarshal(raw, &widened); err != nil {
		t.Fatal(err)
	}
	h.grantScope = "read:user"
	if location := h.consentAndCallback(widened); !strings.Contains(location, "reason=scopes_denied") {
		t.Fatalf("partial grant location = %q", location)
	}
	if len(h.revoked) != 0 {
		t.Fatalf("re-issued committed tokens must not be revoked, revoked = %v", h.revoked)
	}
}

// TestConnectionCompletionRetryDoesNotOverwriteNewerCommit covers a
// completion that wrote custody but failed its status write, retried after a
// newer completion committed different tokens: the retry must not write the
// superseded tokens over the newer commit.
func TestConnectionCompletionRetryDoesNotOverwriteNewerCommit(t *testing.T) {
	h := newConnectorTestHarness(t, acceptedTestProvider())
	var fail atomic.Bool
	h.statusFailure = &fail
	created := h.create("readOnly")
	h.tokenSuffix = "-a"
	completionA := completionFromLocation(t, h.consentAndCallback(created))
	fail.Store(true)
	if resp, raw := h.complete(created.Connection.Name, completionA); resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("first attempt = %d %s, want 500", resp.StatusCode, raw)
	}
	fail.Store(false)
	// A second consent commits newer material while A is still retryable.
	var reauthorized ConnectionAuthorizeResponse
	if resp, raw := h.do(http.MethodPost, "/api/v1/connections/"+created.Connection.Name+"/authorize", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("authorize = %d %s", resp.StatusCode, raw)
	} else if err := json.Unmarshal(raw, &reauthorized); err != nil {
		t.Fatal(err)
	}
	h.tokenSuffix = "-b"
	completionB := completionFromLocation(t, h.consentAndCallback(reauthorized))
	if resp, raw := h.complete(created.Connection.Name, completionB); resp.StatusCode != http.StatusOK {
		t.Fatalf("newer completion = %d %s", resp.StatusCode, raw)
	}
	// Retrying A resumes nothing: custody now holds B.
	if resp, raw := h.complete(created.Connection.Name, completionA); resp.StatusCode != http.StatusConflict || !strings.Contains(string(raw), "newer completion") {
		t.Fatalf("stale retry = %d %s, want 409", resp.StatusCode, raw)
	}
	stored := &corev1alpha1.Connection{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: created.Connection.Name}, stored); err != nil {
		t.Fatal(err)
	}
	ref, _ := connectors.CredentialRef(stored)
	credential, err := h.store.GetConnectorCredential(context.Background(), ref)
	if err != nil || credential.AccessToken != "gho_secret_access-b" || credential.RefreshToken != "ghr_secret_refresh-b" {
		t.Fatalf("custody = %+v err = %v, want the newer commit", credential, err)
	}
	// Each commit is a new grant, and the status mirrors custody's number:
	// A took grant 1 (its status write failed), B took grant 2.
	if credential.GrantSequence != 2 || stored.Status.GrantSequence != 2 {
		t.Fatalf("grant after the newer commit: custody %d status %d, want 2 and 2", credential.GrantSequence, stored.Status.GrantSequence)
	}
	if resp, _ := h.complete(created.Connection.Name, completionA); resp.StatusCode != http.StatusConflict {
		t.Fatal("the stale completion must be discarded")
	}
	// A retry whose material is still current resumes the status update.
	h.tokenSuffix = "-c"
	if resp, raw := h.do(http.MethodPost, "/api/v1/connections/"+created.Connection.Name+"/authorize", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("authorize = %d %s", resp.StatusCode, raw)
	} else if err := json.Unmarshal(raw, &reauthorized); err != nil {
		t.Fatal(err)
	}
	completionC := completionFromLocation(t, h.consentAndCallback(reauthorized))
	fail.Store(true)
	if resp, _ := h.complete(created.Connection.Name, completionC); resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("attempt C = %d, want 500", resp.StatusCode)
	}
	fail.Store(false)
	if resp, raw := h.complete(created.Connection.Name, completionC); resp.StatusCode != http.StatusOK {
		t.Fatalf("resumed retry = %d %s, want 200", resp.StatusCode, raw)
	}
	if resp, raw := h.do(http.MethodGet, "/api/v1/connections/"+created.Connection.Name, nil); resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), `"ready":true`) {
		t.Fatalf("resumed link = %d %s, want ready", resp.StatusCode, raw)
	}
	if credential, err := h.store.GetConnectorCredential(context.Background(), ref); err != nil || credential.AccessToken != "gho_secret_access-c" || credential.GrantSequence != 3 {
		t.Fatalf("custody after resumed retry = %+v err = %v, want grant 3", credential, err)
	}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: created.Connection.Name}, stored); err != nil || stored.Status.GrantSequence != 3 {
		t.Fatalf("status grant after resumed retry = %d err = %v, want 3", stored.Status.GrantSequence, err)
	}
}

// TestConnectionReauthorizationRetiresPreviousCredential covers linking twice:
// the first credential is retained sealed so disconnect can revoke it.
func TestConnectionReauthorizationRetiresPreviousCredential(t *testing.T) {
	h := newConnectorTestHarness(t, acceptedTestProvider())
	created := h.create("readOnly")
	h.tokenSuffix = "-a"
	h.link(created)
	var reauthorized ConnectionAuthorizeResponse
	if resp, raw := h.do(http.MethodPost, "/api/v1/connections/"+created.Connection.Name+"/authorize", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("authorize = %d %s", resp.StatusCode, raw)
	} else if err := json.Unmarshal(raw, &reauthorized); err != nil {
		t.Fatal(err)
	}
	h.tokenSuffix = "-b"
	h.link(reauthorized)
	stored := &corev1alpha1.Connection{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: created.Connection.Name}, stored); err != nil {
		t.Fatal(err)
	}
	ref, _ := connectors.CredentialRef(stored)
	retired, err := h.store.ListRetiredConnectorCredentials(context.Background(), ref)
	if err != nil || len(retired) != 1 || retired[0].AccessToken != "gho_secret_access-a" {
		t.Fatalf("retired = %+v err = %v, want the first credential", retired, err)
	}
	if held, err := h.store.GetConnectorCredential(context.Background(), ref); err != nil || held.AccessToken != "gho_secret_access-b" {
		t.Fatalf("custody = %+v err = %v", held, err)
	}
}

// TestConnectionOmittedScopeBindsToConsentRequest covers a provider whose
// configured scopes grow between consent start and callback while the token
// response omits scope: the grant is what the consent asked for, so the
// link stays Pending until the person consents again.
func TestConnectionOmittedScopeBindsToConsentRequest(t *testing.T) {
	h := newConnectorTestHarness(t, acceptedTestProvider())
	created := h.create("readOnly")
	provider := &corev1alpha1.ConnectorProvider{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: "github"}, provider); err != nil {
		t.Fatal(err)
	}
	provider.Spec.OAuth.Scopes.Read = []string{"read:user", "read:org"}
	if err := h.client.Update(context.Background(), provider); err != nil {
		t.Fatal(err)
	}
	h.omitScope = true
	location := h.consentAndCallback(created)
	if resp, raw := h.complete(created.Connection.Name, completionFromLocation(t, location)); resp.StatusCode != http.StatusOK {
		t.Fatalf("complete = %d %s", resp.StatusCode, raw)
	}
	stored := &corev1alpha1.Connection{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: created.Connection.Name}, stored); err != nil {
		t.Fatal(err)
	}
	granted := meta.FindStatusCondition(stored.Status.Conditions, corev1alpha1.ConnectionConditionScopesGranted)
	if strings.Join(stored.Status.GrantedScopes, ",") != "read:user" || granted == nil || granted.Status != metav1.ConditionFalse ||
		stored.Status.State != corev1alpha1.ConnectionStatePending {
		t.Fatalf("status after expanded provider = %+v", stored.Status)
	}
}

// TestConnectionCreateRestoresOwnershipLabels covers a deterministic
// Connection that exists without its list labels: reuse restores them.
func TestConnectionCreateRestoresOwnershipLabels(t *testing.T) {
	h := newConnectorTestHarness(t, acceptedTestProvider())
	name := connectors.ConnectionName("github", connectorTestIssuer, "alice")
	unlabeled := &corev1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: connectorTestNamespace, Finalizers: []string{controller.ConnectionCustodyFinalizer}},
		Spec: corev1alpha1.ConnectionSpec{
			Subject: corev1alpha1.ConnectionSubject{Issuer: connectorTestIssuer, Subject: "alice"}, ProviderRef: corev1alpha1.LocalObjectReference{Name: "github"}, Mode: "readOnly",
		},
	}
	if err := h.client.Create(context.Background(), unlabeled); err != nil {
		t.Fatal(err)
	}
	created := h.create("readOnly")
	stored := &corev1alpha1.Connection{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: created.Connection.Name}, stored); err != nil {
		t.Fatal(err)
	}
	if stored.Labels[ConnectionProviderLabel] != "github" || stored.Labels[ConnectionSubjectDigestLabel] == "" {
		t.Fatalf("labels after reuse = %v", stored.Labels)
	}
	if resp, raw := h.do(http.MethodGet, "/api/v1/connections", nil); resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), created.Connection.Name) {
		t.Fatalf("list after reuse = %d %s", resp.StatusCode, raw)
	}
}

// TestConnectionNarrowingReportsReady covers narrowing a widened Connection
// back to a mode the existing grant covers: the response is usable at once.
func TestConnectionNarrowingReportsReady(t *testing.T) {
	h := newConnectorTestHarness(t, acceptedTestProvider())
	created := h.create("readOnly")
	h.link(created)
	if resp, raw := h.do(http.MethodPut, "/api/v1/connections/"+created.Connection.Name, map[string]string{"mode": "readWrite"}); resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), `"state":"Pending"`) {
		t.Fatalf("widen = %d %s", resp.StatusCode, raw)
	}
	resp, raw := h.do(http.MethodPut, "/api/v1/connections/"+created.Connection.Name, map[string]string{"mode": "readOnly"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("narrow = %d %s", resp.StatusCode, raw)
	}
	var narrowed ConnectionAuthorizeResponse
	if err := json.Unmarshal(raw, &narrowed); err != nil {
		t.Fatal(err)
	}
	if narrowed.AuthorizeURL != "" || !narrowed.Connection.Ready || narrowed.Connection.State != corev1alpha1.ConnectionStateReady {
		t.Fatalf("narrowed = %+v, want ready without consent", narrowed)
	}
}

// TestConnectionCompletionRefusesRetargetedToolAfterCallback covers a tool
// destination changed between callback and completion: the OAuth client is
// unchanged, but the consented authority is not, so nothing is recorded.
func TestConnectionCompletionRefusesRetargetedToolAfterCallback(t *testing.T) {
	h := newConnectorTestHarness(t, acceptedTestProvider())
	created := h.create("readOnly")
	completion := completionFromLocation(t, h.consentAndCallback(created))
	provider := &corev1alpha1.ConnectorProvider{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: "github"}, provider); err != nil {
		t.Fatal(err)
	}
	// A new credential-receiving destination changes the consented authority
	// while the OAuth client (issuer digest) stays the same.
	provider.Spec.Tools = append(provider.Spec.Tools, corev1alpha1.ConnectorTool{
		Name: "gh_search", Class: corev1alpha1.ConnectorToolClassRead, Source: corev1alpha1.ConnectorToolSourceHTTP, Description: "search",
		HTTP: &corev1alpha1.ConnectorHTTPTool{URL: "https://api.github.com/search/issues"},
	})
	if err := h.client.Update(context.Background(), provider); err != nil {
		t.Fatal(err)
	}
	if resp, raw := h.complete(created.Connection.Name, completion); resp.StatusCode != http.StatusConflict || !strings.Contains(string(raw), "provider changed") {
		t.Fatalf("completion after retarget = %d %s, want 409", resp.StatusCode, raw)
	}
	stored := &corev1alpha1.Connection{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: created.Connection.Name}, stored); err != nil {
		t.Fatal(err)
	}
	if stored.Status.Consent != nil || stored.Status.State == corev1alpha1.ConnectionStateReady {
		t.Fatalf("a retargeted provider must not be recorded as consented: %+v", stored.Status)
	}
}

// TestConnectionCreateWideningReusedLinkReportsPending covers POST reusing a
// Ready readOnly Connection with readWrite requested: consent for the wider
// mode is outstanding, so the view is Pending, not the stale Ready state.
func TestConnectionCreateWideningReusedLinkReportsPending(t *testing.T) {
	h := newConnectorTestHarness(t, acceptedTestProvider())
	created := h.create("readOnly")
	h.link(created)
	resp, raw := h.do(http.MethodPost, "/api/v1/connections", map[string]string{"provider": "github", "mode": "readWrite"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("widening create = %d %s", resp.StatusCode, raw)
	}
	var widened ConnectionAuthorizeResponse
	if err := json.Unmarshal(raw, &widened); err != nil {
		t.Fatal(err)
	}
	if widened.AuthorizeURL == "" || widened.Connection.Ready || widened.Connection.State != corev1alpha1.ConnectionStatePending {
		t.Fatalf("widening create must report pending: %+v", widened)
	}
	// Re-authorizing the same mode keeps the current view.
	resp, raw = h.do(http.MethodPost, "/api/v1/connections", map[string]string{"provider": "github", "mode": "readOnly"})
	if resp.StatusCode != http.StatusCreated || !strings.Contains(string(raw), `"ready":true`) {
		t.Fatalf("same-mode create = %d %s, want the current ready view", resp.StatusCode, raw)
	}
}

func TestConnectionCreateReusesConcurrentlyCreatedConnection(t *testing.T) {
	h := newConnectorTestHarness(t, acceptedTestProvider())
	h.createConflict = &atomic.Bool{}
	h.createConflict.Store(true)
	// The create races another request for the same identity and provider:
	// the object exists by the time this create is answered, and this
	// request reuses it under the ordinary ownership checks.
	created := h.create("readOnly")
	if created.AuthorizeURL == "" || created.Connection.Name != connectors.ConnectionName("github", connectorTestIssuer, "alice") {
		t.Fatalf("created = %+v", created)
	}
	stored := &corev1alpha1.Connection{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: created.Connection.Name}, stored); err != nil {
		t.Fatal(err)
	}
	if stored.Labels[ConnectionSubjectDigestLabel] == "" {
		t.Fatalf("reused connection labels = %v", stored.Labels)
	}
	// Another identity's object is still refused.
	h.identity.Subject = "mallory"
	h.createConflict.Store(true)
	if resp, raw := h.do(http.MethodPost, "/api/v1/connections", map[string]string{"provider": "github", "mode": "readOnly"}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("mallory's own connection = %d %s", resp.StatusCode, raw)
	}
}

func TestConnectionCompleteRefusesExpiredTokenWithoutRefresh(t *testing.T) {
	h := newConnectorTestHarness(t, acceptedTestProvider())
	h.omitRefreshToken = true
	h.expiresIn = 60
	h.now = time.Now()
	created := h.create("readOnly")
	location := h.consentAndCallback(created)
	completion := completionFromLocation(t, location)
	// The browser held the completion past the token's lifetime: a link
	// that could never authenticate is not committed as Ready.
	h.now = h.now.Add(2 * time.Minute)
	resp, raw := h.complete(created.Connection.Name, completion)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(raw), "expired before completion") {
		t.Fatalf("complete = %d %s", resp.StatusCode, raw)
	}
	stored := &corev1alpha1.Connection{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: created.Connection.Name}, stored); err != nil {
		t.Fatal(err)
	}
	if connectors.ConnectionLinked(stored) {
		t.Fatal("an expired grant must not link the connection")
	}
	ref, _ := connectors.CredentialRef(stored)
	if _, err := h.store.GetConnectorCredential(context.Background(), ref); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("custody err = %v, want nothing committed", err)
	}
	// The completion is gone; a retry cannot commit it later.
	if resp, _ := h.complete(created.Connection.Name, completion); resp.StatusCode == http.StatusOK {
		t.Fatal("a discarded completion must not commit on retry")
	}
}

func TestConnectionCompleteRejudgesLinkAfterConcurrentModeChange(t *testing.T) {
	h := newConnectorTestHarness(t, acceptedTestProvider())
	h.grantScope = "read:user"
	created := h.create("readOnly")
	location := h.consentAndCallback(created)
	// Between the completion fence and the status write a PUT widened the
	// mode. Custody now holds the read-only grant, so the link is judged
	// against the new mode from the committed material rather than left
	// describing a grant custody no longer holds.
	h.statusConflictOnce = &atomic.Bool{}
	h.statusConflictOnce.Store(true)
	h.statusConflictMode = corev1alpha1.ConnectionModeReadWrite
	resp, raw := h.complete(created.Connection.Name, completionFromLocation(t, location))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("complete = %d %s", resp.StatusCode, raw)
	}
	stored := &corev1alpha1.Connection{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: created.Connection.Name}, stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.Mode != corev1alpha1.ConnectionModeReadWrite || strings.Join(stored.Status.GrantedScopes, " ") != "read:user" {
		t.Fatalf("stored = mode %q scopes %v", stored.Spec.Mode, stored.Status.GrantedScopes)
	}
	if connectors.ConnectionLinked(stored) || stored.Status.State != corev1alpha1.ConnectionStatePending {
		t.Fatalf("a read-only grant must not satisfy the widened mode: state %q conditions %+v", stored.Status.State, stored.Status.Conditions)
	}
	ref, _ := connectors.CredentialRef(stored)
	if held, err := h.store.GetConnectorCredential(context.Background(), ref); err != nil || strings.Join(held.Scopes, " ") != "read:user" {
		t.Fatalf("custody = %+v err = %v", held, err)
	}
}

func TestConnectionCreateRefusesObjectBoundToAnotherProvider(t *testing.T) {
	h := newConnectorTestHarness(t, acceptedTestProvider())
	// An object created outside the API under this identity's deterministic
	// name, but bound to a different provider, is not reused: the provider
	// reference is immutable and a consent for it could never link.
	name := connectors.ConnectionName("github", connectorTestIssuer, "alice")
	foreign := &corev1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: connectorTestNamespace, Finalizers: []string{controller.ConnectionCustodyFinalizer}},
		Spec: corev1alpha1.ConnectionSpec{
			Subject:     corev1alpha1.ConnectionSubject{Issuer: connectorTestIssuer, Subject: "alice"},
			ProviderRef: corev1alpha1.LocalObjectReference{Name: "other-provider"}, Mode: corev1alpha1.ConnectionModeReadOnly,
		},
	}
	if err := h.client.Create(context.Background(), foreign); err != nil {
		t.Fatal(err)
	}
	resp, raw := h.do(http.MethodPost, "/api/v1/connections", map[string]string{"provider": "github", "mode": "readOnly"})
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(raw), "another provider") {
		t.Fatalf("create over a foreign-provider object = %d %s", resp.StatusCode, raw)
	}
	stored := &corev1alpha1.Connection{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: name}, stored); err != nil {
		t.Fatal(err)
	}
	if stored.Labels[ConnectionProviderLabel] == "github" {
		t.Fatal("the foreign object must not be relabeled for the requested provider")
	}
}

func TestValidateConnectorConfigRefusesDelimiterOnlyBases(t *testing.T) {
	for _, base := range []string{"https://orka.example.test?", "https://orka.example.test#", "https://orka.example.test/?", "https://orka.example.test/#frag"} {
		cfg := ConnectorConfig{Enabled: true, CallbackBaseURL: base, StateKey: bytes.Repeat([]byte{9}, connectors.MinStateKeyBytes)}
		if err := ValidateConnectorConfig(cfg); err == nil {
			t.Fatalf("%q must be refused", base)
		}
	}
	cfg := ConnectorConfig{Enabled: true, CallbackBaseURL: "https://orka.example.test", StateKey: bytes.Repeat([]byte{9}, connectors.MinStateKeyBytes)}
	if err := ValidateConnectorConfig(cfg); err != nil && strings.Contains(err.Error(), "callback base URL") {
		t.Fatalf("a plain origin must pass the URL checks: %v", err)
	}
}

func TestConnectionCreateNarrowingReusedLinkStaysReady(t *testing.T) {
	h := newConnectorTestHarness(t, acceptedTestProvider())
	h.grantScope = "read:user repo"
	created := h.create("readWrite")
	h.link(created)
	// Reusing the Ready readWrite link with readOnly narrows it: the grant
	// already covers the narrower mode, so the response stays Ready even
	// though the spec write bumped the generation the conditions observe.
	resp, raw := h.do(http.MethodPost, "/api/v1/connections", map[string]string{"provider": "github", "mode": "readOnly"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("narrowing create = %d %s", resp.StatusCode, raw)
	}
	var reused ConnectionAuthorizeResponse
	if err := json.Unmarshal(raw, &reused); err != nil {
		t.Fatal(err)
	}
	if !reused.Connection.Ready || reused.Connection.State != corev1alpha1.ConnectionStateReady || reused.Connection.Mode != corev1alpha1.ConnectionModeReadOnly {
		t.Fatalf("narrowed reuse view = %+v, want Ready in the narrower mode", reused.Connection)
	}
}

// TestConnectionCommittedRetryKeepsProviderConsentFence covers custody
// committed and the status write lost, then a provider tool retargeted
// before the retry: the committed material is not recorded as consented for
// a destination the person never saw, and nothing is revoked.
func TestConnectionCommittedRetryKeepsProviderConsentFence(t *testing.T) {
	h := newConnectorTestHarness(t, acceptedTestProvider())
	var fail atomic.Bool
	h.statusFailure = &fail
	created := h.create("readOnly")
	completion := completionFromLocation(t, h.consentAndCallback(created))
	fail.Store(true)
	if resp, raw := h.complete(created.Connection.Name, completion); resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("failed status update = %d %s, want 500", resp.StatusCode, raw)
	}
	fail.Store(false)
	provider := &corev1alpha1.ConnectorProvider{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: "github"}, provider); err != nil {
		t.Fatal(err)
	}
	provider.Spec.Tools = append(provider.Spec.Tools, corev1alpha1.ConnectorTool{
		Name: "gh_search", Class: corev1alpha1.ConnectorToolClassRead, Source: corev1alpha1.ConnectorToolSourceHTTP, Description: "search",
		HTTP: &corev1alpha1.ConnectorHTTPTool{URL: "https://api.github.com/search/issues"},
	})
	if err := h.client.Update(context.Background(), provider); err != nil {
		t.Fatal(err)
	}
	h.revoked = nil
	if resp, raw := h.complete(created.Connection.Name, completion); resp.StatusCode != http.StatusConflict || !strings.Contains(string(raw), "provider changed") {
		t.Fatalf("committed retry after retarget = %d %s, want 409", resp.StatusCode, raw)
	}
	if len(h.revoked) != 0 {
		t.Fatalf("committed tokens must not be revoked when their parked row is discarded, revoked = %v", h.revoked)
	}
	stored := &corev1alpha1.Connection{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: created.Connection.Name}, stored); err != nil {
		t.Fatal(err)
	}
	if connectors.ConnectionLinked(stored) || stored.Status.Consent != nil {
		t.Fatalf("a retargeted provider must not be recorded as consented: %+v", stored.Status)
	}
}

// TestConnectionRevocationIdentityBoundAtConsentStart covers a revocation
// endpoint moved while the person is at the provider: the consent still
// completes (the OAuth client and tool destinations are unchanged), but the
// tokens are sealed with the revocation identity consent started under, so
// disconnect never hands them to the endpoint that appeared meanwhile.
func TestConnectionRevocationIdentityBoundAtConsentStart(t *testing.T) {
	h := newConnectorTestHarness(t, acceptedTestProvider())
	created := h.create("readOnly")
	provider := &corev1alpha1.ConnectorProvider{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: "github"}, provider); err != nil {
		t.Fatal(err)
	}
	original := connectors.ProviderRevocationDigest(provider)
	provider.Spec.OAuth.RevocationURL = "https://provider.example.test/revoke-elsewhere"
	if err := h.client.Update(context.Background(), provider); err != nil {
		t.Fatal(err)
	}
	moved := connectors.ProviderRevocationDigest(provider)
	if moved == original {
		t.Fatal("fixture: moving the revocation endpoint must change the revocation identity")
	}
	h.link(created)
	stored := &corev1alpha1.Connection{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: created.Connection.Name}, stored); err != nil {
		t.Fatal(err)
	}
	ref, _ := connectors.CredentialRef(stored)
	credential, err := h.store.GetConnectorCredential(context.Background(), ref)
	if err != nil || credential.RevocationDigest != original {
		t.Fatalf("sealed revocation digest = %q err = %v, want the identity consent started under (%q), not the moved endpoint (%q)", credential.RevocationDigest, err, original, moved)
	}
}

// TestConnectionViewsRevalidateAgainstCurrentProvider covers the window in
// which a provider has changed but the Connection's own conditions still
// say Ready: the API views apply the checks credential resolution applies,
// so the dashboard and CLI never advertise a link it would refuse.
func TestConnectionViewsRevalidateAgainstCurrentProvider(t *testing.T) {
	h := newConnectorTestHarness(t, acceptedTestProvider())
	created := h.create("readOnly")
	h.link(created)
	name := created.Connection.Name
	views := func() (ConnectionResponse, ConnectionResponse) {
		t.Helper()
		var got ConnectionResponse
		resp, raw := h.do(http.MethodGet, "/api/v1/connections/"+name, nil)
		if resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &got) != nil {
			t.Fatalf("get = %d %s", resp.StatusCode, raw)
		}
		var list struct {
			Items []ConnectionResponse `json:"items"`
		}
		resp, raw = h.do(http.MethodGet, "/api/v1/connections", nil)
		if resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &list) != nil || len(list.Items) != 1 {
			t.Fatalf("list = %d %s", resp.StatusCode, raw)
		}
		return got, list.Items[0]
	}
	if got, listed := views(); !got.Ready || !listed.Ready {
		t.Fatalf("linked: get = %+v list = %+v, want ready", got, listed)
	}
	updateProvider := func(mutate func(*corev1alpha1.ConnectorProvider)) {
		t.Helper()
		provider := &corev1alpha1.ConnectorProvider{}
		if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: "github"}, provider); err != nil {
			t.Fatal(err)
		}
		mutate(provider)
		if err := h.client.Update(context.Background(), provider); err != nil {
			t.Fatal(err)
		}
	}

	// A new OAuth client: the recorded consent no longer matches.
	updateProvider(func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.ClientID = "rotated-client" })
	for _, view := range func() []ConnectionResponse { g, l := views(); return []ConnectionResponse{g, l} }() {
		if view.Ready || view.State != corev1alpha1.ConnectionStateReady || !strings.Contains(view.Message, "changed since you consented") {
			t.Fatalf("rotated client: view = %+v, want not ready with a relink message", view)
		}
	}

	// The same client, but a read scope the grant lacks.
	updateProvider(func(p *corev1alpha1.ConnectorProvider) {
		p.Spec.OAuth.ClientID = "client-id"
		p.Spec.OAuth.Scopes.Read = append(p.Spec.OAuth.Scopes.Read, "read:org")
	})
	for _, view := range func() []ConnectionResponse { g, l := views(); return []ConnectionResponse{g, l} }() {
		if view.Ready || !strings.Contains(view.Message, "requires scopes") {
			t.Fatalf("widened scopes: view = %+v, want not ready with a scope message", view)
		}
	}

	// The original configuration, but no longer accepted (its references
	// broke or its generation moved on): resolution refuses the link.
	updateProvider(func(p *corev1alpha1.ConnectorProvider) { p.Spec.OAuth.Scopes.Read = []string{"read:user"} })
	unaccepted := &corev1alpha1.ConnectorProvider{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: "github"}, unaccepted); err != nil {
		t.Fatal(err)
	}
	unaccepted.Status.Conditions = nil
	if err := h.client.Status().Update(context.Background(), unaccepted); err != nil {
		t.Fatal(err)
	}
	for _, view := range func() []ConnectionResponse { g, l := views(); return []ConnectionResponse{g, l} }() {
		if view.Ready || !strings.Contains(view.Message, "not accepted") {
			t.Fatalf("unaccepted provider: view = %+v, want not ready", view)
		}
	}

	// The provider is deleted: the link keeps its tokens but is unusable.
	provider := &corev1alpha1.ConnectorProvider{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: "github"}, provider); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Delete(context.Background(), provider); err != nil {
		t.Fatal(err)
	}
	for _, view := range func() []ConnectionResponse { g, l := views(); return []ConnectionResponse{g, l} }() {
		if view.Ready || !strings.Contains(view.Message, "no longer configured") {
			t.Fatalf("deleted provider: view = %+v, want not ready", view)
		}
	}
}

// TestConnectionCallbackRefusesModeChangeBeforeExchange covers a mode change
// while the person is at the provider: the callback refuses before any code
// exchange, so no token is issued that completion would only discard.
func TestConnectionCallbackRefusesModeChangeBeforeExchange(t *testing.T) {
	h := newConnectorTestHarness(t, acceptedTestProvider())
	created := h.create("readWrite")
	connection := &corev1alpha1.Connection{}
	key := types.NamespacedName{Namespace: connectorTestNamespace, Name: created.Connection.Name}
	if err := h.client.Get(context.Background(), key, connection); err != nil {
		t.Fatal(err)
	}
	connection.Spec.Mode = corev1alpha1.ConnectionModeReadOnly
	if err := h.client.Update(context.Background(), connection); err != nil {
		t.Fatal(err)
	}
	location := h.consentAndCallback(created)
	if !strings.Contains(location, "reason=mode_changed") || strings.Contains(location, "completion=") {
		t.Fatalf("callback after mode change location = %q", location)
	}
	if h.tokens != 0 {
		t.Fatalf("token endpoint calls = %d, want no exchange after a mode change", h.tokens)
	}
}

// TestConnectionListIncludesUnlabeledOwnedConnections covers a Connection
// created through Kubernetes without the owner index label: ownership comes
// from spec.subject, so its owner still sees it.
func TestConnectionListIncludesUnlabeledOwnedConnections(t *testing.T) {
	h := newConnectorTestHarness(t, acceptedTestProvider())
	created := h.create("readOnly")
	connection := &corev1alpha1.Connection{}
	key := types.NamespacedName{Namespace: connectorTestNamespace, Name: created.Connection.Name}
	if err := h.client.Get(context.Background(), key, connection); err != nil {
		t.Fatal(err)
	}
	connection.Labels = nil
	if err := h.client.Update(context.Background(), connection); err != nil {
		t.Fatal(err)
	}
	resp, raw := h.do(http.MethodGet, "/api/v1/connections", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), created.Connection.Name) {
		t.Fatalf("list = %d %s, want the unlabeled owned Connection", resp.StatusCode, raw)
	}
}

// TestConnectionViewsReadProvidersUncached covers informer lag after a
// provider change: the views judge the provider as the API server holds it,
// as credential resolution does, not as the cache last saw it.
func TestConnectionViewsReadProvidersUncached(t *testing.T) {
	h := newConnectorTestHarness(t, acceptedTestProvider())
	created := h.create("readOnly")
	h.link(created)
	current := acceptedTestProvider()
	current.Spec.OAuth.ClientID = "rotated-client"
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	h.handlers.apiReader = fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(current).Build()
	for _, path := range []string{"/api/v1/connections/" + created.Connection.Name, "/api/v1/connections"} {
		resp, raw := h.do(http.MethodGet, path, nil)
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), `"ready":false`) || !strings.Contains(string(raw), "changed since you consented") {
			t.Fatalf("GET %s = %d %s, want the uncached provider's verdict", path, resp.StatusCode, raw)
		}
	}
}

// TestConnectionViewsMarkDuplicateLinksUnready covers a person holding two
// links to one provider: resolution refuses both as ambiguous, so neither
// view advertises a usable link.
func TestConnectionViewsMarkDuplicateLinksUnready(t *testing.T) {
	h := newConnectorTestHarness(t, acceptedTestProvider())
	created := h.create("readOnly")
	h.link(created)
	stored := &corev1alpha1.Connection{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: created.Connection.Name}, stored); err != nil {
		t.Fatal(err)
	}
	extra := &corev1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: "github-extra", Namespace: connectorTestNamespace},
		Spec:       stored.Spec,
	}
	if err := h.client.Create(context.Background(), extra); err != nil {
		t.Fatal(err)
	}
	extra.Status = stored.Status
	if err := h.client.Status().Update(context.Background(), extra); err != nil {
		t.Fatal(err)
	}
	var list struct {
		Items []ConnectionResponse `json:"items"`
	}
	resp, raw := h.do(http.MethodGet, "/api/v1/connections", nil)
	if resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &list) != nil || len(list.Items) != 2 {
		t.Fatalf("list = %d %s", resp.StatusCode, raw)
	}
	for _, item := range list.Items {
		if item.Ready || !strings.Contains(item.Message, "several links") {
			t.Fatalf("listed %s = %+v, want unready as a duplicate", item.Name, item)
		}
	}
	for _, name := range []string{created.Connection.Name, extra.Name} {
		var got ConnectionResponse
		resp, raw := h.do(http.MethodGet, "/api/v1/connections/"+name, nil)
		if resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &got) != nil || got.Ready || !strings.Contains(got.Message, "several links") {
			t.Fatalf("get %s = %d %s, want unready as a duplicate", name, resp.StatusCode, raw)
		}
	}
}
