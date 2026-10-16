/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// stubDashboardProbe reports a served dashboard without the network.
func stubDashboardProbe(t *testing.T) {
	t.Helper()
	orig := dashboardAbsentFunc
	dashboardAbsentFunc = func(string) bool { return false }
	t.Cleanup(func() { dashboardAbsentFunc = orig })
}

func TestDashboardAbsent(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		want   bool
	}{
		{"dashboard served", http.StatusOK, false},
		{"no dashboard", http.StatusNotFound, true},
		{"sign-in proxy", http.StatusUnauthorized, false},
		{"server error", http.StatusBadGateway, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/login" {
					t.Errorf("probe path = %q, want /login", r.URL.Path)
				}
				w.WriteHeader(tt.status)
			}))
			defer server.Close()
			if got := dashboardAbsent(server.URL); got != tt.want {
				t.Fatalf("dashboardAbsent = %t, want %t", got, tt.want)
			}
		})
	}
	// A gateway that sends /login to its own sign-in is judged by that
	// redirect, even when the page it leads to is missing.
	gateway := http.NewServeMux()
	gateway.Handle("/login", http.RedirectHandler("/sso/start", http.StatusFound))
	gateway.Handle("/sso/start", http.NotFoundHandler())
	sso := httptest.NewServer(gateway)
	defer sso.Close()
	if dashboardAbsent(sso.URL) {
		t.Fatal("a redirecting sign-in gateway must not count as a server without a dashboard")
	}
	// An unreachable server says nothing either way.
	if dashboardAbsent("http://127.0.0.1:1") {
		t.Fatal("an unreachable server must not count as one without a dashboard")
	}
}

func TestNewLoginCmd_RefusesServerWithoutDashboard(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	minted := false
	origCreate := serviceAccountLoginFunc
	serviceAccountLoginFunc = func(string, string) (string, error) {
		minted = true
		return "opaque-login-value-000", nil
	}
	t.Cleanup(func() { serviceAccountLoginFunc = origCreate })
	opened := false
	origBrowser := openBrowserFunc
	openBrowserFunc = func(string) error {
		opened = true
		return nil
	}
	t.Cleanup(func() { openBrowserFunc = origBrowser })

	root := newRootCmd()
	root.SetArgs([]string{"login", "--server", server.URL})
	stdout, err := captureOutput(t, root.Execute)
	if err == nil || !strings.Contains(err.Error(), "does not serve the dashboard") || !strings.Contains(err.Error(), "--dashboard-enabled") {
		t.Fatalf("Execute() error = %v, want a missing-dashboard error", err)
	}
	if minted || opened {
		t.Fatalf("token minted = %t, browser opened = %t; neither may happen without a dashboard", minted, opened)
	}
	if strings.Contains(stdout, "Login URL") {
		t.Fatalf("stdout = %q, must not print a login URL", stdout)
	}
}

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
	if flag.DefValue != defaultNamespace {
		t.Errorf("service-account default = %q, want %q", flag.DefValue, defaultNamespace)
	}
}

func TestNewLoginCmd_WithTokenFlag(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	stubDashboardProbe(t)

	// Stub out the browser opener so we never launch a real browser.
	orig := openBrowserFunc
	openBrowserFunc = func(string) error { return nil }
	t.Cleanup(func() { openBrowserFunc = orig })

	cmd := newLoginCmd()

	// Create a root command to provide persistent flags
	root := newRootCmd()
	root.AddCommand(cmd)

	// With --token provided, it should try to construct URL and open browser.
	root.SetArgs([]string{"login", "--server", "http://test-server:8080", "--token", "opaque-login-value-123"})

	err := root.Execute()
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestNewLoginCmd_WithoutToken(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	stubDashboardProbe(t)

	cmd := newLoginCmd()
	root := newRootCmd()
	root.AddCommand(cmd)

	// Without --token, it will try createServiceAccountToken which needs kubectl.
	// This should error in test environment.
	root.SetArgs([]string{"login", "--server", "http://test:8080"})

	err := root.Execute()
	// Expect error since kubectl is likely not available or service account doesn't exist
	if err == nil {
		t.Log("login without token succeeded (kubectl available)")
	}
}

func TestNewLoginCmd_DefaultServerFromConfig(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	stubDashboardProbe(t)

	// Stub out the browser opener so we never launch a real browser.
	orig := openBrowserFunc
	openBrowserFunc = func(string) error { return nil }
	t.Cleanup(func() { openBrowserFunc = orig })

	// Save a config with server
	cfg := orkaConfig{Server: "http://configured-server:9090"}
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("saveConfig error: %v", err)
	}

	cmd := newLoginCmd()
	root := newRootCmd()
	root.AddCommand(cmd)

	root.SetArgs([]string{"login", "--token", "opaque-value-456"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestNewLoginCmd_UsesConfiguredNamespaceForCreatedToken(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	stubDashboardProbe(t)

	cfg := orkaConfig{Server: "http://configured-server:9090", Namespace: "orka-system"}
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("saveConfig error: %v", err)
	}

	var gotServiceAccount, gotNamespace string
	origCreate := serviceAccountLoginFunc
	serviceAccountLoginFunc = func(serviceAccount, namespace string) (string, error) {
		gotServiceAccount = serviceAccount
		gotNamespace = namespace
		return "opaque-login-value-789", nil
	}
	t.Cleanup(func() { serviceAccountLoginFunc = origCreate })

	origBrowser := openBrowserFunc
	openBrowserFunc = func(string) error { return nil }
	t.Cleanup(func() { openBrowserFunc = origBrowser })

	root := newRootCmd()
	root.SetArgs([]string{"login", "--service-account", "orka-client"})

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if gotServiceAccount != "orka-client" {
		t.Fatalf("serviceAccount = %q, want orka-client", gotServiceAccount)
	}
	if gotNamespace != "orka-system" {
		t.Fatalf("namespace = %q, want configured namespace", gotNamespace)
	}
}

func TestNewLoginCmd_NoOpenRedactsTokenInOutput(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	stubDashboardProbe(t)

	opened := false
	orig := openBrowserFunc
	openBrowserFunc = func(string) error {
		opened = true
		return nil
	}
	t.Cleanup(func() { openBrowserFunc = orig })

	root := newRootCmd()
	root.SetArgs([]string{
		"login",
		"--server", "http://test-server:8080",
		"--token", "opaque-login-value-123",
		"--no-open",
		"--redact-token",
	})

	stdout, err := captureOutput(t, root.Execute)
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if opened {
		t.Fatal("browser opener was called despite --no-open")
	}
	if strings.Contains(stdout, "opaque-login-value-123") {
		t.Fatalf("stdout leaked token: %q", stdout)
	}
	if !strings.Contains(stdout, "Login URL: http://test-server:8080/login#token=<redacted>") {
		t.Fatalf("stdout = %q, want redacted login URL", stdout)
	}
	if !strings.Contains(stdout, "printed login URL is redacted") {
		t.Fatalf("stdout = %q, want redacted no-open guidance", stdout)
	}
	if strings.Contains(stdout, "Use the login URL above") {
		t.Fatalf("stdout = %q, should not suggest using the redacted URL", stdout)
	}
}

func TestNewLoginCmd_RedactsOutputButOpensFullURL(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	stubDashboardProbe(t)

	var openedURL string
	orig := openBrowserFunc
	openBrowserFunc = func(url string) error {
		openedURL = url
		return nil
	}
	t.Cleanup(func() { openBrowserFunc = orig })

	root := newRootCmd()
	root.SetArgs([]string{
		"login",
		"--server", "http://test-server:8080",
		"--token", "opaque-login-value-123",
		"--redact-token",
	})

	stdout, err := captureOutput(t, root.Execute)
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if openedURL != "http://test-server:8080/login#token=opaque-login-value-123" {
		t.Fatalf("openedURL = %q, want full token URL", openedURL)
	}
	if strings.Contains(stdout, "opaque-login-value-123") {
		t.Fatalf("stdout leaked token: %q", stdout)
	}
	if !strings.Contains(stdout, "Login URL: http://test-server:8080/login#token=<redacted>") {
		t.Fatalf("stdout = %q, want redacted login URL", stdout)
	}
}

func TestNewLoginCmd_RedactedBrowserFailureExplainsURLIsNotUsable(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	stubDashboardProbe(t)

	orig := openBrowserFunc
	openBrowserFunc = func(string) error { return errors.New("browser unavailable") }
	t.Cleanup(func() { openBrowserFunc = orig })

	root := newRootCmd()
	root.SetArgs([]string{
		"login",
		"--server", "http://test-server:8080",
		"--token", "opaque-login-value-123",
		"--redact-token",
	})

	stderr, err := captureStderr(t, root.Execute)
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if !strings.Contains(stderr, "printed login URL is redacted") {
		t.Fatalf("stderr = %q, want redacted URL guidance", stderr)
	}
	if strings.Contains(stderr, "Open the URL above") {
		t.Fatalf("stderr = %q, should not suggest opening the redacted URL", stderr)
	}
}

func captureStderr(t *testing.T, fn func() error) (string, error) {
	t.Helper()

	oldStderr := os.Stderr
	stderrReader, stderrWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("failed to create stderr pipe: %v", err)
	}

	os.Stderr = stderrWriter
	defer func() {
		os.Stderr = oldStderr
		stderrReader.Close() //nolint:errcheck
	}()

	runErr := fn()
	stderrWriter.Close() //nolint:errcheck

	stderr, err := io.ReadAll(stderrReader)
	if err != nil {
		t.Fatalf("failed to read captured stderr: %v", err)
	}
	return string(stderr), runErr
}
