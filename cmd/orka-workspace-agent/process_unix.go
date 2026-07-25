//go:build !windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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

// childProcessReaper owns wait responsibility for descendants adopted by the
// workspace-agent while preserving exec.Cmd.Wait ownership of direct commands.
// The mutex closes the Start/register race: the reaper cannot inspect a newly
// started direct child until that child is registered.
type childProcessReaper struct {
	enabled bool

	startOnce      sync.Once
	mu             sync.Mutex
	directChildren map[int]*exec.Cmd
	listChildren   func(int) ([]int, error)
	wake           chan struct{}
}

var workspaceChildProcessReaper = newChildProcessReaper(os.Getpid() == 1)

func newChildProcessReaper(enabled bool) *childProcessReaper {
	return &childProcessReaper{
		enabled:        enabled,
		directChildren: make(map[int]*exec.Cmd),
		listChildren:   childProcessIDs,
		wake:           make(chan struct{}, 1),
	}
}

func (r *childProcessReaper) start() {
	if !r.enabled {
		return
	}
	r.startOnce.Do(func() {
		ready := make(chan struct{})
		go r.run(ready)
		<-ready
	})
}

func (r *childProcessReaper) run(ready chan<- struct{}) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGCHLD)
	close(ready)
	defer signal.Stop(signals)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-signals:
		case <-r.wake:
		case <-ticker.C:
		}
		_, _ = r.reapAdoptedChildren()
	}
}

func (r *childProcessReaper) notify() {
	if !r.enabled {
		return
	}
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func startCommand(cmd *exec.Cmd) error {
	return workspaceChildProcessReaper.startCommand(cmd)
}

func (r *childProcessReaper) startCommand(cmd *exec.Cmd) error {
	if !r.enabled {
		return cmd.Start()
	}
	r.start()
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := cmd.Start(); err != nil {
		return err
	}
	r.directChildren[cmd.Process.Pid] = cmd
	return nil
}

func waitCommand(cmd *exec.Cmd) error {
	return workspaceChildProcessReaper.waitCommand(cmd)
}

func (r *childProcessReaper) waitCommand(cmd *exec.Cmd) error {
	if !r.enabled {
		return cmd.Wait()
	}
	pid := commandProcessGroupID(cmd)
	err := cmd.Wait()
	r.mu.Lock()
	if r.directChildren[pid] == cmd {
		delete(r.directChildren, pid)
	}
	r.mu.Unlock()
	r.notify()
	return err
}

// reapAdoptedChildren reaps only unregistered direct children of the agent.
// Registered children remain exclusively owned by exec.Cmd.Wait. The returned
// count is the number of adopted children that were still running when scanned.
func (r *childProcessReaper) reapAdoptedChildren() (int, error) {
	if !r.enabled {
		return 0, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	children, err := r.listChildren(os.Getpid())
	if err != nil {
		return 0, err
	}
	remaining := 0
	for _, pid := range children {
		if _, managed := r.directChildren[pid]; managed {
			continue
		}
		for {
			var status syscall.WaitStatus
			waited, waitErr := syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
			if errors.Is(waitErr, syscall.EINTR) {
				continue
			}
			if errors.Is(waitErr, syscall.ECHILD) || errors.Is(waitErr, syscall.ESRCH) {
				break
			}
			if waitErr != nil {
				return remaining, fmt.Errorf("reap adopted workspace process %d: %w", pid, waitErr)
			}
			if waited == 0 {
				remaining++
			}
			break
		}
	}
	return remaining, nil
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
	workspaceChildProcessReaper.start()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		remainingChildren, err := workspaceChildProcessReaper.reapAdoptedChildren()
		if err != nil {
			return err
		}
		pids, err := otherProcessIDs()
		if err != nil {
			return err
		}
		if len(pids) == 0 && remainingChildren == 0 {
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
		state, _, _, ok := processStat(pid)
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
		state, _, processGroup, ok := processStat(pid)
		if ok && state != "Z" && processGroup == groupID {
			return true, true
		}
	}
	return false, true
}

func childProcessIDs(parentID int) ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("read process table: %w", err)
	}
	children := make([]int, 0)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		_, processParentID, _, ok := processStat(pid)
		if ok && processParentID == parentID {
			children = append(children, pid)
		}
	}
	return children, nil
}

func processStat(pid int) (string, int, int, bool) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return "", 0, 0, false
	}
	line := string(data)
	endCommand := strings.LastIndex(line, ")")
	if endCommand < 0 || endCommand+2 >= len(line) {
		return "", 0, 0, false
	}
	fields := strings.Fields(line[endCommand+2:])
	if len(fields) < 3 {
		return "", 0, 0, false
	}
	parentID, err := strconv.Atoi(fields[1])
	if err != nil {
		return "", 0, 0, false
	}
	groupID, err := strconv.Atoi(fields[2])
	if err != nil {
		return "", 0, 0, false
	}
	return fields[0], parentID, groupID, true
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
