//go:build linux

package supervisor

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/orka-agents/orka/internal/acp"
)

// workspaceIdentityArg is the identity the supervisor would have recorded for
// root (following a symbolic link, the strongest case for refusal tests).
func workspaceIdentityArg(root string) string {
	var stat unix.Stat_t
	if unix.Stat(root, &stat) != nil {
		return "1:1"
	}
	return operatorWorkspaceIdentity{dev: uint64(stat.Dev), ino: stat.Ino}.String() //nolint:unconvert // Dev is uint32 on some Linux architectures.
}

// runFileHelper accepts the pre-identity argument form (mode, root, path,
// bound) and inserts root's identity, so cases read naturally.
func runFileHelper(t *testing.T, args []string, stdin io.Reader) (operatorFileStatus, []byte) {
	t.Helper()
	if len(args) == 4 {
		args = []string{args[0], args[1], workspaceIdentityArg(args[1]), args[2], args[3]}
	}
	var stdout bytes.Buffer
	RunOperatorFileHelper(args, stdin, &stdout)
	reader := bufio.NewReader(&stdout)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatalf("helper wrote no status line: %q", stdout.String())
	}
	var status operatorFileStatus
	if err := json.Unmarshal(line, &status); err != nil {
		t.Fatal(err)
	}
	rest, _ := io.ReadAll(reader)
	return status, rest
}

func TestOperatorFileHelperReadsRegularFilesBeneathTheWorkspace(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	status, content := runFileHelper(t, []string{OperatorFileHelperRead, root, "src/main.go", "1024"}, nil)
	if !status.OK || status.Size != 13 || string(content) != "package main\n" {
		t.Fatalf("read = %+v %q", status, content)
	}
	status, _ = runFileHelper(t, []string{OperatorFileHelperRead, root, "src/main.go", "4"}, nil)
	if status.OK || status.Code != operatorFileCodeTooLarge {
		t.Fatalf("oversized read must be refused: %+v", status)
	}
	status, _ = runFileHelper(t, []string{OperatorFileHelperRead, root, "src", "1024"}, nil)
	if status.OK || status.Code != operatorFileCodeNotRegular {
		t.Fatalf("directory read must be refused: %+v", status)
	}
	status, _ = runFileHelper(t, []string{OperatorFileHelperRead, root, "missing", "1024"}, nil)
	if status.OK || status.Code != operatorFileCodeNotFound {
		t.Fatalf("missing file must be not_found: %+v", status)
	}
}

func TestOperatorFileHelperRefusesTraversalAndSymlinks(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.Symlink(secret, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "dirlink")); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"../secret", "a/../../secret", "/etc/passwd", "link", "dirlink/secret", "./link"} {
		status, content := runFileHelper(t, []string{OperatorFileHelperRead, root, rel, "1024"}, nil)
		if status.OK || strings.Contains(string(content), "secret") {
			t.Fatalf("path %q escaped the workspace: %+v %q", rel, status, content)
		}
	}
	// Writes through a symlinked directory or onto a symlink never touch the target.
	for _, rel := range []string{"dirlink/new", "link"} {
		status, _ := runFileHelper(t, []string{OperatorFileHelperWrite, root, rel, "3"}, strings.NewReader("pwn"))
		if status.OK {
			t.Fatalf("write to %q must be refused", rel)
		}
	}
	if data, _ := os.ReadFile(secret); string(data) != "secret" {
		t.Fatalf("symlink target was modified: %q", data)
	}
	if _, err := os.Stat(filepath.Join(outside, "new")); !os.IsNotExist(err) {
		t.Fatalf("write escaped through a symlinked directory: %v", err)
	}
}

func TestOperatorFileHelperWritesExactlyTheDeclaredSize(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(target, []byte("original"), 0o640); err != nil {
		t.Fatal(err)
	}
	// A truncated upload never replaces the target.
	status, _ := runFileHelper(t, []string{OperatorFileHelperWrite, root, "notes.txt", "10"}, strings.NewReader("short"))
	if status.OK {
		t.Fatal("short upload must fail")
	}
	// An oversized upload never replaces the target.
	status, _ = runFileHelper(t, []string{OperatorFileHelperWrite, root, "notes.txt", "3"}, strings.NewReader("too long"))
	if status.OK || status.Code != operatorFileCodeTooLarge {
		t.Fatalf("oversized upload must fail: %+v", status)
	}
	if data, _ := os.ReadFile(target); string(data) != "original" {
		t.Fatalf("failed uploads modified the target: %q", data)
	}
	status, _ = runFileHelper(t, []string{OperatorFileHelperWrite, root, "notes.txt", strconv.Itoa(len("updated"))}, strings.NewReader("updated"))
	if !status.OK || status.Size != 7 {
		t.Fatalf("exact upload failed: %+v", status)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(target); string(data) != "updated" || info.Mode().Perm() != 0o640 {
		t.Fatalf("upload = %q mode %v; want updated with preserved mode", data, info.Mode().Perm())
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 1 {
		t.Fatalf("temporary upload files were left behind: %v", entries)
	}
	status, _ = runFileHelper(t, []string{OperatorFileHelperWrite, root, "missing-dir/file", "1"}, strings.NewReader("x"))
	if status.OK || status.Code != operatorFileCodeNotFound {
		t.Fatalf("missing parent directory must be not_found: %+v", status)
	}
}

func TestOperatorFileHelperRejectsMalformedInvocations(t *testing.T) {
	root := t.TempDir()
	for _, args := range [][]string{
		{},
		{"chmod", root, "a", "1"},
		{OperatorFileHelperRead, "relative", "a", "1"},
		{OperatorFileHelperRead, root, "a", "-1"},
		{OperatorFileHelperRead, root, "a", "not-a-number"},
		{OperatorFileHelperRead, root, "/a", "1"},
	} {
		if status, _ := runFileHelper(t, args, nil); status.OK {
			t.Fatalf("invocation %v must be refused", args)
		}
	}
}

// The child owns its session root, so it can replace the workspace directory
// with a symbolic link to its private home; the helper must refuse to follow it.
func TestOperatorFileHelperRefusesASymlinkedWorkspaceRoot(t *testing.T) {
	sessionRoot := t.TempDir()
	home := filepath.Join(sessionRoot, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "credentials"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(sessionRoot, "workspace")
	if err := os.Symlink(home, workspace); err != nil {
		t.Fatal(err)
	}
	status, content := runFileHelper(t, []string{OperatorFileHelperRead, workspace, "credentials", "1024"}, nil)
	if status.OK || strings.Contains(string(content), "secret") {
		t.Fatalf("a symlinked workspace root was followed: %+v %q", status, content)
	}
	status, _ = runFileHelper(t, []string{OperatorFileHelperWrite, workspace, "planted", "1"}, strings.NewReader("x"))
	if status.OK {
		t.Fatal("a write through a symlinked workspace root must be refused")
	}
	if _, err := os.Lstat(filepath.Join(home, "planted")); !os.IsNotExist(err) {
		t.Fatalf("write escaped through a symlinked workspace root: %v", err)
	}
}

// The supervisor runs with a 077 umask that the helper inherits; uploads must
// still keep a replaced file's mode and give a new file 0644.
func TestOperatorFileHelperAppliesTheIntendedModeDespiteUmask(t *testing.T) {
	previous := unix.Umask(0o077)
	defer unix.Umask(previous)
	root := t.TempDir()
	for name, mode := range map[string]os.FileMode{"script.sh": 0o755, "notes.txt": 0o644} {
		target := filepath.Join(root, name)
		if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(target, mode); err != nil {
			t.Fatal(err)
		}
		status, _ := runFileHelper(t, []string{OperatorFileHelperWrite, root, name, "3"}, strings.NewReader("new"))
		if !status.OK {
			t.Fatalf("upload %s failed: %+v", name, status)
		}
		if info, err := os.Stat(target); err != nil || info.Mode().Perm() != mode {
			t.Fatalf("%s mode = %v (%v), want %v", name, info.Mode().Perm(), err, mode)
		}
	}
	status, _ := runFileHelper(t, []string{OperatorFileHelperWrite, root, "fresh.txt", "3"}, strings.NewReader("new"))
	if !status.OK {
		t.Fatalf("new upload failed: %+v", status)
	}
	if info, err := os.Stat(filepath.Join(root, "fresh.txt")); err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("new file mode = %v (%v), want 0644", info.Mode().Perm(), err)
	}
}

// The child owns the directories beneath the workspace and can move one into
// its private home between the helper resolving a parent directory and
// opening or committing the final entry. The operation must fail and nothing
// outside the workspace may be read or written.
func TestOperatorFileHelperRefusesADirectoryMovedOutDuringResolution(t *testing.T) {
	setup := func(t *testing.T) (root, outside string) {
		t.Helper()
		session := t.TempDir()
		root, outside = filepath.Join(session, "workspace"), filepath.Join(session, "home")
		for _, dir := range []string{filepath.Join(root, "a"), outside} {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(root, "a", "notes"), []byte("public"), 0o600); err != nil {
			t.Fatal(err)
		}
		return root, outside
	}
	moveOut := func(t *testing.T, root, outside string) func() {
		return func() {
			// Swap the workspace's a with a directory of the same name in
			// the private home whose notes holds a secret.
			if err := os.Rename(filepath.Join(root, "a"), filepath.Join(outside, "a")); err != nil {
				t.Error(err)
			}
			if err := os.WriteFile(filepath.Join(outside, "a", "notes"), []byte("secret"), 0o600); err != nil {
				t.Error(err)
			}
		}
	}
	t.Cleanup(func() { operatorFileHelperResolvedHook = nil })

	t.Run("read", func(t *testing.T) {
		root, outside := setup(t)
		operatorFileHelperResolvedHook = moveOut(t, root, outside)
		defer func() { operatorFileHelperResolvedHook = nil }()
		status, content := runFileHelper(t, []string{OperatorFileHelperRead, root, "a/notes", "1024"}, nil)
		if status.OK || strings.Contains(string(content), "secret") {
			t.Fatalf("read followed a directory moved out of the workspace: %+v %q", status, content)
		}
	})
	t.Run("write", func(t *testing.T) {
		root, outside := setup(t)
		operatorFileHelperResolvedHook = moveOut(t, root, outside)
		defer func() { operatorFileHelperResolvedHook = nil }()
		status, _ := runFileHelper(t, []string{OperatorFileHelperWrite, root, "a/notes", "3"}, strings.NewReader("pwn"))
		if status.OK {
			t.Fatal("write committed into a directory moved out of the workspace")
		}
		if data, _ := os.ReadFile(filepath.Join(outside, "a", "notes")); string(data) != "secret" {
			t.Fatalf("the file outside the workspace was modified: %q", data)
		}
		entries, _ := os.ReadDir(filepath.Join(outside, "a"))
		if len(entries) != 1 {
			t.Fatalf("temporary upload files were left outside the workspace: %v", entries)
		}
	})
}

// The child owns its session root, so it can rename the workspace aside and
// put another directory it can read in its place.
func TestOperatorFileHelperRefusesAReplacedWorkspaceRoot(t *testing.T) {
	session := t.TempDir()
	root, home := filepath.Join(session, "workspace"), filepath.Join(session, "home")
	for _, dir := range []string{root, home} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "credentials"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity := workspaceIdentityArg(root)
	if err := os.Rename(root, filepath.Join(session, "aside")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(home, root); err != nil {
		t.Fatal(err)
	}
	status, content := runFileHelper(t, []string{OperatorFileHelperRead, root, identity, "credentials", "1024"}, nil)
	if status.OK || strings.Contains(string(content), "secret") || status.Code != operatorFileCodeDenied {
		t.Fatalf("a replaced workspace root was used: %+v %q", status, content)
	}
	status, _ = runFileHelper(t, []string{OperatorFileHelperWrite, root, identity, "planted", "1"}, strings.NewReader("x"))
	if status.OK {
		t.Fatal("a write into a replaced workspace root must be refused")
	}
	if _, err := os.Lstat(filepath.Join(root, "planted")); !os.IsNotExist(err) {
		t.Fatalf("write landed in the replacement directory: %v", err)
	}
}

// recreateDirectoryWithInode creates directories under parent until one is
// given the wanted inode number, as a session child that has deleted its
// workspace would, and reports whether one was.
func recreateDirectoryWithInode(t *testing.T, parent, prefix string, want uint64) (string, bool) {
	t.Helper()
	for attempt := range 64 {
		dir := filepath.Join(parent, fmt.Sprintf("%s-%d", prefix, attempt))
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		var stat unix.Stat_t
		if err := unix.Lstat(dir, &stat); err != nil {
			t.Fatal(err)
		}
		if stat.Ino == want {
			return dir, true
		}
	}
	return "", false
}

// Renaming the workspace aside keeps its inode allocated, so the replacement
// has a different one. Deleting it does not: the filesystem can hand the same
// inode number to the next directory the child creates, which then carries
// the recorded device and inode. The supervisor holds the recorded directory
// open for the session's lifetime so that cannot happen.
func TestOperatorFileHelperRefusesADeletedAndRecreatedWorkspaceRoot(t *testing.T) {
	session := t.TempDir()
	probe := filepath.Join(session, "probe")
	if err := os.Mkdir(probe, 0o700); err != nil {
		t.Fatal(err)
	}
	var probeStat unix.Stat_t
	if err := unix.Lstat(probe, &probeStat); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(probe); err != nil {
		t.Fatal(err)
	}
	if _, reused := recreateDirectoryWithInode(t, session, "probe", probeStat.Ino); !reused {
		t.Skip("this filesystem does not hand a deleted directory's inode number to the next one")
	}

	paths := acp.SessionPaths{Workspace: filepath.Join(session, "workspace")}
	if err := os.Mkdir(paths.Workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := acp.RecordWorkspaceIdentity(&paths); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(paths.ReleaseWorkspaceIdentity)
	device, inode, err := paths.PinnedWorkspaceIdentity()
	if err != nil {
		t.Fatal(err)
	}
	identity := operatorWorkspaceIdentity{dev: device, ino: inode}.String()

	// The child deletes its workspace and creates directories until one is
	// given the recorded inode number, then moves it into place. With the
	// directory held, none is; the last attempt stands in for the best the
	// child can do.
	if err := os.Remove(paths.Workspace); err != nil {
		t.Fatal(err)
	}
	replacement, reused := recreateDirectoryWithInode(t, session, "replacement", inode)
	if !reused {
		replacement = filepath.Join(session, "replacement-0")
	}
	if err := os.WriteFile(filepath.Join(replacement, "credentials"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, paths.Workspace); err != nil {
		t.Fatal(err)
	}
	status, content := runFileHelper(t, []string{OperatorFileHelperRead, paths.Workspace, identity, "credentials", "1024"}, nil)
	if status.OK || strings.Contains(string(content), "secret") || status.Code != operatorFileCodeDenied {
		t.Fatalf("a deleted and recreated workspace root was used (inode reused: %v): %+v %q", reused, status, content)
	}
	status, _ = runFileHelper(t, []string{OperatorFileHelperWrite, paths.Workspace, identity, "planted", "1"}, strings.NewReader("x"))
	if status.OK {
		t.Fatal("a write into a deleted and recreated workspace root must be refused")
	}
	if _, err := os.Lstat(filepath.Join(paths.Workspace, "planted")); !os.IsNotExist(err) {
		t.Fatalf("write landed in the recreated directory: %v", err)
	}
}
