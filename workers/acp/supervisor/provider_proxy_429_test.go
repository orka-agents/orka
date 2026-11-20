package supervisor

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestProviderProxy429Scope(t *testing.T) {
	for _, tc := range []struct{ kind, model, method, path string }{
		{"codex", "test-model", http.MethodGet, "/models"},
		{"codex", "test-model", http.MethodPost, "/responses/compact"},
		{"codex", "test-model", http.MethodPost, "/chat/completions"},
		{"codex", "test-model", http.MethodPost, "/responses?variant=other"},
		{"copilot", "test-model", http.MethodPost, "/responses"},
		{"claude", "test-model", http.MethodPost, "/v1/messages"},
		{"opencode", "openai/test-model", http.MethodPost, "/v1/chat/completions"},
	} {
		t.Run(tc.kind+tc.path, func(t *testing.T) {
			var sends atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if sends.Add(1) == 1 {
					http.Error(scriptedQualificationWriter{w, r.Header.Get(testRouteHeader)}, "scripted rejection", http.StatusTooManyRequests)
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer upstream.Close()
			cfg := ProviderProxyConfig{
				UpstreamBaseURL: upstream.URL, UpstreamBearerToken: testUpstreamToken, ProviderKind: tc.kind, Model: tc.model, ModelOutputLimit: 4096,
			}
			qualifyScriptedProxy(&cfg)
			proxy, session, binding := activeTestProviderProxySession(t, cfg)
			dialTestUpstream(t, proxy, cfg.UpstreamBaseURL, upstream.URL)
			defer session.close()
			var body []byte
			if tc.method == http.MethodPost {
				body = []byte(`{"model":"` + tc.model + `","stream":true}`)
			}
			resp := doProviderProxyRequest(t, tc.method, binding.BaseURL+tc.path, binding.Credential, body, nil)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusTooManyRequests || sends.Load() != 1 {
				t.Fatalf("out-of-scope %s %s: HTTP=%d, upstream sends=%d; want 429 and 1", tc.method, tc.path, resp.StatusCode, sends.Load())
			}
		})
	}
}

func TestProviderProxy429CannotOutliveLease(t *testing.T) {
	var sends atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sends.Add(1) == 1 {
			if r.Header.Get(testRouteHeader) == "" {
				t.Error("short-lease fixture did not exercise a qualified send")
			}
			w.Header().Set("Retry-After", "2")
			http.Error(scriptedQualificationWriter{w, r.Header.Get(testRouteHeader)}, "scripted rejection", http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	cfg := ProviderProxyConfig{ProviderKind: "codex", Model: "test-model", UpstreamBearerToken: testUpstreamToken}
	qualifyScriptedProxy(&cfg)
	proxy := newTestProviderProxy(t, cfg)
	dialTestUpstream(t, proxy, cfg.UpstreamBaseURL, upstream.URL)
	session, binding, err := proxy.newSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.close()
	now := time.Now()
	if err := session.activateWithMaxTurns("short-lease", 7, now.Add(500*time.Millisecond), now); err != nil {
		t.Fatal(err)
	}
	resp := doProviderProxyRequest(t, http.MethodPost, binding.BaseURL+"/responses", binding.Credential, []byte(`{"model":"test-model","stream":true}`), nil)
	detail, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if elapsed := time.Since(now); elapsed >= 2*time.Second || resp.StatusCode != http.StatusTooManyRequests || readErr != nil || string(detail) != "scripted rejection\n" {
		t.Errorf("lease shorter than Retry-After: want original 429 and detail, got HTTP=%d after %s (read error=%v)", resp.StatusCode, elapsed, readErr)
	}
	// Leave the upstream open until a detached retry would have become eligible.
	time.Sleep(time.Until(now.Add(2250 * time.Millisecond)))
	if sends.Load() != 1 {
		t.Errorf("short lease: upstream sends=%d, want 1", sends.Load())
	}
}
