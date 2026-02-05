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
	"context"
	"testing"
)

func TestExecuteCommand_Success(t *testing.T) {
	result := executeCommand(context.Background(), []string{"echo", "hello"})

	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
	if result.Stdout != "hello\n" {
		t.Errorf("Stdout = %q, want %q", result.Stdout, "hello\n")
	}
	if result.Stderr != "" {
		t.Errorf("Stderr = %q, want empty", result.Stderr)
	}
}

func TestExecuteCommand_Failed(t *testing.T) {
	result := executeCommand(context.Background(), []string{"false"})

	if result.ExitCode == 0 {
		t.Error("ExitCode should not be 0 for 'false' command")
	}
}

func TestExecuteCommand_CommandNotFound(t *testing.T) {
	result := executeCommand(context.Background(), []string{"nonexistent_command_12345"})

	if result.ExitCode == 0 {
		t.Error("ExitCode should not be 0 for nonexistent command")
	}
}

func TestExecuteCommand_WithStderr(t *testing.T) {
	// Use a command that writes to stderr
	result := executeCommand(context.Background(), []string{"sh", "-c", "echo error >&2"})

	if result.Stderr != "error\n" {
		t.Errorf("Stderr = %q, want %q", result.Stderr, "error\n")
	}
}

func TestExecuteCommand_MixedOutput(t *testing.T) {
	result := executeCommand(context.Background(), []string{"sh", "-c", "echo stdout; echo stderr >&2"})

	if result.Stdout != "stdout\n" {
		t.Errorf("Stdout = %q, want %q", result.Stdout, "stdout\n")
	}
	if result.Stderr != "stderr\n" {
		t.Errorf("Stderr = %q, want %q", result.Stderr, "stderr\n")
	}
}

func TestExecuteCommand_ExitCode(t *testing.T) {
	tests := []struct {
		name     string
		command  []string
		wantCode int
	}{
		{
			name:     "exit 0",
			command:  []string{"sh", "-c", "exit 0"},
			wantCode: 0,
		},
		{
			name:     "exit 1",
			command:  []string{"sh", "-c", "exit 1"},
			wantCode: 1,
		},
		{
			name:     "exit 42",
			command:  []string{"sh", "-c", "exit 42"},
			wantCode: 42,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := executeCommand(context.Background(), tt.command)
			if result.ExitCode != tt.wantCode {
				t.Errorf("ExitCode = %d, want %d", result.ExitCode, tt.wantCode)
			}
		})
	}
}

func TestExecuteCommand_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	result := executeCommand(ctx, []string{"sleep", "10"})

	// Should fail due to cancelled context
	if result.ExitCode == 0 {
		t.Error("Command should fail with cancelled context")
	}
}

func TestResult_Fields(t *testing.T) {
	result := Result{
		ExitCode: 0,
		Stdout:   "output",
		Stderr:   "error",
		Duration: "1s",
	}

	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
	if result.Stdout != "output" {
		t.Errorf("Stdout = %s, want output", result.Stdout)
	}
	if result.Stderr != "error" {
		t.Errorf("Stderr = %s, want error", result.Stderr)
	}
	if result.Duration != "1s" {
		t.Errorf("Duration = %s, want 1s", result.Duration)
	}
}

func TestExecuteCommand_MultipleArgs(t *testing.T) {
	result := executeCommand(context.Background(), []string{"echo", "hello", "world"})

	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
	if result.Stdout != "hello world\n" {
		t.Errorf("Stdout = %q, want %q", result.Stdout, "hello world\n")
	}
}

func TestExecuteCommand_EmptyOutput(t *testing.T) {
	result := executeCommand(context.Background(), []string{"true"})

	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
	if result.Stdout != "" {
		t.Errorf("Stdout = %q, want empty", result.Stdout)
	}
	if result.Stderr != "" {
		t.Errorf("Stderr = %q, want empty", result.Stderr)
	}
}
