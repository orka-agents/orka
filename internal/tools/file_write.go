/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/sozercan/orka/internal/workerenv"
	"os"
	"path/filepath"
	"strings"
)

const (
	modeWrite  = "write"
	modeAppend = "append"
)

// FileWriteTool implements file writing functionality
type FileWriteTool struct {
	workDir      string
	maxFileSize  int64
	allowedPaths []string
}

// FileWriteArgs are the arguments for the file write tool
type FileWriteArgs struct {
	Path       string `json:"path"`
	Content    string `json:"content"`
	Mode       string `json:"mode,omitempty"`
	CreateDirs bool   `json:"create_dirs,omitempty"`
}

// FileWriteResult represents the file write result
type FileWriteResult struct {
	Path    string `json:"path"`
	Size    int    `json:"size"`
	Mode    string `json:"mode"`
	Created bool   `json:"created"`
}

// NewFileWriteTool creates a new file write tool
func NewFileWriteTool() *FileWriteTool {
	workDir := os.Getenv(workerenv.WorkDir)
	if workDir == "" {
		workDir = defaultWorkspacePath
	}

	return &FileWriteTool{
		workDir:     workDir,
		maxFileSize: 1024 * 1024, // 1MB max
		allowedPaths: []string{
			defaultWorkspacePath,
			tempDirPath,
		},
	}
}

// Name returns the tool name
func (t *FileWriteTool) Name() string {
	return fileWriteToolName
}

// Description returns the tool description
func (t *FileWriteTool) Description() string {
	return "Write content to a file in the workspace. Use this to create or modify files."
}

// Parameters returns the JSON Schema for parameters
func (t *FileWriteTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {
				"type": "string",
				"description": "Path to the file to write (relative to workspace or absolute)"
			},
			"content": {
				"type": "string",
				"description": "Content to write to the file"
			},
			"mode": {
				"type": "string",
				"description": "Write mode: 'write' (overwrite) or 'append' (default: write)",
				"enum": ["write", "append"],
				"default": "write"
			},
			"create_dirs": {
				"type": "boolean",
				"description": "Create parent directories if they don't exist (default: true)",
				"default": true
			}
		},
		"required": ["path", "content"]
	}`)
}

// Execute writes the file
func (t *FileWriteTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var writeArgs FileWriteArgs
	if err := json.Unmarshal(args, &writeArgs); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	if writeArgs.Path == "" {
		return "", fmt.Errorf("path is required")
	}

	if writeArgs.Mode == "" {
		writeArgs.Mode = modeWrite
	}
	if writeArgs.Mode != modeWrite && writeArgs.Mode != modeAppend {
		return "", fmt.Errorf("mode must be 'write' or 'append'")
	}

	// Default create_dirs to true — check if explicitly set to false via raw args
	createDirs := true
	var rawMap map[string]any
	if err := json.Unmarshal(args, &rawMap); err == nil {
		if v, ok := rawMap["create_dirs"]; ok {
			if b, ok := v.(bool); ok {
				createDirs = b
			}
		}
	}

	// Check content size
	if int64(len(writeArgs.Content)) > t.maxFileSize {
		return "", fmt.Errorf("content size %d exceeds maximum %d bytes", len(writeArgs.Content), t.maxFileSize)
	}

	// Resolve the path
	filePath := writeArgs.Path
	if !filepath.IsAbs(filePath) {
		filePath = filepath.Join(t.workDir, filePath)
	}

	// Clean the path and reject traversal
	filePath = filepath.Clean(filePath)
	if strings.Contains(filePath, "..") {
		return "", fmt.Errorf("path traversal not allowed")
	}

	if !t.isPathAllowed(filePath) {
		return "", fmt.Errorf("access denied: path outside allowed directories")
	}

	// Check if file already exists
	_, statErr := os.Stat(filePath)
	created := os.IsNotExist(statErr)

	// Create parent directories if needed
	if createDirs {
		dir := filepath.Dir(filePath)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return "", fmt.Errorf("failed to create directories: %w", err)
		}
	}

	// Write or append
	var err error
	if writeArgs.Mode == modeAppend {
		f, openErr := os.OpenFile(filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if openErr != nil {
			return "", fmt.Errorf("failed to open file: %w", openErr)
		}
		_, err = f.WriteString(writeArgs.Content)
		f.Close() //nolint:errcheck
	} else {
		err = os.WriteFile(filePath, []byte(writeArgs.Content), 0644)
	}

	if err != nil {
		return "", fmt.Errorf("failed to write file: %w", err)
	}

	result := FileWriteResult{
		Path:    writeArgs.Path,
		Size:    len(writeArgs.Content),
		Mode:    writeArgs.Mode,
		Created: created,
	}

	output, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return "", err
	}

	return string(output), nil
}

// isPathAllowed checks if the path is within allowed directories
func (t *FileWriteTool) isPathAllowed(path string) bool {
	for _, allowedPath := range t.allowedPaths {
		if strings.HasPrefix(path, allowedPath) {
			return true
		}
	}
	return false
}

// Ensure FileWriteTool implements Tool
var _ Tool = (*FileWriteTool)(nil)
