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
)

const operatorTagKillRounds = 200

type procCredentials struct {
	pid    int
	uid    int
	groups []int
	// zombie is a terminated thread group (every thread exited) awaiting
	// reaping; it can never run again, so it does not count.
	zombie bool
	// complete reports that State, Uid, and Groups were all present and
	// parsed. A partial or malformed record proves nothing about the process.
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

// procEntryGone reports whether a /proc read failed only because the process
// or thread already exited. Every other failure (permission, I/O, a mount that
// hides other users' processes, a malformed tree) leaves its state unknown.
func procEntryGone(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH)
}

// threadGroupLive reports whether any thread of a zombie leader still runs:
// a leader that called pthread_exit is a zombie while its workers live. ok is
// false when the thread list or a thread's state could not be read.
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

// operatorTagScan is one settled scan of the process table for a tag.
type operatorTagScan struct {
	// own are live tagged processes of the requested UID (every UID when the
	// requested UID is negative).
	own []int
	// foreign counts live tagged processes of any other UID. The supervisor
	// never kills them, but while one exists containment is unproven.
	foreign int
}

// scanOperatorTag examines every live process for tag. Processes forked while
// the scan runs are absent from the first listing, so the table is re-listed
// and new PIDs examined until a listing adds none. A scan that cannot settle,
// or a process whose state cannot be read completely for any reason other
// than having exited, reports ok=false: an unreadable process is never
// presumed gone.
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
					continue // exited before inspection
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

// taggedProcesses returns live processes of uid (any uid when negative) that
// carry tag; see scanOperatorTag.
func taggedProcesses(procRoot string, uid, tag int) ([]int, bool) {
	scan, ok := scanOperatorTag(procRoot, uid, tag)
	return scan.own, ok
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
// is caught by the next. An unsettled or unreadable scan, or a tagged process
// of another UID, resets the proof.
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
			for _, pid := range scan.own {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
		time.Sleep(interval)
	}
	return false
}
