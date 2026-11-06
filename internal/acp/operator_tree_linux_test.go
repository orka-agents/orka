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
		status := "Name:\tsh\nPPid:\t1\nUid:\t" + uid + "\t" + uid + "\t" + uid + "\t" + uid + "\nGroups:\t" + groups + "\n"
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
