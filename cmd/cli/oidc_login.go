/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const (
	// tokenKindOIDC marks a configured token as an OIDC access token saved by
	// `orka auth login`, which can refresh itself via its stored refresh token
	// and is sent only to SessionServer.
	tokenKindOIDC = "oidc"

	// oidcRefreshWindow is how long before expiry an OIDC access token refreshes.
	oidcRefreshWindow = 5 * time.Minute

	oidcFormClientID = "client_id"
)

// oidcPollFloor is the shortest wait between device-code polls; tests lower it.
var oidcPollFloor time.Duration

type oidcDiscoveryDoc struct {
	Issuer                      string `json:"issuer"`
	DeviceAuthorizationEndpoint string `json:"device_authorization_endpoint"`
	TokenEndpoint               string `json:"token_endpoint"`
}

type oidcDeviceAuthResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	Message                 string `json:"message"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

type oidcTokenResponse struct {
	AccessToken      string `json:"access_token"`
	IDToken          string `json:"id_token"`
	RefreshToken     string `json:"refresh_token"`
	TokenType        string `json:"token_type"`
	Scope            string `json:"scope"`
	ExpiresIn        int    `json:"expires_in"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

type oidcLoginOptions struct {
	issuer   string
	clientID string
	scope    string
	noOpen   bool
	timeout  time.Duration
}

func newAuthLoginCmd() *cobra.Command {
	var opts oidcLoginOptions

	cmd := &cobra.Command{
		Use:   cliLoginUse,
		Short: "Sign in to Orka with OIDC (device-code flow)",
		Long: "Sign in to Orka using OpenID Connect device-code flow. By default, discovers the OIDC issuer and " +
			"audience from the Orka server (/api/v1/auth/config), requests a device code, saves an Orka-compatible " +
			"JWT (and refresh token when issued) in your Orka config, and refreshes it on demand when a later command " +
			"runs near or after expiry (otherwise run 'orka auth login' again).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runOIDCLogin(cmd, opts)
		},
	}

	cmd.Flags().StringVar(&opts.issuer, "issuer", "", "OIDC issuer URL (defaults to the server's advertised issuer)")
	cmd.Flags().StringVar(&opts.clientID, "client-id", "", "OIDC public client ID (defaults to the server's advertised audience)")
	cmd.Flags().StringVar(&opts.scope, "scope", "", "Space-separated OAuth scopes to request")
	cmd.Flags().BoolVar(&opts.noOpen, "no-open", false, "Print the verification URL without opening a browser")
	cmd.Flags().DurationVar(&opts.timeout, "timeout", 15*time.Minute, "Maximum time to wait for device-code approval")
	return cmd
}

type oidcLoginTargets struct {
	issuer   string
	audience string
	clientID string
}

func resolveOIDCLoginTargets(ctx context.Context, server, issuer, clientID string) (oidcLoginTargets, error) {
	issuer = strings.TrimSpace(issuer)
	clientID = strings.TrimSpace(clientID)
	audience := ""

	var options authConfigView
	if err := plainJSON(ctx, http.MethodGet, server+"/api/v1/auth/config", "", nil, &options); err != nil {
		if issuer == "" || clientID == "" {
			return oidcLoginTargets{}, fmt.Errorf("read auth config from %s: %w", server, err)
		}
	} else {
		if options.OIDC != nil && !*options.OIDC {
			return oidcLoginTargets{}, fmt.Errorf("%s does not have OIDC authentication enabled", server)
		}
		if options.OIDCConfig != nil {
			audience = strings.TrimSpace(options.OIDCConfig.Audience)
			if issuer == "" {
				issuer = strings.TrimSpace(options.OIDCConfig.Issuer)
			}
			if clientID == "" {
				clientID = audience
			}
		}
	}
	if issuer == "" {
		return oidcLoginTargets{}, errors.New("OIDC issuer is required (pass --issuer or configure --oidc-issuer on the server)")
	}
	if clientID == "" {
		return oidcLoginTargets{}, errors.New("OIDC client ID is required (pass --client-id or configure --oidc-audience on the server)")
	}
	if audience == "" {
		audience = clientID
	}
	if err := oidcEndpointAllowed(issuer); err != nil {
		return oidcLoginTargets{}, err
	}
	return oidcLoginTargets{issuer: issuer, audience: audience, clientID: clientID}, nil
}

func runOIDCLogin(cmd *cobra.Command, opts oidcLoginOptions) error {
	server, err := loginServer(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout)
	defer cancel()

	targets, err := resolveOIDCLoginTargets(ctx, server, opts.issuer, opts.clientID)
	if err != nil {
		return err
	}

	discovery, err := fetchOIDCDiscovery(ctx, targets.issuer)
	if err != nil {
		return err
	}
	if discovery.DeviceAuthorizationEndpoint == "" {
		return fmt.Errorf("OIDC provider %s does not advertise device_authorization_endpoint", targets.issuer)
	}
	if discovery.TokenEndpoint == "" {
		return fmt.Errorf("OIDC provider %s does not advertise token_endpoint", targets.issuer)
	}
	if err := oidcEndpointAllowed(discovery.DeviceAuthorizationEndpoint); err != nil {
		return err
	}
	if err := oidcEndpointAllowed(discovery.TokenEndpoint); err != nil {
		return err
	}

	scope := strings.TrimSpace(opts.scope)
	if scope == "" {
		scope = defaultOIDCLoginScope(targets.issuer, targets.audience)
	}

	out := cmd.OutOrStdout()
	if saved := loadConfig(); saved.Token != "" && saved.TokenKind != tokenKindOIDC {
		fmt.Fprintln(cmd.ErrOrStderr(), "Signing in replaces the token saved in your Orka config. "+ //nolint:errcheck
			"Press Ctrl-C now to keep it; after signing in, pass another token with --token.")
	}

	deviceAuth, err := startOIDCDeviceFlow(ctx, discovery.DeviceAuthorizationEndpoint, targets.clientID, scope)
	if err != nil {
		return err
	}

	verifyURL := deviceAuth.VerificationURIComplete
	if verifyURL == "" {
		verifyURL = deviceAuth.VerificationURI
	}
	if deviceAuth.Message != "" {
		fmt.Fprintln(out, deviceAuth.Message) //nolint:errcheck
	} else {
		fmt.Fprintf(out, "Open %s and enter the code %s\n", deviceAuth.VerificationURI, deviceAuth.UserCode) //nolint:errcheck
	}
	if !opts.noOpen && verifyURL != "" {
		if err := openBrowser(verifyURL); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "Could not open browser: %v\nOpen the URL above manually.\n", err) //nolint:errcheck
		}
	}

	tokens, err := pollOIDCDeviceFlow(ctx, discovery.TokenEndpoint, targets.clientID, deviceAuth)
	if err != nil {
		return err
	}
	token, expiresAt, err := selectOIDCBearerToken(tokens, targets.issuer, targets.audience)
	if err != nil {
		return err
	}

	loginID, err := newOIDCLoginID()
	if err != nil {
		return fmt.Errorf("generate login ID: %w", err)
	}
	if err := updateConfig(func(cfg *orkaConfig) error {
		if cfg.TokenKind == tokenKindGitHubSession {
			// Clear the GitHub session's saved namespace so OIDC namespace
			// isolation is not tripped by a stale session namespace.
			cfg.Namespace = ""
		}
		cfg.Server = server
		cfg.Token = token
		cfg.TokenKind = tokenKindOIDC
		cfg.TokenExpiresAt = expiresAt
		cfg.SessionServer = server
		cfg.SessionLogin = ""
		cfg.SessionSubject = loginID
		cfg.OIDCIssuer = targets.issuer
		cfg.OIDCAudience = targets.audience
		cfg.OIDCClientID = targets.clientID
		cfg.OIDCTokenEndpoint = discovery.TokenEndpoint
		cfg.OIDCRefreshToken = tokens.RefreshToken
		// Record only what the provider actually granted; omitting scope on
		// refresh reuses the granted scope per RFC 6749 §6.
		cfg.OIDCScope = strings.TrimSpace(tokens.Scope)
		return nil
	}); err != nil {
		return fmt.Errorf("save OIDC token: %w", err)
	}

	fmt.Fprintf(out, "Signed in to %s with OIDC.\n", server) //nolint:errcheck
	if tokens.RefreshToken != "" {
		fmt.Fprintln(out, "Refresh token saved; Orka CLI will refresh access tokens automatically.") //nolint:errcheck
	}
	return nil
}

// selectOIDCBearerToken picks an Orka-compatible compact JWT (preferring
// access_token, then id_token) matching expectedIssuer and expectedAudience,
// and derives its expiry from expires_in or the JWT's exp claim.
func selectOIDCBearerToken(tokens oidcTokenResponse, expectedIssuer, expectedAudience string) (string, *time.Time, error) {
	candidates := []string{strings.TrimSpace(tokens.AccessToken), strings.TrimSpace(tokens.IDToken)}
	var nonEmpty []string
	for _, cand := range candidates {
		if cand != "" {
			nonEmpty = append(nonEmpty, cand)
		}
	}
	if len(nonEmpty) == 0 {
		return "", nil, errors.New("OIDC provider returned neither access_token nor id_token")
	}

	now := time.Now().UTC()
	var fallbackJWT string
	var fallbackExp *time.Time
	for i, cand := range nonEmpty {
		claims, ok := parseUnverifiedJWTClaims(cand)
		if !ok {
			continue
		}
		// expires_in applies only to the access_token (RFC 6749 §5.1); when
		// selecting an id_token or when the JWT carries an earlier exp claim,
		// prefer the JWT's exp so refresh runs before Orka rejects the token.
		expiresIn := 0
		if i == 0 && strings.TrimSpace(tokens.AccessToken) != "" {
			expiresIn = tokens.ExpiresIn
		}
		exp := resolveTokenExpiry(expiresIn, claims.Exp)
		if exp != nil && !exp.After(now) {
			continue
		}
		if fallbackJWT == "" {
			fallbackJWT, fallbackExp = cand, exp
		}
		if jwtMatchesTarget(claims, expectedIssuer, expectedAudience) {
			return cand, exp, nil
		}
	}
	if fallbackJWT != "" && expectedIssuer == "" && expectedAudience == "" {
		return fallbackJWT, fallbackExp, nil
	}
	return "", nil, fmt.Errorf("OIDC provider returned no compact JWT matching issuer %q and audience %q", expectedIssuer, expectedAudience)
}

type unverifiedJWTClaims struct {
	Issuer   string
	Subject  string
	Audience []string
	Exp      int64
}

func parseUnverifiedJWTClaims(token string) (unverifiedJWTClaims, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return unverifiedJWTClaims{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return unverifiedJWTClaims{}, false
	}
	var raw struct {
		Iss string   `json:"iss"`
		Sub string   `json:"sub"`
		Aud any      `json:"aud"`
		Exp *float64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &raw); err != nil {
		return unverifiedJWTClaims{}, false
	}
	var aud []string
	switch v := raw.Aud.(type) {
	case string:
		if v != "" {
			aud = []string{v}
		}
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				aud = append(aud, s)
			}
		}
	}
	var exp int64
	if raw.Exp != nil && !math.IsNaN(*raw.Exp) && !math.IsInf(*raw.Exp, 0) && *raw.Exp > 0 {
		exp = int64(math.Floor(*raw.Exp))
	}
	return unverifiedJWTClaims{Issuer: raw.Iss, Subject: raw.Sub, Audience: aud, Exp: exp}, true
}

func jwtMatchesTarget(claims unverifiedJWTClaims, expectedIssuer, expectedAudience string) bool {
	if strings.TrimSpace(claims.Subject) == "" || claims.Exp <= 0 {
		return false
	}
	if expectedIssuer != "" && claims.Issuer != expectedIssuer {
		return false
	}
	if expectedAudience != "" && !slices.Contains(claims.Audience, expectedAudience) {
		return false
	}
	return true
}

func resolveTokenExpiry(expiresIn int, jwtExp int64) *time.Time {
	var fromExpiresIn *time.Time
	if expiresIn > 0 {
		exp := time.Now().UTC().Add(time.Duration(expiresIn) * time.Second)
		fromExpiresIn = &exp
	}
	var fromJWT *time.Time
	if jwtExp > 0 {
		exp := time.Unix(jwtExp, 0).UTC()
		fromJWT = &exp
	}
	if fromJWT != nil && fromExpiresIn != nil {
		if fromJWT.Before(*fromExpiresIn) {
			return fromJWT
		}
		return fromExpiresIn
	}
	if fromJWT != nil {
		return fromJWT
	}
	return fromExpiresIn
}

func defaultOIDCLoginScope(issuer, audience string) string {
	// Microsoft Entra v2 requires a resource scope (such as <app-id>/.default or
	// api://<app-id>/...) to issue an access token with the app's audience, plus
	// offline_access for refresh tokens.
	if strings.Contains(strings.ToLower(issuer), "login.microsoftonline.com") ||
		strings.Contains(strings.ToLower(issuer), "sts.windows.net") {
		return fmt.Sprintf("openid profile email offline_access %s/.default", audience)
	}
	return "openid profile email offline_access"
}

func oidcEndpointAllowed(raw string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return fmt.Errorf("%q is not a valid OIDC URL", raw)
	}
	if parsed.Scheme == httpsScheme {
		return nil
	}
	if parsed.Scheme == plainHTTPScheme && isLoopbackHostname(parsed.Hostname()) {
		return nil
	}
	return fmt.Errorf("OIDC endpoint %s must use https (or http only for localhost)", raw)
}

func fetchOIDCDiscovery(ctx context.Context, issuer string) (oidcDiscoveryDoc, error) {
	discoveryURL := strings.TrimRight(issuer, "/") + "/.well-known/openid-configuration"
	var doc oidcDiscoveryDoc
	if err := plainJSON(ctx, http.MethodGet, discoveryURL, "", nil, &doc); err != nil {
		return oidcDiscoveryDoc{}, fmt.Errorf("fetch OIDC discovery document: %w", err)
	}
	if doc.Issuer == "" || doc.Issuer != issuer {
		return oidcDiscoveryDoc{}, fmt.Errorf("OIDC discovery issuer mismatch: got %q, want %q", doc.Issuer, issuer)
	}
	return doc, nil
}

func postOIDCForm(ctx context.Context, endpoint string, form url.Values, into any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := noRedirectClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, fmt.Errorf("read OIDC response: %w", err)
	}
	if into != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, into); err != nil {
			return resp.StatusCode, fmt.Errorf("decode OIDC response (HTTP %d): %w", resp.StatusCode, err)
		}
	}
	return resp.StatusCode, nil
}

func startOIDCDeviceFlow(ctx context.Context, endpoint, clientID, scope string) (oidcDeviceAuthResponse, error) {
	form := url.Values{
		oidcFormClientID: {clientID},
		"scope":          {scope},
	}
	var out oidcDeviceAuthResponse
	status, err := postOIDCForm(ctx, endpoint, form, &out)
	if err != nil {
		return oidcDeviceAuthResponse{}, fmt.Errorf("start OIDC device flow: %w", err)
	}
	if status >= 300 || out.DeviceCode == "" || out.UserCode == "" {
		return oidcDeviceAuthResponse{}, fmt.Errorf("start OIDC device flow: provider returned HTTP %d without a device code", status)
	}
	return out, nil
}

func pollOIDCDeviceFlow(ctx context.Context, tokenEndpoint, clientID string, device oidcDeviceAuthResponse) (oidcTokenResponse, error) {
	interval := time.Duration(device.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if oidcPollFloor > 0 {
		interval = oidcPollFloor
	}
	if device.ExpiresIn > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(device.ExpiresIn)*time.Second)
		defer cancel()
	}

	form := url.Values{
		"grant_type":     {"urn:ietf:params:oauth:grant-type:device_code"},
		oidcFormClientID: {clientID},
		"device_code":    {device.DeviceCode},
	}

	for {
		select {
		case <-ctx.Done():
			return oidcTokenResponse{}, fmt.Errorf("OIDC device login timed out: %w", ctx.Err())
		case <-time.After(interval):
		}

		var resp oidcTokenResponse
		status, err := postOIDCForm(ctx, tokenEndpoint, form, &resp)
		if err != nil {
			var netErr net.Error
			if ctx.Err() == nil && errors.As(err, &netErr) && netErr.Timeout() {
				interval += 5 * time.Second
				continue
			}
			return oidcTokenResponse{}, fmt.Errorf("poll OIDC token endpoint: %w", err)
		}
		if status >= 200 && status < 300 && resp.Error == "" {
			return resp, nil
		}
		switch resp.Error {
		case "authorization_pending":
			continue
		case "slow_down":
			interval += 5 * time.Second
			continue
		case "expired_token":
			return oidcTokenResponse{}, errors.New("device code expired; run 'orka auth login' again")
		case "access_denied":
			return oidcTokenResponse{}, errors.New("sign-in was denied")
		default:
			if resp.ErrorDescription != "" {
				return oidcTokenResponse{}, fmt.Errorf("OIDC login failed (%s): %s", resp.Error, resp.ErrorDescription)
			}
			return oidcTokenResponse{}, fmt.Errorf("OIDC login failed (HTTP %d: %s)", status, resp.Error)
		}
	}
}

// configuredOIDCToken returns the saved OIDC token for server, refreshing it
// with the saved refresh token when it is near or past expiry. Like GitHub
// sessions, it is sent only to the server it was signed in against.
func configuredOIDCToken(cmd *cobra.Command, server string, cfg orkaConfig) string {
	if server == "" {
		server = cfg.Server
	}
	if cfg.Token == "" || strings.TrimRight(server, "/") != cfg.SessionServer || sessionServerAllowed(cfg.SessionServer) != nil {
		return ""
	}
	if !oidcTokenNeedsRefresh(cfg) {
		return cfg.Token
	}
	if cfg.OIDCRefreshToken == "" || cfg.OIDCTokenEndpoint == "" || cfg.OIDCClientID == "" {
		if cfg.TokenExpiresAt != nil && time.Until(*cfg.TokenExpiresAt) <= 0 {
			fmt.Fprintln(cmd.ErrOrStderr(), "Your OIDC token expired; run 'orka auth login'.") //nolint:errcheck
		}
		return cfg.Token
	}
	if err := oidcEndpointAllowed(cfg.OIDCTokenEndpoint); err != nil {
		return cfg.Token
	}

	ctx, cancel := context.WithTimeout(context.Background(), sessionRenewTimeout)
	defer cancel()
	unlock, err := lockConfig(ctx)
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Could not refresh your OIDC token (%v).\n", err) //nolint:errcheck
		return cfg.Token
	}
	defer unlock()

	saved := loadConfig()
	if !holdsOIDCSession(saved, cfg) {
		// Another login replaced or cleared this OIDC session while waiting for
		// the lock; never adopt another account's credentials for this command.
		return cfg.Token
	}
	cfg = saved
	if !oidcTokenNeedsRefresh(cfg) {
		return cfg.Token
	}

	form := url.Values{
		"grant_type":     {"refresh_token"},
		oidcFormClientID: {cfg.OIDCClientID},
		"refresh_token":  {cfg.OIDCRefreshToken},
	}
	if cfg.OIDCScope != "" {
		form.Set("scope", cfg.OIDCScope)
	}
	var refreshed oidcTokenResponse
	status, err := postOIDCForm(ctx, cfg.OIDCTokenEndpoint, form, &refreshed)
	if err != nil || status >= 300 || refreshed.Error != "" {
		if refreshed.Error == "invalid_grant" {
			cfg.OIDCRefreshToken = ""
			_ = saveConfig(cfg)
		}
		fmt.Fprintln(cmd.ErrOrStderr(), "Could not refresh your OIDC token; run 'orka auth login'.") //nolint:errcheck
		return cfg.Token
	}

	audience := cfg.OIDCAudience
	if audience == "" {
		audience = cfg.OIDCClientID
	}
	newToken, expiresAt, err := selectOIDCBearerToken(refreshed, cfg.OIDCIssuer, audience)
	if err != nil {
		if refreshed.RefreshToken != "" && refreshed.RefreshToken != cfg.OIDCRefreshToken {
			cfg.OIDCRefreshToken = refreshed.RefreshToken
			_ = saveConfig(cfg)
		}
		fmt.Fprintln(cmd.ErrOrStderr(), "Could not refresh your OIDC token; run 'orka auth login'.") //nolint:errcheck
		return cfg.Token
	}

	cfg.Token = newToken
	if refreshed.RefreshToken != "" {
		cfg.OIDCRefreshToken = refreshed.RefreshToken
	}
	if strings.TrimSpace(refreshed.Scope) != "" {
		cfg.OIDCScope = strings.TrimSpace(refreshed.Scope)
	}
	if expiresAt != nil {
		cfg.TokenExpiresAt = expiresAt
	}
	if err := saveConfig(cfg); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Could not save the refreshed OIDC token: %v\n", err) //nolint:errcheck
	}
	return newToken
}

func oidcTokenNeedsRefresh(cfg orkaConfig) bool {
	if cfg.TokenExpiresAt != nil {
		return time.Until(*cfg.TokenExpiresAt) <= oidcRefreshWindow
	}
	if claims, ok := parseUnverifiedJWTClaims(cfg.Token); ok && claims.Exp > 0 {
		return time.Until(time.Unix(claims.Exp, 0).UTC()) <= oidcRefreshWindow
	}
	return false
}

// holdsOIDCSession reports whether saved holds the same OIDC login as read
// (the same per-login identifier on the same server and client), analogous to
// holdsSession for GitHub sessions.
func holdsOIDCSession(saved, read orkaConfig) bool {
	return saved.TokenKind == tokenKindOIDC && saved.Token != "" &&
		saved.SessionServer == read.SessionServer &&
		saved.OIDCClientID == read.OIDCClientID &&
		read.SessionSubject != "" && saved.SessionSubject == read.SessionSubject
}

func newOIDCLoginID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "oidc-" + hex.EncodeToString(raw[:]), nil
}
