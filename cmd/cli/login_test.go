package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/orka-agents/orka/internal/cli/client"
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
	hostPolls := 0
	orka := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/auth/config":
			json.NewEncoder(w).Encode(map[string]any{"github": map[string]any{"clientID": "Iv23app", "agentHost": true}}) //nolint:errcheck
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/auth/github":
			json.NewDecoder(r.Body).Decode(&signedIn) //nolint:errcheck
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"token": "orka_s_session", "expiresAt": time.Now().Add(12 * time.Hour), "login": "alice", "namespace": "orka-system", "connection": "github-alice", "agentHost": true}) //nolint:errcheck
		case r.Method == http.MethodGet && r.URL.Path == agentHostAPIPath:
			if r.Header.Get("Authorization") != "Bearer orka_s_session" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			hostPolls++
			if hostPolls == 1 {
				// Until the new link's host exists, a ready host kept from
				// another link is not the one to use.
				json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
					"name": "gh-alice-old", "environment": "orka-gh-alice-old", "connection": "github-old-alice", "phase": "Ready", "ready": true,
				})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"name": "gh-alice", "environment": "orka-gh-alice", "connection": "github-alice", "phase": "Ready", "ready": true}) //nolint:errcheck
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer orka.Close()

	out, err := runLoginCLI(t, "login", "--server", orka.URL, "--timeout", "10s")
	if err != nil {
		t.Fatalf("login: %v %q", err, out)
	}
	for _, want := range []string{"Code running there can read your GitHub token", "enter the code ABCD-1234", "Signed in to", "as alice", "Host ready: orka-gh-alice."} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	if signedIn["accessToken"] != "ghu_alice" || signedIn["refreshToken"] != "ghr_alice" || signedIn["noHost"] != false {
		t.Fatalf("sign-in body = %v", signedIn)
	}
	cfg := loadConfig()
	if cfg.Server != orka.URL || cfg.SessionServer != orka.URL || cfg.Token != "orka_s_session" || cfg.TokenKind != tokenKindGitHubSession ||
		cfg.TokenExpiresAt == nil || cfg.Namespace != "orka-system" || cfg.SessionLogin != "alice" {
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
	// A kubeconfig with a bearer token, which must not stand in for a session
	// withheld from another server.
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(kubeconfig, []byte(`apiVersion: v1
kind: Config
clusters: [{name: c, cluster: {server: "https://127.0.0.1:1"}}]
users: [{name: u, user: {token: kube-bearer-token}}]
contexts: [{name: x, context: {cluster: c, user: u, namespace: team}}]
current-context: x
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", kubeconfig)
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
			json.NewEncoder(w).Encode(map[string]any{"name": "gh-alice", "environment": "orka-gh-alice", "connection": "github-alice", "phase": "Ready", "ready": true}) //nolint:errcheck
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer orka.Close()
	soon := time.Now().Add(time.Hour)
	if err := saveConfig(orkaConfig{Server: orka.URL, SessionServer: orka.URL, Token: "orka_s_old", TokenKind: tokenKindGitHubSession, TokenExpiresAt: &soon, Namespace: "orka-system"}); err != nil {
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
		if auth := r.Header.Get("Authorization"); strings.Contains(auth, "orka_s_") || strings.Contains(auth, "kube-bearer-token") {
			leaked = true
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer other.Close()
	_, _ = runLoginCLI(t, "host", "status", "--server", other.URL)
	if leaked {
		t.Fatal("a session, or a Kubernetes credential in its place, must be sent only to the session's server")
	}
	// Nor after the configured server changes.
	if _, err := runLoginCLI(t, "config", "set-server", other.URL); err != nil {
		t.Fatal(err)
	}
	_, _ = runLoginCLI(t, "host", "status")
	if leaked {
		t.Fatal("changing the configured server must not redirect the session")
	}
}

// A command that read one person's session in its renewal window does not
// renew or use another person's session saved before it took the config
// lock. A successor of its own session, renewed by another process, is used.
func TestSessionRenewalLeavesAnotherPersonsSessionAlone(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	renewals := 0
	orka := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auth/session/renew" {
			renewals++
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer orka.Close()
	soon := time.Now().Add(time.Hour)
	alice := orkaConfig{Server: orka.URL, SessionServer: orka.URL, Token: "orka_s_alice", TokenKind: tokenKindGitHubSession, TokenExpiresAt: &soon, SessionLogin: "alice"}
	bob := alice
	bob.Token, bob.SessionLogin = "orka_s_bob", "bob"
	if err := saveConfig(bob); err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{}
	cmd.SetErr(io.Discard)
	if got := configuredSession(cmd, orka.URL, alice); got != "orka_s_alice" || renewals != 0 {
		t.Fatalf("token = %q after %d renewals, want alice's own session and no renewal", got, renewals)
	}
	successor := alice
	later := time.Now().Add(12 * time.Hour)
	successor.Token, successor.TokenExpiresAt = "orka_s_alice_2", &later
	if err := saveConfig(successor); err != nil {
		t.Fatal(err)
	}
	if got := configuredSession(cmd, orka.URL, alice); got != "orka_s_alice_2" || renewals != 0 {
		t.Fatalf("token = %q after %d renewals, want the successor", got, renewals)
	}
}

// Two orka processes in a session's renewal window renew it once and both
// use the successor: the server renews a session only once.
func TestConcurrentCommandsRenewASessionOnce(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var renewals atomic.Int32
	orka := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/auth/session/renew" || r.Header.Get("Authorization") != "Bearer orka_s_old" ||
			renewals.Add(1) > 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"token": "orka_s_renewed", "expiresAt": time.Now().Add(12 * time.Hour)}) //nolint:errcheck
	}))
	defer orka.Close()
	soon := time.Now().Add(time.Hour)
	cfg := orkaConfig{Server: orka.URL, SessionServer: orka.URL, Token: "orka_s_old", TokenKind: tokenKindGitHubSession, TokenExpiresAt: &soon}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	tokens := make([]string, 4)
	var wg sync.WaitGroup
	for i := range tokens {
		wg.Go(func() {
			cmd := &cobra.Command{}
			cmd.SetErr(new(bytes.Buffer))
			tokens[i] = configuredSession(cmd, "", cfg)
		})
	}
	wg.Wait()
	for i, token := range tokens {
		if token != "orka_s_renewed" {
			t.Fatalf("command %d used %q, want the successor", i, token)
		}
	}
	if got := renewals.Load(); got != 1 {
		t.Fatalf("renewals = %d, want 1", got)
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
	// Logout ends the session at the server that issued it, wherever the
	// configured server points now.
	if err := saveConfig(orkaConfig{Server: "https://elsewhere.example", SessionServer: orka.URL, Token: "orka_s_session",
		TokenKind: tokenKindGitHubSession, TokenExpiresAt: &later}); err != nil {
		t.Fatal(err)
	}
	out, err := runLoginCLI(t, "logout")
	if err != nil || ended != "Bearer orka_s_session" || !strings.Contains(out, "Signed out") {
		t.Fatalf("logout: %v %q, ended with %q", err, out, ended)
	}
	if cfg := loadConfig(); cfg.Token != "" || cfg.TokenKind != "" || cfg.SessionServer != "" || cfg.SessionLogin != "" {
		t.Fatalf("config = %+v", cfg)
	}

	// A session whose saved server would carry it in the clear is not sent.
	ended = ""
	if err := saveConfig(orkaConfig{Server: orka.URL, SessionServer: "http://orka.example.com", Token: "orka_s_session",
		TokenKind: tokenKindGitHubSession, TokenExpiresAt: &later}); err != nil {
		t.Fatal(err)
	}
	if _, err := runLoginCLI(t, "logout"); err == nil || ended != "" {
		t.Fatalf("logout to a plain-HTTP server: %v, ended with %q", err, ended)
	}
}

// A token set by hand is not a session from orka login.
func TestSetTokenReplacesASession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	later := time.Now().Add(10 * time.Hour)
	if err := saveConfig(orkaConfig{Server: "https://orka.example", SessionServer: "https://orka.example", Token: "orka_s_session",
		TokenKind: tokenKindGitHubSession, TokenExpiresAt: &later}); err != nil {
		t.Fatal(err)
	}
	if _, err := runLoginCLI(t, "config", "set-token", "static-token"); err != nil {
		t.Fatal(err)
	}
	if cfg := loadConfig(); cfg.Token != "static-token" || cfg.TokenKind != "" || cfg.TokenExpiresAt != nil || cfg.SessionServer != "" {
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
			json.NewEncoder(w).Encode(map[string]any{"token": "orka_s_new", "expiresAt": time.Now().Add(time.Hour), "login": "alice", "connection": "github-alice", "agentHost": true}) //nolint:errcheck
		default:
			http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusTemporaryRedirect)
		}
	}))
	defer signedIn.Close()
	_, _ = runLoginCLI(t, "login", "--server", signedIn.URL, "--no-open", "--timeout", "2s")
	later := time.Now().Add(10 * time.Hour)
	if err := saveConfig(orkaConfig{Server: orka.URL, SessionServer: orka.URL, Token: "orka_s_session", TokenKind: tokenKindGitHubSession,
		TokenExpiresAt: &later}); err != nil {
		t.Fatal(err)
	}
	_, _ = runLoginCLI(t, "host", "status")
	if leaked {
		t.Fatal("a redirect carried credentials to another origin")
	}
}

// A config write during a renewal waits for it and builds on the saved
// successor, so neither undoes the other.
func TestConfigWritesWaitForARenewal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	setter := make(chan error, 1)
	orka := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Another command changes the namespace while this renewal runs.
		go func() {
			_, err := runLoginCLI(t, "config", "set-namespace", "team")
			setter <- err
		}()
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"token": "orka_s_renewed", "expiresAt": time.Now().Add(12 * time.Hour)}) //nolint:errcheck
	}))
	defer orka.Close()
	soon := time.Now().Add(time.Hour)
	cfg := orkaConfig{Server: orka.URL, SessionServer: orka.URL, Token: "orka_s_old", TokenKind: tokenKindGitHubSession, TokenExpiresAt: &soon}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{}
	cmd.SetErr(new(bytes.Buffer))
	if token := configuredSession(cmd, "", cfg); token != "orka_s_renewed" {
		t.Fatalf("token = %q", token)
	}
	if err := <-setter; err != nil {
		t.Fatal(err)
	}
	if saved := loadConfig(); saved.Token != "orka_s_renewed" || saved.Namespace != "team" {
		t.Fatalf("config = %+v, want the successor and the namespace", saved)
	}
}

// A lock file left by a process that exited holds nothing: the lock is the
// operating system's, and only one caller holds it at a time.
func TestConfigLockIsExclusiveAndNeverStale(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Dir(configPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath()+".lock", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var holders, most atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			unlock, err := lockConfig(ctx)
			if err != nil {
				t.Error(err)
				return
			}
			now := holders.Add(1)
			for {
				seen := most.Load()
				if now <= seen || most.CompareAndSwap(seen, now) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			holders.Add(-1)
			unlock()
		})
	}
	wg.Wait()
	if got := most.Load(); got != 1 {
		t.Fatalf("%d callers held the config lock at once", got)
	}
}

// Logout waits for a renewal in flight and ends the session it saved, not
// the one it replaced.
func TestLogoutWaitsForARenewal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var ended atomic.Value
	orka := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ended.Store(r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer orka.Close()
	later := time.Now().Add(10 * time.Hour)
	if err := saveConfig(orkaConfig{Server: orka.URL, SessionServer: orka.URL, Token: "orka_s_old", TokenKind: tokenKindGitHubSession,
		TokenExpiresAt: &later}); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := runLoginCLI(t, "logout")
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	if err := saveConfig(orkaConfig{Server: orka.URL, SessionServer: orka.URL, Token: "orka_s_successor", TokenKind: tokenKindGitHubSession,
		TokenExpiresAt: &later}); err != nil {
		t.Fatal(err)
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got, _ := ended.Load().(string); got != "Bearer orka_s_successor" {
		t.Fatalf("logout ended %q, want the saved successor", got)
	}
}

// When the server offers no host, so the CLI showed no disclosure, the
// sign-in declines a host even if one is enabled while the code is pending.
func TestLoginDeclinesAHostItDidNotDisclose(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fakeGitHubDeviceFlow(t)
	var signedIn map[string]any
	orka := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/auth/config":
			json.NewEncoder(w).Encode(map[string]any{"github": map[string]any{"clientID": "Iv23app", "agentHost": false}}) //nolint:errcheck
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/auth/github":
			json.NewDecoder(r.Body).Decode(&signedIn) //nolint:errcheck
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"token": "orka_s_session", "expiresAt": time.Now().Add(12 * time.Hour), "login": "alice"}) //nolint:errcheck
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer orka.Close()
	if out, err := runLoginCLI(t, "login", "--server", orka.URL, "--no-open", "--timeout", "10s"); err != nil {
		t.Fatalf("login: %v %q", err, out)
	}
	if signedIn["noHost"] != true {
		t.Fatalf("sign-in body = %v, want noHost", signedIn)
	}
}

// Signing in over a token saved by hand says, before the code, that the token
// will be replaced. Signing in over a session says nothing.
func TestLoginWarnsBeforeReplacingASavedToken(t *testing.T) {
	fakeGitHubDeviceFlow(t)
	orka := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/auth/config":
			json.NewEncoder(w).Encode(map[string]any{"github": map[string]any{"clientID": "Iv23app", "agentHost": false}}) //nolint:errcheck
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/auth/github":
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"token": "orka_s_session", "expiresAt": time.Now().Add(12 * time.Hour), "login": "alice", "namespace": "orka-system"}) //nolint:errcheck
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer orka.Close()
	const warning = "Signing in replaces the token and namespace saved in your Orka config."
	later := time.Now().Add(10 * time.Hour)
	for _, tc := range []struct {
		name  string
		saved orkaConfig
		warns bool
	}{
		{name: "token set by hand", saved: orkaConfig{Server: orka.URL, Token: "oidc-token", Namespace: "team-a"}, warns: true},
		{name: "session", saved: orkaConfig{Server: orka.URL, SessionServer: orka.URL, Token: "orka_s_old", TokenKind: tokenKindGitHubSession,
			TokenExpiresAt: &later, SessionLogin: "alice"}},
		{name: "nothing saved", saved: orkaConfig{Server: orka.URL}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			if err := saveConfig(tc.saved); err != nil {
				t.Fatal(err)
			}
			out, err := runLoginCLI(t, "login", "--server", orka.URL, "--no-open", "--timeout", "10s")
			if err != nil {
				t.Fatalf("login: %v %q", err, out)
			}
			warned, code := strings.Index(out, warning), strings.Index(out, "enter the code")
			if (warned >= 0) != tc.warns || (tc.warns && warned > code) {
				t.Fatalf("want warning %v, before the code:\n%s", tc.warns, out)
			}
			if cfg := loadConfig(); cfg.Token != "orka_s_session" || cfg.Namespace != "orka-system" {
				t.Fatalf("config = %+v", cfg)
			}
		})
	}
}

// Readers never see a partly written config while another command saves it.
func TestConfigSavesAreAtomic(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := orkaConfig{Server: "https://orka.example", Namespace: strings.Repeat("n", 4096)}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
				if err := saveConfig(cfg); err != nil {
					t.Error(err)
					return
				}
			}
		}
	})
	for range 2000 {
		if got := loadConfig(); got.Server != cfg.Server {
			close(stop)
			wg.Wait()
			t.Fatalf("a reader saw a partial config: %+v", got.Server)
		}
	}
	close(stop)
	wg.Wait()
}

// A host kept from before the person left the org still reports that until
// the controller sees the new sign-in; login waits through it.
func TestLoginWaitsThroughAStaleMembershipFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fakeGitHubDeviceFlow(t)
	previousHost := agentHostReadyPollInterval
	agentHostReadyPollInterval = time.Millisecond
	t.Cleanup(func() { agentHostReadyPollInterval = previousHost })
	var polls atomic.Int32
	orka := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/auth/config":
			json.NewEncoder(w).Encode(map[string]any{"github": map[string]any{"clientID": "Iv23app", "agentHost": true}}) //nolint:errcheck
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/auth/github":
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"token": "orka_s_session", "expiresAt": time.Now().Add(12 * time.Hour), "login": "alice", "connection": "github-alice", "agentHost": true}) //nolint:errcheck
		case r.Method == http.MethodGet && r.URL.Path == agentHostAPIPath:
			if polls.Add(1) < 3 {
				json.NewEncoder(w).Encode(map[string]any{"name": "gh-alice", "connection": "github-alice", "phase": "Failed", "reason": "GitHubMembershipEnded", "message": "left"}) //nolint:errcheck
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"name": "gh-alice", "environment": "orka-gh-alice", "connection": "github-alice", "phase": "Ready", "ready": true}) //nolint:errcheck
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer orka.Close()
	out, err := runLoginCLI(t, "login", "--server", orka.URL, "--no-open", "--timeout", "10s")
	if err != nil || !strings.Contains(out, "Host ready: orka-gh-alice") {
		t.Fatalf("login: %v %q", err, out)
	}
}

// An answer cut off after its headers is a failure: neither login nor a
// renewal saves an empty session over the one it holds.
func TestATruncatedAnswerSavesNoSession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fakeGitHubDeviceFlow(t)
	orka := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/auth/config" {
			json.NewEncoder(w).Encode(map[string]any{"github": map[string]any{"clientID": "Iv23app"}}) //nolint:errcheck
			return
		}
		// A 201 that promises a body and closes before sending it.
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusCreated)
	}))
	defer orka.Close()
	soon := time.Now().Add(time.Hour)
	held := orkaConfig{Server: orka.URL, SessionServer: orka.URL, Token: "orka_s_held", TokenKind: tokenKindGitHubSession, TokenExpiresAt: &soon}
	if err := saveConfig(held); err != nil {
		t.Fatal(err)
	}
	if _, err := runLoginCLI(t, "login", "--server", orka.URL, "--no-open", "--no-host", "--timeout", "10s"); err == nil {
		t.Fatal("a truncated sign-in answer must fail")
	}
	cmd := &cobra.Command{}
	cmd.SetErr(new(bytes.Buffer))
	if token := configuredSession(cmd, "", held); token != "orka_s_held" {
		t.Fatalf("token = %q, want the held session", token)
	}
	if saved := loadConfig(); saved.Token != "orka_s_held" {
		t.Fatalf("config token = %q, want the held session", saved.Token)
	}
}

// A returning person's host may be suspended: one that idled out is woken,
// and one an operator suspended is reported, not overridden.
func TestLoginWakesAnIdleHostAndReportsAnOperatorSuspension(t *testing.T) {
	previousHost := agentHostReadyPollInterval
	agentHostReadyPollInterval = time.Millisecond
	t.Cleanup(func() { agentHostReadyPollInterval = previousHost })
	for name, operator := range map[string]bool{"idle": false, "operator": true} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			fakeGitHubDeviceFlow(t)
			var wakes atomic.Int32
			orka := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1/auth/config":
					json.NewEncoder(w).Encode(map[string]any{"github": map[string]any{"clientID": "Iv23app", "agentHost": true}}) //nolint:errcheck
				case r.Method == http.MethodPost && r.URL.Path == "/api/v1/auth/github":
					w.WriteHeader(http.StatusCreated)
					json.NewEncoder(w).Encode(map[string]any{"token": "orka_s_session", "expiresAt": time.Now().Add(12 * time.Hour), "login": "alice", "connection": "github-alice", "agentHost": true}) //nolint:errcheck
				case r.Method == http.MethodPost && r.URL.Path == agentHostAPIPath+"/wake":
					wakes.Add(1)
					json.NewEncoder(w).Encode(map[string]any{"name": "gh-alice", "suspended": true}) //nolint:errcheck
				case r.Method == http.MethodGet && r.URL.Path == agentHostAPIPath:
					if operator || wakes.Load() == 0 {
						json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
							"name": "gh-alice", "connection": "github-alice", "phase": "Suspended", "suspended": true, "operatorSuspended": operator,
						})
						return
					}
					json.NewEncoder(w).Encode(map[string]any{"name": "gh-alice", "environment": "orka-gh-alice", "connection": "github-alice", "phase": "Ready", "ready": true}) //nolint:errcheck
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer orka.Close()
			out, err := runLoginCLI(t, "login", "--server", orka.URL, "--no-open", "--timeout", "10s")
			if operator {
				if err == nil || !strings.Contains(err.Error(), "an operator suspended") || wakes.Load() != 0 {
					t.Fatalf("login with an operator-suspended host: %v, %d wakes", err, wakes.Load())
				}
				return
			}
			if err != nil || !strings.Contains(out, "Host ready: orka-gh-alice") || wakes.Load() != 1 {
				t.Fatalf("login with an idle host: %v %q, %d wakes", err, out, wakes.Load())
			}
		})
	}
}

// A logout the server could not process keeps the saved session, so it can
// be ended on a retry instead of left live.
func TestLogoutKeepsTheSessionWhenTheServerCannotAnswer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	orka := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer orka.Close()
	later := time.Now().Add(10 * time.Hour)
	if err := saveConfig(orkaConfig{Server: orka.URL, SessionServer: orka.URL, Token: "orka_s_live", TokenKind: tokenKindGitHubSession,
		TokenExpiresAt: &later}); err != nil {
		t.Fatal(err)
	}
	if _, err := runLoginCLI(t, "logout"); err == nil {
		t.Fatal("a logout the server could not process must fail")
	}
	if cfg := loadConfig(); cfg.Token != "orka_s_live" || cfg.TokenKind != tokenKindGitHubSession {
		t.Fatalf("config = %+v, want the session kept", cfg)
	}
}

// An operator's suspension wins over a Ready left from the host's last run.
func TestWaitReportsAnOperatorSuspensionOverAStaleReady(t *testing.T) {
	previousHost := agentHostReadyPollInterval
	agentHostReadyPollInterval = time.Millisecond
	t.Cleanup(func() { agentHostReadyPollInterval = previousHost })
	var wakes atomic.Int32
	orka := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			wakes.Add(1)
		}
		json.NewEncoder(w).Encode(map[string]any{"name": "gh-alice", "environment": "orka-gh-alice", "connection": "github-alice", "phase": "Ready", "ready": true, //nolint:errcheck
			"suspended": true, "operatorSuspended": true})
	}))
	defer orka.Close()
	cmd := &cobra.Command{}
	cmd.SetOut(new(bytes.Buffer))
	hosts := client.NewWithNamespace(orka.URL, "orka_s_session", "orka-system")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := waitForAgentHost(ctx, cmd, hosts, "github-alice"); err == nil || !strings.Contains(err.Error(), "an operator suspended") || wakes.Load() != 0 {
		t.Fatalf("wait = %v, %d wakes", err, wakes.Load())
	}
}
