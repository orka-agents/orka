/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"
)

// Config holds the CLI configuration persisted in ~/.mercan/config.yaml.
type Config struct {
	Server string `json:"server,omitempty" yaml:"server,omitempty"`
	Token  string `json:"token,omitempty" yaml:"token,omitempty"`
}

// configDir returns the path to ~/.mercan/.
func configDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("determining home directory: %w", err)
	}
	return filepath.Join(home, ".mercan"), nil
}

// configPath returns the path to ~/.mercan/config.yaml.
func configPath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.yaml"), nil
}

// loadConfig reads the CLI config from ~/.mercan/config.yaml.
// A missing file is not an error — it returns an empty Config.
func loadConfig() (*Config, error) {
	p, err := configPath()
	if err != nil {
		return &Config{}, nil
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{}, nil
		}
		return nil, fmt.Errorf("reading config file: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config file: %w", err)
	}
	return &cfg, nil
}

// saveConfig writes the config to ~/.mercan/config.yaml, creating the
// directory if necessary.
func saveConfig(cfg *Config) error {
	dir, err := configDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshalling config: %w", err)
	}
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, data, 0600); err != nil {
		return fmt.Errorf("writing config file: %w", err)
	}
	return nil
}

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Manage CLI configuration",
	}

	cmd.AddCommand(
		&cobra.Command{
			Use:   "set-server <url>",
			Short: "Set the default server URL",
			Args:  cobra.ExactArgs(1),
			RunE: func(_ *cobra.Command, args []string) error {
				cfg, err := loadConfig()
				if err != nil {
					return err
				}
				cfg.Server = args[0]
				if err := saveConfig(cfg); err != nil {
					return err
				}
				fmt.Printf("Server URL set to %s\n", args[0])
				return nil
			},
		},
		&cobra.Command{
			Use:   "set-token <token>",
			Short: "Set the default authentication token",
			Args:  cobra.ExactArgs(1),
			RunE: func(_ *cobra.Command, args []string) error {
				cfg, err := loadConfig()
				if err != nil {
					return err
				}
				cfg.Token = args[0]
				if err := saveConfig(cfg); err != nil {
					return err
				}
				fmt.Println("Token saved to ~/.mercan/config.yaml")
				return nil
			},
		},
		&cobra.Command{
			Use:   "view",
			Short: "View current configuration",
			RunE: func(_ *cobra.Command, _ []string) error {
				cfg, err := loadConfig()
				if err != nil {
					return err
				}
				data, err := yaml.Marshal(cfg)
				if err != nil {
					return fmt.Errorf("marshalling config: %w", err)
				}
				fmt.Print(string(data))
				return nil
			},
		},
	)

	return cmd
}
