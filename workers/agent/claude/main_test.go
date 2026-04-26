/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"slices"
	"testing"

	"github.com/sozercan/orka/workers/common"
)

func TestBuildClaudeArgs_Minimal(t *testing.T) {
	t.Setenv("ORKA_ALLOW_BASH", "")

	cfg := &common.AgentConfig{
		Prompt:   "hello world",
		MaxTurns: 50,
	}

	args := buildClaudeArgs(cfg, cfg.Prompt)

	// Must contain --print, --verbose, --max-turns, and prompt
	assertContains(t, args, "--print")
	assertContains(t, args, "--verbose")
	assertContains(t, args, "--max-turns")
	assertContains(t, args, "50")

	// Prompt must be the last arg
	if args[len(args)-1] != "hello world" {
		t.Errorf("prompt should be last arg, got %q", args[len(args)-1])
	}
}

func TestBuildClaudeArgs_Full(t *testing.T) {
	t.Setenv("ORKA_ALLOW_BASH", "")

	cfg := &common.AgentConfig{
		Prompt:          "fix bugs",
		Model:           "claude-sonnet-4-20250514",
		SystemPrompt:    "You are a code reviewer",
		MaxTurns:        100,
		AllowedTools:    []string{"Read", "Write"},
		DisallowedTools: []string{"Bash"},
	}

	args := buildClaudeArgs(cfg, cfg.Prompt)

	assertContains(t, args, "--model")
	assertContains(t, args, "claude-sonnet-4-20250514")
	assertContains(t, args, "--append-system-prompt")
	assertContains(t, args, "You are a code reviewer")
	assertContains(t, args, "--max-turns")
	assertContains(t, args, "100")
	assertContains(t, args, "--allowedTools")
	assertContains(t, args, "Read")
	assertContains(t, args, "Write")
	assertContains(t, args, "--disallowedTools")
	assertContains(t, args, "Bash")

	// Prompt must be the last arg
	if args[len(args)-1] != "fix bugs" {
		t.Errorf("prompt should be last arg, got %q", args[len(args)-1])
	}

	// --dangerously-skip-permissions should NOT be present
	if slices.Contains(args, "--dangerously-skip-permissions") {
		t.Error("--dangerously-skip-permissions should not be set when ORKA_ALLOW_BASH is not true")
	}
}

func TestBuildClaudeArgs_AllowBash(t *testing.T) {
	t.Setenv("ORKA_ALLOW_BASH", "true")

	cfg := &common.AgentConfig{
		Prompt:   "hello",
		MaxTurns: 50,
	}

	args := buildClaudeArgs(cfg, cfg.Prompt)
	assertContains(t, args, "--dangerously-skip-permissions")
}

func TestClaudePath_Default(t *testing.T) {
	t.Setenv("CLAUDE_CLI_PATH", "")

	if p := claudePath(); p != defaultClaudePath {
		t.Errorf("claudePath() = %q, want %q", p, defaultClaudePath)
	}
}

func TestClaudePath_Override(t *testing.T) {
	t.Setenv("CLAUDE_CLI_PATH", "/usr/local/bin/claude")

	if p := claudePath(); p != "/usr/local/bin/claude" {
		t.Errorf("claudePath() = %q, want /usr/local/bin/claude", p)
	}
}

// assertContains checks that a string slice contains the given value.
func assertContains(t *testing.T, s []string, val string) {
	t.Helper()
	if slices.Contains(s, val) {
		return
	}
	t.Errorf("expected args to contain %q, got %v", val, s)
}

func TestExecuteClaude_NonexistentBinary(t *testing.T) {
	t.Setenv("CLAUDE_CLI_PATH", "/nonexistent/claude-cli")
	t.Setenv("ORKA_ALLOW_BASH", "")

	cfg := &common.AgentConfig{
		Prompt:   "test prompt",
		MaxTurns: 5,
	}

	_, err := executeClaude(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error when claude binary doesn't exist")
	}
}

func TestExecuteClaude_WithTimeout(t *testing.T) {
	t.Setenv("CLAUDE_CLI_PATH", "/nonexistent/claude-cli")
	t.Setenv("ORKA_ALLOW_BASH", "")

	cfg := &common.AgentConfig{
		Prompt:         "test prompt",
		MaxTurns:       5,
		TimeoutSeconds: 1,
	}

	_, err := executeClaude(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error when claude binary doesn't exist")
	}
}

func TestExecuteClaude_CancelledContext(t *testing.T) {
	t.Setenv("CLAUDE_CLI_PATH", "/nonexistent/claude-cli")
	t.Setenv("ORKA_ALLOW_BASH", "")

	cfg := &common.AgentConfig{
		Prompt:   "test prompt",
		MaxTurns: 5,
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := executeClaude(ctx, cfg)
	if err == nil {
		t.Fatal("expected error with cancelled context")
	}
}

func TestExecuteClaude_WithSubPath(t *testing.T) {
	t.Setenv("CLAUDE_CLI_PATH", "/nonexistent/claude-cli")
	t.Setenv("ORKA_ALLOW_BASH", "")

	cfg := &common.AgentConfig{
		Prompt:   "test",
		MaxTurns: 5,
		SubPath:  "src",
	}

	_, err := executeClaude(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error when claude binary doesn't exist")
	}
}

func TestBuildClaudeArgs_NoTools(t *testing.T) {
	t.Setenv("ORKA_ALLOW_BASH", "")

	cfg := &common.AgentConfig{
		Prompt:   "do something",
		MaxTurns: 10,
	}

	args := buildClaudeArgs(cfg, cfg.Prompt)

	assertContains(t, args, "--print")
	assertContains(t, args, "--verbose")
	assertContains(t, args, "--max-turns")
	assertContains(t, args, "10")

	// Should not have tool flags
	if slices.Contains(args, "--allowedTools") {
		t.Error("should not contain --allowedTools when none specified")
	}
	if slices.Contains(args, "--disallowedTools") {
		t.Error("should not contain --disallowedTools when none specified")
	}
	if slices.Contains(args, "--dangerously-skip-permissions") {
		t.Error("should not contain --dangerously-skip-permissions")
	}
}

func TestBuildClaudeArgs_PromptIsLast(t *testing.T) {
	t.Setenv("ORKA_ALLOW_BASH", "true")

	cfg := &common.AgentConfig{
		Prompt:          "my prompt text",
		Model:           "claude-sonnet-4-20250514",
		SystemPrompt:    "system",
		MaxTurns:        25,
		AllowedTools:    []string{"Read"},
		DisallowedTools: []string{"Bash"},
	}

	args := buildClaudeArgs(cfg, cfg.Prompt)

	// Prompt uses -p flag, so prompt should be last arg
	if args[len(args)-1] != "my prompt text" {
		t.Errorf("prompt should be last arg, got %q", args[len(args)-1])
	}
	// -p flag should be second to last
	if args[len(args)-2] != "-p" {
		t.Errorf("expected -p flag before prompt, got %q", args[len(args)-2])
	}
}
