/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package common

import (
	"os"
	"strings"
	"testing"
)

func TestLoadConfig_RequiredFields(t *testing.T) {
	t.Setenv("ORKA_PROMPT", "")
	t.Setenv("ORKA_TASK_NAME", "t1")
	t.Setenv("ORKA_TASK_NAMESPACE", "default")

	_, err := LoadConfig(50)
	if err == nil {
		t.Fatal("expected error for missing ORKA_PROMPT")
	}
}

func TestLoadConfig_Defaults(t *testing.T) {
	t.Setenv("ORKA_PROMPT", "hello")
	t.Setenv("ORKA_TASK_NAME", "t1")
	t.Setenv("ORKA_TASK_NAMESPACE", "default")
	t.Setenv("ORKA_MAX_TURNS", "")
	t.Setenv("ORKA_ALLOWED_TOOLS", "")
	t.Setenv("ORKA_DISALLOWED_TOOLS", "")
	t.Setenv("ORKA_MODEL", "")
	t.Setenv("ORKA_SYSTEM_PROMPT", "")
	t.Setenv("ORKA_GIT_REPO", "")
	t.Setenv("ORKA_TIMEOUT_SECONDS", "")

	cfg, err := LoadConfig(50)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Prompt != "hello" {
		t.Errorf("expected Prompt 'hello', got %q", cfg.Prompt)
	}
	if cfg.MaxTurns != 50 {
		t.Errorf("expected MaxTurns 50, got %d", cfg.MaxTurns)
	}
}

func TestLoadConfig_AllFields(t *testing.T) {
	t.Setenv("ORKA_PROMPT", "refactor code")
	t.Setenv("ORKA_TASK_NAME", "task1")
	t.Setenv("ORKA_TASK_NAMESPACE", "ns1")
	t.Setenv("ORKA_MODEL", "test-model")
	t.Setenv("ORKA_SYSTEM_PROMPT", "Be helpful")
	t.Setenv("ORKA_MAX_TURNS", "100")
	t.Setenv("ORKA_ALLOWED_TOOLS", "Read,Write,Edit")
	t.Setenv("ORKA_DISALLOWED_TOOLS", "Bash")
	t.Setenv("ORKA_GIT_REPO", "https://github.com/example/repo.git")
	t.Setenv("ORKA_GIT_BRANCH", "main")
	t.Setenv("ORKA_GIT_REF", "abc123")
	t.Setenv("ORKA_WORKSPACE_SUBPATH", "src")
	t.Setenv("ORKA_TIMEOUT_SECONDS", "600")

	cfg, err := LoadConfig(50)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Model != "test-model" {
		t.Errorf("Model = %q, want test-model", cfg.Model)
	}
	if cfg.MaxTurns != 100 {
		t.Errorf("MaxTurns = %d, want 100", cfg.MaxTurns)
	}
	if len(cfg.AllowedTools) != 3 {
		t.Errorf("AllowedTools len = %d, want 3", len(cfg.AllowedTools))
	}
	if len(cfg.DisallowedTools) != 1 {
		t.Errorf("DisallowedTools len = %d, want 1", len(cfg.DisallowedTools))
	}
	if cfg.GitRepo != "https://github.com/example/repo.git" {
		t.Errorf("GitRepo = %q", cfg.GitRepo)
	}
	if cfg.SubPath != "src" {
		t.Errorf("SubPath = %q, want src", cfg.SubPath)
	}
	if cfg.TimeoutSeconds != 600 {
		t.Errorf("TimeoutSeconds = %d, want 600", cfg.TimeoutSeconds)
	}
}

func TestLoadConfig_InvalidMaxTurns(t *testing.T) {
	t.Setenv("ORKA_PROMPT", "hello")
	t.Setenv("ORKA_MAX_TURNS", "not-a-number")
	t.Setenv("ORKA_ALLOWED_TOOLS", "")
	t.Setenv("ORKA_DISALLOWED_TOOLS", "")
	t.Setenv("ORKA_TIMEOUT_SECONDS", "")

	_, err := LoadConfig(50)
	if err == nil {
		t.Fatal("expected error for invalid ORKA_MAX_TURNS")
	}
}

func TestLoadConfig_InvalidTimeoutSeconds(t *testing.T) {
	t.Setenv("ORKA_PROMPT", "hello")
	t.Setenv("ORKA_MAX_TURNS", "")
	t.Setenv("ORKA_ALLOWED_TOOLS", "")
	t.Setenv("ORKA_DISALLOWED_TOOLS", "")
	t.Setenv("ORKA_TIMEOUT_SECONDS", "not-a-number")

	_, err := LoadConfig(50)
	if err == nil {
		t.Fatal("expected error for invalid ORKA_TIMEOUT_SECONDS")
	}
	if !strings.Contains(err.Error(), "invalid ORKA_TIMEOUT_SECONDS") {
		t.Errorf("error = %q, want it to mention ORKA_TIMEOUT_SECONDS", err.Error())
	}
}

func TestSetupGitCredentials_NoSecrets(t *testing.T) {
	t.Setenv("GIT_TOKEN", "")
	t.Setenv("GIT_ASKPASS", "")
	t.Setenv("GIT_USERNAME", "")

	SetupGitCredentials()

	if os.Getenv("GIT_TOKEN") != "" {
		t.Error("GIT_TOKEN should not be set when no secret files exist")
	}
	if os.Getenv("GIT_ASKPASS") != "" {
		t.Error("GIT_ASKPASS should not be set when no secret files exist")
	}
	if os.Getenv("GIT_USERNAME") != "" {
		t.Error("GIT_USERNAME should not be set when no secret files exist")
	}
}
