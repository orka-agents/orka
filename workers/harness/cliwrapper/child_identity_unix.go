//go:build !windows

package cliwrapper

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

const (
	EnvChildUID = "ORKA_HARNESS_WRAPPER_CHILD_UID"
	EnvChildGID = "ORKA_HARNESS_WRAPPER_CHILD_GID"
)

var childIdentityMu sync.Mutex

func childCredentialIDs() (int, int, bool) {
	if os.Geteuid() != 0 {
		return 0, 0, false
	}
	uid, err := strconv.Atoi(strings.TrimSpace(os.Getenv(EnvChildUID)))
	if err != nil || uid <= 0 {
		return 0, 0, false
	}
	gid, err := strconv.Atoi(strings.TrimSpace(os.Getenv(EnvChildGID)))
	if err != nil || gid <= 0 {
		return 0, 0, false
	}
	return uid, gid, true
}

func chownTreeForChild(path string, excludePaths ...string) error {
	uid, gid, ok := childCredentialIDs()
	if !ok || strings.TrimSpace(path) == "" {
		return nil
	}
	return chownTree(path, uid, gid, excludePaths...)
}

func chownTree(path string, uid, gid int, excludePaths ...string) error {
	excluded := make([]string, 0, len(excludePaths))
	for _, excludePath := range excludePaths {
		if strings.TrimSpace(excludePath) == "" {
			continue
		}
		if abs, err := filepath.Abs(excludePath); err == nil {
			excluded = append(excluded, filepath.Clean(abs))
		}
	}
	paths := []string{}
	if err := filepath.WalkDir(path, func(p string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		absPath, absErr := filepath.Abs(p)
		if absErr != nil {
			return absErr
		}
		cleanPath := filepath.Clean(absPath)
		for _, excludePath := range excluded {
			if cleanPath == excludePath || strings.HasPrefix(cleanPath, excludePath+string(filepath.Separator)) {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		paths = append(paths, p)
		return nil
	}); err != nil {
		return err
	}
	for i := len(paths) - 1; i >= 0; i-- {
		if err := os.Lchown(paths[i], uid, gid); err != nil {
			return err
		}
	}
	return nil
}

func prepareArtifactsForChild(path string) error {
	uid, _, ok := childCredentialIDs()
	if !ok || strings.TrimSpace(path) == "" {
		return nil
	}
	root := filepath.Clean(path)
	return filepath.WalkDir(root, func(p string, entry os.DirEntry, err error) error {
		if err != nil {
			if filepath.Clean(p) != root && os.IsPermission(err) {
				return filepath.SkipDir
			}
			return err
		}
		if entry.IsDir() {
			fd, err := unix.Open(p, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
			if err != nil {
				if filepath.Clean(p) != root && os.IsPermission(err) {
					return filepath.SkipDir
				}
				return err
			}
			if err := unix.Fchown(fd, uid, 0); err != nil {
				_ = unix.Close(fd)
				return err
			}
			if err := unix.Fchmod(fd, 0o770); err != nil {
				_ = unix.Close(fd)
				return err
			}
			return unix.Close(fd)
		}
		return os.Lchown(p, uid, 0)
	})
}

func prepareArtifactsForWrapper(path string) error {
	if strings.TrimSpace(path) == "" || os.Geteuid() != 0 {
		return nil
	}
	root := filepath.Clean(path)
	return filepath.WalkDir(root, func(p string, entry os.DirEntry, err error) error {
		if err != nil {
			if filepath.Clean(p) != root && os.IsPermission(err) {
				return filepath.SkipDir
			}
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if entry.IsDir() {
			fd, err := unix.Open(p, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
			if err != nil {
				if filepath.Clean(p) != root && os.IsPermission(err) {
					return filepath.SkipDir
				}
				return err
			}
			if err := unix.Fchown(fd, 0, 0); err != nil {
				_ = unix.Close(fd)
				return err
			}
			if err := unix.Fchmod(fd, 0o750); err != nil {
				_ = unix.Close(fd)
				return err
			}
			return unix.Close(fd)
		}
		info, err := entry.Info()
		if err != nil {
			if filepath.Clean(p) != root && os.IsPermission(err) {
				return nil
			}
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if err := unix.Lchown(p, 0, 0); err != nil {
			return err
		}
		fd, err := unix.Open(p, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		if err := unix.Fchmod(fd, 0o640); err != nil {
			_ = unix.Close(fd)
			return err
		}
		return unix.Close(fd)
	})
}

func prepareHomeForChild(path string) error {
	uid, _, ok := childCredentialIDs()
	if !ok {
		return os.Chmod(path, 0o700)
	}
	if err := os.Lchown(path, 0, 0); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o770); err != nil {
		return err
	}
	if err := os.Lchown(path, uid, 0); err != nil {
		_ = os.Chmod(path, 0o700)
		return err
	}
	return nil
}

func prepareControlFileForChild(path string, mode os.FileMode) error {
	uid, _, ok := childCredentialIDs()
	if !ok {
		return nil
	}
	if err := os.Lchown(path, 0, 0); err != nil {
		return err
	}
	if err := os.Chmod(path, mode); err != nil {
		return err
	}
	if err := os.Lchown(path, uid, 0); err != nil {
		_ = os.Chmod(path, 0o600)
		return err
	}
	return nil
}

func prepareOpenControlFileForChild(file *os.File, mode os.FileMode) error {
	uid, _, ok := childCredentialIDs()
	if !ok {
		return nil
	}
	if err := file.Chown(0, 0); err != nil {
		return err
	}
	if err := file.Chmod(mode); err != nil {
		return err
	}
	if err := file.Chown(uid, 0); err != nil {
		_ = file.Chmod(0o600)
		return err
	}
	return nil
}

func removeAllForChild(path string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	if _, _, ok := childCredentialIDs(); !ok {
		return os.RemoveAll(path)
	}
	cmd := exec.Command("rm", "-rf", "--", path)
	cmd.SysProcAttr = commandSysProcAttr()
	return cmd.Run()
}

func suspendChildIdentity() func() {
	childIdentityMu.Lock()
	uid, hadUID := os.LookupEnv(EnvChildUID)
	gid, hadGID := os.LookupEnv(EnvChildGID)
	_ = os.Unsetenv(EnvChildUID)
	_ = os.Unsetenv(EnvChildGID)
	return func() {
		defer childIdentityMu.Unlock()
		if hadUID {
			_ = os.Setenv(EnvChildUID, uid)
		} else {
			_ = os.Unsetenv(EnvChildUID)
		}
		if hadGID {
			_ = os.Setenv(EnvChildGID, gid)
		} else {
			_ = os.Unsetenv(EnvChildGID)
		}
	}
}
