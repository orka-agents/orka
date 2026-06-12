/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Manage CLI configuration",
	}
	cmd.AddCommand(newConfigSetServerCmd())
	cmd.AddCommand(newConfigSetTokenCmd())
	cmd.AddCommand(newConfigSetNamespaceCmd())
	cmd.AddCommand(newConfigViewCmd())
	return cmd
}

func newConfigSetServerCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set-server <url>",
		Short: "Set the default Orka server URL",
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
	var tokenFile string
	cmd := &cobra.Command{
		Use:   "set-token [token]",
		Short: "Set the default authentication token",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if tokenFile != "" && len(args) > 0 {
				return fmt.Errorf("provide token as an argument or --file, not both")
			}

			value := ""
			if tokenFile != "" {
				data, err := readTokenInput(cmd, tokenFile)
				if err != nil {
					return err
				}
				value = strings.TrimSpace(string(data))
			} else if len(args) == 1 {
				value = strings.TrimSpace(args[0])
			}
			if value == "" {
				return fmt.Errorf("token argument or --file is required")
			}

			cfg := loadConfig()
			cfg.Token = value
			if err := saveConfig(cfg); err != nil {
				return fmt.Errorf("save config: %w", err)
			}
			fmt.Println("Token saved.")
			return nil
		},
	}
	cmd.Flags().StringVarP(&tokenFile, "file", "f", "", "Read token from file (use - for stdin)")
	return cmd
}

func readTokenInput(cmd *cobra.Command, path string) ([]byte, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("token file path is required")
	}
	if path == "-" {
		return io.ReadAll(cmd.InOrStdin())
	}
	return os.ReadFile(path)
}

func newConfigSetNamespaceCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set-namespace <namespace>",
		Short: "Set the default Kubernetes namespace",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			ns := strings.TrimSpace(args[0])
			if ns == "" {
				return fmt.Errorf("namespace is required")
			}
			cfg := loadConfig()
			cfg.Namespace = ns
			if err := saveConfig(cfg); err != nil {
				return fmt.Errorf("save config: %w", err)
			}
			fmt.Printf("Namespace set to %s\n", ns)
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
