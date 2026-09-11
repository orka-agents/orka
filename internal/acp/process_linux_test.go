//go:build linux

package acp

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

var testExecHelperState struct {
	once sync.Once
	dir  string
	path string
	err  error
}

func testExecHelperCommand(t *testing.T) string {
	t.Helper()
	if os.Geteuid() != 0 {
		groups, err := os.Getgroups()
		if err != nil {
			t.Fatal(err)
		}
		if len(groups) != 0 {
			t.Skipf("non-root test environment cannot clear supplementary groups %v without weakening the production fence", groups)
		}
	}
	testExecHelperState.once.Do(func() {
		testExecHelperState.dir, testExecHelperState.err = os.MkdirTemp("", "orka-acp-exec-helper-test-")
		if testExecHelperState.err != nil {
			return
		}
		if testExecHelperState.err = os.Chmod(testExecHelperState.dir, 0o755); testExecHelperState.err != nil {
			return
		}
		testExecHelperState.path = filepath.Join(testExecHelperState.dir, "orka-acp-exec-helper")
		_, sourceFile, _, ok := runtime.Caller(0)
		if !ok {
			testExecHelperState.err = fmt.Errorf("resolve process test source path")
			return
		}
		repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", ".."))
		command := exec.Command("go", "build", "-buildvcs=false", "-trimpath", "-o", testExecHelperState.path, "./cmd/orka-acp-exec-helper")
		command.Dir = repositoryRoot
		command.Env = append(os.Environ(), "CGO_ENABLED=0")
		output, err := command.CombinedOutput()
		if err != nil {
			testExecHelperState.err = fmt.Errorf("build ACP exec helper: %w: %s", err, strings.TrimSpace(string(output)))
			return
		}
		testExecHelperState.err = os.Chmod(testExecHelperState.path, 0o755)
	})
	if testExecHelperState.err != nil {
		t.Fatal(testExecHelperState.err)
	}
	return testExecHelperState.path
}

func cleanupTestExecHelper() {
	if testExecHelperState.dir != "" {
		_ = os.RemoveAll(testExecHelperState.dir)
	}
}

func TestStartProcessAppliesLimitsBeforeAdapterExecutes(t *testing.T) {
	adapter := copyTestExecutableForChild(t)
	uid, gid := os.Getuid(), os.Getgid()
	if uid == 0 {
		uid, gid = 65534, 65534
	}
	process, err := StartProcess(ProcessConfig{
		Command: adapter,
		Args: []string{
			"-test.run=^TestACPProcessLimitReporter$",
		},
		Environment: []string{
			"GO_WANT_ACP_LIMIT_REPORTER=1",
			"ACP_EXPECT_UID=" + strconv.Itoa(uid),
			"ACP_EXPECT_GID=" + strconv.Itoa(gid),
		},
		Paths:             SessionPaths{Workspace: "/tmp"},
		UID:               uid,
		GID:               gid,
		ExecHelperCommand: testExecHelperCommand(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := process.Wait(ctx); err != nil {
		t.Fatalf("adapter wait: %v; diagnostics: %s", err, process.Diagnostics())
	}
	if !strings.Contains(process.Diagnostics(), "limits-applied-before-adapter") {
		t.Fatalf("missing pre-exec limit marker in diagnostics: %q", process.Diagnostics())
	}
}

func TestACPProcessLimitReporter(t *testing.T) {
	if os.Getenv("GO_WANT_ACP_LIMIT_REPORTER") != "1" {
		return
	}
	wantUID, err := strconv.Atoi(os.Getenv("ACP_EXPECT_UID"))
	if err != nil {
		t.Fatal(err)
	}
	wantGID, err := strconv.Atoi(os.Getenv("ACP_EXPECT_GID"))
	if err != nil {
		t.Fatal(err)
	}
	if os.Getuid() != wantUID || os.Geteuid() != wantUID {
		t.Fatalf("adapter UID = %d/%d, want %d", os.Getuid(), os.Geteuid(), wantUID)
	}
	if os.Getgid() != wantGID || os.Getegid() != wantGID {
		t.Fatalf("adapter GID = %d/%d, want %d", os.Getgid(), os.Getegid(), wantGID)
	}
	groups, err := os.Getgroups()
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 0 {
		t.Fatalf("adapter supplementary groups = %v, want none", groups)
	}
	if unix.Getpgrp() != os.Getpid() {
		t.Fatalf("adapter process group = %d, want PID %d", unix.Getpgrp(), os.Getpid())
	}
	for _, configured := range childResourceLimits() {
		var limit unix.Rlimit
		if err := unix.Getrlimit(configured.resource, &limit); err != nil {
			t.Fatal(err)
		}
		if limit.Cur > configured.maximum || limit.Max > configured.maximum || limit.Cur != limit.Max {
			t.Fatalf("resource %d limit = %#v, maximum %d", configured.resource, limit, configured.maximum)
		}
	}
	fmt.Fprintln(os.Stderr, "limits-applied-before-adapter")
}

func TestParseExecHelperInvocationAllowsPIDOneParent(t *testing.T) {
	invocation, err := parseExecHelperInvocation([]string{
		execHelperProtocolVersion, "1", "1000", "1000", "/bin/true", "argument",
	})
	if err != nil {
		t.Fatal(err)
	}
	if invocation.parentPID != 1 || invocation.command != "/bin/true" || len(invocation.args) != 1 {
		t.Fatalf("invocation = %#v", invocation)
	}
}

func TestParseExecHelperInvocationRejectsBypasses(t *testing.T) {
	valid := []string{execHelperProtocolVersion, "100", "1000", "1000", "/bin/true"}
	tests := []struct {
		name string
		args []string
	}{
		{name: "wrong protocol", args: append([]string{"other"}, valid[1:]...)},
		{name: "missing parent", args: valid[:1]},
		{name: "root uid", args: []string{execHelperProtocolVersion, "100", "0", "1000", "/bin/true"}},
		{name: "root gid", args: []string{execHelperProtocolVersion, "100", "1000", "0", "/bin/true"}},
		{name: "relative command", args: []string{execHelperProtocolVersion, "100", "1000", "1000", "bin/true"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseExecHelperInvocation(test.args); err == nil {
				t.Fatal("invalid helper invocation unexpectedly accepted")
			}
		})
	}
}

func TestInspectProcessesForUIDFailsClosedOnStatusErrors(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*testing.T, string)
	}{
		{
			name: "read error",
			prepare: func(t *testing.T, root string) {
				t.Helper()
				path := filepath.Join(root, "101", "status")
				if err := os.MkdirAll(path, 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "numeric pid overflow",
			prepare: func(t *testing.T, root string) {
				t.Helper()
				if err := os.Mkdir(filepath.Join(root, "999999999999999999999999999999"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "malformed uid",
			prepare: func(t *testing.T, root string) {
				t.Helper()
				writeFakeProcStatus(t, root, 101, "State:\tS (sleeping)\nUid:\tnot-a-uid 1000 1000 1000\n")
			},
		},
		{
			name: "missing uid",
			prepare: func(t *testing.T, root string) {
				t.Helper()
				writeFakeProcStatus(t, root, 101, "State:\tS (sleeping)\n")
			},
		},
		{
			name: "malformed state",
			prepare: func(t *testing.T, root string) {
				t.Helper()
				writeFakeProcStatus(t, root, 101, "State:\trunning\nUid:\t1000 1000 1000 1000\n")
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			test.prepare(t, root)
			if processes, supported := inspectProcessesForUIDAt(root, 1000, -1); supported {
				t.Fatalf("inventory unexpectedly proven: %#v", processes)
			}
		})
	}
}

func TestInspectProcessesForUIDAllowsOnlyConfirmedENOENTRaces(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "101"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFakeProcStatus(t, root, 300, "State:\tS (sleeping)\nUid:\t2000 2000 2000 2000\n")
	writeFakeProcStatus(t, root, 200, "State:\tR (running)\nUid:\t1000 1000 1000 1000\n")
	writeFakeProcStatus(t, root, 150, "State:\tT (stopped)\nUid:\t1000 1000 1000 1000\n")

	processes, supported := inspectProcessesForUIDAt(root, 1000, -1)
	if !supported {
		t.Fatal("inventory unexpectedly unproven")
	}
	if len(processes) != 2 || processes[0].pid != 150 || processes[1].pid != 200 {
		t.Fatalf("processes = %#v", processes)
	}
}

func TestSessionProcessStateProofRequiresLiveLeader(t *testing.T) {
	const leaderPID = 10
	tests := []struct {
		name        string
		processes   []procProcess
		wantStopped bool
		wantResumed bool
	}{
		{name: "empty"},
		{name: "only zombie leader", processes: []procProcess{{pid: leaderPID, state: 'Z'}}},
		{name: "dead leader", processes: []procProcess{{pid: leaderPID, state: 'X'}}},
		{name: "lowercase dead leader", processes: []procProcess{{pid: leaderPID, state: 'x'}}},
		{name: "unknown leader state", processes: []procProcess{{pid: leaderPID, state: '?'}}},
		{name: "stopped leader", processes: []procProcess{{pid: leaderPID, state: 'T'}}, wantStopped: true},
		{name: "traced stopped leader", processes: []procProcess{{pid: leaderPID, state: 't'}}, wantStopped: true},
		{name: "sleeping leader", processes: []procProcess{{pid: leaderPID, state: 'S'}}, wantResumed: true},
		{name: "running leader", processes: []procProcess{{pid: leaderPID, state: 'R'}}, wantResumed: true},
		{name: "stopped leader with zombie", processes: []procProcess{{pid: leaderPID, state: 'T'}, {pid: 20, state: 'Z'}}, wantStopped: true},
		{name: "resumed leader with zombie", processes: []procProcess{{pid: leaderPID, state: 'S'}, {pid: 20, state: 'Z'}}, wantResumed: true},
		{name: "resumed leader with dead descendant", processes: []procProcess{{pid: leaderPID, state: 'S'}, {pid: 20, state: 'X'}}, wantResumed: true},
		{name: "resumed leader with unknown descendant", processes: []procProcess{{pid: leaderPID, state: 'S'}, {pid: 20, state: '?'}}},
		{name: "stopped descendant without leader", processes: []procProcess{{pid: 20, state: 'T'}}},
		{name: "resumed descendant without leader", processes: []procProcess{{pid: 20, state: 'S'}}},
		{name: "stopped leader with running descendant", processes: []procProcess{{pid: leaderPID, state: 'T'}, {pid: 20, state: 'S'}}},
		{name: "resumed leader with stopped descendant", processes: []procProcess{{pid: leaderPID, state: 'S'}, {pid: 20, state: 'T'}}},
		{name: "zombie leader with stopped descendant", processes: []procProcess{{pid: leaderPID, state: 'Z'}, {pid: 20, state: 'T'}}},
		{name: "zombie leader with resumed descendant", processes: []procProcess{{pid: leaderPID, state: 'Z'}, {pid: 20, state: 'S'}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := sessionProcessesStopped(test.processes, leaderPID); got != test.wantStopped {
				t.Fatalf("sessionProcessesStopped(%#v, %d) = %v, want %v", test.processes, leaderPID, got, test.wantStopped)
			}
			if got := sessionProcessesResumed(test.processes, leaderPID); got != test.wantResumed {
				t.Fatalf("sessionProcessesResumed(%#v, %d) = %v, want %v", test.processes, leaderPID, got, test.wantResumed)
			}
		})
	}
}

func TestStopEscalatesDetachedDescendantAfterLeaderExit(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires the production root supervisor identity boundary")
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	uid, gid := 65534, 65534
	workspace, err := os.MkdirTemp("", "orka-acp-detached-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workspace) })
	if err := os.Chown(workspace, uid, gid); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(workspace, "descendant.pid")
	process, err := StartProcess(ProcessConfig{
		Command: testAdapterCommand(t),
		Args:    []string{"-test.run=^TestACPDetachedDescendantProcess$"},
		Environment: []string{
			"GO_WANT_ACP_DETACHED_DESCENDANT=leader",
			"ACP_DETACHED_DESCENDANT_MARKER=" + markerPath,
		},
		Paths:             SessionPaths{Workspace: workspace},
		UID:               uid,
		GID:               gid,
		ExecHelperCommand: testExecHelperCommand(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer waitCancel()
	if err := process.Wait(waitCtx); err != nil {
		t.Fatalf("adapter leader wait: %v; diagnostics: %s", err, process.Diagnostics())
	}
	descendantPID := readPIDMarker(t, markerPath)
	if err := syscall.Kill(descendantPID, 0); err != nil {
		t.Fatalf("detached descendant was not alive before Stop: %v", err)
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	status, err := process.Stop(stopCtx, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("Stop: %#v, %v", status, err)
	}
	if !status.Proven || len(status.RemainingPIDs) != 0 {
		t.Fatalf("cleanup status = %#v", status)
	}
	if err := syscall.Kill(descendantPID, 0); err == nil || err != syscall.ESRCH {
		t.Fatalf("detached descendant still exists: %v", err)
	}
}

//nolint:gocyclo // This test intentionally exercises the full freeze/thaw lifecycle and proof checks.
func TestFreezeAndThawIncludeDetachedDescendant(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires the production root supervisor identity boundary")
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	uid, gid := 65534, 65534
	workspace, err := os.MkdirTemp("", "orka-acp-freeze-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workspace) })
	if err := os.Chown(workspace, uid, gid); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(workspace, "descendant.pid")
	process, err := StartProcess(ProcessConfig{
		Command: testAdapterCommand(t),
		Args:    []string{"-test.run=^TestACPDetachedDescendantProcess$"},
		Environment: []string{
			"GO_WANT_ACP_DETACHED_DESCENDANT=leader-wait",
			"ACP_DETACHED_DESCENDANT_MARKER=" + markerPath,
		},
		Paths:             SessionPaths{Workspace: workspace},
		UID:               uid,
		GID:               gid,
		ExecHelperCommand: testExecHelperCommand(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = process.Thaw()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = process.Stop(ctx, 100*time.Millisecond)
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(markerPath); err == nil {
			break
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("detached descendant did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	descendantPID := readPIDMarker(t, markerPath)
	freezeCtx, freezeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer freezeCancel()
	if err := process.Freeze(freezeCtx); err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	if stopped, supported, err := processesStoppedForUID(uid, process.PID()); err != nil || !supported || !stopped {
		t.Fatalf("UID-scoped freeze proof = stopped:%v supported:%v err:%v", stopped, supported, err)
	}
	if err := process.Thaw(); err != nil {
		t.Fatalf("Thaw: %v", err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		processes, supported := inspectProcessesForUIDAt("/proc", uid, os.Getpid())
		resumed := supported && sessionProcessesResumed(processes, process.PID())
		descendantResumed := false
		for _, candidate := range processes {
			if candidate.pid == descendantPID && candidate.state != 'Z' && candidate.state != 'T' && candidate.state != 't' {
				descendantResumed = true
				break
			}
		}
		if resumed && descendantResumed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session UID processes did not resume: %#v", processes)
		}
		time.Sleep(10 * time.Millisecond)
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	status, err := process.Stop(stopCtx, 100*time.Millisecond)
	if err != nil || !status.Proven || len(status.RemainingPIDs) != 0 {
		t.Fatalf("Stop after thaw: status=%#v err=%v", status, err)
	}
	if err := syscall.Kill(descendantPID, 0); err == nil || err != syscall.ESRCH {
		t.Fatalf("detached descendant still exists: %v", err)
	}
}

func TestFreezeFailsAfterLeaderExitWithDetachedDescendant(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires the production root supervisor identity boundary")
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	uid, gid := 65534, 65534
	workspace, err := os.MkdirTemp("", "orka-acp-freeze-exit-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workspace) })
	if err := os.Chown(workspace, uid, gid); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(workspace, "descendant.pid")
	process, err := StartProcess(ProcessConfig{
		Command: testAdapterCommand(t),
		Args:    []string{"-test.run=^TestACPDetachedDescendantProcess$"},
		Environment: []string{
			"GO_WANT_ACP_DETACHED_DESCENDANT=leader",
			"ACP_DETACHED_DESCENDANT_MARKER=" + markerPath,
		},
		Paths:             SessionPaths{Workspace: workspace},
		UID:               uid,
		GID:               gid,
		ExecHelperCommand: testExecHelperCommand(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		status, stopErr := process.Stop(ctx, 100*time.Millisecond)
		if stopErr != nil || !status.Proven || len(status.RemainingPIDs) != 0 {
			t.Errorf("cleanup after leader exit: status=%#v err=%v", status, stopErr)
		}
	})

	select {
	case <-process.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("adapter leader did not exit")
	}
	descendantPID := readPIDMarker(t, markerPath)
	if err := syscall.Kill(descendantPID, 0); err != nil {
		t.Fatalf("detached descendant is not live: %v", err)
	}
	freezeCtx, freezeCancel := context.WithTimeout(context.Background(), time.Second)
	defer freezeCancel()
	if err := process.Freeze(freezeCtx); err == nil || !strings.Contains(err.Error(), "exited while freezing") {
		t.Fatalf("Freeze after leader exit = %v, want exited error", err)
	}
}

//nolint:gocyclo // This test intentionally keeps the hostile descendant lifecycle in one auditable flow.
func TestThawRejectsStoppedDetachedDescendantWithoutUIDScope(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires the supported non-root supervisor path")
	}
	if os.Getuid() <= 0 || os.Getgid() <= 0 {
		t.Skip("requires a non-root UID and GID")
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	uid, gid := os.Getuid(), os.Getgid()
	existing, supported := inspectProcessesForUIDAt("/proc", uid, os.Getpid())
	if !supported {
		t.Fatal("process inventory unexpectedly unsupported")
	}
	if len(existing) != 0 {
		t.Skipf("requires an isolated non-root UID namespace; found processes: %#v", existing)
	}
	workspace := t.TempDir()
	if err := os.Chmod(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(workspace, "descendant.pid")
	process, err := StartProcess(ProcessConfig{
		Command: testAdapterCommand(t),
		Args:    []string{"-test.run=^TestACPDetachedDescendantProcess$"},
		Environment: []string{
			"GO_WANT_ACP_DETACHED_DESCENDANT=leader-wait",
			"ACP_DETACHED_DESCENDANT_MARKER=" + markerPath,
			"ACP_DETACHED_DESCENDANT_SELF_STOP=1",
		},
		Paths:             SessionPaths{Workspace: workspace},
		UID:               uid,
		GID:               gid,
		ExecHelperCommand: testExecHelperCommand(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	descendantPID := 0
	t.Cleanup(func() {
		if descendantPID > 0 {
			_ = syscall.Kill(descendantPID, syscall.SIGCONT)
			_ = syscall.Kill(descendantPID, syscall.SIGKILL)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		status, stopErr := process.Stop(ctx, 100*time.Millisecond)
		if stopErr != nil || !status.Proven || len(status.RemainingPIDs) != 0 {
			t.Errorf("cleanup after rejected thaw: status=%#v err=%v", status, stopErr)
		}
	})

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(markerPath); err == nil {
			break
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		select {
		case <-process.Done():
			t.Fatalf("adapter exited before detached descendant became ready: %s", process.Diagnostics())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("detached descendant did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	descendantPID = readPIDMarker(t, markerPath)
	deadline = time.Now().Add(5 * time.Second)
	for {
		processes, supported := inspectProcessesForUIDAt("/proc", uid, os.Getpid())
		stopped := false
		for _, candidate := range processes {
			if candidate.pid == descendantPID && (candidate.state == 'T' || candidate.state == 't') {
				stopped = true
				break
			}
		}
		if supported && stopped {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("detached descendant did not stop: %#v", processes)
		}
		time.Sleep(10 * time.Millisecond)
	}

	freezeCtx, freezeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer freezeCancel()
	if err := process.Freeze(freezeCtx); err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	if err := process.Thaw(); err == nil || !strings.Contains(err.Error(), "thaw could not be proven") {
		t.Fatalf("Thaw with stopped detached descendant = %v, want proof error", err)
	}
	processes, supported := inspectProcessesForUIDAt("/proc", uid, os.Getpid())
	if !supported {
		t.Fatal("process inventory unexpectedly unsupported")
	}
	descendantFound := false
	for _, candidate := range processes {
		if candidate.pid != descendantPID {
			continue
		}
		descendantFound = true
		if candidate.state != 'T' && candidate.state != 't' {
			t.Fatalf("detached descendant unexpectedly resumed: %#v", candidate)
		}
	}
	if !descendantFound {
		t.Fatalf("detached descendant disappeared after rejected thaw: %#v", processes)
	}
}

func TestACPDetachedDescendantProcess(t *testing.T) {
	mode := os.Getenv("GO_WANT_ACP_DETACHED_DESCENDANT")
	if mode == "" {
		return
	}
	markerPath := os.Getenv("ACP_DETACHED_DESCENDANT_MARKER")
	if markerPath == "" {
		t.Fatal("descendant marker path is required")
	}
	if mode == "child" {
		signal.Ignore(syscall.SIGTERM)
		if err := os.WriteFile(markerPath, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			t.Fatal(err)
		}
		if os.Getenv("ACP_DETACHED_DESCENDANT_SELF_STOP") == "1" {
			if err := syscall.Kill(os.Getpid(), syscall.SIGSTOP); err != nil {
				t.Fatal(err)
			}
		}
		for {
			time.Sleep(time.Second)
		}
	}
	command := exec.Command(os.Args[0], "-test.run=^TestACPDetachedDescendantProcess$")
	command.Env = append(os.Environ(), "GO_WANT_ACP_DETACHED_DESCENDANT=child")
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(markerPath); err == nil {
			if mode == "leader-wait" {
				for {
					time.Sleep(time.Second)
				}
			}
			return
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("detached descendant did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func readPIDMarker(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		t.Fatalf("invalid PID marker %q: %v", data, err)
	}
	return pid
}

func testAdapterCommand(t *testing.T) string {
	t.Helper()
	return copyTestExecutableForChild(t)
}

func copyTestExecutableForChild(t *testing.T) string {
	t.Helper()
	source, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	directory, err := os.MkdirTemp("", "orka-acp-adapter-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	if err := os.Chmod(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(directory, "acp-process-test")
	input, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = input.Close()
		_ = output.Close()
		t.Fatal(err)
	}
	if err := input.Close(); err != nil {
		_ = output.Close()
		t.Fatal(err)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	return target
}

func writeFakeProcStatus(t *testing.T, root string, pid int, contents string) {
	t.Helper()
	directory := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "status"), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
