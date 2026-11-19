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
		{"codex", "test-model", http.MethodGet, "/v1/models"},
		{"codex", "test-model", http.MethodPost, "/responses/compact"},
		{"codex", "test-model", http.MethodPost, "/v1/chat/completions"},
		{"codex", "test-model", http.MethodPost, "/v1/responses?variant=other"},
		{"copilot", "test-model", http.MethodPost, "/v1/responses"},
		{"claude", "test-model", http.MethodPost, "/v1/messages"},
		{"opencode", "openai/test-model", http.MethodPost, "/v1/chat/completions"},
	} {
		t.Run(tc.kind+tc.path, func(t *testing.T) {
			var sends atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if sends.Add(1) == 1 {
					http.Error(w, "scripted rejection", http.StatusTooManyRequests)
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer upstream.Close()
			_, session, binding := activeTestProviderProxySession(t, ProviderProxyConfig{
				UpstreamBaseURL: upstream.URL, UpstreamBearerToken: testUpstreamToken, ProviderKind: tc.kind, Model: tc.model, ModelOutputLimit: 4096,
			})
			defer session.close()
			var body []byte
			if tc.method == http.MethodPost {
				body = []byte(`{"model":"` + tc.model + `"}`)
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
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if sends.Add(1) == 1 {
			w.Header().Set("Retry-After", "2")
			http.Error(w, "scripted rejection", http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	proxy := newTestProviderProxy(t, ProviderProxyConfig{UpstreamBaseURL: upstream.URL, UpstreamBearerToken: testUpstreamToken})
	session, binding, err := proxy.newSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.close()
	now := time.Now()
	if err := session.activateWithMaxTurns("short-lease", 7, now.Add(500*time.Millisecond), now); err != nil {
		t.Fatal(err)
	}
	resp := doProviderProxyRequest(t, http.MethodPost, binding.BaseURL+"/v1/responses", binding.Credential, []byte(`{"model":"test-model"}`), nil)
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
