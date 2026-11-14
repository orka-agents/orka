//go:build linux

package supervisor

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// The runtime's supervisor is root with CHOWN, SETUID and SETGID but not
// DAC_OVERRIDE, so it cannot create files in the child's 0700 ~/.azure as
// itself. A sign-in writes the profile as the child instead. Run it as root,
// ideally with the runtime's capabilities, to cover that.
func TestAzureProfileWriteActsAsTheSessionChild(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root, as the runtime's supervisor runs")
	}
	const child = 4242
	supervisorGroups := mustGetgroups(t)
	home := t.TempDir()
	dir := filepath.Join(home, ".azure")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Setup opens the directory before the session's files become the
	// child's, as session creation does.
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close() //nolint:errcheck
	gateway := &azureGateway{profileDir: root, uid: child, gid: child}
	// The child points the profile at a root-only file.
	victim := filepath.Join(home, "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, azureProfileName)
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Lchown(link, child, child); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(dir, child, child); err != nil {
		t.Fatal(err)
	}
	// Without DAC_OVERRIDE the test's cleanup must reclaim the directory.
	t.Cleanup(func() { _ = os.Chown(dir, 0, 0) })
	if err := gateway.writeProfile(placeholderAzureProfile(azureSignInPendingName), true); err != nil {
		t.Fatalf("write as the child: %v", err)
	}
	if raw, err := os.ReadFile(victim); err != nil || string(raw) != "keep" {
		t.Fatalf("the link's target = %q, %v", raw, err)
	}
	// Only the child can look inside its directory without DAC_OVERRIDE, and
	// only through the directory handle: the test's temporary parent is root's.
	var info os.FileInfo
	var childGroups []int
	if err := asSessionChild(child, child, func() (err error) {
		if childGroups, err = unix.Getgroups(); err != nil {
			return err
		}
		info, err = root.Lstat(azureProfileName)
		return err
	}); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("profile = %v, %v", info, err)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != child || stat.Gid != child {
		t.Fatalf("profile owner = %+v", info.Sys())
	}
	// The child's thread carries none of the supervisor's groups, and the
	// supervisor keeps its own afterwards.
	if len(childGroups) != 0 {
		t.Fatalf("the child's thread kept supplementary groups %v", childGroups)
	}
	if before, after := supervisorGroups, mustGetgroups(t); len(before) != len(after) {
		t.Fatalf("the supervisor's groups changed from %v to %v", before, after)
	}
	// The supervisor's own identity is unchanged afterwards.
	own := filepath.Join(home, "own")
	if err := os.WriteFile(own, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(own); err != nil || info.Sys().(*syscall.Stat_t).Uid != 0 {
		t.Fatalf("a later supervisor file = %v, %v", info, err)
	}
}

// mustGetgroups reads the calling thread's supplementary groups. Outside
// asSessionChild every thread carries the supervisor's.
func mustGetgroups(t *testing.T) []int {
	t.Helper()
	groups, err := unix.Getgroups()
	if err != nil {
		t.Fatal(err)
	}
	return groups
}

// An unprivileged process may act as itself, as the gateway fixtures do,
// but cannot take any other identity: the switch fails closed rather than
// running fn as the caller.
func TestAsSessionChildWithoutPrivilegeActsOnlyAsItself(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("needs an unprivileged user")
	}
	ran := false
	if err := asSessionChild(os.Geteuid(), os.Getegid(), func() error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("acting as itself = %v, ran %v", err, ran)
	}
	ran = false
	if err := asSessionChild(os.Geteuid()+1, os.Getegid()+1, func() error { ran = true; return nil }); err == nil || ran {
		t.Fatalf("taking another identity = %v, ran %v", err, ran)
	}
}
