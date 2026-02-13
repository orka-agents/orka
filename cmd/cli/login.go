/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func newLoginCmd() *cobra.Command {
	var serviceAccount string

	cmd := &cobra.Command{
		Use:   "login",
		Short: "Authenticate with the Mercan dashboard",
		Long:  "Generate a ServiceAccount token and open the Mercan dashboard in your browser.",
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
				ns = "default"
			}

			if token == "" {
				var err error
				token, err = createServiceAccountToken(serviceAccount, ns)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Error creating token: %v\n", err)
					fmt.Fprintf(os.Stderr, "You can provide a token directly with --token\n")
					return err
				}
			}

			loginURL := fmt.Sprintf("%s/login#token=%s", server, token)
			fmt.Printf("Login URL: %s\n", loginURL)

			if err := openBrowser(loginURL); err != nil {
				fmt.Fprintf(os.Stderr, "Could not open browser: %v\n", err)
				fmt.Fprintln(os.Stderr, "Open the URL above in your browser manually.")
				return nil
			}
			fmt.Println("Browser opened successfully. You can now log in to the Mercan dashboard.")
			return nil
		},
	}

	cmd.Flags().StringVar(&serviceAccount, "service-account", "default", "ServiceAccount name")

	return cmd
}
