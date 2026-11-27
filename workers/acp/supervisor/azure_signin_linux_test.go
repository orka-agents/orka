//go:build linux

package supervisor

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

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
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close() //nolint:errcheck
	gateway := &azureGateway{profileDir: root, uid: child, gid: child}
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
	t.Cleanup(func() { _ = os.Chown(dir, 0, 0) })
	if err := gateway.writeProfile(placeholderAzureProfile(azureSignInPendingName), true); err != nil {
		t.Fatalf("write as the child: %v", err)
	}
	if raw, err := os.ReadFile(victim); err != nil || string(raw) != "keep" {
		t.Fatalf("the link's target = %q, %v", raw, err)
	}
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
	if len(childGroups) != 0 {
		t.Fatalf("the child's thread kept supplementary groups %v", childGroups)
	}
	if before, after := supervisorGroups, mustGetgroups(t); len(before) != len(after) {
		t.Fatalf("the supervisor's groups changed from %v to %v", before, after)
	}
	own := filepath.Join(home, "own")
	if err := os.WriteFile(own, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(own); err != nil || info.Sys().(*syscall.Stat_t).Uid != 0 {
		t.Fatalf("a later supervisor file = %v, %v", info, err)
	}
}

func mustGetgroups(t *testing.T) []int {
	t.Helper()
	groups, err := unix.Getgroups()
	if err != nil {
		t.Fatal(err)
	}
	return groups
}

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
