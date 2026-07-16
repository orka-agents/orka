package cliwrapper

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/harness"
	"github.com/orka-agents/orka/internal/workerenv"
)

func TestCodexAdapterBuildsLegacyCompatibleArgs(t *testing.T) {
	t.Setenv(workerenv.AllowBash, "true")
	t.Setenv(workerenv.Model, "gpt-test")
	t.Setenv(workerenv.SystemPrompt, "system guidance")
	t.Setenv(workerenv.MaxTurns, "12")
	t.Setenv(workerenv.OpenAIBaseURL, "https://example.invalid/v1")
	t.Setenv(workerenv.AllowedTools, "web_search")
	t.Setenv(codexReasoningEffortEnv, "high")

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
		"--config model_reasoning_effort=high",
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

func TestCodexAdapterForcesHardenedReadOnlyCommand(t *testing.T) {
	t.Setenv(workerenv.AllowBash, "true")
	t.Setenv(workerenv.CodexSandboxMode, "danger-full-access")
	adapter := NewCodexAdapter(CodexAdapterConfig{Path: "/fake/codex", WorkDir: t.TempDir()})
	spec, err := adapter.BuildCommand(context.Background(), TurnContext{
		Metadata: map[string]string{"allowBash": "false", "readOnly": "true", "reasoningEffort": "high"},
		Env: []string{
			workerenv.AgentReadOnly + "=true",
			"NODE_OPTIONS=--require=/workspace/payload.cjs",
			"CODEX_HOME=/workspace/.codex",
			"PATH=/workspace/bin",
			"HTTP_PROXY=https://attacker.invalid",
			"UNTRUSTED_CHILD_VALUE=present",
		},
	})
	if err != nil {
		t.Fatalf("BuildCommand: %v", err)
	}
	defer removeTempFiles(spec.TempFiles)
	joined := strings.Join(spec.Args, " ")
	for _, want := range []string{
		"--sandbox read-only",
		"--ignore-user-config",
		"--ignore-rules",
		"--disable hooks",
		"--disable shell_snapshot",
		"--disable apps",
		"--disable plugins",
		"--config project_doc_max_bytes=0",
		"--config model_reasoning_effort=high",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args = %q, missing %q", joined, want)
		}
	}
	for _, name := range []string{"NODE_OPTIONS", "ZDOTDIR", "BASH_ENV", "LD_PRELOAD", "LD_AUDIT", "GIT_CONFIG_COUNT"} {
		if !slices.Contains(spec.UnsetEnv, name) {
			t.Fatalf("UnsetEnv = %#v, missing %s", spec.UnsetEnv, name)
		}
	}
	if !containsEnv(spec.Env, "CODEX_HOME=/home/worker/.codex") {
		t.Fatalf("env = %#v, want trusted read-only CODEX_HOME", spec.Env)
	}
	if !spec.ClearEnv {
		t.Fatal("ClearEnv = false, want read-only Codex to drop inherited wrapper environment")
	}
	if !containsEnv(spec.Env, "PATH="+wrapperSafeCommandPath) {
		t.Fatalf("env = %#v, want fixed read-only PATH", spec.Env)
	}
	for _, unwanted := range []string{"NODE_OPTIONS=", "HTTP_PROXY=", "UNTRUSTED_CHILD_VALUE=", "PATH=/workspace"} {
		if strings.Contains(strings.Join(spec.Env, "\n"), unwanted) {
			t.Fatalf("env = %#v, retained untrusted read-only value %q", spec.Env, unwanted)
		}
	}
}

func TestCodexAdapterDoesNotTrustReadOnlyEnvWithoutMetadata(t *testing.T) {
	t.Setenv(workerenv.AllowBash, "false")
	adapter := NewCodexAdapter(CodexAdapterConfig{})
	_, err := adapter.BuildCommand(context.Background(), TurnContext{Env: []string{workerenv.AgentReadOnly + "=true"}})
	if err == nil || !strings.Contains(err.Error(), workerenv.AllowBash) {
		t.Fatalf("BuildCommand error = %v, want trusted metadata requirement", err)
	}
}

func TestCodexAdapterRejectsInvalidReasoningEffort(t *testing.T) {
	t.Setenv(workerenv.AllowBash, "true")
	t.Setenv(codexReasoningEffortEnv, "maximum")
	adapter := NewCodexAdapter(CodexAdapterConfig{})
	_, err := adapter.BuildCommand(context.Background(), TurnContext{})
	if err == nil || !strings.Contains(err.Error(), codexReasoningEffortEnv) {
		t.Fatalf("BuildCommand error = %v, want reasoning effort validation", err)
	}
}

func TestCodexAdapterUsesTrustedReasoningMetadata(t *testing.T) {
	t.Setenv(workerenv.AllowBash, "true")
	t.Setenv(codexReasoningEffortEnv, "low")
	adapter := NewCodexAdapter(CodexAdapterConfig{Path: "/fake/codex", WorkDir: t.TempDir()})
	spec, err := adapter.BuildCommand(context.Background(), TurnContext{
		Metadata: map[string]string{"reasoningEffort": "high"},
		Env:      []string{codexReasoningEffortEnv + "=xhigh"},
	})
	if err != nil {
		t.Fatalf("BuildCommand: %v", err)
	}
	defer removeTempFiles(spec.TempFiles)
	joined := strings.Join(spec.Args, " ")
	if !strings.Contains(joined, "model_reasoning_effort=high") ||
		strings.Contains(joined, "model_reasoning_effort=xhigh") {
		t.Fatalf("args = %q, want controller metadata to override task env and wrapper default", joined)
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

func TestCodexAdapterRunsLargePromptThroughStdinWithoutPromptEnv(t *testing.T) {
	dir := t.TempDir()
	stdinCapture := filepath.Join(dir, "codex-stdin.txt")
	envCapture := filepath.Join(dir, "codex-prompt-env.txt")
	fakeCodex := filepath.Join(dir, "codex-large-prompt.sh")
	if err := os.WriteFile(fakeCodex, []byte(`#!/bin/sh
set -eu
out=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "--output-last-message" ]; then out="$arg"; fi
  prev="$arg"
done
if [ "${ORKA_PROMPT+x}" = "x" ]; then
  printf 'set' > "$CODEX_PROMPT_ENV_CAPTURE"
  exit 64
fi
printf 'unset' > "$CODEX_PROMPT_ENV_CAPTURE"
if [ "${CODEX_INHERITED_ENV:-}" != "inherited-value" ]; then exit 65; fi
if [ "${CODEX_SPEC_ENV:-}" != "spec-value" ]; then exit 66; fi
cat > "$CODEX_STDIN_CAPTURE"
printf 'large prompt received' > "$out"
`), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(workerenv.AllowBash, "true")
	t.Setenv(workerenv.Prompt, "inherited-parent-value")
	t.Setenv("CODEX_INHERITED_ENV", "inherited-value")
	cfg := DefaultConfig()
	cfg.AllowUnauthenticated = true
	cfg.Runtime = RuntimeCodex
	cfg.WorkDir = dir
	cfg.CommandEnv = []string{
		"CODEX_STDIN_CAPTURE=" + stdinCapture,
		"CODEX_PROMPT_ENV_CAPTURE=" + envCapture,
		"CODEX_SPEC_ENV=spec-value",
	}
	adapter := NewCodexAdapter(CodexAdapterConfig{Path: fakeCodex, WorkDir: dir})
	baseURL, cleanup := startWrapperServerWithConfig(t, cfg, adapter)
	defer cleanup()
	client, err := harness.NewClient(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	largePrompt := "begin\n" + strings.Repeat("0123456789abcdef", 10*1024) + "\nend"
	if len(largePrompt) <= 128*1024 {
		t.Fatalf("large prompt length = %d, want more than 128 KiB", len(largePrompt))
	}
	request := validWrapperStartTurnRequest()
	request.Input.Prompt = largePrompt
	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	frames := collectWrapperFrames(t, client, request.TurnID, 0)
	last := frames[len(frames)-1]
	if last.Type != harness.FrameTurnCompleted || last.Completed == nil {
		t.Fatalf("last frame = %#v, want completed", last)
	}
	if last.Completed.Result != "large prompt received" {
		t.Fatalf("completed result = %q, want fake codex result", last.Completed.Result)
	}
	capturedStdin, err := os.ReadFile(stdinCapture)
	if err != nil {
		t.Fatalf("read fake Codex stdin capture: %v", err)
	}
	if got := string(capturedStdin); got != largePrompt {
		t.Fatalf("fake Codex stdin length = %d, want exact %d-byte prompt", len(got), len(largePrompt))
	}
	capturedEnv, err := os.ReadFile(envCapture)
	if err != nil {
		t.Fatalf("read fake Codex prompt env capture: %v", err)
	}
	if got := string(capturedEnv); got != "unset" {
		t.Fatalf("fake Codex ORKA_PROMPT state = %q, want unset", got)
	}
}

func TestCodexAdapterSecurityArtifactFollowUpUsesStdinWithoutPromptEnv(t *testing.T) {
	dir := t.TempDir()
	invocationMarker := filepath.Join(dir, "codex-invoked")
	followUpStdinCapture := filepath.Join(dir, "codex-follow-up-stdin.txt")
	followUpEnvCapture := filepath.Join(dir, "codex-follow-up-prompt-env.txt")
	fakeCodex := filepath.Join(dir, "codex-security-follow-up.sh")
	if err := os.WriteFile(fakeCodex, []byte(`#!/bin/sh
set -eu
out=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "--output-last-message" ]; then out="$arg"; fi
  prev="$arg"
done
if [ -e "$CODEX_INVOCATION_MARKER" ]; then
  if [ "${ORKA_PROMPT+x}" = "x" ]; then
    printf 'set' > "$CODEX_FOLLOW_UP_ENV_CAPTURE"
    exit 64
  fi
  printf 'unset' > "$CODEX_FOLLOW_UP_ENV_CAPTURE"
  cat > "$CODEX_FOLLOW_UP_STDIN_CAPTURE"
  mkdir -p "$ORKA_ARTIFACTS_DIR"
  printf '# threat model\n' > "$ORKA_ARTIFACTS_DIR/security-threat-model.md"
  printf 'SECURITY_ARTIFACTS_WRITTEN' > "$out"
else
  if [ "${ORKA_PROMPT+x}" = "x" ]; then
    printf 'initial-set' > "$CODEX_FOLLOW_UP_ENV_CAPTURE"
    exit 63
  fi
  : > "$CODEX_INVOCATION_MARKER"
  cat > /dev/null
  : > "$out"
fi
`), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(workerenv.AllowBash, "true")
	t.Setenv(workerenv.Prompt, "inherited-parent-value")
	cfg := DefaultConfig()
	cfg.AllowUnauthenticated = true
	cfg.Runtime = RuntimeCodex
	cfg.WorkDir = dir
	cfg.CommandEnv = []string{
		"CODEX_INVOCATION_MARKER=" + invocationMarker,
		"CODEX_FOLLOW_UP_STDIN_CAPTURE=" + followUpStdinCapture,
		"CODEX_FOLLOW_UP_ENV_CAPTURE=" + followUpEnvCapture,
	}
	adapter := NewCodexAdapter(CodexAdapterConfig{Path: fakeCodex, WorkDir: dir})
	baseURL, cleanup := startWrapperServerWithConfig(t, cfg, adapter)
	defer cleanup()
	client, err := harness.NewClient(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	request := validWrapperStartTurnRequest()
	request.Input.Prompt = "REQUIRED_SECURITY_ARTIFACTS: security-threat-model.md\nreview the repository"
	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	frames := collectWrapperFrames(t, client, request.TurnID, 0)
	last := frames[len(frames)-1]
	if last.Type != harness.FrameTurnCompleted || last.Completed == nil {
		t.Fatalf("last frame = %#v, want completed", last)
	}
	if !strings.Contains(last.Completed.Result, "SECURITY_ARTIFACTS_WRITTEN") {
		t.Fatalf("completed result = %q, want follow-up result", last.Completed.Result)
	}
	expectedFollowUp := strings.Join([]string{
		"Before responding, finish the task by writing the missing required security artifacts.",
		"Write them under .orka-artifacts/.",
		"Missing files:",
		"- .orka-artifacts/security-threat-model.md",
		"Do not inspect more repository files unless absolutely necessary.",
		"Reuse the analysis already completed in this run.",
		"Use shell redirection or heredocs so the files are definitely persisted on disk.",
		"security-threat-model.md must be non-empty markdown grounded in the repository.",
		"After writing the files, reply with only: SECURITY_ARTIFACTS_WRITTEN",
		"",
	}, "\n")
	capturedFollowUp, err := os.ReadFile(followUpStdinCapture)
	if err != nil {
		t.Fatalf("read fake Codex follow-up stdin capture: %v", err)
	}
	if got := string(capturedFollowUp); got != expectedFollowUp {
		t.Fatalf("fake Codex follow-up stdin = %q, want %q", got, expectedFollowUp)
	}
	capturedEnv, err := os.ReadFile(followUpEnvCapture)
	if err != nil {
		t.Fatalf("read fake Codex follow-up prompt env capture: %v", err)
	}
	if got := string(capturedEnv); got != "unset" {
		t.Fatalf("fake Codex follow-up ORKA_PROMPT state = %q, want unset", got)
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

func TestCodexAdapterRuntimeAuthOnlyPrefersProtectedTurnBaseURL(t *testing.T) {
	t.Setenv(workerenv.AllowBash, "true")
	t.Setenv(workerenv.OpenAIBaseURL, "https://operator.example.invalid/v1")
	adapter := NewCodexAdapter(CodexAdapterConfig{Path: "/fake/codex", WorkDir: t.TempDir()})
	spec, err := adapter.BuildCommand(context.Background(), TurnContext{
		Prompt:   "do work",
		Metadata: map[string]string{"runtimeAuthOnly": "true"},
		Env:      []string{workerenv.OpenAIBaseURL + "=http://127.0.0.1:4321/v1"},
	})
	if err != nil {
		t.Fatalf("BuildCommand: %v", err)
	}
	defer removeTempFiles(spec.TempFiles)
	joined := strings.Join(spec.Args, " ")
	if !strings.Contains(joined, "openai_base_url=http://127.0.0.1:4321/v1") {
		t.Fatalf("args = %q, want protected turn base URL", joined)
	}
	if !containsEnv(spec.Env, workerenv.OpenAIBaseURL+"=http://127.0.0.1:4321/v1") {
		t.Fatalf("env = %#v, want protected turn base URL", spec.Env)
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
