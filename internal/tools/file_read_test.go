/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

const testDigits = "0123456789"

func TestFileReadTool_Name(t *testing.T) {
	tool := NewFileReadTool()
	if got := tool.Name(); got != "file_read" {
		t.Errorf("Name() = %v, want %v", got, "file_read")
	}
}

func TestFileReadTool_Description(t *testing.T) {
	tool := NewFileReadTool()
	desc := tool.Description()
	if desc == "" {
		t.Error("Description() returned empty string")
	}
}

func TestFileReadTool_Parameters(t *testing.T) {
	tool := NewFileReadTool()
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

func TestFileReadTool_Execute_ReadFile(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.txt")
	testContent := "Hello, World!"
	if err := os.WriteFile(testFile, []byte(testContent), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	tool := &FileReadTool{
		workDir:      tmpDir,
		maxFileSize:  1024 * 1024,
		allowedPaths: []string{tmpDir},
	}

	args := json.RawMessage(`{"path": "` + testFile + `"}`)
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var readResult FileReadResult
	if err := json.Unmarshal([]byte(result), &readResult); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	if readResult.Content != testContent {
		t.Errorf("Content = %q, want %q", readResult.Content, testContent)
	}
}

func TestFileReadTool_Execute_RelativePath(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.txt")
	testContent := "relative path test"
	if err := os.WriteFile(testFile, []byte(testContent), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	tool := &FileReadTool{
		workDir:      tmpDir,
		maxFileSize:  1024 * 1024,
		allowedPaths: []string{tmpDir},
	}

	args := json.RawMessage(`{"path": "test.txt"}`)
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var readResult FileReadResult
	if err := json.Unmarshal([]byte(result), &readResult); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	if readResult.Content != testContent {
		t.Errorf("Content = %q, want %q", readResult.Content, testContent)
	}
}

func TestFileReadTool_Execute_FileNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	tool := &FileReadTool{
		workDir:      tmpDir,
		maxFileSize:  1024 * 1024,
		allowedPaths: []string{tmpDir},
	}

	args := json.RawMessage(`{"path": "nonexistent.txt"}`)
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Error("Execute() expected error for nonexistent file")
	}
}

func TestFileReadTool_Execute_Directory(t *testing.T) {
	tmpDir := t.TempDir()
	tool := &FileReadTool{
		workDir:      tmpDir,
		maxFileSize:  1024 * 1024,
		allowedPaths: []string{tmpDir},
	}

	args := json.RawMessage(`{"path": "` + tmpDir + `"}`)
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Error("Execute() expected error for directory")
	}
}

func TestFileReadTool_Execute_PathTraversal(t *testing.T) {
	tmpDir := t.TempDir()
	tool := &FileReadTool{
		workDir:      tmpDir,
		maxFileSize:  1024 * 1024,
		allowedPaths: []string{tmpDir},
	}

	// Try to read outside allowed directory
	args := json.RawMessage(`{"path": "/etc/passwd"}`)
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Error("Execute() expected error for path traversal")
	}
}

func TestFileReadTool_Execute_PathTraversalRelative(t *testing.T) {
	tmpDir := t.TempDir()
	tool := &FileReadTool{
		workDir:      tmpDir,
		maxFileSize:  1024 * 1024,
		allowedPaths: []string{tmpDir},
	}

	// Try to use .. to escape
	args := json.RawMessage(`{"path": "../../../etc/passwd"}`)
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Error("Execute() expected error for path traversal with ..")
	}
}

func TestFileReadTool_Execute_EmptyPath(t *testing.T) {
	tool := NewFileReadTool()

	args := json.RawMessage(`{"path": ""}`)
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Error("Execute() expected error for empty path")
	}
}

func TestFileReadTool_Execute_InvalidJSON(t *testing.T) {
	tool := NewFileReadTool()

	args := json.RawMessage(`{invalid}`)
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Error("Execute() expected error for invalid JSON")
	}
}

func TestFileReadTool_Execute_WithOffset(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.txt")
	testContent := testDigits
	if err := os.WriteFile(testFile, []byte(testContent), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	tool := &FileReadTool{
		workDir:      tmpDir,
		maxFileSize:  1024 * 1024,
		allowedPaths: []string{tmpDir},
	}

	args := json.RawMessage(`{"path": "` + testFile + `", "offset": 5}`)
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var readResult FileReadResult
	if err := json.Unmarshal([]byte(result), &readResult); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	if readResult.Content != "56789" {
		t.Errorf("Content = %q, want %q", readResult.Content, "56789")
	}
}

func TestFileReadTool_Execute_WithLimit(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.txt")
	testContent := testDigits
	if err := os.WriteFile(testFile, []byte(testContent), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	tool := &FileReadTool{
		workDir:      tmpDir,
		maxFileSize:  1024 * 1024,
		allowedPaths: []string{tmpDir},
	}

	args := json.RawMessage(`{"path": "` + testFile + `", "limit": 5}`)
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var readResult FileReadResult
	if err := json.Unmarshal([]byte(result), &readResult); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	if readResult.Content != "01234" {
		t.Errorf("Content = %q, want %q", readResult.Content, "01234")
	}
	if !readResult.Truncated {
		t.Error("Truncated should be true")
	}
}

func TestFileReadTool_Execute_OffsetAndLimit(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.txt")
	testContent := testDigits
	if err := os.WriteFile(testFile, []byte(testContent), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	tool := &FileReadTool{
		workDir:      tmpDir,
		maxFileSize:  1024 * 1024,
		allowedPaths: []string{tmpDir},
	}

	args := json.RawMessage(`{"path": "` + testFile + `", "offset": 2, "limit": 4}`)
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var readResult FileReadResult
	if err := json.Unmarshal([]byte(result), &readResult); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	if readResult.Content != "2345" {
		t.Errorf("Content = %q, want %q", readResult.Content, "2345")
	}
}

func TestFileReadTool_Execute_LimitExceedsMax(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.txt")
	testContent := "test content"
	if err := os.WriteFile(testFile, []byte(testContent), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	tool := &FileReadTool{
		workDir:      tmpDir,
		maxFileSize:  5, // Small max file size
		allowedPaths: []string{tmpDir},
	}

	// Request more than max
	args := json.RawMessage(`{"path": "` + testFile + `", "limit": 1000000}`)
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var readResult FileReadResult
	if err := json.Unmarshal([]byte(result), &readResult); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	// Should be limited to maxFileSize
	if len(readResult.Content) > 5 {
		t.Errorf("Content length = %d, should be limited to 5", len(readResult.Content))
	}
}

func TestFileReadTool_isPathAllowed(t *testing.T) {
	tool := &FileReadTool{
		allowedPaths: []string{"/workspace", "/tmp"},
	}

	tests := []struct {
		path    string
		allowed bool
	}{
		{"/workspace/file.txt", true},
		{"/workspace/subdir/file.txt", true},
		{"/tmp/test.txt", true},
		{"/etc/passwd", false},
		{"/home/user/file.txt", false},
		{"/work", false}, // Not a prefix of /workspace
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := tool.isPathAllowed(tt.path); got != tt.allowed {
				t.Errorf("isPathAllowed(%q) = %v, want %v", tt.path, got, tt.allowed)
			}
		})
	}
}

func TestNewFileReadTool(t *testing.T) {
	tool := NewFileReadTool()
	if tool == nil {
		t.Fatal("NewFileReadTool returned nil")
	}

	if tool.maxFileSize != 1024*1024 {
		t.Errorf("maxFileSize = %d, want %d", tool.maxFileSize, 1024*1024)
	}

	if len(tool.allowedPaths) != 2 {
		t.Errorf("allowedPaths length = %d, want 2", len(tool.allowedPaths))
	}
}

func TestNewFileReadTool_WithEnvVar(t *testing.T) {
	originalWorkDir := os.Getenv("ORKA_WORK_DIR")
	os.Setenv("ORKA_WORK_DIR", "/custom/workspace")   //nolint:errcheck
	defer os.Setenv("ORKA_WORK_DIR", originalWorkDir) //nolint:errcheck

	tool := NewFileReadTool()
	if tool.workDir != "/custom/workspace" {
		t.Errorf("workDir = %v, want %v", tool.workDir, "/custom/workspace")
	}
}

func TestFileReadTool_Execute_Size(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.txt")
	testContent := "12345"
	if err := os.WriteFile(testFile, []byte(testContent), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	tool := &FileReadTool{
		workDir:      tmpDir,
		maxFileSize:  1024 * 1024,
		allowedPaths: []string{tmpDir},
	}

	args := json.RawMessage(`{"path": "` + testFile + `"}`)
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var readResult FileReadResult
	if err := json.Unmarshal([]byte(result), &readResult); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	if readResult.Size != 5 {
		t.Errorf("Size = %d, want 5", readResult.Size)
	}
}
