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
	// tokenKindGitHubSession marks a configured token as an Orka session
	// from orka login, which renews itself and belongs to one server.
	tokenKindGitHubSession = "github-session"
	// sessionRenewWindow is how long before its end a session renews.
	sessionRenewWindow = 2 * time.Hour
	// sessionRenewTimeout bounds one renewal, waiting for the config lock
	// included.
	sessionRenewTimeout = 30 * time.Second
	// plainHTTPScheme carries sessions only to this machine.
	plainHTTPScheme = "http"
)

// noRedirectClient carries credentials: a redirect, from the server or a
// proxy in front of it, would send them past the origin and transport checks,
// so it is answered as it is instead of followed.
var noRedirectClient = &http.Client{
	Timeout:       time.Minute,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// githubWebURL is GitHub's device-flow host, and githubPollFloor the shortest
// wait between polls (zero is GitHub's own); tests change both.
var (
	githubWebURL    = "https://github.com"
	githubPollFloor time.Duration
)

type authConfigView struct {
	OIDC   bool `json:"oidc"`
	GitHub *struct {
		ClientID  string `json:"clientID"`
		AgentHost bool   `json:"agentHost"`
	} `json:"github"`
}

type sessionView struct {
	Token      string    `json:"token"`
	ExpiresAt  time.Time `json:"expiresAt"`
	Login      string    `json:"login"`
	Namespace  string    `json:"namespace"`
	Connection string    `json:"connection"`
	AgentHost  bool      `json:"agentHost"`
}

func newLoginCmd() *cobra.Command {
	var noHost, noOpen bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "login",
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
			// Said before the code is shown, while stopping keeps the
			// saved token: logout clears the session and never restores it.
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
			// Without the disclosure above, no host may start: a host
			// enabled while the code was pending is declined.
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
				cfg.SessionServer, cfg.TokenExpiresAt, cfg.SessionLogin = server, &expires, session.Login
				return nil
			}); err != nil {
				return fmt.Errorf("save the session: %w", err)
			}
			fmt.Fprintf(out, "Signed in to %s as %s.\n", server, session.Login) //nolint:errcheck
			if !session.AgentHost {
				if options.GitHub.AgentHost && !noHost {
					// The server starts a host only for a token whose expiry
					// GitHub confirmed through its refresh token.
					fmt.Fprintln(out, "Orka started no Copilot environment: GitHub gave this sign-in no refresh token, so its token does not expire. "+ //nolint:errcheck
						"Turn on user-token expiration in the GitHub App, then sign in again.")
				}
				return nil
			}
			// The new session goes only to its own server: never through a
			// redirect.
			hosts := client.NewWithNamespace(server, session.Token, session.Namespace)
			hosts.HTTPClient = noRedirectClient
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
			// A renewal in flight would leave this the ended token while its
			// successor stays live: logout waits for it and ends what is saved.
			unlock, err := lockConfig(ctx)
			if err != nil {
				return fmt.Errorf("wait for a session renewal: %w", err)
			}
			defer unlock()
			cfg := loadConfig()
			if cfg.TokenKind != tokenKindGitHubSession || cfg.Token == "" {
				return errors.New("not signed in with orka login")
			}
			if err := sessionServerAllowed(cfg.SessionServer); err != nil {
				return fmt.Errorf("the saved session is not sent to its server: %w", err)
			}
			if err := plainJSON(ctx, http.MethodDelete, cfg.SessionServer+"/api/v1/auth/session", cfg.Token, nil, nil); err != nil &&
				!strings.Contains(err.Error(), "HTTP 401") {
				return fmt.Errorf("end the session: %w", err)
			}
			cfg.Token, cfg.TokenKind, cfg.TokenExpiresAt, cfg.SessionServer, cfg.SessionLogin = "", "", nil, "", ""
			if err := saveConfig(cfg); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Signed out.") //nolint:errcheck
			return nil
		},
	}
}

// loginServer is --server or the configured server. Sessions are bearer
// credentials, so plain HTTP is refused except to this machine.
func loginServer(cmd *cobra.Command) (string, error) {
	server, _ := cmd.Flags().GetString("server")
	if server == "" {
		server = loadConfig().Server
	}
	server = strings.TrimRight(strings.TrimSpace(server), "/")
	if server == "" {
		return "", errors.New("pass --server with your Orka address, such as https://orka.<tailnet>.ts.net")
	}
	if err := sessionServerAllowed(server); err != nil {
		return "", err
	}
	return server, nil
}

func sessionServerAllowed(server string) error {
	parsed, err := url.Parse(server)
	if err != nil || parsed.Host == "" {
		return fmt.Errorf("%q is not a server URL", server)
	}
	if parsed.Scheme == "https" {
		return nil
	}
	if parsed.Scheme == plainHTTPScheme {
		switch parsed.Hostname() {
		case "localhost", "127.0.0.1", "::1":
			return nil
		}
	}
	return fmt.Errorf("%s would carry your session in the clear; use https, or http only to localhost", server)
}

// configuredSession returns the configured session token for server, renewed
// when it ends soon. A session is sent only to the server that issued it.
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
	// The server renews a session once: another orka process renewing it
	// at the same time would leave this one the ended token. Renewals take
	// turns, and one that finds a successor saved meanwhile uses it.
	unlock, err := lockConfig(ctx)
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Could not renew your Orka session (%v).\n", err) //nolint:errcheck
		return cfg.Token
	}
	defer unlock()
	saved := loadConfig()
	if !holdsSession(saved, cfg) {
		// The session was replaced or ended meanwhile: nothing to renew.
		// Another person's session saved meanwhile is theirs, never this
		// command's.
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
	// The successor goes onto the config read under the lock: every config
	// write takes it, so no change made meanwhile is undone.
	expires := session.ExpiresAt
	cfg.Token, cfg.TokenExpiresAt = session.Token, &expires
	if err := saveConfig(cfg); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Could not save the renewed session: %v\n", err) //nolint:errcheck
	}
	return session.Token
}

// holdsSession reports whether saved holds a session of the same person from
// the same server as read: read's own, or its successor from a renewal.
func holdsSession(saved, read orkaConfig) bool {
	return saved.TokenKind == tokenKindGitHubSession && saved.Token != "" &&
		saved.SessionServer == read.SessionServer && saved.SessionLogin == read.SessionLogin
}

// sessionEndsSoon reports a session in its renewal window. One that already
// ended is not renewed: the person signs in again.
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

// plainJSON sends one JSON request outside the API client, for the sign-in
// calls that precede or replace its credential.
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
