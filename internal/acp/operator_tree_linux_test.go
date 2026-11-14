//go:build linux

package acp

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestTaggedProcessesMatchUIDAndSupplementaryTag(t *testing.T) {
	root := t.TempDir()
	write := func(pid, uid, groups string) {
		dir := filepath.Join(root, pid)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		status := "Name:\tsh\nState:\tS (sleeping)\nPPid:\t1\nUid:\t" + uid + "\t" + uid + "\t" + uid + "\t" + uid + "\nGroups:\t" + groups + "\n"
		if err := os.WriteFile(filepath.Join(dir, "status"), []byte(status), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("10", "20001", "61005")       // tagged operation process
	write("11", "20001", "")            // the agent: same UID, no tag
	write("12", "20002", "61005")       // another UID can never be killed
	write("13", "20001", "61006 61005") // multiple groups still match
	write("self", "0", "")
	// A terminated, unreaped process (for example the operation leader before
	// its parent's Wait) no longer counts.
	if err := os.MkdirAll(filepath.Join(root, "14"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "14", "status"),
		[]byte("Name:\tsh\nState:\tZ (zombie)\nUid:\t20001\t20001\t20001\t20001\nGroups:\t61005\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tagged, ok := taggedProcesses(root, 20001, 61005)
	if !ok {
		t.Fatal("scan failed")
	}
	slices.Sort(tagged)
	if !slices.Equal(tagged, []int{10, 13}) {
		t.Fatalf("tagged = %v", tagged)
	}
	if KillOperatorTag(20001, 100) || KillOperatorTag(0, OperatorTagFirstGID) {
		t.Fatal("out-of-range tags and root must be refused")
	}
}

func TestOperatorExecHelperInvocationCarriesTag(t *testing.T) {
	invocation, err := parseExecHelperInvocation([]string{operatorExecHelperProtocolVersion, "1", "20001", "20000", "61001", "/bin/sh", "-c", "true"})
	if err != nil || invocation.tagGID != 61001 || invocation.command != "/bin/sh" || !slices.Equal(invocation.args, []string{"-c", "true"}) {
		t.Fatalf("invocation = %+v, %v", invocation, err)
	}
	for _, tag := range []string{"0", "20000", "70000", "x"} {
		if _, err := parseExecHelperInvocation([]string{operatorExecHelperProtocolVersion, "1", "20001", "20000", tag, "/bin/sh"}); err == nil {
			t.Fatalf("tag %q must be refused", tag)
		}
	}
	if invocation, err := parseExecHelperInvocation([]string{execHelperProtocolVersion, "1", "20001", "20000", "/bin/sh"}); err != nil || invocation.tagGID != 0 {
		t.Fatalf("adapter invocation changed: %+v %v", invocation, err)
	}
}

// TestKillOperatorTagContainsEscapedDescendantsAsRoot proves containment end
// to end: a tagged command spawns a daemon in a new session and exits; the
// daemon is reparented away, yet the tag kill still finds and kills it while
// an untagged process of the same UID survives. Requires root (setgroups).
func TestKillOperatorTagContainsEscapedDescendantsAsRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to assign supplementary groups")
	}
	const uid, gid, tag = 23456, 23456, OperatorTagLastGID
	credential := func(groups []uint32) *syscall.SysProcAttr {
		return &syscall.SysProcAttr{Setpgid: true, Credential: &syscall.Credential{Uid: uid, Gid: gid, Groups: groups}}
	}
	bystander := exec.Command("/bin/sleep", "60")
	bystander.SysProcAttr = credential([]uint32{})
	if err := bystander.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bystander.Process.Kill(); _ = bystander.Wait() }()
	escaper := exec.Command("/bin/sh", "-c", "setsid /bin/sleep 60 </dev/null >/dev/null 2>&1 & echo $!")
	escaper.SysProcAttr = credential([]uint32{tag})
	out, err := escaper.Output()
	if err != nil {
		t.Fatal(err)
	}
	daemon, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatal(err)
	}
	if !OperatorTagInUse(tag) {
		t.Fatal("the escaped daemon must still carry the tag")
	}
	if !KillOperatorTag(uid, tag) {
		t.Fatal("tag kill was not proven")
	}
	if OperatorTagInUse(tag) {
		t.Fatalf("daemon %d escaped containment", daemon)
	}
	if err := syscall.Kill(bystander.Process.Pid, 0); err != nil {
		t.Fatal("an untagged process of the same UID was killed")
	}
}

// TestOperatorCommandRunsThroughExecHelperAsRoot launches a real operator
// command through the built exec helper (ORKA_TEST_EXEC_HELPER) and checks the
// child identity, empty-but-tag groups, and working directory.
func TestOperatorCommandRunsThroughExecHelperAsRoot(t *testing.T) {
	helper := os.Getenv("ORKA_TEST_EXEC_HELPER")
	if os.Geteuid() != 0 || helper == "" {
		t.Skip("requires root and ORKA_TEST_EXEC_HELPER")
	}
	dir := t.TempDir()
	if err := os.Chmod(filepath.Dir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(dir, 23457, 23457); err != nil {
		t.Fatal(err)
	}
	cmd, err := NewOperatorCommand(OperatorCommandConfig{
		ExecHelperCommand: helper, UID: 23457, GID: 23457, TagGID: OperatorTagLastGID - 1,
		Argv: []string{"/bin/sh", "-c", "id -u; id -g; id -G; pwd"}, Dir: dir, Environment: []string{"PATH=/usr/bin:/bin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("operator command failed: %v", err)
	}
	lines := strings.Fields(string(out))
	want := []string{"23457", "23457", "23457", strconv.Itoa(OperatorTagLastGID - 1), dir}
	if !slices.Equal(lines, want) {
		t.Fatalf("identity = %v, want %v", lines, want)
	}
}

func TestOperatorTerminalCommandRunsThroughExecHelperAsRoot(t *testing.T) {
	helper := os.Getenv("ORKA_TEST_EXEC_HELPER")
	if os.Geteuid() != 0 || helper == "" {
		t.Skip("requires root and ORKA_TEST_EXEC_HELPER")
	}
	master, slave, err := OpenOperatorPTY(23458, 23458, 24, 80)
	if err != nil {
		t.Skipf("pseudo-terminals unavailable: %v", err)
	}
	defer master.Close() //nolint:errcheck
	cmd, err := NewOperatorCommand(OperatorCommandConfig{
		ExecHelperCommand: helper, UID: 23458, GID: 23458, TagGID: OperatorTagLastGID - 2, TTY: slave,
		Argv: []string{"/bin/sh", "-c", "tty >/dev/null && stty size && id -u"}, Dir: "/", Environment: []string{"PATH=/usr/bin:/bin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = slave.Close()
	var output []byte
	buffer := make([]byte, 256)
	for {
		n, err := master.Read(buffer)
		output = append(output, buffer[:n]...)
		if err != nil {
			break
		}
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("terminal command failed: %v (%q)", err, output)
	}
	if got := strings.Fields(string(output)); !slices.Equal(got, []string{"24", "80", "23458"}) {
		t.Fatalf("terminal output = %q", output)
	}
}

// TestKillOperatorTagProvesContainmentBeforeReapAsRoot covers the cp path:
// the helper exits on its own, and containment must be proven while its
// zombie still awaits Wait.
func TestKillOperatorTagProvesContainmentBeforeReapAsRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to assign supplementary groups")
	}
	const uid, tag = 23459, OperatorTagLastGID - 3
	cmd := exec.Command("/bin/true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Credential: &syscall.Credential{Uid: uid, Gid: uid, Groups: []uint32{tag}}}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Wait() }()
	time.Sleep(200 * time.Millisecond) // exited, not yet reaped
	if !KillOperatorTag(uid, tag) {
		t.Fatal("an exited, unreaped operation must count as contained")
	}
}

// Only exited processes of the operation's UID that carry its tag are reaped;
// the agent's processes, other UIDs, live processes, and held operator
// leaders are left alone.
func TestReapOperatorTagReapsOnlyTheOperationsZombies(t *testing.T) {
	root := t.TempDir()
	write := func(pid, state, uid, groups string) {
		dir := filepath.Join(root, pid)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		status := "Name:\tsh\nState:\t" + state + "\nUid:\t" + uid + "\t" + uid + "\t" + uid + "\t" + uid + "\nGroups:\t" + groups + "\n"
		if err := os.WriteFile(filepath.Join(dir, "status"), []byte(status), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tag := OperatorTagFirstGID + 5
	tagged := strconv.Itoa(tag)
	write("40", "Z (zombie)", "20001", tagged)                              // orphaned descendant: reaped
	write("41", "Z (zombie)", "20001", strconv.Itoa(tag+1)+" "+tagged)      // multiple groups: reaped
	write("42", "Z (zombie)", "20001", "")                                  // the agent's orphan
	write("43", "Z (zombie)", "20002", tagged)                              // another UID
	write("44", "S (sleeping)", "20001", tagged)                            // still live
	write("45", "Z (zombie)", "20001", tagged)                              // a held operator leader
	write("46", "Z (zombie)", "20001", strconv.Itoa(OperatorTagFirstGID+6)) // another operation
	held := &operatorLeaderRegistration{pid: 45}
	operatorLeaders.mu.Lock()
	operatorLeaders.pids[45] = held
	operatorLeaders.mu.Unlock()
	t.Cleanup(func() { releaseOperatorLeader(held) })
	var reaped []int
	reapOperatorTag(root, 20001, tag, func(pid int) { reaped = append(reaped, pid) })
	slices.Sort(reaped)
	if !slices.Equal(reaped, []int{40, 41}) {
		t.Fatalf("reaped = %v, want [40 41]", reaped)
	}
	reaped = nil
	reapOperatorTag(root, 0, tag, func(pid int) { reaped = append(reaped, pid) })
	reapOperatorTag(root, 20001, 100, func(pid int) { reaped = append(reaped, pid) })
	if len(reaped) != 0 {
		t.Fatalf("root or an out-of-range tag reaped %v", reaped)
	}
}

func TestTaggedProcessesCountZombieLeadersWithLiveThreads(t *testing.T) {
	root := t.TempDir()
	write := func(path, state string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		status := "Name:\tworker\nState:\t" + state + "\nUid:\t20001\t20001\t20001\t20001\nGroups:\t61007\n"
		if err := os.WriteFile(path, []byte(status), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Leader 20 called pthread_exit; thread 21 still runs.
	write(filepath.Join(root, "20", "status"), "Z (zombie)")
	write(filepath.Join(root, "20", "task", "20", "status"), "Z (zombie)")
	write(filepath.Join(root, "20", "task", "21", "status"), "S (sleeping)")
	// Leader 30 and all its threads have exited.
	write(filepath.Join(root, "30", "status"), "Z (zombie)")
	write(filepath.Join(root, "30", "task", "30", "status"), "Z (zombie)")
	tagged, ok := taggedProcesses(root, 20001, 61007)
	if !ok || !slices.Equal(tagged, []int{20}) {
		t.Fatalf("tagged = %v ok=%v; a zombie leader with live threads must count", tagged, ok)
	}
}

// TestKillOperatorTagStopsForkAndExitChainAsRoot runs a descendant chain in
// which every process forks its successor and exits immediately.
func TestKillOperatorTagStopsForkAndExitChainAsRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to assign supplementary groups")
	}
	const uid, tag = 23460, OperatorTagLastGID - 4
	cmd := exec.Command("/bin/sh", "-c", "c() { c & }; c")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Credential: &syscall.Credential{Uid: uid, Gid: uid, Groups: []uint32{tag}}}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Wait() }()
	time.Sleep(200 * time.Millisecond)
	if !KillOperatorTag(uid, tag) {
		t.Fatal("containment of the chain was not proven")
	}
	time.Sleep(500 * time.Millisecond)
	if OperatorTagInUse(tag) {
		_ = KillOperatorTag(uid, tag)
		t.Fatal("a fork-and-exit chain survived proven containment")
	}
}

// An operator process whose state cannot be read completely is never presumed
// gone: the scan reports unsettled and containment stays unproven.
func TestOperatorTagScanFailsClosedOnUninspectableProcesses(t *testing.T) {
	const tag = 61009
	write := func(t *testing.T, root, pid, status string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(root, pid), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, pid, "status"), []byte(status), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	clean := "Name:\tsh\nState:\tS (sleeping)\nUid:\t20001\t20001\t20001\t20001\nGroups:\t\n"
	cases := map[string]func(t *testing.T, root string){
		"unreadable status": func(t *testing.T, root string) {
			// A directory in place of the status file fails the read with
			// EISDIR, standing in for EACCES or EIO.
			if err := os.MkdirAll(filepath.Join(root, "21", "status"), 0o755); err != nil {
				t.Fatal(err)
			}
		},
		"status without credentials": func(t *testing.T, root string) {
			write(t, root, "21", "Name:\tsh\nState:\tS (sleeping)\n")
		},
		"malformed uid": func(t *testing.T, root string) {
			write(t, root, "21", "Name:\tsh\nState:\tS (sleeping)\nUid:\tx\nGroups:\t61009\n")
		},
		"malformed groups": func(t *testing.T, root string) {
			write(t, root, "21", "Name:\tsh\nState:\tS (sleeping)\nUid:\t20001\t20001\t20001\t20001\nGroups:\t6100x\n")
		},
		"zombie leader whose threads cannot be listed": func(t *testing.T, root string) {
			write(t, root, "21", "Name:\tsh\nState:\tZ (zombie)\nUid:\t20001\t20001\t20001\t20001\nGroups:\t61009\n")
			// A regular file in place of the task directory fails ReadDir with
			// ENOTDIR, not ENOENT.
			if err := os.WriteFile(filepath.Join(root, "21", "task"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"zombie leader with an unreadable thread": func(t *testing.T, root string) {
			write(t, root, "21", "Name:\tsh\nState:\tZ (zombie)\nUid:\t20001\t20001\t20001\t20001\nGroups:\t61009\n")
			if err := os.MkdirAll(filepath.Join(root, "21", "task", "22", "status"), 0o755); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			write(t, root, "20", clean)
			setup(t, root)
			if _, ok := taggedProcesses(root, 20001, tag); ok {
				t.Fatal("a scan with an uninspectable process must not settle")
			}
			if killOperatorTag(root, 20001, tag, 4, time.Millisecond) {
				t.Fatal("containment was proven over an uninspectable process")
			}
		})
	}
	t.Run("exited processes still settle", func(t *testing.T) {
		root := t.TempDir()
		write(t, root, "20", clean)
		// A listed PID whose directory vanished exited before inspection.
		if err := os.MkdirAll(filepath.Join(root, "23"), 0o755); err != nil {
			t.Fatal(err)
		}
		if !killOperatorTag(root, 20001, tag, 4, time.Millisecond) {
			t.Fatal("an exited process must not block containment")
		}
	})
}

// The supervisor never kills a tagged process of another UID, but while one
// is live it cannot claim the operation is contained.
func TestOperatorTagContainmentRefusesATaggedProcessOfAnotherUID(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "30"), 0o755); err != nil {
		t.Fatal(err)
	}
	status := "Name:\tsh\nState:\tS (sleeping)\nUid:\t0\t0\t0\t0\nGroups:\t61010\n"
	if err := os.WriteFile(filepath.Join(root, "30", "status"), []byte(status), 0o644); err != nil {
		t.Fatal(err)
	}
	if tagged, ok := taggedProcesses(root, 20001, 61010); !ok || len(tagged) != 0 {
		t.Fatalf("another UID must never be selected for killing: %v %v", tagged, ok)
	}
	if killOperatorTag(root, 20001, 61010, 4, time.Millisecond) {
		t.Fatal("containment was proven while a tagged process of another UID is live")
	}
}

// startOperatorKillVictim starts a real process standing in for an unrelated
// process that reused a scanned operator PID, and lists it in a fake /proc as
// a tagged operator process.
func startOperatorKillVictim(t *testing.T, procRoot string, uid, tag int) *exec.Cmd {
	t.Helper()
	victim := exec.Command("sleep", "30")
	if err := victim.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = victim.Process.Kill()
		_, _ = victim.Process.Wait()
	})
	writeFakeOperatorStatus(t, procRoot, victim.Process.Pid, uid, strconv.Itoa(tag))
	return victim
}

func writeFakeOperatorStatus(t *testing.T, procRoot string, pid, uid int, groups string) {
	t.Helper()
	dir := filepath.Join(procRoot, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	id := strconv.Itoa(uid)
	status := "Name:\tsleep\nState:\tS (sleeping)\nUid:\t" + id + "\t" + id + "\t" + id + "\t" + id + "\nGroups:\t" + groups + "\n"
	if err := os.WriteFile(filepath.Join(dir, "status"), []byte(status), 0o644); err != nil {
		t.Fatal(err)
	}
}

// requireOperatorVictimAlive checks the real process state: a killed child
// stays a zombie until reaped, and signal 0 still succeeds on a zombie.
func requireOperatorVictimAlive(t *testing.T, victim *exec.Cmd) {
	t.Helper()
	time.Sleep(20 * time.Millisecond)
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(victim.Process.Pid), "status"))
	if err != nil || parseProcCredentials(victim.Process.Pid, data).zombie {
		t.Fatalf("an unrelated process was killed: %v", err)
	}
}

// A scanned operator PID can exit and be reused by an unrelated process
// before the root supervisor signals it. The process must be re-verified
// after it is pinned, and an unrelated successor never signaled.
func TestKillOperatorTagNeverSignalsAProcessThatReusedAScannedPID(t *testing.T) {
	const uid, tag = 20001, 61011
	procRoot := t.TempDir()
	victim := startOperatorKillVictim(t, procRoot, uid, tag)
	t.Cleanup(func() { operatorTagScannedHook = nil })
	operatorTagScannedHook = func() {
		// The tagged process exited; its PID now belongs to an unrelated
		// process of another UID without the tag.
		writeFakeOperatorStatus(t, procRoot, victim.Process.Pid, 0, "")
	}
	killOperatorTag(procRoot, uid, tag, 3, time.Millisecond)
	operatorTagScannedHook = nil
	requireOperatorVictimAlive(t, victim)
}

// Without pidfds a process is never signaled by bare PID; containment stays
// unproven instead.
func TestKillOperatorTagRefusesToSignalWithoutPidfds(t *testing.T) {
	const uid, tag = 20001, 61012
	procRoot := t.TempDir()
	victim := startOperatorKillVictim(t, procRoot, uid, tag)
	previous := pidfdOpen
	t.Cleanup(func() { pidfdOpen = previous })
	pidfdOpen = func(int, int) (int, error) { return -1, syscall.ENOSYS }
	if killOperatorTag(procRoot, uid, tag, 3, time.Millisecond) {
		t.Fatal("containment was proven without signaling a live tagged process")
	}
	requireOperatorVictimAlive(t, victim)
}

// A process that still carries the tag after it is pinned is killed through
// its pidfd.
func TestKillOperatorTagSignalsAVerifiedProcessThroughItsPidfd(t *testing.T) {
	const uid, tag = 20001, 61013
	procRoot := t.TempDir()
	victim := startOperatorKillVictim(t, procRoot, uid, tag)
	exited := make(chan struct{})
	go func() {
		_, _ = victim.Process.Wait()
		// Once reaped, the fake process table no longer lists it.
		_ = os.RemoveAll(filepath.Join(procRoot, strconv.Itoa(victim.Process.Pid)))
		close(exited)
	}()
	if !killOperatorTag(procRoot, uid, tag, 200, 5*time.Millisecond) {
		t.Fatal("a verified tagged process was not contained")
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the verified process was not killed")
	}
}
