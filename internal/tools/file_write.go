/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/orka-agents/orka/internal/workerenv"
)

const (
	modeWrite  = "write"
	modeAppend = "append"
)

type FileWriteTool struct {
	workDir      string
	maxFileSize  int64
	allowedPaths []string
}

type FileWriteArgs struct {
	Path       string `json:"path"`
	Content    string `json:"content"`
	Mode       string `json:"mode,omitempty"`
	CreateDirs bool   `json:"create_dirs,omitempty"`
}

type FileWriteResult struct {
	Path    string `json:"path"`
	Size    int    `json:"size"`
	Mode    string `json:"mode"`
	Created bool   `json:"created"`
}

func NewFileWriteTool() *FileWriteTool {
	workDir := os.Getenv(workerenv.WorkDir)
	if workDir == "" {
		workDir = defaultWorkspacePath
	}

	return &FileWriteTool{
		workDir:     workDir,
		maxFileSize: 1024 * 1024,
		allowedPaths: []string{
			defaultWorkspacePath,
			tempDirPath,
		},
	}
}

func (t *FileWriteTool) Name() string {
	return fileWriteToolName
}

func (t *FileWriteTool) Description() string {
	return "Write content to a file in the workspace. Use this to create or modify files."
}

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

	createDirs := true
	var rawMap map[string]any
	if err := json.Unmarshal(args, &rawMap); err == nil {
		if v, ok := rawMap["create_dirs"]; ok {
			if b, ok := v.(bool); ok {
				createDirs = b
			}
		}
	}

	if int64(len(writeArgs.Content)) > t.maxFileSize {
		return "", fmt.Errorf("content size %d exceeds maximum %d bytes", len(writeArgs.Content), t.maxFileSize)
	}

	filePath := writeArgs.Path
	if !filepath.IsAbs(filePath) {
		filePath = filepath.Join(t.workDir, filePath)
	}
	filePath = filepath.Clean(filePath)

	rootPath, relativePath, ok := t.allowedRoot(filePath)
	if !ok {
		return "", fmt.Errorf("access denied: path outside allowed directories")
	}

	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return "", fmt.Errorf("failed to open allowed directory: %w", err)
	}
	defer root.Close() //nolint:errcheck

	_, statErr := root.Stat(relativePath)
	created := os.IsNotExist(statErr)

	if createDirs {
		dir := filepath.Dir(relativePath)
		if err := root.MkdirAll(dir, 0755); err != nil {
			return "", fmt.Errorf("failed to create directories: %w", err)
		}
	}

	flags := os.O_CREATE | os.O_WRONLY
	if writeArgs.Mode == modeAppend {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}

	f, err := root.OpenFile(relativePath, flags, 0644)
	if err != nil {
		return "", fmt.Errorf("failed to open file: %w", err)
	}
	if _, err := f.WriteString(writeArgs.Content); err != nil {
		f.Close() //nolint:errcheck
		return "", fmt.Errorf("failed to write file: %w", err)
	}
	if err := f.Close(); err != nil {
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

func (t *FileWriteTool) allowedRoot(path string) (rootPath, relativePath string, ok bool) {
	for _, allowedPath := range t.allowedPaths {
		rootPath = filepath.Clean(allowedPath)
		relativePath, err := filepath.Rel(rootPath, path)
		if err != nil || !filepath.IsLocal(relativePath) {
			continue
		}
		return rootPath, relativePath, true
	}
	return "", "", false
}

var _ Tool = (*FileWriteTool)(nil)
