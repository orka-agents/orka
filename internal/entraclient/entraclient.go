// Copyright (c) 2026. MIT License - see LICENSE file for details.

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
	DefaultAuthority = "https://login.microsoftonline.com"

	JWTBearerAssertionType = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"

	MaxCachedTokenLifetime = 5 * time.Minute
	MaxCachedTokens        = 1024

	tokenExpirySafetyMargin = time.Minute
	maxTokenResponseBytes   = 1 << 20
	RequestDeadline         = 15 * time.Second
	flightDeadline          = 2 * RequestDeadline
)

var ErrIssuerUnavailable = errors.New("the identity issuer is unavailable")

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

func CanonicalAuthority(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return DefaultAuthority
	}
	authority := strings.TrimSuffix(trimmed, "/")
	if authority == "" {
		return trimmed
	}
	parsed, err := url.Parse(authority)
	if err != nil || parsed.Host == "" {
		return authority
	}
	parsed.Scheme, parsed.Host = strings.ToLower(parsed.Scheme), strings.ToLower(parsed.Host)
	return parsed.String()
}

func ValidateAuthority(value string) error {
	authority, err := url.Parse(CanonicalAuthority(value))
	if err != nil || authority.Scheme != "https" || authority.Hostname() == "" || authority.User != nil ||
		authority.RawQuery != "" || authority.ForceQuery || authority.Fragment != "" || strings.ContainsAny(value, "?#") {
		return errors.New("the Entra authority must be an https URL with a host and without credentials, query, or fragment")
	}
	return nil
}

type Client struct {
	Authority     string
	TenantID      string
	HTTPClient    *http.Client
	MetricAdapter string
}

type tokenResponse struct {
	AccessToken string          `json:"access_token"`
	ExpiresIn   json.RawMessage `json:"expires_in"`
	Error       string          `json:"error"`
	ErrorCodes  []int           `json:"error_codes"`
}

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
		reason = "malformed_response"
		return "", 0, fmt.Errorf("the Entra token endpoint returned an unreadable response (HTTP %d): %w", response.StatusCode, ErrIssuerUnavailable)
	}
	if response.StatusCode != http.StatusOK || parsed.AccessToken == "" {
		code := OAuthErrorCode(parsed.Error)
		reason = code
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

const unknownOAuthError = "unknown_error"

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

type Cache struct {
	Now func() time.Time

	mu           sync.Mutex
	entries      map[string]cachedToken
	flights      singleflight.Group
	FlightJoined func()
}

type cachedToken struct {
	value   string
	expires time.Time
}

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
