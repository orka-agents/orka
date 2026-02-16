/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"testing"
)

func TestNewAgentCmd(t *testing.T) {
	cmd := newAgentCmd()

	if cmd.Use != "agent" {
		t.Errorf("Use = %q, want %q", cmd.Use, "agent")
	}
	if cmd.Short != "Manage agents" {
		t.Errorf("Short = %q, want %q", cmd.Short, "Manage agents")
	}

	subNames := make(map[string]bool)
	for _, sub := range cmd.Commands() {
		subNames[sub.Use] = true
	}
	for _, want := range []string{"list", "get <name>", "delete <name>"} {
		if !subNames[want] {
			t.Errorf("missing subcommand %q", want)
		}
	}
}

func TestNewAgentGetCmdRequiresArgs(t *testing.T) {
	cmd := newAgentGetCmd()
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err == nil {
		t.Error("expected error when no args")
	}
}

func TestNewAgentDeleteCmdRequiresArgs(t *testing.T) {
	cmd := newAgentDeleteCmd()
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err == nil {
		t.Error("expected error when no args")
	}
}

func TestNewAgentListCmdStructure(t *testing.T) {
	cmd := newAgentListCmd()

	if cmd.Use != "list" {
		t.Errorf("Use = %q, want %q", cmd.Use, "list")
	}
	if cmd.Short != "List agents" {
		t.Errorf("Short = %q, want %q", cmd.Short, "List agents")
	}
}
