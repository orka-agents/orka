/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/providerproxy"
)

const (
	healthPath                   = "/healthz"
	readinessPath                = "/readyz"
	defaultMaxRequestBytes       = 32 << 20
	defaultMaxResponseBytes      = 64 << 20
	defaultResponseHeaderTimeout = 2 * time.Minute
	defaultReadHeaderTimeout     = 5 * time.Second
	defaultIdleTimeout           = 30 * time.Second
	defaultMaxConcurrentRequests = 32
	authorizationHeader          = "Authorization"
)

var errRequestBodyTooLarge = errors.New("provider request body exceeds limit")

type proxyConfig struct {
	UpstreamBaseURL       string
	MaxRequestBytes       int64
	MaxResponseBytes      int64
	ResponseHeaderTimeout time.Duration
	MaxConcurrentRequests int
}

type providerAuthProxy struct {
	upstreamBase     *url.URL
	tokens           *bearerTokenStore
	maxRequestBytes  int64
	maxResponseBytes int64
	client           *http.Client
	requestSlots     chan struct{}
}

func newProviderAuthProxyWithTokenStore(cfg proxyConfig, tokens *bearerTokenStore) (*providerAuthProxy, error) {
	normalized, upstream, err := normalizeProxyConfig(cfg)
	if err != nil {
		return nil, err
	}
	if tokens == nil {
		return nil, fmt.Errorf("provider auth token store is required")
	}
	transport := &http.Transport{
		Proxy:                  nil,
		DialContext:            (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:      false,
		DisableKeepAlives:      false,
		MaxIdleConns:           normalized.MaxConcurrentRequests,
		MaxIdleConnsPerHost:    normalized.MaxConcurrentRequests,
		IdleConnTimeout:        90 * time.Second,
		TLSHandshakeTimeout:    10 * time.Second,
		ResponseHeaderTimeout:  normalized.ResponseHeaderTimeout,
		ExpectContinueTimeout:  time.Second,
		MaxResponseHeaderBytes: 64 << 10,
		DisableCompression:     true,
	}
	return &providerAuthProxy{
		upstreamBase:     upstream,
		tokens:           tokens,
		maxRequestBytes:  normalized.MaxRequestBytes,
		maxResponseBytes: normalized.MaxResponseBytes,
		client: &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		requestSlots: make(chan struct{}, normalized.MaxConcurrentRequests),
	}, nil
}

func normalizeProxyConfig(cfg proxyConfig) (proxyConfig, *url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(cfg.UpstreamBaseURL))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return proxyConfig{}, nil, fmt.Errorf("provider upstream base URL is invalid")
	}
	if parsed.Path == "" {
		parsed.Path = "/"
	}
	if providerproxy.HasUnsafePathSegment(parsed.Path) {
		return proxyConfig{}, nil, fmt.Errorf("provider upstream base URL is invalid")
	}
	if cfg.MaxRequestBytes <= 0 {
		cfg.MaxRequestBytes = defaultMaxRequestBytes
	}
	if cfg.MaxResponseBytes <= 0 {
		cfg.MaxResponseBytes = defaultMaxResponseBytes
	}
	if cfg.ResponseHeaderTimeout <= 0 {
		cfg.ResponseHeaderTimeout = defaultResponseHeaderTimeout
	}
	if cfg.MaxConcurrentRequests <= 0 {
		cfg.MaxConcurrentRequests = defaultMaxConcurrentRequests
	}
	cfg.UpstreamBaseURL = parsed.String()
	return cfg, parsed, nil
}

func validateBearerToken(token []byte) error {
	if len(token) < 32 || len(token) > maxBearerTokenBytes {
		return fmt.Errorf("provider auth bearer token is invalid")
	}
	for _, value := range token {
		if value <= ' ' || value == 0x7f {
			return fmt.Errorf("provider auth bearer token is invalid")
		}
	}
	return nil
}

func (p *providerAuthProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == healthPath {
		serveHealth(w, r)
		return
	}
	if r.URL.Path == readinessPath {
		if !p.tokens.isReady() {
			providerproxy.WriteError(w, http.StatusServiceUnavailable, "provider proxy authentication is unavailable")
			return
		}
		serveHealth(w, r)
		return
	}
	if !p.authorized(r.Header.Values(authorizationHeader)) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="orka-provider-auth-proxy"`)
		providerproxy.WriteError(w, http.StatusUnauthorized, "provider proxy authentication required")
		return
	}
	if !providerproxy.TryAcquireSlot(p.requestSlots) {
		providerproxy.WriteError(w, http.StatusTooManyRequests, "provider proxy request capacity is exhausted")
		return
	}
	defer providerproxy.ReleaseSlot(p.requestSlots)
	if r.Method == http.MethodConnect || r.Method == http.MethodTrace {
		providerproxy.WriteError(w, http.StatusMethodNotAllowed, "provider request method is not allowed")
		return
	}
	if providerproxy.HasUnsafePathSegment(r.URL.Path) {
		providerproxy.WriteError(w, http.StatusBadRequest, "provider request path is invalid")
		return
	}
	if providerproxy.HasDisallowedContentEncoding(r.Header) {
		providerproxy.WriteError(w, http.StatusUnsupportedMediaType, "compressed provider requests are forbidden")
		return
	}
	if r.ContentLength > p.maxRequestBytes {
		providerproxy.WriteError(w, http.StatusRequestEntityTooLarge, "provider request body exceeds limit")
		return
	}

	target := providerproxy.Target(p.upstreamBase, r.URL.Path, r.URL.RawQuery)
	body := &boundedReadCloser{ReadCloser: r.Body, remaining: p.maxRequestBytes}
	upstreamRequest, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), body)
	if err != nil {
		providerproxy.WriteError(w, http.StatusBadGateway, "provider request could not be prepared")
		return
	}
	upstreamRequest.ContentLength = r.ContentLength
	providerproxy.CopyRequestHeaders(upstreamRequest.Header, r.Header)
	upstreamRequest.Header.Set("Accept-Encoding", "identity")

	response, err := p.client.Do(upstreamRequest)
	if err != nil {
		if errors.Is(err, errRequestBodyTooLarge) {
			providerproxy.WriteError(w, http.StatusRequestEntityTooLarge, "provider request body exceeds limit")
			return
		}
		providerproxy.WriteError(w, http.StatusBadGateway, "provider upstream request failed")
		return
	}
	defer response.Body.Close() //nolint:errcheck
	if response.StatusCode >= http.StatusMultipleChoices && response.StatusCode < http.StatusBadRequest {
		providerproxy.WriteError(w, http.StatusBadGateway, "provider upstream redirects are forbidden")
		return
	}
	if providerproxy.HasDisallowedContentEncoding(response.Header) {
		providerproxy.WriteError(w, http.StatusBadGateway, "compressed provider responses are forbidden")
		return
	}
	if response.ContentLength > p.maxResponseBytes {
		providerproxy.WriteError(w, http.StatusBadGateway, "provider upstream response exceeds limit")
		return
	}
	providerproxy.CopyResponseHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	flusher, _ := w.(http.Flusher)
	// Flush headers before waiting for the first upstream body chunk, then flush
	// every chunk so streamed responses reach the ACP runtime promptly.
	if flusher != nil {
		flusher.Flush()
	}
	if err := providerproxy.StreamBoundedResponse(w, response.Body, p.maxResponseBytes, flusher); err != nil {
		panic(http.ErrAbortHandler)
	}
}

func (p *providerAuthProxy) authorized(values []string) bool {
	return p.tokens.authorized(values)
}

func serveHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		providerproxy.WriteError(w, http.StatusMethodNotAllowed, "health probe method is not allowed")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = io.WriteString(w, "ok\n")
	}
}

type boundedReadCloser struct {
	io.ReadCloser
	remaining int64
}

func (r *boundedReadCloser) Read(buffer []byte) (int, error) {
	if r.remaining < 0 {
		return 0, errRequestBodyTooLarge
	}
	maxRead := min(int64(len(buffer)), r.remaining+1)
	n, err := r.ReadCloser.Read(buffer[:maxRead])
	if int64(n) > r.remaining {
		allowed := int(r.remaining)
		r.remaining = -1
		return allowed, errRequestBodyTooLarge
	}
	r.remaining -= int64(n)
	return n, err
}

func newProxyHTTPServer(address string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: defaultReadHeaderTimeout,
		IdleTimeout:       defaultIdleTimeout,
		MaxHeaderBytes:    32 << 10,
		BaseContext: func(net.Listener) context.Context {
			return context.Background()
		},
	}
}
