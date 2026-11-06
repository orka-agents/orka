//go:build linux

package acp

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// OperatorShimSubcommand is the hidden orka-acp-runtime subcommand that runs
// one operator command as a child subreaper. Every descendant, including one
// that changes its process group or session, stays in the shim's process tree
// while the shim lives, and the shim kills and reaps all of them before it
// exits. The supervisor is itself a subreaper shared with the ACP adapter, so
// without the shim an escaped operator descendant would be indistinguishable
// from adapter processes.
const OperatorShimSubcommand = "__orka_operator_exec"

const operatorTreeKillRounds = 100

// RunOperatorShim runs argv (argv[0] absolute) and returns its exit status.
func RunOperatorShim(argv []string) int {
	if len(argv) == 0 || !filepath.IsAbs(argv[0]) {
		_, _ = fmt.Fprintln(os.Stderr, "operator shim: an absolute command is required")
		return 126
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "operator shim: subreaper is unavailable")
		return 126
	}
	// Terminal and group signals reach the command directly (it shares the
	// shim's process group). Handled, not ignored, so the command does not
	// inherit SIG_IGN across exec.
	interactive := make(chan os.Signal, 8)
	signal.Notify(interactive, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTSTP, syscall.SIGTTIN, syscall.SIGTTOU)
	terminate := make(chan os.Signal, 2)
	signal.Notify(terminate, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		for signal := range interactive {
			_ = signal
		}
	}()

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "operator shim: %v\n", err)
		return 127
	}
	done := make(chan struct{})
	go func() {
		<-terminate
		killOperatorChildren(os.Getpid())
		close(done)
	}()
	err := cmd.Wait()
	killOperatorChildren(os.Getpid())
	select {
	case <-done:
		return 143
	default:
	}
	if err == nil {
		return 0
	}
	if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	if code := cmd.ProcessState.ExitCode(); code >= 0 {
		return code
	}
	return 126
}

// killOperatorChildren kills every child of the subreaper until none remain.
// Orphaned descendants are reparented to the subreaper, so repeated rounds
// reach the whole tree.
func killOperatorChildren(self int) {
	for range operatorTreeKillRounds {
		children := processChildren("/proc", self, -1)
		for _, pid := range children {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		reaped := false
		for {
			pid, err := syscall.Wait4(-1, nil, syscall.WNOHANG, nil)
			if pid <= 0 || err != nil {
				break
			}
			reaped = true
		}
		if len(children) == 0 && !reaped {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// KillOperatorTree kills every descendant of leader that runs as uid, then
// the leader itself. While the leader (a subreaper shim) lives, descendants
// cannot leave its tree, so repeated rounds are complete.
func KillOperatorTree(leader, uid int) {
	if leader <= 0 {
		return
	}
	for range operatorTreeKillRounds {
		descendants := processDescendants("/proc", leader, uid)
		if len(descendants) == 0 {
			break
		}
		for _, pid := range descendants {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = syscall.Kill(-leader, syscall.SIGKILL)
	_ = syscall.Kill(leader, syscall.SIGKILL)
}

type procParent struct {
	pid  int
	ppid int
	uid  int
}

func readProcParents(procRoot string) []procParent {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil
	}
	parents := make([]procParent, 0, len(entries))
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
			continue
		}
		ppid, uid := -1, -1
		for line := range bytes.SplitSeq(data, []byte("\n")) {
			switch {
			case bytes.HasPrefix(line, []byte("PPid:")):
				ppid, _ = strconv.Atoi(string(bytes.TrimSpace(line[len("PPid:"):])))
			case bytes.HasPrefix(line, []byte("Uid:")):
				if fields := bytes.Fields(line[len("Uid:"):]); len(fields) > 0 {
					uid, _ = strconv.Atoi(string(fields[0]))
				}
			}
		}
		if ppid >= 0 {
			parents = append(parents, procParent{pid: pid, ppid: ppid, uid: uid})
		}
	}
	return parents
}

// processChildren returns direct children of parent (any uid when uid < 0).
func processChildren(procRoot string, parent, uid int) []int {
	var children []int
	for _, process := range readProcParents(procRoot) {
		if process.ppid == parent && (uid < 0 || process.uid == uid) {
			children = append(children, process.pid)
		}
	}
	return children
}

// processDescendants returns the transitive descendants of root running as uid.
func processDescendants(procRoot string, root, uid int) []int {
	byParent := map[int][]procParent{}
	for _, process := range readProcParents(procRoot) {
		byParent[process.ppid] = append(byParent[process.ppid], process)
	}
	var descendants []int
	queue := []int{root}
	seen := map[int]bool{root: true}
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		for _, child := range byParent[next] {
			if seen[child.pid] || child.uid != uid {
				continue
			}
			seen[child.pid] = true
			descendants = append(descendants, child.pid)
			queue = append(queue, child.pid)
		}
	}
	return descendants
}
