package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/orka-agents/orka/internal/cli/client"
	"github.com/orka-agents/orka/internal/githubauth"
)

const (
	sessionTokenPrefix     = "orka_s_"
	tokenKindGitHubSession = "github-session"
	sessionRenewWindow     = 2 * time.Hour
	sessionRenewTimeout    = 30 * time.Second
	plainHTTPScheme        = "http"
	httpsScheme            = "https"
	cliLoginUse            = "login"
)

var noRedirectClient = &http.Client{
	Timeout:       time.Minute,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

var sessionAPIClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

var (
	githubWebURL    = "https://github.com"
	githubPollFloor time.Duration
)

type authConfigView struct {
	OIDC       *bool `json:"oidc"`
	OIDCConfig *struct {
		Issuer   string   `json:"issuer"`
		Audience string   `json:"audience"`
		Scopes   []string `json:"scopes"`
	} `json:"oidcConfig"`
	GitHub *struct {
		ClientID  string `json:"clientID"`
		AgentHost bool   `json:"agentHost"`
	} `json:"github"`
}

type sessionView struct {
	Token      string    `json:"token"`
	ExpiresAt  time.Time `json:"expiresAt"`
	Login      string    `json:"login"`
	Subject    string    `json:"subject"`
	Namespace  string    `json:"namespace"`
	Connection string    `json:"connection"`
	AgentHost  bool      `json:"agentHost"`
}

func newLoginCmd() *cobra.Command {
	var noHost, noOpen bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   cliLoginUse,
		Short: "Sign in to Orka with GitHub",
		Long: "Sign in to Orka with your GitHub account. You approve a code on github.com, Orka checks you are in an " +
			"allowed org or team, links your GitHub account, and starts your Copilot environment unless you pass " +
			"--no-host. The session replaces the token and namespace saved in your Orka config, and renews itself.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			server, err := loginServer(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			out := cmd.OutOrStdout()
			var options authConfigView
			if err := plainJSON(ctx, http.MethodGet, server+"/api/v1/auth/config", "", nil, &options); err != nil {
				return fmt.Errorf("read how to sign in to %s: %w", server, err)
			}
			if options.GitHub == nil || options.GitHub.ClientID == "" {
				return fmt.Errorf("%s does not offer GitHub sign-in", server)
			}
			if options.GitHub.AgentHost && !noHost {
				fmt.Fprintln(out, "Signing in starts your Copilot environment on Orka. Code running there can read your GitHub token.") //nolint:errcheck
			}
			if saved := loadConfig(); saved.Token != "" && saved.TokenKind != tokenKindGitHubSession {
				fmt.Fprintln(cmd.ErrOrStderr(), "Signing in replaces the token and namespace saved in your Orka config. "+ //nolint:errcheck
					"Press Ctrl-C now to keep them; after signing in, pass them with --token and --namespace.")
			}
			github := githubauth.Client{WebURL: githubWebURL, MinPollInterval: githubPollFloor}
			code, err := github.StartDeviceFlow(ctx, options.GitHub.ClientID)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "Open %s and enter the code %s\n", code.VerificationURI, code.UserCode) //nolint:errcheck
			if !noOpen {
				if err := openBrowser(code.VerificationURI); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "Could not open a browser: %v\n", err) //nolint:errcheck
				}
			}
			tokens, err := github.PollDeviceFlow(ctx, options.GitHub.ClientID, code)
			if err != nil {
				return err
			}
			body, err := json.Marshal(map[string]any{
				"accessToken": tokens.AccessToken, "refreshToken": tokens.RefreshToken, "expiresIn": tokens.ExpiresIn,
				"noHost": noHost || !options.GitHub.AgentHost,
			})
			if err != nil {
				return err
			}
			var session sessionView
			if err := plainJSON(ctx, http.MethodPost, server+"/api/v1/auth/github", "", body, &session); err != nil {
				return fmt.Errorf("sign in to %s: %w", server, err)
			}
			if session.Token == "" {
				return fmt.Errorf("sign in to %s: the server answered without a session", server)
			}
			if err := updateConfig(func(cfg *orkaConfig) error {
				cfg.Server, cfg.Token, cfg.TokenKind, cfg.Namespace = server, session.Token, tokenKindGitHubSession, session.Namespace
				expires := session.ExpiresAt
				cfg.SessionServer, cfg.TokenExpiresAt, cfg.SessionLogin, cfg.SessionSubject = server, &expires, session.Login, session.Subject
				cfg.OIDCIssuer, cfg.OIDCAudience, cfg.OIDCClientID, cfg.OIDCTokenEndpoint, cfg.OIDCRefreshToken, cfg.OIDCScope = "", "", "", "", "", ""
				return nil
			}); err != nil {
				return fmt.Errorf("save the session: %w", err)
			}
			fmt.Fprintf(out, "Signed in to %s as %s.\n", server, session.Login) //nolint:errcheck
			if !session.AgentHost {
				if options.GitHub.AgentHost && !noHost {
					fmt.Fprintln(out, "Orka started no Copilot environment: GitHub gave this sign-in no refresh token, so its token does not expire. "+ //nolint:errcheck
						"Turn on user-token expiration in the GitHub App, then sign in again.")
				}
				return nil
			}
			hosts := client.NewWithNamespace(server, session.Token, session.Namespace)
			hosts.HTTPClient = sessionAPIClient
			return waitForAgentHost(ctx, cmd, hosts, session.Connection, agentHostReasonGitHubMembershipEnded, agentHostReasonGitHubTokenNeverExpires)
		},
	}
	cmd.Flags().BoolVar(&noHost, "no-host", false, "Link GitHub without starting a Copilot environment on Orka")
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "Print the GitHub URL without opening a browser")
	cmd.Flags().DurationVar(&timeout, "timeout", 15*time.Minute, "How long to wait for the code to be approved and the host to start")
	return cmd
}

func newLogoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "End your Orka session from orka login",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			unlock, err := lockConfig(ctx)
			if err != nil {
				return fmt.Errorf("wait for a session renewal: %w", err)
			}
			defer unlock()
			cfg := loadConfig()
			if cfg.TokenKind == tokenKindOIDC && cfg.Token != "" {
				cfg.Token, cfg.TokenKind, cfg.TokenExpiresAt, cfg.SessionServer, cfg.SessionLogin, cfg.SessionSubject = "", "", nil, "", "", ""
				cfg.OIDCIssuer, cfg.OIDCAudience, cfg.OIDCClientID, cfg.OIDCTokenEndpoint, cfg.OIDCRefreshToken, cfg.OIDCScope = "", "", "", "", "", ""
				if err := saveConfig(cfg); err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), "Signed out.") //nolint:errcheck
				return nil
			}
			if cfg.TokenKind != tokenKindGitHubSession || cfg.Token == "" {
				return errors.New("not signed in with orka login or orka auth login")
			}
			if err := sessionServerAllowed(cfg.SessionServer); err != nil {
				return fmt.Errorf("the saved session is not sent to its server: %w", err)
			}
			if err := plainJSON(ctx, http.MethodDelete, cfg.SessionServer+"/api/v1/auth/session", cfg.Token, nil, nil); err != nil &&
				!strings.Contains(err.Error(), "HTTP 401") {
				return fmt.Errorf("end the session: %w", err)
			}
			cfg.Token, cfg.TokenKind, cfg.TokenExpiresAt, cfg.SessionServer, cfg.SessionLogin, cfg.SessionSubject = "", "", nil, "", "", ""
			if err := saveConfig(cfg); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Signed out.") //nolint:errcheck
			return nil
		},
	}
}

func loginServer(cmd *cobra.Command) (string, error) {
	server := strings.TrimRight(strings.TrimSpace(configuredServer(cmd, loadConfig())), "/")
	if server == "" {
		return "", errors.New("pass --server, or set " + envServer + ", with your Orka address, such as https://orka.<tailnet>.ts.net")
	}
	if err := sessionServerAllowed(server); err != nil {
		return "", err
	}
	return server, nil
}

func isLoopbackHostname(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1":
		return true
	default:
		return false
	}
}

func sessionServerAllowed(server string) error {
	parsed, err := url.Parse(server)
	if err != nil || parsed.Host == "" {
		return fmt.Errorf("%q is not a server URL", server)
	}
	if parsed.Scheme == httpsScheme {
		return nil
	}
	if parsed.Scheme == plainHTTPScheme && isLoopbackHostname(parsed.Hostname()) {
		return nil
	}
	return fmt.Errorf("%s would carry your session in the clear; use https, or http only to localhost", server)
}

func configuredSession(cmd *cobra.Command, server string, cfg orkaConfig) string {
	if server == "" {
		server = cfg.Server
	}
	if cfg.Token == "" || strings.TrimRight(server, "/") != cfg.SessionServer || sessionServerAllowed(cfg.SessionServer) != nil {
		return ""
	}
	if !sessionEndsSoon(cmd, cfg) {
		return cfg.Token
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionRenewTimeout)
	defer cancel()
	unlock, err := lockConfig(ctx)
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Could not renew your Orka session (%v).\n", err) //nolint:errcheck
		return cfg.Token
	}
	defer unlock()
	saved := loadConfig()
	if !holdsSession(saved, cfg) {
		return cfg.Token
	}
	cfg = saved
	if !sessionEndsSoon(cmd, cfg) {
		return cfg.Token
	}
	var session sessionView
	err = plainJSON(ctx, http.MethodPost, cfg.SessionServer+"/api/v1/auth/session/renew", cfg.Token, nil, &session)
	if err == nil && session.Token == "" {
		err = errors.New("the server answered without a session")
	}
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Could not renew your Orka session (%v); run 'orka login' before %s.\n", //nolint:errcheck
			err, cfg.TokenExpiresAt.Local().Format(time.Kitchen))
		return cfg.Token
	}
	expires := session.ExpiresAt
	cfg.Token, cfg.TokenExpiresAt = session.Token, &expires
	if err := saveConfig(cfg); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Could not save the renewed session: %v\n", err) //nolint:errcheck
	}
	return session.Token
}

func holdsSession(saved, read orkaConfig) bool {
	return saved.TokenKind == tokenKindGitHubSession && saved.Token != "" &&
		saved.SessionServer == read.SessionServer &&
		read.SessionSubject != "" && saved.SessionSubject == read.SessionSubject
}

func sessionEndsSoon(cmd *cobra.Command, cfg orkaConfig) bool {
	if cfg.TokenExpiresAt == nil {
		return false
	}
	left := time.Until(*cfg.TokenExpiresAt)
	if left <= 0 {
		fmt.Fprintln(cmd.ErrOrStderr(), "Your Orka session ended; run 'orka login'.") //nolint:errcheck
		return false
	}
	return left <= sessionRenewWindow
}

func plainJSON(ctx context.Context, method, target, token string, body []byte, into any) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := noRedirectClient.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read the answer: %w", err)
	}
	if response.StatusCode >= 300 {
		var problem struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &problem)
		message := problem.Error.Message
		if message == "" {
			message = http.StatusText(response.StatusCode)
		}
		return fmt.Errorf("HTTP %d: %s", response.StatusCode, message)
	}
	if into != nil && len(raw) > 0 {
		return json.Unmarshal(raw, into)
	}
	return nil
}
