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
	"path/filepath"
	"strings"

	"github.com/orka-agents/orka/internal/workerenv"
)

// FileReadTool implements file reading functionality
type FileReadTool struct {
	workDir      string
	maxFileSize  int64
	allowedPaths []string
}

// FileReadArgs are the arguments for the file read tool
type FileReadArgs struct {
	Path     string `json:"path"`
	Offset   int64  `json:"offset,omitempty"`
	Limit    int64  `json:"limit,omitempty"`
	Encoding string `json:"encoding,omitempty"`
}

// FileReadResult represents the file read result
type FileReadResult struct {
	Content   string `json:"content"`
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	Truncated bool   `json:"truncated"`
}

// NewFileReadTool creates a new file read tool
func NewFileReadTool() *FileReadTool {
	workDir := os.Getenv(workerenv.WorkDir)
	if workDir == "" {
		workDir = defaultWorkspacePath
	}

	return &FileReadTool{
		workDir:     workDir,
		maxFileSize: 1024 * 1024, // 1MB max
		allowedPaths: []string{
			defaultWorkspacePath,
			tempDirPath,
		},
	}
}

// Name returns the tool name
func (t *FileReadTool) Name() string {
	return fileReadToolName
}

// Description returns the tool description
func (t *FileReadTool) Description() string {
	return "Read the contents of a file from the workspace. Use this to examine code, configuration, or data files."
}

// Parameters returns the JSON Schema for parameters
func (t *FileReadTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {
				"type": "string",
				"description": "Path to the file to read (relative to workspace or absolute)"
			},
			"offset": {
				"type": "integer",
				"description": "Byte offset to start reading from (default: 0)"
			},
			"limit": {
				"type": "integer",
				"description": "Maximum number of bytes to read (default: 65536)"
			}
		},
		"required": ["path"]
	}`)
}

// Execute reads the file.
func (t *FileReadTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	_ = ctx
	var readArgs FileReadArgs
	if err := json.Unmarshal(args, &readArgs); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if readArgs.Path == "" {
		return "", fmt.Errorf("path is required")
	}
	if readArgs.Offset < 0 {
		return "", fmt.Errorf("offset must be non-negative")
	}

	rootPath, relativePath, ok := t.allowedRootForPath(readArgs.Path)
	if !ok {
		return "", fmt.Errorf("access denied: path outside allowed directories")
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return "", fmt.Errorf("access denied: cannot open allowed directory")
	}
	defer func() { _ = root.Close() }()

	file, err := root.Open(relativePath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("file not found: %s", readArgs.Path)
		}
		return "", fmt.Errorf("failed to open file: %w", err)
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("failed to stat file: %w", err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("path is a directory, not a file")
	}
	if readArgs.Offset > 0 {
		if _, err := file.Seek(readArgs.Offset, io.SeekStart); err != nil {
			return "", fmt.Errorf("failed to seek: %w", err)
		}
	}

	const (
		defaultFileReadBytes = int64(65536)
		maxFileReadBytes     = int64(1024 * 1024)
	)
	limit := defaultFileReadBytes
	if readArgs.Limit > 0 {
		limit = readArgs.Limit
	}
	if limit > maxFileReadBytes {
		limit = maxFileReadBytes
	}
	if t.maxFileSize > 0 && limit > t.maxFileSize {
		limit = t.maxFileSize
	}
	if limit < 0 {
		return "", fmt.Errorf("invalid file read limit")
	}

	content := make([]byte, int(limit))
	n, err := file.Read(content)
	if err != nil && err != io.EOF {
		return "", fmt.Errorf("failed to read file: %w", err)
	}

	remaining := max(info.Size()-readArgs.Offset, 0)
	result := FileReadResult{
		Content:   string(content[:n]),
		Path:      readArgs.Path,
		Size:      info.Size(),
		Truncated: int64(n) < remaining,
	}
	output, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return "", err
	}
	return string(output), nil
}

// allowedRootForPath maps a requested path to one os.Root-relative name. os.Root
// enforces confinement across symlink traversal and closes the check/open race.
func (t *FileReadTool) allowedRootForPath(requested string) (string, string, bool) {
	candidate := requested
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(t.workDir, candidate)
	}
	candidate, err := filepath.Abs(filepath.Clean(candidate))
	if err != nil {
		return "", "", false
	}
	for _, allowedPath := range t.allowedPaths {
		allowed, err := filepath.Abs(filepath.Clean(allowedPath))
		if err != nil {
			continue
		}
		relative, err := filepath.Rel(allowed, candidate)
		if err != nil || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			continue
		}
		return allowed, relative, true
	}
	return "", "", false
}

// Ensure FileReadTool implements Tool
var _ Tool = (*FileReadTool)(nil)
