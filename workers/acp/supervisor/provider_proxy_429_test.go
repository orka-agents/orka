package supervisor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/orka-agents/orka/internal/providerproxy"
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
	time.Sleep(time.Until(now.Add(2250 * time.Millisecond)))
	if sends.Load() != 1 {
		t.Errorf("short lease: upstream sends=%d, want 1", sends.Load())
	}
}

type codexTimedReader struct {
	io.Reader
	context.Context
	readyAt     time.Time
	observedEOF *bool
}

func (r codexTimedReader) Read(buf []byte) (int, error) {
	select {
	case <-r.Done():
		return 0, r.Err()
	case <-time.After(time.Until(r.readyAt)):
		read, err := r.Reader.Read(buf)
		if err == io.EOF && r.observedEOF != nil {
			*r.observedEOF = true
		}
		return read, err
	}
}

func TestProviderProxy429RetryBudget(t *testing.T) {
	for _, tc := range []struct {
		name           string
		qualify        bool
		firstHeaders   time.Duration
		firstBodyAt    time.Duration
		firstDetail    string
		retryHeaders   time.Duration
		bodyAt         time.Duration
		retryRejects   bool
		closeAdmission string
		wantStatus     int
		wantRoundTrips int
		wantBody       string
	}{
		{name: "unqualified late 429 keeps error body", firstHeaders: 31 * time.Second, firstBodyAt: 32 * time.Second, wantStatus: 429, wantRoundTrips: 1, wantBody: "rate limited"},
		{name: "qualified late 429 truncates only body", qualify: true, firstHeaders: 31 * time.Second, firstBodyAt: 32 * time.Second, wantStatus: 429, wantRoundTrips: 1},
		{name: "accepted retry streams beyond budget", qualify: true, firstBodyAt: time.Second, retryHeaders: 29 * time.Second, bodyAt: 31 * time.Second, wantStatus: 200, wantRoundTrips: 2, wantBody: "accepted"},
		{name: "rejection larger than relay cap is drained before retry", qualify: true, firstDetail: strings.Repeat("x", 2048), wantStatus: 200, wantRoundTrips: 2, wantBody: "accepted"},
		{name: "confirmed retry 429 survives a body past the deadline", qualify: true, retryHeaders: 29 * time.Second, bodyAt: 31 * time.Second, retryRejects: true, wantStatus: 429, wantRoundTrips: 2, wantBody: "retry was rate limited"},
		{name: "late retry headers never relay partial success", qualify: true, retryHeaders: 31 * time.Second, bodyAt: 32 * time.Second, wantStatus: 502, wantRoundTrips: 2},
		{name: "close cancels retry awaiting headers", qualify: true, closeAdmission: "during retry", wantStatus: 429, wantRoundTrips: 2, wantBody: "rate limited"},
		{name: "accepted retry can drain", qualify: true, closeAdmission: "after headers", bodyAt: time.Second, wantStatus: 200, wantRoundTrips: 2, wantBody: "accepted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const routeID = "12345678901234567890123456789012"
				gate, cancel := context.WithCancel(t.Context())
				defer cancel()
				start := time.Now()
				session := &providerProxySession{
					activePromptID: "test-prompt", turnPromptID: "test-prompt", gateContext: gate,
					leaseExpiresAt: start.Add(time.Minute), admissionDone: make(chan struct{}),
				}
				retryEntered := make(chan struct{})
				finished := make(chan struct{})
				defer close(finished)
				if tc.closeAdmission == "during retry" {
					go func() {
						select {
						case <-retryEntered:
							session.closeAdmission("test-prompt")
						case <-finished:
						}
					}()
				}
				var roundTrips int
				var firstBodyEOF bool
				session.proxy = &providerProxy{providerKind: providerKindCodex, codex429RouteID: routeID, maxResponseBytes: 1024,
					client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
						roundTrips++
						status, wait, bodyAt, text := http.StatusTooManyRequests, tc.firstHeaders, tc.firstBodyAt, "rate limited"
						if roundTrips == 1 && tc.firstDetail != "" {
							text = tc.firstDetail
						}
						if roundTrips > 1 {
							if !firstBodyEOF {
								t.Error("retry started before EOF on the previous rejection body")
							}
							if roundTrips == 2 && tc.closeAdmission == "during retry" {
								close(retryEntered)
								select {
								case <-req.Context().Done():
									return nil, req.Context().Err()
								case <-time.After(time.Second):
									t.Error("closing admission did not cancel the active retry transport")
								}
							}
							status, wait, bodyAt, text = http.StatusOK, tc.retryHeaders, tc.bodyAt, "accepted"
							if tc.retryRejects {
								status = http.StatusTooManyRequests
							}
							if tc.closeAdmission == "after headers" {
								time.AfterFunc(500*time.Millisecond, func() { session.closeAdmission("test-prompt") })
							}
						}
						time.Sleep(time.Until(start.Add(wait)))
						header := make(http.Header)
						header.Set("Retry-After", "0")
						header.Set(providerproxy.Codex429RouteHeader, req.Header.Get(providerproxy.Codex429RouteHeader))
						timedBody := codexTimedReader{Reader: strings.NewReader(text), Context: req.Context(), readyAt: start.Add(bodyAt)}
						if roundTrips == 1 {
							timedBody.observedEOF = &firstBodyEOF
						}
						var body io.Reader = timedBody
						contentLength := int64(-1)
						if roundTrips == 1 && tc.firstDetail != "" {
							contentLength = int64(len(tc.firstDetail))
						}
						if roundTrips > 1 && tc.retryRejects {
							body = io.MultiReader(strings.NewReader("retry was rate limited"), body)
						}
						return &http.Response{StatusCode: status, Header: header, ContentLength: contentLength, Body: io.NopCloser(body)}, nil
					})},
				}
				req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://gateway.example/v1/responses", strings.NewReader("{}"))
				if err != nil {
					t.Fatal(err)
				}
				if tc.qualify {
					req.Header.Set(providerproxy.Codex429RouteHeader, providerproxy.NewCodex429Challenge(routeID))
				}
				out := httptest.NewRecorder()
				session.forwardProviderRequest(out, req, providerOpenAIResponsesV1Path, []byte("{}"), providerRequestInference,
					providerProxyAuthorization{promptID: "test-prompt", gateContext: gate, inferenceSeq: 1}, func(error) {})
				if out.Code != tc.wantStatus || roundTrips != tc.wantRoundTrips || tc.wantStatus != 502 && out.Body.String() != tc.wantBody {
					t.Errorf("HTTP %d, %d transport calls, body %q; want HTTP %d, %d transport calls, body %q", out.Code, roundTrips, out.Body.String(), tc.wantStatus, tc.wantRoundTrips, tc.wantBody)
				}
			})
		})
	}
}
