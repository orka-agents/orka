/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"testing"
)

func TestNewConfigCmd(t *testing.T) {
	cmd := newConfigCmd()

	if cmd.Use != "config" {
		t.Errorf("Use = %q, want %q", cmd.Use, "config")
	}

	subNames := make(map[string]bool)
	for _, sub := range cmd.Commands() {
		subNames[sub.Use] = true
	}
	for _, want := range []string{"set-server <url>", "set-token <token>", "view"} {
		if !subNames[want] {
			t.Errorf("missing subcommand %q", want)
		}
	}
}

func TestConfigSetServerCmd(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	cmd := newConfigSetServerCmd()
	cmd.SetArgs([]string{"http://my-server.local:8080"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}

	cfg := loadConfig()
	if cfg.Server != "http://my-server.local:8080" {
		t.Errorf("Server = %q, want %q", cfg.Server, "http://my-server.local:8080")
	}
}

func TestConfigSetServerCmdNoArgs(t *testing.T) {
	cmd := newConfigSetServerCmd()
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err == nil {
		t.Error("expected error with no args")
	}
}

func TestConfigSetTokenCmd(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	cmd := newConfigSetTokenCmd()
	cmd.SetArgs([]string{"my-secret-token"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}

	cfg := loadConfig()
	if cfg.Token != "my-secret-token" {
		t.Errorf("Token = %q, want %q", cfg.Token, "my-secret-token")
	}
}

func TestConfigSetTokenCmdNoArgs(t *testing.T) {
	cmd := newConfigSetTokenCmd()
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err == nil {
		t.Error("expected error with no args")
	}
}

func TestConfigViewCmd(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	// Save a config first
	cfg := orkaConfig{
		Server:    "http://test:8080",
		Token:     "abcdef12345",
		Namespace: "test-ns",
	}
	if err := saveConfig(cfg); err != nil {
		t.Fatalf("saveConfig() error: %v", err)
	}

	cmd := newConfigViewCmd()
	// Execute should not error
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestConfigViewCmdEmptyConfig(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	cmd := newConfigViewCmd()
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
}

func TestConfigSetServerPreservesToken(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	// Set token first
	tokenCmd := newConfigSetTokenCmd()
	tokenCmd.SetArgs([]string{"my-token"})
	if err := tokenCmd.Execute(); err != nil {
		t.Fatalf("set-token error: %v", err)
	}

	// Set server
	serverCmd := newConfigSetServerCmd()
	serverCmd.SetArgs([]string{"http://srv:8080"})
	if err := serverCmd.Execute(); err != nil {
		t.Fatalf("set-server error: %v", err)
	}

	// Verify both are preserved
	cfg := loadConfig()
	if cfg.Token != "my-token" {
		t.Errorf("Token = %q, want %q (should be preserved)", cfg.Token, "my-token")
	}
	if cfg.Server != "http://srv:8080" {
		t.Errorf("Server = %q, want %q", cfg.Server, "http://srv:8080")
	}
}
