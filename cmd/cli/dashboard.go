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

// dashboardProbeClient answers with the first response, so an SSO gateway
// that redirects /login is judged by that redirect, not by wherever it leads.
var dashboardProbeClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

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
	resp, err := dashboardProbeClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close() //nolint:errcheck
	return resp.StatusCode == http.StatusNotFound
}

func newDashboardCmd() *cobra.Command {
	var serviceAccount string
	var noOpen, showToken bool

	cmd := &cobra.Command{
		Use:   "dashboard",
		Short: "Open the Orka dashboard in your browser, signed in",
		Long: "Create a ServiceAccount token (or use --token) and open the Orka dashboard in your browser, signed in " +
			"with it. The token goes only to the browser; other orka commands do not use it. The controller serves the " +
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
				token, err = serviceAccountTokenFunc(serviceAccount, ns)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Error creating token: %v\n", err)
					fmt.Fprintf(os.Stderr, "You can provide a token directly with --token\n")
					return err
				}
			}

			dashboardURL := fmt.Sprintf("%s/login#token=%s", server, token)
			displayURL := dashboardURL
			if !showToken {
				displayURL = fmt.Sprintf("%s/login#token=<redacted>", server)
			}
			fmt.Printf("Dashboard URL: %s\n", displayURL)

			if noOpen {
				if !showToken {
					fmt.Println(
						"Browser opening skipped. The printed dashboard URL is redacted and cannot be opened manually. " +
							"Rerun with --show-token in a trusted terminal if you need to copy the URL.",
					)
				} else {
					fmt.Println("Browser opening skipped. Open the dashboard URL above in your browser manually.")
				}
				return nil
			}

			if err := openBrowser(dashboardURL); err != nil {
				fmt.Fprintf(os.Stderr, "Could not open browser: %v\n", err)
				if !showToken {
					fmt.Fprintln(
						os.Stderr,
						"The printed dashboard URL is redacted and cannot be opened manually. "+
							"Rerun with --show-token in a trusted terminal if you need to copy the URL.",
					)
				} else {
					fmt.Fprintln(os.Stderr, "Open the URL above in your browser manually.")
				}
				return nil
			}
			fmt.Println("Opened the Orka dashboard in your browser.")
			return nil
		},
	}

	cmd.Flags().StringVar(&serviceAccount, "service-account", "default", "ServiceAccount name")
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "Print the dashboard URL without opening a browser")
	cmd.Flags().BoolVar(
		&showToken,
		"show-token",
		false,
		"Print the dashboard URL with the token in it instead of <redacted>; use only in a trusted terminal",
	)

	return cmd
}
