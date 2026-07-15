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

	"github.com/spf13/cobra"
	"k8s.io/client-go/tools/clientcmd"
)

const defaultServer = "http://localhost:8080"

// version is set via -ldflags at build time.
var version = "dev"

func main() {
	rootCmd := newRootCmd()
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "orka",
		Short:         "Orka CLI — Kubernetes-native task execution platform",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPostRun: func(cmd *cobra.Command, args []string) {
			// Don't kill port-forward on normal exit — let it persist for caching.
			// It will be cleaned up when the cache expires or on interrupt.
		},
	}

	// Global flags
	cmd.PersistentFlags().StringP("server", "s", "", "Orka server URL (default \"http://localhost:8080\")")
	cmd.PersistentFlags().StringP("token", "t", "", "Bearer token for authentication")
	cmd.PersistentFlags().String("txn-token", "", "Transaction token to send via Txn-Token header")
	cmd.PersistentFlags().String(
		"txn-token-file",
		"",
		"Path to file containing a Transaction token (use - for stdin)",
	)
	cmd.PersistentFlags().StringP("namespace", "n", "", "Kubernetes namespace (default \"default\")")
	cmd.PersistentFlags().String("kubeconfig", "", "Path to kubeconfig file")

	// Register subcommands
	cmd.AddCommand(newLoginCmd())
	cmd.AddCommand(newRunCmd())
	cmd.AddCommand(newConfigCmd())
	cmd.AddCommand(newAgentCmd())
	cmd.AddCommand(newTaskCmd())
	cmd.AddCommand(newAuditCmd())
	cmd.AddCommand(newStatusCmd())
	cmd.AddCommand(newSkillCmd())
	cmd.AddCommand(newProviderCmd())
	cmd.AddCommand(newToolCmd())
	cmd.AddCommand(newSessionCmd())
	cmd.AddCommand(newSecretCmd())
	cmd.AddCommand(newSecurityCmd())
	cmd.AddCommand(newMonitorCmd())
	cmd.AddCommand(newMemoryCmd())
	cmd.AddCommand(newAuthCmd())
	cmd.AddCommand(newModelsCmd())
	cmd.AddCommand(newWorkspaceCmd())
	cmd.AddCommand(newSubstrateCmd())

	return cmd
}

// serviceAccountLoginFunc is used by login to request ServiceAccount login credentials.
// Tests can replace this to avoid calling kubectl.
var serviceAccountLoginFunc = createServiceAccountToken

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

// kubeContext holds values extracted from the current kubeconfig context.
type kubeContext struct {
	token     string
	namespace string
}

// extractKubeContext reads the token and namespace from the current kubeconfig context.
func extractKubeContext(kubeconfigPath string) kubeContext {
	var kc kubeContext
	loadingRules := &clientcmd.ClientConfigLoadingRules{}
	if kubeconfigPath != "" {
		loadingRules.ExplicitPath = kubeconfigPath
	} else {
		loadingRules = clientcmd.NewDefaultClientConfigLoadingRules()
	}

	config := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, nil)

	rawConfig, err := config.RawConfig()
	if err != nil {
		return kc
	}

	contextName := rawConfig.CurrentContext
	if contextName == "" {
		return kc
	}

	ctx, ok := rawConfig.Contexts[contextName]
	if !ok {
		return kc
	}

	// Extract namespace from context
	if ctx.Namespace != "" {
		kc.namespace = ctx.Namespace
	}

	// Extract token
	if t, err := extractToken(kubeconfigPath); err == nil && t != "" {
		kc.token = t
	}

	return kc
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

// openBrowserFunc is the function used to open URLs in the default browser.
// Tests can replace this to avoid launching a real browser.
var openBrowserFunc = openBrowserDefault

// openBrowser opens the given URL in the default browser.
func openBrowser(url string) error {
	return openBrowserFunc(url)
}

func openBrowserDefault(url string) error {
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
