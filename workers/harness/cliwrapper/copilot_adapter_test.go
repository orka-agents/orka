package cliwrapper

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	copilot "github.com/github/copilot-sdk/go"

	"github.com/orka-agents/orka/internal/harness"
	"github.com/orka-agents/orka/internal/workerenv"
	"github.com/orka-agents/orka/workers/common"
)

func TestCopilotAdapterBuildCommandUsesHelperAndCLIEnv(t *testing.T) {
	dir := t.TempDir()
	adapter := NewCopilotAdapter(CopilotAdapterConfig{
		HelperPath: "/tmp/fake-copilot-helper",
		Path:       "/tmp/fake-copilot-cli",
		WorkDir:    dir,
	})
	spec, err := adapter.BuildCommand(context.Background(), TurnContext{
		Prompt: "hello",
		Env:    []string{"GITHUB_TOKEN=token"},
	})
	if err != nil {
		t.Fatalf("BuildCommand: %v", err)
	}
	if spec.Path != "/tmp/fake-copilot-helper" {
		t.Fatalf("Path = %q, want helper", spec.Path)
	}
	if !slices.Equal(spec.Args, []string{"copilot-turn"}) {
		t.Fatalf("Args = %#v, want copilot-turn", spec.Args)
	}
	if spec.Dir != dir {
		t.Fatalf("Dir = %q, want %q", spec.Dir, dir)
	}
	if string(spec.Stdin) != "hello" {
		t.Fatalf("Stdin = %q, want prompt", string(spec.Stdin))
	}
	if !containsEnv(spec.Env, workerenv.CopilotCLIPath+"=/tmp/fake-copilot-cli") {
		t.Fatalf("Env = %#v, want copilot CLI path", spec.Env)
	}
}

func TestCopilotAdapterRunsFakeHelperThroughWrapper(t *testing.T) {
	dir := t.TempDir()
	helper := filepath.Join(dir, "copilot-helper.sh")
	helperScript := `#!/bin/sh
prompt=$(cat)
if [ -z "$prompt" ]; then prompt=$ORKA_PROMPT; fi
printf 'copilot:%s' "$prompt"
`
	if err := os.WriteFile(helper, []byte(helperScript), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.AllowUnauthenticated = true
	cfg.Runtime = RuntimeCopilot
	cfg.Copilot.HelperPath = helper
	cfg.Copilot.WorkDir = dir
	baseURL, cleanup := startWrapperServerWithConfig(t, cfg, NewCopilotAdapter(cfg.Copilot))
	defer cleanup()
	client, err := harness.NewClient(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	request := validWrapperStartTurnRequest()
	if request.Metadata == nil {
		request.Metadata = map[string]string{}
	}
	request.Metadata["runtime"] = RuntimeCopilot
	request.Input.Prompt = "hello copilot"
	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	frames := collectWrapperFrames(t, client, request.TurnID, 0)
	last := frames[len(frames)-1]
	if last.Type != harness.FrameTurnCompleted || last.Completed == nil {
		t.Fatalf("last frame = %#v, want completed", last)
	}
	if got := strings.TrimSpace(last.Completed.Result); got != "copilot:hello copilot" {
		t.Fatalf("result = %q, want fake copilot output", got)
	}
}

func TestBuildCopilotSessionConfig(t *testing.T) {
	cfg := &common.AgentConfig{
		Model:           "gpt-5.4",
		SystemPrompt:    "system",
		AllowedTools:    []string{" bash ", "create_file"},
		AllowedToolsSet: true,
		DisallowedTools: []string{" web_search "},
	}
	sessionCfg := buildCopilotSessionConfig(cfg, "/workspace/repo")
	if sessionCfg.Model != "gpt-5.4" || sessionCfg.WorkingDirectory != "/workspace/repo" {
		t.Fatalf("session config = %#v", sessionCfg)
	}
	if sessionCfg.SystemMessage == nil ||
		sessionCfg.SystemMessage.Content != "system" ||
		sessionCfg.SystemMessage.Mode != "append" {
		t.Fatalf("SystemMessage = %#v, want append system", sessionCfg.SystemMessage)
	}
	if !slices.Equal(sessionCfg.AvailableTools, []string{"bash", "create_file"}) {
		t.Fatalf("AvailableTools = %#v", sessionCfg.AvailableTools)
	}
	if !slices.Equal(sessionCfg.ExcludedTools, []string{"web_search"}) {
		t.Fatalf("ExcludedTools = %#v", sessionCfg.ExcludedTools)
	}
	if sessionCfg.OnPermissionRequest == nil {
		t.Fatal("OnPermissionRequest = nil, want auto-approve handler")
	}
	permission, err := sessionCfg.OnPermissionRequest(copilot.PermissionRequest{}, copilot.PermissionInvocation{})
	if err != nil || permission.Kind != "approved" {
		t.Fatalf("permission = %#v, %v, want approved", permission, err)
	}
}

func TestBuildCopilotSessionConfigExplicitEmptyAllowedToolsDeniesTools(t *testing.T) {
	t.Setenv(workerenv.Prompt, "pure prompt")
	t.Setenv(workerenv.MaxTurns, "")
	t.Setenv(workerenv.AllowedTools, "")
	t.Setenv(workerenv.DisallowedTools, "")
	t.Setenv(workerenv.TimeoutSeconds, "")

	cfg, err := common.LoadConfig(defaultCopilotMaxTurns)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.AllowedToolsSet || len(cfg.AllowedTools) != 0 {
		t.Fatalf("AllowedTools = %#v (set=%t), want explicit empty allowlist", cfg.AllowedTools, cfg.AllowedToolsSet)
	}

	sessionCfg := buildCopilotSessionConfig(cfg, "/workspace/repo")
	if sessionCfg.AvailableTools == nil || len(sessionCfg.AvailableTools) != 0 {
		t.Fatalf("AvailableTools = %#v, want explicit empty allowlist", sessionCfg.AvailableTools)
	}
	permission, err := sessionCfg.OnPermissionRequest(copilot.PermissionRequest{}, copilot.PermissionInvocation{})
	if err != nil || permission.Kind != copilotPermissionDeniedByToolPolicy {
		t.Fatalf("permission = %#v, %v, want deny-all result", permission, err)
	}
	if sessionCfg.Hooks == nil || sessionCfg.Hooks.OnPreToolUse == nil {
		t.Fatal("OnPreToolUse = nil, want deny-all hook")
	}
	decision, err := sessionCfg.Hooks.OnPreToolUse(copilot.PreToolUseHookInput{}, copilot.HookInvocation{})
	if err != nil || decision == nil || decision.PermissionDecision != "deny" {
		t.Fatalf("pre-tool decision = %#v, %v, want deny", decision, err)
	}
}

func TestBuildCopilotSessionConfigOmittedAllowedToolsRetainsDefaultBehavior(t *testing.T) {
	sessionCfg := buildCopilotSessionConfig(&common.AgentConfig{}, "/workspace/repo")
	if sessionCfg.AvailableTools != nil {
		t.Fatalf("AvailableTools = %#v, want omitted", sessionCfg.AvailableTools)
	}
	permission, err := sessionCfg.OnPermissionRequest(copilot.PermissionRequest{}, copilot.PermissionInvocation{})
	if err != nil || permission.Kind != "approved" {
		t.Fatalf("permission = %#v, %v, want approved", permission, err)
	}
	if sessionCfg.Hooks != nil {
		t.Fatalf("Hooks = %#v, want no deny-all hook", sessionCfg.Hooks)
	}
}

func TestExtractCopilotResultPreferenceOrder(t *testing.T) {
	content := "assistant content"
	if got := extractCopilotResult(&copilot.SessionEvent{Data: copilot.Data{Content: &content}}); got != content {
		t.Fatalf("content result = %q", got)
	}
	summary := "summary content"
	if got := extractCopilotResult(&copilot.SessionEvent{Data: copilot.Data{SummaryContent: &summary}}); got != summary {
		t.Fatalf("summary result = %q", got)
	}
	detailed := "detailed result"
	event := &copilot.SessionEvent{Data: copilot.Data{Result: &copilot.Result{DetailedContent: &detailed}}}
	if got := extractCopilotResult(event); got != detailed {
		t.Fatalf("detailed result = %q", got)
	}
	event = &copilot.SessionEvent{Data: copilot.Data{Result: &copilot.Result{Content: "plain result"}}}
	if got := extractCopilotResult(event); got != "plain result" {
		t.Fatalf("plain result = %q", got)
	}
}
