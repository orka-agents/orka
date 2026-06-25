package cliwrapper

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sozercan/orka/internal/harness"
	"github.com/sozercan/orka/internal/workerenv"
)

func TestCodexAdapterBuildsLegacyCompatibleArgs(t *testing.T) {
	t.Setenv(workerenv.AllowBash, "true")
	t.Setenv(workerenv.Model, "gpt-test")
	t.Setenv(workerenv.SystemPrompt, "system guidance")
	t.Setenv(workerenv.MaxTurns, "12")
	t.Setenv(workerenv.OpenAIBaseURL, "https://example.invalid/v1")
	t.Setenv(workerenv.AllowedTools, "Bash,web_search")

	adapter := NewCodexAdapter(CodexAdapterConfig{Path: "/fake/codex", WorkDir: t.TempDir()})
	spec, err := adapter.BuildCommand(context.Background(), TurnContext{Prompt: "do work"})
	if err != nil {
		t.Fatalf("BuildCommand: %v", err)
	}
	defer removeTempFiles(spec.TempFiles)
	joined := strings.Join(spec.Args, " ")
	for _, want := range []string{
		"exec",
		"--skip-git-repo-check",
		"--ephemeral",
		"--color never",
		"--output-last-message",
		"--config approval_policy=never",
		"--sandbox workspace-write",
		"--model gpt-test",
		"--config openai_base_url=https://example.invalid/v1",
		"--config web_search=live",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args = %q, missing %q", joined, want)
		}
	}
	if string(spec.Stdin) != "do work" {
		t.Fatalf("stdin = %q, want prompt", string(spec.Stdin))
	}
	if !containsEnv(spec.Env, "HOME=/home/worker") {
		t.Fatalf("env = %#v, want HOME", spec.Env)
	}
}

func TestCodexAdapterRequiresAllowBash(t *testing.T) {
	t.Setenv(workerenv.AllowBash, "false")
	adapter := NewCodexAdapter(CodexAdapterConfig{})
	_, err := adapter.BuildCommand(context.Background(), TurnContext{})
	if err == nil || !strings.Contains(err.Error(), workerenv.AllowBash) {
		t.Fatalf("BuildCommand error = %v, want allow bash requirement", err)
	}
}

func TestCodexAdapterRejectsAllowlistWithoutBash(t *testing.T) {
	t.Setenv(workerenv.AllowBash, "true")
	t.Setenv(workerenv.AllowedTools, "Read,WebSearch")
	adapter := NewCodexAdapter(CodexAdapterConfig{Path: "/fake/codex", WorkDir: t.TempDir()})
	_, err := adapter.BuildCommand(context.Background(), TurnContext{Prompt: "do work"})
	if err == nil {
		t.Fatal("BuildCommand error = nil, want unsupported Codex allowlist error")
	}
	if !strings.Contains(err.Error(), workerenv.AllowedTools) ||
		!strings.Contains(err.Error(), "without Bash") {
		t.Fatalf("BuildCommand error = %v, want unsupported Codex allowlist error", err)
	}
}

func TestCodexAdapterAllowsBashAliasesInAllowlist(t *testing.T) {
	for _, tc := range []struct {
		name string
		tool string
	}{
		{name: "bash", tool: "Bash"},
		{name: "shell", tool: "shell"},
		{name: "code exec", tool: "code_exec"},
		{name: "terminal", tool: "terminal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(workerenv.AllowBash, "true")
			t.Setenv(workerenv.AllowedTools, "Read,"+tc.tool)
			adapter := NewCodexAdapter(CodexAdapterConfig{Path: "/fake/codex", WorkDir: t.TempDir()})

			spec, err := adapter.BuildCommand(context.Background(), TurnContext{Prompt: "do work"})
			if err != nil {
				t.Fatalf("BuildCommand error = %v, want alias to satisfy Bash allowlist", err)
			}
			defer removeTempFiles(spec.TempFiles)
		})
	}
}

func TestCodexAdapterRejectsScopedBashAllowlist(t *testing.T) {
	t.Setenv(workerenv.AllowBash, "true")
	t.Setenv(workerenv.AllowedTools, "Read,Bash(git push *)")
	adapter := NewCodexAdapter(CodexAdapterConfig{Path: "/fake/codex", WorkDir: t.TempDir()})

	_, err := adapter.BuildCommand(context.Background(), TurnContext{Prompt: "do work"})
	if err == nil {
		t.Fatal("BuildCommand error = nil, want unsupported scoped Bash allowlist error")
	}
	if !strings.Contains(err.Error(), workerenv.AllowedTools) ||
		!strings.Contains(err.Error(), "without Bash") {
		t.Fatalf("BuildCommand error = %v, want unsupported scoped Bash allowlist error", err)
	}
}

func TestCodexAdapterRejectsDisallowedBash(t *testing.T) {
	for _, tc := range []struct {
		name string
		tool string
	}{
		{name: "bash", tool: "Bash"},
		{name: "scoped bash", tool: "Bash(git push *)"},
		{name: "shell", tool: "shell"},
		{name: "code exec", tool: "code_exec"},
		{name: "terminal", tool: "terminal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(workerenv.AllowBash, "true")
			t.Setenv(workerenv.DisallowedTools, tc.tool)
			adapter := NewCodexAdapter(CodexAdapterConfig{Path: "/fake/codex", WorkDir: t.TempDir()})

			_, err := adapter.BuildCommand(context.Background(), TurnContext{Prompt: "do work"})
			if err == nil {
				t.Fatal("BuildCommand error = nil, want unsupported Codex disallowlist error")
			}
			if !strings.Contains(err.Error(), workerenv.DisallowedTools) ||
				!strings.Contains(err.Error(), "cannot disable shell execution") {
				t.Fatalf("BuildCommand error = %v, want unsupported Codex disallowlist error", err)
			}
		})
	}
}

func TestCodexAdapterRejectsUnsupportedDisallowedTools(t *testing.T) {
	for _, tc := range []struct {
		name string
		tool string
	}{
		{name: "write", tool: "Write"},
		{name: "mixed", tool: "WebSearch,Write"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(workerenv.AllowBash, "true")
			t.Setenv(workerenv.DisallowedTools, tc.tool)
			adapter := NewCodexAdapter(CodexAdapterConfig{Path: "/fake/codex", WorkDir: t.TempDir()})

			_, err := adapter.BuildCommand(context.Background(), TurnContext{Prompt: "do work"})
			if err == nil {
				t.Fatal("BuildCommand error = nil, want unsupported Codex disallowlist error")
			}
			if !strings.Contains(err.Error(), workerenv.DisallowedTools) ||
				!strings.Contains(err.Error(), "only supports disabling WebSearch") {
				t.Fatalf("BuildCommand error = %v, want unsupported Codex disallowlist error", err)
			}
		})
	}
}

func TestCodexAdapterAllowsDisallowedWebSearch(t *testing.T) {
	t.Setenv(workerenv.AllowBash, "true")
	t.Setenv(workerenv.DisallowedTools, "WebSearch")
	adapter := NewCodexAdapter(CodexAdapterConfig{Path: "/fake/codex", WorkDir: t.TempDir()})

	spec, err := adapter.BuildCommand(context.Background(), TurnContext{Prompt: "do work"})
	if err != nil {
		t.Fatalf("BuildCommand error = %v, want WebSearch disallowlist to be enforced", err)
	}
	defer removeTempFiles(spec.TempFiles)
	if !strings.Contains(strings.Join(spec.Args, " "), "--config web_search=disabled") {
		t.Fatalf("args = %#v, want web_search disabled", spec.Args)
	}
}

func TestCodexAdapterCleansTempFilesOnWorkspaceStatError(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv(workerenv.AllowBash, "true")
	t.Setenv(workerenv.SystemPrompt, "system guidance requiring an instructions file")
	loop := filepath.Join(t.TempDir(), "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Skipf("symlink loop unavailable: %v", err)
	}
	adapter := NewCodexAdapter(CodexAdapterConfig{Path: "/fake/codex", WorkDir: loop})

	_, err := adapter.BuildCommand(context.Background(), TurnContext{Prompt: "do work"})
	if err == nil || !strings.Contains(err.Error(), "stat codex workspace directory") {
		t.Fatalf("BuildCommand error = %v, want workspace stat error", err)
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatalf("ReadDir temp: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "codex-instructions-") || strings.HasPrefix(entry.Name(), "codex-last-message-") {
			t.Fatalf("temporary file %q was not cleaned up after BuildCommand failure", entry.Name())
		}
	}
}

func TestCodexAdapterRunsFakeCLIThroughWrapper(t *testing.T) {
	dir := t.TempDir()
	fakeCodex := filepath.Join(dir, "codex-fake.sh")
	if err := os.WriteFile(fakeCodex, []byte(`#!/bin/sh
set -eu
out=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "--output-last-message" ]; then out="$arg"; fi
  prev="$arg"
done
prompt=$(cat)
printf 'last message: %s' "$prompt" > "$out"
printf 'progress for %s' "$prompt"
`), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(workerenv.AllowBash, "true")
	cfg := DefaultConfig()
	cfg.AllowUnauthenticated = true
	cfg.Runtime = RuntimeCodex
	cfg.WorkDir = dir
	adapter := NewCodexAdapter(CodexAdapterConfig{Path: fakeCodex, WorkDir: dir})
	baseURL, cleanup := startWrapperServerWithConfig(t, cfg, adapter)
	defer cleanup()
	client, err := harness.NewClient(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	request := validWrapperStartTurnRequest()
	request.Input.Prompt = "codex prompt"
	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	frames := collectWrapperFrames(t, client, request.TurnID, 0)
	last := frames[len(frames)-1]
	if last.Type != harness.FrameTurnCompleted || last.Completed == nil {
		t.Fatalf("last frame = %#v, want completed", last)
	}
	if !strings.Contains(last.Completed.Result, "last message: codex prompt") {
		t.Fatalf("completed result = %q, want fake codex result", last.Completed.Result)
	}
}

func TestCodexAdapterFailurePath(t *testing.T) {
	dir := t.TempDir()
	fakeCodex := filepath.Join(dir, "codex-fail.sh")
	failingScript := "#!/bin/sh\n" +
		"echo 'Authorization: Bearer redaction-value-1234567890'\n" +
		"exit 42\n"
	if err := os.WriteFile(fakeCodex, []byte(failingScript), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(workerenv.AllowBash, "true")
	cfg := DefaultConfig()
	cfg.AllowUnauthenticated = true
	cfg.Runtime = RuntimeCodex
	adapter := NewCodexAdapter(CodexAdapterConfig{Path: fakeCodex, WorkDir: dir})
	baseURL, cleanup := startWrapperServerWithConfig(t, cfg, adapter)
	defer cleanup()
	client, err := harness.NewClient(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	request := validWrapperStartTurnRequest()
	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	frames := collectWrapperFrames(t, client, request.TurnID, 0)
	last := frames[len(frames)-1]
	if last.Type != harness.FrameTurnFailed || last.Failed == nil {
		t.Fatalf("last frame = %#v, want failed", last)
	}
	encoded := stringifyFrames(t, frames)
	if strings.Contains(encoded, "redaction-value") ||
		(strings.Contains(encoded, "Authorization") && !strings.Contains(encoded, "[REDACTED]")) {
		t.Fatalf("failure frames leaked secret or missed redaction: %s", encoded)
	}
}

func stringifyFrames(t *testing.T, frames []harness.HarnessEventFrame) string {
	t.Helper()
	data, err := json.Marshal(frames)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestCodexInstructionsDenyAllWhenAllowlistIntersectionEmpty(t *testing.T) {
	instructions := buildCodexInstructions(&agentEnvConfig{AllowedToolsSet: true, MaxTurns: defaultCodexMaxTurns})
	if !strings.Contains(instructions, "Do not use runtime tools") {
		t.Fatalf("instructions = %q, want deny-all tool guidance", instructions)
	}
}

func TestCodexAdapterPreservesExplicitCodexAPIKey(t *testing.T) {
	t.Setenv(workerenv.AllowBash, "true")
	t.Setenv(workerenv.OpenAIAPIKey, "operator-openai-key")
	adapter := NewCodexAdapter(CodexAdapterConfig{Path: "/fake/codex", WorkDir: t.TempDir()})
	spec, err := adapter.BuildCommand(context.Background(), TurnContext{
		Prompt: "do work",
		Env:    []string{workerenv.CodexAPIKey + "=explicit-codex-key"},
	})
	if err != nil {
		t.Fatalf("BuildCommand: %v", err)
	}
	defer removeTempFiles(spec.TempFiles)
	if !containsEnv(spec.Env, workerenv.CodexAPIKey+"=explicit-codex-key") {
		t.Fatalf("env = %#v, want explicit Codex API key preserved", spec.Env)
	}
	if containsEnv(spec.Env, workerenv.CodexAPIKey+"=operator-openai-key") {
		t.Fatalf("env = %#v, want OpenAI fallback not to overwrite explicit Codex key", spec.Env)
	}
}

func TestCodexAdapterUsesRuntimeSecretBaseURLWithoutOperatorBaseURL(t *testing.T) {
	t.Setenv(workerenv.AllowBash, "true")
	t.Setenv(workerenv.OpenAIBaseURL, "")
	adapter := NewCodexAdapter(CodexAdapterConfig{Path: "/fake/codex", WorkDir: t.TempDir()})
	spec, err := adapter.BuildCommand(context.Background(), TurnContext{
		Prompt: "do work",
		Env:    []string{workerenv.OpenAIBaseURL + "=https://runtime-secret.example.invalid/v1"},
	})
	if err != nil {
		t.Fatalf("BuildCommand: %v", err)
	}
	defer removeTempFiles(spec.TempFiles)
	joined := strings.Join(spec.Args, " ")
	if !strings.Contains(joined, "openai_base_url=https://runtime-secret.example.invalid/v1") {
		t.Fatalf("args = %q, want runtime secret base URL", joined)
	}
	if !containsEnv(spec.Env, workerenv.OpenAIBaseURL+"=https://runtime-secret.example.invalid/v1") {
		t.Fatalf("env = %#v, want runtime secret base URL", spec.Env)
	}
}

func TestCodexAdapterIgnoresTurnEnvOpenAIBaseURL(t *testing.T) {
	t.Setenv(workerenv.AllowBash, "true")
	t.Setenv(workerenv.OpenAIBaseURL, "https://operator.example.invalid/v1")
	adapter := NewCodexAdapter(CodexAdapterConfig{Path: "/fake/codex", WorkDir: t.TempDir()})
	spec, err := adapter.BuildCommand(context.Background(), TurnContext{
		Prompt: "do work",
		Env:    []string{workerenv.OpenAIBaseURL + "=https://turn.example.invalid/v1"},
	})
	if err != nil {
		t.Fatalf("BuildCommand: %v", err)
	}
	defer removeTempFiles(spec.TempFiles)
	joined := strings.Join(spec.Args, " ")
	if strings.Contains(joined, "https://turn.example.invalid/v1") {
		t.Fatalf("args = %q, want turn env base URL ignored", joined)
	}
	if !strings.Contains(joined, "openai_base_url=https://operator.example.invalid/v1") {
		t.Fatalf("args = %q, want operator base URL", joined)
	}
	if containsEnv(spec.Env, workerenv.OpenAIBaseURL+"=https://turn.example.invalid/v1") {
		t.Fatalf("env = %#v, want turn env base URL removed", spec.Env)
	}
	if !containsEnv(spec.Env, workerenv.OpenAIBaseURL+"=https://operator.example.invalid/v1") {
		t.Fatalf("env = %#v, want operator base URL", spec.Env)
	}
}

func TestCodexAdapterIgnoresTurnEnvSandboxPolicy(t *testing.T) {
	t.Setenv(workerenv.AllowBash, "true")
	t.Setenv(workerenv.CodexSandboxMode, "")
	t.Setenv(workerenv.CodexDisableSandbox, "")
	adapter := NewCodexAdapter(CodexAdapterConfig{Path: "/fake/codex", WorkDir: t.TempDir()})
	spec, err := adapter.BuildCommand(context.Background(), TurnContext{
		Prompt: "do work",
		Env: []string{
			workerenv.CodexSandboxMode + "=danger-full-access",
			workerenv.CodexDisableSandbox + "=true",
		},
	})
	if err != nil {
		t.Fatalf("BuildCommand: %v", err)
	}
	defer removeTempFiles(spec.TempFiles)
	joined := strings.Join(spec.Args, " ")
	if strings.Contains(joined, "--dangerously-bypass-approvals-and-sandbox") {
		t.Fatalf("args = %q, want turn env unable to disable sandbox", joined)
	}
	if strings.Contains(joined, "--sandbox danger-full-access") || !strings.Contains(joined, "--sandbox workspace-write") {
		t.Fatalf("args = %q, want operator/default sandbox policy", joined)
	}
}
