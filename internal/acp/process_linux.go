//go:build linux

package acp

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const execHelperProtocolVersion = "orka-acp-exec-v1"

func newChildCommand(cfg ProcessConfig) (*exec.Cmd, error) {
	helperCommand := strings.TrimSpace(cfg.ExecHelperCommand)
	if helperCommand == "" {
		helperCommand = DefaultExecHelperCommand
	}
	if !filepath.IsAbs(helperCommand) {
		return nil, fmt.Errorf("ACP exec helper command must be an absolute path")
	}
	helperArgs := []string{
		execHelperProtocolVersion,
		strconv.Itoa(os.Getpid()),
		strconv.Itoa(cfg.UID),
		strconv.Itoa(cfg.GID),
		cfg.Command,
	}
	helperArgs = append(helperArgs, cfg.Args...)
	cmd := exec.Command(helperCommand, helperArgs...)
	cmd.SysProcAttr = childSysProcAttr(cfg.UID, cfg.GID)
	return cmd, nil
}

// RunExecHelper validates the trusted launch envelope, applies hard resource
// limits to the current process, and only then replaces it with the adapter.
// It is exported solely for cmd/orka-acp-exec-helper.
func RunExecHelper(args, environment []string) error {
	invocation, err := parseExecHelperInvocation(args)
	if err != nil {
		return err
	}
	if err := verifyExecHelperEnvelope(invocation); err != nil {
		return err
	}
	if err := applyCurrentProcessLimits(); err != nil {
		return fmt.Errorf("apply child resource limits: %w", err)
	}
	argv := append([]string{invocation.command}, invocation.args...)
	return unix.Exec(invocation.command, argv, environment)
}

type execHelperInvocation struct {
	parentPID int
	uid       int
	gid       int
	command   string
	args      []string
}

func parseExecHelperInvocation(args []string) (execHelperInvocation, error) {
	if len(args) < 5 || args[0] != execHelperProtocolVersion {
		return execHelperInvocation{}, fmt.Errorf("invalid ACP exec helper invocation")
	}
	parentPID, err := strconv.Atoi(args[1])
	if err != nil || parentPID <= 0 {
		return execHelperInvocation{}, fmt.Errorf("invalid ACP exec helper parent PID")
	}
	uid, err := strconv.Atoi(args[2])
	if err != nil || uid <= 0 {
		return execHelperInvocation{}, fmt.Errorf("invalid ACP exec helper UID")
	}
	gid, err := strconv.Atoi(args[3])
	if err != nil || gid <= 0 {
		return execHelperInvocation{}, fmt.Errorf("invalid ACP exec helper GID")
	}
	command := strings.TrimSpace(args[4])
	if command == "" || !filepath.IsAbs(command) {
		return execHelperInvocation{}, fmt.Errorf("invalid ACP exec helper adapter command")
	}
	return execHelperInvocation{
		parentPID: parentPID,
		uid:       uid,
		gid:       gid,
		command:   command,
		args:      append([]string(nil), args[5:]...),
	}, nil
}

func verifyExecHelperEnvelope(invocation execHelperInvocation) error {
	if os.Getuid() != invocation.uid || os.Geteuid() != invocation.uid {
		return fmt.Errorf("ACP exec helper UID fence mismatch")
	}
	if os.Getgid() != invocation.gid || os.Getegid() != invocation.gid {
		return fmt.Errorf("ACP exec helper GID fence mismatch")
	}
	groups, err := os.Getgroups()
	if err != nil {
		return fmt.Errorf("inspect ACP exec helper supplementary groups: %w", err)
	}
	if len(groups) != 0 {
		return fmt.Errorf("ACP exec helper supplementary-group fence mismatch")
	}
	if unix.Getpgrp() != os.Getpid() {
		return fmt.Errorf("ACP exec helper process-group fence mismatch")
	}
	if os.Getppid() != invocation.parentPID {
		return fmt.Errorf("ACP exec helper parent fence mismatch")
	}
	if err := unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(unix.SIGKILL), 0, 0, 0); err != nil {
		return fmt.Errorf("set ACP adapter parent-death signal: %w", err)
	}
	if os.Getppid() != invocation.parentPID {
		return fmt.Errorf("ACP exec helper parent exited during launch")
	}
	return nil
}

type childResourceLimit struct {
	resource int
	maximum  uint64
}

func childResourceLimits() []childResourceLimit {
	return []childResourceLimit{
		{resource: unix.RLIMIT_NPROC, maximum: 256},
		{resource: unix.RLIMIT_NOFILE, maximum: 4096},
		{resource: unix.RLIMIT_FSIZE, maximum: 1 << 30},
	}
}

func applyCurrentProcessLimits() error {
	for _, configured := range childResourceLimits() {
		var inherited unix.Rlimit
		if err := unix.Getrlimit(configured.resource, &inherited); err != nil {
			return err
		}
		target := min(configured.maximum, inherited.Cur, inherited.Max)
		limit := &unix.Rlimit{Cur: target, Max: target}
		if err := unix.Setrlimit(configured.resource, limit); err != nil {
			return err
		}
		var applied unix.Rlimit
		if err := unix.Getrlimit(configured.resource, &applied); err != nil {
			return err
		}
		if applied.Cur != target || applied.Max != target {
			return fmt.Errorf("resource limit verification failed")
		}
	}
	return nil
}

func childSysProcAttr(uid, gid int) *syscall.SysProcAttr {
	attr := &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if os.Geteuid() == 0 {
		attr.Credential = &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: []uint32{}}
	}
	return attr
}

func signalProcessGroup(pid int, signal os.Signal) error {
	sig, ok := signal.(syscall.Signal)
	if !ok {
		return syscall.EINVAL
	}
	return syscall.Kill(-pid, sig)
}

func terminateSignal() os.Signal { return syscall.SIGTERM }
func killSignal() os.Signal      { return syscall.SIGKILL }
func stopSignal() os.Signal      { return syscall.SIGSTOP }
func continueSignal() os.Signal  { return syscall.SIGCONT }

type procProcess struct {
	pid     int
	realUID uint64
	state   byte
}

func processesForUID(uid int) ([]int, bool) {
	processes, supported := inspectProcessesForUIDAt("/proc", uid, os.Getpid())
	if !supported {
		return nil, false
	}
	pids := make([]int, 0, len(processes))
	for _, process := range processes {
		pids = append(pids, process.pid)
	}
	return pids, true
}

func inspectProcessesForUIDAt(procRoot string, uid, selfPID int) ([]procProcess, bool) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, false
	}
	var processes []procProcess
	for _, entry := range entries {
		name := entry.Name()
		if !isDecimalPIDName(name) {
			continue
		}
		pid, err := strconv.Atoi(name)
		if err != nil || pid <= 0 || strconv.Itoa(pid) != name {
			return nil, false
		}
		if pid == selfPID {
			continue
		}
		data, err := os.ReadFile(filepath.Join(procRoot, entry.Name(), "status"))
		if errors.Is(err, fs.ErrNotExist) {
			// A numeric /proc entry may disappear between ReadDir and ReadFile.
			continue
		}
		if err != nil {
			return nil, false
		}
		process, err := parseProcStatus(pid, data)
		if err != nil {
			return nil, false
		}
		if process.realUID == uint64(uid) {
			processes = append(processes, process)
		}
	}
	sort.Slice(processes, func(i, j int) bool { return processes[i].pid < processes[j].pid })
	return processes, true
}

func isDecimalPIDName(name string) bool {
	if name == "" {
		return false
	}
	for _, character := range name {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func parseProcStatus(pid int, data []byte) (procProcess, error) {
	process := procProcess{pid: pid}
	var foundUID, foundState bool
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "Uid:":
			if foundUID || len(fields) < 5 {
				return procProcess{}, fmt.Errorf("malformed process UID status")
			}
			for index, field := range fields[1:5] {
				value, err := strconv.ParseUint(field, 10, 32)
				if err != nil {
					return procProcess{}, fmt.Errorf("malformed process UID status")
				}
				if index == 0 {
					process.realUID = value
				}
			}
			foundUID = true
		case "State:":
			if foundState || len(fields) < 2 || len(fields[1]) != 1 {
				return procProcess{}, fmt.Errorf("malformed process state status")
			}
			process.state = fields[1][0]
			foundState = true
		}
	}
	if !foundUID || !foundState {
		return procProcess{}, fmt.Errorf("incomplete process status")
	}
	return process, nil
}

func reapExitedProcessesForUID(uid int) {
	reapExitedProcessesForUIDExcept(uid, 0)
}

func reapExitedProcessesForUIDExcept(uid, excludedPID int) {
	processes, supported := inspectProcessesForUIDAt("/proc", uid, os.Getpid())
	if !supported {
		return
	}
	for _, process := range processes {
		if process.pid == excludedPID || process.state != 'Z' {
			continue
		}
		var status unix.WaitStatus
		_, _ = unix.Wait4(process.pid, &status, unix.WNOHANG, nil)
	}
}

func processesStoppedForUID(uid, leaderPID int) (bool, bool, error) {
	processes, supported := inspectProcessesForUIDAt("/proc", uid, os.Getpid())
	if !supported {
		return false, false, fmt.Errorf("inspect processes for UID %d", uid)
	}
	return sessionProcessesStopped(processes, leaderPID), true, nil
}

func processesResumedForUID(uid, leaderPID int) (bool, bool, error) {
	processes, supported := inspectProcessesForUIDAt("/proc", uid, os.Getpid())
	if !supported {
		return false, false, fmt.Errorf("inspect processes for UID %d", uid)
	}
	return sessionProcessesResumed(processes, leaderPID), true, nil
}

func sessionProcessesStopped(processes []procProcess, leaderPID int) bool {
	leaderStopped := false
	for _, process := range processes {
		switch process.state {
		case 'Z':
			continue
		case 'T', 't':
			if process.pid == leaderPID {
				leaderStopped = true
			}
		default:
			return false
		}
	}
	return leaderStopped
}

func sessionProcessesResumed(processes []procProcess, leaderPID int) bool {
	leaderResumed := false
	for _, process := range processes {
		switch process.state {
		case 'Z', 'X', 'x':
			continue
		case 'T', 't':
			return false
		case 'R', 'S', 'D', 'I', 'K', 'P', 'W':
			if process.pid == leaderPID {
				leaderResumed = true
			}
		default:
			return false
		}
	}
	return leaderResumed
}

func signalProcessesForUID(uid int, signal os.Signal) (bool, error) {
	if os.Geteuid() != 0 || uid == os.Getuid() {
		return false, nil
	}
	sig, ok := signal.(syscall.Signal)
	if !ok {
		return true, syscall.EINVAL
	}
	processes, supported := inspectProcessesForUIDAt("/proc", uid, os.Getpid())
	if !supported {
		return false, nil
	}
	var first error
	for _, process := range processes {
		if process.state == 'Z' {
			continue
		}
		if err := syscall.Kill(process.pid, sig); err != nil && err != syscall.ESRCH && first == nil {
			first = err
		}
	}
	return true, first
}
