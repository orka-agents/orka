//go:build unix

package toolbox

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// realTempDir resolves the temporary directory so no symlink (such as macOS
// /var -> /private/var) sits in the source chain.
func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func fakeELF(machine uint16) []byte {
	header := make([]byte, 64)
	copy(header, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(header[18:20], machine)
	return header
}

func elfFor(arch string) []byte {
	if arch == "arm64" {
		return fakeELF(elfMachineAARCH6)
	}
	return fakeELF(elfMachineAMD64)
}

func otherArch() string {
	if runtime.GOARCH == "arm64" {
		return "amd64"
	}
	return "arm64"
}

func mustWrite(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func newLayout(t *testing.T) (source, destination string) {
	t.Helper()
	root := realTempDir(t)
	source = filepath.Join(root, "opt", "tools")
	destination = filepath.Join(root, "out")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(destination, 0o777); err != nil {
		t.Fatal(err)
	}
	return source, destination
}

func copyOptions(source, destination string, entries ...string) Options {
	return Options{Source: source, Destination: destination, PathEntries: entries, Arch: runtime.GOARCH, Limits: DefaultLimits()}
}

func failureReason(t *testing.T, err error) string {
	t.Helper()
	var failure *Failure
	if !errors.As(err, &failure) {
		t.Fatalf("expected a toolbox Failure, got %T: %v", err, err)
	}
	return failure.Reason
}

func TestCopySanitizesModesLinksAndSpecialBits(t *testing.T) {
	source, destination := newLayout(t)
	mustWrite(t, filepath.Join(source, "bin", "tool"), elfFor(runtime.GOARCH), 0o755)
	mustWrite(t, filepath.Join(source, "bin", "setuid"), elfFor(runtime.GOARCH), 0o4755)
	mustWrite(t, filepath.Join(source, "bin", "setgid"), elfFor(runtime.GOARCH), 0o2755)
	mustWrite(t, filepath.Join(source, "bin", "script"), []byte("#!/bin/sh\necho hi\n"), 0o700)
	mustWrite(t, filepath.Join(source, "share", "doc.txt"), []byte("docs"), 0o600)
	mustWrite(t, filepath.Join(source, "share", "world.txt"), []byte("w"), 0o666)
	if err := os.Mkdir(filepath.Join(source, "sticky"), 0o1777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(source, "sticky"), 0o1777); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(source, "share", "doc.txt"), filepath.Join(source, "share", "hard.txt")); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{
		"bin/relative": "tool",
		"bin/absolute": "/opt/tools/bin/tool",
		"bin/escape":   "../../../etc/passwd",
		"bin/loop-a":   "loop-b",
		"bin/loop-b":   "loop-a",
		"bin/outside":  "/usr/bin/env",
	} {
		if err := os.Symlink(target, filepath.Join(source, name)); err != nil {
			t.Fatal(err)
		}
	}

	summary, err := Copy(copyOptions(source, destination, "bin"))
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if summary.AlreadyComplete || summary.Entries == 0 || summary.Bytes == 0 || summary.Arch != runtime.GOARCH {
		t.Fatalf("unexpected summary %+v", summary)
	}
	root := filepath.Join(destination, OutputRootName)
	for path, want := range map[string]os.FileMode{
		"bin":             0o755 | os.ModeDir,
		"sticky":          0o755 | os.ModeDir,
		"bin/tool":        0o555,
		"bin/setuid":      0o555,
		"bin/setgid":      0o555,
		"bin/script":      0o555,
		"share/doc.txt":   0o444,
		"share/world.txt": 0o444,
		"share/hard.txt":  0o444,
	} {
		info, err := os.Lstat(filepath.Join(root, path))
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if info.Mode() != want {
			t.Errorf("%s mode = %v, want %v", path, info.Mode(), want)
		}
	}
	for name, target := range map[string]string{
		"bin/relative": "tool", "bin/absolute": "/opt/tools/bin/tool", "bin/escape": "../../../etc/passwd",
		"bin/loop-a": "loop-b", "bin/loop-b": "loop-a", "bin/outside": "/usr/bin/env",
	} {
		got, err := os.Readlink(filepath.Join(root, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != target {
			t.Errorf("%s -> %q, want %q", name, got, target)
		}
	}
	var docStat, hardStat unix.Stat_t
	if err := unix.Lstat(filepath.Join(root, "share", "doc.txt"), &docStat); err != nil {
		t.Fatal(err)
	}
	if err := unix.Lstat(filepath.Join(root, "share", "hard.txt"), &hardStat); err != nil {
		t.Fatal(err)
	}
	if docStat.Ino == hardStat.Ino {
		t.Fatal("hardline files must be copied as separate files")
	}
	if _, err := os.Stat(filepath.Join(destination, completeName)); err != nil {
		t.Fatalf("completion marker: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(destination, stagingName)); !os.IsNotExist(err) {
		t.Fatalf("staging folder must be renamed away, got %v", err)
	}

	again, err := Copy(copyOptions(source, destination, "bin"))
	if err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if !again.AlreadyComplete || again.Entries != summary.Entries {
		t.Fatalf("rerun must be a no-op, got %+v", again)
	}
}

func TestCopyDoesNotCopyExtendedAttributes(t *testing.T) {
	source, destination := newLayout(t)
	file := filepath.Join(source, "bin", "tool")
	mustWrite(t, file, elfFor(runtime.GOARCH), 0o755)
	attr := "user.orka.test"
	if runtime.GOOS == "darwin" {
		attr = "ai.orka.test"
	}
	if err := unix.Setxattr(file, attr, []byte("x"), 0); err != nil {
		t.Skipf("extended attributes are unavailable here: %v", err)
	}
	if _, err := Copy(copyOptions(source, destination, "bin")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1024)
	n, err := unix.Listxattr(filepath.Join(destination, OutputRootName, "bin", "tool"), buf)
	if err != nil && !errors.Is(err, unix.ENOTSUP) {
		t.Fatal(err)
	}
	if n > 0 && strings.Contains(string(buf[:n]), attr) {
		t.Fatalf("extended attribute %s was copied", attr)
	}
}

func TestCopyRejectsFIFO(t *testing.T) {
	source, destination := newLayout(t)
	if err := unix.Mkfifo(filepath.Join(source, "fifo"), 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	_, err := Copy(copyOptions(source, destination))
	if got := failureReason(t, err); got != ReasonUnsupportedFileType {
		t.Fatalf("reason = %s, want %s", got, ReasonUnsupportedFileType)
	}
}

func TestCopyEnforcesLimits(t *testing.T) {
	t.Run("too deep", func(t *testing.T) {
		source, destination := newLayout(t)
		mustWrite(t, filepath.Join(source, "a", "b", "c", "d", "file"), []byte("x"), 0o644)
		opts := copyOptions(source, destination)
		opts.Limits.MaxDepth = 2
		_, err := Copy(opts)
		if got := failureReason(t, err); got != ReasonTooDeep {
			t.Fatalf("reason = %s, want %s", got, ReasonTooDeep)
		}
	})
	t.Run("file too large", func(t *testing.T) {
		source, destination := newLayout(t)
		mustWrite(t, filepath.Join(source, "big"), bytes.Repeat([]byte("x"), 100), 0o644)
		opts := copyOptions(source, destination)
		opts.Limits.MaxFileBytes = 50
		_, err := Copy(opts)
		if got := failureReason(t, err); got != ReasonFileTooLarge {
			t.Fatalf("reason = %s, want %s", got, ReasonFileTooLarge)
		}
	})
	t.Run("total too large", func(t *testing.T) {
		source, destination := newLayout(t)
		mustWrite(t, filepath.Join(source, "a"), bytes.Repeat([]byte("x"), 40), 0o644)
		mustWrite(t, filepath.Join(source, "b"), bytes.Repeat([]byte("x"), 40), 0o644)
		opts := copyOptions(source, destination)
		opts.Limits.MaxTotalBytes = 60
		_, err := Copy(opts)
		if got := failureReason(t, err); got != ReasonTooLarge {
			t.Fatalf("reason = %s, want %s", got, ReasonTooLarge)
		}
	})
	t.Run("symlink text counts toward the total", func(t *testing.T) {
		source, destination := newLayout(t)
		if err := os.Symlink(strings.Repeat("t", 40), filepath.Join(source, "link")); err != nil {
			t.Fatal(err)
		}
		opts := copyOptions(source, destination)
		opts.Limits.MaxTotalBytes = 30
		_, err := Copy(opts)
		if got := failureReason(t, err); got != ReasonTooLarge {
			t.Fatalf("reason = %s, want %s", got, ReasonTooLarge)
		}
	})
	t.Run("too many entries", func(t *testing.T) {
		source, destination := newLayout(t)
		for _, name := range []string{"a", "b", "c", "d"} {
			mustWrite(t, filepath.Join(source, name), []byte("x"), 0o644)
		}
		opts := copyOptions(source, destination)
		opts.Limits.MaxEntries = 3
		_, err := Copy(opts)
		if got := failureReason(t, err); got != ReasonTooManyEntries {
			t.Fatalf("reason = %s, want %s", got, ReasonTooManyEntries)
		}
	})
}

func TestCopyRejectsBadSources(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		root := realTempDir(t)
		destination := filepath.Join(root, "out")
		if err := os.Mkdir(destination, 0o777); err != nil {
			t.Fatal(err)
		}
		_, err := Copy(copyOptions(filepath.Join(root, "missing"), destination))
		if got := failureReason(t, err); got != ReasonSourceOpen {
			t.Fatalf("reason = %s, want %s", got, ReasonSourceOpen)
		}
	})
	t.Run("symlink source", func(t *testing.T) {
		source, destination := newLayout(t)
		link := filepath.Join(filepath.Dir(source), "link")
		if err := os.Symlink(source, link); err != nil {
			t.Fatal(err)
		}
		_, err := Copy(copyOptions(link, destination))
		if got := failureReason(t, err); got != ReasonSourceOpen {
			t.Fatalf("reason = %s, want %s", got, ReasonSourceOpen)
		}
	})
	t.Run("symlinked parent", func(t *testing.T) {
		source, destination := newLayout(t)
		parentLink := filepath.Join(filepath.Dir(filepath.Dir(source)), "opt-link")
		if err := os.Symlink(filepath.Dir(source), parentLink); err != nil {
			t.Fatal(err)
		}
		_, err := Copy(copyOptions(filepath.Join(parentLink, "tools"), destination))
		if got := failureReason(t, err); got != ReasonSourceOpen {
			t.Fatalf("reason = %s, want %s", got, ReasonSourceOpen)
		}
	})
	t.Run("relative source", func(t *testing.T) {
		_, err := Copy(copyOptions("opt/tools", "/tmp"))
		if got := failureReason(t, err); got != ReasonInvalidArguments {
			t.Fatalf("reason = %s, want %s", got, ReasonInvalidArguments)
		}
	})
}

func TestCopyChecksArchitecture(t *testing.T) {
	t.Run("mismatch", func(t *testing.T) {
		source, destination := newLayout(t)
		mustWrite(t, filepath.Join(source, "bin", "tool"), elfFor(otherArch()), 0o755)
		_, err := Copy(copyOptions(source, destination, "bin"))
		if got := failureReason(t, err); got != ReasonArchMismatch {
			t.Fatalf("reason = %s, want %s", got, ReasonArchMismatch)
		}
		if _, err := os.Lstat(filepath.Join(destination, OutputRootName)); !os.IsNotExist(err) {
			t.Fatal("a failed copy must not publish root")
		}
	})
	t.Run("unknown machine", func(t *testing.T) {
		source, destination := newLayout(t)
		mustWrite(t, filepath.Join(source, "bin", "tool"), fakeELF(0x08), 0o755)
		_, err := Copy(copyOptions(source, destination, "bin"))
		if got := failureReason(t, err); got != ReasonArchMismatch {
			t.Fatalf("reason = %s, want %s", got, ReasonArchMismatch)
		}
	})
	t.Run("scripts and files outside path entries are ignored", func(t *testing.T) {
		source, destination := newLayout(t)
		mustWrite(t, filepath.Join(source, "bin", "script"), []byte("#!/bin/sh\n"), 0o755)
		mustWrite(t, filepath.Join(source, "bin", "tool"), elfFor(runtime.GOARCH), 0o755)
		mustWrite(t, filepath.Join(source, "lib", "foreign.so"), elfFor(otherArch()), 0o644)
		if _, err := Copy(copyOptions(source, destination, "bin")); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("symlinks inside the tree are followed", func(t *testing.T) {
		source, destination := newLayout(t)
		mustWrite(t, filepath.Join(source, "libexec", "real"), elfFor(otherArch()), 0o755)
		if err := os.Mkdir(filepath.Join(source, "bin"), 0o755); err != nil {
			t.Fatal(err)
		}
		// In the Pod the source path is the mount path itself, so absolute
		// link text under it stays inside the tree.
		if err := os.Symlink(filepath.Join(source, "libexec", "real"), filepath.Join(source, "bin", "absolute")); err != nil {
			t.Fatal(err)
		}
		_, err := Copy(copyOptions(source, destination, "bin"))
		if got := failureReason(t, err); got != ReasonArchMismatch {
			t.Fatalf("absolute in-tree symlink: reason = %s, want %s", got, ReasonArchMismatch)
		}
		source, destination = newLayout(t)
		mustWrite(t, filepath.Join(source, "libexec", "real"), elfFor(otherArch()), 0o755)
		if err := os.Mkdir(filepath.Join(source, "bin"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../libexec/real", filepath.Join(source, "bin", "relative")); err != nil {
			t.Fatal(err)
		}
		_, err = Copy(copyOptions(source, destination, "bin"))
		if got := failureReason(t, err); got != ReasonArchMismatch {
			t.Fatalf("relative in-tree symlink: reason = %s, want %s", got, ReasonArchMismatch)
		}
	})
	t.Run("in-tree symlink chains beyond the bound are rejected", func(t *testing.T) {
		source, destination := newLayout(t)
		mustWrite(t, filepath.Join(source, "libexec", "real"), elfFor(otherArch()), 0o755)
		if err := os.Mkdir(filepath.Join(source, "bin"), 0o755); err != nil {
			t.Fatal(err)
		}
		previous := "../libexec/real"
		for i := range maxLinkHops + 1 {
			name := filepath.Join(source, "libexec", "hop"+strconv.Itoa(i))
			if err := os.Symlink(previous, name); err != nil {
				t.Fatal(err)
			}
			previous = "../libexec/hop" + strconv.Itoa(i)
		}
		if err := os.Symlink(previous, filepath.Join(source, "bin", "tool")); err != nil {
			t.Fatal(err)
		}
		_, err := Copy(copyOptions(source, destination, "bin"))
		if got := failureReason(t, err); got != ReasonUnsupportedFileType {
			t.Fatalf("reason = %s, want %s", got, ReasonUnsupportedFileType)
		}
	})
	t.Run("symlinks leaving the tree are skipped", func(t *testing.T) {
		source, destination := newLayout(t)
		if err := os.Mkdir(filepath.Join(source, "bin"), 0o755); err != nil {
			t.Fatal(err)
		}
		for name, target := range map[string]string{"escape": "../../../bin/sh", "outside": "/usr/bin/env", "loop": "loop"} {
			if err := os.Symlink(target, filepath.Join(source, "bin", name)); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := Copy(copyOptions(source, destination, "bin")); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("missing path entry", func(t *testing.T) {
		source, destination := newLayout(t)
		mustWrite(t, filepath.Join(source, "bin", "tool"), elfFor(runtime.GOARCH), 0o755)
		_, err := Copy(copyOptions(source, destination, "bin", "sbin"))
		if got := failureReason(t, err); got != ReasonMissingPathEntry {
			t.Fatalf("reason = %s, want %s", got, ReasonMissingPathEntry)
		}
		source, destination = newLayout(t)
		mustWrite(t, filepath.Join(source, "bin"), []byte("not a folder"), 0o644)
		_, err = Copy(copyOptions(source, destination, "bin"))
		if got := failureReason(t, err); got != ReasonMissingPathEntry {
			t.Fatalf("file path entry: reason = %s, want %s", got, ReasonMissingPathEntry)
		}
	})
}

func TestCopyRecoversFromInterruptedRun(t *testing.T) {
	source, destination := newLayout(t)
	mustWrite(t, filepath.Join(source, "bin", "tool"), elfFor(runtime.GOARCH), 0o755)
	// Simulate a crash mid-copy: a partial staging tree and a published root
	// without its completion marker.
	mustWrite(t, filepath.Join(destination, stagingName, "junk"), []byte("partial"), 0o644)
	mustWrite(t, filepath.Join(destination, OutputRootName, "stale"), []byte("stale"), 0o644)
	summary, err := Copy(copyOptions(source, destination, "bin"))
	if err != nil {
		t.Fatal(err)
	}
	if summary.AlreadyComplete {
		t.Fatal("interrupted run must be redone")
	}
	if _, err := os.Lstat(filepath.Join(destination, OutputRootName, "stale")); !os.IsNotExist(err) {
		t.Fatal("stale root must be discarded")
	}
	if _, err := os.Lstat(filepath.Join(destination, OutputRootName, "bin", "tool")); err != nil {
		t.Fatal(err)
	}
}

func TestCheckMountedAndVerifyMounted(t *testing.T) {
	root := realTempDir(t)
	mount := filepath.Join(root, "opt", "tools")
	mustWrite(t, filepath.Join(mount, "bin", "tool"), elfFor(runtime.GOARCH), 0o755)
	if err := VerifyMounted(mount, []string{"bin"}); err != nil {
		t.Fatal(err)
	}
	if err := CheckMounted(mount, []string{"bin"}, runtime.GOARCH); err != nil {
		t.Fatal(err)
	}
	if err := CheckMounted(mount, []string{"bin"}, otherArch()); failureReason(t, err) != ReasonArchMismatch {
		t.Fatal("expected architecture mismatch")
	}
	if err := VerifyMounted(mount, []string{"missing"}); failureReason(t, err) != ReasonMissingPathEntry {
		t.Fatal("expected missing path entry")
	}
	if err := VerifyMounted(filepath.Join(root, "nope"), nil); failureReason(t, err) != ReasonMissingMount {
		t.Fatal("expected missing mount")
	}
	link := filepath.Join(root, "opt", "link")
	if err := os.Symlink(mount, link); err != nil {
		t.Fatal(err)
	}
	if err := VerifyMounted(link, nil); failureReason(t, err) != ReasonMissingMount {
		t.Fatal("a symlinked mount path must be rejected")
	}
	if err := CheckMounted(link, []string{"bin"}, runtime.GOARCH); failureReason(t, err) != ReasonMissingMount {
		t.Fatal("check must not follow a symlinked mount path")
	}
}

func TestCheckMountedBoundsDirectoryEntries(t *testing.T) {
	root := realTempDir(t)
	mount := filepath.Join(root, "opt", "tools")
	for i := range 5 {
		mustWrite(t, filepath.Join(mount, "bin", "tool"+strconv.Itoa(i)), []byte("#!/bin/sh\n"), 0o755)
	}
	previous := checkArchEntryLimit
	checkArchEntryLimit = 3
	t.Cleanup(func() { checkArchEntryLimit = previous })
	if err := CheckMounted(mount, []string{"bin"}, runtime.GOARCH); failureReason(t, err) != ReasonTooManyEntries {
		t.Fatalf("expected %s, got %v", ReasonTooManyEntries, err)
	}
	checkArchEntryLimit = previous
	if err := CheckMounted(mount, []string{"bin"}, runtime.GOARCH); err != nil {
		t.Fatal(err)
	}
}

func TestMountedToolboxMustBeWorldAccessible(t *testing.T) {
	root := realTempDir(t)
	mount := filepath.Join(root, "opt", "tools")
	mustWrite(t, filepath.Join(mount, "bin", "tool"), elfFor(runtime.GOARCH), 0o755)
	if err := os.Chmod(filepath.Join(mount, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := VerifyMounted(mount, []string{"bin"}); failureReason(t, err) != ReasonPermissionDenied {
		t.Fatalf("a 0700 path entry must fail: %v", err)
	}
	if err := CheckMounted(mount, []string{"bin"}, runtime.GOARCH); failureReason(t, err) != ReasonPermissionDenied {
		t.Fatalf("check must fail on a 0700 path entry: %v", err)
	}
	if err := os.Chmod(filepath.Join(mount, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(mount, "bin", "tool"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := CheckMounted(mount, []string{"bin"}, runtime.GOARCH); failureReason(t, err) != ReasonPermissionDenied {
		t.Fatalf("a tool without o+rx must fail: %v", err)
	}
	if err := os.Chmod(filepath.Join(mount, "bin", "tool"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CheckMounted(mount, []string{"bin"}, runtime.GOARCH); err != nil {
		t.Fatal(err)
	}
	// Executable scripts need the same bits; plain data files do not.
	mustWrite(t, filepath.Join(mount, "bin", "script"), []byte("#!/bin/sh\n"), 0o744)
	if err := CheckMounted(mount, []string{"bin"}, runtime.GOARCH); failureReason(t, err) != ReasonPermissionDenied {
		t.Fatalf("a 0744 script must fail: %v", err)
	}
	if err := os.Chmod(filepath.Join(mount, "bin", "script"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(mount, "bin", "notes.txt"), []byte("data"), 0o640)
	if err := CheckMounted(mount, []string{"bin"}, runtime.GOARCH); err != nil {
		t.Fatalf("a non-executable data file must not be checked: %v", err)
	}
	if err := os.Chmod(mount, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := VerifyMounted(mount, nil); failureReason(t, err) != ReasonPermissionDenied {
		t.Fatalf("a 0750 mount root must fail: %v", err)
	}
}

func TestHandoffPublishesExecutableCopy(t *testing.T) {
	dir := realTempDir(t)
	final, err := Handoff(dir)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(final)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode() != 0o555 || filepath.Base(final) != HandoffBinaryName {
		t.Fatalf("handoff file = %s mode %v", final, info.Mode())
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.Stat(self)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != want.Size() {
		t.Fatalf("handoff size %d != executable size %d", info.Size(), want.Size())
	}
	if _, err := Handoff(dir); err != nil {
		t.Fatalf("second handoff: %v", err)
	}
}

func TestRunSubcommands(t *testing.T) {
	source, destination := newLayout(t)
	mustWrite(t, filepath.Join(source, "bin", "tool"), elfFor(runtime.GOARCH), 0o755)
	var stdout, stderr bytes.Buffer
	if code := Run(CopySubcommand, []string{"--src", source, "--dst", destination, "--path-entries", "bin"}, &stdout, &stderr); code != 0 {
		t.Fatalf("copy exit %d: %s", code, stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "OK entries=") {
		t.Fatalf("unexpected stdout %q", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run(CheckSubcommand, []string{"--mount", MountFlagValue(filepath.Join(destination, OutputRootName), []string{"bin"})}, &stdout, &stderr); code != 0 {
		t.Fatalf("check exit %d: %s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run(CopySubcommand, []string{"--src", source, "--dst", realTempDir(t), "--arch", otherArch(), "--path-entries", "bin"}, &stdout, &stderr); code != 1 {
		t.Fatalf("mismatch exit %d", code)
	}
	failure, ok := ParseFailureLine(stderr.String())
	if !ok || failure.Reason != ReasonArchMismatch {
		t.Fatalf("stderr %q did not carry the stable failure line", stderr.String())
	}
	stderr.Reset()
	if code := Run(CopySubcommand, []string{"--dst", destination}, &stdout, &stderr); code != 1 {
		t.Fatal("missing --src must fail")
	}
	if failure, ok := ParseFailureLine(stderr.String()); !ok || failure.Reason != ReasonInvalidArguments {
		t.Fatalf("stderr %q", stderr.String())
	}
	stderr.Reset()
	handoff := realTempDir(t)
	if code := Run(HandoffSubcommand, []string{"--dst", handoff}, &stdout, &stderr); code != 0 {
		t.Fatalf("handoff exit %d: %s", code, stderr.String())
	}
	if code := Run("toolbox-unknown", nil, &stdout, &stderr); code != 1 {
		t.Fatal("unknown subcommand must fail")
	}
	if IsSubcommand("toolbox-unknown") || !IsSubcommand(CopySubcommand) {
		t.Fatal("IsSubcommand mismatch")
	}
}

func TestParseFailureLine(t *testing.T) {
	line := FormatFailureLine(&Failure{Reason: ReasonTooDeep, Message: "a\nb"})
	if line != "FAIL reason=TOOLBOX_TOO_DEEP msg=a b" {
		t.Fatalf("line = %q", line)
	}
	failure, ok := ParseFailureLine("noise\nFAIL reason=TOOLBOX_SOURCE_OPEN msg=first\nmore\n" + line + "\n")
	if !ok || failure.Reason != ReasonTooDeep || failure.Message != "a b" {
		t.Fatalf("parsed %+v ok=%v", failure, ok)
	}
	if _, ok := ParseFailureLine("exit status 1"); ok {
		t.Fatal("plain text must not parse")
	}
}

// FuzzCopyTree builds a pseudo-random tree from the fuzz input and checks the
// copier's invariants: it never fails on a tree made only of folders, regular
// files, and symlinks, every copied file has a sanitized mode, and the entry
// count matches the source.
func FuzzCopyTree(f *testing.F) {
	f.Add([]byte{1, 2, 3, 4, 5, 6, 7, 8})
	f.Add([]byte{0})
	f.Add([]byte{255, 254, 253, 252, 251, 250, 249, 248, 247, 246})
	f.Fuzz(func(t *testing.T, seed []byte) {
		if len(seed) > 64 {
			seed = seed[:64]
		}
		source, destination := newLayout(t)
		entries := int64(0)
		dirs := []string{source}
		for i, b := range seed {
			parent := dirs[int(b)%len(dirs)]
			name := "e" + strings.Repeat("x", int(b)%5) + string(rune('a'+i%26))
			path := filepath.Join(parent, name)
			if _, err := os.Lstat(path); err == nil {
				continue
			}
			switch b % 4 {
			case 0:
				if filepath.Dir(path) != source && strings.Count(strings.TrimPrefix(path, source), "/") > 6 {
					continue
				}
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				dirs = append(dirs, path)
			case 1:
				if err := os.Symlink(strings.Repeat("../", int(b)%3)+"target", path); err != nil {
					t.Fatal(err)
				}
			default:
				mode := os.FileMode(0o600)
				if b%8 >= 4 {
					mode = 0o4711
				}
				mustWrite(t, path, bytes.Repeat([]byte{b}, int(b)%40), mode)
			}
			entries++
		}
		summary, err := Copy(copyOptions(source, destination))
		if err != nil {
			t.Fatalf("copy failed: %v", err)
		}
		if summary.Entries != entries {
			t.Fatalf("entries = %d, want %d", summary.Entries, entries)
		}
		root := filepath.Join(destination, OutputRootName)
		if err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if path == root {
				return nil
			}
			switch {
			case info.Mode()&os.ModeSymlink != 0:
			case info.IsDir():
				if info.Mode().Perm() != 0o755 {
					t.Fatalf("%s dir mode %v", path, info.Mode())
				}
			default:
				if perm := info.Mode().Perm(); perm != 0o555 && perm != 0o444 {
					t.Fatalf("%s file mode %v", path, info.Mode())
				}
				if info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
					t.Fatalf("%s kept special bits %v", path, info.Mode())
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
}
