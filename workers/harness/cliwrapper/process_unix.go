//go:build !windows

package cliwrapper

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	childCredentialDrainMaxScans           = 32
	childCredentialDrainRequiredEmptyScans = 2
	childCredentialDrainRetryDelay         = 10 * time.Millisecond
)

type childCredentialProcessScanFunc func() ([]int, error)
type childCredentialProcessSignalFunc func(int, syscall.Signal) error
type childCredentialProcessSleepFunc func(time.Duration)

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
func terminateChildCredentialProcesses(grace time.Duration) error {
	if _, _, ok := childCredentialIDs(); !ok {
		return nil
	}
	return drainChildCredentialProcesses(
		grace,
		childCredentialDrainMaxScans,
		childCredentialPIDs,
		syscall.Kill,
		time.Sleep,
	)
}

// drainChildCredentialProcesses requires two consecutive complete empty scans.
// An incomplete scan never counts as proof, and each later non-empty scan is
// killed again so a process forked after an earlier snapshot cannot survive.
func drainChildCredentialProcesses(
	grace time.Duration,
	maxScans int,
	scan childCredentialProcessScanFunc,
	signal childCredentialProcessSignalFunc,
	sleep childCredentialProcessSleepFunc,
) error {
	if maxScans < childCredentialDrainRequiredEmptyScans {
		return fmt.Errorf("child credential process drain requires at least %d scans", childCredentialDrainRequiredEmptyScans)
	}
	if scan == nil || signal == nil || sleep == nil {
		return errors.New("child credential process drain operations are required")
	}

	termSent := false
	emptyScans := 0
	lastPIDs := []int(nil)
	var lastScanErr error
	for scanNumber := range maxScans {
		pids, scanErr := scan()
		lastPIDs = append(lastPIDs[:0], pids...)
		if scanErr != nil {
			lastScanErr = scanErr
			emptyScans = 0
		} else {
			lastScanErr = nil
			if len(pids) == 0 {
				emptyScans++
				if emptyScans >= childCredentialDrainRequiredEmptyScans {
					return nil
				}
			} else {
				emptyScans = 0
			}
		}

		wait := childCredentialDrainRetryDelay
		if len(pids) > 0 {
			sig := syscall.SIGKILL
			if !termSent {
				sig = syscall.SIGTERM
				termSent = true
				wait = grace
			}
			for _, pid := range pids {
				if err := signal(pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
					return fmt.Errorf("signal child credential process %d with %s: %w", pid, sig, err)
				}
			}
		}
		if scanNumber+1 < maxScans && wait > 0 {
			sleep(wait)
		}
	}

	if lastScanErr != nil {
		return fmt.Errorf(
			"child credential process drain could not complete a process scan after %d attempts: %w",
			maxScans,
			lastScanErr,
		)
	}
	if len(lastPIDs) > 0 {
		return fmt.Errorf(
			"child credential process drain still found %d live processes after %d scans",
			len(lastPIDs),
			maxScans,
		)
	}
	return fmt.Errorf("child credential process drain could not confirm an empty UID after %d scans", maxScans)
}

func childCredentialPIDs() ([]int, error) {
	uid, _, ok := childCredentialIDs()
	if !ok {
		return nil, nil
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("read /proc for child credential process drain: %w", err)
	}
	self := os.Getpid()
	pids := []int(nil)
	var scanErr error
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
			if errors.Is(err, os.ErrNotExist) {
				scanErr = errors.Join(scanErr, fmt.Errorf("process %d changed during child credential scan: %w", pid, err))
				continue
			}
			return pids, fmt.Errorf("read status for process %d during child credential scan: %w", pid, err)
		}
		statusUID, zombie, err := processStatusUIDAndZombie(status)
		if err != nil {
			return pids, fmt.Errorf("parse status for process %d during child credential scan: %w", pid, err)
		}
		if statusUID == uid && !zombie {
			pids = append(pids, pid)
		}
	}
	return pids, scanErr
}

func processStatusUIDAndZombie(status []byte) (int, bool, error) {
	uid := -1
	zombie := false
	foundState := false
	for line := range strings.SplitSeq(string(status), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "Uid:":
			parsedUID, err := strconv.Atoi(fields[1])
			if err != nil {
				return -1, false, fmt.Errorf("parse process UID: %w", err)
			}
			uid = parsedUID
		case "State:":
			foundState = true
			zombie = fields[1] == "Z"
		}
	}
	if uid < 0 {
		return -1, false, errors.New("process status is missing Uid")
	}
	if !foundState {
		return -1, false, errors.New("process status is missing State")
	}
	return uid, zombie, nil
}
