/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const (
	authTokenFormatToken      = "token"
	authTokenFormatHeaderJSON = "header-json"
)

func newAuthTokenCmd() *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Print the bearer token the CLI sends to the Orka server",
		Long: "Print the bearer token the CLI sends to the configured Orka server, so another client, " +
			"such as an MCP client or a script, can authenticate as you. The token comes from --token, " +
			"--token-file, " + envToken + " or your Orka config, in that order. A token saved by " +
			"'orka auth login' is refreshed first when it is near expiry, and the command fails instead of " +
			"printing a token that has expired. It never prints a session saved by 'orka login', and never " +
			"prints a kubeconfig credential: a server must be named with --server, " + envServer +
			" or the config. The output is a credential; do not log or share it.",
		Example: "  orka auth token\n" +
			"  orka auth token --format header-json",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAuthToken(cmd, format)
		},
	}
	cmd.Flags().StringVar(&format, "format", authTokenFormatToken,
		"Output format: token prints the bare token; header-json prints {\"Authorization\":\"Bearer <token>\"}")
	return cmd
}

func runAuthToken(cmd *cobra.Command, format string) error {
	if format != authTokenFormatToken && format != authTokenFormatHeaderJSON {
		return fmt.Errorf("--format must be %s or %s", authTokenFormatToken, authTokenFormatHeaderJSON)
	}
	token, err := authTokenFromConfig(cmd, loadConfig())
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	if format == authTokenFormatHeaderJSON {
		data, err := json.Marshal(map[string]string{"Authorization": "Bearer " + token})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, string(data))
		return err
	}
	_, err = fmt.Fprintln(out, token)
	return err
}

func authTokenFromConfig(cmd *cobra.Command, cfg orkaConfig) (string, error) {
	server := strings.TrimRight(strings.TrimSpace(configuredServer(cmd, cfg)), "/")
	if server == "" {
		return "", errors.New("no Orka server is set; pass --server, set " + envServer +
			", or sign in with 'orka auth login --server <url>'")
	}
	token, saved, err := configuredTokenSource(cmd, cfg)
	if err != nil {
		return "", err
	}
	if saved {
		switch cfg.TokenKind {
		case tokenKindGitHubSession:
			return "", errors.New("the saved session is from 'orka login' and reaches only your own connections " +
				"and agent host, so it is not printed; sign in with 'orka auth login' for a token other clients can use")
		case tokenKindOIDC:
			var expiresAt *time.Time
			token, expiresAt = configuredOIDCTokenWithExpiry(cmd, server, cfg)
			if token == "" {
				return "", fmt.Errorf("your 'orka auth login' sign-in is for %s, not %s; run 'orka auth login --server %s'",
					cfg.SessionServer, server, server)
			}
			if oidcTokenExpired(token, expiresAt) {
				return "", errors.New("your OIDC token has expired and could not be refreshed; run 'orka auth login'")
			}
		}
	}
	if token == "" {
		return "", fmt.Errorf("no token is set for %s; run 'orka auth login', or pass --token-file or set %s",
			server, envToken)
	}
	return token, nil
}

func oidcTokenExpired(token string, expiresAt *time.Time) bool {
	now := time.Now()
	if claims, ok := parseUnverifiedJWTClaims(token); ok && claims.Exp > 0 && !now.Before(time.Unix(claims.Exp, 0)) {
		return true
	}
	return expiresAt != nil && !now.Before(*expiresAt)
}
