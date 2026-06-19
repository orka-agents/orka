//go:build !windows

package cliwrapper

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

func commandSysProcAttr() *syscall.SysProcAttr {
	attr := &syscall.SysProcAttr{Setpgid: true}
	childIdentityMu.Lock()
	defer childIdentityMu.Unlock()
	if uid, gid, ok := childCredentialIDs(); ok {
		attr.Credential = &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
	}
	return attr
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

func terminateMarkedChildProcesses(marker string, grace time.Duration) {
	pids := markedChildPIDs(marker)
	if len(pids) == 0 {
		return
	}
	for _, pid := range pids {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	if grace > 0 {
		time.Sleep(grace)
	}
	for _, pid := range markedChildPIDs(marker) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

func markedChildPIDs(marker string) []int {
	if marker == "" {
		return nil
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	needle := []byte(turnProcessMarkerEnv + "=" + marker)
	self := os.Getpid()
	pids := []int(nil)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 || pid == self {
			continue
		}
		env, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "environ"))
		if err != nil {
			continue
		}
		for item := range bytes.SplitSeq(env, []byte{0}) {
			if bytes.Equal(item, needle) {
				pids = append(pids, pid)
				break
			}
		}
	}
	return pids
}
