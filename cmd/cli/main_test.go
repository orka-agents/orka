/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"os"
	"path/filepath"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// ---------------------------------------------------------------------------
// newRootCmd
// ---------------------------------------------------------------------------

func TestNewRootCmd_Structure(t *testing.T) {
	cmd := newRootCmd()

	if cmd.Use != "orka" {
		t.Errorf("Use = %q, want %q", cmd.Use, "orka")
	}
	if cmd.Version != version {
		t.Errorf("Version = %q, want %q", cmd.Version, version)
	}
	if !cmd.SilenceUsage {
		t.Error("expected SilenceUsage to be true")
	}
	if !cmd.SilenceErrors {
		t.Error("expected SilenceErrors to be true")
	}
}

func TestNewRootCmd_Subcommands(t *testing.T) {
	cmd := newRootCmd()

	subNames := make(map[string]bool)
	for _, sub := range cmd.Commands() {
		subNames[sub.Name()] = true
	}

	for _, want := range []string{"login", "run", "config", "agent", "task", "status"} {
		if !subNames[want] {
			t.Errorf("missing subcommand %q", want)
		}
	}
}

func TestNewRootCmd_PersistentFlags(t *testing.T) {
	cmd := newRootCmd()

	for _, flag := range []string{"server", "token", "namespace", "kubeconfig"} {
		if cmd.PersistentFlags().Lookup(flag) == nil {
			t.Errorf("missing persistent flag %q", flag)
		}
	}

	// Check shorthands
	if f := cmd.PersistentFlags().Lookup("server"); f != nil && f.Shorthand != "s" {
		t.Errorf("server shorthand = %q, want %q", f.Shorthand, "s")
	}
	if f := cmd.PersistentFlags().Lookup("token"); f != nil && f.Shorthand != "t" {
		t.Errorf("token shorthand = %q, want %q", f.Shorthand, "t")
	}
	if f := cmd.PersistentFlags().Lookup("namespace"); f != nil && f.Shorthand != "n" {
		t.Errorf("namespace shorthand = %q, want %q", f.Shorthand, "n")
	}
}

func TestNewRootCmd_HelpDoesNotError(t *testing.T) {
	cmd := newRootCmd()
	cmd.SetArgs([]string{"--help"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("help execution error: %v", err)
	}
}

func TestNewRootCmd_VersionFlag(t *testing.T) {
	cmd := newRootCmd()
	cmd.SetArgs([]string{"--version"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("version execution error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// extractKubeContext
// ---------------------------------------------------------------------------

func writeKubeconfig(t *testing.T, dir string, config *clientcmdapi.Config) string {
	t.Helper()
	path := filepath.Join(dir, "kubeconfig")
	if err := clientcmd.WriteToFile(*config, path); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	return path
}

func TestExtractKubeContext_WithToken(t *testing.T) {
	dir := t.TempDir()

	config := clientcmdapi.NewConfig()
	config.CurrentContext = "test-ctx"
	config.Contexts["test-ctx"] = &clientcmdapi.Context{
		Cluster:   "test-cluster",
		AuthInfo:  "test-user",
		Namespace: "my-ns",
	}
	config.Clusters["test-cluster"] = &clientcmdapi.Cluster{
		Server: "https://k8s.example.com",
	}
	config.AuthInfos["test-user"] = &clientcmdapi.AuthInfo{
		Token: "my-secret-token",
	}

	path := writeKubeconfig(t, dir, config)

	kc := extractKubeContext(path)
	if kc.token != "my-secret-token" {
		t.Errorf("token = %q, want %q", kc.token, "my-secret-token")
	}
	if kc.namespace != "my-ns" {
		t.Errorf("namespace = %q, want %q", kc.namespace, "my-ns")
	}
}

func TestExtractKubeContext_NoNamespace(t *testing.T) {
	dir := t.TempDir()

	config := clientcmdapi.NewConfig()
	config.CurrentContext = "test-ctx"
	config.Contexts["test-ctx"] = &clientcmdapi.Context{
		Cluster:  "test-cluster",
		AuthInfo: "test-user",
	}
	config.Clusters["test-cluster"] = &clientcmdapi.Cluster{
		Server: "https://k8s.example.com",
	}
	config.AuthInfos["test-user"] = &clientcmdapi.AuthInfo{
		Token: "tok123",
	}

	path := writeKubeconfig(t, dir, config)

	kc := extractKubeContext(path)
	if kc.namespace != "" {
		t.Errorf("namespace = %q, want empty", kc.namespace)
	}
	if kc.token != "tok123" {
		t.Errorf("token = %q, want %q", kc.token, "tok123")
	}
}

func TestExtractKubeContext_NoCurrentContext(t *testing.T) {
	dir := t.TempDir()

	config := clientcmdapi.NewConfig()
	// No current context set

	path := writeKubeconfig(t, dir, config)

	kc := extractKubeContext(path)
	if kc.token != "" {
		t.Errorf("token = %q, want empty", kc.token)
	}
	if kc.namespace != "" {
		t.Errorf("namespace = %q, want empty", kc.namespace)
	}
}

func TestExtractKubeContext_BadPath(t *testing.T) {
	kc := extractKubeContext("/nonexistent/kubeconfig")
	if kc.token != "" || kc.namespace != "" {
		t.Errorf("expected empty kubeContext for bad path, got %+v", kc)
	}
}

func TestExtractKubeContext_MissingContext(t *testing.T) {
	dir := t.TempDir()

	config := clientcmdapi.NewConfig()
	config.CurrentContext = "missing-context"
	// Context not actually defined

	path := writeKubeconfig(t, dir, config)

	kc := extractKubeContext(path)
	if kc.token != "" {
		t.Errorf("token = %q, want empty for missing context", kc.token)
	}
}

// ---------------------------------------------------------------------------
// extractToken
// ---------------------------------------------------------------------------

func TestExtractToken_WithDirectToken(t *testing.T) {
	dir := t.TempDir()

	config := clientcmdapi.NewConfig()
	config.CurrentContext = "ctx"
	config.Contexts["ctx"] = &clientcmdapi.Context{
		Cluster:  "c",
		AuthInfo: "u",
	}
	config.Clusters["c"] = &clientcmdapi.Cluster{Server: "https://localhost"}
	config.AuthInfos["u"] = &clientcmdapi.AuthInfo{Token: "direct-token"}

	path := writeKubeconfig(t, dir, config)

	tok, err := extractToken(path)
	if err != nil {
		t.Fatalf("extractToken error: %v", err)
	}
	if tok != "direct-token" {
		t.Errorf("token = %q, want %q", tok, "direct-token")
	}
}

func TestExtractToken_WithTokenFile(t *testing.T) {
	dir := t.TempDir()

	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte("file-token"), 0o600); err != nil {
		t.Fatal(err)
	}

	config := clientcmdapi.NewConfig()
	config.CurrentContext = "ctx"
	config.Contexts["ctx"] = &clientcmdapi.Context{
		Cluster:  "c",
		AuthInfo: "u",
	}
	config.Clusters["c"] = &clientcmdapi.Cluster{Server: "https://localhost"}
	config.AuthInfos["u"] = &clientcmdapi.AuthInfo{TokenFile: tokenFile}

	path := writeKubeconfig(t, dir, config)

	tok, err := extractToken(path)
	if err != nil {
		t.Fatalf("extractToken error: %v", err)
	}
	if tok != "file-token" {
		t.Errorf("token = %q, want %q", tok, "file-token")
	}
}

func TestExtractToken_NoToken(t *testing.T) {
	dir := t.TempDir()

	config := clientcmdapi.NewConfig()
	config.CurrentContext = "ctx"
	config.Contexts["ctx"] = &clientcmdapi.Context{
		Cluster:  "c",
		AuthInfo: "u",
	}
	config.Clusters["c"] = &clientcmdapi.Cluster{Server: "https://localhost"}
	config.AuthInfos["u"] = &clientcmdapi.AuthInfo{}

	path := writeKubeconfig(t, dir, config)

	_, err := extractToken(path)
	if err == nil {
		t.Error("expected error when no token source is available")
	}
}

func TestExtractToken_NoCurrentContext(t *testing.T) {
	dir := t.TempDir()

	config := clientcmdapi.NewConfig()

	path := writeKubeconfig(t, dir, config)

	_, err := extractToken(path)
	if err == nil {
		t.Error("expected error for no current context")
	}
}

func TestExtractToken_MissingUser(t *testing.T) {
	dir := t.TempDir()

	config := clientcmdapi.NewConfig()
	config.CurrentContext = "ctx"
	config.Contexts["ctx"] = &clientcmdapi.Context{
		Cluster:  "c",
		AuthInfo: "nonexistent-user",
	}
	config.Clusters["c"] = &clientcmdapi.Cluster{Server: "https://localhost"}

	path := writeKubeconfig(t, dir, config)

	_, err := extractToken(path)
	if err == nil {
		t.Error("expected error for missing user")
	}
}

func TestExtractToken_BadTokenFile(t *testing.T) {
	dir := t.TempDir()

	config := clientcmdapi.NewConfig()
	config.CurrentContext = "ctx"
	config.Contexts["ctx"] = &clientcmdapi.Context{
		Cluster:  "c",
		AuthInfo: "u",
	}
	config.Clusters["c"] = &clientcmdapi.Cluster{Server: "https://localhost"}
	config.AuthInfos["u"] = &clientcmdapi.AuthInfo{TokenFile: "/nonexistent/token"}

	path := writeKubeconfig(t, dir, config)

	_, err := extractToken(path)
	if err == nil {
		t.Error("expected error for bad token file")
	}
}

func TestExtractToken_BadPath(t *testing.T) {
	_, err := extractToken("/nonexistent/kubeconfig")
	if err == nil {
		t.Error("expected error for nonexistent kubeconfig")
	}
}

func TestExtractToken_MissingContext(t *testing.T) {
	dir := t.TempDir()

	config := clientcmdapi.NewConfig()
	config.CurrentContext = "missing"

	path := writeKubeconfig(t, dir, config)

	_, err := extractToken(path)
	if err == nil {
		t.Error("expected error for missing context")
	}
}

// ---------------------------------------------------------------------------
// openBrowser
// ---------------------------------------------------------------------------

func TestOpenBrowser_DoesNotPanic(t *testing.T) {
	// We can't really test browser opening, but ensure it doesn't panic
	err := openBrowser("http://example.com")
	// May succeed or fail depending on platform — just check no panic
	_ = err
}

// ---------------------------------------------------------------------------
// defaultServer constant
// ---------------------------------------------------------------------------

func TestDefaultServer(t *testing.T) {
	if defaultServer != "http://localhost:8080" {
		t.Errorf("defaultServer = %q, want %q", defaultServer, "http://localhost:8080")
	}
}
