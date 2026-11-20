// Copyright (c) 2026. MIT License - see LICENSE file for details.

// Package entraclient holds the controller's Microsoft Entra ID token client
// and token cache, shared by every feature that presents the controller's
// projected ServiceAccount token as a federated credential (ADR 0039).
// Credentials and tokens it handles never appear in errors, logs, or metrics.
package entraclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/orka-agents/orka/internal/metrics"
)

const (
	// DefaultAuthority is the public-cloud Entra authority.
	DefaultAuthority = "https://login.microsoftonline.com"

	// JWTBearerAssertionType is the client assertion type of a federated
	// credential.
	JWTBearerAssertionType = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"

	// MaxCachedTokenLifetime bounds how long any token is reused, so a
	// disabled identity or removed role stops working within it.
	MaxCachedTokenLifetime = 5 * time.Minute
	// MaxCachedTokens bounds the cache however many identities and targets
	// the controller serves.
	MaxCachedTokens = 1024

	tokenExpirySafetyMargin = time.Minute
	maxTokenResponseBytes   = 1 << 20
	// RequestDeadline bounds one token request.
	RequestDeadline = 15 * time.Second
	// flightDeadline bounds one shared fetch, which may take two requests.
	flightDeadline = 2 * RequestDeadline
)

// ErrIssuerUnavailable marks an issuer that could not answer, as opposed to
// one that refused: a timeout, a network failure, throttling, or a server
// error. Callers may retry it later.
var ErrIssuerUnavailable = errors.New("the identity issuer is unavailable")

// FileCredential reads a projected ServiceAccount token from path on every
// call, since the kubelet rotates it. A missing or empty token is
// ErrIssuerUnavailable: Entra was never asked, so it says nothing about the
// identity.
func FileCredential(path string) func() (string, error) {
	return func() (string, error) {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read the controller's federated credential: %w: %w", err, ErrIssuerUnavailable)
		}
		token := strings.TrimSpace(string(data))
		if token == "" {
			return "", fmt.Errorf("the controller's federated credential is empty: %w", ErrIssuerUnavailable)
		}
		return token, nil
	}
}

// CanonicalAuthority returns the authority with no surrounding space or
// trailing slash, scheme and host in lower case, the path as given, and
// DefaultAuthority when unset. Paths are case-sensitive, so they are never
// folded.
func CanonicalAuthority(value string) string {
	authority := strings.TrimSuffix(strings.TrimSpace(value), "/")
	if authority == "" {
		return DefaultAuthority
	}
	parsed, err := url.Parse(authority)
	if err != nil || parsed.Host == "" {
		// ValidateAuthority refuses it; keep the value as given.
		return authority
	}
	parsed.Scheme, parsed.Host = strings.ToLower(parsed.Scheme), strings.ToLower(parsed.Host)
	return parsed.String()
}

// ValidateAuthority rejects an authority that is not an https URL with a
// host, or that carries credentials, a query, or a fragment.
func ValidateAuthority(value string) error {
	authority, err := url.Parse(CanonicalAuthority(value))
	// A bare "?" or "#" parses to an empty query or fragment but still changes
	// the token endpoint built from the authority, so the delimiters are
	// refused too.
	if err != nil || authority.Scheme != "https" || authority.Hostname() == "" || authority.User != nil ||
		authority.RawQuery != "" || authority.ForceQuery || authority.Fragment != "" || strings.ContainsAny(value, "?#") {
		return errors.New("the Entra authority must be an https URL with a host and without credentials, query, or fragment")
	}
	return nil
}

// Client posts token requests to one tenant's Entra token endpoint.
type Client struct {
	// Authority is the Entra authority; DefaultAuthority when empty.
	Authority string
	// TenantID is the tenant whose token endpoint is used.
	TenantID string
	// HTTPClient is copied for each request and never follows redirects.
	HTTPClient *http.Client
	// MetricAdapter labels the token exchange metric.
	MetricAdapter string
}

type tokenResponse struct {
	AccessToken string          `json:"access_token"`
	ExpiresIn   json.RawMessage `json:"expires_in"`
	Error       string          `json:"error"`
	ErrorCodes  []int           `json:"error_codes"`
}

// RequestToken posts one token request and returns the token and its
// lifetime. Errors carry only Entra's error code, never a credential or the
// response body. step labels the metric.
func (c *Client) RequestToken(ctx context.Context, step string, form url.Values) (token string, lifetime time.Duration, err error) {
	started := time.Now()
	reason := "issued"
	defer func() {
		result := "success"
		if err != nil {
			result = "error"
		}
		metrics.RecordTokenExchange(c.MetricAdapter, step, result, reason, time.Since(started).Seconds())
	}()
	endpoint := CanonicalAuthority(c.Authority) + "/" + url.PathEscape(strings.ToLower(c.TenantID)) + "/oauth2/v2.0/token"
	ctx, cancel := context.WithTimeout(ctx, RequestDeadline)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, fmt.Errorf("build the Entra token request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: RequestDeadline}
	if c.HTTPClient != nil {
		copied := *c.HTTPClient
		client = &copied
	}
	// A redirect would replay the form, which carries a federated credential
	// or an exchange token, to wherever the response points. Entra's token
	// endpoint never redirects, so the 3xx is returned as is, and counts as
	// the issuer being unavailable.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		reason = "unreachable"
		return "", 0, fmt.Errorf("the Entra token endpoint is unreachable: %w", ErrIssuerUnavailable)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxTokenResponseBytes))
	if err != nil {
		return "", 0, fmt.Errorf("read the Entra token response: %w", ErrIssuerUnavailable)
	}
	var parsed tokenResponse
	if err := json.Unmarshal(body, &parsed); err != nil ||
		(response.StatusCode == http.StatusOK && parsed.AccessToken == "" && strings.TrimSpace(parsed.Error) == "") {
		// An answer that is not a token response, such as a proxy's HTML
		// page or a truncated body, says nothing about the identity, so it is
		// an outage rather than a refusal.
		reason = "malformed_response"
		return "", 0, fmt.Errorf("the Entra token endpoint returned an unreadable response (HTTP %d): %w", response.StatusCode, ErrIssuerUnavailable)
	}
	if response.StatusCode != http.StatusOK || parsed.AccessToken == "" {
		code := OAuthErrorCode(parsed.Error)
		reason = code
		// Only an OAuth error Entra defines refuses the identity. Anything
		// else, such as a proxy's JSON answer or redirect, says nothing about
		// it and must not be kept as a refusal.
		if response.StatusCode >= http.StatusInternalServerError || response.StatusCode == http.StatusTooManyRequests ||
			code == "temporarily_unavailable" || code == "server_error" || code == unknownOAuthError {
			return "", 0, fmt.Errorf("Entra could not answer the token request: %s (HTTP %d): %w", code, response.StatusCode, ErrIssuerUnavailable) //nolint:staticcheck // Product name begins the message.
		}
		if len(parsed.ErrorCodes) > 0 {
			return "", 0, fmt.Errorf("Entra refused the token request: %s (AADSTS%d, HTTP %d)", code, parsed.ErrorCodes[0], response.StatusCode) //nolint:staticcheck // Product name begins the message.
		}
		return "", 0, fmt.Errorf("Entra refused the token request: %s (HTTP %d)", code, response.StatusCode) //nolint:staticcheck // Product name begins the message.
	}
	return parsed.AccessToken, parseExpiresIn(parsed.ExpiresIn), nil
}

// OAuthErrorCode returns an OAuth error code Orka knows, or "unknown_error".
// The response is the issuer's or a proxy's text, and the code reaches
// status, errors, and a metric label, so nothing else is passed through.
func OAuthErrorCode(value string) string {
	switch code := strings.TrimSpace(value); code {
	case "invalid_request", "invalid_client", "invalid_grant", "unauthorized_client",
		"unsupported_grant_type", "invalid_scope", "invalid_target", "access_denied",
		"interaction_required", "consent_required", "server_error", "temporarily_unavailable":
		return code
	default:
		return unknownOAuthError
	}
}

// unknownOAuthError stands for an error response that carries no OAuth
// error code Orka knows.
const unknownOAuthError = "unknown_error"

// parseExpiresIn accepts expires_in as a number or a numeric string. An
// unknown lifetime is treated as zero, so the token is used once and not cached.
func parseExpiresIn(raw json.RawMessage) time.Duration {
	var seconds int64
	if err := json.Unmarshal(raw, &seconds); err == nil {
		return time.Duration(seconds) * time.Second
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		var parsed int64
		if _, err := fmt.Sscanf(strings.TrimSpace(text), "%d", &parsed); err == nil {
			return time.Duration(parsed) * time.Second
		}
	}
	return 0
}

// Cache holds short-lived tokens under digested keys. The zero value is
// ready to use.
type Cache struct {
	// Now returns the current time; time.Now when nil.
	Now func() time.Time

	mu      sync.Mutex
	entries map[string]cachedToken
	// flights coalesces concurrent misses for one key into a single fetch.
	flights singleflight.Group
	// FlightJoined, when set, runs once a caller has started or joined a
	// flight; tests use it to order callers.
	FlightJoined func()
}

type cachedToken struct {
	value   string
	expires time.Time
}

// Len returns the number of cached entries, live or not yet reclaimed.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

func (c *Cache) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Get returns a live cached token for key or fetches one. An entry lives at
// most MaxCachedTokenLifetime, and never past a minute before the token
// expires. The key is digested before it is stored.
//
// Concurrent misses for one key share a single fetch. The fetch runs under a
// context no caller can cancel, bounded by twice RequestDeadline, so a caller
// that gives up does not fail the others; each caller still returns when its
// own context ends. A caller whose deadline passes while it waits gets
// ErrIssuerUnavailable. A failed fetch is returned to every waiter and
// nothing is cached.
func (c *Cache) Get(ctx context.Context, key string, fetch func(context.Context) (string, time.Duration, error)) (string, error) {
	digest := sha256.Sum256([]byte(key))
	cacheKey := hex.EncodeToString(digest[:])
	if token, ok := c.lookup(cacheKey); ok {
		return token, nil
	}
	results := c.flights.DoChan(cacheKey, func() (any, error) {
		if token, ok := c.lookup(cacheKey); ok {
			return token, nil
		}
		flightCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), flightDeadline)
		defer cancel()
		now := c.now()
		token, lifetime, err := fetch(flightCtx)
		if err != nil {
			return "", err
		}
		lifetime -= tokenExpirySafetyMargin
		if lifetime > MaxCachedTokenLifetime {
			lifetime = MaxCachedTokenLifetime
		}
		if lifetime > 0 {
			c.mu.Lock()
			c.storeLocked(cacheKey, cachedToken{value: token, expires: now.Add(lifetime)}, c.now())
			c.mu.Unlock()
		}
		return token, nil
	})
	if c.FlightJoined != nil {
		c.FlightJoined()
	}
	select {
	case result := <-results:
		if result.Err != nil {
			return "", result.Err
		}
		token, _ := result.Val.(string)
		return token, nil
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("the Entra token request did not finish in time: %w", ErrIssuerUnavailable)
		}
		return "", ctx.Err()
	}
}

// lookup returns a live cached token, dropping an expired one.
func (c *Cache) lookup(cacheKey string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[cacheKey]
	if !ok {
		return "", false
	}
	if c.now().Before(entry.expires) {
		return entry.value, true
	}
	delete(c.entries, cacheKey)
	return "", false
}

// storeLocked caches one entry. It first drops every expired entry, so keys
// never looked up again are reclaimed, and when the cache is still full it
// evicts the entries closest to expiry. The caller holds c.mu.
func (c *Cache) storeLocked(key string, entry cachedToken, now time.Time) {
	if c.entries == nil {
		c.entries = map[string]cachedToken{}
	}
	for existing, cached := range c.entries {
		if !now.Before(cached.expires) {
			delete(c.entries, existing)
		}
	}
	if _, replacing := c.entries[key]; !replacing {
		for len(c.entries) >= MaxCachedTokens {
			oldest, oldestExpiry := "", time.Time{}
			for existing, cached := range c.entries {
				if oldest == "" || cached.expires.Before(oldestExpiry) {
					oldest, oldestExpiry = existing, cached.expires
				}
			}
			delete(c.entries, oldest)
		}
	}
	c.entries[key] = entry
}
