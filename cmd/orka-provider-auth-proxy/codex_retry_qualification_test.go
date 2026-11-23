package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/providerproxy/testdata/codexroutes"
)

const (
	fixtureEgress    = "http://gateway-a1b2c3.test.svc:1337"
	fixtureModel     = "test-model"
	fixtureRouteID   = "04a11d52ead447b3e59aad569e2ad08e" // fixtureEgress and fixtureModel
	fixtureChallenge = fixtureRouteID + ".aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	routeHeader      = "X-Orka-Codex-429-Route"
	replayHeader     = "X-Orka-Codex-429-Retry"
	fixturePost      = `{"model":"test-model","stream":true}`
)

func TestAuthProxyNeverTrustsGatewayQualification(t *testing.T) {
	runQualificationExchange(t, proxyConfig{UpstreamBaseURL: fixtureEgress}, "/v1/responses", fixturePost, fixtureChallenge, false, 429, 1, false)
}

func TestAuthProxyQualificationBoundToPhysicalEgress(t *testing.T) {
	for _, tc := range []struct {
		name, actual, model, body, path, challenge string
		replay                                     bool
		status, sends                              int
		proof                                      bool
	}{
		{"qualified_original", fixtureEgress, fixtureModel, fixturePost, "/v1/responses", fixtureChallenge, false, 429, 1, true},
		{"qualified_replay", fixtureEgress, fixtureModel, fixturePost, "/v1/responses", fixtureChallenge, true, 429, 1, true},
		{"missing_challenge", fixtureEgress, fixtureModel, fixturePost, "/v1/responses", "", false, 429, 1, false},
		{"wrong_route_id", fixtureEgress, fixtureModel, fixturePost, "/v1/responses", strings.Repeat("b", 32) + fixtureChallenge[32:], false, 429, 1, false},
		{"repointed_original", "http://gateway-d4e5f6.test.svc:1337", fixtureModel, fixturePost, "/v1/responses", fixtureChallenge, false, 429, 1, false},
		{"repointed_replay", "http://gateway-d4e5f6.test.svc:1337", fixtureModel, fixturePost, "/v1/responses", fixtureChallenge, true, 429, 0, false},
		{"changed_operator_model", fixtureEgress, "other-model", fixturePost, "/v1/responses", fixtureChallenge, false, 429, 1, false},
		{"wrong_body_replay", fixtureEgress, fixtureModel, `{"model":"other-model","stream":true}`, "/v1/responses", fixtureChallenge, true, 429, 0, false},
		{"body_other_model", fixtureEgress, fixtureModel, `{"model":"other-model","stream":true}`, "/v1/responses", fixtureChallenge, false, 429, 1, false},
		{"body_duplicate_model", fixtureEgress, fixtureModel, `{"model":"other-model","model":"test-model","stream":true}`, "/v1/responses", fixtureChallenge, false, 429, 1, false},
		{"other_endpoint", fixtureEgress, fixtureModel, fixturePost, "/v1/responses/compact", fixtureChallenge, false, 429, 1, false},
		{"query", fixtureEgress, fixtureModel, fixturePost, "/v1/responses?variant=other", fixtureChallenge, false, 429, 1, false},
		{"empty_query", fixtureEgress, fixtureModel, fixturePost, "/v1/responses?", fixtureChallenge, false, 429, 1, false},
		{"encoded_endpoint", fixtureEgress, fixtureModel, fixturePost, "/v1/%72esponses", fixtureChallenge, false, 429, 1, false},
		{"non_429", fixtureEgress, fixtureModel, fixturePost, "/v1/responses", fixtureChallenge, false, 503, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := proxyConfig{UpstreamBaseURL: tc.actual, Codex429RetryBaseURL: fixtureEgress, Codex429RetryModel: tc.model}
			runQualificationExchange(t, cfg, tc.path, tc.body, tc.challenge, tc.replay, tc.status, tc.sends, tc.proof)
		})
	}
}

// Keep the actual configured origin/Host; map only its socket to loopback.
// The backend deliberately supplies the exact proof it should never own.
func runQualificationExchange(t *testing.T, cfg proxyConfig, path, body, challenge string, replay bool, backendStatus, wantSends int, wantProof bool) {
	t.Helper()
	runQualificationWireExchange(t, cfg, path, body, challenge, replay, backendStatus, wantSends, wantProof, nil)
}

func TestAuthProxyQualificationDirectStreamShape(t *testing.T) {
	cfg := proxyConfig{UpstreamBaseURL: fixtureEgress, Codex429RetryBaseURL: fixtureEgress, Codex429RetryModel: fixtureModel}
	for _, tc := range codexroutes.Cases() {
		t.Run(tc.Name, func(t *testing.T) {
			h := http.Header{}
			if tc.Header != "" {
				h.Set(tc.Header, "true")
			}
			if tc.Header == "Upgrade" {
				h.Set("Upgrade", "websocket")
				h.Set("Connection", "Upgrade")
			}
			runQualificationWireExchange(t, cfg, "/v1/responses", tc.Body, fixtureChallenge, false, 429, 1, tc.Qualified, h)
			wantReplay := 0
			if tc.Qualified {
				wantReplay = 1
			}
			runQualificationWireExchange(t, cfg, "/v1/responses", tc.Body, fixtureChallenge, true, 429, wantReplay, tc.Qualified, h)
		})
	}
}

func TestAuthProxyRejectsConflictingRetryAuthorization(t *testing.T) {
	cfg := proxyConfig{UpstreamBaseURL: fixtureEgress, Codex429RetryBaseURL: fixtureEgress, Codex429RetryModel: fixtureModel}
	t.Run("conditional_with_first_send_scheme", func(t *testing.T) {
		runQualificationWireExchange(t, cfg, "/v1/responses", fixturePost, fixtureChallenge, true, 429, 0, false, http.Header{authorizationHeader: {"Bearer " + testSharedProviderToken}})
	})
	t.Run("retry_scheme_without_retry_precondition", func(t *testing.T) {
		runQualificationWireExchange(t, cfg, "/v1/responses", fixturePost, fixtureChallenge, false, 429, 0, false, http.Header{authorizationHeader: {"Orka-Codex-429 " + testSharedProviderToken}})
	})
}

func TestAuthProxyConfirmed429ReleasesSlotBeforeRetry(t *testing.T) {
	const rejection = `{"error":{"message":"gateway rejected request"}}`
	var sends atomic.Int32
	firstClosed := make(chan struct{})
	firstHeaders := make(chan struct{})
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sends.Add(1) == 1 {
			defer close(firstClosed)
			w.Header().Set("Retry-After", "0")
			w.Header().Set("Content-Length", "128")
			w.WriteHeader(http.StatusTooManyRequests)
			w.(http.Flusher).Flush()
			select {
			case <-firstHeaders:
			case <-r.Context().Done():
				return
			}
			_, _ = io.WriteString(w, rejection)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		_, _ = io.WriteString(w, "accepted")
	}))
	defer gateway.Close()
	proxy := newQualificationProxy(t, proxyConfig{
		UpstreamBaseURL: fixtureEgress, Codex429RetryBaseURL: fixtureEgress, Codex429RetryModel: fixtureModel, MaxConcurrentRequests: 1,
	}, gateway.Listener.Addr().String())
	frontend := httptest.NewServer(proxy)
	defer frontend.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	send := func(retry bool) *http.Response {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, frontend.URL+"/v1/responses", strings.NewReader(fixturePost))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(authorizationHeader, "Bearer "+testSharedProviderToken)
		req.Header.Set(routeHeader, fixtureChallenge)
		if retry {
			req.Header.Set(authorizationHeader, "Orka-Codex-429 "+testSharedProviderToken)
			req.Header.Set(routeHeader, fixtureRouteID+".bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
			req.Header.Set(replayHeader, "1")
		}
		resp, err := frontend.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	first := send(false)
	defer func() { _ = first.Body.Close() }()
	if first.StatusCode != http.StatusTooManyRequests || first.Header.Get(routeHeader) != fixtureChallenge || first.Header.Get("Retry-After") != "0" {
		t.Fatalf("first response HTTP=%d, want a confirmed 429 with no backoff", first.StatusCode)
	}
	close(firstHeaders)
	firstBody, err := io.ReadAll(first.Body)
	if string(firstBody) != rejection || err != nil {
		t.Fatalf("original rejection body=%q, err=%v; want %q", firstBody, err, rejection)
	}
	retry := send(true)
	defer func() { _ = retry.Body.Close() }()
	retryBody, err := io.ReadAll(retry.Body)
	if retry.StatusCode != http.StatusOK || string(retryBody) != "accepted" || err != nil || sends.Load() != 2 {
		t.Fatalf("retry HTTP=%d, gateway sends=%d, body=%q, err=%v; want accepted after two sends", retry.StatusCode, sends.Load(), retryBody, err)
	}
	select {
	case <-firstClosed:
	case <-ctx.Done():
		t.Fatal("gateway rejection stream was not cancelled")
	}
}

type rejectionDeadlineRecorder struct {
	*httptest.ResponseRecorder
	writeDeadline time.Time
}

func (r *rejectionDeadlineRecorder) SetWriteDeadline(deadline time.Time) error {
	r.writeDeadline = deadline
	return http.NewResponseController(r.ResponseRecorder).SetWriteDeadline(deadline)
}

func runQualificationWireExchange(t *testing.T, cfg proxyConfig, path, body, challenge string, replay bool, backendStatus, wantSends int, wantProof bool, extra http.Header) {
	t.Helper()
	var sends atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sends.Add(1)
		if r.Header.Get(routeHeader) != "" || r.Header.Get(replayHeader) != "" {
			t.Error("gateway received reserved Orka metadata")
		}
		if r.Header.Get(authorizationHeader) != "" {
			t.Error("gateway received auth-proxy authorization")
		}
		got, err := io.ReadAll(r.Body)
		if err != nil || string(got) != body {
			t.Error("auth proxy changed provider payload")
		}
		if !wantProof {
			w.Header().Set(routeHeader, fixtureChallenge)
		}
		w.Header().Set(replayHeader, "1")
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(backendStatus)
		_, _ = io.WriteString(w, `{"error":{"message":"fixture evidence"}}`)
	}))
	defer backend.Close()
	proxy := newQualificationProxy(t, cfg, backend.Listener.Addr().String())
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(authorizationHeader, "Bearer "+testSharedProviderToken)
	if challenge != "" {
		request.Header.Set(routeHeader, challenge)
	}
	if replay {
		request.Header.Set(authorizationHeader, "Orka-Codex-429 "+testSharedProviderToken)
		request.Header.Set(replayHeader, "1")
	}
	for k, vs := range extra {
		request.Header.Del(k)
		for _, v := range vs {
			request.Header.Add(k, v)
		}
	}
	resp := &rejectionDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	proxy.ServeHTTP(resp, request)
	if got := !resp.writeDeadline.IsZero(); got != wantProof {
		t.Errorf("rejection write deadline set=%t, want %t", got, wantProof)
	}
	if int(sends.Load()) != wantSends {
		t.Errorf("backend sends=%d, want %d", sends.Load(), wantSends)
	}
	if wantSends > 0 && (resp.Code != backendStatus || resp.Header().Get("Retry-After") != "1" || resp.Body.String() != `{"error":{"message":"fixture evidence"}}`) {
		t.Errorf("auth proxy lost original status, delay or evidence: HTTP=%d", resp.Code)
	}
	if wantSends == 0 && resp.Code < 400 {
		t.Errorf("repointed replay received successful HTTP %d", resp.Code)
	}
	proofs := resp.Header().Values(routeHeader)
	if (wantProof && (len(proofs) != 1 || proofs[0] != challenge)) || (!wantProof && len(proofs) != 0) || resp.Header().Get(replayHeader) != "" {
		t.Errorf("invalid returned route qualification: proof count=%d, want proof=%t", len(proofs), wantProof)
	}
}

func newQualificationProxy(t *testing.T, cfg proxyConfig, backendAddr string) *providerAuthProxy {
	t.Helper()
	proxy, err := newProviderAuthProxy(cfg, []byte(testSharedProviderToken))
	if err != nil {
		t.Fatal(err)
	}
	actual, err := url.Parse(cfg.UpstreamBaseURL)
	if err != nil {
		t.Fatal(err)
	}
	transport := proxy.client.Transport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr != actual.Host {
			t.Errorf("unexpected dial destination %q", addr)
		}
		return (&net.Dialer{}).DialContext(ctx, network, backendAddr)
	}
	proxy.client.Transport = transport
	t.Cleanup(transport.CloseIdleConnections)
	return proxy
}
