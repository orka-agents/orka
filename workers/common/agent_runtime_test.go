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

package common

import (
	"os"
	"strings"
	"testing"
)

func TestLoadConfig_RequiredFields(t *testing.T) {
	t.Setenv("MERCAN_PROMPT", "")
	t.Setenv("MERCAN_TASK_NAME", "t1")
	t.Setenv("MERCAN_TASK_NAMESPACE", "default")

	_, err := LoadConfig(50)
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
	t.Setenv("MERCAN_PROMPT", "refactor code")
	t.Setenv("MERCAN_TASK_NAME", "task1")
	t.Setenv("MERCAN_TASK_NAMESPACE", "ns1")
	t.Setenv("MERCAN_MODEL", "test-model")
	t.Setenv("MERCAN_SYSTEM_PROMPT", "Be helpful")
	t.Setenv("MERCAN_MAX_TURNS", "100")
	t.Setenv("MERCAN_ALLOWED_TOOLS", "Read,Write,Edit")
	t.Setenv("MERCAN_DISALLOWED_TOOLS", "Bash")
	t.Setenv("MERCAN_GIT_REPO", "https://github.com/example/repo.git")
	t.Setenv("MERCAN_GIT_BRANCH", "main")
	t.Setenv("MERCAN_GIT_REF", "abc123")
	t.Setenv("MERCAN_WORKSPACE_SUBPATH", "src")
	t.Setenv("MERCAN_TIMEOUT_SECONDS", "600")

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
	t.Setenv("MERCAN_PROMPT", "hello")
	t.Setenv("MERCAN_MAX_TURNS", "not-a-number")
	t.Setenv("MERCAN_ALLOWED_TOOLS", "")
	t.Setenv("MERCAN_DISALLOWED_TOOLS", "")
	t.Setenv("MERCAN_TIMEOUT_SECONDS", "")

	_, err := LoadConfig(50)
	if err == nil {
		t.Fatal("expected error for invalid MERCAN_MAX_TURNS")
	}
}

func TestLoadConfig_InvalidTimeoutSeconds(t *testing.T) {
	t.Setenv("MERCAN_PROMPT", "hello")
	t.Setenv("MERCAN_MAX_TURNS", "")
	t.Setenv("MERCAN_ALLOWED_TOOLS", "")
	t.Setenv("MERCAN_DISALLOWED_TOOLS", "")
	t.Setenv("MERCAN_TIMEOUT_SECONDS", "not-a-number")

	_, err := LoadConfig(50)
	if err == nil {
		t.Fatal("expected error for invalid MERCAN_TIMEOUT_SECONDS")
	}
	if !strings.Contains(err.Error(), "invalid MERCAN_TIMEOUT_SECONDS") {
		t.Errorf("error = %q, want it to mention MERCAN_TIMEOUT_SECONDS", err.Error())
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
