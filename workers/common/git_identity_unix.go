//go:build !windows

package common

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

const (
	harnessWrapperChildUIDEnv = "ORKA_HARNESS_WRAPPER_CHILD_UID"
	harnessWrapperChildGIDEnv = "ORKA_HARNESS_WRAPPER_CHILD_GID"
)

func configureWorkspaceGitCommand(cmd *exec.Cmd) error {
	attr := gitCommandSysProcAttr()
	if attr == nil {
		attr = &syscall.SysProcAttr{}
	}
	attr.Setpgid = true
	cmd.SysProcAttr = attr
	cmd.Cancel = func() error {
		return killWorkspaceGitProcessGroup(cmd)
	}
	cmd.WaitDelay = workspaceGitWaitDelay
	return nil
}

func runWorkspaceGitCommand(cmd *exec.Cmd) error { return cmd.Run() }

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

func cleanupWorkspaceGitDescendants(cmd *exec.Cmd) {
	_ = killWorkspaceGitProcessGroup(cmd)
}

func killWorkspaceGitProcessGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return os.ErrProcessDone
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err == nil {
		return nil
	} else if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	if err := cmd.Process.Kill(); err != nil {
		if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	return nil
}
