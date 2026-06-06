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
	assertContains(t, args, "--tools")
	assertContains(t, args, "Read,Write")
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

func TestBuildClaudeArgs_ScopesReadOnlyPermissions(t *testing.T) {
	t.Setenv("ORKA_ALLOW_BASH", "")
	t.Setenv("ORKA_CLAUDE_BARE", "true")
	t.Setenv("ORKA_CLAUDE_DISABLE_SETTING_SOURCES", "true")
	t.Setenv("ORKA_CLAUDE_PERMISSION_MODE", "dontAsk")

	cfg := &common.AgentConfig{
		Prompt:   "review",
		MaxTurns: 50,
		AllowedTools: []string{
			"Read(/workspace/**)",
			"Glob(/workspace/**)",
			"Grep(/workspace/**)",
			"LS(/workspace/**)",
			"Read(/workspace/**)",
			" ",
		},
		DisallowedTools: []string{
			"Bash",
			"Read(/proc/**)",
			" ",
		},
	}

	args := buildClaudeArgs(cfg, cfg.Prompt)

	assertContains(t, args, "--bare")
	assertFlagValue(t, args, "--setting-sources", "")
	assertContains(t, args, "--permission-mode")
	assertContains(t, args, "dontAsk")
	assertContains(t, args, "--tools")
	assertContains(t, args, "Read,Glob,Grep,LS")
	assertContains(t, args, "--allowedTools")
	assertContains(t, args, "Read(/workspace/**)")
	assertContains(t, args, "Glob(/workspace/**)")
	assertContains(t, args, "Grep(/workspace/**)")
	assertContains(t, args, "LS(/workspace/**)")
	assertContains(t, args, "--disallowedTools")
	assertContains(t, args, "Bash")
	assertContains(t, args, "Read(/proc/**)")
	if slices.Contains(args, "Read(/workspace/**),Glob(/workspace/**),Grep(/workspace/**),LS(/workspace/**)") {
		t.Fatal("--tools should receive bare tool names, not scoped permission specs")
	}
}

func TestClaudeAvailableToolsDeduplicatesScopedToolSpecs(t *testing.T) {
	got := claudeAvailableTools([]string{
		"Read(/workspace/**)",
		"Read(/workspace/src/**)",
		"Glob(/workspace/**)",
		"",
		"  ",
		"LS",
	})
	want := []string{"Read", "Glob", "LS"}
	if !slices.Equal(got, want) {
		t.Fatalf("claudeAvailableTools() = %#v, want %#v", got, want)
	}
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
	if slices.Contains(args, "--tools") {
		t.Error("should not contain --tools when no tools are specified")
	}
	if slices.Contains(args, "--bare") {
		t.Error("should not contain --bare when unset")
	}
	if slices.Contains(args, "--setting-sources") {
		t.Error("should not contain --setting-sources when unset")
	}
	if slices.Contains(args, "--permission-mode") {
		t.Error("should not contain --permission-mode when unset")
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

func assertFlagValue(t *testing.T, args []string, flag, want string) {
	t.Helper()
	idx := slices.Index(args, flag)
	if idx == -1 {
		t.Fatalf("expected args to contain %q, got %v", flag, args)
	}
	if idx+1 >= len(args) {
		t.Fatalf("expected %q to have value %q, got no following arg in %v", flag, want, args)
	}
	if got := args[idx+1]; got != want {
		t.Fatalf("%s value = %q, want %q", flag, got, want)
	}
}
