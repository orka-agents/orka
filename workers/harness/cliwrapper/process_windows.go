//go:build windows

package cliwrapper

import (
	"os"
	"syscall"
	"time"
)

func commandSysProcAttr() *syscall.SysProcAttr { return nil }

func terminateProcessGroup(process *os.Process, grace time.Duration) {
	if process == nil {
		return
	}
	_ = process.Signal(os.Interrupt)
	if grace > 0 {
		time.Sleep(grace)
	}
	_ = process.Kill()
}

func terminateChildCredentialProcesses(time.Duration) {}
