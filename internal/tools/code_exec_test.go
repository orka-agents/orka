/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const typeObject = "object"

func newInProcessCodeExecTestTool(workDir string, timeout time.Duration, allowed map[string]bool) *CodeExecTool {
	if allowed == nil {
		allowed = defaultCodeExecAllowedLangs()
	}
	return &CodeExecTool{
		workDir:      workDir,
		timeout:      timeout,
		allowedLangs: allowed,
		denyPatterns: defaultDenyPatterns,
		executor:     &InProcessCodeExecutor{},
		backend:      codeExecBackendInProcess,
	}
}

type recordingCodeExecutor struct {
	calls  int
	req    CodeExecutionRequest
	result CodeExecResult
}

func (e *recordingCodeExecutor) Execute(_ context.Context, req CodeExecutionRequest) CodeExecResult {
	e.calls++
	e.req = req
	return e.result
}

func TestCodeExecTool_Name(t *testing.T) {
	tool := NewCodeExecTool()
	if got := tool.Name(); got != codeExecToolName {
		t.Errorf("Name() = %v, want %v", got, codeExecToolName)
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
	if schema[jsonSchemaTypeField] != typeObject {
		t.Error("Parameters schema should have type: object")
	}
}

func TestCodeExecTool_Execute_Bash(t *testing.T) {
	tmpDir := t.TempDir()
	tool := newInProcessCodeExecTestTool(tmpDir, 30*time.Second, nil)

	tests := []struct {
		name       string
		code       string
		wantOutput string
		wantErr    bool
	}{
		{
			name:       "simple echo",
			code:       "echo hello",
			wantOutput: helloOutputString,
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
	tool := newInProcessCodeExecTestTool(tmpDir, 30*time.Second, nil)

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

	args := json.RawMessage(invalidJSONText)
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Error("Execute() expected error for invalid JSON")
	}
}

func TestCodeExecTool_Execute_Timeout(t *testing.T) {
	tmpDir := t.TempDir()
	tool := newInProcessCodeExecTestTool(tmpDir, 30*time.Second, nil)

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
	tool := newInProcessCodeExecTestTool(tmpDir, 30*time.Second, nil)

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
	tool := newInProcessCodeExecTestTool(tmpDir, 30*time.Second, nil)

	args := json.RawMessage(`{"language": "python", "code": "print('hello')"}`)
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var execResult CodeExecResult
	if err := json.Unmarshal([]byte(result), &execResult); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	if execResult.Output != helloOutputString {
		t.Errorf("Execute() output = %q, want %q", execResult.Output, helloOutputString)
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
	tool := newInProcessCodeExecTestTool(tmpDir, 30*time.Second, nil)

	args := json.RawMessage(`{"language": "javascript", "code": "console.log('hello')"}`)
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var execResult CodeExecResult
	if err := json.Unmarshal([]byte(result), &execResult); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	if execResult.Output != helloOutputString {
		t.Errorf("Execute() output = %q, want %q", execResult.Output, helloOutputString)
	}
}

func TestCodeExecTool_Execute_FailedCommand(t *testing.T) {
	tmpDir := t.TempDir()
	tool := newInProcessCodeExecTestTool(tmpDir, 30*time.Second, nil)

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
	tool := newInProcessCodeExecTestTool(tmpDir, 30*time.Second, nil)

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
	tool := newInProcessCodeExecTestTool(tmpDir, 30*time.Second, nil)

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

	expectedLangs := []string{codeLanguagePython, python3BinaryName, codeLanguageJavaScript, codeLanguageNode, codeLanguageBash, codeLanguageShell}
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

func TestCodeExecTool_DenyPatterns_Bash(t *testing.T) {
	tmpDir := t.TempDir()
	tool := newInProcessCodeExecTestTool(tmpDir, 30*time.Second, map[string]bool{codeLanguageBash: true, codeLanguageShell: true})

	blockedCmds := []struct {
		name string
		code string
	}{
		{"rm -rf", `rm -rf /`},
		{"rm -f", `rm -f important.txt`},
		{"dd if", `dd if=/dev/zero of=/dev/sda`},
		{"shutdown", `shutdown -h now`},
		{"reboot", `reboot`},
		{"mkfs", `mkfs.ext4 /dev/sda1`},
		{"fork bomb", `:(){ :|:& };:`},
	}

	for _, tt := range blockedCmds {
		t.Run(tt.name, func(t *testing.T) {
			args, _ := json.Marshal(CodeExecArgs{Language: codeLanguageBash, Code: tt.code})
			result, err := tool.Execute(context.Background(), args)
			if err != nil {
				t.Fatalf("Execute() returned error: %v", err)
			}
			var execResult CodeExecResult
			if err := json.Unmarshal([]byte(result), &execResult); err != nil {
				t.Fatalf("failed to unmarshal: %v", err)
			}
			if execResult.ExitCode != -1 {
				t.Errorf("expected exit code -1 for blocked command, got %d", execResult.ExitCode)
			}
			if execResult.Error == "" {
				t.Error("expected error message for blocked command")
			}
		})
	}
}

func TestCodeExecTool_DenyPatterns_AllowsNormal(t *testing.T) {
	tmpDir := t.TempDir()
	tool := newInProcessCodeExecTestTool(tmpDir, 30*time.Second, map[string]bool{codeLanguageBash: true})

	safeCmds := []string{
		"echo hello",
		"ls -la",
		"cat /etc/hostname",
		"rm single_file.txt",
		"grep -r pattern .",
	}

	for _, code := range safeCmds {
		t.Run(code, func(t *testing.T) {
			args, _ := json.Marshal(CodeExecArgs{Language: codeLanguageBash, Code: code})
			result, err := tool.Execute(context.Background(), args)
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			var execResult CodeExecResult
			if err := json.Unmarshal([]byte(result), &execResult); err != nil {
				t.Fatalf("failed to unmarshal: %v", err)
			}
			if execResult.Error != "" && execResult.ExitCode == -1 {
				t.Errorf("safe command %q was blocked: %s", code, execResult.Error)
			}
		})
	}
}

func TestCodeExecTool_DenyPatterns_NotAppliedToPython(t *testing.T) {
	// Skip if python3 is not available
	if _, err := os.Stat("/usr/bin/python3"); os.IsNotExist(err) {
		t.Skip("python3 not available")
	}

	tmpDir := t.TempDir()
	tool := newInProcessCodeExecTestTool(tmpDir, 30*time.Second, map[string]bool{codeLanguagePython: true, python3BinaryName: true})

	// Python code containing "rm -rf" in a string should NOT be blocked
	args, _ := json.Marshal(CodeExecArgs{Language: codeLanguagePython, Code: `print("rm -rf / is dangerous")`})
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	var execResult CodeExecResult
	if err := json.Unmarshal([]byte(result), &execResult); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if execResult.ExitCode != 0 {
		t.Errorf("Python code should not be blocked by deny patterns, got exit code %d, error: %s", execResult.ExitCode, execResult.Error)
	}
}

func TestCodeExecTool_Execute_Node(t *testing.T) {
	// Test node execution directly via executeNode
	tmpDir := t.TempDir()
	tool := newInProcessCodeExecTestTool(tmpDir, 30*time.Second, nil)

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
	tool := newInProcessCodeExecTestTool(tmpDir, 30*time.Second, nil)

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
	tool := newInProcessCodeExecTestTool(tmpDir, 30*time.Second, nil)

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

func TestCodeExecTool_Execute_LocalUnixCPUUlimit(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("local ulimit wrapper is only used on Unix platforms")
	}

	t.Setenv(codeExecLocalCPUSecondsEnv, "7")
	tmpDir := t.TempDir()
	tool := newInProcessCodeExecTestTool(tmpDir, 30*time.Second, nil)

	args := json.RawMessage(`{"language": "sh", "code": "ulimit -t"}`)
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var execResult CodeExecResult
	if err := json.Unmarshal([]byte(result), &execResult); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}
	if execResult.ExitCode != 0 {
		t.Fatalf("exit_code = %d, want 0; error=%q", execResult.ExitCode, execResult.Error)
	}
	if strings.TrimSpace(execResult.Output) != "7" {
		t.Fatalf("ulimit -t output = %q, want 7", execResult.Output)
	}
}

func TestCodeExecTool_Execute_MixedCase(t *testing.T) {
	tmpDir := t.TempDir()
	tool := newInProcessCodeExecTestTool(tmpDir, 30*time.Second, nil)

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
	tool := newInProcessCodeExecTestTool(tmpDir, 0, nil)

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
	tool := newInProcessCodeExecTestTool(tmpDir, 30*time.Second, nil)

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

func TestCodeExecTool_Execute_EnvironmentScrubbed(t *testing.T) {
	t.Setenv("ORKA_SECRET_TOKEN", "super-secret-code-exec")
	tmpDir := t.TempDir()
	tool := newInProcessCodeExecTestTool(tmpDir, 30*time.Second, nil)

	args := json.RawMessage(`{"language": "bash", "code": "if [ -z \"$ORKA_SECRET_TOKEN\" ] && [ -n \"$PATH\" ]; then echo scrubbed; else echo leaked; fi"}`)
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var execResult CodeExecResult
	if err := json.Unmarshal([]byte(result), &execResult); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}
	if execResult.Output != "scrubbed\n" {
		t.Fatalf("expected scrubbed environment, output=%q error=%q", execResult.Output, execResult.Error)
	}
}

func TestCodeExecTool_Execute_OutputCapsStdoutAndStderr(t *testing.T) {
	tmpDir := t.TempDir()
	tool := newInProcessCodeExecTestTool(tmpDir, 30*time.Second, map[string]bool{codeLanguageBash: true})
	tool.outputLimitBytes = 10

	args := json.RawMessage(`{"language": "bash", "code": "printf '12345678901234567890'; printf 'abcdefghijabcdefghij' >&2"}`)
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var execResult CodeExecResult
	if err := json.Unmarshal([]byte(result), &execResult); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}
	if !execResult.OutputTruncated {
		t.Fatal("expected stdout truncation")
	}
	if !execResult.ErrorTruncated {
		t.Fatal("expected stderr truncation")
	}
	if !strings.HasPrefix(execResult.Output, "1234567890") {
		t.Errorf("stdout prefix = %q, want cap prefix", execResult.Output)
	}
	if strings.Contains(execResult.Output, "12345678901") {
		t.Errorf("stdout contains bytes beyond cap: %q", execResult.Output)
	}
	if !strings.HasPrefix(execResult.Error, "abcdefghij") {
		t.Errorf("stderr prefix = %q, want cap prefix", execResult.Error)
	}
	if !strings.Contains(execResult.Output, "truncated after 10 bytes") {
		t.Errorf("stdout should include truncation marker, got %q", execResult.Output)
	}
	if !strings.Contains(execResult.Error, "truncated after 10 bytes") {
		t.Errorf("stderr should include truncation marker, got %q", execResult.Error)
	}
}

func TestCodeExecTool_Execute_EmptyStdinEOF(t *testing.T) {
	tmpDir := t.TempDir()
	tool := newInProcessCodeExecTestTool(tmpDir, 30*time.Second, nil)

	args := json.RawMessage(`{"language": "bash", "code": "if read line; then echo got; else echo eof; fi"}`)
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var execResult CodeExecResult
	if err := json.Unmarshal([]byte(result), &execResult); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}
	if execResult.Output != "eof\n" {
		t.Fatalf("expected stdin EOF, output=%q error=%q", execResult.Output, execResult.Error)
	}
}

func TestCodeExecTool_Execute_RealTimeout(t *testing.T) {
	// Keep the process limit above busy developer/CI hosts so the child shell can
	// fork sleep and exercise timeout handling instead of failing at startup.
	t.Setenv(codeExecLocalMaxProcessesEnv, "1048576")

	tmpDir := t.TempDir()
	tool := newInProcessCodeExecTestTool(tmpDir, 30*time.Second, nil)

	args := json.RawMessage(`{"language": "bash", "code": "sleep 2; echo done", "timeout": 1}`)
	start := time.Now()
	result, err := tool.Execute(context.Background(), args)
	duration := time.Since(start)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if duration > 3*time.Second {
		t.Fatalf("timeout took too long: %v", duration)
	}

	var execResult CodeExecResult
	if err := json.Unmarshal([]byte(result), &execResult); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}
	if !execResult.TimedOut {
		t.Fatalf("expected timed_out=true, result=%+v", execResult)
	}
	if execResult.ExitCode != -1 {
		t.Errorf("exit_code = %d, want -1", execResult.ExitCode)
	}
	if !strings.Contains(execResult.Error, "timed out") {
		t.Errorf("expected timeout error, got %q", execResult.Error)
	}
	if strings.Contains(execResult.Output, "done") {
		t.Errorf("command should have been killed before completion, output=%q", execResult.Output)
	}
}

func TestCodeExecTool_BackendSelector(t *testing.T) {
	t.Setenv("ORKA_WORK_DIR", t.TempDir())

	t.Setenv(codeExecBackendEnv, "")
	tool := NewCodeExecTool()
	if tool.backend != codeExecBackendKubernetes {
		t.Fatalf("empty backend = %q, want %q", tool.backend, codeExecBackendKubernetes)
	}
	if _, ok := tool.executor.(*KubernetesJobCodeExecutor); !ok {
		t.Fatalf("empty backend executor type = %T, want *KubernetesJobCodeExecutor", tool.executor)
	}

	t.Setenv(codeExecBackendEnv, "in_process")
	tool = NewCodeExecTool()
	if tool.backend != codeExecBackendInProcess {
		t.Fatalf("backend = %q, want %q", tool.backend, codeExecBackendInProcess)
	}
	if _, ok := tool.executor.(*InProcessCodeExecutor); !ok {
		t.Fatalf("executor type = %T, want *InProcessCodeExecutor", tool.executor)
	}

	t.Setenv(codeExecBackendEnv, "unsupported-test-backend")
	tool = NewCodeExecTool()
	args := json.RawMessage(`{"language": "bash", "code": "echo should-not-run"}`)
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var execResult CodeExecResult
	if err := json.Unmarshal([]byte(result), &execResult); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}
	if execResult.ExitCode != -1 {
		t.Fatalf("exit_code = %d, want -1", execResult.ExitCode)
	}
	if !strings.Contains(execResult.Error, "unsupported code_exec backend") {
		t.Fatalf("expected unsupported backend error, got %q", execResult.Error)
	}
	if strings.Contains(execResult.Output, "should-not-run") {
		t.Fatalf("unsupported backend executed code, output=%q", execResult.Output)
	}
}

func TestCodeExecTool_ExecuteUsesConfiguredExecutorWithoutScopedOverride(t *testing.T) {
	provider := "test-provider-no-env"
	tenant := "test-tenant-no-env"
	t.Setenv(codeExecBackendEnv, "")
	for _, envName := range codeExecScopedEnvNames(codeExecBackendEnv, provider, tenant) {
		t.Setenv(envName, "")
	}

	recorder := &recordingCodeExecutor{result: CodeExecResult{Output: "from configured executor", ExitCode: 23}}
	tool := &CodeExecTool{
		workDir:          t.TempDir(),
		timeout:          time.Second,
		allowedLangs:     defaultCodeExecAllowedLangs(),
		denyPatterns:     defaultDenyPatterns,
		executor:         recorder,
		backend:          codeExecBackendKubernetes,
		outputLimitBytes: 123,
	}
	ctx := WithToolContext(context.Background(), &ToolContext{
		Tenant:       tenant,
		ProviderType: provider,
	})

	result, err := tool.Execute(ctx, json.RawMessage(`{"language":"python","code":"print('should not run')"}`))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if recorder.calls != 1 {
		t.Fatalf("configured executor calls = %d, want 1", recorder.calls)
	}
	if recorder.req.Backend != codeExecBackendKubernetes {
		t.Fatalf("request backend = %q, want %q", recorder.req.Backend, codeExecBackendKubernetes)
	}
	if recorder.req.Language != codeLanguagePython || recorder.req.Code != "print('should not run')" {
		t.Fatalf("unexpected recorded request: %+v", recorder.req)
	}
	if recorder.req.Tenant != tenant || recorder.req.ProviderType != provider {
		t.Fatalf("request scope tenant/provider_type = %q/%q, want %q/%q", recorder.req.Tenant, recorder.req.ProviderType, tenant, provider)
	}
	if recorder.req.OutputLimitBytes != 123 {
		t.Fatalf("OutputLimitBytes = %d, want 123", recorder.req.OutputLimitBytes)
	}

	var execResult CodeExecResult
	if err := json.Unmarshal([]byte(result), &execResult); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}
	if execResult.Output != "from configured executor" || execResult.ExitCode != 23 {
		t.Fatalf("result = %+v, want configured executor result", execResult)
	}
}

func TestCodeExecTool_ExecuteScopedBackendOverrideBypassesConfiguredExecutor(t *testing.T) {
	provider := "test provider override"
	tenant := "test tenant override"
	t.Setenv(codeExecBackendEnv, "")
	for _, envName := range codeExecScopedEnvNames(codeExecBackendEnv, provider, tenant) {
		t.Setenv(envName, "")
	}
	t.Setenv(codeExecBackendEnv+"_PROVIDER_TEST_PROVIDER_OVERRIDE_TENANT_TEST_TENANT_OVERRIDE", "in_process")

	recorder := &recordingCodeExecutor{result: CodeExecResult{Output: "should not be used", ExitCode: 99}}
	tool := &CodeExecTool{
		workDir:      t.TempDir(),
		timeout:      time.Second,
		allowedLangs: defaultCodeExecAllowedLangs(),
		denyPatterns: defaultDenyPatterns,
		executor:     recorder,
		backend:      codeExecBackendKubernetes,
	}
	ctx := WithToolContext(context.Background(), &ToolContext{
		Tenant:   tenant,
		Provider: provider,
	})

	result, err := tool.Execute(ctx, json.RawMessage(`{"language":"bash","code":"echo scoped-override"}`))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if recorder.calls != 0 {
		t.Fatalf("configured executor calls = %d, want 0 after scoped backend override", recorder.calls)
	}

	var execResult CodeExecResult
	if err := json.Unmarshal([]byte(result), &execResult); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}
	if execResult.ExitCode != 0 {
		t.Fatalf("exit_code = %d, want 0; error=%q", execResult.ExitCode, execResult.Error)
	}
	if execResult.Output != "scoped-override\n" {
		t.Fatalf("output = %q, want scoped override output", execResult.Output)
	}
}

type captureLogSink struct {
	entries strings.Builder
}

func (s *captureLogSink) Init(logr.RuntimeInfo) {}

func (s *captureLogSink) Enabled(_ int) bool { return true }

func (s *captureLogSink) Info(_ int, msg string, keysAndValues ...any) {
	s.entries.WriteString(msg)
	for _, kv := range keysAndValues {
		s.entries.WriteString(" ")
		fmt.Fprint(&s.entries, kv)
	}
	s.entries.WriteString("\n")
}

func (s *captureLogSink) Error(err error, msg string, keysAndValues ...any) {
	s.Info(0, msg, append([]any{"error", err}, keysAndValues...)...)
}

func (s *captureLogSink) WithValues(keysAndValues ...any) logr.LogSink { return s }

func (s *captureLogSink) WithName(name string) logr.LogSink { return s }

func (s *captureLogSink) String() string { return s.entries.String() }

func TestCodeExecTool_AuditLogRedactsCodeAndOutput(t *testing.T) {
	t.Setenv(codeExecLocalCPUSecondsEnv, "7")
	t.Setenv(codeExecLocalMemoryKBEnv, "2048")
	t.Setenv(codeExecLocalMaxProcessesEnv, "512")

	tmpDir := t.TempDir()
	tool := newInProcessCodeExecTestTool(tmpDir, 30*time.Second, nil)
	sink := &captureLogSink{}
	ctx := log.IntoContext(context.Background(), logr.New(sink))
	ctx = WithToolContext(ctx, &ToolContext{
		SessionID:  "session-123",
		ToolCallID: "tool-call-456",
		Namespace:  "tenant-ns",
	})

	args := json.RawMessage(`{"language": "bash", "code": "echo raw-code-secret-123; echo stderr-secret-456 >&2"}`)
	result, err := tool.Execute(ctx, args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	var execResult CodeExecResult
	if err := json.Unmarshal([]byte(result), &execResult); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}
	if !strings.Contains(execResult.Output, "raw-code-secret-123") {
		t.Fatalf("test setup expected stdout secret in result, got %q", execResult.Output)
	}
	if !strings.Contains(execResult.Error, "stderr-secret-456") {
		t.Fatalf("test setup expected stderr secret in result, got %q", execResult.Error)
	}

	logs := sink.String()
	for _, secret := range []string{"raw-code-secret-123", "stderr-secret-456", "echo raw-code-secret"} {
		if strings.Contains(logs, secret) {
			t.Fatalf("audit log leaked %q: %s", secret, logs)
		}
	}
	for _, field := range []string{
		"code_exec audit",
		"code_sha256",
		"code_bytes",
		"stdout_bytes",
		"stderr_bytes",
		"exit_code",
		"session_id",
		"session-123",
		"tool_call_id",
		"tool-call-456",
		"tenant",
		"tenant-ns",
		"cpu_seconds_limit",
		"7",
		"memory_kb_limit",
		"2048",
		"max_processes",
		"512",
	} {
		if !strings.Contains(logs, field) {
			t.Fatalf("audit log missing %q: %s", field, logs)
		}
	}
}

func TestCodeExecTool_BackendSelector_KubernetesAliases(t *testing.T) {
	for _, backend := range []string{"kubernetes", "k8s", jobField} {
		t.Run(backend, func(t *testing.T) {
			executor, normalized := newCodeExecutorFromBackend(backend)
			if normalized != codeExecBackendKubernetes {
				t.Fatalf("backend = %q, want %q", normalized, codeExecBackendKubernetes)
			}
			if _, ok := executor.(*KubernetesJobCodeExecutor); !ok {
				t.Fatalf("executor type = %T, want *KubernetesJobCodeExecutor", executor)
			}
		})
	}
}

func TestKubernetesJobCodeExecutor_ExecuteSuccessWithFakeClient(t *testing.T) {
	setKubernetesCodeExecTestEnv(t)
	t.Setenv("ORKA_SECRET_TOKEN", "must-not-appear-in-job-env")

	fakeClient := newKubernetesCodeExecFakeClient(t)
	namespace := testNamespace
	jobName := codeExecKubernetesJobPrefix + successStatusString
	podName := "pod-success"
	code := "print('k8s-raw-code-secret-123')"
	resourcesCh := make(chan *fakeKubernetesCodeExecResources, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errCh := finishFakeKubernetesCodeExecJob(ctx, fakeClient, namespace, jobName, podName, batchv1.JobStatus{
		Succeeded: 1,
		Conditions: []batchv1.JobCondition{
			{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, Reason: "Completed"},
		},
	}, 0, resourcesCh)

	executor := &KubernetesJobCodeExecutor{
		resolveClients: func(context.Context) (kubernetesCodeExecClients, error) {
			return kubernetesCodeExecClients{client: fakeClient, namespace: namespace}, nil
		},
		logStreamer:  fakePodLogStreamer{logs: map[string]string{podName: fakeKubernetesCodeExecLogs(jobName, "hello from kubernetes\n", "")}},
		pollInterval: time.Millisecond,
		randomSuffix: func() string { return successStatusString },
	}

	result := executor.Execute(ctx, CodeExecutionRequest{
		Backend:          codeExecBackendKubernetes,
		Language:         codeLanguagePython,
		Code:             code,
		Timeout:          time.Second,
		DenyPatterns:     defaultDenyPatterns,
		OutputLimitBytes: defaultCodeExecOutputLimitBytes,
	})
	requireFakeKubernetesCompletion(t, errCh)

	if result.ExitCode != 0 {
		t.Fatalf("exit_code = %d, want 0; error=%q output=%q", result.ExitCode, result.Error, result.Output)
	}
	if result.Error != "" {
		t.Fatalf("error = %q, want empty", result.Error)
	}
	if result.Output != "hello from kubernetes\n" {
		t.Fatalf("output = %q, want fake pod logs", result.Output)
	}
	if result.TimedOut {
		t.Fatal("timed_out = true, want false")
	}

	createdResources := requireFakeKubernetesResources(t, resourcesCh)
	assertKubernetesCodeExecResourcesHardened(t, createdResources, code)
	assertFakeKubernetesCodeExecResourcesDeleted(t, fakeClient, namespace, jobName)
}

func TestKubernetesJobCodeExecutor_ExecuteFailureWithFakeClient(t *testing.T) {
	setKubernetesCodeExecTestEnv(t)

	fakeClient := newKubernetesCodeExecFakeClient(t)
	namespace := testNamespace
	jobName := codeExecKubernetesJobPrefix + "failure"
	podName := "pod-failure"
	resourcesCh := make(chan *fakeKubernetesCodeExecResources, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errCh := finishFakeKubernetesCodeExecJob(ctx, fakeClient, namespace, jobName, podName, batchv1.JobStatus{
		Failed: 1,
		Conditions: []batchv1.JobCondition{
			{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: taskPhaseFailedString, Message: "container failed"},
		},
	}, 7, resourcesCh)

	executor := &KubernetesJobCodeExecutor{
		resolveClients: func(context.Context) (kubernetesCodeExecClients, error) {
			return kubernetesCodeExecClients{client: fakeClient, namespace: namespace}, nil
		},
		logStreamer:  fakePodLogStreamer{logs: map[string]string{podName: fakeKubernetesCodeExecLogs(jobName, "failure output\n", "failure stderr\n")}},
		pollInterval: time.Millisecond,
		randomSuffix: func() string { return "failure" },
	}

	result := executor.Execute(ctx, CodeExecutionRequest{
		Backend:          codeExecBackendKubernetes,
		Language:         codeLanguageBash,
		Code:             "echo failure; exit 7",
		Timeout:          time.Second,
		DenyPatterns:     defaultDenyPatterns,
		OutputLimitBytes: defaultCodeExecOutputLimitBytes,
	})
	requireFakeKubernetesCompletion(t, errCh)
	_ = requireFakeKubernetesResources(t, resourcesCh)

	if result.ExitCode != 7 {
		t.Fatalf("exit_code = %d, want 7; error=%q output=%q", result.ExitCode, result.Error, result.Output)
	}
	if !strings.Contains(result.Error, "container failed") {
		t.Fatalf("error = %q, want job failure message", result.Error)
	}
	if !strings.Contains(result.Error, "failure stderr\n") {
		t.Fatalf("error = %q, want stderr logs", result.Error)
	}
	if result.Output != "failure output\n" {
		t.Fatalf("output = %q, want fake pod logs", result.Output)
	}
	if result.TimedOut {
		t.Fatal("timed_out = true, want false")
	}
	assertFakeKubernetesCodeExecResourcesDeleted(t, fakeClient, namespace, jobName)
}

func TestKubernetesJobCodeExecutor_ExecuteTimeoutWithFakeClient(t *testing.T) {
	setKubernetesCodeExecTestEnv(t)

	fakeClient := newKubernetesCodeExecFakeClient(t)
	namespace := testNamespace
	jobName := codeExecKubernetesJobPrefix + timeoutField

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()

	executor := &KubernetesJobCodeExecutor{
		resolveClients: func(context.Context) (kubernetesCodeExecClients, error) {
			return kubernetesCodeExecClients{client: fakeClient, namespace: namespace}, nil
		},
		logStreamer:  fakePodLogStreamer{},
		pollInterval: time.Millisecond,
		randomSuffix: func() string { return timeoutField },
	}

	result := executor.Execute(ctx, CodeExecutionRequest{
		Backend:          codeExecBackendKubernetes,
		Language:         codeLanguageNode,
		Code:             "setTimeout(() => console.log('late'), 1000)",
		Timeout:          time.Second,
		DenyPatterns:     defaultDenyPatterns,
		OutputLimitBytes: defaultCodeExecOutputLimitBytes,
	})

	if !result.TimedOut {
		t.Fatalf("timed_out = false, want true; result=%+v", result)
	}
	if result.ExitCode != -1 {
		t.Fatalf("exit_code = %d, want -1", result.ExitCode)
	}
	if !strings.Contains(result.Error, "execution timed out") {
		t.Fatalf("error = %q, want timeout message", result.Error)
	}

	assertFakeKubernetesCodeExecResourcesDeleted(t, fakeClient, namespace, jobName)
}

func TestKubernetesJobCodeExecutor_ExecuteDeadlineExceededWithFakeClient(t *testing.T) {
	setKubernetesCodeExecTestEnv(t)

	fakeClient := newKubernetesCodeExecFakeClient(t)
	namespace := testNamespace
	jobName := codeExecKubernetesJobPrefix + "deadline"
	podName := "pod-deadline"

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errCh := finishFakeKubernetesCodeExecJob(ctx, fakeClient, namespace, jobName, podName, batchv1.JobStatus{
		Failed: 1,
		Conditions: []batchv1.JobCondition{
			{Type: batchv1.JobConditionType("FailureTarget"), Status: corev1.ConditionTrue, Reason: "DeadlineExceeded", Message: "Job was active longer than specified deadline"},
		},
	}, 137, nil)

	executor := &KubernetesJobCodeExecutor{
		resolveClients: func(context.Context) (kubernetesCodeExecClients, error) {
			return kubernetesCodeExecClients{client: fakeClient, namespace: namespace}, nil
		},
		logStreamer:  fakePodLogStreamer{logs: map[string]string{podName: fakeKubernetesCodeExecLogs(jobName, "timeout output\n", "")}},
		pollInterval: time.Millisecond,
		randomSuffix: func() string { return "deadline" },
	}

	result := executor.Execute(ctx, CodeExecutionRequest{
		Backend:          codeExecBackendKubernetes,
		Language:         codeLanguageBash,
		Code:             "echo timeout output; sleep 20",
		Timeout:          time.Second,
		DenyPatterns:     defaultDenyPatterns,
		OutputLimitBytes: defaultCodeExecOutputLimitBytes,
	})
	requireFakeKubernetesCompletion(t, errCh)

	if !result.TimedOut {
		t.Fatalf("timed_out = false, want true; result=%+v", result)
	}
	if result.ExitCode != -1 {
		t.Fatalf("exit_code = %d, want -1", result.ExitCode)
	}
	if result.Output != "timeout output\n" {
		t.Fatalf("output = %q, want preserved partial output", result.Output)
	}
	if !strings.Contains(result.Error, "execution timed out") {
		t.Fatalf("error = %q, want timeout message", result.Error)
	}

	assertFakeKubernetesCodeExecResourcesDeleted(t, fakeClient, namespace, jobName)
}

func TestKubernetesJobCodeExecutor_BlocksDeniedBashBeforeCreatingJob(t *testing.T) {
	calledResolver := false
	executor := &KubernetesJobCodeExecutor{
		resolveClients: func(context.Context) (kubernetesCodeExecClients, error) {
			calledResolver = true
			return kubernetesCodeExecClients{}, nil
		},
	}

	result := executor.Execute(context.Background(), CodeExecutionRequest{
		Backend:      codeExecBackendKubernetes,
		Language:     codeLanguageBash,
		Code:         "rm -rf /",
		Timeout:      time.Second,
		DenyPatterns: defaultDenyPatterns,
	})

	if calledResolver {
		t.Fatal("expected denied bash code to return before resolving Kubernetes clients")
	}
	if result.ExitCode != -1 {
		t.Fatalf("exit_code = %d, want -1", result.ExitCode)
	}
	if !strings.Contains(result.Error, "command blocked") {
		t.Fatalf("error = %q, want deny-pattern error", result.Error)
	}
}

func TestKubernetesJobCodeExecutor_BuildResourcesRuntimeClassName(t *testing.T) {
	setKubernetesCodeExecTestEnv(t)
	t.Setenv(codeExecKubernetesRuntimeClassNameEnv, "  gvisor  ")

	executor := &KubernetesJobCodeExecutor{randomSuffix: func() string { return "runtime-class" }}
	resources, err := executor.buildResources(testNamespace, CodeExecutionRequest{
		Language:         codeLanguagePython,
		Code:             "print('runtime')",
		Timeout:          time.Second,
		OutputLimitBytes: defaultCodeExecOutputLimitBytes,
	})
	if err != nil {
		t.Fatalf("buildResources() error = %v", err)
	}
	if resources.job.Spec.Template.Spec.RuntimeClassName == nil {
		t.Fatal("RuntimeClassName = nil, want gvisor")
	}
	if *resources.job.Spec.Template.Spec.RuntimeClassName != "gvisor" {
		t.Fatalf("RuntimeClassName = %q, want gvisor", *resources.job.Spec.Template.Spec.RuntimeClassName)
	}
}

func TestKubernetesJobCodeExecutor_BuildResourcesOmitsAppArmorByDefault(t *testing.T) {
	setKubernetesCodeExecTestEnv(t)

	executor := &KubernetesJobCodeExecutor{randomSuffix: func() string { return "apparmor-default" }}
	resources, err := executor.buildResources(testNamespace, CodeExecutionRequest{
		Language:         codeLanguagePython,
		Code:             "print('apparmor default')",
		Timeout:          time.Second,
		OutputLimitBytes: defaultCodeExecOutputLimitBytes,
	})
	if err != nil {
		t.Fatalf("buildResources() error = %v", err)
	}
	podSpec := resources.job.Spec.Template.Spec
	if podSpec.SecurityContext.AppArmorProfile != nil {
		t.Fatalf("pod AppArmorProfile = %#v, want nil by default", podSpec.SecurityContext.AppArmorProfile)
	}
	containerSecurityContext := podSpec.Containers[0].SecurityContext
	if containerSecurityContext.AppArmorProfile != nil {
		t.Fatalf("container AppArmorProfile = %#v, want nil by default", containerSecurityContext.AppArmorProfile)
	}
}

func TestKubernetesJobCodeExecutor_BuildResourcesAppArmorRuntimeDefaultOptIn(t *testing.T) {
	setKubernetesCodeExecTestEnv(t)
	t.Setenv(codeExecKubernetesAppArmorProfileEnv, " runtime/default ")

	executor := &KubernetesJobCodeExecutor{randomSuffix: func() string { return "apparmor-runtime" }}
	resources, err := executor.buildResources(testNamespace, CodeExecutionRequest{
		Language:         codeLanguagePython,
		Code:             "print('apparmor runtime default')",
		Timeout:          time.Second,
		OutputLimitBytes: defaultCodeExecOutputLimitBytes,
	})
	if err != nil {
		t.Fatalf("buildResources() error = %v", err)
	}
	podSpec := resources.job.Spec.Template.Spec
	if podSpec.SecurityContext.AppArmorProfile == nil || podSpec.SecurityContext.AppArmorProfile.Type != corev1.AppArmorProfileTypeRuntimeDefault {
		t.Fatalf("pod AppArmorProfile = %#v, want RuntimeDefault", podSpec.SecurityContext.AppArmorProfile)
	}
	containerSecurityContext := podSpec.Containers[0].SecurityContext
	if containerSecurityContext.AppArmorProfile == nil || containerSecurityContext.AppArmorProfile.Type != corev1.AppArmorProfileTypeRuntimeDefault {
		t.Fatalf("container AppArmorProfile = %#v, want RuntimeDefault", containerSecurityContext.AppArmorProfile)
	}
}

func TestParseKubernetesCodeExecLogsSplitsStreamsAndTruncation(t *testing.T) {
	jobName := codeExecKubernetesJobPrefix + "parse"
	markers := codeExecKubernetesLogMarkers(jobName)
	rawLogs := fmt.Sprintf("%s\nhello\n%s\n%s\nwarn%s 4 6\n%s\n",
		markers.stdoutStart,
		markers.stdoutEnd,
		markers.stderrStart,
		"\n"+markers.stderrTruncated,
		markers.stderrEnd,
	)

	logs := parseKubernetesCodeExecLogs(rawLogs, jobName, 10)
	if logs.stdout != testHelloText {
		t.Fatalf("stdout = %q, want hello", logs.stdout)
	}
	if logs.stdoutTruncated {
		t.Fatal("stdoutTruncated = true, want false")
	}
	if !logs.stderrTruncated {
		t.Fatal("stderrTruncated = false, want true")
	}
	if !strings.Contains(logs.stderr, "warn") || !strings.Contains(logs.stderr, "truncated after 4 bytes") {
		t.Fatalf("stderr = %q, want stderr content and truncation message", logs.stderr)
	}
}

type fakeKubernetesCodeExecResources struct {
	job            *batchv1.Job
	secret         *corev1.Secret
	serviceAccount *corev1.ServiceAccount
	networkPolicy  *networkingv1.NetworkPolicy
}

type fakePodLogStreamer struct {
	logs map[string]string
	err  error
}

func (s fakePodLogStreamer) Stream(_ context.Context, _, podName string, _ *corev1.PodLogOptions) (io.ReadCloser, error) {
	if s.err != nil {
		return nil, s.err
	}
	return io.NopCloser(strings.NewReader(s.logs[podName])), nil
}

func fakeKubernetesCodeExecLogs(jobName, stdout, stderr string) string {
	markers := codeExecKubernetesLogMarkers(jobName)
	return fmt.Sprintf("%s\n%s\n%s\n%s\n%s\n%s\n",
		markers.stdoutStart,
		stdout,
		markers.stdoutEnd,
		markers.stderrStart,
		stderr,
		markers.stderrEnd,
	)
}

func newKubernetesCodeExecFakeClient(t *testing.T) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add corev1 to scheme: %v", err)
	}
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add batchv1 to scheme: %v", err)
	}
	if err := networkingv1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add networkingv1 to scheme: %v", err)
	}
	return crfake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&batchv1.Job{}).Build()
}

func setKubernetesCodeExecTestEnv(t *testing.T) {
	t.Helper()
	for _, envName := range []string{
		codeExecKubernetesImageEnv,
		codeExecKubernetesPythonImageEnv,
		codeExecKubernetesNodeImageEnv,
		codeExecKubernetesBashImageEnv,
		codeExecKubernetesRuntimeClassNameEnv,
		codeExecKubernetesAppArmorProfileEnv,
	} {
		t.Setenv(envName, "")
	}
	t.Setenv(codeExecKubernetesCPURequestEnv, "50m")
	t.Setenv(codeExecKubernetesCPULimitEnv, "500m")
	t.Setenv(codeExecKubernetesMemoryRequestEnv, "64Mi")
	t.Setenv(codeExecKubernetesMemoryLimitEnv, "256Mi")
	t.Setenv(codeExecKubernetesNetworkPolicyEnv, "")
}

func finishFakeKubernetesCodeExecJob(ctx context.Context, c client.Client, namespace, jobName, podName string, status batchv1.JobStatus, exitCode int32, resourcesCh chan<- *fakeKubernetesCodeExecResources) <-chan error {
	errCh := make(chan error, 1)
	go func() {
		job, err := waitForFakeKubernetesCodeExecJob(ctx, c, namespace, jobName)
		if err != nil {
			errCh <- err
			return
		}
		if resourcesCh != nil {
			resources, err := readFakeKubernetesCodeExecResources(ctx, c, namespace, jobName)
			if err != nil {
				errCh <- err
				return
			}
			resourcesCh <- resources
		}

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:              podName,
				Namespace:         namespace,
				Labels:            map[string]string{codeExecKubernetesLabelJob: jobName},
				CreationTimestamp: metav1.Now(),
			},
			Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{
					{
						Name: codeExecKubernetesContainerName,
						State: corev1.ContainerState{
							Terminated: &corev1.ContainerStateTerminated{ExitCode: exitCode},
						},
					},
				},
			},
		}
		if err := c.Create(ctx, pod); err != nil {
			errCh <- fmt.Errorf("failed to create fake pod: %w", err)
			return
		}

		job.Status = status
		if err := c.Status().Update(ctx, job); err != nil {
			errCh <- fmt.Errorf("failed to update fake job status: %w", err)
			return
		}
		errCh <- nil
	}()
	return errCh
}

func waitForFakeKubernetesCodeExecJob(ctx context.Context, c client.Client, namespace, jobName string) (*batchv1.Job, error) {
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		job := &batchv1.Job{}
		err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: jobName}, job)
		if err == nil {
			return job, nil
		}
		if !apierrors.IsNotFound(err) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func readFakeKubernetesCodeExecResources(ctx context.Context, c client.Client, namespace, jobName string) (*fakeKubernetesCodeExecResources, error) {
	resources := &fakeKubernetesCodeExecResources{
		job:            &batchv1.Job{},
		secret:         &corev1.Secret{},
		serviceAccount: &corev1.ServiceAccount{},
		networkPolicy:  &networkingv1.NetworkPolicy{},
	}
	key := types.NamespacedName{Namespace: namespace, Name: jobName}
	if err := c.Get(ctx, key, resources.job); err != nil {
		return nil, fmt.Errorf("failed to get fake job: %w", err)
	}
	if err := c.Get(ctx, key, resources.secret); err != nil {
		return nil, fmt.Errorf("failed to get fake secret: %w", err)
	}
	if err := c.Get(ctx, key, resources.serviceAccount); err != nil {
		return nil, fmt.Errorf("failed to get fake service account: %w", err)
	}
	if err := c.Get(ctx, key, resources.networkPolicy); err != nil {
		return nil, fmt.Errorf("failed to get fake network policy: %w", err)
	}
	return resources, nil
}

func requireFakeKubernetesCompletion(t *testing.T, errCh <-chan error) {
	t.Helper()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("fake Kubernetes completion failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for fake Kubernetes completion")
	}
}

func requireFakeKubernetesResources(t *testing.T, resourcesCh <-chan *fakeKubernetesCodeExecResources) *fakeKubernetesCodeExecResources {
	t.Helper()
	select {
	case resources := <-resourcesCh:
		return resources
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for created fake Kubernetes resources")
	}
	return nil
}

func assertFakeKubernetesCodeExecResourcesDeleted(t *testing.T, c client.Client, namespace, jobName string) {
	t.Helper()
	key := types.NamespacedName{Namespace: namespace, Name: jobName}
	checks := []struct {
		name string
		obj  client.Object
	}{
		{name: jobField, obj: &batchv1.Job{}},
		{name: "secret", obj: &corev1.Secret{}},
		{name: "service account", obj: &corev1.ServiceAccount{}},
		{name: "network policy", obj: &networkingv1.NetworkPolicy{}},
	}
	for _, check := range checks {
		err := c.Get(context.Background(), key, check.obj)
		if !apierrors.IsNotFound(err) {
			t.Fatalf("expected cleanup to delete %s %s/%s, got err=%v", check.name, namespace, jobName, err)
		}
	}
}

func assertKubernetesCodeExecResourcesHardened(t *testing.T, resources *fakeKubernetesCodeExecResources, expectedCode string) {
	t.Helper()
	if resources == nil {
		t.Fatal("resources are nil")
	}
	assertKubernetesCodeExecJobHardened(t, resources.job, resources.secret, resources.serviceAccount, expectedCode)
	assertKubernetesCodeExecSecretHardened(t, resources.secret, expectedCode)
	assertKubernetesCodeExecServiceAccountHardened(t, resources.serviceAccount)
	assertKubernetesCodeExecNetworkPolicyHardened(t, resources.networkPolicy, resources.job.Name)
}

//nolint:gocyclo // Keeping these Kubernetes hardening assertions together makes test failures easier to diagnose.
func assertKubernetesCodeExecJobHardened(t *testing.T, job *batchv1.Job, secret *corev1.Secret, serviceAccount *corev1.ServiceAccount, expectedCode string) {
	t.Helper()
	if job == nil {
		t.Fatal("job is nil")
	}
	if secret == nil {
		t.Fatal("secret is nil")
	}
	if serviceAccount == nil {
		t.Fatal("service account is nil")
	}
	jobJSON, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("failed to marshal job: %v", err)
	}
	if strings.Contains(string(jobJSON), expectedCode) {
		t.Fatalf("job JSON leaked raw code %q: %s", expectedCode, string(jobJSON))
	}
	if strings.Contains(string(jobJSON), "ORKA_SECRET_TOKEN") {
		t.Fatalf("job JSON leaked parent ORKA_SECRET_TOKEN: %s", string(jobJSON))
	}

	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Fatalf("BackoffLimit = %v, want 0", job.Spec.BackoffLimit)
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds < 1 {
		t.Fatalf("ActiveDeadlineSeconds = %v, want >= 1", job.Spec.ActiveDeadlineSeconds)
	}
	if job.Spec.TTLSecondsAfterFinished == nil || *job.Spec.TTLSecondsAfterFinished != codeExecKubernetesFinishedTTLSeconds {
		t.Fatalf("TTLSecondsAfterFinished = %v, want %d", job.Spec.TTLSecondsAfterFinished, codeExecKubernetesFinishedTTLSeconds)
	}

	podSpec := job.Spec.Template.Spec
	if podSpec.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatalf("RestartPolicy = %q, want Never", podSpec.RestartPolicy)
	}
	if podSpec.ServiceAccountName != serviceAccount.Name {
		t.Fatalf("ServiceAccountName = %q, want %q", podSpec.ServiceAccountName, serviceAccount.Name)
	}
	if podSpec.AutomountServiceAccountToken == nil || *podSpec.AutomountServiceAccountToken {
		t.Fatalf("AutomountServiceAccountToken = %v, want false", podSpec.AutomountServiceAccountToken)
	}
	if podSpec.SecurityContext == nil {
		t.Fatal("pod security context is nil")
	}
	if podSpec.SecurityContext.RunAsNonRoot == nil || !*podSpec.SecurityContext.RunAsNonRoot {
		t.Fatalf("pod RunAsNonRoot = %v, want true", podSpec.SecurityContext.RunAsNonRoot)
	}
	if podSpec.SecurityContext.RunAsUser == nil || *podSpec.SecurityContext.RunAsUser == 0 {
		t.Fatalf("pod RunAsUser = %v, want non-zero", podSpec.SecurityContext.RunAsUser)
	}
	if podSpec.SecurityContext.SeccompProfile == nil || podSpec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("pod SeccompProfile = %v, want RuntimeDefault", podSpec.SecurityContext.SeccompProfile)
	}
	if podSpec.SecurityContext.AppArmorProfile != nil {
		t.Fatalf("pod AppArmorProfile = %v, want nil by default", podSpec.SecurityContext.AppArmorProfile)
	}
	if len(podSpec.Containers) != 1 {
		t.Fatalf("containers = %d, want 1", len(podSpec.Containers))
	}

	container := podSpec.Containers[0]
	if container.Name != codeExecKubernetesContainerName {
		t.Fatalf("container name = %q, want %q", container.Name, codeExecKubernetesContainerName)
	}
	if container.Image == "" {
		t.Fatal("container image is empty")
	}
	if !hasString(container.Command, codeExecKubernetesCodePath) {
		t.Fatalf("container command = %+v, want reference to %q", container.Command, codeExecKubernetesCodePath)
	}
	if strings.Contains(strings.Join(container.Command, " "), expectedCode) {
		t.Fatalf("container command leaked raw code: %+v", container.Command)
	}
	if len(container.Resources.Requests) == 0 || len(container.Resources.Limits) == 0 {
		t.Fatalf("container resources must include requests and limits: %+v", container.Resources)
	}
	if !hasVolumeMount(container.VolumeMounts, "tmp", tempDirPath) {
		t.Fatalf("container volume mounts = %+v, want tmp mounted at /tmp", container.VolumeMounts)
	}
	if !hasReadOnlyVolumeMount(container.VolumeMounts, codeExecKubernetesCodeVolumeName, codeExecKubernetesCodeMountPath) {
		t.Fatalf("container volume mounts = %+v, want read-only code Secret mounted at %s", container.VolumeMounts, codeExecKubernetesCodeMountPath)
	}

	env := map[string]string{}
	for _, envVar := range container.Env {
		env[envVar.Name] = envVar.Value
	}
	if value, ok := env["ORKA_CODE_EXEC_CODE"]; ok {
		t.Fatalf("ORKA_CODE_EXEC_CODE env = %q, want absent", value)
	}
	if _, ok := env["ORKA_SECRET_TOKEN"]; ok {
		t.Fatal("container env leaked parent ORKA_SECRET_TOKEN")
	}
	if env["HOME"] != tempDirPath || env["TERM"] != "dumb" {
		t.Fatalf("container env should be scrubbed, got HOME=%q TERM=%q", env["HOME"], env["TERM"])
	}

	securityContext := container.SecurityContext
	if securityContext == nil {
		t.Fatal("container security context is nil")
	}
	if securityContext.RunAsNonRoot == nil || !*securityContext.RunAsNonRoot {
		t.Fatalf("container RunAsNonRoot = %v, want true", securityContext.RunAsNonRoot)
	}
	if securityContext.ReadOnlyRootFilesystem == nil || !*securityContext.ReadOnlyRootFilesystem {
		t.Fatalf("ReadOnlyRootFilesystem = %v, want true", securityContext.ReadOnlyRootFilesystem)
	}
	if securityContext.AllowPrivilegeEscalation == nil || *securityContext.AllowPrivilegeEscalation {
		t.Fatalf("AllowPrivilegeEscalation = %v, want false", securityContext.AllowPrivilegeEscalation)
	}
	if securityContext.Capabilities == nil || !hasDroppedCapability(securityContext.Capabilities.Drop, "ALL") {
		t.Fatalf("capabilities drop = %+v, want ALL", securityContext.Capabilities)
	}
	if securityContext.SeccompProfile == nil || securityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("container SeccompProfile = %v, want RuntimeDefault", securityContext.SeccompProfile)
	}
	if securityContext.AppArmorProfile != nil {
		t.Fatalf("container AppArmorProfile = %v, want nil by default", securityContext.AppArmorProfile)
	}

	if !hasEmptyDirVolume(podSpec.Volumes, "tmp") {
		t.Fatalf("pod volumes = %+v, want tmp EmptyDir", podSpec.Volumes)
	}
	if !hasSecretVolume(podSpec.Volumes, codeExecKubernetesCodeVolumeName, secret.Name) {
		t.Fatalf("pod volumes = %+v, want code Secret volume %q", podSpec.Volumes, secret.Name)
	}
}

func assertKubernetesCodeExecSecretHardened(t *testing.T, secret *corev1.Secret, expectedCode string) {
	t.Helper()
	if secret == nil {
		t.Fatal("secret is nil")
	}
	if secret.Type != corev1.SecretTypeOpaque {
		t.Fatalf("secret type = %q, want Opaque", secret.Type)
	}
	if string(secret.Data[codeExecKubernetesCodeVolumeName]) != expectedCode {
		t.Fatalf("secret code data = %q, want expected raw code", string(secret.Data[codeExecKubernetesCodeVolumeName]))
	}
}

func assertKubernetesCodeExecServiceAccountHardened(t *testing.T, serviceAccount *corev1.ServiceAccount) {
	t.Helper()
	if serviceAccount == nil {
		t.Fatal("service account is nil")
	}
	if serviceAccount.AutomountServiceAccountToken == nil || *serviceAccount.AutomountServiceAccountToken {
		t.Fatalf("service account AutomountServiceAccountToken = %v, want false", serviceAccount.AutomountServiceAccountToken)
	}
}

func assertKubernetesCodeExecNetworkPolicyHardened(t *testing.T, networkPolicy *networkingv1.NetworkPolicy, jobName string) {
	t.Helper()
	if networkPolicy == nil {
		t.Fatal("network policy is nil")
	}
	if networkPolicy.Spec.PodSelector.MatchLabels[codeExecKubernetesLabelJob] != jobName {
		t.Fatalf("network policy selector = %+v, want %s=%s", networkPolicy.Spec.PodSelector.MatchLabels, codeExecKubernetesLabelJob, jobName)
	}
	if !hasPolicyType(networkPolicy.Spec.PolicyTypes, networkingv1.PolicyTypeIngress) {
		t.Fatalf("network policy types = %+v, want Ingress", networkPolicy.Spec.PolicyTypes)
	}
	if !hasPolicyType(networkPolicy.Spec.PolicyTypes, networkingv1.PolicyTypeEgress) {
		t.Fatalf("network policy types = %+v, want Egress", networkPolicy.Spec.PolicyTypes)
	}
	if len(networkPolicy.Spec.Ingress) != 0 {
		t.Fatalf("network policy ingress rules = %+v, want deny-all", networkPolicy.Spec.Ingress)
	}
	if len(networkPolicy.Spec.Egress) != 0 {
		t.Fatalf("network policy egress rules = %+v, want deny-all", networkPolicy.Spec.Egress)
	}
}

func hasVolumeMount(mounts []corev1.VolumeMount, name, mountPath string) bool {
	for _, mount := range mounts {
		if mount.Name == name && mount.MountPath == mountPath {
			return true
		}
	}
	return false
}

func hasReadOnlyVolumeMount(mounts []corev1.VolumeMount, name, mountPath string) bool {
	for _, mount := range mounts {
		if mount.Name == name && mount.MountPath == mountPath && mount.ReadOnly {
			return true
		}
	}
	return false
}

func hasString(values []string, want string) bool {
	return slices.Contains(values, want)
}

func hasDroppedCapability(drops []corev1.Capability, value string) bool {
	for _, drop := range drops {
		if string(drop) == value {
			return true
		}
	}
	return false
}

func hasSecretVolume(volumes []corev1.Volume, name, secretName string) bool {
	for _, volume := range volumes {
		if volume.Name != name || volume.Secret == nil || volume.Secret.SecretName != secretName {
			continue
		}
		if volume.Secret.DefaultMode == nil || *volume.Secret.DefaultMode != 0444 {
			return false
		}
		if len(volume.Secret.Items) != 1 {
			return false
		}
		item := volume.Secret.Items[0]
		return item.Key == codeExecKubernetesCodeVolumeName && item.Path == codeExecKubernetesCodeVolumeName && item.Mode != nil && *item.Mode == 0444
	}
	return false
}

func hasPolicyType(policyTypes []networkingv1.PolicyType, want networkingv1.PolicyType) bool {
	return slices.Contains(policyTypes, want)
}

func hasEmptyDirVolume(volumes []corev1.Volume, name string) bool {
	for _, volume := range volumes {
		if volume.Name == name && volume.EmptyDir != nil && volume.EmptyDir.SizeLimit != nil {
			return true
		}
	}
	return false
}
