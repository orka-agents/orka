/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"

	"github.com/spf13/cobra"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	rootCmd := newRootCmd()
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "mercan",
		Short:         "Mercan CLI — Kubernetes-native task execution platform",
		Long:          "Mercan CLI — Kubernetes-native task execution platform",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			// Load config file defaults for flags not explicitly set.
			if cfg, err := loadConfig(); err == nil {
				flags := cmd.Root().PersistentFlags()
				if cfg.Server != "" && !flags.Changed("server") {
					_ = flags.Set("server", cfg.Server)
				}
				if cfg.Token != "" && !flags.Changed("token") {
					_ = flags.Set("token", cfg.Token)
				}
			}

			// Skip auth resolution for login and config commands
			if cmd.Name() == "login" || cmd.Parent() != nil && cmd.Parent().Name() == "config" {
				return nil
			}
			token, _ := cmd.Root().PersistentFlags().GetString("token")
			if token != "" {
				return nil
			}
			kubeconfig, _ := cmd.Root().PersistentFlags().GetString("kubeconfig")
			extracted, err := extractToken(kubeconfig)
			if err != nil {
				return nil // non-fatal: some commands may not need auth
			}
			if extracted != "" {
				_ = cmd.Root().PersistentFlags().Set("token", extracted)
			}
			return nil
		},
	}

	cmd.PersistentFlags().String("server", "http://localhost:8080", "Mercan server URL")
	cmd.PersistentFlags().String("token", "", "Authentication token")
	cmd.PersistentFlags().String("kubeconfig", "", "Path to kubeconfig file")
	cmd.PersistentFlags().StringP("namespace", "n", "default", "Kubernetes namespace")
	cmd.PersistentFlags().StringP("output", "o", "table", "Output format (table, json, yaml)")

	cmd.AddCommand(
		newLoginCmd(),
		newTaskCmd(),
		newAgentCmd(),
		newSessionCmd(),
		newToolCmd(),
		newRunCmd(),
		newConfigCmd(),
	)

	return cmd
}

func newLoginCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Authenticate with the Mercan dashboard",
		Long:  "Extract a token from your kubeconfig and open the Mercan dashboard in your browser.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			server, _ := cmd.Root().PersistentFlags().GetString("server")
			token, _ := cmd.Flags().GetString("token")
			kubeconfig, _ := cmd.Root().PersistentFlags().GetString("kubeconfig")

			if token == "" {
				var err error
				token, err = extractToken(kubeconfig)
				if err != nil {
					return fmt.Errorf("error extracting token from kubeconfig: %w\nYou can provide a token directly with --token", err)
				}
			}

			if token == "" {
				return fmt.Errorf("no token found in kubeconfig. Use --token to provide one directly")
			}

			loginURL := fmt.Sprintf("%s/#token=%s", server, token)

			fmt.Printf("Opening Mercan dashboard at %s\n", server)
			if err := openBrowser(loginURL); err != nil {
				fmt.Fprintf(os.Stderr, "Could not open browser: %v\n", err)
				fmt.Printf("\nOpen this URL manually:\n  %s\n", loginURL)
			}
			return nil
		},
	}

	cmd.Flags().StringP("token", "t", "", "Use a specific token instead of extracting from kubeconfig")

	return cmd
}

// extractToken reads the bearer token from the current kubeconfig context.
func extractToken(kubeconfigPath string) (string, error) {
	loadingRules := &clientcmd.ClientConfigLoadingRules{}
	if kubeconfigPath != "" {
		loadingRules.ExplicitPath = kubeconfigPath
	} else {
		loadingRules = clientcmd.NewDefaultClientConfigLoadingRules()
	}

	config := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, nil)

	rawConfig, err := config.RawConfig()
	if err != nil {
		return "", fmt.Errorf("failed to load kubeconfig: %w", err)
	}

	contextName := rawConfig.CurrentContext
	if contextName == "" {
		return "", fmt.Errorf("no current context set in kubeconfig")
	}

	ctx, ok := rawConfig.Contexts[contextName]
	if !ok {
		return "", fmt.Errorf("context %q not found in kubeconfig", contextName)
	}

	authInfo, ok := rawConfig.AuthInfos[ctx.AuthInfo]
	if !ok {
		return "", fmt.Errorf("user %q not found in kubeconfig", ctx.AuthInfo)
	}

	if authInfo.Token != "" {
		return authInfo.Token, nil
	}

	if authInfo.TokenFile != "" {
		data, err := os.ReadFile(authInfo.TokenFile)
		if err != nil {
			return "", fmt.Errorf("failed to read token file %q: %w", authInfo.TokenFile, err)
		}
		return string(data), nil
	}

	if authInfo.Exec != nil {
		restConfig, err := config.ClientConfig()
		if err != nil {
			return "", fmt.Errorf("failed to get REST config for exec-based auth: %w", err)
		}
		if restConfig.BearerToken != "" {
			return restConfig.BearerToken, nil
		}
	}

	if authInfo.AuthProvider != nil {
		restConfig, err := config.ClientConfig()
		if err != nil {
			return "", fmt.Errorf("failed to get REST config for auth provider: %w", err)
		}
		if restConfig.BearerToken != "" {
			return restConfig.BearerToken, nil
		}
	}

	return "", fmt.Errorf("no token, token file, exec, or auth provider found for user %q", ctx.AuthInfo)
}

// openBrowser opens the given URL in the default browser.
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
	return cmd.Start()
}
