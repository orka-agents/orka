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
)

const (
	healthPath                    = "/healthz"
	readinessPath                 = "/readyz"
	defaultMaxRequestBytes        = 32 << 20
	defaultMaxResponseBytes       = 64 << 20
	defaultResponseHeaderTimeout  = 30 * time.Second
	defaultReadHeaderTimeout      = 5 * time.Second
	defaultIdleTimeout            = 30 * time.Second
	defaultMaxConcurrentRequests  = 32
	authorizationHeader           = "Authorization"
	proxyAuthorizationHeader      = "Proxy-Authorization"
	providerAPIKeyHeader          = "X-Api-Key"
	providerLegacyAPIKeyHeader    = "Api-Key"
	providerContentEncodingHeader = "Content-Encoding"
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

func newProviderAuthProxy(cfg proxyConfig, bearerToken []byte) (*providerAuthProxy, error) {
	tokens, err := newStaticBearerTokenStore(bearerToken)
	if err != nil {
		return nil, err
	}
	return newProviderAuthProxyWithTokenStore(cfg, tokens)
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
	if hasUnsafePathSegment(parsed.Path) {
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
			writeProxyError(w, http.StatusServiceUnavailable, "provider proxy authentication is unavailable")
			return
		}
		serveHealth(w, r)
		return
	}
	if !p.authorized(r.Header.Values(authorizationHeader)) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="orka-provider-auth-proxy"`)
		writeProxyError(w, http.StatusUnauthorized, "provider proxy authentication required")
		return
	}
	if !tryAcquire(p.requestSlots) {
		writeProxyError(w, http.StatusTooManyRequests, "provider proxy request capacity is exhausted")
		return
	}
	defer release(p.requestSlots)
	if r.Method == http.MethodConnect || r.Method == http.MethodTrace {
		writeProxyError(w, http.StatusMethodNotAllowed, "provider request method is not allowed")
		return
	}
	if hasUnsafePathSegment(r.URL.Path) {
		writeProxyError(w, http.StatusBadRequest, "provider request path is invalid")
		return
	}
	if encoding := strings.TrimSpace(r.Header.Get(providerContentEncodingHeader)); encoding != "" && !strings.EqualFold(encoding, "identity") {
		writeProxyError(w, http.StatusUnsupportedMediaType, "compressed provider requests are forbidden")
		return
	}
	if r.ContentLength > p.maxRequestBytes {
		writeProxyError(w, http.StatusRequestEntityTooLarge, "provider request body exceeds limit")
		return
	}

	target := providerTarget(p.upstreamBase, r.URL.Path, r.URL.RawQuery)
	body := &boundedReadCloser{ReadCloser: r.Body, remaining: p.maxRequestBytes}
	upstreamRequest, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), body)
	if err != nil {
		writeProxyError(w, http.StatusBadGateway, "provider request could not be prepared")
		return
	}
	upstreamRequest.ContentLength = r.ContentLength
	copyProviderRequestHeaders(upstreamRequest.Header, r.Header)
	upstreamRequest.Header.Set("Accept-Encoding", "identity")

	response, err := p.client.Do(upstreamRequest)
	if err != nil {
		if errors.Is(err, errRequestBodyTooLarge) {
			writeProxyError(w, http.StatusRequestEntityTooLarge, "provider request body exceeds limit")
			return
		}
		writeProxyError(w, http.StatusBadGateway, "provider upstream request failed")
		return
	}
	defer response.Body.Close() //nolint:errcheck
	if response.StatusCode >= http.StatusMultipleChoices && response.StatusCode < http.StatusBadRequest {
		writeProxyError(w, http.StatusBadGateway, "provider upstream redirects are forbidden")
		return
	}
	if encoding := strings.TrimSpace(response.Header.Get(providerContentEncodingHeader)); encoding != "" && !strings.EqualFold(encoding, "identity") {
		writeProxyError(w, http.StatusBadGateway, "compressed provider responses are forbidden")
		return
	}
	if response.ContentLength > p.maxResponseBytes {
		writeProxyError(w, http.StatusBadGateway, "provider upstream response exceeds limit")
		return
	}
	copyProviderResponseHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	if err := streamBoundedResponse(w, response.Body, p.maxResponseBytes); err != nil {
		panic(http.ErrAbortHandler)
	}
}

func (p *providerAuthProxy) authorized(values []string) bool {
	return p.tokens.authorized(values)
}

func serveHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeProxyError(w, http.StatusMethodNotAllowed, "health probe method is not allowed")
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

func streamBoundedResponse(destination io.Writer, source io.Reader, limit int64) error {
	remaining := limit
	buffer := make([]byte, 32<<10)
	for {
		readSize := len(buffer)
		if int64(readSize) > remaining+1 {
			readSize = int(remaining + 1)
		}
		n, readErr := source.Read(buffer[:readSize])
		if int64(n) > remaining {
			if remaining > 0 {
				if _, writeErr := destination.Write(buffer[:remaining]); writeErr != nil {
					return writeErr
				}
			}
			return fmt.Errorf("provider upstream response exceeds limit")
		}
		if n > 0 {
			written, writeErr := destination.Write(buffer[:n])
			remaining -= int64(written)
			if writeErr != nil {
				return writeErr
			}
			if written != n {
				return io.ErrShortWrite
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
}

func providerTarget(base *url.URL, requestPath, rawQuery string) *url.URL {
	target := *base
	target.Path = strings.TrimSuffix(base.Path, "/") + "/" + strings.TrimPrefix(requestPath, "/")
	target.RawPath = ""
	target.RawQuery = rawQuery
	return &target
}

func hasUnsafePathSegment(path string) bool {
	decoded, err := url.PathUnescape(path)
	if err != nil {
		return true
	}
	for segment := range strings.SplitSeq(decoded, "/") {
		if segment == "." || segment == ".." {
			return true
		}
	}
	return false
}

func copyProviderRequestHeaders(destination, source http.Header) {
	blocked := blockedHeaders(source)
	for name, values := range source {
		canonical := http.CanonicalHeaderKey(name)
		if blocked[canonical] || isSensitiveRequestHeader(canonical) {
			continue
		}
		for _, value := range values {
			destination.Add(canonical, value)
		}
	}
}

func copyProviderResponseHeaders(destination, source http.Header) {
	blocked := blockedHeaders(source)
	for name, values := range source {
		canonical := http.CanonicalHeaderKey(name)
		if blocked[canonical] || isSensitiveResponseHeader(canonical) {
			continue
		}
		for _, value := range values {
			destination.Add(canonical, value)
		}
	}
}

func blockedHeaders(header http.Header) map[string]bool {
	blocked := map[string]bool{
		"Connection":             true,
		"Keep-Alive":             true,
		"Proxy-Authenticate":     true,
		proxyAuthorizationHeader: true,
		"Proxy-Connection":       true,
		"Te":                     true,
		"Trailer":                true,
		"Transfer-Encoding":      true,
		"Upgrade":                true,
	}
	for _, connection := range header.Values("Connection") {
		for name := range strings.SplitSeq(connection, ",") {
			name = http.CanonicalHeaderKey(strings.TrimSpace(name))
			if name != "" {
				blocked[name] = true
			}
		}
	}
	return blocked
}

func isSensitiveRequestHeader(name string) bool {
	switch name {
	case authorizationHeader, proxyAuthorizationHeader, providerAPIKeyHeader, providerLegacyAPIKeyHeader,
		"Cookie", "Set-Cookie", "Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto",
		"X-Real-Ip", "X-Forwarded-Prefix", "X-Original-Url", "X-Rewrite-Url", "X-Envoy-Original-Path",
		"X-Http-Method-Override", "Txn-Token", "Origin", "Referer", "Openai-Organization", "Openai-Project",
		"Anthropic-Organization-Id", "Traceparent", "Tracestate", "Baggage", providerContentEncodingHeader, "Expect":
		return true
	default:
		return strings.HasPrefix(name, "X-Orka-") || strings.HasPrefix(name, "X-Forwarded-") || strings.HasPrefix(name, "Sec-Fetch-")
	}
}

func isSensitiveResponseHeader(name string) bool {
	switch name {
	case authorizationHeader, proxyAuthorizationHeader, providerAPIKeyHeader, providerLegacyAPIKeyHeader,
		"Set-Cookie", "Set-Cookie2", "Location", "Server", "Alt-Svc", "Www-Authenticate", "Proxy-Authenticate",
		providerContentEncodingHeader:
		return true
	default:
		return false
	}
}

func tryAcquire(slots chan struct{}) bool {
	select {
	case slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func release(slots chan struct{}) {
	<-slots
}

func writeProxyError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, message+"\n")
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
