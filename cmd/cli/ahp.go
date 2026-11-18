/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

const (
	ahpTokensAPIPath = "/api/v1/ahp-tokens"
	ahpEndpointPath  = "/ahp"
	ahpTokenQuery    = "tkn"

	ahpStatusActive  = "Active"
	ahpStatusExpired = "Expired"
	ahpStatusRevoked = "Revoked"
)

// ahpTokenView is the API's view of an AHP connection token. Token is set
// only in the response that minted it.
type ahpTokenView struct {
	ID          string     `json:"id"`
	Token       string     `json:"token,omitempty"`
	Description string     `json:"description,omitempty"`
	Namespace   string     `json:"namespace,omitempty"`
	CreatedAt   *time.Time `json:"createdAt,omitempty"`
	ExpiresAt   time.Time  `json:"expiresAt"`
	RevokedAt   *time.Time `json:"revokedAt,omitempty"`
}

// ahpMintRequest asks the API for an AHP connection token.
type ahpMintRequest struct {
	TTL         string `json:"ttl,omitempty"`
	Description string `json:"description,omitempty"`
}

// ahpTokenListItem is one row of 'orka ahp tokens list' structured output.
type ahpTokenListItem struct {
	ahpTokenView
	Status string `json:"status"`
}

// ahpConnectResult is the structured output of 'orka ahp connect'.
type ahpConnectResult struct {
	ID        string    `json:"id"`
	Name      string    `json:"name,omitempty"`
	Namespace string    `json:"namespace,omitempty"`
	ExpiresAt time.Time `json:"expiresAt"`
	URL       string    `json:"url"`
}

func newAHPCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ahp",
		Short: "Connect desktop Agent Host Protocol clients to Orka",
		Long: "Desktop Agent Host Protocol (AHP) clients connect to Orka as a remote agent host by a WebSocket URL " +
			"that carries a connection token. 'orka ahp connect' mints one for the signed-in person and prints the URL; " +
			"'orka ahp tokens' lists and revokes them.",
	}
	tokens := &cobra.Command{Use: "tokens", Short: "List and revoke your AHP connection tokens"}
	tokens.AddCommand(newAHPTokensListCmd(), newAHPTokensRevokeCmd())
	cmd.AddCommand(newAHPConnectCmd(), tokens)
	return cmd
}

func newAHPConnectCmd() *cobra.Command {
	var name, ttl, hostURL string
	var insecure bool
	cmd := &cobra.Command{
		Use:   "connect",
		Short: "Mint an AHP connection token and print the URL to paste into a desktop AHP client",
		Long: "Mint an AHP connection token for the signed-in person and print the WebSocket URL a desktop AHP client " +
			"connects to. The token you use must be your own OIDC sign-in; context tokens and ServiceAccount " +
			"tokens cannot mint AHP tokens. The URL is shown once and carries the token: anyone holding it acts as " +
			"you on Orka until it expires or you revoke it.\n\n" +
			"The URL is built from --url, or else from the server the CLI talks to. Use --url with the public " +
			"https address people reach the Orka API on when the CLI reaches it another way, such as a port-forward.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := newClientFromCmd(cmd)
			base := hostURL
			if base == "" {
				base = c.BaseURL
			}
			endpoint, loopback, err := ahpHostURL(base, insecure)
			if err != nil {
				return err
			}
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			if !cmd.Flags().Changed("name") {
				name = defaultAHPDeviceName()
			}
			body, err := json.Marshal(ahpMintRequest{TTL: ttl, Description: name})
			if err != nil {
				return err
			}
			raw, err := c.DoJSON(cmd.Context(), http.MethodPost, ahpTokensAPIPath, nil, body)
			if err != nil {
				return ahpError(err)
			}
			var minted ahpTokenView
			if err := decodeInto(raw, &minted); err != nil {
				return err
			}
			if minted.Token == "" || minted.ID == "" {
				return errors.New("the server did not return an AHP token")
			}
			endpoint.RawQuery = url.Values{ahpTokenQuery: {minted.Token}}.Encode()
			connectURL := endpoint.String()
			if format != outputTable {
				return printStructuredTo(cmd.OutOrStdout(), format, ahpConnectResult{
					ID: minted.ID, Name: minted.Description, Namespace: minted.Namespace,
					ExpiresAt: minted.ExpiresAt, URL: connectURL,
				})
			}
			// Only the URL goes to standard output, so it can be piped to a
			// clipboard; the instructions go to standard error.
			errOut := cmd.ErrOrStderr()
			label := minted.ID
			if minted.Description != "" {
				label = fmt.Sprintf("%s (%s)", minted.ID, minted.Description)
			}
			fmt.Fprintf(errOut, "Minted AHP connection token %s, valid until %s.\n", label, minted.ExpiresAt.Local().Format(time.RFC1123)) //nolint:errcheck
			fmt.Fprintln(errOut, "In your desktop AHP client, add a remote agent host with this WebSocket URL:")                           //nolint:errcheck
			fmt.Fprintln(cmd.OutOrStdout(), connectURL)                                                                                    //nolint:errcheck
			fmt.Fprintln(errOut, "The URL carries your token and is shown only once: keep it private, as you would a password.")           //nolint:errcheck
			if loopback {
				fmt.Fprintln(errOut, "This URL points at this machine and works only while the API is reachable there, for example through a port-forward. "+ //nolint:errcheck
					"Pass --url with your Orka's public https address for a URL that works anywhere.")
			}
			fmt.Fprintf(errOut, "List your tokens with 'orka ahp tokens list'; revoke this one with 'orka ahp tokens revoke %s'.\n", minted.ID) //nolint:errcheck
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Name for this token, such as the device it is for (default: this machine's host name)")
	cmd.Flags().StringVar(&ttl, "ttl", "", "Token lifetime as a Go duration such as 720h (default: the server's, 720h unless configured)")
	cmd.Flags().StringVar(&hostURL, "url", "", "Public address of the Orka API to build the URL from, such as https://orka.example.com (default: --server)")
	cmd.Flags().BoolVar(&insecure, "insecure", false, "Allow a plaintext ws:// URL to a host other than this machine; the token then crosses the network unencrypted")
	addOutputFlag(cmd, outputTable)
	return cmd
}

// ahpHostURL turns an Orka API address into its AHP WebSocket endpoint. It
// reports whether the host is this machine, and refuses to put a token in a
// plaintext URL for any other host unless insecure is set.
func ahpHostURL(base string, insecure bool) (*url.URL, bool, error) {
	parsed, err := url.Parse(strings.TrimSpace(base))
	if err != nil || parsed.Host == "" {
		return nil, false, fmt.Errorf("invalid Orka API address %q: use an https URL such as https://orka.example.com", base)
	}
	if parsed.User != nil {
		return nil, false, errors.New("the Orka API address must not carry credentials")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https", "wss":
		parsed.Scheme = "wss"
	case "http", "ws":
		parsed.Scheme = "ws"
	default:
		return nil, false, fmt.Errorf("the Orka API address must be an http(s) or ws(s) URL, not %q", parsed.Scheme)
	}
	loopback := isLoopbackHost(parsed.Hostname())
	if parsed.Scheme == "ws" && !loopback && !insecure {
		return nil, false, fmt.Errorf("refusing to put a connection token in a plaintext ws:// URL for %s: "+
			"pass --url with the https address of your Orka API, or --insecure to allow it", parsed.Hostname())
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + ahpEndpointPath
	parsed.RawPath, parsed.RawQuery, parsed.Fragment, parsed.RawFragment = "", "", "", ""
	return parsed, loopback, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// defaultAHPDeviceName names a token after this machine.
func defaultAHPDeviceName() string {
	host, err := os.Hostname()
	if err != nil {
		return ""
	}
	host = strings.TrimSpace(host)
	if len(host) > 128 {
		host = host[:128]
	}
	return host
}

func ahpTokenStatus(token ahpTokenView, now time.Time) string {
	switch {
	case token.RevokedAt != nil:
		return ahpStatusRevoked
	case !now.Before(token.ExpiresAt):
		return ahpStatusExpired
	default:
		return ahpStatusActive
	}
}

func newAHPTokensListCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   cliListUse,
		Short: "List your AHP connection tokens",
		Long:  "List your AHP connection tokens without their secrets. Expired and revoked tokens are shown with --all.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			c := newClientFromCmd(cmd)
			raw, err := c.DoJSON(cmd.Context(), http.MethodGet, ahpTokensAPIPath, nil, nil)
			if err != nil {
				return ahpError(err)
			}
			var list struct {
				Items []ahpTokenView `json:"items"`
			}
			if err := decodeInto(raw, &list); err != nil {
				return err
			}
			now := time.Now()
			items := make([]ahpTokenListItem, 0, len(list.Items))
			for _, token := range list.Items {
				token.Token = ""
				status := ahpTokenStatus(token, now)
				if all || status == ahpStatusActive {
					items = append(items, ahpTokenListItem{ahpTokenView: token, Status: status})
				}
			}
			if format != outputTable {
				return printStructuredTo(cmd.OutOrStdout(), format, map[string]any{"items": items})
			}
			if len(items) == 0 {
				message := "No active AHP tokens. Mint one with 'orka ahp connect'."
				if !all && len(list.Items) > 0 {
					message = "No active AHP tokens; 'orka ahp tokens list --all' shows expired and revoked ones. Mint one with 'orka ahp connect'."
				}
				fmt.Fprintln(cmd.OutOrStdout(), message) //nolint:errcheck
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tNAME\tNAMESPACE\tCREATED\tEXPIRES\tSTATUS") //nolint:errcheck
			for _, item := range items {
				created := ""
				if item.CreatedAt != nil {
					created = item.CreatedAt.UTC().Format(time.RFC3339)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", item.ID, item.Description, item.Namespace, created, //nolint:errcheck
					item.ExpiresAt.UTC().Format(time.RFC3339), item.Status)
			}
			return w.Flush()
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "Include expired and revoked tokens")
	addOutputFlag(cmd, outputTable)
	return cmd
}

func newAHPTokensRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <id>...",
		Short: "Revoke AHP connection tokens and close their connections",
		Long: "Revoke one or more of your AHP connection tokens. Clients connected with a revoked token are " +
			"disconnected, and its URL stops working.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)
			var failed []error
			for _, id := range args {
				_, err := c.DoJSON(cmd.Context(), http.MethodDelete, ahpTokensAPIPath+"/"+url.PathEscape(id), nil, nil)
				if err != nil {
					if strings.Contains(err.Error(), "HTTP 404") {
						err = fmt.Errorf("AHP token %s not found", id)
					}
					failed = append(failed, ahpError(err))
					continue
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Revoked AHP token %s\n", id) //nolint:errcheck
			}
			return errors.Join(failed...)
		},
	}
}

// ahpError explains the failures people hit first: a token that is not a
// person's, and a controller without the AHP endpoint.
func ahpError(err error) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	switch {
	case strings.Contains(message, "HTTP 501"):
		return errors.Join(err, errors.New("the AHP endpoint is not enabled on this Orka; an operator turns it on with controller.ahp.enabled"))
	case strings.Contains(message, "HTTP 403") && strings.Contains(message, "AHP tokens require"):
		return errors.Join(err, errors.New("AHP tokens belong to a person signed in with OIDC: pass your OIDC token with --token (context tokens and ServiceAccount tokens cannot mint them)"))
	}
	return err
}
