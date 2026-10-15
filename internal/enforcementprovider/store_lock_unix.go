//go:build unix

package enforcementprovider

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

func acquireStoreLock(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open enforcement store lock: %w", err)
	}
	if err = lockFile(file); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.New("enforcement store is locked by another process")
		}
		return nil, fmt.Errorf("lock enforcement store: %w", err)
	}
	return file, nil
}

func acquireStoreStateLock(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open enforcement store state: %w", err)
	}
	if err = lockStoreStateFile(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func lockStoreStateFile(file *os.File) error {
	if err := lockFile(file); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return errors.New("enforcement store state is locked through another filesystem alias")
		}
		return fmt.Errorf("lock enforcement store state: %w", err)
	}
	return nil
}

func lockFile(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func storeStateFileHasSingleLink(info fs.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink == 1
}

func storeStateFileUnlinked(info fs.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink == 0
}

func releaseStoreStateLock(file *os.File) error { return releaseStoreLock(file) }

func releaseStoreStateLocks(files []*os.File) error {
	var result error
	for _, file := range files {
		if err := releaseStoreStateLock(file); result == nil {
			result = err
		}
	}
	return result
}

func releaseStoreLock(file *os.File) error {
	if file == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	closeErr := file.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
