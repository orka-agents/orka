/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package githubauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCheckTokenAcceptsOnlyTheAppsOwnTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, secret, ok := r.BasicAuth()
		if r.Method != http.MethodPost || r.URL.Path != "/applications/Iv23app/token" || !ok || id != "Iv23app" || secret != "app-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body struct {
			AccessToken string `json:"access_token"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch body.AccessToken {
		case "ghu_ours":
			_ = json.NewEncoder(w).Encode(map[string]any{"app": map[string]any{"client_id": "Iv23app"}, "user": map[string]any{"id": 852750, "login": "alice"}})
		case "ghu_other_app":
			_ = json.NewEncoder(w).Encode(map[string]any{"app": map[string]any{"client_id": "Iv23other"}, "user": map[string]any{"id": 1, "login": "x"}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := Client{APIURL: srv.URL}
	user, err := c.CheckToken(context.Background(), "Iv23app", "app-secret", "ghu_ours")
	if err != nil || user.ID != 852750 || user.Login != "alice" {
		t.Fatalf("user = %+v, err = %v", user, err)
	}
	for _, token := range []string{"ghu_other_app", "ghu_unknown"} {
		if _, err := c.CheckToken(context.Background(), "Iv23app", "app-secret", token); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("%s: err = %v, want ErrInvalidToken", token, err)
		}
	}
	if _, err := c.CheckToken(context.Background(), "Iv23app", "wrong", "ghu_ours"); err == nil {
		t.Fatal("a wrong app secret must fail")
	}
}

func TestRequireMemberChecksOrgsAndTeamsAndFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ghu_alice" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/user/memberships/orgs/acme":
			_ = json.NewEncoder(w).Encode(map[string]string{"state": "active"})
		case "/user/memberships/orgs/pending-org":
			_ = json.NewEncoder(w).Encode(map[string]string{"state": "pending"})
		case "/orgs/acme/teams/agents/memberships/alice":
			_ = json.NewEncoder(w).Encode(map[string]string{"state": "active"})
		case "/user/memberships/orgs/broken":
			w.WriteHeader(http.StatusInternalServerError)
		case "/user/memberships/orgs/throttled":
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.WriteHeader(http.StatusForbidden)
		case "/user/memberships/orgs/slow-down":
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusForbidden)
		case "/user/memberships/orgs/forbidden":
			w.WriteHeader(http.StatusForbidden)
		case "/user/memberships/orgs/empty":
		case "/user/memberships/orgs/no-state":
			_, _ = w.Write([]byte(`{}`))
		case "/user/memberships/orgs/null-state":
			_, _ = w.Write([]byte(`{"state":null}`))
		case "/user/memberships/orgs/odd-state":
			_ = json.NewEncoder(w).Encode(map[string]string{"state": "suspended"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := Client{APIURL: srv.URL}
	member := func(token string, allowed Allowed) error {
		return c.RequireMember(context.Background(), token, "alice", allowed)
	}
	if err := member("ghu_alice", Allowed{Orgs: []string{"elsewhere", "acme"}}); err != nil {
		t.Fatalf("an active org member: %v", err)
	}
	if err := member("ghu_alice", Allowed{Teams: []Team{{Org: "acme", Slug: "agents"}}}); err != nil {
		t.Fatalf("an active team member: %v", err)
	}
	for name, allowed := range map[string]Allowed{
		"no allow-list":            {},
		"pending membership":       {Orgs: []string{"pending-org"}},
		"another team":             {Teams: []Team{{Org: "acme", Slug: "admins"}}},
		"not a member anywhere":    {Orgs: []string{"elsewhere"}},
		"org does not admit teams": {Orgs: []string{"elsewhere"}, Teams: []Team{{Org: "elsewhere", Slug: "agents"}}},
	} {
		if err := member("ghu_alice", allowed); !errors.Is(err, ErrNotMember) {
			t.Fatalf("%s: err = %v, want ErrNotMember", name, err)
		}
	}
	// GitHub failing, rate-limiting, refusing with a bare 403 (which may be
	// a secondary rate limit), or answering without a known state is no
	// membership too, but not a verdict to act on.
	for _, org := range []string{"broken", "throttled", "slow-down", "forbidden", "empty", "no-state", "null-state", "odd-state"} {
		if err := member("ghu_alice", Allowed{Orgs: []string{org}}); err == nil || errors.Is(err, ErrNotMember) {
			t.Fatalf("%s: err = %v, want a retryable error", org, err)
		}
	}
	if err := member("ghu_revoked", Allowed{Orgs: []string{"acme"}}); err == nil {
		t.Fatalf("a token GitHub refuses: err = %v", err)
	}
}

func TestParseAllowed(t *testing.T) {
	allowed, err := ParseAllowed(" acme, beta ", "acme/agents,beta/ops")
	if err != nil || len(allowed.Orgs) != 2 || allowed.Teams[1] != (Team{Org: "beta", Slug: "ops"}) {
		t.Fatalf("allowed = %+v, err = %v", allowed, err)
	}
	for _, bad := range [][2]string{{"acme/agents", ""}, {"", "agents"}, {"", "acme/"}, {"", "a/b/c"}} {
		if _, err := ParseAllowed(bad[0], bad[1]); err == nil {
			t.Fatalf("ParseAllowed(%q, %q) must fail", bad[0], bad[1])
		}
	}
}

func TestDeviceFlowWaitsForApproval(t *testing.T) {
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch r.URL.Path {
		case "/login/device/code":
			_ = json.NewEncoder(w).Encode(DeviceCode{DeviceCode: "dev", UserCode: "ABCD-1234", VerificationURI: "https://github.com/login/device", ExpiresIn: 900, Interval: 0})
		case "/login/oauth/access_token":
			if r.PostForm.Get("device_code") != "dev" || !strings.Contains(r.PostForm.Get("grant_type"), "device_code") {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			polls++
			if polls < 2 {
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
				return
			}
			_ = json.NewEncoder(w).Encode(Tokens{AccessToken: "ghu_new", RefreshToken: "ghr_new", ExpiresIn: 28800})
		}
	}))
	defer srv.Close()
	c := Client{WebURL: srv.URL, MinPollInterval: 10 * time.Millisecond}
	code, err := c.StartDeviceFlow(context.Background(), "Iv23app")
	if err != nil || code.UserCode != "ABCD-1234" {
		t.Fatalf("code = %+v, err = %v", code, err)
	}
	code.Interval = 0
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tokens, err := c.PollDeviceFlow(ctx, "Iv23app", code)
	if err != nil || tokens.AccessToken != "ghu_new" || tokens.ExpiresIn != 28800 {
		t.Fatalf("tokens = %+v, err = %v", tokens.ExpiresIn, err)
	}
}

// A server error or a dropped connection while the code is pending is no
// verdict: polling goes on, and the approval that follows is taken.
func TestDeviceFlowOutlastsTransientFailures(t *testing.T) {
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		polls++
		switch polls {
		case 1:
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("<html>bad gateway</html>"))
		case 2:
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Error("the test server cannot drop a connection")
				return
			}
			conn, _, err := hijacker.Hijack()
			if err == nil {
				_ = conn.Close()
			}
		case 3:
			w.WriteHeader(http.StatusTooManyRequests)
		case 4:
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
		default:
			_ = json.NewEncoder(w).Encode(Tokens{AccessToken: "ghu_new", RefreshToken: "ghr_new", ExpiresIn: 28800})
		}
	}))
	defer srv.Close()
	c := Client{WebURL: srv.URL, MinPollInterval: 10 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tokens, err := c.PollDeviceFlow(ctx, "Iv23app", DeviceCode{DeviceCode: "dev", ExpiresIn: 900})
	if err != nil || tokens.AccessToken != "ghu_new" || polls != 5 {
		t.Fatalf("tokens = %q, err = %v, polls = %d", tokens.AccessToken, err, polls)
	}

	// An answer GitHub gives about the request itself still ends it.
	refused := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "incorrect_client_credentials"})
	}))
	defer refused.Close()
	c.WebURL = refused.URL
	if _, err := c.PollDeviceFlow(ctx, "Iv23app", DeviceCode{DeviceCode: "dev", ExpiresIn: 900}); err == nil || errors.Is(err, ErrDeviceFlowDenied) {
		t.Fatalf("err = %v, want the request refused", err)
	}
}

func TestDeviceFlowStopsWhenRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "access_denied"})
	}))
	defer srv.Close()
	c := Client{WebURL: srv.URL, MinPollInterval: 10 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := c.PollDeviceFlow(ctx, "Iv23app", DeviceCode{DeviceCode: "dev", ExpiresIn: 900}); !errors.Is(err, ErrDeviceFlowDenied) {
		t.Fatalf("err = %v, want ErrDeviceFlowDenied", err)
	}
}

// No call follows a redirect: a 307 or 308 would replay its body, which
// carries a token or device code, to another origin.
func TestCallsNeverFollowARedirect(t *testing.T) {
	var leaked []string
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = append(leaked, r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer elsewhere.Close()
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusPermanentRedirect)
	}))
	defer redirecting.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for name, c := range map[string]Client{
		"default client": {APIURL: redirecting.URL, WebURL: redirecting.URL, MinPollInterval: time.Millisecond},
		"custom client":  {HTTP: &http.Client{}, APIURL: redirecting.URL, WebURL: redirecting.URL, MinPollInterval: time.Millisecond},
	} {
		_, _ = c.CheckToken(ctx, "Iv23app", "app-secret", "ghu_ours")
		_ = c.DeleteToken(ctx, "Iv23app", "app-secret", "ghu_ours")
		_, _ = c.PollDeviceFlow(ctx, "Iv23app", DeviceCode{DeviceCode: "dev", ExpiresIn: 1, Interval: 1})
		if len(leaked) != 0 {
			t.Fatalf("%s followed a redirect to %v", name, leaked)
		}
	}
}
