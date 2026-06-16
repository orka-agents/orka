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

func chownTreeForChild(path string) error {
	uid, gid, ok := childCredentialIDs()
	if !ok || strings.TrimSpace(path) == "" {
		return nil
	}
	return filepath.WalkDir(path, func(p string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Chown(p, uid, gid)
	})
}
