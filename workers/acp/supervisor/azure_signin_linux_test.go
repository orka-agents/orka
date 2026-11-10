//go:build linux

package supervisor

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
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
	if err := gateway.writeProfile(placeholderAzureProfile(), true); err != nil {
		t.Fatalf("write as the child: %v", err)
	}
	if raw, err := os.ReadFile(victim); err != nil || string(raw) != "keep" {
		t.Fatalf("the link's target = %q, %v", raw, err)
	}
	// Only the child can look inside its directory without DAC_OVERRIDE, and
	// only through the directory handle: the test's temporary parent is root's.
	var info os.FileInfo
	if err := asSessionChild(child, child, func() (err error) {
		info, err = root.Lstat(azureProfileName)
		return err
	}); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("profile = %v, %v", info, err)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != child || stat.Gid != child {
		t.Fatalf("profile owner = %+v", info.Sys())
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
