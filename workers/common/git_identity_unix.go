//go:build !windows

package common

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

const (
	harnessWrapperChildUIDEnv = "ORKA_HARNESS_WRAPPER_CHILD_UID"
	harnessWrapperChildGIDEnv = "ORKA_HARNESS_WRAPPER_CHILD_GID"
)

func gitCommandSysProcAttr() *syscall.SysProcAttr {
	if os.Geteuid() != 0 {
		return nil
	}
	uid, err := strconv.Atoi(strings.TrimSpace(os.Getenv(harnessWrapperChildUIDEnv)))
	if err != nil || uid <= 0 {
		return nil
	}
	gid, err := strconv.Atoi(strings.TrimSpace(os.Getenv(harnessWrapperChildGIDEnv)))
	if err != nil || gid <= 0 {
		return nil
	}
	return &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}}
}
