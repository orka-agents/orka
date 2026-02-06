/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"

	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[0] + " " + os.Args[1] {
	case programName() + " login":
		loginCmd(os.Args[2:])
	default:
		// If called as just "mercan login" or with other subcommands
		if os.Args[1] == "login" {
			loginCmd(os.Args[2:])
		} else {
			fmt.Fprintf(os.Stderr, "Unknown command: %s\n", os.Args[1])
			printUsage()
			os.Exit(1)
		}
	}
}

func programName() string {
	if len(os.Args) > 0 {
		return os.Args[0]
	}
	return "mercan"
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `Usage: mercan <command>

Commands:
  login    Authenticate with the Mercan dashboard

Run 'mercan login --help' for more information.
`)
}

func loginCmd(args []string) {
	var server string
	var kubeconfig string
	var token string
	var help bool

	// Simple flag parsing for the login subcommand
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--server", "-s":
			if i+1 < len(args) {
				i++
				server = args[i]
			}
		case "--kubeconfig":
			if i+1 < len(args) {
				i++
				kubeconfig = args[i]
			}
		case "--token", "-t":
			if i+1 < len(args) {
				i++
				token = args[i]
			}
		case "--help", "-h":
			help = true
		default:
			if args[i][0] == '-' {
				fmt.Fprintf(os.Stderr, "Unknown flag: %s\n", args[i])
				os.Exit(1)
			}
		}
	}

	if help {
		fmt.Print(`Usage: mercan login [flags]

Extract a token from your kubeconfig and open the Mercan dashboard in your browser.

Flags:
  -s, --server string       Mercan server URL (default "http://localhost:8080")
  -t, --token string        Use a specific token instead of extracting from kubeconfig
      --kubeconfig string   Path to kubeconfig file (default: $KUBECONFIG or ~/.kube/config)
  -h, --help                Show this help message
`)
		return
	}

	if server == "" {
		server = "http://localhost:8080"
	}

	// Get token
	if token == "" {
		var err error
		token, err = extractToken(kubeconfig)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error extracting token from kubeconfig: %v\n", err)
			fmt.Fprintf(os.Stderr, "You can provide a token directly with --token\n")
			os.Exit(1)
		}
	}

	if token == "" {
		fmt.Fprintln(os.Stderr, "No token found in kubeconfig. Use --token to provide one directly.")
		os.Exit(1)
	}

	// Build the login URL
	loginURL := fmt.Sprintf("%s/#token=%s", server, token)

	fmt.Printf("Opening Mercan dashboard at %s\n", server)
	if err := openBrowser(loginURL); err != nil {
		fmt.Fprintf(os.Stderr, "Could not open browser: %v\n", err)
		fmt.Printf("\nOpen this URL manually:\n  %s\n", loginURL)
	}
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

	// Get current context
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

	// Try bearer token first
	if authInfo.Token != "" {
		return authInfo.Token, nil
	}

	// Try token file
	if authInfo.TokenFile != "" {
		data, err := os.ReadFile(authInfo.TokenFile)
		if err != nil {
			return "", fmt.Errorf("failed to read token file %q: %w", authInfo.TokenFile, err)
		}
		return string(data), nil
	}

	// Try exec-based auth (e.g., gke-gcloud-auth-plugin, aws-iam-authenticator)
	if authInfo.Exec != nil {
		restConfig, err := config.ClientConfig()
		if err != nil {
			return "", fmt.Errorf("failed to get REST config for exec-based auth: %w", err)
		}
		if restConfig.BearerToken != "" {
			return restConfig.BearerToken, nil
		}
	}

	// Try auth provider (e.g., OIDC)
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
