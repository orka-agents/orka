//go:build !windows

package cliwrapper

import (
	"os"
	"syscall"
	"time"
)

func commandSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

func terminateProcessGroup(process *os.Process, grace time.Duration) {
	if process == nil {
		return
	}
	pid := process.Pid
	if pid <= 0 {
		return
	}
	pgid := -pid
	if err := syscall.Kill(pgid, syscall.SIGTERM); err != nil {
		_ = process.Signal(syscall.SIGTERM)
	}
	if grace > 0 {
		time.Sleep(grace)
	}
	if err := syscall.Kill(pgid, syscall.SIGKILL); err != nil {
		_ = process.Kill()
	}
}
