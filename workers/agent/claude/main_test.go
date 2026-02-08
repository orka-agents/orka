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
	"slices"
	"testing"

	"github.com/sozercan/mercan/workers/common"
)

func TestBuildClaudeArgs_Minimal(t *testing.T) {
	t.Setenv("MERCAN_ALLOW_BASH", "")

	cfg := &common.AgentConfig{
		Prompt:   "hello world",
		MaxTurns: 50,
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
	t.Setenv("MERCAN_ALLOW_BASH", "")

	cfg := &common.AgentConfig{
		Prompt:          "fix bugs",
		Model:           "claude-sonnet-4-20250514",
		SystemPrompt:    "You are a code reviewer",
		MaxTurns:        100,
		AllowedTools:    []string{"Read", "Write"},
		DisallowedTools: []string{"Bash"},
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

	// --dangerously-skip-permissions should NOT be present
	if slices.Contains(args, "--dangerously-skip-permissions") {
		t.Error("--dangerously-skip-permissions should not be set when MERCAN_ALLOW_BASH is not true")
	}
}

func TestBuildClaudeArgs_AllowBash(t *testing.T) {
	t.Setenv("MERCAN_ALLOW_BASH", "true")

	cfg := &common.AgentConfig{
		Prompt:   "hello",
		MaxTurns: 50,
	}

	args := buildClaudeArgs(cfg)
	assertContains(t, args, "--dangerously-skip-permissions")
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

// assertContains checks that a string slice contains the given value.
func assertContains(t *testing.T, s []string, val string) {
	t.Helper()
	if slices.Contains(s, val) {
		return
	}
	t.Errorf("expected args to contain %q, got %v", val, s)
}
