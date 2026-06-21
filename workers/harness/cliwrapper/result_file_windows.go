//go:build windows

package cliwrapper

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func createResultFileNoFollow(workDir, path string) (*os.File, error) {
	if strings.TrimSpace(workDir) == "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("create result file directory: %w", err)
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("remove stale result file: %w", err)
		}
		return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	}
	root, rel, err := workspaceRelativePath(workDir, path, "result file")
	if err != nil {
		return nil, err
	}
	cleaned := filepath.Clean(rel)
	if cleaned == "." {
		return nil, fmt.Errorf("result file path must not be workspace root")
	}
	current := root
	parts := strings.Split(cleaned, string(filepath.Separator))
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, fmt.Errorf("invalid result file path component %q", part)
		}
		current = filepath.Join(current, part)
		last := i == len(parts)-1
		info, err := os.Lstat(current)
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return nil, os.ErrPermission
		}
		if !last {
			if os.IsNotExist(err) {
				if mkErr := os.Mkdir(current, 0o755); mkErr != nil && !os.IsExist(mkErr) {
					return nil, mkErr
				}
			} else if err != nil {
				return nil, err
			} else if !info.IsDir() {
				return nil, fmt.Errorf("result file directory %q is not a directory", current)
			}
		}
	}
	if err := os.Remove(current); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return os.OpenFile(current, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
}

func openResultFileNoFollow(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, os.ErrPermission
	}
	return os.Open(path)
}

func openResultFileInWorkspaceNoFollow(workDir, path string) (*os.File, error) {
	root, rel, err := workspaceRelativePath(workDir, path, "result file")
	if err != nil {
		return nil, err
	}
	cleaned := filepath.Clean(rel)
	if cleaned == "." {
		return nil, fmt.Errorf("result file path must not be workspace root")
	}
	current := root
	for _, part := range strings.Split(cleaned, string(filepath.Separator)) {
		if part == "" || part == "." || part == ".." {
			return nil, fmt.Errorf("invalid result file path component %q", part)
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, os.ErrPermission
		}
	}
	return os.Open(current)
}

func markOpenResultFileUnwritten(_ *os.File, path string) error {
	return os.Chtimes(path, unwrittenResultFileModTime, unwrittenResultFileModTime)
}

func resultFileLinkCount(os.FileInfo) (uint64, bool) { return 0, false }
