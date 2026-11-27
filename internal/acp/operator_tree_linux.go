//go:build linux

package acp

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const operatorTagKillRounds = 200

type procCredentials struct {
	pid      int
	uid      int
	groups   []int
	zombie   bool
	complete bool
}

func parseProcCredentials(pid int, data []byte) procCredentials {
	process := procCredentials{pid: pid, uid: -1}
	var haveState, haveUID, haveGroups, malformed bool
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		switch {
		case bytes.HasPrefix(line, []byte("State:")):
			fields := bytes.Fields(line[len("State:"):])
			if len(fields) == 0 {
				malformed = true
				continue
			}
			haveState = true
			process.zombie = fields[0][0] == 'Z' || fields[0][0] == 'X'
		case bytes.HasPrefix(line, []byte("Uid:")):
			fields := bytes.Fields(line[len("Uid:"):])
			if len(fields) == 0 {
				malformed = true
				continue
			}
			uid, err := strconv.Atoi(string(fields[0]))
			if err != nil || uid < 0 {
				malformed = true
				continue
			}
			haveUID, process.uid = true, uid
		case bytes.HasPrefix(line, []byte("Groups:")):
			haveGroups = true
			for field := range bytes.FieldsSeq(line[len("Groups:"):]) {
				group, err := strconv.Atoi(string(field))
				if err != nil {
					malformed = true
					continue
				}
				process.groups = append(process.groups, group)
			}
		}
	}
	process.complete = haveState && haveUID && haveGroups && !malformed
	return process
}

func procEntryGone(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH)
}

func threadGroupLive(procRoot string, pid int) (live, ok bool) {
	tasks, err := os.ReadDir(filepath.Join(procRoot, strconv.Itoa(pid), "task"))
	if err != nil {
		return false, procEntryGone(err)
	}
	for _, task := range tasks {
		data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "task", task.Name(), "status"))
		if err != nil {
			if procEntryGone(err) {
				continue
			}
			return false, false
		}
		thread := parseProcCredentials(pid, data)
		if !thread.complete {
			return false, false
		}
		if !thread.zombie {
			return true, true
		}
	}
	return false, true
}

func listPIDs(procRoot string) (map[int]struct{}, bool) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, false
	}
	pids := make(map[int]struct{}, len(entries))
	for _, entry := range entries {
		if !isDecimalPIDName(entry.Name()) {
			continue
		}
		if pid, err := strconv.Atoi(entry.Name()); err == nil {
			pids[pid] = struct{}{}
		}
	}
	return pids, true
}

const operatorScanRelistLimit = 16

type operatorTagScan struct {
	own     []int
	foreign int
}

func scanOperatorTag(procRoot string, uid, tag int) (operatorTagScan, bool) {
	examined := map[int]struct{}{}
	var scan operatorTagScan
	for range operatorScanRelistLimit {
		pids, ok := listPIDs(procRoot)
		if !ok {
			return operatorTagScan{}, false
		}
		fresh := 0
		for pid := range pids {
			if _, seen := examined[pid]; seen {
				continue
			}
			examined[pid] = struct{}{}
			fresh++
			data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "status"))
			if err != nil {
				if procEntryGone(err) {
					continue
				}
				return operatorTagScan{}, false
			}
			process := parseProcCredentials(pid, data)
			if !process.complete {
				return operatorTagScan{}, false
			}
			if !slices.Contains(process.groups, tag) {
				continue
			}
			if process.zombie {
				live, ok := threadGroupLive(procRoot, pid)
				if !ok {
					return operatorTagScan{}, false
				}
				if !live {
					continue
				}
			}
			if uid < 0 || process.uid == uid {
				scan.own = append(scan.own, pid)
			} else {
				scan.foreign++
			}
		}
		if fresh == 0 {
			return scan, true
		}
	}
	return scan, false
}

func taggedProcesses(procRoot string, uid, tag int) ([]int, bool) {
	scan, ok := scanOperatorTag(procRoot, uid, tag)
	return scan.own, ok
}

func OperatorTagInUse(tag int) bool {
	tagged, ok := taggedProcesses("/proc", -1, tag)
	return !ok || len(tagged) > 0
}

func ReapOperatorTag(uid, tag int) {
	reapOperatorTag("/proc", uid, tag, func(pid int) {
		var status unix.WaitStatus
		_, _ = unix.Wait4(pid, &status, unix.WNOHANG, nil)
	})
}

func reapOperatorTag(procRoot string, uid, tag int, reap func(pid int)) {
	if uid <= 0 || tag < OperatorTagFirstGID || tag > OperatorTagLastGID {
		return
	}
	pids, ok := listPIDs(procRoot)
	if !ok {
		return
	}
	operatorLeaders.mu.Lock()
	defer operatorLeaders.mu.Unlock()
	for pid := range pids {
		data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "status"))
		if err != nil {
			continue
		}
		process := parseProcCredentials(pid, data)
		if !process.complete || !process.zombie || process.uid != uid || !slices.Contains(process.groups, tag) {
			continue
		}
		if _, held := operatorLeaders.pids[pid]; held {
			continue
		}
		reap(pid)
	}
}

func KillOperatorTag(uid, tag int) bool {
	return killOperatorTag("/proc", uid, tag, operatorTagKillRounds, cleanupProofObservationInterval)
}

func killOperatorTag(procRoot string, uid, tag, rounds int, interval time.Duration) bool {
	if uid <= 0 || tag < OperatorTagFirstGID || tag > OperatorTagLastGID {
		return false
	}
	clean := 0
	for range rounds {
		scan, ok := scanOperatorTag(procRoot, uid, tag)
		switch {
		case !ok:
			clean = 0
		case len(scan.own) == 0 && scan.foreign == 0:
			clean++
			if clean == 2 {
				return true
			}
		default:
			clean = 0
			operatorTagScanned()
			for _, pid := range scan.own {
				killOperatorTagProcess(procRoot, pid, uid, tag)
			}
		}
		time.Sleep(interval)
	}
	return false
}

var operatorTagScannedHook func()

func operatorTagScanned() {
	if hook := operatorTagScannedHook; hook != nil {
		hook()
	}
}

var pidfdOpen = unix.PidfdOpen

func killOperatorTagProcess(procRoot string, pid, uid, tag int) {
	pidfd, err := pidfdOpen(pid, 0)
	if err != nil {
		return
	}
	defer unix.Close(pidfd) //nolint:errcheck
	data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "status"))
	if err != nil {
		return
	}
	process := parseProcCredentials(pid, data)
	if !process.complete || process.uid != uid || !slices.Contains(process.groups, tag) {
		return
	}
	_ = unix.PidfdSendSignal(pidfd, unix.SIGKILL, nil, 0)
}
