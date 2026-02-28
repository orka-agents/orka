/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"testing"
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
	if flag.DefValue != defaultNamespace {
		t.Errorf("service-account default = %q, want %q", flag.DefValue, defaultNamespace)
	}
}

func TestNewLoginCmd_WithTokenFlag(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	cmd := newLoginCmd()

	// Create a root command to provide persistent flags
	root := newRootCmd()
	root.AddCommand(cmd)

	// With --token provided, it should try to construct URL and open browser.
	// The openBrowser will fail in test, but the command should return nil.
	root.SetArgs([]string{"login", "--server", "http://test-server:8080", "--token", "test-token-123"})

	err := root.Execute()
	// openBrowser may fail, but the cmd returns nil on browser error
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestNewLoginCmd_WithoutToken(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

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

	// Save a config with server
	cfg := orkaConfig{Server: "http://configured-server:9090"}
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("saveConfig error: %v", err)
	}

	cmd := newLoginCmd()
	root := newRootCmd()
	root.AddCommand(cmd)

	root.SetArgs([]string{"login", "--token", "my-token"})

	// Should use configured server. openBrowser may fail but command returns nil.
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}
