//go:build !windows

package cliwrapper

import (
	"os"
	"strconv"
	"strings"
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

// terminateChildCredentialProcesses reaps processes still running as the
// wrapper's dedicated child UID before the server frees its single active turn
// slot. The wrapper pod runs as root and only turn subprocesses use this UID.
func terminateChildCredentialProcesses(grace time.Duration) {
	pids := childCredentialPIDs()
	if len(pids) == 0 {
		return
	}
	for _, pid := range pids {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	if grace > 0 {
		time.Sleep(grace)
	}
	for _, pid := range childCredentialPIDs() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

func childCredentialPIDs() []int {
	uid, _, ok := childCredentialIDs()
	if !ok {
		return nil
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
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
		status, err := os.ReadFile("/proc/" + entry.Name() + "/status")
		if err != nil {
			continue
		}
		if statusUID(status) == uid {
			pids = append(pids, pid)
		}
	}
	return pids
}

func statusUID(status []byte) int {
	for line := range strings.SplitSeq(string(status), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "Uid:" {
			uid, err := strconv.Atoi(fields[1])
			if err == nil {
				return uid
			}
		}
	}
	return -1
}
