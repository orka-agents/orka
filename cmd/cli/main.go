/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

const defaultServer = "http://localhost:8080"

var version = "dev"

func main() {
	rootCmd := newRootCmd()
	if err := rootCmd.Execute(); err != nil {
		if exitErr, ok := errors.AsType[*operatorExitError](err); ok {
			if exitErr.code != 0 && !strings.HasPrefix(exitErr.message, "command exited with status") {
				fmt.Fprintln(os.Stderr, "Error:", exitErr.message)
			}
			os.Exit(exitErr.code)
		}
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:           cliProgramName,
		Short:         "Orka CLI — Kubernetes-native task execution platform",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPostRun: func(cmd *cobra.Command, args []string) {
		},
	}

	cmd.PersistentFlags().StringP("server", "s", "",
		"Orka server URL, or set "+envServer+" (default: the Orka Service reached through kubeconfig, else \""+defaultServer+"\")")
	cmd.PersistentFlags().StringP("token", "t", "", tokenFlagUsage)
	cmd.PersistentFlags().String("token-file", "", "Path to file containing the bearer token (use - for stdin)")
	cmd.PersistentFlags().String("txn-token", "", "Transaction token to send via Txn-Token header")
	cmd.PersistentFlags().String(
		"txn-token-file",
		"",
		"Path to file containing a Transaction token (use - for stdin)",
	)
	cmd.PersistentFlags().StringP("namespace", "n", "",
		"Kubernetes namespace, or set "+envNamespace+" (default: from config, else the namespace the Orka server watches)")
	cmd.PersistentFlags().String("kubeconfig", "", "Path to kubeconfig file")

	cmd.AddCommand(newVersionCmd())
	cmd.AddCommand(newDashboardCmd())
	cmd.AddCommand(newConnectCmd())
	cmd.AddCommand(newConnectionCmd())
	cmd.AddCommand(newHostCmd())
	cmd.AddCommand(newLoginCmd())
	cmd.AddCommand(newLogoutCmd())
	cmd.AddCommand(newAHPCmd())
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
	cmd.AddCommand(newUsageCmd())
	cmd.AddCommand(newMemoryCmd())
	cmd.AddCommand(newAuthCmd())
	cmd.AddCommand(newModelsCmd())
	cmd.AddCommand(newWorkspaceCmd())
	cmd.AddCommand(newRuntimePoolCmd())
	cmd.AddCommand(newAgentRuntimeCmd())
	cmd.AddCommand(newSubstrateCmd())
	cmd.AddCommand(newGatewayCmd())

	return cmd
}

var serviceAccountTokenFunc = createServiceAccountToken

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

var openBrowserFunc = openBrowserDefault

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
