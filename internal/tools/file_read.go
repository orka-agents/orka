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

// Execute reads the file
func (t *FileReadTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var readArgs FileReadArgs
	if err := json.Unmarshal(args, &readArgs); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	if readArgs.Path == "" {
		return "", fmt.Errorf("path is required")
	}

	// Resolve the path
	filePath := readArgs.Path
	if !filepath.IsAbs(filePath) {
		filePath = filepath.Join(t.workDir, filePath)
	}

	// Clean the path and check for traversal attacks
	filePath = filepath.Clean(filePath)

	// Resolve symlinks to prevent bypass
	resolvedPath, err := filepath.EvalSymlinks(filePath)
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("access denied: cannot resolve path")
	}
	if err == nil {
		filePath = resolvedPath
	}

	if !t.isPathAllowed(filePath) {
		return "", fmt.Errorf("access denied: path outside allowed directories")
	}

	// Check if file exists
	info, err := os.Stat(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("file not found: %s", readArgs.Path)
		}
		return "", fmt.Errorf("failed to stat file: %w", err)
	}

	if info.IsDir() {
		return "", fmt.Errorf("path is a directory, not a file")
	}

	// Open the file
	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close() //nolint:errcheck

	// Handle offset
	if readArgs.Offset > 0 {
		_, err = file.Seek(readArgs.Offset, io.SeekStart)
		if err != nil {
			return "", fmt.Errorf("failed to seek: %w", err)
		}
	}

	// Set limit
	limit := int64(65536) // Default 64KB
	if readArgs.Limit > 0 {
		limit = readArgs.Limit
	}
	if limit > t.maxFileSize {
		limit = t.maxFileSize
	}

	// Read the file
	content := make([]byte, limit)
	n, err := file.Read(content)
	if err != nil && err != io.EOF {
		return "", fmt.Errorf("failed to read file: %w", err)
	}

	result := FileReadResult{
		Content:   string(content[:n]),
		Path:      readArgs.Path,
		Size:      info.Size(),
		Truncated: int64(n) < info.Size()-readArgs.Offset,
	}

	output, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return "", err
	}

	return string(output), nil
}

// isPathAllowed checks if the path is within allowed directories
func (t *FileReadTool) isPathAllowed(path string) bool {
	for _, allowedPath := range t.allowedPaths {
		if path == allowedPath || strings.HasPrefix(path, allowedPath+"/") {
			return true
		}
		// Also check with symlinks resolved for consistent comparison
		resolved, err := filepath.EvalSymlinks(allowedPath)
		if err == nil && resolved != allowedPath {
			if path == resolved || strings.HasPrefix(path, resolved+"/") {
				return true
			}
		}
	}
	return false
}

// Ensure FileReadTool implements Tool
var _ Tool = (*FileReadTool)(nil)
