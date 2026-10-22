/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/connectors"
	"github.com/orka-agents/orka/internal/connectors/credential"
	"github.com/orka-agents/orka/internal/githubauth"
)

const signInTestUserID = "852750"

// fakeGitHub knows the app client-id, the person alice (852750) with token
// ghu_alice, a member of acme, and bob (1002) with ghu_bob, who is not.
func fakeGitHub(t *testing.T) *httptest.Server {
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
			users := map[string]map[string]any{
				"ghu_alice": {"id": 852750, "login": "alice"}, "ghu_alice_renewed": {"id": 852750, "login": "alice"},
				"ghu_alice_refreshed": {"id": 852750, "login": "alice"}, "ghu_alice_left": {"id": 852750, "login": "alice"},
				"ghu_bob": {"id": 1002, "login": "bob"}, "ghu_bob_refreshed": {"id": 1002, "login": "bob"},
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
			case "Bearer ghu_alice", "Bearer ghu_alice_renewed", "Bearer ghu_alice_refreshed":
				_ = json.NewEncoder(w).Encode(map[string]string{"state": "active"})
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

type fakeSignInCredentials struct{ token string }

func (f fakeSignInCredentials) HostCredential(context.Context, string, string, time.Duration) (credential.HostCredential, error) {
	if f.token == "" {
		return credential.HostCredential{}, credential.ErrHostLinkUnavailable
	}
	return credential.HostCredential{AccessToken: f.token}, nil
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
		GitHub:      githubauth.Client{APIURL: fakeGitHub(t).URL},
	}
	h.handlers.watchNamespace = connectorTestNamespace
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

// asSession makes the harness's next requests carry the session's identity,
// as the auth middleware resolves it.
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
		session.Namespace != connectorTestNamespace || !session.AgentHost {
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
	// The refresh token was used once to prove it is alice's: custody holds
	// what GitHub issued from it.
	held, err := h.store.GetConnectorCredential(context.Background(), ref)
	if err != nil || held.AccessToken != "ghu_alice_refreshed" || held.RefreshToken != "ghr_alice_2" || held.ExpiresAt.IsZero() {
		t.Fatalf("custody = %v", err)
	}
	if err := h.asSession(session.Token); err != nil || h.identity.Subject != signInTestUserID ||
		h.identity.AuthType != AuthTypeGitHubSession {
		t.Fatalf("session identity: %v %+v", err, h.identity)
	}

	// Signing in again with --no-host keeps the link and records the
	// person declined a host.
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
	// A member's access token with someone else's refresh token links no one.
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
	if err := json.Unmarshal(raw, &renewed); err != nil || renewed.Token == session.Token || !strings.HasPrefix(renewed.Token, sessionTokenPrefix) {
		t.Fatalf("renewed = %+v, %v", renewed, err)
	}
	if err := h.asSession(session.Token); err == nil {
		t.Fatal("a renewed session must end")
	}
	// A session renews once: its successor replaced it.
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

func TestSessionRenewalChecksMembershipAgain(t *testing.T) {
	h := newSignInHarness(t)
	h.identity = nil
	_, session := h.signIn(map[string]any{"accessToken": "ghu_alice", "refreshToken": "ghr_alice", "expiresIn": 28800})
	if err := h.asSession(session.Token); err != nil {
		t.Fatal(err)
	}
	// alice's refreshed token, but GitHub no longer places her in the org.
	h.handlers.githubSignIn.Credentials = fakeSignInCredentials{token: "ghu_alice_left"}
	resp, _ := h.do(http.MethodPost, "/api/v1/auth/session/renew", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("renew after leaving the org = %d, want 403", resp.StatusCode)
	}
	// A link that came to hold another account never renews this session,
	// even for a member.
	h.handlers.githubSignIn.Credentials = fakeSignInCredentials{token: "ghu_bob"}
	if resp, _ := h.do(http.MethodPost, "/api/v1/auth/session/renew", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("renew with another account's token = %d, want 401", resp.StatusCode)
	}
	if err := h.asSession(session.Token); err != nil {
		t.Fatal("a refused renewal leaves the session to expire on its own")
	}
}

// An Orka session reaches only the routes that act on the person's own
// links, host and AHP tokens.
func TestGitHubSessionsReachOnlyIdentityRoutes(t *testing.T) {
	handlers := NewHandlers(HandlersConfig{})
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, &UserInfo{AuthType: AuthTypeGitHubSession, Issuer: githubauth.Issuer, Subject: "1", Namespace: "team"})
		return c.Next()
	})
	app.Get("/api/v1/tasks", handlers.authorizeExternalRoute("GET /api/v1/tasks"), func(c fiber.Ctx) error { return c.SendStatus(http.StatusOK) })
	app.Get("/api/v1/connections", handlers.authorizeExternalRoute("GET /api/v1/connections"), func(c fiber.Ctx) error { return c.SendStatus(http.StatusOK) })
	for path, want := range map[string]int{"/api/v1/tasks": http.StatusForbidden, "/api/v1/connections": http.StatusOK} {
		resp, err := app.Test(httptest.NewRequest(http.MethodGet, path, nil))
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != want {
			t.Fatalf("%s: %d, want %d", path, resp.StatusCode, want)
		}
	}
}

// A session token is answered only by the session store, never tried as an
// OIDC or ServiceAccount token.
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
