//go:build !windows

package cliwrapper

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func createResultFileNoFollow(workDir, path string) (*os.File, error) {
	if strings.TrimSpace(workDir) == "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("create result file directory: %w", err)
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("remove stale result file: %w", err)
		}
		return openResultFileNoFollow(path, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, 0o600)
	}
	return createResultFileInWorkspaceNoFollow(workDir, path)
}

func openResultFileNoFollow(path string, flags ...int) (*os.File, error) {
	openFlags := unix.O_RDONLY
	mode := uint32(0)
	if len(flags) > 0 {
		openFlags = flags[0]
		if len(flags) > 1 {
			mode = uint32(flags[1])
		}
	}
	fd, err := unix.Open(path, openFlags|unix.O_CLOEXEC|unix.O_NOFOLLOW, mode)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("wrap result file descriptor")
	}
	return file, nil
}

func createResultFileInWorkspaceNoFollow(workDir, path string) (*os.File, error) {
	root, rel, err := workspaceRelativePath(workDir, path, "result file")
	if err != nil {
		return nil, err
	}
	return openRelativeResultFileNoFollow(root, rel, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, 0o600, true)
}

func openResultFileInWorkspaceNoFollow(workDir, path string) (*os.File, error) {
	root, rel, err := workspaceRelativePath(workDir, path, "result file")
	if err != nil {
		return nil, err
	}
	return openRelativeResultFileNoFollow(root, rel, unix.O_RDONLY, 0, false)
}

func openRelativeResultFileNoFollow(root, rel string, flags int, mode uint32, createParents bool) (*os.File, error) {
	cleaned := filepath.Clean(rel)
	if cleaned == "." {
		return nil, fmt.Errorf("result file path must not be workspace root")
	}
	parts := strings.Split(cleaned, string(filepath.Separator))
	dirFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer func() {
		if dirFD >= 0 {
			_ = unix.Close(dirFD)
		}
	}()
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, fmt.Errorf("invalid result file path component %q", part)
		}
		last := i == len(parts)-1
		if last {
			if createParents {
				if err := unix.Unlinkat(dirFD, part, 0); err != nil && !os.IsNotExist(err) {
					return nil, err
				}
			}
			fd, err := unix.Openat(dirFD, part, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW, mode)
			if err != nil {
				return nil, err
			}
			file := os.NewFile(uintptr(fd), filepath.Join(root, cleaned))
			if file == nil {
				_ = unix.Close(fd)
				return nil, fmt.Errorf("wrap result file descriptor")
			}
			return file, nil
		}
		nextFD, err := unix.Openat(dirFD, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil && createParents && os.IsNotExist(err) {
			if mkErr := unix.Mkdirat(dirFD, part, 0o755); mkErr != nil && !os.IsExist(mkErr) {
				return nil, mkErr
			}
			nextFD, err = unix.Openat(dirFD, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		}
		if err != nil {
			return nil, err
		}
		_ = unix.Close(dirFD)
		dirFD = nextFD
	}
	return nil, fmt.Errorf("result file path is required")
}

func markOpenResultFileUnwritten(file *os.File, _ string) error {
	tv := unix.NsecToTimeval(unwrittenResultFileModTime.UnixNano())
	return unix.Futimes(int(file.Fd()), []unix.Timeval{tv, tv})
}

func resultFileLinkCount(info os.FileInfo) (uint64, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(stat.Nlink), true //nolint:unconvert // Stat_t.Nlink is narrower on Darwin and uint64 on Linux.
}
