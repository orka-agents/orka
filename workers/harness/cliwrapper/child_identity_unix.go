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

func chownTreeForChild(path string) error {
	uid, gid, ok := childCredentialIDs()
	if !ok || strings.TrimSpace(path) == "" {
		return nil
	}
	return chownTree(path, uid, gid)
}

func chownTree(path string, uid, gid int) error {
	return filepath.WalkDir(path, func(p string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(p, uid, gid)
	})
}

func prepareArtifactsForChild(path string) error {
	uid, _, ok := childCredentialIDs()
	if !ok || strings.TrimSpace(path) == "" {
		return nil
	}
	return filepath.WalkDir(path, func(p string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			fd, err := unix.Open(p, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
			if err != nil {
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
