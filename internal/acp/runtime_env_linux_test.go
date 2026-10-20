//go:build linux

package acp

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/workspacedelta"
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

func TestReadOnlyWorkspaceDeniesChildWrites(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to launch distinct child identities")
	}
	parent, err := os.MkdirTemp("", "orka-read-only-workspace-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	if err := os.Chmod(parent, 0o711); err != nil {
		t.Fatal(err)
	}
	paths, err := PrepareSessionPaths(filepath.Join(parent, "sessions"), "read")
	if err != nil {
		t.Fatal(err)
	}
	// The supervisor materializes with umask 077. ".claude" is a protected
	// path whose exact modes workspace validation compares with the baseline.
	for _, dir := range []string{"dir", ".claude"} {
		if err := os.Mkdir(filepath.Join(paths.Workspace, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for path, mode := range map[string]os.FileMode{
		filepath.Join(paths.Workspace, "readme"):                   0o600,
		filepath.Join(paths.Workspace, ".claude", "settings.json"): 0o600,
		filepath.Join(paths.Workspace, "dir", "writable"):          0o666,
	} {
		if err := os.WriteFile(path, []byte("original"), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := PrepareReadOnlyWorkspaceModes(paths.Workspace); err != nil {
		t.Fatal(err)
	}
	baseline, err := workspacedelta.Capture(paths.Workspace, workspacedelta.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := FinalizeSessionOwnership(paths.Root, 20010, 20010); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ReclaimSessionOwnership(paths.Root) })
	if err := ProtectReadOnlySessionWorkspace(paths, 20010, 20010); err != nil {
		t.Fatal(err)
	}

	run := func(uid uint32, script string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, "/bin/sh", "-c", "set -eu\n"+script,
			"read-only-probe", paths.Workspace, paths.Root, paths.Home)
		command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{
			Uid: uid, Gid: uid, Groups: []uint32{},
		}}
		if output, err := command.CombinedOutput(); err != nil {
			return fmt.Errorf("%w\n%s", err, output)
		}
		return nil
	}
	if err := run(20010, `
cd "$1"
test "$(cat readme)" = original
test "$(cat .claude/settings.json)" = original
test "$(cat dir/writable)" = original
ls dir >/dev/null
if printf x > created 2>/dev/null; then exit 11; fi
if printf x >> readme 2>/dev/null; then exit 12; fi
if printf x >> dir/writable 2>/dev/null; then exit 13; fi
if rm readme 2>/dev/null; then exit 14; fi
if mkdir created-dir 2>/dev/null; then exit 15; fi
if chmod 0666 readme 2>/dev/null; then exit 16; fi
if mv "$1" "$2/workspace-aside" 2>/dev/null; then exit 17; fi
if mkdir "$2/replacement" 2>/dev/null; then exit 18; fi
printf '%s' home > "$3/home-write"
`); err != nil {
		t.Fatalf("read-only child filesystem access: %v", err)
	}
	if err := run(20011, `
if cat "$1/readme" >/dev/null 2>&1; then exit 21; fi
if ls "$1" >/dev/null 2>&1; then exit 22; fi
`); err != nil {
		t.Fatalf("another child identity reached the protected workspace: %v", err)
	}

	if err := ReclaimSessionOwnership(paths.Root); err != nil {
		t.Fatal(err)
	}
	result, err := workspacedelta.BuildWithLimits(baseline, paths.Workspace, workspacedelta.IntentRead, workspacedelta.BuildLimits{})
	if err != nil {
		t.Fatalf("read-only protection changed the validated workspace: %v", err)
	}
	if len(result.Changes) != 0 || len(result.Deletions) != 0 {
		t.Fatalf("read-only workspace changed: %+v", result)
	}
	if contents, err := os.ReadFile(filepath.Join(paths.Home, "home-write")); err != nil || string(contents) != "home" {
		t.Fatalf("child home write = %q, %v", contents, err)
	}
}
