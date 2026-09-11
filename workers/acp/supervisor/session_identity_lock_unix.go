//go:build darwin || linux

package supervisor

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func lockSessionIdentityState(baseDir string) (io.Closer, error) {
	path := filepath.Join(baseDir, sessionIdentityLockFile)
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open session identity lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), sessionIdentityLockFile)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("create session identity lock handle")
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock session identity state: %w", err)
	}
	return &sessionIdentityLock{file: file}, nil
}

type sessionIdentityLock struct{ file *os.File }

func (l *sessionIdentityLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil
	if unlockErr != nil {
		return fmt.Errorf("unlock session identity state: %w", unlockErr)
	}
	return closeErr
}
