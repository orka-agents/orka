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
	// plainHTTPScheme carries sessions only to this machine.
	plainHTTPScheme = "http"
)

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
			"--no-host. The session is saved in your Orka config and renews itself.",
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
				"accessToken": tokens.AccessToken, "refreshToken": tokens.RefreshToken, "expiresIn": tokens.ExpiresIn, "noHost": noHost,
			})
			if err != nil {
				return err
			}
			var session sessionView
			if err := plainJSON(ctx, http.MethodPost, server+"/api/v1/auth/github", "", body, &session); err != nil {
				return fmt.Errorf("sign in to %s: %w", server, err)
			}
			cfg := loadConfig()
			cfg.Server, cfg.Token, cfg.TokenKind, cfg.Namespace = server, session.Token, tokenKindGitHubSession, session.Namespace
			expires := session.ExpiresAt
			cfg.TokenExpiresAt = &expires
			if err := saveConfig(cfg); err != nil {
				return fmt.Errorf("save the session: %w", err)
			}
			fmt.Fprintf(out, "Signed in to %s as %s.\n", server, session.Login) //nolint:errcheck
			if !session.AgentHost {
				return nil
			}
			return waitForAgentHost(ctx, cmd, client.NewWithNamespace(server, session.Token, session.Namespace))
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
			cfg := loadConfig()
			if cfg.TokenKind != tokenKindGitHubSession || cfg.Token == "" {
				return errors.New("not signed in with orka login")
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := plainJSON(ctx, http.MethodDelete, cfg.Server+"/api/v1/auth/session", cfg.Token, nil, nil); err != nil &&
				!strings.Contains(err.Error(), "HTTP 401") {
				return fmt.Errorf("end the session: %w", err)
			}
			cfg.Token, cfg.TokenKind, cfg.TokenExpiresAt = "", "", nil
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
// when it ends soon. A session is sent only to the server it came from.
func configuredSession(cmd *cobra.Command, server string, cfg orkaConfig) string {
	if cfg.Token == "" || (server != "" && strings.TrimRight(server, "/") != cfg.Server) || sessionServerAllowed(cfg.Server) != nil {
		return ""
	}
	if cfg.TokenExpiresAt == nil {
		return cfg.Token
	}
	left := time.Until(*cfg.TokenExpiresAt)
	if left <= 0 {
		fmt.Fprintln(cmd.ErrOrStderr(), "Your Orka session ended; run 'orka login'.") //nolint:errcheck
		return cfg.Token
	}
	if left > sessionRenewWindow {
		return cfg.Token
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var session sessionView
	if err := plainJSON(ctx, http.MethodPost, cfg.Server+"/api/v1/auth/session/renew", cfg.Token, nil, &session); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Could not renew your Orka session (%v); run 'orka login' before %s.\n", //nolint:errcheck
			err, cfg.TokenExpiresAt.Local().Format(time.Kitchen))
		return cfg.Token
	}
	cfg.Token = session.Token
	expires := session.ExpiresAt
	cfg.TokenExpiresAt = &expires
	if err := saveConfig(cfg); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Could not save the renewed session: %v\n", err) //nolint:errcheck
	}
	return cfg.Token
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
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
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
