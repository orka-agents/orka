/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

// Package tokenexchange implements reusable RFC 8693 token exchange and RFC
// 7523 JWT bearer grants. It deliberately knows nothing about transaction-token
// governance or Kubernetes policy objects; callers supply already-resolved
// inputs and choose the response validation class they require.
package tokenexchange

import (
	"bytes"
	"container/list"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/orka-agents/orka/internal/metrics"
	"github.com/orka-agents/orka/internal/redact"
)

const (
	GrantTypeTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange"
	GrantTypeJWTBearer     = "urn:ietf:params:oauth:grant-type:jwt-bearer"

	TokenTypeAccessToken = "urn:ietf:params:oauth:token-type:access_token"
	TokenTypeJWT         = "urn:ietf:params:oauth:token-type:jwt"

	ClientAssertionTypeJWTBearer = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"

	ClientAuthNone             = "none"
	ClientAuthSecretBasic      = "client-secret-basic"
	ClientAuthSecretPost       = "client-secret-post"
	ClientAuthPrivateKeyJWT    = "private_key_jwt"
	defaultTimeout             = 10 * time.Second
	defaultResponseLimit       = int64(1 << 20)
	defaultMaxCacheEntries     = 256
	defaultAssertionLifetime   = 5 * time.Minute
	maxPrivateJWTClockSkewPast = 30 * time.Second
	maxPublicEndpointAddresses = 16
	metricResultFailure        = "failure"
	metricReasonInvalidRequest = "invalid_request"
)

var reservedParameters = map[string]struct{}{
	"grant_type":            {},
	"subject_token":         {},
	"subject_token_type":    {},
	"actor_token":           {},
	"actor_token_type":      {},
	"assertion":             {},
	"scope":                 {},
	"audience":              {},
	"resource":              {},
	"requested_token_type":  {},
	"client_id":             {},
	"client_secret":         {},
	"client_assertion":      {},
	"client_assertion_type": {},
}

// IsReservedParameter reports whether name is owned by OAuth grant or client
// authentication construction and therefore cannot be supplied statically.
func IsReservedParameter(name string) bool {
	_, reserved := reservedParameters[strings.ToLower(strings.TrimSpace(name))]
	return reserved
}

// TLSConfig controls TLS verification for one exchange endpoint.
type TLSConfig struct {
	ServerName string
	CAPEM      []byte
}

// ClientAuthentication describes OAuth client authentication. Secret and
// signing-key values must be resolved before calling Exchange and are never
// included verbatim in cache keys or errors.
type ClientAuthentication struct {
	Method        string
	ClientID      string
	ClientSecret  string
	PrivateKeyPEM []byte
	KeyID         string
	Audience      string
	AssertionTTL  time.Duration
}

// Request is a fully resolved OAuth exchange request.
type Request struct {
	Adapter string

	Endpoint              string
	Timeout               time.Duration
	TLS                   TLSConfig
	RequirePublicEndpoint bool
	DisableProxy          bool

	GrantType        string
	SubjectToken     string
	SubjectTokenType string
	SubjectExpiresAt time.Time
	ActorToken       string
	ActorTokenType   string
	ActorExpiresAt   time.Time

	Audiences            []string
	Scopes               []string
	Resources            []string
	RequestedTokenType   string
	AdditionalParameters map[string]string
	ClientAuthentication ClientAuthentication

	// ExpectedIssuedTokenType requires an exact issued_token_type match.
	ExpectedIssuedTokenType string
	// RequiredTokenType requires an exact, case-insensitive token_type match.
	// Resource credentials normally set this to Bearer; transaction-token
	// callers set it to N_A.
	RequiredTokenType string

	// CacheNamespace should be a safe digest of the policy generation/config.
	// The client combines it with digests of all exchange inputs.
	CacheNamespace string
}

// Result is a validated OAuth token response.
type Result struct {
	AccessToken     string
	IssuedTokenType string
	TokenType       string
	ExpiresIn       time.Duration
	ExpiresAt       time.Time
}

// ExchangeError is a safe OAuth/HTTP failure. Description is redacted before
// storage and never contains raw response bodies.
type ExchangeError struct {
	StatusCode  int
	Code        string
	Description string
}

func (e *ExchangeError) Error() string {
	if e == nil {
		return "token exchange failed"
	}
	if e.Code != "" && e.Description != "" {
		return fmt.Sprintf("token endpoint error %q: %s", e.Code, e.Description)
	}
	if e.Code != "" {
		return fmt.Sprintf("token endpoint error %q", e.Code)
	}
	if e.StatusCode != 0 {
		return fmt.Sprintf("token endpoint returned HTTP %d", e.StatusCode)
	}
	return "token exchange failed"
}

// Exchanger is the small shared exchange seam used by transaction-token and
// resource-credential adapters.
type Exchanger interface {
	Exchange(context.Context, Request) (Result, error)
}

// ClientOptions configures a reusable exchange client.
type ClientOptions struct {
	HTTPClient       *http.Client
	MaxResponseBytes int64
	MaxCacheEntries  int
	Now              func() time.Time
}

type cacheEntry struct {
	key       string
	result    Result
	expiresAt time.Time
}

type endpointDialResult struct {
	connection net.Conn
	err        error
}

type exchangeFlight struct {
	done      chan struct{}
	result    Result
	err       error
	cancel    context.CancelFunc
	waiters   int
	completed bool
}

// Client executes exchanges with bounded digest-keyed caching and singleflight.
type Client struct {
	httpClient       *http.Client
	maxResponseBytes int64
	maxCacheEntries  int
	now              func() time.Time

	mu       sync.Mutex
	cache    map[string]*list.Element
	cacheLRU *list.List

	flightMu sync.Mutex
	flights  map[string]*exchangeFlight
}

// NewClient creates an exchange client.
func NewClient(opts ClientOptions) *Client {
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}
	limit := opts.MaxResponseBytes
	if limit <= 0 {
		limit = defaultResponseLimit
	}
	entries := opts.MaxCacheEntries
	if entries <= 0 {
		entries = defaultMaxCacheEntries
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Client{
		httpClient:       httpClient,
		maxResponseBytes: limit,
		maxCacheEntries:  entries,
		now:              now,
		cache:            make(map[string]*list.Element),
		cacheLRU:         list.New(),
		flights:          make(map[string]*exchangeFlight),
	}
}

// Exchange validates, executes, validates the response, and caches successful
// exchanges. Cache and singleflight keys contain only SHA-256 digests.
func (c *Client) Exchange(ctx context.Context, req Request) (Result, error) {
	if c == nil {
		return Result{}, errors.New("token exchange client is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	start := c.now()
	grantClass := metricGrantClass(req.GrantType)
	resultLabel, reason := "success", "ok"
	defer func() {
		metrics.RecordTokenExchange(metricAdapter(req.Adapter), grantClass, resultLabel, reason, c.now().Sub(start).Seconds())
	}()

	if err := validateRequest(req); err != nil {
		resultLabel, reason = metricResultFailure, metricReasonInvalidRequest
		return Result{}, err
	}
	req = cloneRequest(req)

	cacheKey, err := digestRequest(req)
	if err != nil {
		resultLabel, reason = metricResultFailure, metricReasonInvalidRequest
		return Result{}, err
	}
	if cached, ok := c.cached(cacheKey); ok {
		if err := ctx.Err(); err != nil {
			resultLabel, reason = metricResultFailure, exchangeFailureReason(err)
			return Result{}, err
		}
		reason = "cache_hit"
		return cached, nil
	}

	flightKey := digestFlightKey(cacheKey, req.Timeout)
	result, err := c.exchangeWithFlight(ctx, flightKey, cacheKey, req)
	if err != nil {
		resultLabel, reason = metricResultFailure, exchangeFailureReason(err)
		return Result{}, err
	}
	return result, nil
}

func (c *Client) exchangeWithFlight(ctx context.Context, flightKey, cacheKey string, req Request) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	c.flightMu.Lock()
	flight := c.flights[flightKey]
	if flight == nil {
		exchangeCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		flight = &exchangeFlight{done: make(chan struct{}), cancel: cancel}
		c.flights[flightKey] = flight
		go c.runFlight(exchangeCtx, flightKey, cacheKey, req, flight)
	}
	flight.waiters++
	c.flightMu.Unlock()

	select {
	case <-ctx.Done():
		c.leaveFlight(flightKey, flight)
		return Result{}, ctx.Err()
	case <-flight.done:
		c.leaveFlight(flightKey, flight)
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		return flight.result, flight.err
	}
}

func (c *Client) runFlight(ctx context.Context, flightKey, cacheKey string, req Request, flight *exchangeFlight) {
	result, ok := c.cached(cacheKey)
	var err error
	if !ok {
		result, err = c.exchange(ctx, req)
		if err == nil {
			c.store(cacheKey, result, req.SubjectExpiresAt, req.ActorExpiresAt)
		}
	}
	c.flightMu.Lock()
	flight.result = result
	flight.err = err
	flight.completed = true
	if current := c.flights[flightKey]; current == flight {
		delete(c.flights, flightKey)
	}
	close(flight.done)
	c.flightMu.Unlock()
	flight.cancel()
}

func (c *Client) leaveFlight(flightKey string, flight *exchangeFlight) {
	c.flightMu.Lock()
	defer c.flightMu.Unlock()
	if flight.waiters > 0 {
		flight.waiters--
	}
	if flight.waiters == 0 && !flight.completed {
		if current := c.flights[flightKey]; current == flight {
			delete(c.flights, flightKey)
		}
		flight.cancel()
	}
}

func cloneRequest(req Request) Request {
	cloned := req
	cloned.TLS.CAPEM = append([]byte(nil), req.TLS.CAPEM...)
	cloned.Audiences = append([]string(nil), req.Audiences...)
	cloned.Scopes = append([]string(nil), req.Scopes...)
	cloned.Resources = append([]string(nil), req.Resources...)
	cloned.ClientAuthentication.PrivateKeyPEM = append(
		[]byte(nil), req.ClientAuthentication.PrivateKeyPEM...,
	)
	if req.AdditionalParameters != nil {
		cloned.AdditionalParameters = make(map[string]string, len(req.AdditionalParameters))
		maps.Copy(cloned.AdditionalParameters, req.AdditionalParameters)
	}
	return cloned
}

func validateRequest(req Request) error {
	endpoint := strings.TrimSpace(req.Endpoint)
	if endpoint == "" {
		return errors.New("token endpoint is required")
	}
	if endpoint != req.Endpoint {
		return errors.New("token endpoint must not contain surrounding whitespace")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil ||
		parsed.Fragment != "" || strings.Contains(endpoint, "#") || parsed.String() != endpoint {
		return errors.New("token endpoint must be an exact absolute URL without userinfo or fragment")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("token endpoint scheme must be http or https")
	}
	if req.RequirePublicEndpoint && parsed.Scheme != "https" {
		return errors.New("public token endpoints must use https")
	}
	switch req.GrantType {
	case GrantTypeTokenExchange:
		if strings.TrimSpace(req.SubjectToken) == "" {
			return errors.New("subject token is required for token exchange")
		}
		if strings.TrimSpace(req.SubjectTokenType) == "" {
			return errors.New("subject token type is required for token exchange")
		}
		if (req.ActorToken == "") != (req.ActorTokenType == "") {
			return errors.New("actor token and actor token type must be configured together")
		}
	case GrantTypeJWTBearer:
		if strings.TrimSpace(req.SubjectToken) == "" {
			return errors.New("JWT bearer assertion is required")
		}
		if req.ActorToken != "" || req.ActorTokenType != "" || req.SubjectTokenType != "" {
			return errors.New("JWT bearer grants do not accept subject or actor token types")
		}
	default:
		return fmt.Errorf("unsupported OAuth grant type %q", req.GrantType)
	}
	for name := range req.AdditionalParameters {
		if IsReservedParameter(name) {
			return fmt.Errorf("additional parameter %q is reserved", name)
		}
		if strings.TrimSpace(name) == "" {
			return errors.New("additional parameter names must not be empty")
		}
	}
	return validateClientAuthentication(req.ClientAuthentication)
}

func validateClientAuthentication(auth ClientAuthentication) error {
	method := strings.TrimSpace(auth.Method)
	if method != auth.Method {
		return errors.New("client authentication method must not contain surrounding whitespace")
	}
	if method == "" {
		method = ClientAuthNone
	}
	switch method {
	case ClientAuthNone:
		if auth.ClientSecret != "" || len(auth.PrivateKeyPEM) != 0 {
			return errors.New("client authentication credentials require an authentication method")
		}
	case ClientAuthSecretBasic, ClientAuthSecretPost:
		if strings.TrimSpace(auth.ClientID) == "" || auth.ClientSecret == "" {
			return fmt.Errorf("%s requires client ID and client secret", method)
		}
		if len(auth.PrivateKeyPEM) != 0 {
			return fmt.Errorf("%s does not accept a private key", method)
		}
	case ClientAuthPrivateKeyJWT:
		if strings.TrimSpace(auth.ClientID) == "" || len(auth.PrivateKeyPEM) == 0 {
			return errors.New("private_key_jwt requires client ID and private key")
		}
		if auth.ClientSecret != "" {
			return errors.New("private_key_jwt does not accept a client secret")
		}
	default:
		return fmt.Errorf("unsupported client authentication method %q", method)
	}
	return nil
}

func (c *Client) exchange(ctx context.Context, req Request) (Result, error) {
	form, err := buildForm(req)
	if err != nil {
		return Result{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, req.Endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Result{}, fmt.Errorf("create token exchange request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpReq.Header.Set("Accept", "application/json")
	if req.ClientAuthentication.Method == ClientAuthSecretBasic {
		httpReq.SetBasicAuth(url.QueryEscape(req.ClientAuthentication.ClientID), url.QueryEscape(req.ClientAuthentication.ClientSecret))
	}

	httpClient, err := c.clientFor(req)
	if err != nil {
		return Result{}, err
	}
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return Result{}, context.DeadlineExceeded
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return Result{}, context.DeadlineExceeded
		}
		return Result{}, errors.New("token endpoint request failed")
	}
	defer resp.Body.Close() //nolint:errcheck

	body, err := readResponse(resp.Body, resp.ContentLength, c.maxResponseBytes)
	if err != nil {
		return Result{}, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		secrets := []string{req.SubjectToken, req.ActorToken, req.ClientAuthentication.ClientSecret, form.Get("client_assertion")}
		return Result{}, decodeOAuthError(resp.StatusCode, body, secrets...)
	}

	var decoded struct {
		AccessToken     string      `json:"access_token"`
		IssuedTokenType string      `json:"issued_token_type"`
		TokenType       string      `json:"token_type"`
		ExpiresIn       json.Number `json:"expires_in"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		return Result{}, fmt.Errorf("decode token endpoint response: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Result{}, errors.New("decode token endpoint response: trailing data")
	}
	if strings.TrimSpace(decoded.AccessToken) == "" {
		return Result{}, errors.New("token endpoint response missing access_token")
	}
	if req.ExpectedIssuedTokenType != "" && decoded.IssuedTokenType != req.ExpectedIssuedTokenType {
		return Result{}, errors.New("token endpoint issued_token_type does not match expected type")
	}
	if req.RequiredTokenType != "" && !strings.EqualFold(strings.TrimSpace(decoded.TokenType), req.RequiredTokenType) {
		return Result{}, errors.New("token endpoint token_type does not match required type")
	}

	result := Result{
		AccessToken:     decoded.AccessToken,
		IssuedTokenType: decoded.IssuedTokenType,
		TokenType:       decoded.TokenType,
	}
	if decoded.ExpiresIn != "" {
		seconds, err := strconv.ParseInt(decoded.ExpiresIn.String(), 10, 64)
		const maxExpiresInSeconds = int64(^uint64(0)>>1) / int64(time.Second)
		if err != nil || seconds <= 0 || seconds > maxExpiresInSeconds {
			return Result{}, errors.New("token endpoint expires_in must be a representable positive integer")
		}
		result.ExpiresIn = time.Duration(seconds) * time.Second
		result.ExpiresAt = c.now().Add(result.ExpiresIn)
	}
	return result, nil
}

func buildForm(req Request) (url.Values, error) {
	form := make(url.Values)
	form.Set("grant_type", req.GrantType)
	switch req.GrantType {
	case GrantTypeTokenExchange:
		form.Set("subject_token", req.SubjectToken)
		form.Set("subject_token_type", req.SubjectTokenType)
		if req.ActorToken != "" {
			form.Set("actor_token", req.ActorToken)
			form.Set("actor_token_type", req.ActorTokenType)
		}
		if req.RequestedTokenType != "" {
			form.Set("requested_token_type", req.RequestedTokenType)
		}
	case GrantTypeJWTBearer:
		form.Set("assertion", req.SubjectToken)
	}
	for _, audience := range req.Audiences {
		if audience = strings.TrimSpace(audience); audience != "" {
			form.Add("audience", audience)
		}
	}
	for _, resource := range req.Resources {
		if resource = strings.TrimSpace(resource); resource != "" {
			form.Add("resource", resource)
		}
	}
	if scopes := normalizedValues(req.Scopes); len(scopes) > 0 {
		form.Set("scope", strings.Join(scopes, " "))
	}
	keys := make([]string, 0, len(req.AdditionalParameters))
	for key := range req.AdditionalParameters {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		form.Set(key, req.AdditionalParameters[key])
	}

	auth := req.ClientAuthentication
	method := auth.Method
	if method == "" {
		method = ClientAuthNone
	}
	switch method {
	case ClientAuthNone:
		if strings.TrimSpace(auth.ClientID) != "" {
			form.Set("client_id", auth.ClientID)
		}
	case ClientAuthSecretBasic:
		// HTTP Basic header is applied after request creation.
	case ClientAuthSecretPost:
		form.Set("client_id", auth.ClientID)
		form.Set("client_secret", auth.ClientSecret)
	case ClientAuthPrivateKeyJWT:
		assertion, err := signClientAssertion(req.Endpoint, auth)
		if err != nil {
			return nil, err
		}
		form.Set("client_id", auth.ClientID)
		form.Set("client_assertion_type", ClientAssertionTypeJWTBearer)
		form.Set("client_assertion", assertion)
	}
	return form, nil
}

func (c *Client) clientFor(req Request) (*http.Client, error) {
	client := *c.httpClient
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	if req.Timeout > 0 {
		client.Timeout = req.Timeout
	} else if client.Timeout <= 0 {
		client.Timeout = defaultTimeout
	}
	if len(req.TLS.CAPEM) == 0 && strings.TrimSpace(req.TLS.ServerName) == "" &&
		!req.RequirePublicEndpoint && !req.DisableProxy {
		return &client, nil
	}
	baseTransport := c.httpClient.Transport
	if baseTransport == nil {
		baseTransport = http.DefaultTransport
	}
	transport, ok := baseTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("custom TLS settings require an *http.Transport")
	}
	clone := transport.Clone()
	clone.DisableKeepAlives = true
	if len(req.TLS.CAPEM) > 0 || strings.TrimSpace(req.TLS.ServerName) != "" {
		clone.DialTLS = nil //nolint:staticcheck // Request TLS verification must not be bypassed by a legacy hook.
		clone.DialTLSContext = nil
		var tlsConfig *tls.Config
		if clone.TLSClientConfig != nil {
			tlsConfig = clone.TLSClientConfig.Clone()
		} else {
			tlsConfig = &tls.Config{}
		}
		tlsConfig.MinVersion = max(tlsConfig.MinVersion, tls.VersionTLS12)
		tlsConfig.InsecureSkipVerify = false
		if serverName := strings.TrimSpace(req.TLS.ServerName); serverName != "" {
			tlsConfig.ServerName = serverName
		}
		if len(req.TLS.CAPEM) > 0 {
			roots, err := x509.SystemCertPool()
			if err != nil || roots == nil {
				roots = x509.NewCertPool()
			}
			if !roots.AppendCertsFromPEM(req.TLS.CAPEM) {
				return nil, errors.New("token endpoint CA secret does not contain a valid PEM certificate")
			}
			tlsConfig.RootCAs = roots
		}
		clone.TLSClientConfig = tlsConfig
	}
	if req.RequirePublicEndpoint || req.DisableProxy {
		clone.Proxy = nil
	}
	if req.RequirePublicEndpoint {
		clone.DialTLS = nil //nolint:staticcheck // Clear the legacy hook so it cannot bypass public-address validation.
		clone.DialTLSContext = nil
		clone.DialContext = publicEndpointDialContext
		if clone.TLSClientConfig != nil {
			clone.TLSClientConfig = clone.TLSClientConfig.Clone()
		} else {
			clone.TLSClientConfig = &tls.Config{}
		}
		clone.TLSClientConfig.MinVersion = max(clone.TLSClientConfig.MinVersion, tls.VersionTLS12)
		clone.TLSClientConfig.InsecureSkipVerify = false
		if strings.TrimSpace(req.TLS.ServerName) == "" {
			clone.TLSClientConfig.ServerName = ""
		}
	}
	disableHTTP2(clone)
	client.Transport = clone
	return &client, nil
}

func disableHTTP2(transport *http.Transport) {
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	transport.Protocols = protocols
	transport.ForceAttemptHTTP2 = false
	transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	if transport.TLSClientConfig != nil {
		transport.TLSClientConfig.NextProtos = []string{"http/1.1"}
	}
}

// PublicEndpointDialContext resolves and dials only globally routable addresses.
func PublicEndpointDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return publicEndpointDialContext(ctx, network, address)
}

func publicEndpointDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, errors.New("token endpoint address is invalid")
	}
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, errors.New("token endpoint host could not be resolved")
	}
	if err := validatePublicEndpointAddresses(addresses); err != nil {
		return nil, err
	}
	dialCtx, cancel := context.WithCancel(ctx)
	results := make(chan endpointDialResult, len(addresses))
	for _, address := range addresses {
		go func() {
			dialer := &net.Dialer{}
			connection, dialErr := dialer.DialContext(
				dialCtx, network, net.JoinHostPort(address.IP.String(), port),
			)
			results <- endpointDialResult{connection: connection, err: dialErr}
		}()
	}
	var lastErr error
	for remaining := len(addresses); remaining > 0; remaining-- {
		select {
		case <-ctx.Done():
			cancel()
			go closeUnusedDialResults(results, remaining)
			return nil, ctx.Err()
		case result := <-results:
			if result.err == nil && result.connection != nil {
				cancel()
				go closeUnusedDialResults(results, remaining-1)
				return result.connection, nil
			}
			lastErr = result.err
		}
	}
	cancel()
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errors.New("token endpoint has no dialable public address")
}

func closeUnusedDialResults(results <-chan endpointDialResult, remaining int) {
	for range remaining {
		result := <-results
		if result.connection != nil {
			_ = result.connection.Close()
		}
	}
}

var globallyAllocatedIPv6Prefixes = []netip.Prefix{
	netip.MustParsePrefix("2001:200::/23"),
	netip.MustParsePrefix("2001:400::/23"),
	netip.MustParsePrefix("2001:600::/23"),
	netip.MustParsePrefix("2001:800::/22"),
	netip.MustParsePrefix("2001:c00::/23"),
	netip.MustParsePrefix("2001:e00::/23"),
	netip.MustParsePrefix("2001:1200::/23"),
	netip.MustParsePrefix("2001:1400::/22"),
	netip.MustParsePrefix("2001:1800::/23"),
	netip.MustParsePrefix("2001:1a00::/23"),
	netip.MustParsePrefix("2001:1c00::/22"),
	netip.MustParsePrefix("2001:2000::/19"),
	netip.MustParsePrefix("2001:4000::/23"),
	netip.MustParsePrefix("2001:4200::/23"),
	netip.MustParsePrefix("2001:4400::/23"),
	netip.MustParsePrefix("2001:4600::/23"),
	netip.MustParsePrefix("2001:4800::/23"),
	netip.MustParsePrefix("2001:4a00::/23"),
	netip.MustParsePrefix("2001:4c00::/23"),
	netip.MustParsePrefix("2001:5000::/20"),
	netip.MustParsePrefix("2001:8000::/19"),
	netip.MustParsePrefix("2001:a000::/20"),
	netip.MustParsePrefix("2001:b000::/20"),
	netip.MustParsePrefix("2003::/18"),
	netip.MustParsePrefix("2400::/12"),
	netip.MustParsePrefix("2410::/12"),
	netip.MustParsePrefix("2600::/12"),
	netip.MustParsePrefix("2610::/23"),
	netip.MustParsePrefix("2620::/23"),
	netip.MustParsePrefix("2630::/12"),
	netip.MustParsePrefix("2800::/12"),
	netip.MustParsePrefix("2a00::/12"),
	netip.MustParsePrefix("2a10::/12"),
	netip.MustParsePrefix("2c00::/12"),
}

var nonPublicSpecialUsePrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("100:0:0:1::/64"),
	netip.MustParsePrefix("2001:2::/48"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2001:10::/28"),
	netip.MustParsePrefix("2001:20::/28"),
	netip.MustParsePrefix("3fff::/20"),
	netip.MustParsePrefix("5f00::/16"),
}

func validatePublicEndpointAddresses(addresses []net.IPAddr) error {
	if len(addresses) == 0 {
		return errors.New("token endpoint host could not be resolved")
	}
	if len(addresses) > maxPublicEndpointAddresses {
		return errors.New("token endpoint resolved to too many addresses")
	}
	for _, address := range addresses {
		if !IsPublicAddress(address.IP) {
			return errors.New("token endpoint resolved to a non-public address")
		}
	}
	return nil
}

// IsPublicAddress reports whether ip is globally routable and not within an
// IANA special-use range that must never receive outbound credentials.
func IsPublicAddress(ip net.IP) bool {
	if ip == nil {
		return false
	}
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	address = address.Unmap()
	if address.Is6() {
		allocated := false
		for _, prefix := range globallyAllocatedIPv6Prefixes {
			if prefix.Contains(address) {
				allocated = true
				break
			}
		}
		if !allocated {
			return false
		}
	}
	if !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() ||
		address.IsLinkLocalUnicast() || address.IsMulticast() || address.IsUnspecified() {
		return false
	}
	for _, prefix := range nonPublicSpecialUsePrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

func readResponse(reader io.Reader, contentLength, limit int64) ([]byte, error) {
	if limit <= 0 {
		limit = defaultResponseLimit
	}
	if contentLength > limit {
		return nil, fmt.Errorf("token endpoint response exceeds %d bytes", limit)
	}
	limited := io.LimitReader(reader, limit+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read token endpoint response: %w", err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("token endpoint response exceeds %d bytes", limit)
	}
	return body, nil
}

func decodeOAuthError(status int, body []byte, secrets ...string) error {
	var decoded struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if json.Unmarshal(body, &decoded) == nil && strings.TrimSpace(decoded.Error) != "" {
		return &ExchangeError{
			StatusCode:  status,
			Code:        safeOAuthField(decoded.Error, secrets...),
			Description: safeOAuthField(decoded.ErrorDescription, secrets...),
		}
	}
	return &ExchangeError{StatusCode: status, Description: safeOAuthField(strings.TrimSpace(string(body)), secrets...)}
}

func safeOAuthField(value string, secrets ...string) string {
	for _, secret := range secrets {
		if secret = strings.TrimSpace(secret); secret != "" {
			value = strings.ReplaceAll(value, secret, "[REDACTED]")
		}
	}
	value = redact.SensitiveText(value)
	if len(value) > 512 {
		value = value[:512]
	}
	return value
}

func signClientAssertion(endpoint string, auth ClientAuthentication) (string, error) {
	key, alg, err := parseSigningKey(auth.PrivateKeyPEM)
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	ttl := auth.AssertionTTL
	if ttl <= 0 {
		ttl = defaultAssertionLifetime
	}
	audience := strings.TrimSpace(auth.Audience)
	if audience == "" {
		audience = endpoint
	}
	jtiBytes := make([]byte, 16)
	if _, err := rand.Read(jtiBytes); err != nil {
		return "", fmt.Errorf("generate private_key_jwt jti: %w", err)
	}
	header := map[string]any{"alg": alg, "typ": "JWT"}
	if strings.TrimSpace(auth.KeyID) != "" {
		header["kid"] = strings.TrimSpace(auth.KeyID)
	}
	claims := map[string]any{
		"iss": auth.ClientID,
		"sub": auth.ClientID,
		"aud": audience,
		"iat": now.Add(-maxPrivateJWTClockSkewPast).Unix(),
		"exp": now.Add(ttl).Unix(),
		"jti": hex.EncodeToString(jtiBytes),
	}
	return signJWT(header, claims, key)
}

func parseSigningKey(data []byte) (crypto.Signer, string, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, "", errors.New("private_key_jwt key is not valid PEM")
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		switch typed := key.(type) {
		case *rsa.PrivateKey:
			return typed, "RS256", nil
		case *ecdsa.PrivateKey:
			if typed.Curve != elliptic.P256() {
				return nil, "", errors.New("private_key_jwt supports only P-256 ECDSA keys")
			}
			return typed, "ES256", nil
		}
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, "RS256", nil
	}
	if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		if key.Curve != elliptic.P256() {
			return nil, "", errors.New("private_key_jwt supports only P-256 ECDSA keys")
		}
		return key, "ES256", nil
	}
	return nil, "", errors.New("private_key_jwt key must be RSA or P-256 ECDSA")
}

func signJWT(header, claims any, signer crypto.Signer) (string, error) {
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	unsigned := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(claimsJSON)
	digest := sha256.Sum256([]byte(unsigned))
	var signature []byte
	switch key := signer.(type) {
	case *rsa.PrivateKey:
		signature, err = rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	case *ecdsa.PrivateKey:
		r, s, signErr := ecdsa.Sign(rand.Reader, key, digest[:])
		if signErr != nil {
			err = signErr
			break
		}
		byteLen := (key.Curve.Params().BitSize + 7) / 8
		signature = make([]byte, byteLen*2)
		r.FillBytes(signature[:byteLen])
		s.FillBytes(signature[byteLen:])
	default:
		err = errors.New("unsupported private_key_jwt signing key")
	}
	if err != nil {
		return "", fmt.Errorf("sign private_key_jwt: %w", err)
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func digestFlightKey(cacheKey string, timeout time.Duration) string {
	sum := sha256.Sum256([]byte(cacheKey + "\x00" + timeout.String()))
	return hex.EncodeToString(sum[:])
}

func digestRequest(req Request) (string, error) {
	type digestShape struct {
		CacheNamespace        string
		Endpoint              string
		GrantType             string
		SubjectDigest         string
		SubjectType           string
		SubjectExpiresAt      int64
		ActorDigest           string
		ActorType             string
		ActorExpiresAt        int64
		Audiences             []string
		Scopes                []string
		Resources             []string
		RequestedType         string
		Additional            map[string]string
		ClientMethod          string
		ClientID              string
		ClientSecretDigest    string
		PrivateKeyDigest      string
		ClientKeyID           string
		ClientAudience        string
		ClientAssertionTTL    string
		ExpectedIssuedType    string
		RequiredTokenType     string
		TLSServerName         string
		CADigest              string
		RequirePublicEndpoint bool
		DisableProxy          bool
	}
	shape := digestShape{
		CacheNamespace:        req.CacheNamespace,
		Endpoint:              req.Endpoint,
		GrantType:             req.GrantType,
		SubjectDigest:         digestString(req.SubjectToken),
		SubjectType:           req.SubjectTokenType,
		SubjectExpiresAt:      req.SubjectExpiresAt.UnixNano(),
		ActorDigest:           digestString(req.ActorToken),
		ActorType:             req.ActorTokenType,
		ActorExpiresAt:        req.ActorExpiresAt.UnixNano(),
		Audiences:             append([]string(nil), req.Audiences...),
		Scopes:                append([]string(nil), req.Scopes...),
		Resources:             append([]string(nil), req.Resources...),
		RequestedType:         req.RequestedTokenType,
		Additional:            req.AdditionalParameters,
		ClientMethod:          req.ClientAuthentication.Method,
		ClientID:              req.ClientAuthentication.ClientID,
		ClientSecretDigest:    digestString(req.ClientAuthentication.ClientSecret),
		PrivateKeyDigest:      digestBytes(req.ClientAuthentication.PrivateKeyPEM),
		ClientKeyID:           req.ClientAuthentication.KeyID,
		ClientAudience:        req.ClientAuthentication.Audience,
		ClientAssertionTTL:    req.ClientAuthentication.AssertionTTL.String(),
		ExpectedIssuedType:    req.ExpectedIssuedTokenType,
		RequiredTokenType:     req.RequiredTokenType,
		TLSServerName:         req.TLS.ServerName,
		CADigest:              digestBytes(req.TLS.CAPEM),
		RequirePublicEndpoint: req.RequirePublicEndpoint,
		DisableProxy:          req.DisableProxy,
	}
	data, err := json.Marshal(shape)
	if err != nil {
		return "", fmt.Errorf("digest token exchange request: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func digestString(value string) string { return digestBytes([]byte(value)) }
func digestBytes(value []byte) string {
	if len(value) == 0 {
		return ""
	}
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func (c *Client) cached(key string) (Result, bool) {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.cache[key]
	if !ok {
		return Result{}, false
	}
	entry := element.Value.(*cacheEntry)
	if !entry.expiresAt.After(now) {
		delete(c.cache, key)
		c.cacheLRU.Remove(element)
		return Result{}, false
	}
	c.cacheLRU.MoveToFront(element)
	return entry.result, true
}

func (c *Client) store(key string, result Result, subjectExpiresAt, actorExpiresAt time.Time) {
	expiresAt := result.ExpiresAt
	if expiresAt.IsZero() || !expiresAt.After(c.now()) {
		return
	}
	for _, authorityExpiresAt := range []time.Time{subjectExpiresAt, actorExpiresAt} {
		if !authorityExpiresAt.IsZero() && authorityExpiresAt.Before(expiresAt) {
			expiresAt = authorityExpiresAt
		}
	}
	if !expiresAt.After(c.now()) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if element, ok := c.cache[key]; ok {
		entry := element.Value.(*cacheEntry)
		entry.result = result
		entry.expiresAt = expiresAt
		c.cacheLRU.MoveToFront(element)
		return
	}
	element := c.cacheLRU.PushFront(&cacheEntry{key: key, result: result, expiresAt: expiresAt})
	c.cache[key] = element
	for len(c.cache) > c.maxCacheEntries {
		oldest := c.cacheLRU.Back()
		if oldest == nil {
			break
		}
		entry := oldest.Value.(*cacheEntry)
		delete(c.cache, entry.key)
		c.cacheLRU.Remove(oldest)
	}
}

func normalizedValues(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}

func exchangeFailureReason(err error) string {
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var exchangeErr *ExchangeError
	if errors.As(err, &exchangeErr) {
		switch {
		case exchangeErr.StatusCode == http.StatusUnauthorized || exchangeErr.StatusCode == http.StatusForbidden:
			return "client_auth"
		case exchangeErr.StatusCode >= 500:
			return "endpoint_5xx"
		default:
			return "endpoint_4xx"
		}
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "response exceeds"):
		return "response_too_large"
	case strings.Contains(message, "decode"):
		return "malformed_response"
	case strings.Contains(message, "token_type"), strings.Contains(message, "access_token"), strings.Contains(message, "expires_in"):
		return "invalid_response"
	default:
		return "transport_error"
	}
}

func metricAdapter(adapter string) string {
	switch strings.ToLower(strings.TrimSpace(adapter)) {
	case "direct":
		return "direct"
	case "transaction-token":
		return "transaction-token"
	default:
		return "unknown"
	}
}

func metricGrantClass(grant string) string {
	switch grant {
	case GrantTypeTokenExchange:
		return "rfc8693"
	case GrantTypeJWTBearer:
		return "rfc7523"
	default:
		return "unknown"
	}
}
