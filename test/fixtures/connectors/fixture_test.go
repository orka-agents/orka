package connectorsfixture

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func TestFixtureOAuthLifecycle(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	f, plain, secure := newTestFixture(t, &now)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	// Authorize redirects back with a code bound to the PKCE challenge.
	verifier := "verifier-verifier-verifier-verifier-verifier"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	authorize := secure.URL + "/oauth/authorize?" + url.Values{
		"client_id": {"client"}, "response_type": {"code"}, "redirect_uri": {"http://localhost:1/cb"}, "state": {"s1"},
		"scope": {"read write"}, "code_challenge": {challenge}, "code_challenge_method": {"S256"},
	}.Encode()
	resp, err := client.Get(authorize)
	if err != nil || resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize = %v %v", resp, err)
	}
	location, _ := url.Parse(resp.Header.Get("Location"))
	if location.Query().Get("state") != "s1" || location.Query().Get("code") == "" {
		t.Fatalf("location = %s", location)
	}
	code := location.Query().Get("code")

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
	// A wrong verifier is refused; the right one issues a token pair.
	if _, status := exchange(url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {"http://localhost:1/cb"}, "code_verifier": {"wrong"}}); status != http.StatusBadRequest {
		t.Fatalf("wrong verifier status = %d", status)
	}
	resp, _ = client.Get(authorize)
	code = mustQuery(t, resp.Header.Get("Location"), "code")
	body, status := exchange(url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {"http://localhost:1/cb"}, "code_verifier": {verifier}})
	if status != http.StatusOK || body["token_type"] != "Bearer" || body["scope"] != "read write" || body["access_token"] == "" || body["refresh_token"] == "" {
		t.Fatalf("exchange = %d %v", status, body)
	}
	access, refresh := body["access_token"].(string), body["refresh_token"].(string)

	// The resource API honours the token until it expires.
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
	if call(http.MethodGet, access, "") != http.StatusOK || call(http.MethodPost, access, "hello") != http.StatusCreated || call(http.MethodGet, "nope", "") != http.StatusUnauthorized {
		t.Fatal("resource API must honour only issued tokens")
	}
	now = now.Add(31 * time.Second)
	if call(http.MethodGet, access, "") != http.StatusUnauthorized {
		t.Fatal("an expired token must be rejected")
	}
	// Refresh rotates the pair; the old access token stays dead.
	body, status = exchange(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}})
	if status != http.StatusOK || body["access_token"] == access {
		t.Fatalf("refresh = %d %v", status, body)
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
		t.Fatalf("revoke = %v %v", resp, err)
	}
	if call(http.MethodGet, rotated, "") != http.StatusUnauthorized {
		t.Fatal("a revoked pair must be rejected")
	}
	counters := f.Snapshot()
	if counters.Authorizations != 2 || counters.CodeExchanges != 1 || counters.Refreshes != 1 || counters.Revocations != 1 ||
		counters.Reads != 2 || counters.Writes != 1 || counters.LastWriteTitle != "hello" || counters.DistinctBearers != 2 || counters.Rejected < 3 {
		t.Fatalf("counters = %+v", counters)
	}
	// State is readable over plain HTTP and never carries tokens.
	resp, _ = http.Get(plain.URL + "/fixture/state")
	raw, _ := json.Marshal(counters)
	if resp.StatusCode != http.StatusOK || strings.Contains(string(raw), "fx-access") {
		t.Fatalf("state = %d %s", resp.StatusCode, raw)
	}
}

func TestFixtureOIDCAndModelScript(t *testing.T) {
	now := time.Now()
	_, plain, _ := newTestFixture(t, &now)
	resp, err := http.Post(plain.URL+"/oidc/mint", "application/json", strings.NewReader(`{"subject":"alice","email":"alice@example.test"}`))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("mint = %v %v", resp, err)
	}
	var minted struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&minted)
	parts := strings.Split(minted.Token, ".")
	if len(parts) != 3 {
		t.Fatalf("token = %q", minted.Token)
	}
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	if !strings.Contains(string(payload), `"iss":"http://issuer.test/oidc"`) || !strings.Contains(string(payload), `"sub":"alice"`) || !strings.Contains(string(payload), `"aud":"orka"`) {
		t.Fatalf("claims = %s", payload)
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
	approved := map[string]any{"role": "system", "content": "## Resolved Human Approvals\n\n- APPROVED ap-1 for itemswrite"}
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
		t.Fatalf("%s has no %s: %v", raw, key, err)
	}
	return parsed.Query().Get(key)
}
