/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
			"audience from the Orka server (/api/v1/auth/config), requests a device code, saves the access token " +
			"(and refresh token when issued) in your Orka config, and automatically refreshes it before expiry.",
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

func resolveOIDCLoginTargets(ctx context.Context, server, issuer, clientID string) (string, string, error) {
	issuer = strings.TrimSpace(issuer)
	clientID = strings.TrimSpace(clientID)
	if issuer == "" || clientID == "" {
		var options authConfigView
		if err := plainJSON(ctx, http.MethodGet, server+"/api/v1/auth/config", "", nil, &options); err != nil {
			return "", "", fmt.Errorf("read auth config from %s: %w", server, err)
		}
		if !options.OIDC {
			return "", "", fmt.Errorf("%s does not have OIDC authentication enabled", server)
		}
		if options.OIDCConfig != nil {
			if issuer == "" {
				issuer = strings.TrimSpace(options.OIDCConfig.Issuer)
			}
			if clientID == "" {
				clientID = strings.TrimSpace(options.OIDCConfig.Audience)
			}
		}
	}
	if issuer == "" {
		return "", "", errors.New("OIDC issuer is required (pass --issuer or configure --oidc-issuer on the server)")
	}
	if clientID == "" {
		return "", "", errors.New("OIDC client ID is required (pass --client-id or configure --oidc-audience on the server)")
	}
	if err := oidcEndpointAllowed(issuer); err != nil {
		return "", "", err
	}
	return issuer, clientID, nil
}

func runOIDCLogin(cmd *cobra.Command, opts oidcLoginOptions) error {
	server, err := loginServer(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout)
	defer cancel()

	issuer, clientID, err := resolveOIDCLoginTargets(ctx, server, opts.issuer, opts.clientID)
	if err != nil {
		return err
	}

	discovery, err := fetchOIDCDiscovery(ctx, issuer)
	if err != nil {
		return err
	}
	if discovery.DeviceAuthorizationEndpoint == "" {
		return fmt.Errorf("OIDC provider %s does not advertise device_authorization_endpoint", issuer)
	}
	if discovery.TokenEndpoint == "" {
		return fmt.Errorf("OIDC provider %s does not advertise token_endpoint", issuer)
	}
	if err := oidcEndpointAllowed(discovery.DeviceAuthorizationEndpoint); err != nil {
		return err
	}
	if err := oidcEndpointAllowed(discovery.TokenEndpoint); err != nil {
		return err
	}

	scope := strings.TrimSpace(opts.scope)
	if scope == "" {
		scope = defaultOIDCLoginScope(issuer, clientID)
	}

	out := cmd.OutOrStdout()
	if saved := loadConfig(); saved.Token != "" && saved.TokenKind != tokenKindOIDC {
		fmt.Fprintln(cmd.ErrOrStderr(), "Signing in replaces the token saved in your Orka config. "+ //nolint:errcheck
			"Press Ctrl-C now to keep it; after signing in, pass another token with --token.")
	}

	deviceAuth, err := startOIDCDeviceFlow(ctx, discovery.DeviceAuthorizationEndpoint, clientID, scope)
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

	tokens, err := pollOIDCDeviceFlow(ctx, discovery.TokenEndpoint, clientID, deviceAuth)
	if err != nil {
		return err
	}
	token := tokens.AccessToken
	if token == "" {
		token = tokens.IDToken
	}
	if token == "" {
		return errors.New("OIDC provider returned neither access_token nor id_token")
	}

	var expiresAt *time.Time
	if tokens.ExpiresIn > 0 {
		exp := time.Now().UTC().Add(time.Duration(tokens.ExpiresIn) * time.Second)
		expiresAt = &exp
	}

	loginID, err := newOIDCLoginID()
	if err != nil {
		return fmt.Errorf("generate login ID: %w", err)
	}
	if err := updateConfig(func(cfg *orkaConfig) error {
		cfg.Server = server
		cfg.Token = token
		cfg.TokenKind = tokenKindOIDC
		cfg.TokenExpiresAt = expiresAt
		cfg.SessionServer = server
		cfg.SessionLogin = ""
		cfg.SessionSubject = loginID
		cfg.OIDCIssuer = issuer
		cfg.OIDCClientID = clientID
		cfg.OIDCTokenEndpoint = discovery.TokenEndpoint
		cfg.OIDCRefreshToken = tokens.RefreshToken
		cfg.OIDCScope = scope
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

func defaultOIDCLoginScope(issuer, clientID string) string {
	// Microsoft Entra v2 requires a resource scope (such as <app-id>/.default or
	// api://<app-id>/...) to issue an access token with the app's audience, plus
	// offline_access for refresh tokens.
	if strings.Contains(strings.ToLower(issuer), "login.microsoftonline.com") ||
		strings.Contains(strings.ToLower(issuer), "sts.windows.net") {
		return fmt.Sprintf("openid profile email offline_access %s/.default", clientID)
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
	if err != nil || status >= 300 || refreshed.Error != "" || (refreshed.AccessToken == "" && refreshed.IDToken == "") {
		fmt.Fprintln(cmd.ErrOrStderr(), "Could not refresh your OIDC token; run 'orka auth login'.") //nolint:errcheck
		return cfg.Token
	}

	newToken := refreshed.AccessToken
	if newToken == "" {
		newToken = refreshed.IDToken
	}
	cfg.Token = newToken
	if refreshed.RefreshToken != "" {
		cfg.OIDCRefreshToken = refreshed.RefreshToken
	}
	if refreshed.ExpiresIn > 0 {
		exp := time.Now().UTC().Add(time.Duration(refreshed.ExpiresIn) * time.Second)
		cfg.TokenExpiresAt = &exp
	}
	if err := saveConfig(cfg); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Could not save the refreshed OIDC token: %v\n", err) //nolint:errcheck
	}
	return newToken
}

func oidcTokenNeedsRefresh(cfg orkaConfig) bool {
	if cfg.TokenExpiresAt == nil {
		return false
	}
	return time.Until(*cfg.TokenExpiresAt) <= oidcRefreshWindow
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
