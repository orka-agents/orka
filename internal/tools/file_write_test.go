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
	"strings"
	"testing"
)

func TestFileWriteTool_Name(t *testing.T) {
	tool := NewFileWriteTool()
	if got := tool.Name(); got != "file_write" {
		t.Errorf("Name() = %v, want %v", got, "file_write")
	}
}

func TestFileWriteTool_Description(t *testing.T) {
	tool := NewFileWriteTool()
	if desc := tool.Description(); desc == "" {
		t.Error("Description() returned empty string")
	}
}

func TestFileWriteTool_Parameters(t *testing.T) {
	tool := NewFileWriteTool()
	params := tool.Parameters()
	if params == nil {
		t.Fatal("Parameters() returned nil")
	}
	var schema map[string]any
	if err := json.Unmarshal(params, &schema); err != nil {
		t.Errorf("Parameters() returned invalid JSON: %v", err)
	}
	if schema["type"] != typeObject {
		t.Error("Parameters schema should have type: object")
	}
}

func TestFileWriteTool_Execute_Write(t *testing.T) {
	tmpDir := t.TempDir()
	tool := &FileWriteTool{
		workDir:      tmpDir,
		maxFileSize:  1024 * 1024,
		allowedPaths: []string{tmpDir},
	}

	args := json.RawMessage(`{"path": "test.txt", "content": "hello world"}`)
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var writeResult FileWriteResult
	if err := json.Unmarshal([]byte(result), &writeResult); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if !writeResult.Created {
		t.Error("expected created = true for new file")
	}
	if writeResult.Mode != "write" {
		t.Errorf("mode = %q, want %q", writeResult.Mode, "write")
	}

	// Verify file content
	content, err := os.ReadFile(filepath.Join(tmpDir, "test.txt"))
	if err != nil {
		t.Fatalf("failed to read file: %v", err)
	}
	if string(content) != "hello world" {
		t.Errorf("file content = %q, want %q", string(content), "hello world")
	}
}

func TestFileWriteTool_Execute_Append(t *testing.T) {
	tmpDir := t.TempDir()
	tool := &FileWriteTool{
		workDir:      tmpDir,
		maxFileSize:  1024 * 1024,
		allowedPaths: []string{tmpDir},
	}

	// Write initial content
	filePath := filepath.Join(tmpDir, "append.txt")
	if err := os.WriteFile(filePath, []byte("line1\n"), 0644); err != nil {
		t.Fatalf("failed to write initial file: %v", err)
	}

	args := json.RawMessage(`{"path": "append.txt", "content": "line2\n", "mode": "append"}`)
	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	var writeResult FileWriteResult
	if err := json.Unmarshal([]byte(result), &writeResult); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if writeResult.Created {
		t.Error("expected created = false for existing file")
	}
	if writeResult.Mode != "append" {
		t.Errorf("mode = %q, want %q", writeResult.Mode, "append")
	}

	content, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("failed to read file: %v", err)
	}
	if string(content) != "line1\nline2\n" {
		t.Errorf("file content = %q, want %q", string(content), "line1\nline2\n")
	}
}

func TestFileWriteTool_Execute_CreateDirs(t *testing.T) {
	tmpDir := t.TempDir()
	tool := &FileWriteTool{
		workDir:      tmpDir,
		maxFileSize:  1024 * 1024,
		allowedPaths: []string{tmpDir},
	}

	args := json.RawMessage(`{"path": "deep/nested/dir/file.txt", "content": "nested"}`)
	_, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(tmpDir, "deep", "nested", "dir", "file.txt"))
	if err != nil {
		t.Fatalf("failed to read file: %v", err)
	}
	if string(content) != "nested" {
		t.Errorf("file content = %q, want %q", string(content), "nested")
	}
}

func TestFileWriteTool_Execute_PathTraversal(t *testing.T) {
	tmpDir := t.TempDir()
	tool := &FileWriteTool{
		workDir:      tmpDir,
		maxFileSize:  1024 * 1024,
		allowedPaths: []string{tmpDir},
	}

	args := json.RawMessage(`{"path": "../../../etc/passwd", "content": "hack"}`)
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Error("Execute() expected error for path traversal")
	}
}

func TestFileWriteTool_Execute_PathRestriction(t *testing.T) {
	tool := &FileWriteTool{
		workDir:      "/workspace",
		maxFileSize:  1024 * 1024,
		allowedPaths: []string{"/workspace", "/tmp"},
	}

	args := json.RawMessage(`{"path": "/etc/passwd", "content": "hack"}`)
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Error("Execute() expected error for restricted path")
	}
	if err != nil && !strings.Contains(err.Error(), "access denied") {
		t.Errorf("expected access denied error, got: %v", err)
	}
}

func TestFileWriteTool_Execute_MaxFileSize(t *testing.T) {
	tmpDir := t.TempDir()
	tool := &FileWriteTool{
		workDir:      tmpDir,
		maxFileSize:  100,
		allowedPaths: []string{tmpDir},
	}

	bigContent := strings.Repeat("x", 200)
	args := json.RawMessage(`{"path": "big.txt", "content": "` + bigContent + `"}`)
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Error("Execute() expected error for oversized content")
	}
}

func TestFileWriteTool_Execute_EmptyPath(t *testing.T) {
	tool := NewFileWriteTool()
	args := json.RawMessage(`{"path": "", "content": "test"}`)
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Error("Execute() expected error for empty path")
	}
}

func TestFileWriteTool_Execute_InvalidMode(t *testing.T) {
	tool := NewFileWriteTool()
	args := json.RawMessage(`{"path": "test.txt", "content": "test", "mode": "invalid"}`)
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Error("Execute() expected error for invalid mode")
	}
}

func TestFileWriteTool_Execute_InvalidJSON(t *testing.T) {
	tool := NewFileWriteTool()
	args := json.RawMessage(`{invalid}`)
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Error("Execute() expected error for invalid JSON")
	}
}
