//go:build linux

package acp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestSessionBaseAllowsOnlyPrivateChildAccess(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to launch two distinct child identities")
	}
	parent, err := os.MkdirTemp("", "orka-private-session-access-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	if err := os.Chmod(parent, 0o711); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(parent, "sessions")
	if err := os.Mkdir(base, 0o700); err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join(base, "identity-state")
	if err := os.WriteFile(ledger, []byte("supervisor-private"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := PrepareSessionPaths(base, "first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := PrepareSessionPaths(base, "second")
	if err != nil {
		t.Fatal(err)
	}
	for index, paths := range []SessionPaths{first, second} {
		if err := FinalizeSessionOwnership(paths.Root, 20000+index, 20000+index); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ReclaimSessionOwnership(paths.Root) })
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/bin/sh", "-c", `
set -eu
cd "$1"
printf '%s' owned > child-write
if cd "$2" 2>/dev/null; then exit 11; fi
if ls "$3" >/dev/null 2>&1; then exit 12; fi
if touch "$3/child-created" 2>/dev/null; then exit 13; fi
if cat "$4" >/dev/null 2>&1; then exit 14; fi
`, "session-access-probe", first.Workspace, second.Workspace, base, ledger)
	command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{
		Uid: 20000, Gid: 20000, Groups: []uint32{},
	}}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("isolated child filesystem access: %v\n%s", err, output)
	}
	if err := ReclaimSessionOwnership(first.Root); err != nil {
		t.Fatal(err)
	}
	if contents, err := os.ReadFile(filepath.Join(first.Workspace, "child-write")); err != nil || string(contents) != "owned" {
		t.Fatalf("child workspace write = %q, %v", contents, err)
	}
	for path, mode := range map[string]os.FileMode{base: 0o711, ledger: 0o600} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		owner, ok := info.Sys().(*syscall.Stat_t)
		if !ok || owner.Uid != 0 || info.Mode().Perm() != mode {
			t.Fatalf("supervisor ownership or mode changed for %s: %v", path, info)
		}
	}
}
