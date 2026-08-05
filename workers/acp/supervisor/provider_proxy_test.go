package supervisor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testUpstreamToken          = "upstream-token"
	testConnectionSecretHeader = "X-Connection-Secret"
)

func TestProviderProxyConfigValidationAndRedaction(t *testing.T) {
	const secret = "do-not-print-this-provider-token"
	cfg := ProviderProxyConfig{UpstreamBaseURL: "http://vekil.example/v1", UpstreamBearerToken: secret}
	if rendered := fmt.Sprintf("%#v", cfg); strings.Contains(rendered, secret) {
		t.Fatalf("provider proxy config formatting leaked its bearer token: %s", rendered)
	}
	binding := ProviderProxyBinding{BaseURL: "http://127.0.0.1/private", Credential: secret}
	if rendered := fmt.Sprintf("%#v", binding); strings.Contains(rendered, secret) {
		t.Fatalf("provider proxy binding formatting leaked its local credential: %s", rendered)
	}
	for _, invalid := range []ProviderProxyConfig{
		{UpstreamBaseURL: "http://vekil.example/v1?token=query", UpstreamBearerToken: secret},
		{UpstreamBaseURL: "http://vekil.example/../admin", UpstreamBearerToken: secret},
		{UpstreamBaseURL: "http://vekil.example/v1", UpstreamBearerToken: "bad\nheader"},
	} {
		if _, _, err := invalid.normalized(); err == nil {
			t.Fatalf("unsafe provider proxy config unexpectedly validated: %#v", invalid)
		}
	}
}

func TestProviderProxyGatesSessionsAndInjectsSupervisorBearer(t *testing.T) {
	const upstreamToken = "supervisor-upstream-token-canary"
	type observedRequest struct {
		method string
		path   string
		query  string
		header http.Header
		body   string
	}
	observed := make(chan observedRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		observed <- observedRequest{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, header: r.Header.Clone(), body: string(body)}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream-Safe", "yes")
		w.Header().Set("Set-Cookie", "upstream-secret=1")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	proxy := newTestProviderProxy(t, ProviderProxyConfig{
		UpstreamBaseURL: upstream.URL + "/v1", UpstreamBearerToken: upstreamToken,
		MaxRequestBytes: 1 << 20, MaxResponseBytes: 1 << 20,
	})
	session, binding, err := proxy.newSession()
	if err != nil {
		t.Fatal(err)
	}
	second, secondBinding, err := proxy.newSession()
	if err != nil {
		t.Fatal(err)
	}
	if binding.BaseURL == secondBinding.BaseURL || binding.Credential == secondBinding.Credential {
		t.Fatal("runtime sessions reused a provider proxy route or credential")
	}
	cleanupNow := time.Now().UTC()
	if err := second.activate("cleanup-prompt", cleanupNow.Add(time.Minute), cleanupNow); err != nil {
		t.Fatal(err)
	}
	second.close()
	assertProviderProxyStatus(t, secondBinding.BaseURL+"/responses", secondBinding.Credential, http.StatusNotFound)
	parsed, err := url.Parse(binding.BaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Scheme != providerProxyScheme || parsed.Hostname() != "127.0.0.1" || !strings.HasPrefix(parsed.Path, providerProxyPathPrefix) || len(binding.Credential) < 40 {
		t.Fatalf("provider proxy binding is not private and unguessable: %#v", binding)
	}

	response := doProviderProxyRequest(t, http.MethodPost, binding.BaseURL+"/responses?stream=true", binding.Credential, []byte(`{"model":"test-model","prompt":"idle"}`), nil)
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("idle request status = %d, want %d", response.StatusCode, http.StatusForbidden)
	}
	_ = response.Body.Close()
	select {
	case request := <-observed:
		t.Fatalf("idle request reached upstream: %#v", request)
	default:
	}

	now := time.Now().UTC()
	if err := session.activate(testPromptOneID, now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	response = doProviderProxyRequest(t, http.MethodPost, binding.BaseURL+"/responses", "wrong-local-credential", []byte(`{"model":"test-model"}`), nil)
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong credential status = %d, want %d", response.StatusCode, http.StatusForbidden)
	}
	_ = response.Body.Close()

	headers := http.Header{
		"Content-Type":                   []string{"application/json"},
		providerAPIKeyHeader:             []string{binding.Credential},
		providerCookieHeader:             []string{"local-secret=1"},
		providerProxyAuthorizationHeader: []string{"Bearer proxy-secret"},
		providerForwardedForHeader:       []string{"203.0.113.1"},
		"Connection":                     []string{testConnectionSecretHeader},
		testConnectionSecretHeader:       []string{"remove-me"},
		"X-Safe-Request":                 []string{"preserve-me"},
	}
	response = doProviderProxyRequest(t, http.MethodPost, binding.BaseURL+"/responses?stream=true", binding.Credential, []byte(`{"model":"test-model","prompt":"active"}`), headers)
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("active request status = %d body=%s", response.StatusCode, data)
	}
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"ok":true}` || response.Header.Get("X-Upstream-Safe") != "yes" || len(response.Cookies()) != 0 {
		t.Fatalf("unexpected proxied response: body=%q header=%v", data, response.Header)
	}
	request := <-observed
	if request.method != http.MethodPost || request.path != providerOpenAIResponsesV1Path || request.query != "stream=true" || request.body != `{"model":"test-model","prompt":"active"}` {
		t.Fatalf("unexpected upstream request: %#v", request)
	}
	if got := request.header.Get(providerAuthorizationHeader); got != "Bearer "+upstreamToken {
		t.Fatalf("upstream authorization = %q", got)
	}
	for _, name := range []string{
		providerAPIKeyHeader, providerLegacyAPIKeyHeader, providerCookieHeader,
		providerProxyAuthorizationHeader, providerForwardedForHeader, testConnectionSecretHeader,
	} {
		if value := request.header.Get(name); value != "" {
			t.Fatalf("sensitive inbound header %s reached upstream: %q", name, value)
		}
	}
	if request.header.Get("X-Safe-Request") != "preserve-me" {
		t.Fatalf("safe request header was not preserved: %v", request.header)
	}

	session.deactivate(testPromptOneID)
	assertProviderProxyStatus(t, binding.BaseURL+"/responses", binding.Credential, http.StatusForbidden)

	session.close()
	assertProviderProxyStatus(t, binding.BaseURL+"/responses", binding.Credential, http.StatusNotFound)
}

func TestProviderProxyAcceptsAnthropicAPIKeyCredential(t *testing.T) {
	observed := make(chan *http.Request, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed <- r.Clone(r.Context())
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()
	proxy, session, binding := activeTestProviderProxySession(t, ProviderProxyConfig{
		UpstreamBaseURL: upstream.URL + "/anthropic", UpstreamBearerToken: testUpstreamToken,
		ProviderKind: "claude", Model: "test-model",
	})
	_ = proxy
	defer session.close()
	request, err := http.NewRequest(http.MethodPost, binding.BaseURL+"/v1/messages", strings.NewReader(`{"model":"test-model"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(providerAPIKeyHeader, binding.Credential)
	request.Header.Set("Anthropic-Version", "2023-06-01")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("Anthropic proxy status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	upstreamRequest := <-observed
	if upstreamRequest.URL.Path != "/anthropic/v1/messages" ||
		upstreamRequest.Header.Get(providerAuthorizationHeader) != "Bearer "+testUpstreamToken ||
		upstreamRequest.Header.Get(providerAPIKeyHeader) != "" ||
		upstreamRequest.Header.Get("Anthropic-Version") != "2023-06-01" {
		t.Fatalf("unexpected Anthropic upstream request: path=%s headers=%v", upstreamRequest.URL.Path, upstreamRequest.Header)
	}
}

func TestProviderProxyAcceptsVersionedOpenAIPath(t *testing.T) {
	observed := make(chan *http.Request, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed <- r.Clone(r.Context())
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()
	_, session, binding := activeTestProviderProxySession(t, ProviderProxyConfig{
		UpstreamBaseURL: upstream.URL, UpstreamBearerToken: testUpstreamToken,
		ProviderKind: providerKindCodex, Model: "test-model",
	})
	defer session.close()
	response := doProviderProxyRequest(
		t, http.MethodPost, binding.BaseURL+providerOpenAIResponsesV1Path, binding.Credential,
		[]byte(`{"model":"test-model"}`), nil,
	)
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("versioned OpenAI request status = %d body=%s", response.StatusCode, body)
	}
	request := <-observed
	if request.URL.Path != providerOpenAIResponsesV1Path || request.Method != http.MethodPost {
		t.Fatalf("versioned OpenAI upstream request = %s %s", request.Method, request.URL.Path)
	}
}

func TestProviderProxyLeaseExpiryCancelsInflightAndDeniesLateRequests(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: started\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		close(started)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				close(cancelled)
				return
			case <-ticker.C:
				if _, err := io.WriteString(w, "data: waiting\n\n"); err != nil {
					close(cancelled)
					return
				}
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
			}
		}
	}))
	defer upstream.Close()
	proxy := newTestProviderProxy(t, ProviderProxyConfig{UpstreamBaseURL: upstream.URL, UpstreamBearerToken: testUpstreamToken})
	session, binding, err := proxy.newSession()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := session.activate("prompt-expiring", now.Add(150*time.Millisecond), now); err != nil {
		t.Fatal(err)
	}

	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		request, _ := http.NewRequest(http.MethodPost, binding.BaseURL+"/responses", bytes.NewReader([]byte(`{"model":"test-model"}`)))
		request.Header.Set(providerAuthorizationHeader, "Bearer "+binding.Credential)
		response, requestErr := http.DefaultClient.Do(request)
		if requestErr == nil {
			_, _ = io.ReadAll(response.Body)
			_ = response.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("provider request did not reach upstream while lease was active")
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("lease expiry did not cancel the in-flight upstream request")
	}
	select {
	case <-requestDone:
	case <-time.After(2 * time.Second):
		t.Fatal("local proxy request did not terminate after lease expiry")
	}
	response := doProviderProxyRequest(t, http.MethodPost, binding.BaseURL+"/responses", binding.Credential, []byte(`{"model":"test-model"}`), nil)
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("late request status = %d, want %d", response.StatusCode, http.StatusForbidden)
	}
	_ = response.Body.Close()
}

func TestProviderProxyLeaseRenewalExtendsAccess(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()
	proxy := newTestProviderProxy(t, ProviderProxyConfig{
		UpstreamBaseURL: upstream.URL, UpstreamBearerToken: testUpstreamToken,
	})
	session, binding, err := proxy.newSession()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := session.activate("prompt-renew", now.Add(75*time.Millisecond), now); err != nil {
		t.Fatal(err)
	}
	if err := session.renew("prompt-renew", now.Add(time.Second), now.Add(25*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(125 * time.Millisecond)
	response := doProviderProxyRequest(t, http.MethodPost, binding.BaseURL+"/responses", binding.Credential, []byte(`{"model":"test-model"}`), nil)
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("renewed provider request status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	session.deactivate("prompt-renew")
}

func TestProviderProxyMaxTurnsCountsOnlyInferenceRequests(t *testing.T) {
	t.Run("OpenAI-compatible", func(t *testing.T) {
		var reached atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			reached.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"ok":true}`)
		}))
		defer upstream.Close()

		proxy := newTestProviderProxy(t, ProviderProxyConfig{
			UpstreamBaseURL: upstream.URL, UpstreamBearerToken: "test-auth-token",
		})
		session, binding, err := proxy.newSession()
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		if err := session.activateWithMaxTurns(testPromptOneID, 3, now.Add(time.Minute), now); err != nil {
			t.Fatal(err)
		}
		defer session.close()

		for _, request := range []struct {
			method string
			path   string
			body   []byte
		}{
			{method: http.MethodGet, path: "/models"},
			{method: http.MethodPost, path: "/responses", body: []byte(`{"model":"test-model"}`)},
			{method: http.MethodPost, path: "/v1/chat/completions", body: []byte(`{"model":"test-model"}`)},
			{method: http.MethodPost, path: "/responses/compact", body: []byte(`{"model":"test-model"}`)},
			{method: http.MethodGet, path: providerModelsV1Path},
		} {
			response := doProviderProxyRequest(t, request.method, binding.BaseURL+request.path, binding.Credential, request.body, nil)
			if response.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(response.Body)
				_ = response.Body.Close()
				t.Fatalf("%s %s status = %d body=%s", request.method, request.path, response.StatusCode, body)
			}
			_ = response.Body.Close()
		}

		blocked := doProviderProxyRequest(
			t, http.MethodPost, binding.BaseURL+providerOpenAIResponsesV1Path, binding.Credential,
			[]byte(`{"model":"test-model"}`), nil,
		)
		defer func() { _ = blocked.Body.Close() }()
		if blocked.StatusCode != http.StatusBadRequest {
			body, _ := io.ReadAll(blocked.Body)
			t.Fatalf("N+1 OpenAI request status = %d body=%s", blocked.StatusCode, body)
		}
		body, _ := io.ReadAll(blocked.Body)
		if !strings.Contains(string(body), `"code":"max_turn_requests"`) {
			t.Fatalf("OpenAI turn-limit body = %s", body)
		}
		if got := reached.Load(); got != 5 {
			t.Fatalf("OpenAI upstream requests = %d, want 5", got)
		}
		if !session.maxTurnsExceeded(testPromptOneID) {
			t.Fatal("OpenAI prompt was not marked turn-limit exhausted")
		}
	})

	t.Run("Anthropic", func(t *testing.T) {
		var reached atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			reached.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"ok":true}`)
		}))
		defer upstream.Close()

		proxy := newTestProviderProxy(t, ProviderProxyConfig{
			UpstreamBaseURL: upstream.URL, UpstreamBearerToken: "test-auth-token", ProviderKind: providerKindClaude,
		})
		session, binding, err := proxy.newSession()
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		if err := session.activateWithMaxTurns(testPromptOneID, 1, now.Add(time.Minute), now); err != nil {
			t.Fatal(err)
		}
		defer session.close()

		for _, request := range []struct {
			method string
			path   string
			body   []byte
		}{
			{method: http.MethodPost, path: "/v1/messages/count_tokens", body: []byte(`{"model":"test-model"}`)},
			{method: http.MethodGet, path: providerModelsV1Path},
			{method: http.MethodPost, path: "/v1/messages", body: []byte(`{"model":"test-model"}`)},
		} {
			response := doProviderProxyRequest(t, request.method, binding.BaseURL+request.path, binding.Credential, request.body, nil)
			if response.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(response.Body)
				_ = response.Body.Close()
				t.Fatalf("%s %s status = %d body=%s", request.method, request.path, response.StatusCode, body)
			}
			_ = response.Body.Close()
		}

		blocked := doProviderProxyRequest(
			t, http.MethodPost, binding.BaseURL+"/v1/messages", binding.Credential,
			[]byte(`{"model":"test-model"}`), nil,
		)
		defer func() { _ = blocked.Body.Close() }()
		if blocked.StatusCode != http.StatusBadRequest {
			body, _ := io.ReadAll(blocked.Body)
			t.Fatalf("N+1 Anthropic request status = %d body=%s", blocked.StatusCode, body)
		}
		body, _ := io.ReadAll(blocked.Body)
		if !strings.Contains(string(body), `"type":"error"`) {
			t.Fatalf("Anthropic turn-limit body = %s", body)
		}
		if got := reached.Load(); got != 3 {
			t.Fatalf("Anthropic upstream requests = %d, want 3", got)
		}
		if !session.maxTurnsExceeded(testPromptOneID) {
			t.Fatal("Anthropic prompt was not marked turn-limit exhausted")
		}
	})
}

func TestProviderProxyMaxTurnsIsAtomicAcrossConcurrentRequests(t *testing.T) {
	var reached atomic.Int32
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseUpstream := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseUpstream()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Add(1)
		entered <- struct{}{}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	proxy := newTestProviderProxy(t, ProviderProxyConfig{
		UpstreamBaseURL: upstream.URL, UpstreamBearerToken: "test-auth-token",
	})
	session, binding, err := proxy.newSession()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := session.activateWithMaxTurns(testPromptOneID, 1, now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	defer session.close()

	type result struct {
		status int
		err    error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	request := func() {
		<-start
		httpRequest, requestErr := http.NewRequest(
			http.MethodPost, binding.BaseURL+"/responses",
			bytes.NewReader([]byte(`{"model":"test-model"}`)),
		)
		if requestErr != nil {
			results <- result{err: requestErr}
			return
		}
		httpRequest.Header.Set(providerAuthorizationHeader, "Bearer "+binding.Credential)
		response, requestErr := http.DefaultClient.Do(httpRequest)
		if requestErr != nil {
			results <- result{err: requestErr}
			return
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		results <- result{status: response.StatusCode}
	}
	go request()
	go request()
	close(start)

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("allowed inference request did not reach upstream")
	}
	select {
	case got := <-results:
		if got.err != nil || got.status != http.StatusBadRequest {
			t.Fatalf("concurrent N+1 result = %#v, want status %d", got, http.StatusBadRequest)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent N+1 request was not blocked")
	}
	if got := reached.Load(); got != 1 {
		t.Fatalf("concurrent upstream requests = %d, want 1", got)
	}
	releaseUpstream()
	select {
	case got := <-results:
		if got.err != nil || got.status != http.StatusOK {
			t.Fatalf("allowed concurrent request result = %#v, want status %d", got, http.StatusOK)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("allowed concurrent request did not complete")
	}
	if !session.maxTurnsExceeded(testPromptOneID) {
		t.Fatal("concurrent prompt was not marked turn-limit exhausted")
	}
}

func TestProviderProxyMaxTurnsResetsOnActivationButNotRenewal(t *testing.T) {
	var reached atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	proxy := newTestProviderProxy(t, ProviderProxyConfig{
		UpstreamBaseURL: upstream.URL, UpstreamBearerToken: "test-auth-token",
	})
	session, binding, err := proxy.newSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.close()
	now := time.Now().UTC()
	if err := session.activateWithMaxTurns("prompt-one", 1, now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}

	requestInference := func(want int) {
		t.Helper()
		response := doProviderProxyRequest(
			t, http.MethodPost, binding.BaseURL+"/responses", binding.Credential,
			[]byte(`{"model":"test-model"}`), nil,
		)
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode != want {
			body, _ := io.ReadAll(response.Body)
			t.Fatalf("provider response status = %d, want %d body=%s", response.StatusCode, want, body)
		}
	}

	requestInference(http.StatusOK)
	if err := session.renew("prompt-one", now.Add(2*time.Minute), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	requestInference(http.StatusBadRequest)
	if !session.maxTurnsExceeded("prompt-one") {
		t.Fatal("lease renewal reset the prompt turn-limit state")
	}

	session.deactivate("prompt-one")
	secondNow := time.Now().UTC()
	if err := session.activateWithMaxTurns("prompt-two", 1, secondNow.Add(time.Minute), secondNow); err != nil {
		t.Fatal(err)
	}
	if session.maxTurnsExceeded("prompt-one") || session.maxTurnsExceeded("prompt-two") {
		t.Fatal("new prompt activation did not reset turn-limit state")
	}
	requestInference(http.StatusOK)
	requestInference(http.StatusBadRequest)
	if !session.maxTurnsExceeded("prompt-two") {
		t.Fatal("second prompt was not marked turn-limit exhausted")
	}
	if got := reached.Load(); got != 2 {
		t.Fatalf("upstream requests across prompt resets = %d, want 2", got)
	}
}

func TestProviderProxySessionCleanupWaitsForInflightRequest(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		close(started)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				close(cancelled)
				return
			case <-ticker.C:
				if _, err := io.WriteString(w, "data: waiting\n\n"); err != nil {
					close(cancelled)
					return
				}
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
			}
		}
	}))
	defer upstream.Close()
	proxy, session, binding := activeTestProviderProxySession(t, ProviderProxyConfig{
		UpstreamBaseURL: upstream.URL, UpstreamBearerToken: testUpstreamToken,
	})
	_ = proxy
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		request, _ := http.NewRequest(http.MethodPost, binding.BaseURL+"/responses", bytes.NewReader([]byte(`{"model":"test-model"}`)))
		request.Header.Set(providerAuthorizationHeader, "Bearer "+binding.Credential)
		response, requestErr := http.DefaultClient.Do(request)
		if requestErr == nil {
			_, _ = io.ReadAll(response.Body)
			_ = response.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("provider request did not become in-flight")
	}
	session.close()
	waitCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := session.wait(waitCtx); err != nil {
		t.Fatalf("wait for provider request cleanup: %v", err)
	}
	select {
	case <-requestDone:
	case <-time.After(2 * time.Second):
		t.Fatal("local provider request remained active after session cleanup")
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream provider request remained active after session cleanup")
	}
}

func TestProviderProxyForbidsRedirectsAndBoundsBodies(t *testing.T) {
	t.Run("redirect", func(t *testing.T) {
		var redirected atomic.Bool
		target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Store(true) }))
		defer target.Close()
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Redirect(w, &http.Request{}, target.URL, http.StatusTemporaryRedirect)
		}))
		defer upstream.Close()
		proxy, session, binding := activeTestProviderProxySession(t, ProviderProxyConfig{UpstreamBaseURL: upstream.URL, UpstreamBearerToken: testUpstreamToken})
		_ = proxy
		defer session.close()
		response := doProviderProxyRequest(t, http.MethodPost, binding.BaseURL+"/responses", binding.Credential, []byte(`{"model":"test-model"}`), nil)
		if response.StatusCode != http.StatusBadGateway {
			t.Fatalf("redirect status = %d, want %d", response.StatusCode, http.StatusBadGateway)
		}
		_ = response.Body.Close()
		if redirected.Load() {
			t.Fatal("provider proxy followed an upstream redirect")
		}
	})

	t.Run("request body", func(t *testing.T) {
		var reached atomic.Bool
		upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Store(true) }))
		defer upstream.Close()
		_, session, binding := activeTestProviderProxySession(t, ProviderProxyConfig{
			UpstreamBaseURL: upstream.URL, UpstreamBearerToken: testUpstreamToken, MaxRequestBytes: 4,
		})
		defer session.close()
		response := doProviderProxyRequest(t, http.MethodPost, binding.BaseURL+"/responses", binding.Credential, []byte("12345"), nil)
		if response.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("oversized request status = %d, want %d", response.StatusCode, http.StatusRequestEntityTooLarge)
		}
		_ = response.Body.Close()
		if reached.Load() {
			t.Fatal("oversized request reached upstream")
		}
	})

	t.Run("response body", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "5")
			_, _ = io.WriteString(w, "12345")
		}))
		defer upstream.Close()
		_, session, binding := activeTestProviderProxySession(t, ProviderProxyConfig{
			UpstreamBaseURL: upstream.URL, UpstreamBearerToken: testUpstreamToken, MaxResponseBytes: 4,
		})
		defer session.close()
		response := doProviderProxyRequest(t, http.MethodPost, binding.BaseURL+"/responses", binding.Credential, []byte(`{"model":"test-model"}`), nil)
		if response.StatusCode != http.StatusBadGateway {
			t.Fatalf("oversized response status = %d, want %d", response.StatusCode, http.StatusBadGateway)
		}
		_ = response.Body.Close()
	})

	t.Run("compressed request", func(t *testing.T) {
		var reached atomic.Bool
		upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Store(true) }))
		defer upstream.Close()
		_, session, binding := activeTestProviderProxySession(t, ProviderProxyConfig{
			UpstreamBaseURL: upstream.URL, UpstreamBearerToken: testUpstreamToken,
		})
		defer session.close()
		response := doProviderProxyRequest(
			t, http.MethodPost, binding.BaseURL+"/responses", binding.Credential, []byte("compressed"),
			http.Header{providerContentEncodingHeader: []string{"gzip"}},
		)
		if response.StatusCode != http.StatusUnsupportedMediaType {
			t.Fatalf("compressed request status = %d, want %d", response.StatusCode, http.StatusUnsupportedMediaType)
		}
		_ = response.Body.Close()
		if reached.Load() {
			t.Fatal("compressed request reached upstream")
		}
	})

	t.Run("compressed response", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set(providerContentEncodingHeader, "gzip")
			_, _ = io.WriteString(w, "compressed")
		}))
		defer upstream.Close()
		_, session, binding := activeTestProviderProxySession(t, ProviderProxyConfig{
			UpstreamBaseURL: upstream.URL, UpstreamBearerToken: testUpstreamToken,
		})
		defer session.close()
		response := doProviderProxyRequest(t, http.MethodPost, binding.BaseURL+"/responses", binding.Credential, []byte(`{"model":"test-model"}`), nil)
		if response.StatusCode != http.StatusBadGateway {
			t.Fatalf("compressed response status = %d, want %d", response.StatusCode, http.StatusBadGateway)
		}
		_ = response.Body.Close()
	})
}

func newTestProviderProxy(t *testing.T, cfg ProviderProxyConfig) *providerProxy {
	t.Helper()
	if cfg.ProviderKind == "" {
		cfg.ProviderKind = providerKindCodex
	}
	if cfg.Model == "" {
		cfg.Model = "test-model"
	}
	proxy, err := newProviderProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := proxy.close(ctx); err != nil {
			t.Errorf("close provider proxy: %v", err)
		}
	})
	return proxy
}

//nolint:unparam // Stable test helper signatures keep related cases uniform.
func activeTestProviderProxySession(t *testing.T, cfg ProviderProxyConfig) (*providerProxy, *providerProxySession, ProviderProxyBinding) {
	t.Helper()
	proxy := newTestProviderProxy(t, cfg)
	session, binding, err := proxy.newSession()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := session.activate(testPromptOneID, now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	return proxy, session, binding
}

//nolint:unparam // Stable test helper signatures keep related cases uniform.
func doProviderProxyRequest(t *testing.T, method, endpoint, credential string, body []byte, headers http.Header) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for name, values := range headers {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	request.Header.Set(providerAuthorizationHeader, "Bearer "+credential)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func assertProviderProxyStatus(t *testing.T, endpoint, credential string, want int) {
	t.Helper()
	response := doProviderProxyRequest(t, http.MethodPost, endpoint, credential, []byte(`{"model":"test-model"}`), nil)
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != want {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
		t.Fatalf("provider proxy status = %d, want %d body=%s", response.StatusCode, want, body)
	}
}
