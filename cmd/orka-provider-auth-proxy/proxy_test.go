package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func newStaticBearerTokenStore(token []byte) (*bearerTokenStore, error) {
	if err := validateBearerToken(token); err != nil {
		return nil, err
	}
	store := newBearerTokenStore(time.Now)
	store.activate(token, nil, time.Time{})
	return store, nil
}

func newProviderAuthProxy(cfg proxyConfig, bearerToken []byte) (*providerAuthProxy, error) {
	tokens, err := newStaticBearerTokenStore(bearerToken)
	if err != nil {
		return nil, err
	}
	return newProviderAuthProxyWithTokenStore(cfg, tokens)
}

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

func TestProviderAuthProxyFlushesStreamedResponseChunks(t *testing.T) {
	const keepalive = ": keepalive\n\n"
	releaseBody := make(chan struct{})
	upstreamHeadersFlushed := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseBody) }) }

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("upstream response writer does not support flushing")
			close(upstreamHeadersFlushed)
			return
		}
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		close(upstreamHeadersFlushed)
		<-releaseBody
		_, _ = io.WriteString(w, keepalive)
		flusher.Flush()
	}))
	t.Cleanup(upstream.Close)

	proxy := newTestProxy(t, upstream.URL, testSharedProviderToken)
	proxyServer := httptest.NewServer(proxy)
	t.Cleanup(proxyServer.Close)
	// Cleanups run in LIFO order. Release the upstream handler before either
	// server waits for active connections to close.
	t.Cleanup(release)
	request, err := http.NewRequest(http.MethodPost, proxyServer.URL+"/v1/responses", strings.NewReader(`{"model":"test"}`))
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	request.Header.Set(authorizationHeader, "Bearer "+testSharedProviderToken)

	type responseResult struct {
		response *http.Response
		err      error
	}
	responseCh := make(chan responseResult, 1)
	go func() {
		response, requestErr := proxyServer.Client().Do(request)
		responseCh <- responseResult{response: response, err: requestErr}
	}()

	select {
	case <-upstreamHeadersFlushed:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not flush its response headers")
	}

	var result responseResult
	select {
	case result = <-responseCh:
	case <-time.After(2 * time.Second):
		t.Fatal("proxy buffered the streamed response headers")
	}
	if result.err != nil {
		t.Fatalf("proxy request: %v", result.err)
	}
	defer result.response.Body.Close() //nolint:errcheck
	release()

	chunk := make([]byte, len(keepalive))
	readCh := make(chan error, 1)
	go func() {
		_, readErr := io.ReadFull(result.response.Body, chunk)
		readCh <- readErr
	}()
	select {
	case readErr := <-readCh:
		if readErr != nil {
			t.Fatalf("read streamed response chunk: %v", readErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("proxy buffered the streamed response body")
	}
	if string(chunk) != keepalive {
		t.Fatalf("streamed response chunk = %q, want %q", chunk, keepalive)
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
