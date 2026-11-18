/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package connectors

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/tokenexchange"
)

const (
	defaultOAuthTimeout      = 15 * time.Second
	maxOAuthResponseBytes    = 1 << 20
	oauthErrorCodeMaxLength  = 64
	pkceVerifierBytes        = 32
	oauthStateNonceBytes     = 32
	grantAuthorizationCode   = "authorization_code"
	grantRefreshToken        = "refresh_token"
	pkceMethodS256           = "S256"
	unknownOAuthErrorCode    = "unknown_error"
	headerContentTypeFormURL = "application/x-www-form-urlencoded"
	schemeHTTPS              = "https"
)

var oauthErrorCodePattern = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// OAuthProviderConfig is the resolved, secret-bearing OAuth configuration for
// one ConnectorProvider. It never leaves controller memory.
type OAuthProviderConfig struct {
	AuthorizeURL                  string
	TokenURL                      string
	RevocationURL                 string
	ClientID                      string
	ClientSecret                  string
	ClientAuthentication          string
	PKCE                          bool
	AdditionalAuthorizeParameters map[string]string
}

// ProviderOAuthConfig resolves a provider's public settings plus its client
// secret into an OAuthProviderConfig.
func ProviderOAuthConfig(provider *corev1alpha1.ConnectorProvider, clientSecret string) OAuthProviderConfig {
	if provider == nil {
		return OAuthProviderConfig{}
	}
	oauth := provider.Spec.OAuth
	pkce := true
	if oauth.PKCE != nil {
		pkce = *oauth.PKCE
	}
	auth := oauth.ClientAuthentication
	if auth == "" {
		auth = corev1alpha1.ConnectorClientAuthSecretBasic
	}
	return OAuthProviderConfig{
		AuthorizeURL:                  oauth.AuthorizeURL,
		TokenURL:                      oauth.TokenURL,
		RevocationURL:                 oauth.RevocationURL,
		ClientID:                      oauth.ClientID,
		ClientSecret:                  clientSecret,
		ClientAuthentication:          auth,
		PKCE:                          pkce,
		AdditionalAuthorizeParameters: oauth.AdditionalAuthorizeParameters,
	}
}

// TokenResponse is a sanitized OAuth token endpoint response.
type TokenResponse struct {
	AccessToken  string
	RefreshToken string
	TokenType    string
	// ExpiresAt is zero when the provider reported no expires_in.
	ExpiresAt time.Time
	Scopes    []string
	// ScopePresent reports whether the response carried a scope field at
	// all. An omitted field means "as requested" (RFC 6749 §5.1); an
	// explicitly empty one is a grant of nothing.
	ScopePresent bool
}

// OAuthError is a provider rejection. It carries only the HTTP status and a
// sanitized error code, never the response body.
type OAuthError struct {
	StatusCode int
	Code       string
}

func (e *OAuthError) Error() string {
	if e == nil {
		return "oauth request failed"
	}
	return fmt.Sprintf("oauth request failed: status %d code %s", e.StatusCode, e.Code)
}

// IsInvalidGrant reports whether the provider rejected the grant itself, which
// means the stored material is unusable and the Connection should be marked
// revoked rather than retried.
func (e *OAuthError) IsInvalidGrant() bool {
	return e != nil && (e.Code == "invalid_grant" || e.Code == "bad_refresh_token" || e.Code == "bad_verification_code")
}

// OAuthClient performs the authorization-code, refresh, and revocation calls
// against public HTTPS endpoints. The one exception is the fixture-only
// private-endpoint allowance (OAuthClientOptions.AllowPrivateEndpoints,
// behind --connectors-allow-private-endpoints): it lets the client reach
// private and cluster-local HTTPS endpoints through the hardened
// PrivateEndpointDialContext, which still refuses infrastructure addresses.
// Production configurations cannot enable it.
type OAuthClient struct {
	httpClient *http.Client
	now        func() time.Time
	// allowPrivate mirrors OAuthClientOptions.AllowPrivateEndpoints: when
	// set, the public-address check on the token and revocation endpoints
	// is skipped, because the dialer enforces the infrastructure block list.
	allowPrivate bool
}

// OAuthClientOptions customizes an OAuthClient. Tests supply an HTTPClient
// that reaches a local fixture; production leaves it nil.
type OAuthClientOptions struct {
	HTTPClient *http.Client
	Now        func() time.Time
	// AllowPrivateEndpoints dials private and cluster-local provider
	// endpoints too. Fixture use only; see SetAllowPrivateEndpoints.
	AllowPrivateEndpoints bool
}

// NewOAuthClient builds a client whose default transport dials only public
// addresses, follows no redirects, and bounds response size and time.
func NewOAuthClient(opts OAuthClientOptions) *OAuthClient {
	httpClient := opts.HTTPClient
	if httpClient == nil {
		// No proxy: through a CONNECT proxy the public-address dialer would only
		// validate the proxy hop, so a hostname resolving to a private address
		// could bypass the endpoint check. Providers are public; dial them directly.
		dialContext := tokenexchange.PublicEndpointDialContext
		if opts.AllowPrivateEndpoints {
			dialContext = PrivateEndpointDialContext
		}
		transport := &http.Transport{
			Proxy:               nil,
			DialContext:         dialContext,
			ForceAttemptHTTP2:   false,
			TLSHandshakeTimeout: 10 * time.Second,
			MaxIdleConns:        16,
			IdleConnTimeout:     60 * time.Second,
		}
		httpClient = &http.Client{
			Transport: transport,
			Timeout:   defaultOAuthTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &OAuthClient{httpClient: httpClient, now: now, allowPrivate: opts.AllowPrivateEndpoints}
}

// GeneratePKCE returns a fresh RFC 7636 verifier and its S256 challenge.
func GeneratePKCE() (verifier, challenge string, err error) {
	raw := make([]byte, pkceVerifierBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("generate PKCE verifier: %w", err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// GenerateStateNonce returns a random single-use consent identifier.
func GenerateStateNonce() (string, error) {
	raw := make([]byte, oauthStateNonceBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate state nonce: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// AuthorizeURL builds the provider consent URL for one pending consent.
func AuthorizeURL(cfg OAuthProviderConfig, redirectURI, state string, scopes []string, codeChallenge string) (string, error) {
	parsed, err := url.Parse(cfg.AuthorizeURL)
	if err != nil || parsed.Scheme != schemeHTTPS || parsed.Host == "" {
		return "", errors.New("provider authorize URL is not an absolute HTTPS URL")
	}
	if strings.TrimSpace(state) == "" || strings.TrimSpace(redirectURI) == "" {
		return "", errors.New("state and redirect URI are required")
	}
	query := parsed.Query()
	for key, value := range cfg.AdditionalAuthorizeParameters {
		if _, reserved := reservedAuthorizeParameters[strings.ToLower(key)]; reserved {
			return "", fmt.Errorf("authorize parameter %q is reserved", key)
		}
		query.Set(key, value)
	}
	query.Set("response_type", "code")
	query.Set("client_id", cfg.ClientID)
	query.Set("redirect_uri", redirectURI)
	query.Set("state", state)
	if len(scopes) > 0 {
		query.Set("scope", strings.Join(scopes, " "))
	}
	if cfg.PKCE {
		if strings.TrimSpace(codeChallenge) == "" {
			return "", errors.New("PKCE is enabled but no code challenge was supplied")
		}
		query.Set("code_challenge", codeChallenge)
		query.Set("code_challenge_method", pkceMethodS256)
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

// ExchangeCode redeems an authorization code.
func (c *OAuthClient) ExchangeCode(ctx context.Context, cfg OAuthProviderConfig, code, codeVerifier, redirectURI string) (TokenResponse, error) {
	if strings.TrimSpace(code) == "" {
		return TokenResponse{}, errors.New("authorization code is required")
	}
	form := url.Values{}
	form.Set("grant_type", grantAuthorizationCode)
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	if cfg.PKCE {
		if strings.TrimSpace(codeVerifier) == "" {
			return TokenResponse{}, errors.New("PKCE is enabled but no code verifier was supplied")
		}
		form.Set("code_verifier", codeVerifier)
	}
	return c.tokenRequest(ctx, cfg, form)
}

// Refresh exchanges a refresh token for new material. Providers that rotate
// refresh tokens return the new one in the response; callers must persist it.
func (c *OAuthClient) Refresh(ctx context.Context, cfg OAuthProviderConfig, refreshToken string) (TokenResponse, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return TokenResponse{}, errors.New("refresh token is required")
	}
	form := url.Values{}
	form.Set("grant_type", grantRefreshToken)
	form.Set("refresh_token", refreshToken)
	return c.tokenRequest(ctx, cfg, form)
}

// Revoke asks the provider to invalidate a token (RFC 7009). It is best
// effort: a provider without a revocation URL succeeds immediately.
func (c *OAuthClient) Revoke(ctx context.Context, cfg OAuthProviderConfig, token string) error {
	if strings.TrimSpace(cfg.RevocationURL) == "" || strings.TrimSpace(token) == "" {
		return nil
	}
	// The same rule as provider validation, enforced where the token
	// leaves: it goes only to the host that issued it.
	if !SameEndpointHost(cfg.TokenURL, cfg.RevocationURL) {
		return errors.New("provider revocation endpoint is not on the token endpoint's host")
	}
	form := url.Values{}
	form.Set("token", token)
	resp, err := c.post(ctx, cfg, cfg.RevocationURL, form)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxOAuthResponseBytes))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &OAuthError{StatusCode: resp.StatusCode, Code: unknownOAuthErrorCode}
	}
	return nil
}

// SameEndpointHost reports whether two absolute URLs name the same host and
// port, compared case-insensitively.
func SameEndpointHost(a, b string) bool {
	left, err := url.Parse(strings.TrimSpace(a))
	if err != nil || left.Host == "" {
		return false
	}
	right, err := url.Parse(strings.TrimSpace(b))
	if err != nil || right.Host == "" {
		return false
	}
	return strings.EqualFold(left.Hostname(), right.Hostname()) && endpointPort(left) == endpointPort(right)
}

func endpointPort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	if strings.EqualFold(u.Scheme, schemeHTTPS) {
		return "443"
	}
	return "80"
}

func (c *OAuthClient) tokenRequest(ctx context.Context, cfg OAuthProviderConfig, form url.Values) (TokenResponse, error) {
	resp, err := c.post(ctx, cfg, cfg.TokenURL, form)
	if err != nil {
		return TokenResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxOAuthResponseBytes+1))
	if err != nil {
		return TokenResponse{}, fmt.Errorf("read token response: %w", err)
	}
	if len(body) > maxOAuthResponseBytes {
		return TokenResponse{}, errors.New("token response exceeds the size limit")
	}
	var payload struct {
		AccessToken  string          `json:"access_token"`
		RefreshToken string          `json:"refresh_token"`
		TokenType    string          `json:"token_type"`
		ExpiresIn    json.RawMessage `json:"expires_in"`
		Scope        json.RawMessage `json:"scope"`
		Error        string          `json:"error"`
	}
	decodeErr := json.Unmarshal(body, &payload)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || payload.Error != "" {
		code := unknownOAuthErrorCode
		if decodeErr == nil && oauthErrorCodePattern.MatchString(payload.Error) {
			code = payload.Error
		}
		return TokenResponse{}, &OAuthError{StatusCode: resp.StatusCode, Code: code}
	}
	if decodeErr != nil {
		return TokenResponse{}, errors.New("token response is not valid JSON")
	}
	if strings.TrimSpace(payload.AccessToken) == "" {
		return TokenResponse{}, errors.New("token response has no access_token")
	}
	// Tokens are injected verbatim into an Authorization header; material
	// outside the RFC 6750 b64token grammar could never authenticate and
	// must not be parked as a Ready link.
	if !validBearerToken(payload.AccessToken) {
		return TokenResponse{}, errors.New("token response access_token is not a valid bearer token")
	}
	if payload.RefreshToken != "" && !validCredentialToken(payload.RefreshToken) {
		return TokenResponse{}, errors.New("token response refresh_token contains invalid characters")
	}
	// Credentials are injected as a bearer header; any other token type would
	// yield a Ready link that can never authenticate.
	if !strings.EqualFold(strings.TrimSpace(payload.TokenType), "bearer") {
		return TokenResponse{}, errors.New("token response token_type must be Bearer")
	}
	result := TokenResponse{
		AccessToken:  payload.AccessToken,
		RefreshToken: payload.RefreshToken,
		TokenType:    payload.TokenType,
	}
	// An omitted scope field means "as requested". A present field must be
	// a string: JSON null is neither an omission nor a grant and is refused
	// rather than collapsed into the omitted case.
	if len(payload.Scope) > 0 {
		var scope string
		if bytes.Equal(bytes.TrimSpace(payload.Scope), []byte("null")) || json.Unmarshal(payload.Scope, &scope) != nil {
			return TokenResponse{}, errors.New("token response scope must be a string")
		}
		result.ScopePresent = true
		result.Scopes = splitScopes(scope)
		// The granted list is copied into Connection status, one API object:
		// a provider cannot be allowed to make that object unwritable.
		if len(result.Scopes) > maxGrantedScopes {
			return TokenResponse{}, errors.New("token response grants more scopes than Orka records")
		}
		for _, granted := range result.Scopes {
			if len(granted) > maxGrantedScopeBytes {
				return TokenResponse{}, errors.New("token response grants a scope longer than Orka records")
			}
		}
	}
	seconds, present, err := parseExpiresIn(payload.ExpiresIn)
	if err != nil {
		return TokenResponse{}, err
	}
	if present {
		if seconds > maxExpiresInSeconds {
			return TokenResponse{}, errors.New("token response expires_in is out of range")
		}
		result.ExpiresAt = c.now().Add(time.Duration(seconds) * time.Second).UTC()
	}
	return result, nil
}

// maxExpiresInSeconds bounds expires_in (ten years) so the seconds-to-
// duration conversion cannot overflow into a past expiry.
const maxExpiresInSeconds int64 = 10 * 365 * 24 * 60 * 60

func (c *OAuthClient) post(ctx context.Context, cfg OAuthProviderConfig, endpoint string, form url.Values) (*http.Response, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != schemeHTTPS || parsed.Host == "" || parsed.User != nil {
		return nil, errors.New("provider endpoint is not an absolute HTTPS URL")
	}
	// The same host rules as provider validation: an endpoint stored before
	// a rule existed, or that bypassed admission, is refused here too rather
	// than left to DNS resolution alone. The fixture allowance relaxes only
	// the general private and cluster-local names; infrastructure hosts stay
	// denied.
	host := parsed.Hostname()
	if host == "" || strings.HasSuffix(host, ".") || strings.Contains(host, "%") || !asciiHost(host) ||
		(net.ParseIP(host) == nil && nonCanonicalNumericHost(host)) || InfrastructureHostDenied(host) ||
		(!c.allowPrivate && hostDenied(strings.ToLower(host))) {
		return nil, errors.New("provider endpoint host is not allowed")
	}
	if ip := net.ParseIP(parsed.Hostname()); ip != nil && !tokenexchange.IsPublicAddress(ip) && !c.allowPrivate {
		return nil, errors.New("provider endpoint host is not a public address")
	}
	switch cfg.ClientAuthentication {
	case corev1alpha1.ConnectorClientAuthSecretPost:
		form.Set("client_id", cfg.ClientID)
		form.Set("client_secret", cfg.ClientSecret)
	case "", corev1alpha1.ConnectorClientAuthSecretBasic:
	default:
		return nil, fmt.Errorf("unsupported client authentication %q", cfg.ClientAuthentication)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build provider request: %w", err)
	}
	req.Header.Set("Content-Type", headerContentTypeFormURL)
	req.Header.Set("Accept", "application/json")
	if cfg.ClientAuthentication == "" || cfg.ClientAuthentication == corev1alpha1.ConnectorClientAuthSecretBasic {
		req.SetBasicAuth(url.QueryEscape(cfg.ClientID), url.QueryEscape(cfg.ClientSecret))
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Transport errors can embed the URL but never credentials; still
		// keep the message generic so query material cannot leak.
		return nil, errors.New("provider request failed: " + sanitizeTransportError(err))
	}
	return resp, nil
}

func sanitizeTransportError(err error) string {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	return "transport error"
}

// validBearerToken applies the RFC 6750 b64token grammar: one or more of
// ALPHA / DIGIT / "-" / "." / "_" / "~" / "+" / "/" followed by optional "=".
func validBearerToken(token string) bool {
	if token == "" || len(token) > maxCredentialTokenBytes {
		return false
	}
	trimmed := strings.TrimRight(token, "=")
	if trimmed == "" {
		return false
	}
	for i := 0; i < len(trimmed); i++ {
		b := trimmed[i]
		switch {
		case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		case b == '-', b == '.', b == '_', b == '~', b == '+', b == '/':
		default:
			return false
		}
	}
	return true
}

// validCredentialToken refuses whitespace and control bytes in material that
// is sent verbatim in a form body.
func validCredentialToken(token string) bool {
	if len(token) > maxCredentialTokenBytes {
		return false
	}
	for i := 0; i < len(token); i++ {
		if b := token[i]; b <= 0x20 || b == 0x7F {
			return false
		}
	}
	return true
}

// maxGrantedScopes and maxGrantedScopeBytes bound the scope list a token
// response may carry; they are far above any provider's real grant.
const (
	maxGrantedScopes     = 256
	maxGrantedScopeBytes = 256
)

// maxCredentialTokenBytes bounds a single token; real bearer tokens are far
// smaller, and a larger value cannot be a credential.
const maxCredentialTokenBytes = 16 << 10

func splitScopes(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	// RFC 6749 scope lists are space-delimited, but GitHub reports the
	// granted scopes comma-delimited in its JSON token response
	// ("repo,gist"). Both delimiters are accepted. This cannot fabricate a
	// grant: Orka's own scope grammar refuses commas inside a configured
	// scope name (validScopeToken), so no required scope can be produced by
	// splitting a comma-bearing token the provider issued as one.
	fields := strings.FieldsFunc(raw, func(r rune) bool { return r == ' ' || r == ',' })
	result := make([]string, 0, len(fields))
	for _, field := range fields {
		if field = strings.TrimSpace(field); field != "" {
			result = append(result, field)
		}
	}
	return result
}

// parseExpiresIn accepts the integer RFC form and the string form some
// providers emit.
// parseExpiresIn reads expires_in as a positive integer number of seconds,
// given as a JSON number or a decimal string. An absent or null value is not
// present; a present value that is zero, negative, fractional, or otherwise
// malformed is an error, never silently a non-expiring credential.
func parseExpiresIn(raw json.RawMessage) (int64, bool, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		text = string(raw)
	}
	seconds, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
	if err != nil || seconds <= 0 {
		return 0, true, errors.New("token response expires_in must be a positive integer")
	}
	return seconds, true, nil
}
