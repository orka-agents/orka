package cliwrapper

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sozercan/orka/internal/harness"
	"github.com/sozercan/orka/internal/workerenv"
)

func TestClaudeAdapterBuildsMinimalArgs(t *testing.T) {
	turn := TurnContext{Prompt: "hello world", Metadata: map[string]string{"maxTurns": "50"}}
	args := buildClaudeArgs(agentConfigFromTurn(turn), turn)
	assertContains(t, args, "--print")
	assertContains(t, args, "--verbose")
	assertFlagValue(t, args, "--max-turns", "50")
	assertFlagValue(t, args, "-p", "hello world")
	if args[len(args)-1] != "hello world" {
		t.Fatalf("prompt should be last arg, got %q", args[len(args)-1])
	}
}

func TestClaudeAdapterBuildsFullArgs(t *testing.T) {
	turn := TurnContext{
		Prompt: "fix bugs",
		Metadata: map[string]string{
			"model":           "claude-sonnet-4-20250514",
			"systemPrompt":    "You are a code reviewer",
			"maxTurns":        "100",
			"allowedTools":    "Read,Write",
			"disallowedTools": "Bash",
		},
	}
	args := buildClaudeArgs(agentConfigFromTurn(turn), turn)
	assertFlagValue(t, args, "--model", "claude-sonnet-4-20250514")
	assertFlagValue(t, args, "--append-system-prompt", "You are a code reviewer")
	assertFlagValue(t, args, "--max-turns", "100")
	assertFlagValue(t, args, "--tools", "Read,Write")
	assertContains(t, args, "--allowedTools")
	assertContains(t, args, "Read")
	assertContains(t, args, "Write")
	assertContains(t, args, "--disallowedTools")
	assertContains(t, args, "Bash")
	if slices.Contains(args, "--dangerously-skip-permissions") {
		t.Fatal("--dangerously-skip-permissions should not be set when allowBash is false")
	}
}

func TestClaudeAdapterBuildsAllowBashArgs(t *testing.T) {
	t.Setenv(workerenv.AllowBash, "true")
	turn := TurnContext{Prompt: "hello", Metadata: map[string]string{"allowBash": "true"}}
	args := buildClaudeArgs(agentConfigFromTurn(turn), turn)
	assertContains(t, args, "--dangerously-skip-permissions")
}

func TestClaudeAdapterScopesReadOnlyPermissions(t *testing.T) {
	turn := TurnContext{
		Prompt: "review",
		Env: []string{
			workerenv.ClaudeBare + "=true",
			workerenv.ClaudeDisableSettingSources + "=true",
			workerenv.ClaudePermissionMode + "=dontAsk",
		},
		Metadata: map[string]string{
			"allowedTools": strings.Join([]string{
				"Read(/workspace/**)",
				"Glob(/workspace/**)",
				"Grep(/workspace/**)",
				"LS(/workspace/**)",
				"Read(/workspace/**)",
			}, ","),
			"disallowedTools": "Bash,Read(/proc/**)",
		},
	}
	args := buildClaudeArgs(agentConfigFromTurn(turn), turn)
	assertContains(t, args, "--bare")
	assertFlagValue(t, args, "--setting-sources", "")
	assertFlagValue(t, args, "--permission-mode", "dontAsk")
	assertFlagValue(t, args, "--tools", "Read,Glob,Grep,LS")
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

func TestClaudeAdapterRunsFakeCLIThroughWrapper(t *testing.T) {
	dir := t.TempDir()
	fakeClaude := writeFakeCommand(t, dir, "claude", `#!/bin/sh
last=
for arg in "$@"; do last="$arg"; done
printf 'claude:%s' "$last"
`)
	cfg := DefaultConfig()
	cfg.AllowUnauthenticated = true
	cfg.Runtime = RuntimeClaude
	adapter := NewClaudeAdapter(ClaudeAdapterConfig{Path: fakeClaude, WorkDir: dir})
	baseURL, cleanup := startWrapperServerWithConfig(t, cfg, adapter)
	defer cleanup()
	client, err := harness.NewClient(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	request := validWrapperStartTurnRequest()
	request.Metadata = map[string]string{"runtime": RuntimeClaude, "allowBash": "true"}
	request.Input.Prompt = "hello claude"
	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	frames := collectWrapperFrames(t, client, request.TurnID, 0)
	last := frames[len(frames)-1]
	if last.Type != harness.FrameTurnCompleted || last.Completed == nil {
		t.Fatalf("last frame = %#v, want completed", last)
	}
	if got := strings.TrimSpace(last.Completed.Result); got != "claude:hello claude" {
		t.Fatalf("result = %q, want fake claude output", got)
	}
}

func assertContains(t *testing.T, values []string, want string) {
	t.Helper()
	if slices.Contains(values, want) {
		return
	}
	t.Fatalf("%q not found in %v", want, values)
}

func assertFlagValue(t *testing.T, args []string, flag, want string) {
	t.Helper()
	for i, arg := range args {
		if arg == flag {
			if i+1 >= len(args) {
				t.Fatalf("flag %s missing value in %v", flag, args)
			}
			if args[i+1] != want {
				t.Fatalf("flag %s value = %q, want %q (args=%v)", flag, args[i+1], want, args)
			}
			return
		}
	}
	t.Fatalf("flag %s not found in %v", flag, args)
}

func writeFakeCommand(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
