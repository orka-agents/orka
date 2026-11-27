package supervisor

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/orka-agents/orka/internal/providerproxy/testdata/codexroutes"
)

const (
	qualifiedRouteID = "04a11d52ead447b3e59aad569e2ad08e"
	qualifiedFront   = "http://codex-429-" + qualifiedRouteID + ".orka-system.svc:8080/v1"
	qualifiedModel   = "test-model"
	testRouteHeader  = "X-Orka-Codex-429-Route"
	testReplayHeader = "X-Orka-Codex-429-Retry"
	clientClaim      = qualifiedRouteID + ".aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

func dialTestUpstream(t *testing.T, proxy *providerProxy, front, target string) {
	t.Helper()
	from, err := url.Parse(front)
	if err != nil {
		t.Fatal(err)
	}
	to, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	host, _, err := net.SplitHostPort(to.Host)
	if err != nil || !net.ParseIP(host).IsLoopback() {
		t.Fatal("fixture destination must be loopback")
	}
	tr := proxy.client.Transport.(*http.Transport).Clone()
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr != from.Host {
			return nil, fmt.Errorf("unexpected fixture dial %q", addr)
		}
		return (&net.Dialer{}).DialContext(ctx, network, to.Host)
	}
	proxy.client.Transport = tr
	t.Cleanup(tr.CloseIdleConnections)
}

func qualifyScriptedProxy(c *ProviderProxyConfig) {
	id := sha256.Sum256([]byte("orka-codex-429-v1\nhttp://gateway-a1b2c3.test.svc:1337\n" + c.Model + "\n"))
	c.UpstreamBaseURL = fmt.Sprintf("http://codex-429-%x.orka-system.svc:8080/v1", id[:16])
	c.Codex429RetryBaseURL, c.Codex429RetryModel = c.UpstreamBaseURL, c.Model
}

func newQualifiedNativeServer(t *testing.T, cfg Config) (*Server, error) {
	t.Helper()
	local := cfg.ProviderProxy.UpstreamBaseURL
	qualifyScriptedProxy(&cfg.ProviderProxy)
	s, err := New(cfg)
	if err == nil {
		dialTestUpstream(t, s.providerProxy, cfg.ProviderProxy.UpstreamBaseURL, local)
	}
	return s, err
}

type scriptedQualificationWriter struct {
	http.ResponseWriter
	challenge string
}

func (w scriptedQualificationWriter) WriteHeader(status int) {
	if status == http.StatusTooManyRequests && w.challenge != "" {
		w.Header().Set(testRouteHeader, w.challenge)
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w scriptedQualificationWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w scriptedQualificationWriter) Flush()                      { w.ResponseWriter.(http.Flusher).Flush() }

func TestCodexUnqualified429IsNotRetried(t *testing.T) {
	testCodexRoutePost(t, ProviderProxyConfig{UpstreamBaseURL: "http://default-gateway.example.test:8080/v1", Model: qualifiedModel, ProviderKind: "codex", UpstreamBearerToken: testUpstreamToken}, "/responses", "foreign", 1)
}

func testCodexRoutePost(t *testing.T, cfg ProviderProxyConfig, routePath, proof string, wantPosts int) {
	t.Helper()
	body := `{"model":"` + cfg.Model + `","stream":true,"input":"one turn","metadata":{"retry_qualified":"true","task_retry":"3"},"tools":[{"type":"function","name":"read"}]}`
	testCodexWirePost(t, cfg, routePath, proof, wantPosts, body, nil)
}

func testCodexWirePost(t *testing.T, cfg ProviderProxyConfig, routePath, proof string, wantPosts int, bodyText string, headers http.Header) {
	t.Helper()
	const rejection = `{"error":{"code":"rate_limit_exceeded","message":"fixture quota"}}`
	var sends atomic.Int32
	var firstChallenge atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		n := sends.Add(1)
		challenge := r.Header.Get(testRouteHeader)
		if challenge == clientClaim {
			t.Error("forwarded child-supplied route challenge")
		}
		if n == 1 && r.Header.Get(testReplayHeader) != "" {
			t.Error("forwarded child-supplied replay precondition")
		}
		if n > 1 && r.Header.Get(testReplayHeader) != "1" {
			t.Error("replayed without the auth-proxy dispatch precondition")
		}
		scheme := "Bearer"
		if n > 1 {
			scheme = "Orka-Codex-429"
		}
		if r.Header.Get("Authorization") != scheme+" "+cfg.UpstreamBearerToken {
			t.Errorf("unexpected auth scheme on physical POST %d", n)
		}
		if n == 1 {
			firstChallenge.Store(challenge)
		}
		if n == 1 || (proof == "stale_on_second" && n == 2) {
			switch proof {
			case "echo":
				w.Header().Set(testRouteHeader, challenge)
			case "stale_on_second":
				w.Header().Set(testRouteHeader, firstChallenge.Load().(string))
			case "duplicate":
				w.Header().Add(testRouteHeader, challenge)
				w.Header().Add(testRouteHeader, challenge)
			case "foreign":
				w.Header().Set(testRouteHeader, clientClaim)
			}
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, rejection)
			return
		}
		_, _ = io.WriteString(w, `{"id":"unexpected-replay"}`)
	}))
	defer upstream.Close()
	proxy, session, binding := activeTestProviderProxySession(t, cfg)
	defer session.close()
	dialTestUpstream(t, proxy, cfg.UpstreamBaseURL, upstream.URL)
	if headers == nil {
		headers = make(http.Header)
	}
	headers.Set(testRouteHeader, clientClaim)
	headers.Set(testReplayHeader, "1")
	response := doProviderProxyRequest(t, http.MethodPost, binding.BaseURL+routePath, binding.Credential, []byte(bodyText), headers)
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	wantStatus, wantBody := http.StatusTooManyRequests, rejection
	if wantPosts == 2 && proof != "stale_on_second" {
		wantStatus, wantBody = http.StatusOK, `{"id":"unexpected-replay"}`
	}
	if wantPosts == 0 {
		if sends.Load() != 0 || response.StatusCode < 400 {
			t.Errorf("invalid input reached gateway or succeeded: POSTs=%d HTTP=%d", sends.Load(), response.StatusCode)
		}
		return
	}
	if int(sends.Load()) != wantPosts || response.StatusCode != wantStatus || err != nil || string(body) != wantBody {
		t.Fatalf("gateway: POSTs=%d HTTP=%d; want %d POSTs and HTTP %d", sends.Load(), response.StatusCode, wantPosts, wantStatus)
	}
	if response.Header.Get(testRouteHeader) != "" || response.Header.Get(testReplayHeader) != "" {
		t.Error("internal qualification metadata reached the child")
	}
}

func TestCodexQualificationDirectStreamShape(t *testing.T) {
	cfg := ProviderProxyConfig{ProviderKind: "codex", Model: qualifiedModel, UpstreamBearerToken: testUpstreamToken, ModelOutputLimit: 4096}
	qualifyScriptedProxy(&cfg)
	for _, tc := range codexroutes.Cases() {
		t.Run(tc.Name, func(t *testing.T) {
			want := 1
			if tc.Qualified {
				want = 2
			} else if tc.InvalidJSON {
				want = 0
			}
			h := make(http.Header)
			if tc.Header != "" {
				h.Set(tc.Header, "true")
				if tc.Header == "Upgrade" {
					h.Set("Upgrade", "websocket")
					h.Set("Connection", "Upgrade")
				}
			}
			testCodexWirePost(t, cfg, "/responses", "echo", want, tc.Body, h)
		})
	}
}

func TestCodexRetryOperatorRouteBinding(t *testing.T) {
	for _, tc := range []struct {
		name, actual, pinned, model, path, proof string
		want                                     int
	}{
		{"unqualified_versioned_name", qualifiedFront, "", "", "/responses", "echo", 1},
		{"changed_frontend_revision", strings.Replace(qualifiedFront, qualifiedRouteID, strings.Repeat("a", 32), 1), qualifiedFront, qualifiedModel, "/responses", "echo", 1},
		{"legacy_frontend", "http://orka-provider-auth-proxy.orka-system.svc:8080/v1", qualifiedFront, qualifiedModel, "/responses", "echo", 1},
		{"changed_path", qualifiedFront + "/moved", qualifiedFront, qualifiedModel, "/responses", "echo", 1},
		{"changed_port", strings.Replace(qualifiedFront, ":8080", ":8081", 1), qualifiedFront, qualifiedModel, "/responses", "echo", 1},
		{"other_model", qualifiedFront, qualifiedFront, "other-model", "/responses", "echo", 1},
		{"duplicate_version_in_path", qualifiedFront, qualifiedFront, qualifiedModel, "/v1/responses", "echo", 1},
		{"empty_query", qualifiedFront, qualifiedFront, qualifiedModel, "/responses?", "echo", 1},
		{"encoded_endpoint", qualifiedFront, qualifiedFront, qualifiedModel, "/%72esponses", "echo", 1},
		{"missing_auth_proxy_proof", qualifiedFront, qualifiedFront, qualifiedModel, "/responses", "absent", 1},
		{"foreign_auth_proxy_proof", qualifiedFront, qualifiedFront, qualifiedModel, "/responses", "foreign", 1},
		{"ambiguous_auth_proxy_proof", qualifiedFront, qualifiedFront, qualifiedModel, "/responses", "duplicate", 1},
		{"stale_second_proof", qualifiedFront, qualifiedFront, qualifiedModel, "/responses", "stale_on_second", 2},
		{"qualified", qualifiedFront, qualifiedFront, qualifiedModel, "/responses", "echo", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setLoadConfigEnv(t)
			t.Setenv(EnvModel, qualifiedModel)
			t.Setenv(EnvProviderProxyBaseURL, tc.actual)
			t.Setenv("ORKA_ACP_CODEX_429_RETRY_BASE_URL", tc.pinned)
			t.Setenv("ORKA_ACP_CODEX_429_RETRY_MODEL", tc.model)
			cfg, err := LoadConfigFromEnv()
			if err != nil {
				t.Fatal(err)
			}
			testCodexRoutePost(t, cfg.ProviderProxy, tc.path, tc.proof, tc.want)
		})
	}
}

func TestCodexRetryRefusesInvalidOperatorBinding(t *testing.T) {
	for _, tc := range []struct{ name, base, model string }{
		{"missing_model", qualifiedFront, ""}, {"missing_url", "", qualifiedModel},
		{"unversioned_service", "http://orka-provider-auth-proxy.orka-system.svc:8080/v1", qualifiedModel},
		{"ambiguous_url", qualifiedFront + "?route=old", qualifiedModel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setLoadConfigEnv(t)
			t.Setenv("ORKA_ACP_CODEX_429_RETRY_BASE_URL", tc.base)
			t.Setenv("ORKA_ACP_CODEX_429_RETRY_MODEL", tc.model)
			if _, err := LoadConfigFromEnv(); err == nil {
				t.Fatal("invalid operator qualification accepted at startup")
			}
		})
	}
}
