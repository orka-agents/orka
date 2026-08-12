package supervisor

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/orka-agents/orka/internal/providerproxy"
)

const (
	providerProxyPathPrefix               = "/_orka/provider/"
	providerProxyScheme                   = "http"
	providerProxyTLSScheme                = "https"
	providerAuthorizationHeader           = "Authorization"
	providerAPIKeyHeader                  = "X-Api-Key"
	providerLegacyAPIKeyHeader            = "Api-Key"
	providerProxyAuthorizationHeader      = "Proxy-Authorization"
	providerCookieHeader                  = "Cookie"
	providerForwardedForHeader            = "X-Forwarded-For"
	providerContentEncodingHeader         = "Content-Encoding"
	providerOpenAIResponsesV1Path         = "/v1/responses"
	providerOpenAIChatCompletionsPath     = "/chat/completions"
	providerOpenAIChatCompletionsV1Path   = "/v1/chat/completions"
	providerModelsV1Path                  = "/v1/models"
	providerMaxTokensField                = "max_tokens"
	providerMaxCompletionTokensField      = "max_completion_tokens"
	providerReasoningEffortField          = "reasoning_effort"
	providerToolsField                    = "tools"
	providerVerbosityField                = "verbosity"
	defaultProviderProxyMaxRequestBytes   = 32 << 20
	defaultProviderProxyMaxResponseBytes  = 64 << 20
	defaultProviderProxyHeaderTimeout     = 30 * time.Second
	defaultProviderProxyReadHeaderTimeout = 5 * time.Second
	defaultProviderProxyReadTimeout       = 30 * time.Second
	defaultProviderProxySessionRequests   = 2
	defaultProviderProxyGlobalRequests    = 8
)

type ProviderProxyConfig struct {
	UpstreamBaseURL       string
	UpstreamBearerToken   string
	ProviderKind          string
	Model                 string
	MaxRequestBytes       int64
	MaxResponseBytes      int64
	ResponseHeaderTimeout time.Duration
	ModelOutputLimit      int64
}

type ProviderProxyBinding struct {
	BaseURL    string
	Credential string
}

func (b ProviderProxyBinding) String() string {
	return fmt.Sprintf("{BaseURL:%q Credential:[redacted]}", b.BaseURL)
}

func (b ProviderProxyBinding) GoString() string { return b.String() }

func (c ProviderProxyConfig) String() string {
	return fmt.Sprintf("{UpstreamBaseURL:%q UpstreamBearerToken:[redacted] MaxRequestBytes:%d MaxResponseBytes:%d ResponseHeaderTimeout:%s}", c.UpstreamBaseURL, c.MaxRequestBytes, c.MaxResponseBytes, c.ResponseHeaderTimeout)
}

func (c ProviderProxyConfig) GoString() string { return c.String() }

type providerProxy struct {
	upstreamBase     *url.URL
	upstreamToken    []byte
	providerKind     string
	model            string
	maxRequestBytes  int64
	maxResponseBytes int64
	modelOutputLimit int64
	client           *http.Client
	listener         net.Listener
	server           *http.Server
	requestSlots     chan struct{}

	mu        sync.RWMutex
	sessions  map[string]*providerProxySession
	closed    bool
	closeDone chan struct{}
	closeErr  error
}

type providerProxySession struct {
	proxy      *providerProxy
	route      string
	credential []byte
	baseURL    string
	basePath   string

	mu                sync.Mutex
	activePromptID    string
	leaseExpiresAt    time.Time
	leaseVersion      uint64
	leaseTimer        *time.Timer
	gateContext       context.Context
	gateCancel        context.CancelFunc
	turnPromptID      string
	maxTurns          int32
	inferenceRequests int32
	turnLimitExceeded bool
	inflight          int
	drained           chan struct{}
	closed            bool
	requestSlots      chan struct{}
}

type providerProxyAuthorization struct {
	upstreamBase *url.URL
	gateContext  context.Context
	promptID     string
	release      func()
}

type providerRequestClass uint8

const (
	providerRequestMetadata providerRequestClass = iota
	providerRequestInference
)

var errProviderTurnLimitExceeded = errors.New("provider inference request limit exceeded")

func (c ProviderProxyConfig) normalized() (ProviderProxyConfig, *url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(c.UpstreamBaseURL))
	if err != nil ||
		(parsed.Scheme != providerProxyScheme && parsed.Scheme != providerProxyTLSScheme) ||
		parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return ProviderProxyConfig{}, nil, fmt.Errorf("provider proxy upstream URL is invalid")
	}
	if parsed.Path == "" {
		parsed.Path = "/"
	}
	if strings.TrimSpace(c.UpstreamBearerToken) == "" {
		return ProviderProxyConfig{}, nil, fmt.Errorf("provider proxy upstream bearer token is required")
	}
	c.ProviderKind = strings.TrimSpace(c.ProviderKind)
	c.Model = strings.TrimSpace(c.Model)
	if c.ProviderKind == "" || c.Model == "" {
		return ProviderProxyConfig{}, nil, fmt.Errorf("provider proxy kind and model are required")
	}
	c.UpstreamBaseURL = parsed.String()
	c.UpstreamBearerToken = normalizeBearerToken(c.UpstreamBearerToken)
	if c.UpstreamBearerToken == "" || strings.IndexFunc(c.UpstreamBearerToken, func(value rune) bool { return value <= ' ' || value == 0x7f }) >= 0 {
		return ProviderProxyConfig{}, nil, fmt.Errorf("provider proxy upstream bearer token is required")
	}
	if providerproxy.HasUnsafePathSegment(parsed.Path) {
		return ProviderProxyConfig{}, nil, fmt.Errorf("provider proxy upstream URL is invalid")
	}
	if c.MaxRequestBytes <= 0 {
		c.MaxRequestBytes = defaultProviderProxyMaxRequestBytes
	}
	if c.MaxResponseBytes <= 0 {
		c.MaxResponseBytes = defaultProviderProxyMaxResponseBytes
	}
	if c.ResponseHeaderTimeout <= 0 {
		c.ResponseHeaderTimeout = defaultProviderProxyHeaderTimeout
	}
	if c.ModelOutputLimit < 0 {
		return ProviderProxyConfig{}, nil, fmt.Errorf("provider proxy model output limit must be positive")
	}
	if c.ProviderKind == providerKindOpencode && c.ModelOutputLimit == 0 {
		return ProviderProxyConfig{}, nil, fmt.Errorf("OpenCode provider proxy model output limit is required")
	}
	return c, parsed, nil
}

func normalizeBearerToken(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= len("Bearer ") && strings.EqualFold(value[:len("Bearer ")], "Bearer ") {
		return strings.TrimSpace(value[len("Bearer "):])
	}
	return value
}

func newProviderProxy(cfg ProviderProxyConfig) (*providerProxy, error) {
	normalized, upstream, err := cfg.normalized()
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen on provider proxy loopback: %w", err)
	}
	transport := &http.Transport{
		Proxy:       nil,
		DialContext: (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		// A provider request owns its upstream connection so revoking one
		// session lease cannot interfere with another session sharing an HTTP/2
		// connection, and cancellation can close the exact request transport.
		ForceAttemptHTTP2:      false,
		DisableKeepAlives:      true,
		MaxIdleConns:           16,
		MaxIdleConnsPerHost:    8,
		IdleConnTimeout:        90 * time.Second,
		TLSHandshakeTimeout:    10 * time.Second,
		ResponseHeaderTimeout:  normalized.ResponseHeaderTimeout,
		ExpectContinueTimeout:  time.Second,
		MaxResponseHeaderBytes: 64 << 10,
		DisableCompression:     true,
	}
	proxy := &providerProxy{
		upstreamBase:     upstream,
		upstreamToken:    []byte(normalized.UpstreamBearerToken),
		providerKind:     normalized.ProviderKind,
		model:            normalized.Model,
		maxRequestBytes:  normalized.MaxRequestBytes,
		maxResponseBytes: normalized.MaxResponseBytes,
		modelOutputLimit: normalized.ModelOutputLimit,
		client: &http.Client{
			Transport:     transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		listener:     listener,
		sessions:     make(map[string]*providerProxySession),
		requestSlots: make(chan struct{}, defaultProviderProxyGlobalRequests),
	}
	proxy.server = &http.Server{
		Handler:           http.HandlerFunc(proxy.serveHTTP),
		ErrorLog:          log.New(io.Discard, "", 0),
		ReadHeaderTimeout: defaultProviderProxyReadHeaderTimeout,
		ReadTimeout:       defaultProviderProxyReadTimeout,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
	go func() {
		_ = proxy.server.Serve(listener)
	}()
	return proxy, nil
}

func (p *providerProxy) newSession() (*providerProxySession, ProviderProxyBinding, error) {
	if p == nil {
		return nil, ProviderProxyBinding{}, fmt.Errorf("provider proxy is required")
	}
	credential, err := randomProxySecret(32)
	if err != nil {
		return nil, ProviderProxyBinding{}, err
	}
	for range 8 {
		route, routeErr := randomProxySecret(24)
		if routeErr != nil {
			return nil, ProviderProxyBinding{}, routeErr
		}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, ProviderProxyBinding{}, fmt.Errorf("provider proxy is closed")
		}
		if _, exists := p.sessions[route]; exists {
			p.mu.Unlock()
			continue
		}
		basePath := providerProxyPathPrefix + route
		if upstreamPath := strings.TrimSuffix(p.upstreamBase.Path, "/"); upstreamPath != "" {
			basePath += upstreamPath
		}
		baseURL := providerProxyScheme + "://" + p.listener.Addr().String() + basePath
		drained := make(chan struct{})
		close(drained)
		session := &providerProxySession{
			proxy:        p,
			route:        route,
			credential:   []byte(credential),
			baseURL:      baseURL,
			basePath:     basePath,
			drained:      drained,
			requestSlots: make(chan struct{}, defaultProviderProxySessionRequests),
		}
		p.sessions[route] = session
		p.mu.Unlock()
		return session, ProviderProxyBinding{BaseURL: baseURL, Credential: credential}, nil
	}
	return nil, ProviderProxyBinding{}, fmt.Errorf("allocate unique provider proxy route")
}

func randomProxySecret(size int) (string, error) {
	if size < 16 {
		return "", fmt.Errorf("provider proxy secret size is too small")
	}
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		return "", fmt.Errorf("generate provider proxy secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func (s *providerProxySession) activate(promptID string, expiresAt, now time.Time) error {
	return s.activateWithMaxTurns(promptID, 50, expiresAt, now)
}

func (s *providerProxySession) activateWithMaxTurns(promptID string, maxTurns int32, expiresAt, now time.Time) error {
	promptID = strings.TrimSpace(promptID)
	if promptID == "" || maxTurns <= 0 || !expiresAt.After(now) {
		return fmt.Errorf("active prompt identity, positive max turns, and future lease expiry are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("provider proxy session is closed")
	}
	if s.activePromptID != "" {
		return fmt.Errorf("provider proxy session already has an active prompt")
	}
	s.activePromptID = promptID
	s.leaseExpiresAt = expiresAt
	s.turnPromptID = promptID
	s.maxTurns = maxTurns
	s.inferenceRequests = 0
	s.turnLimitExceeded = false
	s.leaseVersion++
	version := s.leaseVersion
	s.gateContext, s.gateCancel = context.WithCancel(context.Background())
	s.leaseTimer = time.AfterFunc(time.Until(expiresAt), func() {
		s.expire(promptID, version)
	})
	return nil
}

func (s *providerProxySession) renew(promptID string, expiresAt, now time.Time) error {
	if !expiresAt.After(now) {
		return fmt.Errorf("provider proxy lease expiry must be in the future")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.activePromptID != promptID || s.gateCancel == nil || !now.Before(s.leaseExpiresAt) {
		return fmt.Errorf("provider proxy prompt lease is no longer active")
	}
	s.leaseExpiresAt = expiresAt
	s.leaseVersion++
	version := s.leaseVersion
	if s.leaseTimer != nil {
		s.leaseTimer.Stop()
	}
	s.leaseTimer = time.AfterFunc(time.Until(expiresAt), func() {
		s.expire(promptID, version)
	})
	return nil
}

func (s *providerProxySession) deactivate(promptID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activePromptID != promptID {
		return
	}
	s.revokeLocked()
}

func (s *providerProxySession) revoke() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revokeLocked()
}

func (s *providerProxySession) revokeLocked() {
	if s.leaseTimer != nil {
		s.leaseTimer.Stop()
		s.leaseTimer = nil
	}
	if s.gateCancel != nil {
		s.gateCancel()
		s.gateCancel = nil
	}
	s.gateContext = nil
	s.activePromptID = ""
	s.leaseExpiresAt = time.Time{}
	s.leaseVersion++
}

func (s *providerProxySession) expire(promptID string, version uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.activePromptID != promptID || s.leaseVersion != version {
		return
	}
	if remaining := time.Until(s.leaseExpiresAt); remaining > 0 {
		s.leaseTimer = time.AfterFunc(remaining, func() { s.expire(promptID, version) })
		return
	}
	s.revokeLocked()
}

func (s *providerProxySession) close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.revokeLocked()
	for i := range s.credential {
		s.credential[i] = 0
	}
	s.mu.Unlock()
	if s.proxy != nil {
		s.proxy.mu.Lock()
		if s.proxy.sessions[s.route] == s {
			delete(s.proxy.sessions, s.route)
		}
		s.proxy.mu.Unlock()
	}
}

func (s *providerProxySession) wait(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	drained := s.drained
	s.mu.Unlock()
	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *providerProxySession) authorize(r *http.Request, now time.Time) (providerProxyAuthorization, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.activePromptID == "" || s.gateContext == nil || !now.Before(s.leaseExpiresAt) {
		if s.activePromptID != "" && !now.Before(s.leaseExpiresAt) {
			s.revokeLocked()
		}
		return providerProxyAuthorization{}, false
	}
	if !requestHasCredential(r, s.credential) {
		return providerProxyAuthorization{}, false
	}
	if s.inflight == 0 {
		s.drained = make(chan struct{})
	}
	s.inflight++
	var releaseOnce sync.Once
	target := *s.proxy.upstreamBase
	return providerProxyAuthorization{
		upstreamBase: &target,
		gateContext:  s.gateContext,
		promptID:     s.activePromptID,
		release: func() {
			releaseOnce.Do(s.releaseRequest)
		},
	}, true
}

func (s *providerProxySession) releaseRequest() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inflight <= 0 {
		return
	}
	s.inflight--
	if s.inflight == 0 {
		close(s.drained)
	}
}

func (s *providerProxySession) consumeInferenceRequest(promptID string, class providerRequestClass, now time.Time) error {
	if class != providerRequestInference {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.activePromptID != promptID || s.gateContext == nil || !now.Before(s.leaseExpiresAt) {
		return context.Canceled
	}
	if s.inferenceRequests >= s.maxTurns {
		s.turnLimitExceeded = true
		return errProviderTurnLimitExceeded
	}
	s.inferenceRequests++
	return nil
}

func (s *providerProxySession) maxTurnsExceeded(promptID string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turnPromptID == strings.TrimSpace(promptID) && s.turnLimitExceeded
}

func requestHasCredential(r *http.Request, expected []byte) bool {
	for _, value := range r.Header.Values(providerAuthorizationHeader) {
		value = strings.TrimSpace(value)
		if len(value) < len("Bearer ") || !strings.EqualFold(value[:len("Bearer ")], "Bearer ") {
			continue
		}
		if constantTimeStringMatch(strings.TrimSpace(value[len("Bearer "):]), expected) {
			return true
		}
	}
	for _, name := range []string{providerAPIKeyHeader, providerLegacyAPIKeyHeader} {
		for _, value := range r.Header.Values(name) {
			if constantTimeStringMatch(strings.TrimSpace(value), expected) {
				return true
			}
		}
	}
	return false
}

func constantTimeStringMatch(value string, expected []byte) bool {
	if len(value) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(value), expected) == 1
}

func (p *providerProxy) serveHTTP(w http.ResponseWriter, r *http.Request) {
	route, ok := splitProviderProxyRoute(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	p.mu.RLock()
	session := p.sessions[route]
	p.mu.RUnlock()
	if session == nil {
		http.NotFound(w, r)
		return
	}
	suffix, ok := session.requestSuffix(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	authorization, ok := session.authorize(r, time.Now().UTC())
	if !ok {
		providerproxy.WriteError(w, http.StatusForbidden, "provider access is not active")
		return
	}
	defer authorization.release()
	if !providerproxy.TryAcquireSlot(session.requestSlots) {
		providerproxy.WriteError(w, http.StatusTooManyRequests, "provider session request capacity is exhausted")
		return
	}
	defer providerproxy.ReleaseSlot(session.requestSlots)
	if !providerproxy.TryAcquireSlot(p.requestSlots) {
		providerproxy.WriteError(w, http.StatusTooManyRequests, "provider proxy request capacity is exhausted")
		return
	}
	defer providerproxy.ReleaseSlot(p.requestSlots)
	if r.Method == http.MethodConnect || r.Method == http.MethodTrace {
		providerproxy.WriteError(w, http.StatusMethodNotAllowed, "provider request method is not allowed")
		return
	}
	if providerproxy.HasUnsafePathSegment(suffix) {
		providerproxy.WriteError(w, http.StatusBadRequest, "provider request path is invalid")
		return
	}
	if providerproxy.HasDisallowedContentEncoding(r.Header) {
		providerproxy.WriteError(w, http.StatusUnsupportedMediaType, "compressed provider requests are forbidden")
		return
	}

	requestContext, cancel := context.WithCancel(r.Context())
	stopGate := context.AfterFunc(authorization.gateContext, cancel)
	stopBody := context.AfterFunc(authorization.gateContext, func() { _ = r.Body.Close() })
	var connectionMu sync.Mutex
	var upstreamConnection net.Conn
	connectionRevoked := false
	closeUpstreamConnection := func() {
		connectionMu.Lock()
		connectionRevoked = true
		if upstreamConnection != nil {
			_ = upstreamConnection.Close()
		}
		connectionMu.Unlock()
	}
	stopConnection := context.AfterFunc(authorization.gateContext, closeUpstreamConnection)
	defer func() {
		stopGate()
		stopBody()
		stopConnection()
		cancel()
	}()
	body, err := readBoundedProviderBody(requestContext, r.Body, p.maxRequestBytes)
	if err != nil {
		if errors.Is(err, errProviderBodyTooLarge) {
			providerproxy.WriteError(w, http.StatusRequestEntityTooLarge, "provider request body exceeds limit")
		} else {
			providerproxy.WriteError(w, http.StatusForbidden, "provider request is no longer active")
		}
		return
	}
	requestClass, err := validateProviderRequest(p.providerKind, p.model, suffix, r.Method, body)
	if err != nil {
		providerproxy.WriteError(w, http.StatusForbidden, "provider request is outside the immutable profile")
		return
	}
	body, err = normalizeProviderRequestBody(p.providerKind, p.model, suffix, p.modelOutputLimit, body)
	if err != nil {
		providerproxy.WriteError(w, http.StatusForbidden, "provider request is outside the immutable profile")
		return
	}
	select {
	case <-authorization.gateContext.Done():
		providerproxy.WriteError(w, http.StatusForbidden, "provider request is no longer active")
		return
	default:
	}
	if err := session.consumeInferenceRequest(authorization.promptID, requestClass, time.Now().UTC()); err != nil {
		if errors.Is(err, errProviderTurnLimitExceeded) {
			writeProviderTurnLimitError(w, p.providerKind)
		} else {
			providerproxy.WriteError(w, http.StatusForbidden, "provider request is no longer active")
		}
		return
	}

	requestContext = httptrace.WithClientTrace(requestContext, &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		connectionMu.Lock()
		upstreamConnection = info.Conn
		if connectionRevoked {
			_ = info.Conn.Close()
		}
		connectionMu.Unlock()
	}})
	target := providerproxy.Target(authorization.upstreamBase, suffix, r.URL.RawQuery)
	upstreamRequest, err := http.NewRequestWithContext(requestContext, r.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		providerproxy.WriteError(w, http.StatusBadGateway, "provider request could not be prepared")
		return
	}
	providerproxy.CopyRequestHeaders(upstreamRequest.Header, r.Header)
	upstreamRequest.Header.Set(providerAuthorizationHeader, "Bearer "+string(p.upstreamToken))
	upstreamRequest.Header.Set("Accept-Encoding", "identity")

	response, err := p.client.Do(upstreamRequest)
	if err != nil {
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
	// Flushing after every chunk keeps streamed provider responses (SSE)
	// flowing to the ACP child without buffering delays.
	flusher, _ := w.(http.Flusher)
	if err := providerproxy.StreamBoundedResponse(w, response.Body, p.maxResponseBytes, flusher); err != nil {
		panic(http.ErrAbortHandler)
	}
}

func normalizeProviderRequestBody(providerKind, model, requestPath string, modelOutputLimit int64, body []byte) ([]byte, error) {
	if providerKind != providerKindOpencode ||
		(requestPath != providerOpenAIChatCompletionsPath && requestPath != providerOpenAIChatCompletionsV1Path) {
		return body, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode OpenCode provider request: %w", err)
	}
	if err := ensureProviderJSONEOF(decoder); err != nil {
		return nil, err
	}
	if modelOutputLimit <= 0 {
		return nil, fmt.Errorf("OpenCode model output limit is required")
	}
	providerID, upstreamModel, ok := strings.Cut(model, "/")
	providerID = strings.TrimSpace(providerID)
	upstreamModel = strings.TrimSpace(upstreamModel)
	if !ok || providerID == "" || upstreamModel == "" {
		return nil, fmt.Errorf("OpenCode model must use provider/model form")
	}
	maxTokens, hasMaxTokens, err := positiveProviderOutputLimit(payload, providerMaxTokensField)
	if err != nil {
		return nil, err
	}
	maxCompletionTokens, hasMaxCompletionTokens, err := positiveProviderOutputLimit(payload, providerMaxCompletionTokensField)
	if err != nil {
		return nil, err
	}
	outputLimit := modelOutputLimit
	outputField := providerMaxTokensField
	if strings.EqualFold(providerID, "openai") {
		outputField = providerMaxCompletionTokensField
		delete(payload, providerVerbosityField)
		if tools, ok := payload[providerToolsField].([]any); ok && len(tools) > 0 {
			delete(payload, providerReasoningEffortField)
		}
	}
	if hasMaxTokens && maxTokens < outputLimit {
		outputLimit = maxTokens
	}
	if hasMaxCompletionTokens {
		outputField = providerMaxCompletionTokensField
		if maxCompletionTokens < outputLimit {
			outputLimit = maxCompletionTokens
		}
	}
	delete(payload, providerMaxTokensField)
	delete(payload, providerMaxCompletionTokensField)
	payload[outputField] = outputLimit
	payload["model"] = upstreamModel
	return json.Marshal(payload)
}

func positiveProviderOutputLimit(payload map[string]any, name string) (int64, bool, error) {
	value, ok := payload[name]
	if !ok {
		return 0, false, nil
	}
	number, ok := value.(json.Number)
	if !ok {
		return 0, false, fmt.Errorf("OpenCode %s must be a positive integer", name)
	}
	parsed, err := strconv.ParseInt(string(number), 10, 64)
	if err != nil || parsed <= 0 {
		return 0, false, fmt.Errorf("OpenCode %s must be a positive integer", name)
	}
	return parsed, true, nil
}

func ensureProviderJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return fmt.Errorf("provider request contains trailing JSON data")
	}
	return fmt.Errorf("decode provider request trailer: %w", err)
}

func validateProviderRequest(providerKind, model, requestPath, method string, body []byte) (providerRequestClass, error) {
	allowed := false
	requiresModel := false
	class := providerRequestMetadata
	switch providerKind {
	case providerKindCodex, providerKindCopilot:
		switch requestPath {
		case "/responses", providerOpenAIResponsesV1Path, "/responses/compact", "/v1/responses/compact", providerOpenAIChatCompletionsPath, providerOpenAIChatCompletionsV1Path:
			allowed, requiresModel, class = method == http.MethodPost, true, providerRequestInference
		case "/models", providerModelsV1Path:
			allowed = method == http.MethodGet
		}
	case providerKindOpencode:
		switch requestPath {
		case providerOpenAIChatCompletionsPath, providerOpenAIChatCompletionsV1Path:
			allowed, requiresModel, class = method == http.MethodPost, true, providerRequestInference
		}
	case "claude":
		switch requestPath {
		case "/v1/messages":
			allowed, requiresModel, class = method == http.MethodPost, true, providerRequestInference
		case "/v1/messages/count_tokens":
			allowed, requiresModel = method == http.MethodPost, true
		case providerModelsV1Path:
			allowed = method == http.MethodGet
		}
	}
	if !allowed {
		return providerRequestMetadata, fmt.Errorf("provider path or method is not allowed")
	}
	if !requiresModel {
		return class, nil
	}
	var envelope struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || strings.TrimSpace(envelope.Model) != model {
		return providerRequestMetadata, fmt.Errorf("provider model does not match immutable profile")
	}
	return class, nil
}

var errProviderBodyTooLarge = errors.New("provider body too large")

func readBoundedProviderBody(ctx context.Context, body io.ReadCloser, limit int64) ([]byte, error) {
	defer body.Close() //nolint:errcheck
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	if int64(len(data)) > limit {
		return nil, errProviderBodyTooLarge
	}
	return data, nil
}

func splitProviderProxyRoute(path string) (route string, ok bool) {
	if !strings.HasPrefix(path, providerProxyPathPrefix) {
		return "", false
	}
	remainder := strings.TrimPrefix(path, providerProxyPathPrefix)
	route, _, _ = strings.Cut(remainder, "/")
	if route == "" {
		return "", false
	}
	return route, true
}

func (s *providerProxySession) requestSuffix(path string) (string, bool) {
	if path == s.basePath {
		return "/", true
	}
	if !strings.HasPrefix(path, s.basePath+"/") {
		return "", false
	}
	return strings.TrimPrefix(path, s.basePath), true
}

func writeProviderTurnLimitError(w http.ResponseWriter, providerKind string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusBadRequest)
	if providerKind == providerKindClaude {
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"maximum provider inference requests reached for active prompt"}}`+"\n")
		return
	}
	_, _ = io.WriteString(w, `{"error":{"message":"maximum provider inference requests reached for active prompt","type":"invalid_request_error","code":"max_turn_requests"}}`+"\n")
}

func (p *providerProxy) close(ctx context.Context) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	if p.closeDone != nil {
		done := p.closeDone
		p.mu.Unlock()
		select {
		case <-done:
			p.mu.RLock()
			err := p.closeErr
			p.mu.RUnlock()
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	p.closed = true
	p.closeDone = make(chan struct{})
	done := p.closeDone
	sessions := make([]*providerProxySession, 0, len(p.sessions))
	for _, session := range p.sessions {
		sessions = append(sessions, session)
	}
	p.mu.Unlock()
	for _, session := range sessions {
		session.close()
	}
	var errs []error
	for _, session := range sessions {
		if err := session.wait(ctx); err != nil {
			errs = append(errs, fmt.Errorf("wait for provider proxy session requests: %w", err))
		}
	}
	if err := p.server.Shutdown(ctx); err != nil {
		errs = append(errs, err, p.server.Close())
	}
	if transport, ok := p.client.Transport.(*http.Transport); ok {
		transport.CloseIdleConnections()
	}
	closeErr := errors.Join(errs...)
	p.mu.Lock()
	p.closeErr = closeErr
	close(done)
	p.mu.Unlock()
	return closeErr
}
