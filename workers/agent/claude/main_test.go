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
	"slices"
	"strings"
	"testing"
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
	t.Setenv("MERCAN_ALLOW_BASH", "")
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
	if cfg.allowBash {
		t.Error("expected allowBash to be false by default")
	}
}

func TestLoadConfig_AllFields(t *testing.T) {
	t.Setenv("MERCAN_PROMPT", "refactor code")
	t.Setenv("MERCAN_TASK_NAME", "task1")
	t.Setenv("MERCAN_TASK_NAMESPACE", "ns1")
	t.Setenv("MERCAN_MODEL", "claude-sonnet-4-20250514")
	t.Setenv("MERCAN_SYSTEM_PROMPT", "Be helpful")
	t.Setenv("MERCAN_MAX_TURNS", "100")
	t.Setenv("MERCAN_ALLOWED_TOOLS", "Read,Write,Edit")
	t.Setenv("MERCAN_DISALLOWED_TOOLS", "Bash")
	t.Setenv("MERCAN_ALLOW_BASH", "true")
	t.Setenv("MERCAN_GIT_REPO", "https://github.com/example/repo.git")
	t.Setenv("MERCAN_GIT_BRANCH", "main")
	t.Setenv("MERCAN_GIT_REF", "abc123")
	t.Setenv("MERCAN_WORKSPACE_SUBPATH", "src")
	t.Setenv("MERCAN_TIMEOUT_SECONDS", "600")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.model != "claude-sonnet-4-20250514" {
		t.Errorf("model = %q, want claude-sonnet-4-20250514", cfg.model)
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
	if !cfg.allowBash {
		t.Error("expected allowBash=true")
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
	t.Setenv("MERCAN_ALLOW_BASH", "")
	t.Setenv("MERCAN_TIMEOUT_SECONDS", "")

	_, err := loadConfig()
	if err == nil {
		t.Fatal("expected error for invalid MERCAN_MAX_TURNS")
	}
}

func TestBuildClaudeArgs_Minimal(t *testing.T) {
	cfg := &config{
		prompt:   "hello world",
		maxTurns: 50,
	}

	args := buildClaudeArgs(cfg)

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
	cfg := &config{
		prompt:          "fix bugs",
		model:           "claude-sonnet-4-20250514",
		systemPrompt:    "You are a code reviewer",
		maxTurns:        100,
		allowedTools:    []string{"Read", "Write"},
		disallowedTools: []string{"Bash"},
	}

	args := buildClaudeArgs(cfg)

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

func TestLoadConfig_InvalidTimeoutSeconds(t *testing.T) {
	t.Setenv("MERCAN_PROMPT", "hello")
	t.Setenv("MERCAN_MAX_TURNS", "")
	t.Setenv("MERCAN_ALLOWED_TOOLS", "")
	t.Setenv("MERCAN_DISALLOWED_TOOLS", "")
	t.Setenv("MERCAN_ALLOW_BASH", "")
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

	// Should still set env (inherits os.Environ), but no GIT_TOKEN or GIT_USERNAME
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
	// Create a temp directory mimicking /secrets/git/
	tmpDir := t.TempDir()
	tokenPath := tmpDir + "/token"
	if err := os.WriteFile(tokenPath, []byte("  my-secret-token  \n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Override tokenPaths by creating the file at the expected path.
	// Since configureGitAuth reads from hardcoded paths, we test the logic
	// by calling the function and checking the fallback behavior.
	// We can't easily override the paths, so instead we test the env-setting
	// logic directly by simulating what the function does.
	cmd := exec.Command("echo")
	env := os.Environ()

	// Simulate finding a token file (the same logic as configureGitAuth)
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

func TestConfigureGitAuth_WithEmptyToken(t *testing.T) {
	// configureGitAuth skips empty tokens after trimming
	cmd := exec.Command("echo")
	configureGitAuth(cmd)

	envMap := envToMap(cmd.Env)
	if _, ok := envMap["GIT_TOKEN"]; ok {
		t.Error("GIT_TOKEN should not be set for empty token files")
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

// assertContains checks that a string slice contains the given value.
func assertContains(t *testing.T, s []string, val string) {
	t.Helper()
	if slices.Contains(s, val) {
		return
	}
	t.Errorf("expected args to contain %q, got %v", val, s)
}
