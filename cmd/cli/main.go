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
	"strings"

	"k8s.io/client-go/tools/clientcmd"

	"github.com/sozercan/mercan/internal/cli"
)

const (
	flagServer    = "--server"
	flagToken     = "--token"
	flagHelp      = "--help"
	flagNamespace = "--namespace"
	defaultServer = "http://localhost:8080"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "login":
		loginCmd(os.Args[2:])
	case "chat":
		chatCmd(os.Args[2:])
	case "agent":
		agentCmd(os.Args[2:])
	case "task":
		cli.RunTaskCmd(os.Args[2:])
	case "status":
		statusCmd(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `Usage: mercan <command>

Commands:
  login    Authenticate with the Mercan dashboard
  chat     Interactive chat with the Mercan AI assistant
  status   Show system overview (health, tasks, agents)
  agent    Manage agents
  task     Manage tasks

Run 'mercan <command> --help' for more information.
`)
}

func loginCmd(args []string) {
	var server string
	var namespace string
	var serviceAccount string
	var token string
	var help bool

	// Simple flag parsing for the login subcommand
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case flagServer, "-s":
			if i+1 < len(args) {
				i++
				server = args[i]
			}
		case flagNamespace, "-n":
			if i+1 < len(args) {
				i++
				namespace = args[i]
			}
		case "--service-account":
			if i+1 < len(args) {
				i++
				serviceAccount = args[i]
			}
		case flagToken, "-t":
			if i+1 < len(args) {
				i++
				token = args[i]
			}
		case flagHelp, "-h":
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

Generate a ServiceAccount token and open the Mercan dashboard in your browser.

Flags:
  -s, --server string            Mercan server URL (default "http://localhost:8080")
  -n, --namespace string         Namespace of the ServiceAccount (default "default")
      --service-account string   ServiceAccount name (default "default")
  -t, --token string             Use a specific token instead of generating one
  -h, --help                     Show this help message
`)
		return
	}

	if server == "" {
		server = defaultServer
	}
	if namespace == "" {
		namespace = "default"
	}
	if serviceAccount == "" {
		serviceAccount = "default"
	}

	// Get token
	if token == "" {
		var err error
		token, err = createServiceAccountToken(serviceAccount, namespace)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error creating token: %v\n", err)
			fmt.Fprintf(os.Stderr, "You can provide a token directly with --token\n")
			os.Exit(1)
		}
	}

	// Build the login URL
	loginURL := fmt.Sprintf("%s/login#token=%s", server, token)

	fmt.Printf("Login URL: %s\n", loginURL)
	if err := openBrowser(loginURL); err != nil {
		fmt.Fprintf(os.Stderr, "Could not open browser: %v\n", err)
		fmt.Fprintln(os.Stderr, "Open the URL above in your browser manually.")
		return
	}
	fmt.Println("Browser opened successfully. You can now log in to the Mercan dashboard.")
}

// createServiceAccountToken generates a token for the given ServiceAccount using kubectl.
func createServiceAccountToken(serviceAccount, namespace string) (string, error) {
	kubectlPath, err := exec.LookPath("kubectl")
	if err != nil {
		return "", fmt.Errorf("kubectl not found in PATH: %w", err)
	}

	cmd := exec.Command(kubectlPath, "create", "token", serviceAccount, "-n", namespace, "--duration=24h")
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("kubectl create token failed: %s", string(exitErr.Stderr))
		}
		return "", fmt.Errorf("kubectl create token failed: %w", err)
	}

	token := strings.TrimSpace(string(out))
	if token == "" {
		return "", fmt.Errorf("kubectl returned an empty token")
	}
	return token, nil
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

func chatCmd(args []string) {
	var opts cli.ChatOptions
	var help bool

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case flagServer, "-s":
			if i+1 < len(args) {
				i++
				opts.Server = args[i]
			}
		case flagToken, "-t":
			if i+1 < len(args) {
				i++
				opts.Token = args[i]
			}
		case flagNamespace, "-n":
			if i+1 < len(args) {
				i++
				opts.Namespace = args[i]
			}
		case "--session":
			if i+1 < len(args) {
				i++
				opts.SessionID = args[i]
			}
		case flagHelp, "-h":
			help = true
		default:
			if len(args[i]) > 0 && args[i][0] == '-' {
				fmt.Fprintf(os.Stderr, "Unknown flag: %s\n", args[i])
				os.Exit(1)
			}
		}
	}

	if help {
		fmt.Print(`Usage: mercan chat [flags]

Start an interactive chat session with the Mercan AI assistant.

Flags:
  -s, --server string       Mercan server URL (default defaultServer)
  -t, --token string        Bearer token for authentication
  -n, --namespace string    Kubernetes namespace (default "default")
      --session string      Resume a specific session ID
  -h, --help                Show this help message

Commands (during chat):
  /help      Show available commands
  /clear     Start a new session
  /session   Show current session ID
  /quit      Exit chat
`)
		return
	}

	// If no token provided, try to extract from kubeconfig
	if opts.Token == "" {
		token, err := extractToken("")
		if err == nil && token != "" {
			opts.Token = token
		}
	}

	cli.RunChat(opts)
}

func statusCmd(args []string) {
	var opts cli.StatusOptions
	var help bool

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case flagServer, "-s":
			if i+1 < len(args) {
				i++
				opts.Server = args[i]
			}
		case flagToken, "-t":
			if i+1 < len(args) {
				i++
				opts.Token = args[i]
			}
		case flagHelp, "-h":
			help = true
		default:
			if len(args[i]) > 0 && args[i][0] == '-' {
				fmt.Fprintf(os.Stderr, "Unknown flag: %s\n", args[i])
				os.Exit(1)
			}
		}
	}

	if help {
		fmt.Print(`Usage: mercan status [flags]

Show system overview including health status, task counts, and agent count.

Flags:
  -s, --server string   Mercan server URL (default defaultServer)
  -t, --token string    Bearer token for authentication
  -h, --help            Show this help message
`)
		return
	}

	// If no token provided, try to extract from kubeconfig
	if opts.Token == "" {
		token, err := extractToken("")
		if err == nil && token != "" {
			opts.Token = token
		}
	}

	cli.RunStatus(opts)
}
