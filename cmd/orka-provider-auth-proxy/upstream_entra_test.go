/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const (
	testEntraTenant      = "11111111-1111-1111-1111-111111111111"
	testEntraClient      = "22222222-2222-2222-2222-222222222222"
	testProxyCredential  = "proxy-sa-token"
	testUpstreamToken    = "upstream-data-plane-token"
	testUpstreamBasePath = "/openai/v1"
)

// fakeAzureResource serves an Entra token endpoint and an Azure OpenAI v1
// endpoint on one https origin.
type fakeAzureResource struct {
	mu          sync.Mutex
	issuerState int // http status the token endpoint answers; 0 means 200
	tokenForms  []map[string]string
	modelCalls  []modelCall
}

type modelCall struct {
	path, authorization, apiKey, model string
}

func (f *fakeAzureResource) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /"+testEntraTenant+"/oauth2/v2.0/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		f.mu.Lock()
		f.tokenForms = append(f.tokenForms, map[string]string{
			"client_id": r.PostForm.Get("client_id"), "scope": r.PostForm.Get("scope"),
			"client_assertion": r.PostForm.Get("client_assertion"),
		})
		status := f.issuerState
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if status != 0 {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_client"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": testUpstreamToken, "expires_in": 3599})
	})
	mux.HandleFunc("POST "+testUpstreamBasePath+"/responses", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.modelCalls = append(f.modelCalls, modelCall{
			path: r.URL.Path, authorization: r.Header.Get("Authorization"), apiKey: r.Header.Get("Api-Key"), model: body.Model,
		})
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_1","status":"completed"}`)
	})
	return mux
}

func (f *fakeAzureResource) calls() ([]map[string]string, []modelCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]string(nil), f.tokenForms...), append([]modelCall(nil), f.modelCalls...)
}

func testEntraIdentities(t *testing.T, authority, allowed string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"identities": []any{map[string]any{
		"name": "models", "tenantID": testEntraTenant, "clientID": testEntraClient,
		"authority": authority, "allowedEndpoints": []string{allowed},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func testProxyCredentialFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(testProxyCredential+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// newEntraTestProxy returns a proxy whose upstream is server's Azure OpenAI
// v1 base URL, authenticated with the "models" identity.
func newEntraTestProxy(t *testing.T, server *httptest.Server) *providerAuthProxy {
	t.Helper()
	tokens, err := newStaticBearerTokenStore([]byte(testSharedProviderToken))
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := newProviderAuthProxyWithTokenStore(proxyConfig{
		UpstreamBaseURL: server.URL + testUpstreamBasePath,
		UpstreamEntra: upstreamEntraConfig{
			Identities: testEntraIdentities(t, server.URL, server.URL),
			Identity:   "models", TokenFile: testProxyCredentialFile(t),
		},
	}, tokens)
	if err != nil {
		t.Fatal(err)
	}
	// Trust the test server's certificate for both the issuer and upstream.
	proxy.client.Transport = server.Client().Transport
	proxy.upstreamEntra.source.HTTPClient = server.Client()
	return proxy
}

func sendThroughProxy(proxy *providerAuthProxy, model string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(`{"model":"`+model+`","input":"hi"}`))
	request.Header.Set(authorizationHeader, "Bearer "+testSharedProviderToken)
	request.Header.Set("Api-Key", "caller-key")
	response := httptest.NewRecorder()
	proxy.ServeHTTP(response, request)
	return response
}

// The upstream sees only the proxy's own Entra token, under the v1 base
// path, with the caller's model unchanged; the caller never sees the token.
func TestUpstreamEntraAuthenticatesTheUpstreamWithTheProxysIdentity(t *testing.T) {
	fake := &fakeAzureResource{}
	server := httptest.NewTLSServer(fake.handler(t))
	t.Cleanup(server.Close)
	proxy := newEntraTestProxy(t, server)

	for _, model := range []string{"gpt-6.2-sol", "gpt-6-luna"} {
		response := sendThroughProxy(proxy, model)
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), testUpstreamToken) || strings.Contains(strings.Join(headerValues(response.Header()), " "), testUpstreamToken) {
			t.Fatal("the upstream token reached the caller")
		}
	}
	forms, calls := fake.calls()
	if len(calls) != 2 || calls[0].model != "gpt-6.2-sol" || calls[1].model != "gpt-6-luna" {
		t.Fatalf("model calls = %+v, want each caller's model unchanged", calls)
	}
	for _, call := range calls {
		if call.authorization != "Bearer "+testUpstreamToken || call.apiKey != "" || call.path != testUpstreamBasePath+"/responses" {
			t.Fatalf("upstream call = %+v, want only the proxy's Entra token at %s/responses", call, testUpstreamBasePath)
		}
	}
	if len(forms) != 1 {
		t.Fatalf("token requests = %d, want one cached token", len(forms))
	}
	if forms[0]["client_assertion"] != testProxyCredential || forms[0]["client_id"] != testEntraClient ||
		forms[0]["scope"] != "https://ai.azure.com/.default" {
		t.Fatalf("token request = %+v, want the proxy's credential for the identity's scope", forms[0])
	}
}

// Without a token the proxy forwards nothing and answers a retryable 503;
// the next request after the issuer recovers succeeds.
func TestUpstreamEntraTokenFailureFailsClosedAndRecovers(t *testing.T) {
	for name, status := range map[string]int{"refused": http.StatusUnauthorized, "unavailable": http.StatusServiceUnavailable} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeAzureResource{issuerState: status}
			server := httptest.NewTLSServer(fake.handler(t))
			t.Cleanup(server.Close)
			proxy := newEntraTestProxy(t, server)

			response := sendThroughProxy(proxy, "gpt-6-luna")
			if response.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", response.Code)
			}
			if strings.Contains(response.Body.String(), testProxyCredential) {
				t.Fatal("the proxy's credential reached the caller")
			}
			if _, calls := fake.calls(); len(calls) != 0 {
				t.Fatalf("upstream calls = %d, want none without a token", len(calls))
			}

			fake.mu.Lock()
			fake.issuerState = 0
			fake.mu.Unlock()
			if response := sendThroughProxy(proxy, "gpt-6-luna"); response.Code != http.StatusOK {
				t.Fatalf("status after recovery = %d, want 200", response.Code)
			}
		})
	}
}

func TestUpstreamEntraConfigurationFailsClosed(t *testing.T) {
	tokens, err := newStaticBearerTokenStore([]byte(testSharedProviderToken))
	if err != nil {
		t.Fatal(err)
	}
	const authority = "https://login.example"
	tokenFile := testProxyCredentialFile(t)
	for name, cfg := range map[string]proxyConfig{
		"upstream outside allowedEndpoints": {UpstreamBaseURL: "https://other.openai.azure.com/openai/v1", UpstreamEntra: upstreamEntraConfig{
			Identities: testEntraIdentities(t, authority, "https://res.openai.azure.com"), Identity: "models", TokenFile: tokenFile}},
		"plain http upstream": {UpstreamBaseURL: "http://res.openai.azure.com/openai/v1", UpstreamEntra: upstreamEntraConfig{
			Identities: testEntraIdentities(t, authority, "https://res.openai.azure.com"), Identity: "models", TokenFile: tokenFile}},
		"unknown identity": {UpstreamBaseURL: "https://res.openai.azure.com/openai/v1", UpstreamEntra: upstreamEntraConfig{
			Identities: testEntraIdentities(t, authority, "https://res.openai.azure.com"), Identity: "other", TokenFile: tokenFile}},
		"identity without identities": {UpstreamBaseURL: "https://res.openai.azure.com/openai/v1", UpstreamEntra: upstreamEntraConfig{
			Identity: "models", TokenFile: tokenFile}},
		"identities without identity": {UpstreamBaseURL: "https://res.openai.azure.com/openai/v1", UpstreamEntra: upstreamEntraConfig{
			Identities: testEntraIdentities(t, authority, "https://res.openai.azure.com"), TokenFile: tokenFile}},
	} {
		if _, err := newProviderAuthProxyWithTokenStore(cfg, tokens); err == nil {
			t.Errorf("%s: the proxy started", name)
		}
	}
}

// The token is attached only to the pinned upstream origin.
func TestUpstreamEntraTokenStaysOnTheUpstreamOrigin(t *testing.T) {
	fake := &fakeAzureResource{}
	server := httptest.NewTLSServer(fake.handler(t))
	t.Cleanup(server.Close)
	proxy := newEntraTestProxy(t, server)
	for _, target := range []string{"https://attacker.example/openai/v1/responses", "http" + strings.TrimPrefix(server.URL, "https") + "/openai/v1/responses"} {
		request, _ := http.NewRequest(http.MethodPost, target, nil)
		if err := proxy.upstreamEntra.authenticate(context.Background(), request); err == nil || request.Header.Get(authorizationHeader) != "" {
			t.Fatalf("authenticate(%s) = %v, Authorization %q; want a refusal", target, err, request.Header.Get(authorizationHeader))
		}
	}
	if forms, _ := fake.calls(); len(forms) != 0 {
		t.Fatalf("token requests = %d, want none for another origin", len(forms))
	}
}

func headerValues(header http.Header) []string {
	var values []string
	for _, v := range header {
		values = append(values, v...)
	}
	return values
}
