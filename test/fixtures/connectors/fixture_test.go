//nolint:lll // The fixture assertions read best as single lines.
package connectorsfixture

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"
)

func newTestFixture(t *testing.T, now *time.Time) (*Fixture, *httptest.Server, *httptest.Server) {
	t.Helper()
	f, err := New(Config{
		Issuer: "http://issuer.test/oidc", Audience: "orka", ClientID: "client", ClientSecret: "secret",
		ModelCredential: "model-secret", AccessTokenTTL: 30 * time.Second, Now: func() time.Time { return *now },
	})
	if err != nil {
		t.Fatal(err)
	}
	plain := httptest.NewServer(f.PlainHandler())
	secure := httptest.NewServer(f.TLSHandler())
	t.Cleanup(plain.Close)
	t.Cleanup(secure.Close)
	return f, plain, secure
}

// linkedTokens runs authorize and code exchange with PKCE and returns the
// issued pair plus the helpers the lifecycle tests share.
type linkedTokens struct {
	f               *Fixture
	plain, secure   *httptest.Server
	client          *http.Client
	access, refresh string
	exchange        func(form url.Values) (map[string]any, int)
	call            func(method, bearer, title string) int
}

func link(t *testing.T, now *time.Time, scopes ...string) linkedTokens {
	t.Helper()
	scope := "items:read items:write"
	if len(scopes) > 0 {
		scope = strings.Join(scopes, " ")
	}
	f, plain, secure := newTestFixture(t, now)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	verifier := "verifier-verifier-verifier-verifier-verifier"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	authorize := secure.URL + "/oauth/authorize?" + url.Values{
		"client_id": {"client"}, "response_type": {"code"}, "redirect_uri": {"http://localhost:1/cb"}, "state": {"s1"},
		"scope": {scope}, "code_challenge": {challenge}, "code_challenge_method": {"S256"},
	}.Encode()
	exchange := func(form url.Values) (map[string]any, int) {
		req, _ := http.NewRequest(http.MethodPost, secure.URL+"/oauth/token", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.SetBasicAuth("client", "secret")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return body, resp.StatusCode
	}
	call := func(method, bearer, title string) int {
		var req *http.Request
		if method == http.MethodPost {
			req, _ = http.NewRequest(method, secure.URL+"/api/items", strings.NewReader(`{"title":"`+title+`"}`))
		} else {
			req, _ = http.NewRequest(method, secure.URL+"/api/items", nil)
		}
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	resp, err := client.Get(authorize)
	if err != nil || resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize status = %d err = %v", statusOf(resp), err)
	}
	location, _ := url.Parse(resp.Header.Get("Location"))
	if location.Query().Get("state") != "s1" || location.Query().Get("code") == "" {
		t.Fatalf("location query keys = %v", queryKeys(location))
	}
	code := location.Query().Get("code")
	// A wrong verifier is refused; the right one issues a token pair.
	if _, status := exchange(url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {"http://localhost:1/cb"}, "code_verifier": {"wrong"}}); status != http.StatusBadRequest {
		t.Fatalf("wrong verifier status = %d", status)
	}
	resp, _ = client.Get(authorize)
	code = mustQuery(t, resp.Header.Get("Location"), "code")
	body, status := exchange(url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {"http://localhost:1/cb"}, "code_verifier": {verifier}})
	if status != http.StatusOK || body["token_type"] != "Bearer" || body["scope"] != scope || body["access_token"] == "" || body["refresh_token"] == "" {
		t.Fatalf("exchange = %d keys=%v token_type=%v scope=%v", status, bodyKeys(body), body["token_type"], body["scope"])
	}
	return linkedTokens{f: f, plain: plain, secure: secure, client: client, access: body["access_token"].(string), refresh: body["refresh_token"].(string), exchange: exchange, call: call}
}

func TestFixtureOAuthLifecycle(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	l := link(t, &now)
	f, plain, client, access, refresh, exchange, call := l.f, l.plain, l.client, l.access, l.refresh, l.exchange, l.call
	secure := l.secure

	// The resource API honours the token until it expires.
	if call(http.MethodGet, access, "") != http.StatusOK || call(http.MethodPost, access, "hello") != http.StatusCreated || call(http.MethodGet, "nope", "") != http.StatusUnauthorized {
		t.Fatal("resource API must honour only issued tokens")
	}
	now = now.Add(31 * time.Second)
	if call(http.MethodGet, access, "") != http.StatusUnauthorized {
		t.Fatal("an expired token must be rejected")
	}
	// Refresh rotates the pair; the old access token stays dead.
	body, status := exchange(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}})
	if status != http.StatusOK || body["access_token"] == access {
		t.Fatalf("refresh = %d keys=%v rotated=%t", status, bodyKeys(body), body["access_token"] != access)
	}
	rotated := body["access_token"].(string)
	if call(http.MethodGet, rotated, "") != http.StatusOK {
		t.Fatal("the refreshed token must work")
	}
	if _, status := exchange(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}}); status != http.StatusBadRequest {
		t.Fatal("a spent refresh token must be refused")
	}
	// Revocation kills the pair.
	req, _ := http.NewRequest(http.MethodPost, secure.URL+"/oauth/revoke", strings.NewReader(url.Values{"token": {body["refresh_token"].(string)}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("client", "secret")
	if resp, err := client.Do(req); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke status = %d err = %v", statusOf(resp), err)
	}
	if call(http.MethodGet, rotated, "") != http.StatusUnauthorized {
		t.Fatal("a revoked pair must be rejected")
	}
	counters := f.Snapshot()
	if counters.LastAuthorizedScope != "items:read items:write" {
		t.Fatalf("last authorized scope = %q", counters.LastAuthorizedScope)
	}
	if counters.Authorizations != 2 || counters.CodeExchanges != 1 || counters.Refreshes != 1 || counters.Revocations != 1 ||
		counters.Reads != 2 || counters.Writes != 1 || counters.LastWriteTitle != "hello" || counters.DistinctBearers != 2 || counters.Rejected < 3 {
		t.Fatalf("counters = %+v", counters)
	}
	// State is readable over plain HTTP and never carries tokens.
	resp, _ := http.Get(plain.URL + "/fixture/state")
	raw, _ := json.Marshal(counters)
	if resp.StatusCode != http.StatusOK || strings.Contains(string(raw), "fx-access") {
		t.Fatalf("state = %d %s", resp.StatusCode, raw)
	}
}

// TestFixtureRefusesUnknownScopes covers a consent that asks for more
// than the fixture provider offers: no code is issued, so a lane whose
// consent over-asks fails instead of passing with the extra privilege.
func TestFixtureRefusesUnknownScopes(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	f, _, secure := newTestFixture(t, &now)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	authorize := secure.URL + "/oauth/authorize?" + url.Values{
		"client_id": {"client"}, "response_type": {"code"}, "redirect_uri": {"http://localhost:1/cb"}, "state": {"s1"},
		"scope": {"items:read items:write items:admin"}, "code_challenge": {"challenge"}, "code_challenge_method": {"S256"},
	}.Encode()
	resp, err := client.Get(authorize)
	if err != nil || resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize status = %d err = %v", statusOf(resp), err)
	}
	location, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || location.Query().Get("error") != "invalid_scope" || location.Query().Get("code") != "" {
		t.Fatalf("redirect error = %q code issued = %t, want invalid_scope and no code", location.Query().Get("error"), location.Query().Get("code") != "")
	}
	if snapshot := f.Snapshot(); snapshot.LastAuthorizedScope != "" {
		t.Fatalf("last authorized scope = %q, want none", snapshot.LastAuthorizedScope)
	}
}

func TestFixtureResourceAPIEnforcesGrantedScopes(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	readOnly := link(t, &now, "items:read")
	if status := readOnly.call(http.MethodGet, readOnly.access, ""); status != http.StatusOK {
		t.Fatalf("read with items:read status = %d", status)
	}
	// A consent that granted only reads cannot write, so a lane whose
	// consent stopped asking for items:write fails at the write.
	if status := readOnly.call(http.MethodPost, readOnly.access, "nope"); status != http.StatusForbidden {
		t.Fatalf("write with items:read only status = %d", status)
	}
	if snapshot := readOnly.f.Snapshot(); snapshot.Writes != 0 || snapshot.Rejected != 1 {
		t.Fatalf("counters after refused write = %+v", snapshot)
	}
}

func TestFixtureOIDCAndModelScript(t *testing.T) {
	now := time.Now()
	_, plain, _ := newTestFixture(t, &now)
	resp, err := http.Post(plain.URL+"/oidc/mint", "application/json", strings.NewReader(`{"subject":"alice","email":"alice@example.test"}`))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("mint status = %d err = %v", statusOf(resp), err)
	}
	var minted struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&minted)
	parts := strings.Split(minted.Token, ".")
	if len(parts) != 3 {
		t.Fatalf("minted token has %d parts, want 3", len(parts))
	}
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	_ = json.Unmarshal(payload, &claims)
	if claims["iss"] != "http://issuer.test/oidc" || claims["sub"] != "alice" || claims["aud"] != "orka" {
		t.Fatalf("claims iss=%v sub=%v aud=%v", claims["iss"], claims["sub"], claims["aud"])
	}
	resp, _ = http.Get(plain.URL + "/oidc/.well-known/openid-configuration")
	var discovery map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&discovery)
	if discovery["jwks_uri"] != "http://issuer.test/oidc/jwks" {
		t.Fatalf("discovery = %v", discovery)
	}
	resp, _ = http.Get(plain.URL + "/oidc/jwks")
	var jwks struct {
		Keys []map[string]string `json:"keys"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&jwks)
	if len(jwks.Keys) != 1 || jwks.Keys[0]["alg"] != "RS256" || jwks.Keys[0]["kid"] == "" {
		t.Fatalf("jwks = %v", jwks)
	}

	// The model script: read, write (parks); after approval write, read, plan, done.
	turn := func(messages []map[string]any, instructions string) (string, string) {
		body, _ := json.Marshal(map[string]any{"model": "fixture", "messages": messages, "tools": []any{map[string]any{"type": "function"}}})
		req, _ := http.NewRequest(http.MethodPost, plain.URL+"/v1/chat/completions", strings.NewReader(string(body)))
		req.Header.Set("Authorization", "Bearer model-secret")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var out struct {
			Choices []struct {
				Message struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"message"`
			} `json:"choices"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		if len(out.Choices[0].Message.ToolCalls) > 0 {
			return out.Choices[0].Message.ToolCalls[0].Function.Name, out.Choices[0].Message.ToolCalls[0].ID
		}
		_ = instructions
		return "", out.Choices[0].Message.Content
	}
	user := map[string]any{"role": "user", "content": "exercise the linked account"}
	if name, _ := turn([]map[string]any{user}, ""); name != ReadToolName {
		t.Fatalf("first turn = %q", name)
	}
	readResult := map[string]any{"role": "tool", "tool_call_id": "read1-x", "content": `{"items":[]}`}
	if name, _ := turn([]map[string]any{user, readResult}, ""); name != WriteToolName {
		t.Fatalf("second turn = %q", name)
	}
	approvedText := "## Resolved Human Approvals\n\n- APPROVED ap-1 for itemswrite"
	approved := map[string]any{"role": "system", "content": approvedText}
	if name, _ := turn([]map[string]any{approved, user}, ""); name != WriteToolName {
		t.Fatal("after approval the write must be reissued first")
	}
	writeResult := map[string]any{"role": "tool", "tool_call_id": "write-x", "content": `{"status":"created"}`}
	if name, _ := turn([]map[string]any{approved, user, writeResult}, ""); name != ReadToolName {
		t.Fatal("after the write the read must run again")
	}
	readTwo := map[string]any{"role": "tool", "tool_call_id": "read2-x", "content": `{"items":[]}`}
	if name, _ := turn([]map[string]any{approved, user, writeResult, readTwo}, ""); name != "update_plan" {
		t.Fatal("then the plan must be closed")
	}
	plan := map[string]any{"role": "tool", "tool_call_id": "plan-x", "content": `{"ok":true}`}
	if name, content := turn([]map[string]any{approved, user, writeResult, readTwo, plan}, ""); name != "" || !strings.Contains(content, "CONNECTORS_E2E_DONE") {
		t.Fatalf("final = %q %q", name, content)
	}
	req, _ := http.NewRequest(http.MethodPost, plain.URL+"/v1/chat/completions", strings.NewReader(`{}`))
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatal("the model must require its credential")
	}
}

func mustQuery(t *testing.T, raw, key string) string {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Query().Get(key) == "" {
		t.Fatalf("redirect has no %s (keys %v): %v", key, queryKeys(parsed), err)
	}
	return parsed.Query().Get(key)
}

// queryKeys and bodyKeys describe a response without its values, so a
// failing assertion never prints a code, state, or token.
func queryKeys(location *url.URL) []string {
	keys := make([]string, 0, len(location.Query()))
	for key := range location.Query() {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func bodyKeys(body map[string]any) []string {
	keys := make([]string, 0, len(body))
	for key := range body {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// statusOf reports only a response's status: a redirect's Location would
// carry a live authorization code and state into test output.
func statusOf(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}
