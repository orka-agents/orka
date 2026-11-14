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

// ReapOperatorTag reaps exited processes of uid that carry the operation's
// tag group. The supervisor is a child subreaper, so an operation's orphaned
// descendants are reparented to it; the session-wide reaper runs only on
// freeze and stop, and until then each such zombie keeps a PID allocated
// against the session's process limit. Only the operation's processes carry
// the tag, so the agent is never reaped here, and held operator leaders stay
// with their owners. Call it only after containment is proven and before the
// tag is released for reuse.
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
	// Held for the whole pass, as in the session-wide reaper, so a leader
	// cannot start and be registered behind the scan.
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
		// An unreaped zombie's PID cannot be reused, and no other owner
		// waits for a reparented descendant, so the PID still names it.
		reap(pid)
	}
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
			operatorTagScanned()
			for _, pid := range scan.own {
				killOperatorTagProcess(procRoot, pid, uid, tag)
			}
		}
		time.Sleep(interval)
	}
	return false
}

// operatorTagScannedHook runs after a scan found tagged processes and before
// they are signaled. Tests use it to replace a process in between.
var operatorTagScannedHook func()

func operatorTagScanned() {
	if hook := operatorTagScannedHook; hook != nil {
		hook()
	}
}

// pidfdOpen is replaceable in tests that simulate a kernel without pidfds.
var pidfdOpen = unix.PidfdOpen

// killOperatorTagProcess SIGKILLs pid only if it is still a process of the
// operation. A scanned PID can exit and be reused by an unrelated process,
// possibly of another UID, before the root supervisor signals it, so the
// process is pinned with a pidfd first and its UID and tag are re-read only
// after that. While the pinned process lives, /proc/<pid> describes it; if it
// exits, the re-read can only describe a successor, and pidfd_send_signal
// then fails with ESRCH instead of reaching that successor. Without pidfds
// the process is never signaled by bare PID: it stays alive, so the next scan
// finds it and containment remains unproven.
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
