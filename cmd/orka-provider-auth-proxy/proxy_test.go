package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testSharedProviderToken = "0123456789abcdef0123456789abcdef"

func TestProviderAuthProxyRejectsMissingAndWrongBearerTokens(t *testing.T) {
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls++
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)
	proxy := newTestProxy(t, upstream.URL, testSharedProviderToken)

	for name, authorization := range map[string]string{
		"missing": "",
		"wrong":   "Bearer 0123456789abcdef0123456789abcdeg",
		"basic":   "Basic " + testSharedProviderToken,
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "http://proxy/v1/responses", strings.NewReader(`{"model":"test"}`))
			if authorization != "" {
				request.Header.Set(authorizationHeader, authorization)
			}
			response := httptest.NewRecorder()
			proxy.ServeHTTP(response, request)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
			}
		})
	}
	if upstreamCalls != 0 {
		t.Fatalf("upstream calls = %d, want 0", upstreamCalls)
	}
}

func TestProviderAuthProxyForwardsAuthorizedRequestWithoutSensitiveHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/base/v1/responses" || r.URL.RawQuery != "stream=true" {
			t.Fatalf("upstream URL = %s, want /base/v1/responses?stream=true", r.URL.String())
		}
		for _, name := range []string{authorizationHeader, "X-Api-Key", "Cookie", "Txn-Token", "X-Orka-Internal"} {
			if value := r.Header.Get(name); value != "" {
				t.Fatalf("upstream received sensitive header %s", name)
			}
		}
		if got := r.Header.Get("Accept-Encoding"); got != "identity" {
			t.Fatalf("Accept-Encoding = %q, want identity", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream request: %v", err)
		}
		if string(body) != `{"model":"test"}` {
			t.Fatalf("upstream body = %q", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "secret=value")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(upstream.Close)
	proxy := newTestProxy(t, upstream.URL+"/base", testSharedProviderToken)
	request := httptest.NewRequest(http.MethodPost, "http://proxy/v1/responses?stream=true", strings.NewReader(`{"model":"test"}`))
	request.Header.Set(authorizationHeader, "Bearer "+testSharedProviderToken)
	request.Header.Set("X-Api-Key", "child-key")
	request.Header.Set("Cookie", "child=cookie")
	request.Header.Set("Txn-Token", "transaction")
	request.Header.Set("X-Orka-Internal", "internal")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	proxy.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != `{"ok":true}` {
		t.Fatalf("response = %d %q", response.Code, response.Body.String())
	}
	if value := response.Header().Get("Set-Cookie"); value != "" {
		t.Fatalf("sensitive response header leaked: %q", value)
	}
}

func TestProviderAuthProxyHealthDoesNotRequireAuthentication(t *testing.T) {
	proxy := newTestProxy(t, "http://upstream.example", testSharedProviderToken)
	for _, path := range []string{healthPath, readinessPath} {
		request := httptest.NewRequest(http.MethodGet, "http://proxy"+path, nil)
		response := httptest.NewRecorder()
		proxy.ServeHTTP(response, request)
		if response.Code != http.StatusOK || response.Body.String() != "ok\n" {
			t.Fatalf("%s response = %d %q", path, response.Code, response.Body.String())
		}
	}
}

func TestProviderAuthProxyRejectsRedirectsAndCompressedResponses(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"redirect": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "http://elsewhere.example")
			w.WriteHeader(http.StatusTemporaryRedirect)
		},
		"compressed": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = io.WriteString(w, "compressed")
		},
	} {
		t.Run(name, func(t *testing.T) {
			upstream := httptest.NewServer(handler)
			t.Cleanup(upstream.Close)
			proxy := newTestProxy(t, upstream.URL, testSharedProviderToken)
			request := httptest.NewRequest(http.MethodGet, "http://proxy/v1/models", nil)
			request.Header.Set(authorizationHeader, "Bearer "+testSharedProviderToken)
			response := httptest.NewRecorder()
			proxy.ServeHTTP(response, request)
			if response.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusBadGateway)
			}
		})
	}
}

func TestProviderAuthProxyRejectsOversizeKnownRequest(t *testing.T) {
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls++
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)
	proxy, err := newProviderAuthProxy(proxyConfig{UpstreamBaseURL: upstream.URL, MaxRequestBytes: 4}, []byte(testSharedProviderToken))
	if err != nil {
		t.Fatalf("new proxy: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "http://proxy/v1/responses", strings.NewReader("12345"))
	request.Header.Set(authorizationHeader, "Bearer "+testSharedProviderToken)
	response := httptest.NewRecorder()
	proxy.ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusRequestEntityTooLarge)
	}
	if upstreamCalls != 0 {
		t.Fatalf("upstream calls = %d, want 0", upstreamCalls)
	}
}

//nolint:unparam // The stable parameter keeps call sites explicit across related test cases.
func newTestProxy(t *testing.T, upstreamURL, token string) *providerAuthProxy {
	t.Helper()
	proxy, err := newProviderAuthProxy(proxyConfig{UpstreamBaseURL: upstreamURL}, []byte(token))
	if err != nil {
		t.Fatalf("new provider auth proxy: %v", err)
	}
	return proxy
}
