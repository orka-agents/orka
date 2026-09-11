//go:build darwin || linux

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package sqlite

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const databaseLockSuffix = ".controller.lock"

// lockDatabaseFile holds a non-blocking advisory lock adjacent to the SQLite
// database. It deliberately runs before sql.Open and migrations.
func lockDatabaseFile(databasePath string) (io.Closer, error) {
	databasePath = strings.TrimSpace(databasePath)
	if databasePath == "" || databasePath == ":memory:" {
		return nil, fmt.Errorf("production SQLite database path must name a filesystem file")
	}
	lockPath := databasePath + databaseLockSuffix
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o750); err != nil {
		return nil, fmt.Errorf("create SQLite lock directory: %w", err)
	}
	fd, err := unix.Open(lockPath, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open SQLite process lock %q: %w", lockPath, err)
	}
	file := os.NewFile(uintptr(fd), lockPath)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("create SQLite process lock handle %q", lockPath)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("acquire SQLite process lock %q: %w", lockPath, err)
	}
	return &databaseFileLock{file: file, path: lockPath}, nil
}

type databaseFileLock struct {
	file *os.File
	path string
}

func (l *databaseFileLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil
	if unlockErr != nil {
		return fmt.Errorf("release SQLite process lock %q: %w", l.path, unlockErr)
	}
	return closeErr
}
