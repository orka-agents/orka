//go:build windows

package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type secureFileMetadata struct {
	Size    int64
	Mode    uint32
	ModTime time.Time
}

func secureWriteFile(
	requested string,
	data []byte,
	mode os.FileMode,
	_ bool,
	_, _ uint32,
	modTime time.Time,
) (secureFileMetadata, error) {
	path, err := safePath(requested)
	if err != nil {
		return secureFileMetadata{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return secureFileMetadata{}, err
	}
	writeRequired := true
	if existing, err := os.ReadFile(path); err == nil {
		existingHash := sha256.Sum256(existing)
		expectedHash := sha256.Sum256(data)
		writeRequired = existingHash != expectedHash
	}
	if writeRequired {
		if err := os.WriteFile(path, data, mode.Perm()); err != nil {
			return secureFileMetadata{}, err
		}
	}
	if err := os.Chmod(path, mode.Perm()); err != nil {
		return secureFileMetadata{}, err
	}
	if !modTime.IsZero() {
		_ = os.Chtimes(path, modTime, modTime)
	}
	info, err := os.Stat(path)
	if err != nil {
		return secureFileMetadata{}, err
	}
	return secureFileMetadata{Size: info.Size(), Mode: uint32(info.Mode().Perm()), ModTime: info.ModTime()}, nil
}

func secureReadFile(requested string, maxBytes int64) ([]byte, secureFileMetadata, error) {
	path, err := safePath(requested)
	if err != nil {
		return nil, secureFileMetadata{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, secureFileMetadata{}, err
	}
	defer file.Close() //nolint:errcheck
	info, err := file.Stat()
	if err != nil {
		return nil, secureFileMetadata{}, err
	}
	if maxBytes < 0 || info.Size() < 0 || info.Size() > maxBytes {
		return nil, secureFileMetadata{}, errWorkspaceFileTooLarge
	}
	limit := maxBytes + 1
	if maxBytes == int64(^uint64(0)>>1) {
		limit = maxBytes
	}
	data, err := io.ReadAll(io.LimitReader(file, limit))
	if err == nil && int64(len(data)) > maxBytes {
		err = errWorkspaceFileTooLarge
	}
	return data, secureFileMetadata{Size: info.Size(), Mode: uint32(info.Mode().Perm()), ModTime: info.ModTime()}, err
}

func secureRemoveAll(requested string) error {
	return secureRemoveAllContext(context.Background(), requested)
}

func secureRemoveAllContext(ctx context.Context, requested string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := safePath(requested)
	if err != nil {
		return err
	}
	return os.RemoveAll(path)
}

func secureResetDirectory(requested string, setOwner bool, uid, gid uint32, protected []string) error {
	return secureResetDirectoryContext(context.Background(), requested, setOwner, uid, gid, protected)
}

func secureResetDirectoryContext(
	ctx context.Context,
	requested string,
	_ bool,
	_, _ uint32,
	protected []string,
) error {
	path, err := safePath(requested)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o755); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return os.MkdirAll(path, 0o755)
	}
	if err != nil {
		return err
	}
	defer file.Close() //nolint:errcheck
	count := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, readErr := file.ReadDir(workspaceDirectoryBatchSize)
		for _, entry := range entries {
			count++
			if count > maxWorkspaceTreeEntries {
				return errWorkspaceTreeTooLarge
			}
			child := filepath.Join(path, entry.Name())
			if protectedPath(child, protected) || hasProtectedDescendant(child, protected) {
				continue
			}
			if err := os.RemoveAll(child); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

func secureListFiles(requested string) ([]string, error) {
	root, err := safePath(requested)
	if err != nil {
		return nil, err
	}
	entries := 0
	var paths []string
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		entries++
		if entries > maxWorkspaceTreeEntries {
			return errWorkspaceTreeTooLarge
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if strings.Count(relative, string(os.PathSeparator))+1 > maxWorkspaceTreeDepth {
			return errWorkspaceTreeTooDeep
		}
		if !entry.IsDir() {
			paths = append(paths, path)
		}
		return nil
	})
	return paths, err
}

func protectedPath(path string, protected []string) bool {
	path = filepath.Clean(path)
	for _, candidate := range protected {
		if path == filepath.Clean(candidate) {
			return true
		}
	}
	return false
}

func hasProtectedDescendant(path string, protected []string) bool {
	path = filepath.Clean(path) + string(os.PathSeparator)
	for _, candidate := range protected {
		if strings.HasPrefix(filepath.Clean(candidate), path) {
			return true
		}
	}
	return false
}

func securePathAllowed(path string) bool {
	_, err := safePath(path)
	return err == nil
}

func workspaceFileErrorStatus(err error) int {
	if errors.Is(err, errWorkspaceFileTooLarge) || errors.Is(err, errWorkspaceTreeTooLarge) ||
		errors.Is(err, errWorkspaceTreeTooDeep) {
		return http.StatusRequestEntityTooLarge
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return http.StatusRequestTimeout
	}
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrInvalid) {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}
