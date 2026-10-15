/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"
)

// dashboardAbsentFunc reports whether a server definitively serves no
// dashboard. Tests replace it to stay off the network.
var dashboardAbsentFunc = dashboardAbsent

// dashboardAbsent asks server for the dashboard's login page. Only a 404 is
// an answer: the controller serves that page whenever its dashboard is
// enabled, while an unreachable server or a proxy that wants its own sign-in
// first says nothing either way and keeps the URL flow as it was.
func dashboardAbsent(server string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server+"/login", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close() //nolint:errcheck
	return resp.StatusCode == http.StatusNotFound
}

func newLoginCmd() *cobra.Command {
	var serviceAccount string
	var noOpen, redactToken bool

	cmd := &cobra.Command{
		Use:   "login",
		Short: "Authenticate with the Orka dashboard",
		Long: "Generate a ServiceAccount token and open the Orka dashboard in your browser. The controller serves the " +
			"dashboard only when it runs with --dashboard-enabled; every other orka command works without it.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			server, _ := cmd.Flags().GetString("server")
			token, _ := cmd.Flags().GetString("token")
			ns, _ := cmd.Flags().GetString("namespace")

			cfg := loadConfig()
			if server == "" {
				server = cfg.Server
			}
			if server == "" {
				server = defaultServer
			}
			if ns == "" {
				ns = cfg.Namespace
			}
			if ns == "" {
				ns = "default"
			}

			// Checked before a token is minted, so a server without the
			// dashboard never gets a credential that has nowhere to go.
			if dashboardAbsentFunc(server) {
				return fmt.Errorf("%s does not serve the dashboard: start the controller with --dashboard-enabled "+
					"(Helm: controller.dashboard.enabled=true), or use the orka CLI, which works without it", server)
			}

			if token == "" {
				var err error
				token, err = serviceAccountLoginFunc(serviceAccount, ns)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Error creating token: %v\n", err)
					fmt.Fprintf(os.Stderr, "You can provide a token directly with --token\n")
					return err
				}
			}

			loginURL := fmt.Sprintf("%s/login#token=%s", server, token)
			displayURL := loginURL
			if redactToken {
				displayURL = fmt.Sprintf("%s/login#token=<redacted>", server)
			}
			fmt.Printf("Login URL: %s\n", displayURL)

			if noOpen {
				if redactToken {
					fmt.Println(
						"Browser opening skipped. The printed login URL is redacted and cannot be opened manually. " +
							"Rerun without --redact-token in a trusted terminal if you need to copy the URL.",
					)
				} else {
					fmt.Println("Browser opening skipped. Open the login URL above in your browser manually.")
				}
				return nil
			}

			if err := openBrowser(loginURL); err != nil {
				fmt.Fprintf(os.Stderr, "Could not open browser: %v\n", err)
				if redactToken {
					fmt.Fprintln(
						os.Stderr,
						"The printed login URL is redacted and cannot be opened manually. "+
							"Rerun without --redact-token in a trusted terminal if you need to copy the URL.",
					)
				} else {
					fmt.Fprintln(os.Stderr, "Open the URL above in your browser manually.")
				}
				return nil
			}
			fmt.Println("Browser opened successfully. You can now log in to the Orka dashboard.")
			return nil
		},
	}

	cmd.Flags().StringVar(&serviceAccount, "service-account", "default", "ServiceAccount name")
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "Print the login URL without opening a browser")
	cmd.Flags().BoolVar(
		&redactToken,
		"redact-token",
		false,
		"Redact the token in printed output while preserving browser login",
	)

	return cmd
}
