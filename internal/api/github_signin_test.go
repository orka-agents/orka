/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/connectors"
	"github.com/orka-agents/orka/internal/connectors/credential"
	"github.com/orka-agents/orka/internal/controller"
	"github.com/orka-agents/orka/internal/githubauth"
	"github.com/orka-agents/orka/internal/store"
)

const signInTestUserID = "852750"

type revocations struct {
	mu     sync.Mutex
	tokens []string
	fail   bool
}

func (r *revocations) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.tokens...)
}

func fakeGitHub(t *testing.T, revoked *revocations) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/applications/client-id/token" {
			id, secret, ok := r.BasicAuth()
			if !ok || id != "client-id" || secret != "client-secret-value" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			var body struct {
				AccessToken string `json:"access_token"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if r.Method == http.MethodDelete {
				if revoked != nil {
					revoked.mu.Lock()
					defer revoked.mu.Unlock()
					if revoked.fail {
						w.WriteHeader(http.StatusBadGateway)
						return
					}
					revoked.tokens = append(revoked.tokens, body.AccessToken)
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}
			if body.AccessToken == "ghu_check_down" {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			users := map[string]map[string]any{
				"ghu_alice": {"id": 852750, "login": "alice"}, "ghu_alice_renewed": {"id": 852750, "login": "alice"},
				"ghu_alice_refreshed": {"id": 852750, "login": "alice"}, "ghu_alice_left": {"id": 852750, "login": "alice"},
				"ghu_bob": {"id": 1002, "login": "bob"}, "ghu_bob_refreshed": {"id": 1002, "login": "bob"},
				"ghu_alice_unanswered":        {"id": 852750, "login": "alice"},
				"ghu_alice_renamed_refreshed": {"id": 852750, "login": "alice-renamed"},
			}
			user, known := users[body.AccessToken]
			if !known {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"app": map[string]any{"client_id": "client-id"}, "user": user})
			return
		}
		if r.URL.Path == "/user/memberships/orgs/acme" {
			switch r.Header.Get("Authorization") {
			case "Bearer ghu_alice_unanswered":
				w.WriteHeader(http.StatusBadGateway)
				return
			case "Bearer ghu_alice", "Bearer ghu_alice_renewed", "Bearer ghu_alice_refreshed", "Bearer ghu_alice_renamed_refreshed":
				_ = json.NewEncoder(w).Encode(map[string]string{"state": "active"})
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

type fakeSignInCredentials struct {
	token         string
	err           error
	unrefreshable bool
}

func (f fakeSignInCredentials) HostCredential(context.Context, string, string, time.Duration) (credential.HostCredential, error) {
	if f.err != nil {
		return credential.HostCredential{}, f.err
	}
	if f.token == "" {
		return credential.HostCredential{}, credential.ErrHostLinkUnavailable
	}
	return credential.HostCredential{AccessToken: f.token, ExpiresAt: time.Now().Add(time.Hour), Refreshable: !f.unrefreshable}, nil
}

func newSignInHarness(t *testing.T) *connectorTestHarness {
	t.Helper()
	provider := acceptedTestProvider()
	provider.Spec.AgentHost = &corev1alpha1.ConnectorAgentHost{TemplateRef: corev1alpha1.LocalObjectReference{Name: "copilotd"}}
	h := newConnectorTestHarness(t, provider)
	h.handlers.githubSignIn = GitHubSignInConfig{
		Enabled: true, Provider: "github", CreateHosts: true,
		Allowed:     githubauth.Allowed{Orgs: []string{"acme"}},
		Sessions:    h.store,
		Credentials: fakeSignInCredentials{token: "ghu_alice_renewed"},
		GitHub:      githubauth.Client{APIURL: fakeGitHub(t, nil).URL},
	}
	h.handlers.watchNamespace = connectorTestNamespace
	h.handlers.signInLimiter = nil
	h.app.Get("/api/v1/auth/config", h.handlers.GetAuthConfig)
	h.app.Post("/api/v1/auth/github", h.handlers.SignInWithGitHub)
	h.app.Post("/api/v1/auth/session/renew", h.handlers.RenewSession)
	h.app.Delete("/api/v1/auth/session", h.handlers.SignOut)
	return h
}

func (h *connectorTestHarness) signIn(body map[string]any) (int, SessionResponse) {
	h.t.Helper()
	raw, _ := json.Marshal(body)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/github", bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	resp, err := h.app.Test(request)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var session SessionResponse
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusCreated {
		if err := json.Unmarshal(data, &session); err != nil {
			h.t.Fatalf("decode %s: %v", data, err)
		}
	}
	return resp.StatusCode, session
}

func (h *connectorTestHarness) asSession(token string) error {
	ui, err := h.handlers.githubSignIn.authenticateSession(context.Background(), token)
	if err != nil {
		return err
	}
	h.identity = ui
	return nil
}

func TestGitHubSignInLinksTheAccountAndIssuesASession(t *testing.T) {
	h := newSignInHarness(t)
	h.identity = nil
	status, session := h.signIn(map[string]any{"accessToken": "ghu_alice", "refreshToken": "ghr_alice", "expiresIn": 28800})
	if status != http.StatusCreated || !strings.HasPrefix(session.Token, sessionTokenPrefix) || session.Login != "alice" ||
		session.Subject != signInTestUserID || session.Namespace != connectorTestNamespace || !session.AgentHost {
		t.Fatalf("sign-in: %d %+v", status, session)
	}
	wantName := connectors.ConnectionName("github", githubauth.Issuer, signInTestUserID)
	if session.Connection != wantName {
		t.Fatalf("connection = %q, want %q", session.Connection, wantName)
	}
	connection := &corev1alpha1.Connection{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: wantName}, connection); err != nil {
		t.Fatal(err)
	}
	if connection.Spec.Subject.Issuer != githubauth.Issuer || connection.Spec.Subject.Subject != signInTestUserID ||
		connection.Status.GrantSequence == 0 || connection.Annotations[corev1alpha1.ConnectionAnnotationAgentHost] != "" {
		t.Fatalf("connection = %+v", connection)
	}
	ref, err := connectors.CredentialRef(connection)
	if err != nil {
		t.Fatal(err)
	}
	held, err := h.store.GetConnectorCredential(context.Background(), ref)
	if err != nil || held.AccessToken != "ghu_alice_refreshed" || held.RefreshToken != "ghr_alice_2" || held.ExpiresAt.IsZero() {
		t.Fatalf("custody = %v", err)
	}
	if err := h.asSession(session.Token); err != nil || h.identity.Subject != signInTestUserID ||
		h.identity.AuthType != AuthTypeGitHubSession {
		t.Fatalf("session identity: %v %+v", err, h.identity)
	}

	h.identity = nil
	status, again := h.signIn(map[string]any{"accessToken": "ghu_alice", "refreshToken": "ghr_alice", "expiresIn": 28800, "noHost": true})
	if status != http.StatusCreated || again.Connection != wantName || again.AgentHost {
		t.Fatalf("second sign-in: %d %+v", status, again)
	}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: wantName}, connection); err != nil {
		t.Fatal(err)
	}
	if connection.Annotations[corev1alpha1.ConnectionAnnotationAgentHost] != corev1alpha1.ConnectionAgentHostDeclined {
		t.Fatalf("annotations = %v", connection.Annotations)
	}
}

func TestGitHubSignInStartsAHostOnlyForAConfirmedExpiry(t *testing.T) {
	h := newSignInHarness(t)
	name := connectors.ConnectionName("github", githubauth.Issuer, signInTestUserID)
	annotation := func() string {
		t.Helper()
		connection := &corev1alpha1.Connection{}
		if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: name}, connection); err != nil {
			t.Fatal(err)
		}
		return connection.Annotations[corev1alpha1.ConnectionAnnotationAgentHost]
	}
	h.identity = nil
	status, session := h.signIn(map[string]any{"accessToken": "ghu_alice", "expiresIn": 28800})
	if status != http.StatusCreated || session.AgentHost {
		t.Fatalf("sign-in without a refresh token: %d %+v, want a session without a host", status, session)
	}
	if got := annotation(); got != corev1alpha1.ConnectionAgentHostDeclined {
		t.Fatalf("annotation = %q, want the link to get no host", got)
	}
	source := &credential.Source{Client: h.client, APIReader: h.client, Credentials: h.store, OAuth: h.handlers.connectors.OAuth}
	hostGets := func() credential.HostCredential {
		t.Helper()
		held, err := source.HostCredential(context.Background(), connectorTestNamespace, name, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		return held
	}
	if held := hostGets(); held.AccessToken != "ghu_alice" || held.Refreshable {
		t.Fatalf("custody holds %q (refreshable %v), want the submitted token", held.AccessToken, held.Refreshable)
	}

	h.handlers.githubSignIn.Credentials = fakeSignInCredentials{err: errors.New("custody unavailable")}
	h.identity = nil
	if status, _ := h.signIn(map[string]any{"accessToken": "ghu_alice", "refreshToken": "ghr_alice"}); status != http.StatusServiceUnavailable {
		t.Fatalf("sign-in over unreadable custody: %d, want 503", status)
	}
	if got := annotation(); got != corev1alpha1.ConnectionAgentHostDeclined {
		t.Fatalf("annotation = %q, want the decline kept until the link is replaced", got)
	}

	h.handlers.githubSignIn.Credentials = source
	h.identity = nil
	if status, session = h.signIn(map[string]any{"accessToken": "ghu_alice", "refreshToken": "ghr_alice"}); status != http.StatusCreated || !session.AgentHost {
		t.Fatalf("sign-in with a refresh token: %d %+v, want a host", status, session)
	}
	if got := annotation(); got != "" {
		t.Fatalf("annotation = %q, want the decline lifted", got)
	}
	if held := hostGets(); held.AccessToken != "ghu_alice_refreshed" || !held.Refreshable {
		t.Fatalf("a host gets %q (refreshable %v), want the refreshed token", held.AccessToken, held.Refreshable)
	}
}

func TestGitHubSignInStartsNoHostForATokenItCannotRefresh(t *testing.T) {
	h := newSignInHarness(t)
	h.identity = nil
	status, session := h.signIn(map[string]any{"accessToken": "ghu_alice", "refreshToken": "ghr_alice_final"})
	if status != http.StatusCreated || session.AgentHost {
		t.Fatalf("sign-in: %d %+v, want a session without a host", status, session)
	}
	connection := &corev1alpha1.Connection{}
	name := connectors.ConnectionName("github", githubauth.Issuer, signInTestUserID)
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: name}, connection); err != nil {
		t.Fatal(err)
	}
	if got := connection.Annotations[corev1alpha1.ConnectionAnnotationAgentHost]; got != corev1alpha1.ConnectionAgentHostDeclined {
		t.Fatalf("annotation = %q, want the link to get no host", got)
	}
}

func TestGitHubSignInKeepsTheDeclineOfAnOverlappingSignIn(t *testing.T) {
	h := newSignInHarness(t)
	h.identity = nil
	if status, _ := h.signIn(map[string]any{"accessToken": "ghu_alice", "refreshToken": "ghr_alice"}); status != http.StatusCreated {
		t.Fatalf("first sign-in: %d", status)
	}
	key := types.NamespacedName{Namespace: connectorTestNamespace, Name: connectors.ConnectionName("github", githubauth.Issuer, signInTestUserID)}
	h.handlers.apiReader = interceptor.NewClient(h.client.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, k client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1alpha1.Connection); ok && k == key {
				stored := &corev1alpha1.Connection{}
				if err := c.Get(ctx, k, stored); err != nil {
					return err
				}
				base := stored.DeepCopy()
				delete(stored.Annotations, corev1alpha1.ConnectionAnnotationAgentHost)
				if err := c.Patch(ctx, stored, client.MergeFrom(base)); err != nil {
					return err
				}
			}
			return c.Get(ctx, k, obj, opts...)
		},
	})
	h.identity = nil
	status, session := h.signIn(map[string]any{"accessToken": "ghu_alice", "refreshToken": "ghr_alice", "noHost": true})
	if status != http.StatusCreated || session.AgentHost {
		t.Fatalf("--no-host sign-in: %d, agentHost %v", status, session.AgentHost)
	}
	connection := &corev1alpha1.Connection{}
	if err := h.client.Get(context.Background(), key, connection); err != nil {
		t.Fatal(err)
	}
	if got := connection.Annotations[corev1alpha1.ConnectionAnnotationAgentHost]; got != corev1alpha1.ConnectionAgentHostDeclined {
		t.Fatalf("annotation = %q, want the --no-host sign-in's decline kept", got)
	}
}

type hookedSignInCredentials struct {
	fakeSignInCredentials
	before func()
}

func (f hookedSignInCredentials) HostCredential(ctx context.Context, namespace, name string, horizon time.Duration) (credential.HostCredential, error) {
	f.before()
	return f.fakeSignInCredentials.HostCredential(ctx, namespace, name, horizon)
}

func TestGitHubSignInLeavesAReplacedConnectionsDeclineAlone(t *testing.T) {
	h := newSignInHarness(t)
	h.identity = nil
	if status, _ := h.signIn(map[string]any{"accessToken": "ghu_alice", "refreshToken": "ghr_alice", "noHost": true}); status != http.StatusCreated {
		t.Fatalf("--no-host sign-in: %d", status)
	}
	key := types.NamespacedName{Namespace: connectorTestNamespace, Name: connectors.ConnectionName("github", githubauth.Issuer, signInTestUserID)}
	replace := func() {
		old := &corev1alpha1.Connection{}
		if err := h.client.Get(context.Background(), key, old); err != nil {
			t.Error(err)
			return
		}
		old.Finalizers = nil
		if err := h.client.Update(context.Background(), old); err != nil {
			t.Error(err)
			return
		}
		if err := h.client.Delete(context.Background(), old); err != nil {
			t.Error(err)
			return
		}
		replacement := old.DeepCopy()
		replacement.ResourceVersion, replacement.UID, replacement.DeletionTimestamp = "", "uid-replacement", nil
		replacement.Finalizers = []string{controller.ConnectionCustodyFinalizer}
		replacement.Annotations = map[string]string{corev1alpha1.ConnectionAnnotationAgentHost: corev1alpha1.ConnectionAgentHostDeclined}
		if err := h.client.Create(context.Background(), replacement); err != nil {
			t.Error(err)
		}
	}
	h.handlers.githubSignIn.Credentials = hookedSignInCredentials{fakeSignInCredentials: fakeSignInCredentials{token: "ghu_alice_renewed"}, before: replace}
	h.identity = nil
	if status, _ := h.signIn(map[string]any{"accessToken": "ghu_alice", "refreshToken": "ghr_alice"}); status == http.StatusCreated {
		t.Fatal("a sign-in whose link was replaced under it must not succeed on the replacement")
	}
	connection := &corev1alpha1.Connection{}
	if err := h.client.Get(context.Background(), key, connection); err != nil {
		t.Fatal(err)
	}
	if connection.UID != "uid-replacement" || connection.Annotations[corev1alpha1.ConnectionAnnotationAgentHost] != corev1alpha1.ConnectionAgentHostDeclined {
		t.Fatalf("replacement = %s %v, want its decline kept", connection.UID, connection.Annotations)
	}
}

func TestGitHubSignInLiftsTheDeclineAfterAnotherWriterChangedTheLink(t *testing.T) {
	h := newSignInHarness(t)
	h.identity = nil
	if status, _ := h.signIn(map[string]any{"accessToken": "ghu_alice", "refreshToken": "ghr_alice", "noHost": true}); status != http.StatusCreated {
		t.Fatalf("--no-host sign-in: %d", status)
	}
	key := types.NamespacedName{Namespace: connectorTestNamespace, Name: connectors.ConnectionName("github", githubauth.Issuer, signInTestUserID)}
	touch := func() {
		connection := &corev1alpha1.Connection{}
		if err := h.client.Get(context.Background(), key, connection); err != nil {
			t.Error(err)
			return
		}
		connection.Labels["test.orka.ai/touched"] = "true"
		if err := h.client.Update(context.Background(), connection); err != nil {
			t.Error(err)
		}
	}
	h.handlers.githubSignIn.Credentials = hookedSignInCredentials{fakeSignInCredentials: fakeSignInCredentials{token: "ghu_alice_renewed"}, before: touch}
	h.identity = nil
	if status, session := h.signIn(map[string]any{"accessToken": "ghu_alice", "refreshToken": "ghr_alice"}); status != http.StatusCreated || !session.AgentHost {
		t.Fatalf("sign-in: %d, agentHost %v", status, session.AgentHost)
	}
	connection := &corev1alpha1.Connection{}
	if err := h.client.Get(context.Background(), key, connection); err != nil {
		t.Fatal(err)
	}
	if got := connection.Annotations[corev1alpha1.ConnectionAnnotationAgentHost]; got != "" || connection.Labels["test.orka.ai/touched"] != "true" {
		t.Fatalf("annotation = %q, labels = %v; want the decline lifted and the other write kept", got, connection.Labels)
	}
}

func TestGitHubSignInAgainKeepsAWorkingLink(t *testing.T) {
	h := newSignInHarness(t)
	revoked := &revocations{}
	h.handlers.githubSignIn.GitHub = githubauth.Client{APIURL: fakeGitHub(t, revoked).URL}
	h.identity = nil
	name := connectors.ConnectionName("github", githubauth.Issuer, signInTestUserID)
	var grant int64
	for cycle := range 20 {
		h.identity = nil
		status, session := h.signIn(map[string]any{"accessToken": "ghu_alice", "refreshToken": "ghr_alice", "expiresIn": 28800})
		if status != http.StatusCreated {
			t.Fatalf("sign-in %d: %d", cycle+1, status)
		}
		connection := &corev1alpha1.Connection{}
		if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: name}, connection); err != nil {
			t.Fatal(err)
		}
		if grant == 0 {
			grant = connection.Status.GrantSequence
		}
		if connection.Status.GrantSequence != grant {
			t.Fatalf("sign-in %d replaced the grant: %d, want %d", cycle+1, connection.Status.GrantSequence, grant)
		}
		if err := h.asSession(session.Token); err != nil {
			t.Fatal(err)
		}
		if resp, _ := h.do(http.MethodDelete, "/api/v1/auth/session", nil); resp.StatusCode != http.StatusNoContent {
			t.Fatalf("sign out %d = %d", cycle+1, resp.StatusCode)
		}
	}
	if got := revoked.list(); len(got) != 19 || got[0] != "ghu_alice_refreshed" {
		t.Fatalf("revoked = %v, want each later sign-in's unused token", got)
	}

	h.handlers.githubSignIn.Credentials = fakeSignInCredentials{token: "ghu_bob"}
	h.identity = nil
	if status, _ := h.signIn(map[string]any{"accessToken": "ghu_alice", "refreshToken": "ghr_alice", "expiresIn": 28800}); status != http.StatusCreated {
		t.Fatalf("relink: %d", status)
	}
	connection := &corev1alpha1.Connection{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: name}, connection); err != nil {
		t.Fatal(err)
	}
	if connection.Status.GrantSequence == grant {
		t.Fatal("a sign-in over a link holding another account must replace its grant")
	}
}

func TestGitHubSignInRetriesWhenItCannotCheckTheLink(t *testing.T) {
	h := newSignInHarness(t)
	revoked := &revocations{}
	h.handlers.githubSignIn.GitHub = githubauth.Client{APIURL: fakeGitHub(t, revoked).URL}
	body := map[string]any{"accessToken": "ghu_alice", "refreshToken": "ghr_alice", "expiresIn": 28800}
	h.identity = nil
	if status, _ := h.signIn(body); status != http.StatusCreated {
		t.Fatalf("first sign-in: %d", status)
	}
	name := connectors.ConnectionName("github", githubauth.Issuer, signInTestUserID)
	grant := func() int64 {
		t.Helper()
		connection := &corev1alpha1.Connection{}
		if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: name}, connection); err != nil {
			t.Fatal(err)
		}
		return connection.Status.GrantSequence
	}
	first := grant()
	for outage, test := range map[string]struct {
		credentials fakeSignInCredentials
		revoked     []string
	}{
		"custody": {credentials: fakeSignInCredentials{err: errors.New("custody cannot answer")}, revoked: []string{"ghu_alice_refreshed"}},
		"github":  {credentials: fakeSignInCredentials{token: "ghu_check_down"}, revoked: []string{"ghu_alice_refreshed"}},
	} {
		h.handlers.githubSignIn.Credentials = test.credentials
		h.identity = nil
		before := len(revoked.list())
		if status, session := h.signIn(body); status != http.StatusServiceUnavailable || session.Token != "" {
			t.Fatalf("%s outage: %d, want 503 and no session", outage, status)
		}
		if got := grant(); got != first {
			t.Fatalf("%s outage replaced the grant: %d, want %d", outage, got, first)
		}
		if got := revoked.list()[before:]; !slices.Equal(got, test.revoked) {
			t.Fatalf("%s outage: revoked = %v, want %v", outage, got, test.revoked)
		}
	}
}

func TestGitHubSignInRevokesARefreshedTokenItRefuses(t *testing.T) {
	h := newSignInHarness(t)
	revoked := &revocations{}
	upstream, err := url.Parse(fakeGitHub(t, revoked).URL)
	if err != nil {
		t.Fatal(err)
	}
	h.handlers.githubSignIn.GitHub = githubauth.Client{APIURL: upstream.String()}
	h.identity = nil
	if status, session := h.signIn(map[string]any{"accessToken": "ghu_alice", "refreshToken": "ghr_bob"}); status != http.StatusUnauthorized ||
		session.Token != "" {
		t.Fatalf("access and refresh tokens of different accounts: %d, want 401 and no session", status)
	}
	if got := revoked.list(); !slices.Equal(got, []string{"ghu_bob_refreshed"}) {
		t.Fatalf("revoked = %v, want the refreshed token", got)
	}

	checks := 0
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/applications/client-id/token" {
			if checks++; checks > 1 {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(flaky.Close)
	h.handlers.githubSignIn.GitHub = githubauth.Client{APIURL: flaky.URL}
	h.identity = nil
	if status, session := h.signIn(map[string]any{"accessToken": "ghu_alice", "refreshToken": "ghr_alice"}); status != http.StatusBadGateway ||
		session.Token != "" {
		t.Fatalf("a refreshed token GitHub cannot check: %d, want 502 and no session", status)
	}
	if got := revoked.list(); !slices.Equal(got, []string{"ghu_bob_refreshed", "ghu_alice_refreshed"}) {
		t.Fatalf("revoked = %v, want the second refreshed token too", got)
	}
}

func TestGitHubSignInKeepsCustodysTokenWhenItCannotCheckIt(t *testing.T) {
	h := newSignInHarness(t)
	revoked := &revocations{}
	upstream, err := url.Parse(fakeGitHub(t, revoked).URL)
	if err != nil {
		t.Fatal(err)
	}
	h.handlers.githubSignIn.GitHub = githubauth.Client{APIURL: upstream.String()}
	h.identity = nil
	if status, _ := h.signIn(map[string]any{"accessToken": "ghu_alice", "refreshToken": "ghr_alice", "expiresIn": 28800}); status != http.StatusCreated {
		t.Fatalf("first sign-in: %d", status)
	}

	checks := 0
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/applications/client-id/token" {
			if checks++; checks > 1 {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(flaky.Close)
	h.handlers.githubSignIn.GitHub = githubauth.Client{APIURL: flaky.URL}
	h.handlers.githubSignIn.Credentials = fakeSignInCredentials{token: "ghu_alice"}
	h.identity = nil
	before := len(revoked.list())
	if status, session := h.signIn(map[string]any{"accessToken": "ghu_alice", "expiresIn": 28800}); status != http.StatusServiceUnavailable ||
		session.Token != "" {
		t.Fatalf("sign-in with custody's token while GitHub cannot check it: %d, want 503 and no session", status)
	}
	if checks != 2 {
		t.Fatalf("check-token calls = %d, want the sign-in's and the reuse check's", checks)
	}
	if got := revoked.list()[before:]; len(got) != 0 {
		t.Fatalf("revoked %v, which custody holds", got)
	}
}

func TestGitHubSignInNeedsAnAgentHostProvider(t *testing.T) {
	h := newSignInHarness(t)
	provider := &corev1alpha1.ConnectorProvider{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: "github"}, provider); err != nil {
		t.Fatal(err)
	}
	provider.Spec.AgentHost = nil
	if err := h.client.Update(context.Background(), provider); err != nil {
		t.Fatal(err)
	}
	h.identity = nil
	if status, session := h.signIn(map[string]any{"accessToken": "ghu_alice"}); status != http.StatusServiceUnavailable || session.Token != "" {
		t.Fatalf("sign-in through a provider without hosts: %d, want 503 and no session", status)
	}
	if resp, _ := h.do(http.MethodGet, "/api/v1/auth/config", nil); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("auth config = %d, want 503", resp.StatusCode)
	}
}

func TestGitHubSignInRefusesWhileProvidersConflict(t *testing.T) {
	h := newSignInHarness(t)
	other := acceptedTestProvider()
	other.Name, other.UID = "github-second", "provider-uid-2"
	other.Spec.AgentHost = &corev1alpha1.ConnectorAgentHost{TemplateRef: corev1alpha1.LocalObjectReference{Name: "copilotd"}}
	if err := h.client.Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	h.identity = nil
	if status, session := h.signIn(map[string]any{"accessToken": "ghu_alice"}); status != http.StatusServiceUnavailable || session.Token != "" {
		t.Fatalf("sign-in while providers conflict: %d, want 503 and no session", status)
	}
}

func TestGitHubSignInRevokesWhatItCouldNotStore(t *testing.T) {
	h := newSignInHarness(t)
	revoked := &revocations{}
	h.handlers.githubSignIn.GitHub = githubauth.Client{APIURL: fakeGitHub(t, revoked).URL}
	name := connectors.ConnectionName("github", githubauth.Issuer, signInTestUserID)
	leaving := &corev1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: connectorTestNamespace, Finalizers: []string{"orka.ai/test-hold"}},
		Spec: corev1alpha1.ConnectionSpec{
			Subject:     corev1alpha1.ConnectionSubject{Issuer: githubauth.Issuer, Subject: signInTestUserID},
			ProviderRef: corev1alpha1.LocalObjectReference{Name: "github"},
			Mode:        corev1alpha1.ConnectionModeReadOnly,
		},
	}
	if err := h.client.Create(context.Background(), leaving); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Delete(context.Background(), leaving); err != nil {
		t.Fatal(err)
	}
	h.identity = nil
	if status, _ := h.signIn(map[string]any{"accessToken": "ghu_alice", "refreshToken": "ghr_alice", "expiresIn": 28800}); status != http.StatusConflict {
		t.Fatalf("sign-in over a Connection being deleted: %d, want 409", status)
	}
	if got := revoked.list(); len(got) != 1 || got[0] != "ghu_alice_refreshed" {
		t.Fatalf("revoked = %v, want the refreshed token nothing stored", got)
	}
}

func TestGitHubSignInRacingTheConnectionCreate(t *testing.T) {
	h := newSignInHarness(t)
	h.createConflict = &atomic.Bool{}
	h.createConflict.Store(true)
	h.identity = nil
	status, session := h.signIn(map[string]any{"accessToken": "ghu_alice", "refreshToken": "ghr_alice", "expiresIn": 28800})
	if status != http.StatusCreated || session.Token == "" {
		t.Fatalf("sign-in after losing the create: %d %+v", status, session)
	}
}

func TestOverlappingSignInsCommitOneGrant(t *testing.T) {
	h := newSignInHarness(t)
	upstream := fakeGitHub(t, nil)
	var mu sync.Mutex
	arrived, together := 0, make(chan struct{})
	barrier := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method == http.MethodPost && strings.Contains(string(body), `"ghu_alice"`) {
			mu.Lock()
			if arrived++; arrived == 2 {
				close(together)
			}
			mu.Unlock()
			select {
			case <-together:
			case <-time.After(5 * time.Second):
			}
		}
		request, _ := http.NewRequestWithContext(r.Context(), r.Method, upstream.URL+r.URL.Path, bytes.NewReader(body))
		request.Header = r.Header.Clone()
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer func() { _ = response.Body.Close() }()
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
	}))
	t.Cleanup(barrier.Close)
	h.handlers.githubSignIn.GitHub = githubauth.Client{APIURL: barrier.URL}
	h.identity = nil
	statuses := make(chan int, 2)
	for range 2 {
		go func() {
			status, _ := h.signIn(map[string]any{"accessToken": "ghu_alice", "refreshToken": "ghr_alice", "expiresIn": 28800})
			statuses <- status
		}()
	}
	for range 2 {
		if status := <-statuses; status != http.StatusCreated {
			t.Fatalf("overlapping sign-in: %d", status)
		}
	}
	connection := &corev1alpha1.Connection{}
	name := connectors.ConnectionName("github", githubauth.Issuer, signInTestUserID)
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: name}, connection); err != nil {
		t.Fatal(err)
	}
	if connection.Status.GrantSequence != 1 {
		t.Fatalf("grant sequence = %d, want one grant for two overlapping sign-ins", connection.Status.GrantSequence)
	}
}

func TestGitHubSignInBoundsATokenWithoutExpiry(t *testing.T) {
	h := newSignInHarness(t)
	h.identity = nil
	before := time.Now()
	status, session := h.signIn(map[string]any{"accessToken": "ghu_alice"})
	if status != http.StatusCreated {
		t.Fatalf("sign-in: %d", status)
	}
	connection := &corev1alpha1.Connection{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: session.Connection}, connection); err != nil {
		t.Fatal(err)
	}
	ref, err := connectors.CredentialRef(connection)
	if err != nil {
		t.Fatal(err)
	}
	held, err := h.store.GetConnectorCredential(context.Background(), ref)
	if err != nil || held.RefreshToken != "" || held.ExpiresAt.IsZero() || held.ExpiresAt.After(before.Add(maxGitHubAccessTokenLife+time.Minute)) {
		t.Fatalf("custody expires at %v (%v), want within %s", held.ExpiresAt, err, maxGitHubAccessTokenLife)
	}
}

func TestGitHubSignInCapsAHugeSubmittedLifetime(t *testing.T) {
	h := newSignInHarness(t)
	h.identity = nil
	before := time.Now()
	status, session := h.signIn(map[string]any{"accessToken": "ghu_alice", "expiresIn": 10_000_000_000})
	if status != http.StatusCreated {
		t.Fatalf("sign-in: %d", status)
	}
	connection := &corev1alpha1.Connection{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: session.Connection}, connection); err != nil {
		t.Fatal(err)
	}
	ref, err := connectors.CredentialRef(connection)
	if err != nil {
		t.Fatal(err)
	}
	held, err := h.store.GetConnectorCredential(context.Background(), ref)
	if err != nil || !held.ExpiresAt.After(before) || held.ExpiresAt.After(before.Add(maxGitHubAccessTokenLife+time.Minute)) {
		t.Fatalf("custody expires at %v (%v), want within %s from now", held.ExpiresAt, err, maxGitHubAccessTokenLife)
	}
}

func TestGitHubSignInWaitsForTheCustodyFinalizer(t *testing.T) {
	h := newSignInHarness(t)
	name := connectors.ConnectionName("github", githubauth.Issuer, signInTestUserID)
	unprotected := &corev1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: connectorTestNamespace},
		Spec: corev1alpha1.ConnectionSpec{
			Subject:     corev1alpha1.ConnectionSubject{Issuer: githubauth.Issuer, Subject: signInTestUserID},
			ProviderRef: corev1alpha1.LocalObjectReference{Name: "github"},
			Mode:        corev1alpha1.ConnectionModeReadOnly,
		},
	}
	if err := h.client.Create(context.Background(), unprotected); err != nil {
		t.Fatal(err)
	}
	h.identity = nil
	if status, session := h.signIn(map[string]any{"accessToken": "ghu_alice", "refreshToken": "ghr_alice", "expiresIn": 28800}); status != http.StatusConflict ||
		session.Token != "" {
		t.Fatalf("sign-in over an unprotected Connection: %d, want 409 and no session", status)
	}
	connection := &corev1alpha1.Connection{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: name}, connection); err != nil {
		t.Fatal(err)
	}
	if connection.Status.GrantSequence != 0 {
		t.Fatalf("an unprotected Connection was linked: %+v", connection.Status)
	}
}

func TestGitHubSignInRefusesALinkBoundToAnotherProvider(t *testing.T) {
	h := newSignInHarness(t)
	revoked := &revocations{}
	h.handlers.githubSignIn.GitHub = githubauth.Client{APIURL: fakeGitHub(t, revoked).URL}
	name := connectors.ConnectionName("github", githubauth.Issuer, signInTestUserID)
	other := &corev1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: connectorTestNamespace, Finalizers: []string{controller.ConnectionCustodyFinalizer},
		},
		Spec: corev1alpha1.ConnectionSpec{
			Subject:     corev1alpha1.ConnectionSubject{Issuer: githubauth.Issuer, Subject: signInTestUserID},
			ProviderRef: corev1alpha1.LocalObjectReference{Name: "github-other"},
			Mode:        corev1alpha1.ConnectionModeReadOnly,
		},
	}
	if err := h.client.Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	h.identity = nil
	if status, session := h.signIn(map[string]any{"accessToken": "ghu_alice", "refreshToken": "ghr_alice", "expiresIn": 28800}); status != http.StatusConflict ||
		session.Token != "" {
		t.Fatalf("sign-in over another provider's Connection: %d, want 409 and no session", status)
	}
	connection := &corev1alpha1.Connection{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: name}, connection); err != nil {
		t.Fatal(err)
	}
	if connection.Status.GrantSequence != 0 || connection.Spec.ProviderRef.Name != "github-other" {
		t.Fatalf("the other provider's Connection was linked: %+v", connection)
	}
	if got := revoked.list(); len(got) != 1 {
		t.Fatalf("revoked = %v, want the sign-in's token", got)
	}
}

func TestGitHubSignInRefusesOtherAppsAndOutsiders(t *testing.T) {
	h := newSignInHarness(t)
	h.identity = nil
	for name, test := range map[string]struct {
		token string
		want  int
	}{
		"a token from another app": {token: "ghu_unknown", want: http.StatusUnauthorized},
		"someone outside the org":  {token: "ghu_bob", want: http.StatusForbidden},
	} {
		if status, session := h.signIn(map[string]any{"accessToken": test.token}); status != test.want || session.Token != "" {
			t.Fatalf("%s: %d %+v, want %d and no session", name, status, session, test.want)
		}
	}
	for name, refresh := range map[string]string{"another person's": "ghr_bob", "an unknown": "ghr_unknown"} {
		if status, session := h.signIn(map[string]any{"accessToken": "ghu_alice", "refreshToken": refresh}); status != http.StatusUnauthorized ||
			session.Token != "" {
			t.Fatalf("%s refresh token: %d, want 401 and no session", name, status)
		}
	}
	connections := &corev1alpha1.ConnectionList{}
	if err := h.client.List(context.Background(), connections); err != nil {
		t.Fatal(err)
	}
	if len(connections.Items) != 0 {
		t.Fatalf("a refused sign-in links nothing: %d Connections", len(connections.Items))
	}
}

func TestSessionRenewsOnceAndSignsOut(t *testing.T) {
	h := newSignInHarness(t)
	h.identity = nil
	_, session := h.signIn(map[string]any{"accessToken": "ghu_alice", "refreshToken": "ghr_alice", "expiresIn": 28800})
	if err := h.asSession(session.Token); err != nil {
		t.Fatal(err)
	}
	original := *h.identity
	resp, raw := h.do(http.MethodPost, "/api/v1/auth/session/renew", nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("renew = %d %s", resp.StatusCode, raw)
	}
	var renewed SessionResponse
	if err := json.Unmarshal(raw, &renewed); err != nil || renewed.Token == session.Token || !strings.HasPrefix(renewed.Token, sessionTokenPrefix) ||
		renewed.Subject != session.Subject {
		t.Fatalf("renewed = %+v, %v", renewed, err)
	}
	if err := h.asSession(session.Token); err == nil {
		t.Fatal("a renewed session must end")
	}
	h.identity = &original
	if resp, _ := h.do(http.MethodPost, "/api/v1/auth/session/renew", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("renewing a renewed session = %d, want 401", resp.StatusCode)
	}
	if err := h.asSession(renewed.Token); err != nil {
		t.Fatal(err)
	}
	resp, _ = h.do(http.MethodDelete, "/api/v1/auth/session", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("sign out = %d", resp.StatusCode)
	}
	if err := h.asSession(renewed.Token); err == nil {
		t.Fatal("a signed-out session must end")
	}
}

func TestASessionWhoseLinkCannotRefreshEndsWithItsToken(t *testing.T) {
	h := newSignInHarness(t)
	h.identity = nil
	h.handlers.githubSignIn.Credentials = fakeSignInCredentials{}
	before := time.Now()
	_, session := h.signIn(map[string]any{"accessToken": "ghu_alice"})
	if session.ExpiresAt.After(before.Add(maxGitHubAccessTokenLife+time.Minute)) || session.ExpiresAt.Before(before.Add(maxGitHubAccessTokenLife-time.Minute)) {
		t.Fatalf("session ends at %v, want with its token about %s from now", session.ExpiresAt, maxGitHubAccessTokenLife)
	}
	if err := h.asSession(session.Token); err != nil {
		t.Fatal(err)
	}
	h.handlers.githubSignIn.Credentials = fakeSignInCredentials{token: "ghu_alice", unrefreshable: true}
	resp, raw := h.do(http.MethodPost, "/api/v1/auth/session/renew", nil)
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(string(raw), "sign in again") {
		t.Fatalf("renew = %d %s, want 401 asking to sign in again", resp.StatusCode, raw)
	}

	kept := time.Now().Add(time.Hour)
	h.identity = nil
	h.handlers.githubSignIn.Credentials = keptSignInCredentials{expires: kept}
	status, session := h.signIn(map[string]any{"accessToken": "ghu_alice_renewed", "refreshToken": "ghr_alice", "expiresIn": 28800, "noHost": true})
	if status != http.StatusCreated || !session.ExpiresAt.Equal(kept.UTC()) {
		t.Fatalf("sign-in %d ends at %v, want with the kept link's token at %v", status, session.ExpiresAt, kept)
	}

	h.identity = nil
	h.handlers.githubSignIn.Credentials = fakeSignInCredentials{token: "ghu_alice"}
	before = time.Now()
	_, session = h.signIn(map[string]any{"accessToken": "ghu_alice", "refreshToken": "ghr_alice", "expiresIn": 28800})
	if session.ExpiresAt.Before(before.Add(SessionTTL - time.Minute)) {
		t.Fatalf("session ends at %v, want %s from now", session.ExpiresAt, SessionTTL)
	}
}

type keptSignInCredentials struct{ expires time.Time }

func (k keptSignInCredentials) HostCredential(context.Context, string, string, time.Duration) (credential.HostCredential, error) {
	return credential.HostCredential{AccessToken: "ghu_alice_renewed", ExpiresAt: k.expires}, nil
}

func TestMembershipThatCannotBeCheckedIsRetryable(t *testing.T) {
	h := newSignInHarness(t)
	revoked := &revocations{}
	h.handlers.githubSignIn.GitHub = githubauth.Client{APIURL: fakeGitHub(t, revoked).URL}
	h.identity = nil
	if status, session := h.signIn(map[string]any{"accessToken": "ghu_alice_unanswered"}); status != http.StatusServiceUnavailable || session.Token != "" {
		t.Fatalf("sign-in GitHub could not check = %d, want 503 and no session", status)
	}
	if got := revoked.list(); len(got) != 0 {
		t.Fatalf("revoked = %v, want the submitted token kept", got)
	}
	_, session := h.signIn(map[string]any{"accessToken": "ghu_alice", "refreshToken": "ghr_alice", "expiresIn": 28800})
	if err := h.asSession(session.Token); err != nil {
		t.Fatal(err)
	}
	h.handlers.githubSignIn.Credentials = fakeSignInCredentials{token: "ghu_alice_unanswered"}
	if resp, _ := h.do(http.MethodPost, "/api/v1/auth/session/renew", nil); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("renewal GitHub could not check = %d, want 503", resp.StatusCode)
	}
	if err := h.asSession(session.Token); err != nil {
		t.Fatal("a renewal that could not check membership leaves the session live")
	}
}

func TestSessionRenewalChecksMembershipAgain(t *testing.T) {
	h := newSignInHarness(t)
	h.identity = nil
	_, session := h.signIn(map[string]any{"accessToken": "ghu_alice", "refreshToken": "ghr_alice", "expiresIn": 28800})
	if err := h.asSession(session.Token); err != nil {
		t.Fatal(err)
	}
	h.handlers.githubSignIn.Credentials = fakeSignInCredentials{token: "ghu_alice_left"}
	resp, _ := h.do(http.MethodPost, "/api/v1/auth/session/renew", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("renew after leaving the org = %d, want 403", resp.StatusCode)
	}
	h.handlers.githubSignIn.Credentials = fakeSignInCredentials{token: "ghu_bob"}
	if resp, _ := h.do(http.MethodPost, "/api/v1/auth/session/renew", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("renew with another account's token = %d, want 401", resp.StatusCode)
	}
	if err := h.asSession(session.Token); err != nil {
		t.Fatal("a refused renewal leaves the session to expire on its own")
	}
}

func TestGitHubSessionsReachOnlyIdentityRoutes(t *testing.T) {
	handlers := NewHandlers(HandlersConfig{})
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, &UserInfo{AuthType: AuthTypeGitHubSession, Issuer: githubauth.Issuer, Subject: "1", Namespace: "team"})
		return c.Next()
	})
	app.Get("/api/v1/tasks", handlers.authorizeExternalRoute("GET /api/v1/tasks"), func(c fiber.Ctx) error { return c.SendStatus(http.StatusOK) })
	app.Get("/api/v1/connections", handlers.authorizeExternalRoute("GET /api/v1/connections"), func(c fiber.Ctx) error { return c.SendStatus(http.StatusOK) })
	app.Get("/mcp", handlers.authorizeExternalRoute("POST /mcp"), func(c fiber.Ctx) error { return c.SendStatus(http.StatusOK) })
	for path, want := range map[string]int{"/api/v1/tasks": http.StatusForbidden, "/api/v1/connections": http.StatusOK, "/mcp": http.StatusForbidden} {
		resp, err := app.Test(httptest.NewRequest(http.MethodGet, path, nil))
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != want {
			t.Fatalf("%s: %d, want %d", path, resp.StatusCode, want)
		}
	}
}

func TestGitHubSessionRoutesAreIdentityOnly(t *testing.T) {
	for route := range githubSessionRoutes {
		policy, ok := externalAPIPolicies[route]
		if !ok || !policy.identityOnly {
			t.Fatalf("%s is not an identity-only route", route)
		}
	}
	if githubSessionRoutes["POST /mcp"] {
		t.Fatal("the MCP endpoint is not a session's")
	}
}

func TestSessionTokensAreNeverTriedElsewhere(t *testing.T) {
	if _, err := authenticateToken(context.Background(), nil, sessionTokenPrefix+"anything", AuthConfig{}); err == nil {
		t.Fatal("a session token must fail without sign-in sessions")
	}
	called := false
	ui, err := authenticateToken(context.Background(), nil, sessionTokenPrefix+"anything", AuthConfig{
		Sessions: func(context.Context, string) (*UserInfo, error) {
			called = true
			return &UserInfo{AuthType: AuthTypeGitHubSession}, nil
		},
	})
	if err != nil || !called || ui.AuthType != AuthTypeGitHubSession {
		t.Fatalf("ui = %+v, err = %v", ui, err)
	}
}

type failingSessions struct{ store.AuthSessionStore }

func (failingSessions) GetAuthSessionByHash(context.Context, string) (store.AuthSession, error) {
	return store.AuthSession{}, errors.New("database is locked")
}

func TestSessionLookupFailureIsNotAnInvalidToken(t *testing.T) {
	cfg := GitHubSignInConfig{Enabled: true, Sessions: failingSessions{}}
	if _, err := cfg.authenticateSession(context.Background(), sessionTokenPrefix+"live"); !errors.Is(err, errSessionStoreUnavailable) {
		t.Fatalf("err = %v, want the store-unavailable error", err)
	}
	app := fiber.New()
	app.Use(NewAuthMiddleware(nil, AuthConfig{Sessions: cfg.authenticateSession}))
	app.Delete("/api/v1/auth/session", func(c fiber.Ctx) error { return c.SendStatus(http.StatusNoContent) })
	request := httptest.NewRequest(http.MethodDelete, "/api/v1/auth/session", nil)
	request.Header.Set("Authorization", "Bearer "+sessionTokenPrefix+"live")
	resp, err := app.Test(request)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

func TestGitHubSignInIsRateLimitedBeforeTheTokenCheck(t *testing.T) {
	h := newSignInHarness(t)
	upstream, err := url.Parse(fakeGitHub(t, nil).URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	var checks atomic.Int32
	counting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checks.Add(1)
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(counting.Close)
	h.handlers.githubSignIn.GitHub = githubauth.Client{APIURL: counting.URL}
	h.handlers.signInLimiter = newGatewayIngressLimiterWithLimits(100, 100, 0.001, 2, 16, time.Minute)
	body := map[string]any{"accessToken": "ghu_not_ours"}
	for i := range 2 {
		if status, _ := h.signIn(body); status != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status %d, want 401", i+1, status)
		}
	}
	if got := checks.Load(); got != 2 {
		t.Fatalf("GitHub was asked %d times, want 2", got)
	}
	if status, _ := h.signIn(body); status != http.StatusTooManyRequests {
		t.Fatalf("third attempt: status %d, want 429", status)
	}
	if got := checks.Load(); got != 2 {
		t.Fatalf("a refused attempt asked GitHub: %d checks", got)
	}
}

func TestSessionRenewalsAreRateLimited(t *testing.T) {
	h := newSignInHarness(t)
	h.identity = nil
	_, session := h.signIn(map[string]any{"accessToken": "ghu_alice", "refreshToken": "ghr_alice", "expiresIn": 28800})
	h.handlers.signInLimiter = newGatewayIngressLimiterWithLimits(100, 100, 0.001, 2, 16, time.Minute)
	for i := range 2 {
		if err := h.asSession(session.Token); err != nil {
			t.Fatal(err)
		}
		resp, raw := h.do(http.MethodPost, "/api/v1/auth/session/renew", nil)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("renewal %d = %d %s", i+1, resp.StatusCode, raw)
		}
		if err := json.Unmarshal(raw, &session); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.asSession(session.Token); err != nil {
		t.Fatal(err)
	}
	resp, raw := h.do(http.MethodPost, "/api/v1/auth/session/renew", nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("third chained renewal = %d %s, want 429", resp.StatusCode, raw)
	}
	if resp.Header.Get(fiber.HeaderRetryAfter) == "" {
		t.Fatal("a refused renewal says when to retry")
	}
}
