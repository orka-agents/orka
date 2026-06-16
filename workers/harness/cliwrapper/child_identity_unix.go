//go:build !windows

package cliwrapper

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	EnvChildUID = "ORKA_HARNESS_WRAPPER_CHILD_UID"
	EnvChildGID = "ORKA_HARNESS_WRAPPER_CHILD_GID"
)

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

func chownPathForChild(path string) error {
	uid, gid, ok := childCredentialIDs()
	if !ok || strings.TrimSpace(path) == "" {
		return nil
	}
	return os.Lchown(path, uid, gid)
}

func chownTreeForChild(path string) error {
	uid, gid, ok := childCredentialIDs()
	if !ok || strings.TrimSpace(path) == "" {
		return nil
	}
	return filepath.WalkDir(path, func(p string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(p, uid, gid)
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
