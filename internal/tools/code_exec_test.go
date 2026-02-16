/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const typeObject = "object"

func TestCodeExecTool_Name(t *testing.T) {
	tool := NewCodeExecTool()
	if got := tool.Name(); got != "code_exec" {
		t.Errorf("Name() = %v, want %v", got, "code_exec")
	}
}

func TestCodeExecTool_Description(t *testing.T) {
	tool := NewCodeExecTool()
	desc := tool.Description()
	if desc == "" {
		t.Error("Description() returned empty string")
	}
}

func TestCodeExecTool_Parameters(t *testing.T) {
	tool := NewCodeExecTool()
	params := tool.Parameters()
	if params == nil {
		t.Fatal("Parameters() returned nil")
	}

	// Verify it's valid JSON
	var schema map[string]any
	if err := json.Unmarshal(params, &schema); err != nil {
		t.Errorf("Parameters() returned invalid JSON: %v", err)
	}

	// Check required fields
	if schema["type"] != typeObject {
		t.Error("Parameters schema should have type: object")
	}
}

func TestCodeExecTool_Execute_Bash(t *testing.T) {
	tmpDir := t.TempDir()
	tool := &CodeExecTool{
		workDir:      tmpDir,
		timeout:      30 * time.Second,
		allowedLangs: map[string]bool{"bash": true, "sh": true},
	}

	tests := []struct {
		name       string
		code       string
		wantOutput string
		wantErr    bool
	}{
		{
			name:       "simple echo",
			code:       "echo hello",
			wantOutput: "hello\n",
			wantErr:    false,
		},
		{
			name:       "exit code",
			code:       "exit 0",
			wantOutput: "",
			wantErr:    false,
		},
		{
			name:    "failing command",
			code:    "exit 1",
			wantErr: false, // Execute doesn't error, it returns the exit code in result
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := json.RawMessage(`{"language": "bash", "code": "` + tt.code + `"}`)
			result, err := tool.Execute(context.Background(), args)
			if (err != nil) != tt.wantErr {
				t.Errorf("Execute() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr && result == "" {
				t.Error("Execute() returned empty result")
			}

			if tt.wantOutput != "" {
				var execResult CodeExecResult
				if err := json.Unmarshal([]byte(result), &execResult); err != nil {
					t.Fatalf("failed to unmarshal result: %v", err)
				}
				if execResult.Output != tt.wantOutput {
					t.Errorf("Execute() output = %q, want %q", execResult.Output, tt.wantOutput)
				}
			}
		})
	}
}

func TestCodeExecTool_Execute_UnsupportedLanguage(t *testing.T) {
	tmpDir := t.TempDir()
	tool := &CodeExecTool{
		workDir:      tmpDir,
		timeout:      30 * time.Second,
		allowedLangs: map[string]bool{"bash": true},
	}

	args := json.RawMessage(`{"language": "ruby", "code": "puts 'hello'"}`)
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Error("Execute() expected error for unsupported language")
	}
}

func TestCodeExecTool_Execute_EmptyCode(t *testing.T) {
	tool := NewCodeExecTool()

	args := json.RawMessage(`{"language": "bash", "code": ""}`)
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Error("Execute() expected error for empty code")
	}
}

func TestCodeExecTool_Execute_InvalidJSON(t *testing.T) {
	tool := NewCodeExecTool()

	args := json.RawMessage(`{invalid}`)
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Error("Execute() expected error for invalid JSON")
	}
}

func TestCodeExecTool_Execute_Timeout(t *testing.T) {
	tmpDir := t.TempDir()
	tool := &CodeExecTool{
		workDir:      tmpDir,
		timeout:      30 * time.Second,
		allowedLangs: map[string]bool{"bash": true},
	}

	// Test with custom timeout from args
	args := json.RawMessage(`{"language": "bash", "code": "echo quick", "timeout": 5}`)
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var execResult CodeExecResult
	if err := json.Unmarshal([]byte(result), &execResult); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}
	if execResult.ExitCode != 0 {
		t.Errorf("Execute() exit_code = %d, want 0", execResult.ExitCode)
	}
}

func TestCodeExecTool_Execute_TimeoutExceedsMax(t *testing.T) {
	tmpDir := t.TempDir()
	tool := &CodeExecTool{
		workDir:      tmpDir,
		timeout:      30 * time.Second,
		allowedLangs: map[string]bool{"bash": true},
	}

	// Timeout > 60 should use default
	args := json.RawMessage(`{"language": "bash", "code": "echo test", "timeout": 120}`)
	_, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
}

func TestCodeExecTool_Execute_Python(t *testing.T) {
	// Skip if python3 is not available
	if _, err := os.Stat("/usr/bin/python3"); os.IsNotExist(err) {
		t.Skip("python3 not available")
	}

	tmpDir := t.TempDir()
	tool := &CodeExecTool{
		workDir:      tmpDir,
		timeout:      30 * time.Second,
		allowedLangs: map[string]bool{"python": true, "python3": true},
	}

	args := json.RawMessage(`{"language": "python", "code": "print('hello')"}`)
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var execResult CodeExecResult
	if err := json.Unmarshal([]byte(result), &execResult); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	if execResult.Output != "hello\n" {
		t.Errorf("Execute() output = %q, want %q", execResult.Output, "hello\n")
	}
}

func TestCodeExecTool_Execute_JavaScript(t *testing.T) {
	// Skip if node is not available
	if _, err := os.Stat("/usr/local/bin/node"); os.IsNotExist(err) {
		if _, err := os.Stat("/usr/bin/node"); os.IsNotExist(err) {
			t.Skip("node not available")
		}
	}

	tmpDir := t.TempDir()
	tool := &CodeExecTool{
		workDir:      tmpDir,
		timeout:      30 * time.Second,
		allowedLangs: map[string]bool{"javascript": true, "node": true},
	}

	args := json.RawMessage(`{"language": "javascript", "code": "console.log('hello')"}`)
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var execResult CodeExecResult
	if err := json.Unmarshal([]byte(result), &execResult); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	if execResult.Output != "hello\n" {
		t.Errorf("Execute() output = %q, want %q", execResult.Output, "hello\n")
	}
}

func TestCodeExecTool_Execute_FailedCommand(t *testing.T) {
	tmpDir := t.TempDir()
	tool := &CodeExecTool{
		workDir:      tmpDir,
		timeout:      30 * time.Second,
		allowedLangs: map[string]bool{"bash": true},
	}

	args := json.RawMessage(`{"language": "bash", "code": "exit 42"}`)
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var execResult CodeExecResult
	if err := json.Unmarshal([]byte(result), &execResult); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	if execResult.ExitCode != 42 {
		t.Errorf("Execute() exit_code = %d, want 42", execResult.ExitCode)
	}
}

func TestCodeExecTool_Execute_Stderr(t *testing.T) {
	tmpDir := t.TempDir()
	tool := &CodeExecTool{
		workDir:      tmpDir,
		timeout:      30 * time.Second,
		allowedLangs: map[string]bool{"bash": true},
	}

	args := json.RawMessage(`{"language": "bash", "code": "echo error >&2"}`)
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var execResult CodeExecResult
	if err := json.Unmarshal([]byte(result), &execResult); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	if execResult.Error != "error\n" {
		t.Errorf("Execute() error = %q, want %q", execResult.Error, "error\n")
	}
}

func TestCodeExecTool_Execute_WorkDirCreation(t *testing.T) {
	tmpDir := filepath.Join(t.TempDir(), "nested", "dir")
	tool := &CodeExecTool{
		workDir:      tmpDir,
		timeout:      30 * time.Second,
		allowedLangs: map[string]bool{"bash": true},
	}

	args := json.RawMessage(`{"language": "bash", "code": "echo test"}`)
	_, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	// Verify directory was created
	if _, err := os.Stat(tmpDir); os.IsNotExist(err) {
		t.Error("work directory was not created")
	}
}

func TestNewCodeExecTool(t *testing.T) {
	tool := NewCodeExecTool()
	if tool == nil {
		t.Fatal("NewCodeExecTool returned nil")
	}

	if tool.timeout != 30*time.Second {
		t.Errorf("timeout = %v, want %v", tool.timeout, 30*time.Second)
	}

	expectedLangs := []string{"python", "python3", "javascript", "node", "bash", "sh"}
	for _, lang := range expectedLangs {
		if !tool.allowedLangs[lang] {
			t.Errorf("language %q should be allowed", lang)
		}
	}
}

func TestNewCodeExecTool_WithEnvVar(t *testing.T) {
	originalWorkDir := os.Getenv("ORKA_WORK_DIR")
	os.Setenv("ORKA_WORK_DIR", "/custom/work/dir")    //nolint:errcheck
	defer os.Setenv("ORKA_WORK_DIR", originalWorkDir) //nolint:errcheck

	tool := NewCodeExecTool()
	if tool.workDir != "/custom/work/dir" {
		t.Errorf("workDir = %v, want %v", tool.workDir, "/custom/work/dir")
	}
}

func TestCodeExecTool_Execute_Node(t *testing.T) {
	// Test node execution directly via executeNode
	tmpDir := t.TempDir()
	tool := &CodeExecTool{
		workDir:      tmpDir,
		timeout:      30 * time.Second,
		allowedLangs: map[string]bool{"javascript": true, "node": true},
	}

	// Test via Execute with "node" language alias
	args := json.RawMessage(`{"language": "node", "code": "console.log('node-test')"}`)
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var execResult CodeExecResult
	if err := json.Unmarshal([]byte(result), &execResult); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	if execResult.Output != "node-test\n" {
		t.Errorf("Execute() output = %q, want %q", execResult.Output, "node-test\n")
	}
}

func TestCodeExecTool_Execute_Python3Alias(t *testing.T) {
	tmpDir := t.TempDir()
	tool := &CodeExecTool{
		workDir:      tmpDir,
		timeout:      30 * time.Second,
		allowedLangs: map[string]bool{"python": true, "python3": true},
	}

	args := json.RawMessage(`{"language": "python3", "code": "print('py3')"}`)
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var execResult CodeExecResult
	if err := json.Unmarshal([]byte(result), &execResult); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	if execResult.Output != "py3\n" {
		t.Errorf("Execute() output = %q, want %q", execResult.Output, "py3\n")
	}
}

func TestCodeExecTool_Execute_ShAlias(t *testing.T) {
	tmpDir := t.TempDir()
	tool := &CodeExecTool{
		workDir:      tmpDir,
		timeout:      30 * time.Second,
		allowedLangs: map[string]bool{"bash": true, "sh": true},
	}

	args := json.RawMessage(`{"language": "sh", "code": "echo sh-test"}`)
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var execResult CodeExecResult
	if err := json.Unmarshal([]byte(result), &execResult); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	if execResult.Output != "sh-test\n" {
		t.Errorf("Execute() output = %q, want %q", execResult.Output, "sh-test\n")
	}
}

func TestCodeExecTool_Execute_MixedCase(t *testing.T) {
	tmpDir := t.TempDir()
	tool := &CodeExecTool{
		workDir:      tmpDir,
		timeout:      30 * time.Second,
		allowedLangs: map[string]bool{"bash": true},
	}

	args := json.RawMessage(`{"language": "BASH", "code": "echo upper"}`)
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var execResult CodeExecResult
	if err := json.Unmarshal([]byte(result), &execResult); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}
	if execResult.Output != "upper\n" {
		t.Errorf("Execute() output = %q, want %q", execResult.Output, "upper\n")
	}
}

func TestCodeExecTool_RunCommand_NonExecError(t *testing.T) {
	tmpDir := t.TempDir()
	tool := &CodeExecTool{
		workDir: tmpDir,
		timeout: 30 * time.Second,
	}

	// Command that doesn't exist — produces a non-ExitError
	cmd := exec.CommandContext(context.Background(), "nonexistent-binary-xyz")
	result := tool.runCommand(cmd)
	if result.ExitCode != -1 {
		t.Errorf("expected exit code -1, got %d", result.ExitCode)
	}
	if result.Error == "" {
		t.Error("expected non-empty error message")
	}
}

func TestCodeExecTool_Execute_StdoutAndStderr(t *testing.T) {
	tmpDir := t.TempDir()
	tool := &CodeExecTool{
		workDir:      tmpDir,
		timeout:      30 * time.Second,
		allowedLangs: map[string]bool{"bash": true},
	}

	args := json.RawMessage(`{"language": "bash", "code": "echo stdout; echo stderr >&2"}`)
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var execResult CodeExecResult
	if err := json.Unmarshal([]byte(result), &execResult); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	if execResult.Output != "stdout\n" {
		t.Errorf("Execute() output = %q, want %q", execResult.Output, "stdout\n")
	}
	if execResult.Error != "stderr\n" {
		t.Errorf("Execute() error = %q, want %q", execResult.Error, "stderr\n")
	}
}
