//go:build !windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func configureCommandCancellation(cmd *exec.Cmd, isolate bool, uid, gid uint32) {
	attrs := &syscall.SysProcAttr{Setpgid: true}
	if isolate {
		attrs.Credential = &syscall.Credential{Uid: uid, Gid: gid}
	}
	cmd.SysProcAttr = attrs
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return terminateProcessGroup(cmd.Process.Pid)
	}
}

func commandProcessGroupID(cmd *exec.Cmd) int {
	if cmd == nil || cmd.Process == nil {
		return 0
	}
	return cmd.Process.Pid
}

func terminateProcessGroup(groupID int) error {
	if groupID <= 0 {
		return nil
	}
	err := syscall.Kill(-groupID, syscall.SIGKILL)
	if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func processGroupAlive(groupID int) bool {
	if groupID <= 0 {
		return false
	}
	if alive, ok := processGroupAliveFromProc(groupID); ok {
		return alive
	}
	err := syscall.Kill(-groupID, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func validateControlAuthFile(path string) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("workspace-agent must run as root to isolate control auth from task commands")
	}
	if os.Getpid() != 1 {
		return fmt.Errorf("workspace-agent must be PID 1 in a dedicated process namespace")
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("control auth file must not be group- or world-readable")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 {
		return fmt.Errorf("control auth file must be owned by root")
	}
	return nil
}

func terminateAttachmentProcesses(ctx context.Context) error {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		pids, err := otherProcessIDs()
		if err != nil {
			return err
		}
		if len(pids) == 0 {
			return nil
		}
		for _, pid := range pids {
			if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func otherProcessIDs() ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("read dedicated process namespace: %w", err)
	}
	self := os.Getpid()
	pids := make([]int, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(filepath.Base(entry.Name()))
		if err != nil || pid <= 0 || pid == self {
			continue
		}
		state, _, ok := processStat(pid)
		if ok && state == "Z" {
			continue
		}
		pids = append(pids, pid)
	}
	return pids, nil
}

func processGroupAliveFromProc(groupID int) (bool, bool) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false, false
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		state, processGroup, ok := processStat(pid)
		if ok && state != "Z" && processGroup == groupID {
			return true, true
		}
	}
	return false, true
}

func processStat(pid int) (string, int, bool) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return "", 0, false
	}
	line := string(data)
	endCommand := strings.LastIndex(line, ")")
	if endCommand < 0 || endCommand+2 >= len(line) {
		return "", 0, false
	}
	fields := strings.Fields(line[endCommand+2:])
	if len(fields) < 3 {
		return "", 0, false
	}
	groupID, err := strconv.Atoi(fields[2])
	if err != nil {
		return "", 0, false
	}
	return fields[0], groupID, true
}

func validatePrivateKeyFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("TLS private key must not be group- or world-readable")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 {
		return fmt.Errorf("TLS private key must be owned by root")
	}
	return nil
}
