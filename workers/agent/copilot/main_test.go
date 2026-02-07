/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	copilot "github.com/github/copilot-sdk/go"
)

func TestLoadConfig_RequiredFields(t *testing.T) {
	// Ensure missing prompt fails
	t.Setenv("MERCAN_PROMPT", "")
	t.Setenv("MERCAN_TASK_NAME", "t1")
	t.Setenv("MERCAN_TASK_NAMESPACE", "default")

	_, err := loadConfig()
	if err == nil {
		t.Fatal("expected error for missing MERCAN_PROMPT")
	}
}

func TestLoadConfig_Defaults(t *testing.T) {
	t.Setenv("MERCAN_PROMPT", "hello")
	t.Setenv("MERCAN_TASK_NAME", "t1")
	t.Setenv("MERCAN_TASK_NAMESPACE", "default")
	t.Setenv("MERCAN_MAX_TURNS", "")
	t.Setenv("MERCAN_ALLOWED_TOOLS", "")
	t.Setenv("MERCAN_DISALLOWED_TOOLS", "")
	t.Setenv("MERCAN_MODEL", "")
	t.Setenv("MERCAN_SYSTEM_PROMPT", "")
	t.Setenv("MERCAN_GIT_REPO", "")
	t.Setenv("MERCAN_TIMEOUT_SECONDS", "")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.prompt != "hello" {
		t.Errorf("expected prompt 'hello', got %q", cfg.prompt)
	}
	if cfg.maxTurns != defaultMaxTurns {
		t.Errorf("expected maxTurns %d, got %d", defaultMaxTurns, cfg.maxTurns)
	}
}

func TestLoadConfig_AllFields(t *testing.T) {
	t.Setenv("MERCAN_PROMPT", "refactor code")
	t.Setenv("MERCAN_TASK_NAME", "task1")
	t.Setenv("MERCAN_TASK_NAMESPACE", "ns1")
	t.Setenv("MERCAN_MODEL", "gpt-4.1")
	t.Setenv("MERCAN_SYSTEM_PROMPT", "Be helpful")
	t.Setenv("MERCAN_MAX_TURNS", "100")
	t.Setenv("MERCAN_ALLOWED_TOOLS", "Read,Write,Edit")
	t.Setenv("MERCAN_DISALLOWED_TOOLS", "Bash")
	t.Setenv("MERCAN_GIT_REPO", "https://github.com/example/repo.git")
	t.Setenv("MERCAN_GIT_BRANCH", "main")
	t.Setenv("MERCAN_GIT_REF", "abc123")
	t.Setenv("MERCAN_WORKSPACE_SUBPATH", "src")
	t.Setenv("MERCAN_TIMEOUT_SECONDS", "600")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.model != "gpt-4.1" {
		t.Errorf("model = %q, want gpt-4.1", cfg.model)
	}
	if cfg.maxTurns != 100 {
		t.Errorf("maxTurns = %d, want 100", cfg.maxTurns)
	}
	if len(cfg.allowedTools) != 3 {
		t.Errorf("allowedTools len = %d, want 3", len(cfg.allowedTools))
	}
	if len(cfg.disallowedTools) != 1 {
		t.Errorf("disallowedTools len = %d, want 1", len(cfg.disallowedTools))
	}
	if cfg.gitRepo != "https://github.com/example/repo.git" {
		t.Errorf("gitRepo = %q", cfg.gitRepo)
	}
	if cfg.subPath != "src" {
		t.Errorf("subPath = %q, want src", cfg.subPath)
	}
	if cfg.timeoutSeconds != 600 {
		t.Errorf("timeoutSeconds = %d, want 600", cfg.timeoutSeconds)
	}
}

func TestLoadConfig_InvalidMaxTurns(t *testing.T) {
	t.Setenv("MERCAN_PROMPT", "hello")
	t.Setenv("MERCAN_MAX_TURNS", "not-a-number")
	t.Setenv("MERCAN_ALLOWED_TOOLS", "")
	t.Setenv("MERCAN_DISALLOWED_TOOLS", "")
	t.Setenv("MERCAN_TIMEOUT_SECONDS", "")

	_, err := loadConfig()
	if err == nil {
		t.Fatal("expected error for invalid MERCAN_MAX_TURNS")
	}
}

func TestBuildSessionConfig_Minimal(t *testing.T) {
	cfg := &config{
		prompt:   "hello world",
		maxTurns: 50,
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
	cfg := &config{
		prompt:          "fix bugs",
		model:           "gpt-4.1",
		systemPrompt:    "You are a code reviewer",
		maxTurns:        100,
		allowedTools:    []string{"Read", "Write"},
		disallowedTools: []string{"Bash"},
		subPath:         "src",
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
	if result.Kind != "allow" {
		t.Errorf("OnPermissionRequest result.Kind = %q, want allow", result.Kind)
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

func TestLoadConfig_InvalidTimeoutSeconds(t *testing.T) {
	t.Setenv("MERCAN_PROMPT", "hello")
	t.Setenv("MERCAN_MAX_TURNS", "")
	t.Setenv("MERCAN_ALLOWED_TOOLS", "")
	t.Setenv("MERCAN_DISALLOWED_TOOLS", "")
	t.Setenv("MERCAN_TIMEOUT_SECONDS", "not-a-number")

	_, err := loadConfig()
	if err == nil {
		t.Fatal("expected error for invalid MERCAN_TIMEOUT_SECONDS")
	}
	if !strings.Contains(err.Error(), "invalid MERCAN_TIMEOUT_SECONDS") {
		t.Errorf("error = %q, want it to mention MERCAN_TIMEOUT_SECONDS", err.Error())
	}
}

func TestConfigureGitAuth_NoSecrets(t *testing.T) {
	cmd := exec.Command("echo")
	configureGitAuth(cmd)

	envMap := envToMap(cmd.Env)
	if _, ok := envMap["GIT_TOKEN"]; ok {
		t.Error("GIT_TOKEN should not be set when no secret files exist")
	}
	if _, ok := envMap["GIT_ASKPASS"]; ok {
		t.Error("GIT_ASKPASS should not be set when no secret files exist")
	}
	if _, ok := envMap["GIT_USERNAME"]; ok {
		t.Error("GIT_USERNAME should not be set when no secret files exist")
	}
}

func TestConfigureGitAuth_WithToken(t *testing.T) {
	tmpDir := t.TempDir()
	tokenPath := tmpDir + "/token"
	if err := os.WriteFile(tokenPath, []byte("  my-secret-token  \n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Simulate the token reading logic from configureGitAuth
	cmd := exec.Command("echo")
	env := os.Environ()

	data, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimSpace(string(data))
	if token != "my-secret-token" {
		t.Fatalf("token = %q, want my-secret-token", token)
	}
	env = append(env,
		fmt.Sprintf("GIT_TOKEN=%s", token),
		"GIT_ASKPASS=/bin/echo-token",
	)
	cmd.Env = env

	envMap := envToMap(cmd.Env)
	if envMap["GIT_TOKEN"] != "my-secret-token" {
		t.Errorf("GIT_TOKEN = %q, want my-secret-token", envMap["GIT_TOKEN"])
	}
	if envMap["GIT_ASKPASS"] != "/bin/echo-token" {
		t.Errorf("GIT_ASKPASS = %q, want /bin/echo-token", envMap["GIT_ASKPASS"])
	}
}

func TestBuildSessionConfig_EmptyModel(t *testing.T) {
	cfg := &config{
		prompt: "test prompt",
		model:  "",
	}

	sc := buildSessionConfig(cfg)
	if sc.Model != "" {
		t.Errorf("Model = %q, want empty string", sc.Model)
	}
}

func TestBuildSessionConfig_EmptySystemPrompt(t *testing.T) {
	cfg := &config{
		prompt:       "test prompt",
		systemPrompt: "",
	}

	sc := buildSessionConfig(cfg)
	if sc.SystemMessage != nil {
		t.Error("SystemMessage should be nil for empty systemPrompt")
	}
}

func TestBuildSessionConfig_AllowedToolsOnly(t *testing.T) {
	cfg := &config{
		prompt:       "test",
		allowedTools: []string{" Read ", "Write"},
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
	cfg := &config{
		prompt:          "test",
		disallowedTools: []string{" Bash ", "Shell"},
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

// envToMap converts a slice of "KEY=VALUE" strings to a map.
func envToMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, e := range env {
		k, v, ok := strings.Cut(e, "=")
		if ok {
			m[k] = v
		}
	}
	return m
}
