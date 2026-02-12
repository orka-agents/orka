/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"testing"

	copilot "github.com/github/copilot-sdk/go"

	"github.com/sozercan/mercan/workers/common"
)

func TestBuildSessionConfig_Minimal(t *testing.T) {
	cfg := &common.AgentConfig{
		Prompt:   "hello world",
		MaxTurns: 50,
	}

	sc := buildSessionConfig(cfg)

	if sc.WorkingDirectory != workspaceDir {
		t.Errorf("WorkingDirectory = %q, want %q", sc.WorkingDirectory, workspaceDir)
	}
	if sc.Model != "" {
		t.Errorf("Model = %q, want empty", sc.Model)
	}
	if sc.SystemMessage != nil {
		t.Error("expected nil SystemMessage for empty systemPrompt")
	}
	if len(sc.AvailableTools) != 0 {
		t.Errorf("AvailableTools = %v, want empty", sc.AvailableTools)
	}
	if len(sc.ExcludedTools) != 0 {
		t.Errorf("ExcludedTools = %v, want empty", sc.ExcludedTools)
	}
	if sc.OnPermissionRequest == nil {
		t.Error("expected OnPermissionRequest to be set")
	}
}

func TestBuildSessionConfig_Full(t *testing.T) {
	cfg := &common.AgentConfig{
		Prompt:          "fix bugs",
		Model:           "gpt-4.1",
		SystemPrompt:    "You are a code reviewer",
		MaxTurns:        100,
		AllowedTools:    []string{"Read", "Write"},
		DisallowedTools: []string{"Bash"},
		SubPath:         "src",
	}

	sc := buildSessionConfig(cfg)

	if sc.Model != "gpt-4.1" {
		t.Errorf("Model = %q, want gpt-4.1", sc.Model)
	}
	if sc.WorkingDirectory != workspaceDir+"/src" {
		t.Errorf("WorkingDirectory = %q, want %s/src", sc.WorkingDirectory, workspaceDir)
	}
	if sc.SystemMessage == nil {
		t.Fatal("expected SystemMessage to be set")
	}
	if sc.SystemMessage.Mode != "append" {
		t.Errorf("SystemMessage.Mode = %q, want append", sc.SystemMessage.Mode)
	}
	if sc.SystemMessage.Content != "You are a code reviewer" {
		t.Errorf("SystemMessage.Content = %q", sc.SystemMessage.Content)
	}
	if len(sc.AvailableTools) != 2 {
		t.Errorf("AvailableTools len = %d, want 2", len(sc.AvailableTools))
	}
	if len(sc.ExcludedTools) != 1 {
		t.Errorf("ExcludedTools len = %d, want 1", len(sc.ExcludedTools))
	}

	// Verify permission handler auto-approves
	result, err := sc.OnPermissionRequest(
		copilot.PermissionRequest{Kind: "tool_use"},
		copilot.PermissionInvocation{SessionID: "test"},
	)
	if err != nil {
		t.Fatalf("OnPermissionRequest error: %v", err)
	}
	if result.Kind != "approved" {
		t.Errorf("OnPermissionRequest result.Kind = %q, want approved", result.Kind)
	}
}

func TestCopilotCLIPath_Default(t *testing.T) {
	t.Setenv("COPILOT_CLI_PATH", "")

	if p := copilotCLIPath(); p != defaultCopilotPath {
		t.Errorf("copilotCLIPath() = %q, want %q", p, defaultCopilotPath)
	}
}

func TestCopilotCLIPath_Override(t *testing.T) {
	t.Setenv("COPILOT_CLI_PATH", "/usr/local/bin/copilot")

	if p := copilotCLIPath(); p != "/usr/local/bin/copilot" {
		t.Errorf("copilotCLIPath() = %q, want /usr/local/bin/copilot", p)
	}
}

func TestExtractResult_Nil(t *testing.T) {
	if r := extractResult(nil); r != "" {
		t.Errorf("extractResult(nil) = %q, want empty", r)
	}
}

func TestExtractResult_WithResultContent(t *testing.T) {
	event := &copilot.SessionEvent{
		Data: copilot.Data{
			Result: &copilot.Result{Content: "task completed"},
		},
	}
	if r := extractResult(event); r != "task completed" {
		t.Errorf("extractResult() = %q, want 'task completed'", r)
	}
}

func TestExtractResult_WithContent(t *testing.T) {
	content := "hello from assistant"
	event := &copilot.SessionEvent{
		Data: copilot.Data{
			Content: &content,
		},
	}
	if r := extractResult(event); r != "hello from assistant" {
		t.Errorf("extractResult() = %q, want 'hello from assistant'", r)
	}
}

func TestExtractResult_ResultTakesPrecedence(t *testing.T) {
	content := "assistant message"
	event := &copilot.SessionEvent{
		Data: copilot.Data{
			Result:  &copilot.Result{Content: "final result"},
			Content: &content,
		},
	}
	if r := extractResult(event); r != "final result" {
		t.Errorf("extractResult() = %q, want 'final result' (Result should take precedence)", r)
	}
}

func TestBuildSessionConfig_EmptyModel(t *testing.T) {
	cfg := &common.AgentConfig{
		Prompt: "test prompt",
	}

	sc := buildSessionConfig(cfg)
	if sc.Model != "" {
		t.Errorf("Model = %q, want empty string", sc.Model)
	}
}

func TestBuildSessionConfig_EmptySystemPrompt(t *testing.T) {
	cfg := &common.AgentConfig{
		Prompt: "test prompt",
	}

	sc := buildSessionConfig(cfg)
	if sc.SystemMessage != nil {
		t.Error("SystemMessage should be nil for empty systemPrompt")
	}
}

func TestBuildSessionConfig_AllowedToolsOnly(t *testing.T) {
	cfg := &common.AgentConfig{
		Prompt:       "test",
		AllowedTools: []string{" Read ", "Write"},
	}

	sc := buildSessionConfig(cfg)
	if len(sc.AvailableTools) != 2 {
		t.Fatalf("AvailableTools len = %d, want 2", len(sc.AvailableTools))
	}
	if sc.AvailableTools[0] != "Read" {
		t.Errorf("AvailableTools[0] = %q, want 'Read' (trimmed)", sc.AvailableTools[0])
	}
	if len(sc.ExcludedTools) != 0 {
		t.Errorf("ExcludedTools should be empty, got %v", sc.ExcludedTools)
	}
}

func TestBuildSessionConfig_DisallowedToolsOnly(t *testing.T) {
	cfg := &common.AgentConfig{
		Prompt:          "test",
		DisallowedTools: []string{" Bash ", "Shell"},
	}

	sc := buildSessionConfig(cfg)
	if len(sc.ExcludedTools) != 2 {
		t.Fatalf("ExcludedTools len = %d, want 2", len(sc.ExcludedTools))
	}
	if sc.ExcludedTools[0] != "Bash" {
		t.Errorf("ExcludedTools[0] = %q, want 'Bash' (trimmed)", sc.ExcludedTools[0])
	}
	if len(sc.AvailableTools) != 0 {
		t.Errorf("AvailableTools should be empty, got %v", sc.AvailableTools)
	}
}

func TestCopilotCLIPath_EmptyString(t *testing.T) {
	t.Setenv("COPILOT_CLI_PATH", "")

	if p := copilotCLIPath(); p != defaultCopilotPath {
		t.Errorf("copilotCLIPath() = %q, want %q", p, defaultCopilotPath)
	}
}

func TestExtractResult_EmptyData(t *testing.T) {
	event := &copilot.SessionEvent{}
	if r := extractResult(event); r != "" {
		t.Errorf("extractResult() = %q, want empty for empty data", r)
	}
}

func TestExtractResult_EmptyResultContent(t *testing.T) {
	event := &copilot.SessionEvent{
		Data: copilot.Data{
			Result: &copilot.Result{Content: ""},
		},
	}
	if r := extractResult(event); r != "" {
		t.Errorf("extractResult() = %q, want empty for empty Result.Content", r)
	}
}
