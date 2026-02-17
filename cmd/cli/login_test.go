/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestNewLoginCmd_Structure(t *testing.T) {
	cmd := newLoginCmd()

	if cmd.Use != "login" {
		t.Errorf("Use = %q, want %q", cmd.Use, "login")
	}
	if cmd.Short == "" {
		t.Error("Short description should not be empty")
	}
	if cmd.Long == "" {
		t.Error("Long description should not be empty")
	}
}

func TestNewLoginCmd_ServiceAccountFlag(t *testing.T) {
	cmd := newLoginCmd()

	flag := cmd.Flags().Lookup("service-account")
	if flag == nil {
		t.Fatal("missing flag 'service-account'")
	}
	if flag.DefValue != "default" {
		t.Errorf("service-account default = %q, want %q", flag.DefValue, "default")
	}
}

func setupFakeLoginEnv(t *testing.T) (string, string) {
	t.Helper()
	binDir := t.TempDir()
	kubectlArgsPath := filepath.Join(binDir, "kubectl-args.txt")
	openArgsPath := filepath.Join(binDir, "open-args.txt")

	writeExecutable := func(name, script string) {
		t.Helper()
		path := filepath.Join(binDir, name)
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatalf("write fake %s: %v", name, err)
		}
	}

	writeExecutable("kubectl", fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$@" > %q
echo generated-token
`, kubectlArgsPath))

	browserScript := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$@" > %q
exit 0
`, openArgsPath)
	writeExecutable("open", browserScript)
	writeExecutable("xdg-open", browserScript)

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return kubectlArgsPath, openArgsPath
}

func readArgFile(t *testing.T, path string) []string {
	t.Helper()
	var data []byte
	var err error
	for range 50 {
		data, err = os.ReadFile(path)
		if err == nil {
			break
		}
		if errors.Is(err, os.ErrNotExist) {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		t.Fatalf("read %s: %v", path, err)
	}
	if err != nil {
		t.Fatalf("timed out waiting for %s", path)
	}
	content := strings.TrimSpace(string(data))
	if content == "" {
		return nil
	}
	return strings.Split(content, "\n")
}

func TestNewLoginCmd_WithTokenFlag(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	kubectlArgsPath, openArgsPath := setupFakeLoginEnv(t)

	root := newRootCmd()
	root.SetArgs([]string{"login", "--server", "http://test-server:8080", "--token", "test-token-123"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}

	if _, err := os.Stat(kubectlArgsPath); !os.IsNotExist(err) {
		t.Fatalf("expected kubectl not to run when --token is provided")
	}

	openArgs := readArgFile(t, openArgsPath)
	if len(openArgs) == 0 || openArgs[0] != "http://test-server:8080/login#token=test-token-123" {
		t.Fatalf("open args = %v", openArgs)
	}
}

func TestNewLoginCmd_WithoutToken(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	kubectlArgsPath, openArgsPath := setupFakeLoginEnv(t)

	root := newRootCmd()
	root.SetArgs([]string{"login", "--server", "http://test:8080"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}

	kubectlArgs := readArgFile(t, kubectlArgsPath)
	wantKubectlArgs := []string{"create", "token", "default", "-n", defaultNamespace, "--duration=24h"}
	if len(kubectlArgs) != len(wantKubectlArgs) {
		t.Fatalf("kubectl args len = %d, want %d (%v)", len(kubectlArgs), len(wantKubectlArgs), kubectlArgs)
	}
	for i := range wantKubectlArgs {
		if kubectlArgs[i] != wantKubectlArgs[i] {
			t.Fatalf("kubectl arg[%d] = %q, want %q (all: %v)", i, kubectlArgs[i], wantKubectlArgs[i], kubectlArgs)
		}
	}

	openArgs := readArgFile(t, openArgsPath)
	if len(openArgs) == 0 || openArgs[0] != "http://test:8080/login#token=generated-token" {
		t.Fatalf("open args = %v", openArgs)
	}
}

func TestNewLoginCmd_UsesKubeconfigForTokenCreation(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	kubectlArgsPath, _ := setupFakeLoginEnv(t)

	config := clientcmdapi.NewConfig()
	config.CurrentContext = "ctx"
	config.Contexts["ctx"] = &clientcmdapi.Context{
		Cluster:   "c",
		AuthInfo:  "u",
		Namespace: "orka-system",
	}
	config.Clusters["c"] = &clientcmdapi.Cluster{Server: "https://k8s.example.com"}
	config.AuthInfos["u"] = &clientcmdapi.AuthInfo{Token: "kube-token"}
	kubeconfigPath := writeKubeconfig(t, tmp, config)

	root := newRootCmd()
	root.SetArgs([]string{"login", "--server", "http://test:8080", "--kubeconfig", kubeconfigPath})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}

	kubectlArgs := readArgFile(t, kubectlArgsPath)
	wantKubectlArgs := []string{"--kubeconfig", kubeconfigPath, "create", "token", "default", "-n", "orka-system", "--duration=24h"}
	if len(kubectlArgs) != len(wantKubectlArgs) {
		t.Fatalf("kubectl args len = %d, want %d (%v)", len(kubectlArgs), len(wantKubectlArgs), kubectlArgs)
	}
	for i := range wantKubectlArgs {
		if kubectlArgs[i] != wantKubectlArgs[i] {
			t.Fatalf("kubectl arg[%d] = %q, want %q (all: %v)", i, kubectlArgs[i], wantKubectlArgs[i], kubectlArgs)
		}
	}
}

func TestNewLoginCmd_DefaultServerFromConfig(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	_, openArgsPath := setupFakeLoginEnv(t)

	// Save a config with server
	cfg := orkaConfig{Server: "http://configured-server:9090"}
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("saveConfig error: %v", err)
	}

	root := newRootCmd()
	root.SetArgs([]string{"login", "--token", "my-token"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}

	openArgs := readArgFile(t, openArgsPath)
	if len(openArgs) == 0 || openArgs[0] != "http://configured-server:9090/login#token=my-token" {
		t.Fatalf("open args = %v", openArgs)
	}
}
