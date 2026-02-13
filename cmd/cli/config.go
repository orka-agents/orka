/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Manage CLI configuration",
	}
	cmd.AddCommand(newConfigSetServerCmd())
	cmd.AddCommand(newConfigSetTokenCmd())
	cmd.AddCommand(newConfigViewCmd())
	return cmd
}

func newConfigSetServerCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set-server <url>",
		Short: "Set the default Mercan server URL",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			cfg := loadConfig()
			cfg.Server = args[0]
			if err := saveConfig(cfg); err != nil {
				return fmt.Errorf("save config: %w", err)
			}
			fmt.Printf("Server set to %s\n", args[0])
			return nil
		},
	}
}

func newConfigSetTokenCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set-token <token>",
		Short: "Set the default authentication token",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			cfg := loadConfig()
			cfg.Token = args[0]
			if err := saveConfig(cfg); err != nil {
				return fmt.Errorf("save config: %w", err)
			}
			fmt.Println("Token saved.")
			return nil
		},
	}
}

func newConfigViewCmd() *cobra.Command {
	const notSet = "(not set)"
	return &cobra.Command{
		Use:   "view",
		Short: "Show current configuration",
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg := loadConfig()
			server := cfg.Server
			if server == "" {
				server = notSet
			}
			token := notSet
			if cfg.Token != "" {
				token = maskToken(cfg.Token)
			}
			ns := cfg.Namespace
			if ns == "" {
				ns = notSet
			}
			path := configPath()
			if path == "" {
				path = "(unknown)"
			}

			fmt.Printf("Config: %s\n\n", path)
			fmt.Printf("  Server:    %s\n", server)
			fmt.Printf("  Token:     %s\n", token)
			fmt.Printf("  Namespace: %s\n", ns)
			return nil
		},
	}
}
