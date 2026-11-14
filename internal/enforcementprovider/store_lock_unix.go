//go:build unix

package enforcementprovider

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

func validateStoreDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect enforcement store directory: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("enforcement store parent is not a directory")
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return errors.New("enforcement store parent is not owned by the current user")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("enforcement store parent permissions are too broad")
	}
	return nil
}

func acquireStoreDirectoryLock(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open enforcement store parent without following links: %w", err)
	}
	directory := os.NewFile(uintptr(fd), path)
	if err = validateOwnedDirectoryFile(directory); err != nil {
		_ = directory.Close()
		return nil, err
	}
	if err = lockFile(directory); err != nil {
		_ = directory.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.New("enforcement store parent is locked by another process")
		}
		return nil, fmt.Errorf("lock enforcement store parent: %w", err)
	}
	if err = verifyLockedDirectoryPath(path, directory); err != nil {
		_ = releaseStoreLock(directory)
		return nil, err
	}
	return directory, nil
}

func validateOwnedDirectoryFile(directory *os.File) error {
	info, err := directory.Stat()
	if err != nil {
		return fmt.Errorf("inspect locked enforcement store parent: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() {
		return errors.New("locked enforcement store parent is not a directory")
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return errors.New("locked enforcement store parent is not owned by the current user")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("locked enforcement store parent permissions are too broad")
	}
	return nil
}

func verifyLockedDirectoryPath(path string, directory *os.File) error {
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("recheck enforcement store parent path: %w", err)
	}
	lockedInfo, err := directory.Stat()
	if err != nil {
		return fmt.Errorf("recheck enforcement store parent inode: %w", err)
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(pathInfo, lockedInfo) {
		return errors.New("enforcement store parent path was replaced during acquisition")
	}
	return validateOwnedDirectoryFile(directory)
}

func acquireStoreLock(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open enforcement store lock without following links: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if err = validateOwnedLockFile(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err = lockFile(file); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.New("enforcement store is locked by another process")
		}
		return nil, fmt.Errorf("lock enforcement store: %w", err)
	}
	if err = verifyLockedPath(path, file); err != nil {
		_ = releaseStoreLock(file)
		return nil, err
	}
	return file, nil
}

func validateOwnedLockFile(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect enforcement store lock: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() {
		return errors.New("enforcement store lock is not a regular file")
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return errors.New("enforcement store lock is not owned by the current user")
	}
	if stat.Nlink != 1 {
		return errors.New("enforcement store lock must have exactly one link")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("enforcement store lock permissions are too broad")
	}
	return nil
}

func verifyLockedPath(path string, file *os.File) error {
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("recheck enforcement store lock path: %w", err)
	}
	lockedInfo, err := file.Stat()
	if err != nil {
		return fmt.Errorf("recheck enforcement store lock inode: %w", err)
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(pathInfo, lockedInfo) {
		return errors.New("enforcement store lock path was replaced during acquisition")
	}
	return validateOwnedLockFile(file)
}

func acquireStoreStateLock(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, syscall.ENOENT) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open enforcement store state without following links: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
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

func storeStateFileOwnedByCurrentUser(info fs.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
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
