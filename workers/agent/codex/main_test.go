/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sozercan/orka/workers/common"
)

func TestBuildCodexArgs_Minimal(t *testing.T) {
	t.Setenv("ORKA_ALLOW_BASH", "true")
	t.Setenv("OPENAI_BASE_URL", "")
	t.Setenv("ORKA_CODEX_SANDBOX_MODE", "")

	cfg := &common.AgentConfig{
		Prompt:   "hello world",
		MaxTurns: 50,
	}

	args := buildCodexArgs(cfg, "/tmp/result.txt", "")

	assertContains(t, args, "exec")
	assertContains(t, args, "--skip-git-repo-check")
	assertContains(t, args, "--ephemeral")
	assertContains(t, args, "--output-last-message")
	assertContains(t, args, "/tmp/result.txt")
	assertContains(t, args, "--sandbox")
	assertContains(t, args, "workspace-write")
	assertContains(t, args, "--config")
	assertContains(t, args, "approval_policy=never")
	assertContains(t, args, "model_auto_compact_token_limit=240000")
	assertContains(t, args, "sandbox_workspace_write.network_access=true")

	if args[len(args)-1] != "-" {
		t.Errorf("prompt sentinel should be last arg, got %q", args[len(args)-1])
	}
}

func TestBuildCodexArgs_UsesConfiguredSandboxMode(t *testing.T) {
	t.Setenv("ORKA_ALLOW_BASH", "true")
	t.Setenv("OPENAI_BASE_URL", "")
	t.Setenv("ORKA_CODEX_SANDBOX_MODE", "danger-full-access")

	cfg := &common.AgentConfig{
		Prompt:   "hello world",
		MaxTurns: 50,
	}

	args := buildCodexArgs(cfg, "/tmp/result.txt", "")

	assertContains(t, args, "--sandbox")
	assertContains(t, args, "danger-full-access")
	assertNotContains(t, args, "workspace-write")
	assertNotContains(t, args, "sandbox_workspace_write.network_access=true")
}

func TestBuildCodexArgs_Full(t *testing.T) {
	t.Setenv("ORKA_ALLOW_BASH", "true")
	t.Setenv("OPENAI_BASE_URL", "https://example.invalid/v1")
	t.Setenv("ORKA_CODEX_SANDBOX_MODE", "")

	cfg := &common.AgentConfig{
		Prompt:          "fix bugs",
		Model:           "gpt-5.4",
		SystemPrompt:    "You are a code reviewer",
		MaxTurns:        100,
		AllowedTools:    []string{"Read", "WebSearch"},
		DisallowedTools: []string{"Shell"},
	}

	args := buildCodexArgs(cfg, "/tmp/result.txt", "/tmp/instructions.md")

	assertContains(t, args, "--model")
	assertContains(t, args, "gpt-5.4")
	assertContains(t, args, "model_instructions_file=/tmp/instructions.md")
	assertContains(t, args, "openai_base_url=https://example.invalid/v1")
	assertContains(t, args, "sandbox_workspace_write.network_access=true")
	assertContains(t, args, "web_search=live")
}

func TestBuildCodexArgs_UsesConfiguredAutoCompactLimit(t *testing.T) {
	t.Setenv("ORKA_ALLOW_BASH", "true")
	t.Setenv("ORKA_CODEX_AUTO_COMPACT_TOKEN_LIMIT", "200000")

	cfg := &common.AgentConfig{Prompt: "test"}

	args := buildCodexArgs(cfg, "/tmp/result.txt", "")
	assertContains(t, args, "model_auto_compact_token_limit=200000")
}

func TestBuildCodexArgs_WebSearchDisabled(t *testing.T) {
	t.Setenv("ORKA_ALLOW_BASH", "true")

	cfg := &common.AgentConfig{
		Prompt:       "test",
		AllowedTools: []string{"Read", "Write"},
	}

	args := buildCodexArgs(cfg, "/tmp/result.txt", "")
	assertContains(t, args, "web_search=disabled")
}

func TestBuildCodexInstructions(t *testing.T) {
	t.Setenv("ORKA_ALLOW_BASH", "")

	cfg := &common.AgentConfig{
		SystemPrompt:    "You are helpful.",
		MaxTurns:        12,
		AllowedTools:    []string{" Read ", "Write"},
		DisallowedTools: []string{" Bash "},
	}

	instructions := buildCodexInstructions(cfg)

	if !strings.Contains(instructions, "You are helpful.") {
		t.Fatalf("expected system prompt in instructions, got %q", instructions)
	}
	if !strings.Contains(instructions, "within 12 turns") {
		t.Errorf("expected max turns guidance, got %q", instructions)
	}
	if !strings.Contains(instructions, "Read, Write") {
		t.Errorf("expected allowlist guidance, got %q", instructions)
	}
	if !strings.Contains(instructions, "Do not use these tools: Bash.") {
		t.Errorf("expected disallow guidance, got %q", instructions)
	}
}

func TestCodexPath_Default(t *testing.T) {
	t.Setenv("CODEX_CLI_PATH", "")

	if p := codexPath(); p != defaultCodexPath {
		t.Errorf("codexPath() = %q, want %q", p, defaultCodexPath)
	}
}

func TestCodexPath_Override(t *testing.T) {
	t.Setenv("CODEX_CLI_PATH", "/usr/local/bin/codex")

	if p := codexPath(); p != "/usr/local/bin/codex" {
		t.Errorf("codexPath() = %q, want /usr/local/bin/codex", p)
	}
}

func TestBuildCodexEnv_UsesOpenAIAPIKeyWhenCodexUnset(t *testing.T) {
	t.Setenv("CODEX_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "openai-test-key")

	env := buildCodexEnv()
	if !slices.Contains(env, "CODEX_API_KEY=openai-test-key") {
		t.Fatalf("expected CODEX_API_KEY to be populated from OPENAI_API_KEY, got %v", env)
	}
	if !slices.Contains(env, "HOME=/home/worker") {
		t.Fatalf("expected HOME override, got %v", env)
	}
}

func TestExecuteCodex_NonexistentBinary(t *testing.T) {
	t.Setenv("CODEX_CLI_PATH", "/nonexistent/codex-cli")
	t.Setenv("ORKA_ALLOW_BASH", "true")

	cfg := &common.AgentConfig{
		Prompt:   "test prompt",
		MaxTurns: 5,
	}

	_, err := executeCodex(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error when codex binary doesn't exist")
	}
}

func TestExecuteCodex_RejectsBashDisabledConfig(t *testing.T) {
	t.Setenv("ORKA_ALLOW_BASH", "")

	cfg := &common.AgentConfig{
		Prompt:   "test prompt",
		MaxTurns: 5,
	}

	_, err := executeCodex(context.Background(), cfg)
	if !errors.Is(err, errCodexRequiresBash) {
		t.Fatalf("executeCodex() error = %v, want %v", err, errCodexRequiresBash)
	}
}

func TestExecuteCodex_UsesOutputFileWhenPresent(t *testing.T) {
	scriptPath := writeCodexStub(t, `#!/bin/sh
OUTPUT=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--output-last-message" ]; then
    OUTPUT="$2"
    shift 2
    continue
  fi
  shift
done
printf 'codex-ok' > "$OUTPUT"
printf 'ignored-stdout'
`)

	t.Setenv("CODEX_CLI_PATH", scriptPath)
	t.Setenv("ORKA_ALLOW_BASH", "true")

	cfg := &common.AgentConfig{
		Prompt:   "test prompt",
		MaxTurns: 5,
	}

	result, err := executeCodex(context.Background(), cfg)
	if err != nil {
		t.Fatalf("executeCodex() error = %v", err)
	}
	if result != "codex-ok" {
		t.Fatalf("executeCodex() = %q, want %q", result, "codex-ok")
	}
}

func TestExecuteCodex_FallsBackToStdoutWhenOutputFileMissing(t *testing.T) {
	scriptPath := writeCodexStub(t, `#!/bin/sh
printf 'stdout-result'
`)

	t.Setenv("CODEX_CLI_PATH", scriptPath)
	t.Setenv("ORKA_ALLOW_BASH", "true")

	cfg := &common.AgentConfig{
		Prompt:   "test prompt",
		MaxTurns: 5,
	}

	result, err := executeCodex(context.Background(), cfg)
	if err != nil {
		t.Fatalf("executeCodex() error = %v", err)
	}
	if result != "stdout-result" {
		t.Fatalf("executeCodex() = %q, want %q", result, "stdout-result")
	}
}

func TestExecuteCodex_SendsPromptViaStdin(t *testing.T) {
	scriptPath := writeCodexStub(t, `#!/bin/sh
OUTPUT=""
LAST_ARG=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--output-last-message" ]; then
    OUTPUT="$2"
    shift 2
    continue
  fi
  LAST_ARG="$1"
  shift
done
INPUT="$(cat)"
printf 'arg=%s\nstdin=%s\n' "$LAST_ARG" "$INPUT" > "$OUTPUT"
`)

	t.Setenv("CODEX_CLI_PATH", scriptPath)
	t.Setenv("ORKA_ALLOW_BASH", "true")

	cfg := &common.AgentConfig{
		Prompt:   "--help",
		MaxTurns: 5,
	}

	result, err := executeCodex(context.Background(), cfg)
	if err != nil {
		t.Fatalf("executeCodex() error = %v", err)
	}
	if !strings.Contains(result, "arg=-") {
		t.Fatalf("executeCodex() = %q, want prompt sentinel", result)
	}
	if !strings.Contains(result, "stdin=--help") {
		t.Fatalf("executeCodex() = %q, want stdin prompt", result)
	}
}

func assertContains(t *testing.T, values []string, want string) {
	t.Helper()
	if slices.Contains(values, want) {
		return
	}
	t.Fatalf("expected args to contain %q, got %v", want, values)
}

func assertNotContains(t *testing.T, values []string, want string) {
	t.Helper()
	if slices.Contains(values, want) {
		t.Fatalf("expected args not to contain %q, got %v", want, values)
	}
}

func writeCodexStub(t *testing.T, content string) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "codex-stub")
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return path
}
