/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"slices"
	"testing"
	"time"
)

func TestFormatAge(t *testing.T) {
	tests := []struct {
		name      string
		timestamp string
		wantExact string // exact match, or "" if we just want a suffix check
		wantSfx   string // suffix to check
	}{
		{"empty", "", "<unknown>", ""},
		{"invalid", "not-a-date", "not-a-date", ""},
		{"seconds_ago", time.Now().Add(-30 * time.Second).Format(time.RFC3339), "", "s"},
		{"minutes_ago", time.Now().Add(-5 * time.Minute).Format(time.RFC3339), "", "m"},
		{"hours_ago", time.Now().Add(-3 * time.Hour).Format(time.RFC3339), "", "h"},
		{"days_ago", time.Now().Add(-48 * time.Hour).Format(time.RFC3339), "", "d"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatAge(tt.timestamp)
			if tt.wantExact != "" {
				if got != tt.wantExact {
					t.Errorf("formatAge(%q) = %q, want %q", tt.timestamp, got, tt.wantExact)
				}
			} else if tt.wantSfx != "" {
				if len(got) == 0 {
					t.Errorf("formatAge(%q) returned empty string", tt.timestamp)
				} else if got[len(got)-1:] != tt.wantSfx {
					t.Errorf("formatAge(%q) = %q, want suffix %q", tt.timestamp, got, tt.wantSfx)
				}
			}
		})
	}
}

func TestNewTaskCmd(t *testing.T) {
	cmd := newTaskCmd()

	if cmd.Use != "task" {
		t.Errorf("Use = %q, want %q", cmd.Use, "task")
	}

	// Verify subcommands exist
	subNames := make(map[string]bool)
	for _, sub := range cmd.Commands() {
		subNames[sub.Use] = true
	}
	for _, want := range []string{"create <prompt>", "list", "get <name>", "logs <name>", "delete <name>"} {
		if !subNames[want] {
			t.Errorf("missing subcommand %q", want)
		}
	}
}

func TestNewTaskCreateCmdFlags(t *testing.T) {
	cmd := newTaskCreateCmd()

	// Verify flags
	for _, flagName := range []string{"type", "agent", "provider", "timeout"} {
		if cmd.Flags().Lookup(flagName) == nil {
			t.Errorf("missing flag %q", flagName)
		}
	}

	// Verify default values
	typeVal, _ := cmd.Flags().GetString("type")
	if typeVal != "ai" {
		t.Errorf("default type = %q, want %q", typeVal, "ai")
	}
	providerVal, _ := cmd.Flags().GetString("provider")
	if providerVal != "default" {
		t.Errorf("default provider = %q, want %q", providerVal, "default")
	}
}

func TestNewTaskListCmdAliases(t *testing.T) {
	cmd := newTaskListCmd()

	found := slices.Contains(cmd.Aliases, "ls")
	if !found {
		t.Error("expected 'ls' alias on list command")
	}

	// Verify flags
	if cmd.Flags().Lookup("status") == nil {
		t.Error("missing flag 'status'")
	}
	if cmd.Flags().Lookup("limit") == nil {
		t.Error("missing flag 'limit'")
	}
}

func TestNewTaskGetCmdArgs(t *testing.T) {
	cmd := newTaskGetCmd()
	cmd.SetArgs([]string{})
	// Without args, should error (requires exactly 1)
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error when no args provided")
	}
}

func TestNewTaskDeleteCmdAliases(t *testing.T) {
	cmd := newTaskDeleteCmd()
	found := slices.Contains(cmd.Aliases, "rm")
	if !found {
		t.Error("expected 'rm' alias on delete command")
	}
}

func TestNewTaskLogsCmdFlags(t *testing.T) {
	cmd := newTaskLogsCmd()

	flag := cmd.Flags().Lookup("follow")
	if flag == nil {
		t.Error("missing flag 'follow'")
	}
	if flag.Shorthand != "f" {
		t.Errorf("follow shorthand = %q, want %q", flag.Shorthand, "f")
	}
}
