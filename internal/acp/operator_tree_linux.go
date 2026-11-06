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
}

func readProcCredentials(procRoot string) ([]procCredentials, bool) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, false
	}
	processes := make([]procCredentials, 0, len(entries))
	for _, entry := range entries {
		if !isDecimalPIDName(entry.Name()) {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(procRoot, entry.Name(), "status"))
		if err != nil {
			continue // exited between ReadDir and ReadFile
		}
		process := procCredentials{pid: pid, uid: -1}
		for line := range bytes.SplitSeq(data, []byte("\n")) {
			switch {
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
		processes = append(processes, process)
	}
	return processes, true
}

func taggedProcesses(procRoot string, uid, tag int) ([]int, bool) {
	processes, ok := readProcCredentials(procRoot)
	if !ok {
		return nil, false
	}
	var tagged []int
	for _, process := range processes {
		if uid >= 0 && process.uid != uid {
			continue
		}
		if slices.Contains(process.groups, tag) {
			tagged = append(tagged, process.pid)
		}
	}
	return tagged, true
}

// OperatorTagInUse reports whether any live process carries the tag group.
// An unreadable process table counts as in use.
func OperatorTagInUse(tag int) bool {
	tagged, ok := taggedProcesses("/proc", -1, tag)
	return !ok || len(tagged) > 0
}

// KillOperatorTag kills every process of uid carrying the operation's tag
// group until none remain, and reports whether that was proven. Because the
// tag cannot be dropped and is inherited, this reaches every descendant,
// including ones that changed process group, session, or parent.
func KillOperatorTag(uid, tag int) bool {
	if uid <= 0 || tag < OperatorTagFirstGID || tag > OperatorTagLastGID {
		return false
	}
	for range operatorTagKillRounds {
		tagged, ok := taggedProcesses("/proc", uid, tag)
		if !ok {
			return false
		}
		if len(tagged) == 0 {
			return true
		}
		for _, pid := range tagged {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
