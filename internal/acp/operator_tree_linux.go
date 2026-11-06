//go:build linux

package acp

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"time"
)

const operatorTagKillRounds = 200

type procCredentials struct {
	pid    int
	uid    int
	groups []int
	// zombie is a terminated thread group (every thread exited) awaiting
	// reaping; it can never run again, so it does not count.
	zombie bool
}

func parseProcCredentials(pid int, data []byte) procCredentials {
	process := procCredentials{pid: pid, uid: -1}
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		switch {
		case bytes.HasPrefix(line, []byte("State:")):
			if fields := bytes.Fields(line[len("State:"):]); len(fields) > 0 {
				process.zombie = fields[0][0] == 'Z' || fields[0][0] == 'X'
			}
		case bytes.HasPrefix(line, []byte("Uid:")):
			if fields := bytes.Fields(line[len("Uid:"):]); len(fields) > 0 {
				process.uid, _ = strconv.Atoi(string(fields[0]))
			}
		case bytes.HasPrefix(line, []byte("Groups:")):
			for field := range bytes.FieldsSeq(line[len("Groups:"):]) {
				if group, err := strconv.Atoi(string(field)); err == nil {
					process.groups = append(process.groups, group)
				}
			}
		}
	}
	return process
}

// threadGroupLive reports whether any thread of a zombie leader still runs:
// a leader that called pthread_exit is a zombie while its workers live.
func threadGroupLive(procRoot string, pid int) bool {
	tasks, err := os.ReadDir(filepath.Join(procRoot, strconv.Itoa(pid), "task"))
	if err != nil {
		return false
	}
	for _, task := range tasks {
		data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "task", task.Name(), "status"))
		if err != nil {
			continue
		}
		if !parseProcCredentials(pid, data).zombie {
			return true
		}
	}
	return false
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

// taggedProcesses returns live processes of uid (any uid when negative) that
// carry tag. Processes forked while the scan runs are absent from the first
// listing, so the table is re-listed and new PIDs examined until a listing
// adds none; a scan that cannot settle reports ok=false.
func taggedProcesses(procRoot string, uid, tag int) ([]int, bool) {
	examined := map[int]struct{}{}
	var tagged []int
	for range operatorScanRelistLimit {
		pids, ok := listPIDs(procRoot)
		if !ok {
			return nil, false
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
				continue // exited before inspection
			}
			process := parseProcCredentials(pid, data)
			if uid >= 0 && process.uid != uid {
				continue
			}
			if process.zombie && !threadGroupLive(procRoot, pid) {
				continue
			}
			if slices.Contains(process.groups, tag) {
				tagged = append(tagged, pid)
			}
		}
		if fresh == 0 {
			return tagged, true
		}
	}
	return tagged, false
}

// OperatorTagInUse reports whether any live process carries the tag group.
// An unreadable or unsettled process table counts as in use.
func OperatorTagInUse(tag int) bool {
	tagged, ok := taggedProcesses("/proc", -1, tag)
	return !ok || len(tagged) > 0
}

// KillOperatorTag kills every process of uid carrying the operation's tag
// group and reports whether termination was proven. Because the tag cannot be
// dropped and is inherited, this reaches every descendant, including ones that
// changed process group, session, or parent. Like the RuntimeSession UID
// cleanup proof, containment requires two consecutive settled empty scans
// separated by an observation interval, so a process forked during one scan
// is caught by the next.
func KillOperatorTag(uid, tag int) bool {
	if uid <= 0 || tag < OperatorTagFirstGID || tag > OperatorTagLastGID {
		return false
	}
	clean := 0
	for range operatorTagKillRounds {
		tagged, ok := taggedProcesses("/proc", uid, tag)
		switch {
		case !ok:
			clean = 0
		case len(tagged) == 0:
			clean++
			if clean == 2 {
				return true
			}
		default:
			clean = 0
			for _, pid := range tagged {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
		time.Sleep(cleanupProofObservationInterval)
	}
	return false
}
