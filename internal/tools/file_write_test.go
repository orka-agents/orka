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
	if got := tool.Name(); got != fileWriteToolName {
		t.Errorf("Name() = %v, want %v", got, fileWriteToolName)
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
	if schema[jsonSchemaTypeField] != typeObject {
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

func TestFileWriteTool_Execute_NestedWriteAndAppend(t *testing.T) {
	tmpDir := t.TempDir()
	tool := &FileWriteTool{
		workDir:      tmpDir,
		maxFileSize:  1024 * 1024,
		allowedPaths: []string{tmpDir},
	}

	writeResultJSON, err := tool.Execute(context.Background(), json.RawMessage(
		`{"path":"nested/dir/file.txt","content":"first","mode":"write"}`,
	))
	if err != nil {
		t.Fatalf("write Execute() error = %v", err)
	}

	var writeResult FileWriteResult
	if err := json.Unmarshal([]byte(writeResultJSON), &writeResult); err != nil {
		t.Fatalf("failed to unmarshal write result: %v", err)
	}
	if writeResult != (FileWriteResult{
		Path:    "nested/dir/file.txt",
		Size:    len("first"),
		Mode:    modeWrite,
		Created: true,
	}) {
		t.Errorf("write result = %+v, want nested write result", writeResult)
	}

	appendResultJSON, err := tool.Execute(context.Background(), json.RawMessage(
		`{"path":"nested/dir/file.txt","content":" second","mode":"append"}`,
	))
	if err != nil {
		t.Fatalf("append Execute() error = %v", err)
	}

	var appendResult FileWriteResult
	if err := json.Unmarshal([]byte(appendResultJSON), &appendResult); err != nil {
		t.Fatalf("failed to unmarshal append result: %v", err)
	}
	if appendResult != (FileWriteResult{
		Path:    "nested/dir/file.txt",
		Size:    len(" second"),
		Mode:    modeAppend,
		Created: false,
	}) {
		t.Errorf("append result = %+v, want nested append result", appendResult)
	}

	content, err := os.ReadFile(filepath.Join(tmpDir, "nested", "dir", "file.txt"))
	if err != nil {
		t.Fatalf("failed to read nested file: %v", err)
	}
	if got, want := string(content), "first second"; got != want {
		t.Errorf("nested file content = %q, want %q", got, want)
	}
}

func TestFileWriteTool_Execute_CreateDirsFalse(t *testing.T) {
	tmpDir := t.TempDir()
	tool := &FileWriteTool{
		workDir:      tmpDir,
		maxFileSize:  1024 * 1024,
		allowedPaths: []string{tmpDir},
	}

	_, err := tool.Execute(context.Background(), json.RawMessage(
		`{"path":"missing/parent/file.txt","content":"blocked","create_dirs":false}`,
	))
	if err == nil {
		t.Fatal("Execute() expected error when create_dirs=false and parent is missing")
	}
	if _, statErr := os.Stat(filepath.Join(tmpDir, "missing")); !os.IsNotExist(statErr) {
		t.Errorf("missing parent was created with create_dirs=false; stat error = %v", statErr)
	}

	existingDir := filepath.Join(tmpDir, "existing")
	if err := os.Mkdir(existingDir, 0755); err != nil {
		t.Fatalf("failed to create existing parent: %v", err)
	}
	_, err = tool.Execute(context.Background(), json.RawMessage(
		`{"path":"existing/file.txt","content":"allowed","create_dirs":false}`,
	))
	if err != nil {
		t.Fatalf("Execute() with existing parent and create_dirs=false error = %v", err)
	}
	content, err := os.ReadFile(filepath.Join(existingDir, "file.txt"))
	if err != nil {
		t.Fatalf("failed to read file under existing parent: %v", err)
	}
	if got, want := string(content), "allowed"; got != want {
		t.Errorf("file content = %q, want %q", got, want)
	}
}

func TestFileWriteTool_Execute_DeniesRootEscapes(t *testing.T) {
	t.Run("sibling prefix", func(t *testing.T) {
		parentDir := t.TempDir()
		allowedDir := filepath.Join(parentDir, "allowed")
		siblingDir := filepath.Join(parentDir, "allowed-sibling")
		if err := os.Mkdir(allowedDir, 0755); err != nil {
			t.Fatalf("failed to create allowed directory: %v", err)
		}
		if err := os.Mkdir(siblingDir, 0755); err != nil {
			t.Fatalf("failed to create sibling directory: %v", err)
		}

		tool := &FileWriteTool{
			workDir:      allowedDir,
			maxFileSize:  1024 * 1024,
			allowedPaths: []string{allowedDir},
		}
		escapedPath := filepath.Join(siblingDir, "escaped.txt")
		args, err := json.Marshal(FileWriteArgs{
			Path:    escapedPath,
			Content: "escaped",
		})
		if err != nil {
			t.Fatalf("failed to marshal arguments: %v", err)
		}

		if _, err := tool.Execute(context.Background(), args); err == nil {
			t.Error("Execute() expected error for lexical sibling-prefix path")
		}
		if _, err := os.Stat(escapedPath); !os.IsNotExist(err) {
			t.Errorf("sibling-prefix escape created a file outside the root; stat error = %v", err)
		}
	})

	t.Run("parent symlink", func(t *testing.T) {
		allowedDir := t.TempDir()
		outsideDir := t.TempDir()
		linkPath := filepath.Join(allowedDir, "outside")
		linkTarget, err := filepath.Rel(filepath.Dir(linkPath), outsideDir)
		if err != nil {
			t.Fatalf("failed to build relative symlink target: %v", err)
		}
		if err := os.Symlink(linkTarget, linkPath); err != nil {
			t.Skipf("symlinks not supported: %v", err)
		}

		tool := &FileWriteTool{
			workDir:      allowedDir,
			maxFileSize:  1024 * 1024,
			allowedPaths: []string{allowedDir},
		}
		escapedPath := filepath.Join(outsideDir, "escaped.txt")
		args := json.RawMessage(
			`{"path":"outside/escaped.txt","content":"escaped","create_dirs":false}`,
		)

		if _, err := tool.Execute(context.Background(), args); err == nil {
			t.Error("Execute() expected error for parent symlink escaping the root")
		}
		if _, err := os.Stat(escapedPath); !os.IsNotExist(err) {
			t.Errorf("parent symlink escape created a file outside the root; stat error = %v", err)
		}
	})

	t.Run("final symlink write", func(t *testing.T) {
		allowedDir := t.TempDir()
		outsideDir := t.TempDir()
		outsidePath := filepath.Join(outsideDir, "outside.txt")
		if err := os.WriteFile(outsidePath, []byte("original"), 0644); err != nil {
			t.Fatalf("failed to create outside file: %v", err)
		}
		linkPath := filepath.Join(allowedDir, "link.txt")
		linkTarget, err := filepath.Rel(filepath.Dir(linkPath), outsidePath)
		if err != nil {
			t.Fatalf("failed to build relative symlink target: %v", err)
		}
		if err := os.Symlink(linkTarget, linkPath); err != nil {
			t.Skipf("symlinks not supported: %v", err)
		}

		tool := &FileWriteTool{
			workDir:      allowedDir,
			maxFileSize:  1024 * 1024,
			allowedPaths: []string{allowedDir},
		}
		args := json.RawMessage(`{"path":"link.txt","content":"overwritten"}`)

		if _, err := tool.Execute(context.Background(), args); err == nil {
			t.Error("Execute() expected error for final symlink escaping the root")
		}
		content, err := os.ReadFile(outsidePath)
		if err != nil {
			t.Fatalf("failed to read outside file: %v", err)
		}
		if got, want := string(content), "original"; got != want {
			t.Errorf("outside file content = %q, want %q", got, want)
		}
	})

	t.Run("final symlink append", func(t *testing.T) {
		allowedDir := t.TempDir()
		outsideDir := t.TempDir()
		outsidePath := filepath.Join(outsideDir, "outside.txt")
		if err := os.WriteFile(outsidePath, []byte("original"), 0644); err != nil {
			t.Fatalf("failed to create outside file: %v", err)
		}
		linkPath := filepath.Join(allowedDir, "link.txt")
		linkTarget, err := filepath.Rel(filepath.Dir(linkPath), outsidePath)
		if err != nil {
			t.Fatalf("failed to build relative symlink target: %v", err)
		}
		if err := os.Symlink(linkTarget, linkPath); err != nil {
			t.Skipf("symlinks not supported: %v", err)
		}

		tool := &FileWriteTool{
			workDir:      allowedDir,
			maxFileSize:  1024 * 1024,
			allowedPaths: []string{allowedDir},
		}
		args := json.RawMessage(
			`{"path":"link.txt","content":" appended","mode":"append"}`,
		)

		if _, err := tool.Execute(context.Background(), args); err == nil {
			t.Error("Execute() expected error when appending through final symlink outside the root")
		}
		content, err := os.ReadFile(outsidePath)
		if err != nil {
			t.Fatalf("failed to read outside file: %v", err)
		}
		if got, want := string(content), "original"; got != want {
			t.Errorf("outside file content = %q, want %q", got, want)
		}
	})
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
		workDir:      defaultWorkspacePath,
		maxFileSize:  1024 * 1024,
		allowedPaths: []string{defaultWorkspacePath, tempDirPath},
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
	args := json.RawMessage(invalidJSONText)
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Error("Execute() expected error for invalid JSON")
	}
}
