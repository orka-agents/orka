/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// CodeExecTool implements code execution functionality
type CodeExecTool struct {
	workDir      string
	timeout      time.Duration
	allowedLangs map[string]bool
}

// CodeExecArgs are the arguments for the code execution tool
type CodeExecArgs struct {
	Language string `json:"language"`
	Code     string `json:"code"`
	Timeout  int    `json:"timeout,omitempty"` // Timeout in seconds
}

// CodeExecResult represents the execution result
type CodeExecResult struct {
	Output   string `json:"output"`
	Error    string `json:"error,omitempty"`
	ExitCode int    `json:"exit_code"`
}

// NewCodeExecTool creates a new code execution tool
func NewCodeExecTool() *CodeExecTool {
	workDir := os.Getenv("ORKA_WORK_DIR")
	if workDir == "" {
		workDir = "/tmp/orka-exec"
	}

	return &CodeExecTool{
		workDir: workDir,
		timeout: 30 * time.Second,
		allowedLangs: map[string]bool{
			"python":     true,
			"python3":    true,
			"javascript": true,
			"node":       true,
			"bash":       true,
			"sh":         true,
		},
	}
}

const codeExecToolName = "code_exec"

// Name returns the tool name
func (t *CodeExecTool) Name() string {
	return codeExecToolName
}

// Description returns the tool description
func (t *CodeExecTool) Description() string {
	return "Execute code in a sandboxed environment. Supports Python, JavaScript (Node.js), and Bash."
}

// Parameters returns the JSON Schema for parameters
func (t *CodeExecTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"language": {
				"type": "string",
				"description": "Programming language (python, javascript, bash)",
				"enum": ["python", "python3", "javascript", "node", "bash", "sh"]
			},
			"code": {
				"type": "string",
				"description": "The code to execute"
			},
			"timeout": {
				"type": "integer",
				"description": "Execution timeout in seconds (default: 30, max: 60)",
				"default": 30
			}
		},
		"required": ["language", "code"]
	}`)
}

// Execute runs the code
func (t *CodeExecTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var execArgs CodeExecArgs
	if err := json.Unmarshal(args, &execArgs); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	if execArgs.Code == "" {
		return "", fmt.Errorf("code is required")
	}

	lang := strings.ToLower(execArgs.Language)
	if !t.allowedLangs[lang] {
		return "", fmt.Errorf("unsupported language: %s", execArgs.Language)
	}

	// Set timeout
	timeout := t.timeout
	if execArgs.Timeout > 0 && execArgs.Timeout <= 60 {
		timeout = time.Duration(execArgs.Timeout) * time.Second
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Create work directory
	if err := os.MkdirAll(t.workDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create work directory: %w", err)
	}

	// Execute based on language
	var result CodeExecResult
	switch lang {
	case "python", "python3":
		result = t.executePython(ctx, execArgs.Code)
	case "javascript", "node":
		result = t.executeNode(ctx, execArgs.Code)
	case "bash", "sh":
		result = t.executeBash(ctx, execArgs.Code)
	default:
		return "", fmt.Errorf("unsupported language: %s", lang)
	}

	output, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return "", err
	}

	return string(output), nil
}

// executePython executes Python code
func (t *CodeExecTool) executePython(ctx context.Context, code string) CodeExecResult {
	tmpFile, err := os.CreateTemp(t.workDir, "script-*.py")
	if err != nil {
		return CodeExecResult{Error: fmt.Sprintf("failed to create temp script: %v", err), ExitCode: -1}
	}
	tmpPath := tmpFile.Name()
	if _, err := tmpFile.Write([]byte(code)); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath) //nolint:errcheck
		return CodeExecResult{Error: fmt.Sprintf("failed to write script: %v", err), ExitCode: -1}
	}
	tmpFile.Close()
	defer os.Remove(tmpPath) //nolint:errcheck

	cmd := exec.CommandContext(ctx, "python3", tmpPath)
	return t.runCommand(cmd)
}

// executeNode executes JavaScript code
func (t *CodeExecTool) executeNode(ctx context.Context, code string) CodeExecResult {
	tmpFile, err := os.CreateTemp(t.workDir, "script-*.js")
	if err != nil {
		return CodeExecResult{Error: fmt.Sprintf("failed to create temp script: %v", err), ExitCode: -1}
	}
	tmpPath := tmpFile.Name()
	if _, err := tmpFile.Write([]byte(code)); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath) //nolint:errcheck
		return CodeExecResult{Error: fmt.Sprintf("failed to write script: %v", err), ExitCode: -1}
	}
	tmpFile.Close()
	defer os.Remove(tmpPath) //nolint:errcheck

	cmd := exec.CommandContext(ctx, "node", tmpPath)
	return t.runCommand(cmd)
}

// executeBash executes Bash code
func (t *CodeExecTool) executeBash(ctx context.Context, code string) CodeExecResult {
	tmpFile, err := os.CreateTemp(t.workDir, "script-*.sh")
	if err != nil {
		return CodeExecResult{Error: fmt.Sprintf("failed to create temp script: %v", err), ExitCode: -1}
	}
	tmpPath := tmpFile.Name()
	if _, err := tmpFile.Write([]byte(code)); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath) //nolint:errcheck
		return CodeExecResult{Error: fmt.Sprintf("failed to write script: %v", err), ExitCode: -1}
	}
	tmpFile.Close()
	if err := os.Chmod(tmpPath, 0755); err != nil {
		os.Remove(tmpPath) //nolint:errcheck
		return CodeExecResult{Error: fmt.Sprintf("failed to chmod script: %v", err), ExitCode: -1}
	}
	defer os.Remove(tmpPath) //nolint:errcheck

	cmd := exec.CommandContext(ctx, "bash", tmpPath)
	return t.runCommand(cmd)
}

// runCommand executes a command and captures output
func (t *CodeExecTool) runCommand(cmd *exec.Cmd) CodeExecResult {
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Dir = t.workDir

	err := cmd.Run()

	result := CodeExecResult{
		Output:   stdout.String(),
		ExitCode: 0,
	}

	if stderr.Len() > 0 {
		result.Error = stderr.String()
	}

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else {
			result.ExitCode = -1
			if result.Error == "" {
				result.Error = err.Error()
			}
		}
	}

	return result
}

// Ensure CodeExecTool implements Tool
var _ Tool = (*CodeExecTool)(nil)
