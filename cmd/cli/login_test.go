package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeGitHubDeviceFlow approves the device code on the first poll.
func fakeGitHubDeviceFlow(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/login/device/code":
			json.NewEncoder(w).Encode(map[string]any{"device_code": "dev", "user_code": "ABCD-1234", "verification_uri": "https://github.com/login/device", "expires_in": 900}) //nolint:errcheck
		case "/login/oauth/access_token":
			json.NewEncoder(w).Encode(map[string]any{"access_token": "ghu_alice", "refresh_token": "ghr_alice", "expires_in": 28800}) //nolint:errcheck
		}
	}))
	t.Cleanup(srv.Close)
	previousURL, previousFloor := githubWebURL, githubPollFloor
	githubWebURL, githubPollFloor = srv.URL, time.Millisecond
	t.Cleanup(func() { githubWebURL, githubPollFloor = previousURL, previousFloor })
}

func runLoginCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestLoginSignsInLinksGitHubAndWaitsForTheHost(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fakeGitHubDeviceFlow(t)
	previousHost := agentHostReadyPollInterval
	agentHostReadyPollInterval = time.Millisecond
	t.Cleanup(func() { agentHostReadyPollInterval = previousHost })
	origBrowser := openBrowserFunc
	openBrowserFunc = func(string) error { return nil }
	t.Cleanup(func() { openBrowserFunc = origBrowser })

	var signedIn map[string]any
	orka := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/auth/config":
			json.NewEncoder(w).Encode(map[string]any{"github": map[string]any{"clientID": "Iv23app", "agentHost": true}}) //nolint:errcheck
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/auth/github":
			json.NewDecoder(r.Body).Decode(&signedIn) //nolint:errcheck
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"token": "orka_s_session", "expiresAt": time.Now().Add(12 * time.Hour), "login": "alice", "namespace": "orka-system", "agentHost": true}) //nolint:errcheck
		case r.Method == http.MethodGet && r.URL.Path == agentHostAPIPath:
			if r.Header.Get("Authorization") != "Bearer orka_s_session" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"name": "gh-alice", "environment": "orka-gh-alice", "phase": "Ready", "ready": true}) //nolint:errcheck
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer orka.Close()

	out, err := runLoginCLI(t, "login", "--server", orka.URL, "--timeout", "10s")
	if err != nil {
		t.Fatalf("login: %v %q", err, out)
	}
	for _, want := range []string{"Code running there can read your GitHub token", "enter the code ABCD-1234", "Signed in to", "as alice", "Host ready: orka-gh-alice"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	if signedIn["accessToken"] != "ghu_alice" || signedIn["refreshToken"] != "ghr_alice" || signedIn["noHost"] != false {
		t.Fatalf("sign-in body = %v", signedIn)
	}
	cfg := loadConfig()
	if cfg.Server != orka.URL || cfg.Token != "orka_s_session" || cfg.TokenKind != tokenKindGitHubSession || cfg.TokenExpiresAt == nil || cfg.Namespace != "orka-system" {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestLoginRefusesAPlainHTTPServer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := runLoginCLI(t, "login", "--server", "http://orka.example.com"); err == nil || !strings.Contains(err.Error(), "in the clear") {
		t.Fatalf("err = %v", err)
	}
}

// A session that ends soon renews itself on the next command, and is sent only
// to the server it came from.
func TestSessionRenewsBeforeItEndsAndStaysWithItsServer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var renewedWith string
	orka := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/auth/session/renew":
			renewedWith = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"token": "orka_s_renewed", "expiresAt": time.Now().Add(12 * time.Hour), "login": "alice", "namespace": "orka-system"}) //nolint:errcheck
		case r.Method == http.MethodGet && r.URL.Path == agentHostAPIPath:
			if r.Header.Get("Authorization") != "Bearer orka_s_renewed" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"name": "gh-alice", "environment": "orka-gh-alice", "phase": "Ready", "ready": true}) //nolint:errcheck
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer orka.Close()
	soon := time.Now().Add(time.Hour)
	if err := saveConfig(orkaConfig{Server: orka.URL, Token: "orka_s_old", TokenKind: tokenKindGitHubSession, TokenExpiresAt: &soon, Namespace: "orka-system"}); err != nil {
		t.Fatal(err)
	}
	out, err := runLoginCLI(t, "host", "status")
	if err != nil || renewedWith != "Bearer orka_s_old" || !strings.Contains(out, "orka-gh-alice: Ready") {
		t.Fatalf("status: %v %q, renewed with %q", err, out, renewedWith)
	}
	if cfg := loadConfig(); cfg.Token != "orka_s_renewed" || cfg.TokenExpiresAt == nil || !cfg.TokenExpiresAt.After(soon) {
		t.Fatalf("config = %+v", cfg)
	}

	// Another server never receives the session.
	var leaked bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), "orka_s_") {
			leaked = true
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer other.Close()
	_, _ = runLoginCLI(t, "host", "status", "--server", other.URL)
	if leaked {
		t.Fatal("a session must be sent only to the server it came from")
	}
}

func TestLogoutEndsTheSession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var ended string
	orka := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == "/api/v1/auth/session" {
			ended = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer orka.Close()
	later := time.Now().Add(10 * time.Hour)
	if err := saveConfig(orkaConfig{Server: orka.URL, Token: "orka_s_session", TokenKind: tokenKindGitHubSession, TokenExpiresAt: &later}); err != nil {
		t.Fatal(err)
	}
	out, err := runLoginCLI(t, "logout")
	if err != nil || ended != "Bearer orka_s_session" || !strings.Contains(out, "Signed out") {
		t.Fatalf("logout: %v %q, ended with %q", err, out, ended)
	}
	if cfg := loadConfig(); cfg.Token != "" || cfg.TokenKind != "" {
		t.Fatalf("config = %+v", cfg)
	}
}

// A redirect from the server, or a proxy in front of it, never carries the
// GitHub tokens or the session anywhere: credential requests do not follow
// redirects.
func TestCredentialsNeverFollowARedirect(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fakeGitHubDeviceFlow(t)
	var leaked bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := new(bytes.Buffer)
		body.ReadFrom(r.Body) //nolint:errcheck
		if strings.Contains(r.Header.Get("Authorization"), "orka_s_") || strings.Contains(body.String(), "ghu_") {
			leaked = true
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer elsewhere.Close()
	orka := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/auth/config" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"github": map[string]any{"clientID": "Iv23app"}}) //nolint:errcheck
			return
		}
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer orka.Close()

	if _, err := runLoginCLI(t, "login", "--server", orka.URL, "--no-open", "--no-host", "--timeout", "10s"); err == nil {
		t.Fatal("a redirected sign-in must fail")
	}
	// After a sign-in that succeeds, waiting for the host never follows a
	// redirect with the new session either.
	signedIn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/auth/config":
			json.NewEncoder(w).Encode(map[string]any{"github": map[string]any{"clientID": "Iv23app", "agentHost": true}}) //nolint:errcheck
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/auth/github":
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"token": "orka_s_new", "expiresAt": time.Now().Add(time.Hour), "login": "alice", "agentHost": true}) //nolint:errcheck
		default:
			http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusTemporaryRedirect)
		}
	}))
	defer signedIn.Close()
	_, _ = runLoginCLI(t, "login", "--server", signedIn.URL, "--no-open", "--timeout", "2s")
	later := time.Now().Add(10 * time.Hour)
	if err := saveConfig(orkaConfig{Server: orka.URL, Token: "orka_s_session", TokenKind: tokenKindGitHubSession, TokenExpiresAt: &later}); err != nil {
		t.Fatal(err)
	}
	_, _ = runLoginCLI(t, "host", "status")
	if leaked {
		t.Fatal("a redirect carried credentials to another origin")
	}
}
