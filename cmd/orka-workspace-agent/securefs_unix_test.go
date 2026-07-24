//go:build !windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestSecureWriteFileRejectsFIFOWithoutBlocking(t *testing.T) {
	root := t.TempDir()
	previousAllowedRoots := allowedRoots
	allowedRoots = []string{root}
	t.Cleanup(func() { allowedRoots = previousAllowedRoots })

	path := filepath.Join(root, "fifo")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("create FIFO: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := secureWriteFile(path, []byte("data"), 0o600, false, 0, 0, time.Time{})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("secureWriteFile accepted FIFO")
		}
	case <-time.After(time.Second):
		t.Fatal("secureWriteFile blocked opening FIFO")
	}
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("FIFO changed: info=%v err=%v", info, err)
	}
}

func TestSecureReadFileRejectsHardLinkedAlias(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	outside := filepath.Join(parent, "outside")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("create workspace root: %v", err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("create outside root: %v", err)
	}
	protected := filepath.Join(outside, "protected")
	alias := filepath.Join(root, "alias")
	if err := os.WriteFile(protected, []byte("protected"), 0o600); err != nil {
		t.Fatalf("write protected file: %v", err)
	}
	if err := os.Link(protected, alias); err != nil {
		t.Fatalf("create hard link: %v", err)
	}

	previousAllowedRoots := allowedRoots
	allowedRoots = []string{root}
	t.Cleanup(func() { allowedRoots = previousAllowedRoots })

	if _, _, err := secureReadFile(alias, defaultMaxDownloadBytes); err == nil {
		t.Fatal("secureReadFile accepted a hard-linked alias")
	}
}

func TestSecureReadFileRejectsOversizedSparseFile(t *testing.T) {
	root := t.TempDir()
	previousAllowedRoots := allowedRoots
	allowedRoots = []string{root}
	t.Cleanup(func() { allowedRoots = previousAllowedRoots })
	path := filepath.Join(root, "sparse")
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create sparse file: %v", err)
	}
	if err := file.Truncate(1 << 30); err != nil {
		_ = file.Close()
		t.Fatalf("truncate sparse file: %v", err)
	}
	_ = file.Close()
	if _, _, err := secureReadFile(path, 1024); !errors.Is(err, errWorkspaceFileTooLarge) {
		t.Fatalf("oversized sparse read error = %v, want %v", err, errWorkspaceFileTooLarge)
	}
}

func TestSecureResetDirectoryUsesSpecialModeOnlyForExactTmpRoot(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "orka-secure-reset-")
	if err != nil {
		t.Fatalf("create nested tmp directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	previousAllowedRoots := allowedRoots
	allowedRoots = []string{"/tmp"}
	t.Cleanup(func() { allowedRoots = previousAllowedRoots })

	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("set initial mode: %v", err)
	}
	if err := secureResetDirectory(dir, false, 0, 0, nil); err != nil {
		t.Fatalf("reset nested tmp directory: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat nested tmp directory: %v", err)
	}
	if got := info.Mode().Perm() | info.Mode()&os.ModeSticky; got != 0o755 {
		t.Fatalf("nested tmp mode = %o, want 755", got)
	}
}

func TestSecureListFilesBoundsEntryCount(t *testing.T) {
	root := t.TempDir()
	previousAllowedRoots := allowedRoots
	allowedRoots = []string{root}
	t.Cleanup(func() { allowedRoots = previousAllowedRoots })
	for i := 0; i <= maxWorkspaceTreeEntries; i++ {
		path := filepath.Join(root, fmt.Sprintf("entry-%05d", i))
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatalf("write entry %d: %v", i, err)
		}
	}
	if _, err := secureListFiles(root); !errors.Is(err, errWorkspaceTreeTooLarge) {
		t.Fatalf("large tree error = %v, want %v", err, errWorkspaceTreeTooLarge)
	}
	if err := secureRemoveAllContext(context.Background(), root); !errors.Is(err, errWorkspaceTreeTooLarge) {
		t.Fatalf("large cleanup error = %v, want %v", err, errWorkspaceTreeTooLarge)
	}
}

func TestSecureCleanupHonorsCancellation(t *testing.T) {
	root := t.TempDir()
	previousAllowedRoots := allowedRoots
	allowedRoots = []string{root}
	t.Cleanup(func() { allowedRoots = previousAllowedRoots })
	if err := os.WriteFile(filepath.Join(root, "data"), []byte("data"), 0o600); err != nil {
		t.Fatalf("write cleanup data: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := secureRemoveAllContext(ctx, root); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled cleanup error = %v, want %v", err, context.Canceled)
	}
}

func TestSecureTraversalBoundsDepth(t *testing.T) {
	root := t.TempDir()
	previousAllowedRoots := allowedRoots
	allowedRoots = []string{root}
	t.Cleanup(func() { allowedRoots = previousAllowedRoots })
	current := root
	for i := 0; i <= maxWorkspaceTreeDepth; i++ {
		current = filepath.Join(current, "nested")
		if err := os.Mkdir(current, 0o700); err != nil {
			t.Fatalf("create depth %d: %v", i, err)
		}
	}
	if err := os.WriteFile(filepath.Join(current, "leaf"), nil, 0o600); err != nil {
		t.Fatalf("write deep leaf: %v", err)
	}
	if _, err := secureListFiles(root); !errors.Is(err, errWorkspaceTreeTooDeep) {
		t.Fatalf("deep list error = %v, want %v", err, errWorkspaceTreeTooDeep)
	}
	if err := secureRemoveAll(filepath.Join(root, "nested")); !errors.Is(err, errWorkspaceTreeTooDeep) {
		t.Fatalf("deep remove error = %v, want %v", err, errWorkspaceTreeTooDeep)
	}
}

func TestResetRootMetadataRestoresSafeModes(t *testing.T) {
	root := t.TempDir()
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	defer unix.Close(fd) //nolint:errcheck
	if err := unix.Fchmod(fd, 0); err != nil {
		t.Fatalf("poison root mode: %v", err)
	}
	if err := resetRootMetadata(fd, root, false, 0, 0); err != nil {
		t.Fatalf("reset root metadata: %v", err)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		t.Fatalf("stat root: %v", err)
	}
	if stat.Mode&0o7777 != 0o755 {
		t.Fatalf("restored root mode = %o, want 755", stat.Mode&0o7777)
	}
	if err := resetRootMetadata(fd, "/tmp", false, 0, 0); err != nil {
		t.Fatalf("reset tmp metadata: %v", err)
	}
	if err := unix.Fstat(fd, &stat); err != nil {
		t.Fatalf("stat tmp policy: %v", err)
	}
	if stat.Mode&0o7777 != 0o1777 {
		t.Fatalf("restored tmp mode = %o, want 1777", stat.Mode&0o7777)
	}
	if err := resetRootMetadata(fd, "/dev/shm", false, 0, 0); err != nil {
		t.Fatalf("reset shared-memory metadata: %v", err)
	}
	if err := unix.Fstat(fd, &stat); err != nil {
		t.Fatalf("stat shared-memory policy: %v", err)
	}
	if stat.Mode&0o7777 != 0o1777 {
		t.Fatalf("restored shared-memory mode = %o, want 1777", stat.Mode&0o7777)
	}
}

func TestWorkspaceFileErrorStatus(t *testing.T) {
	if got := workspaceFileErrorStatus(unix.ELOOP); got != 400 {
		t.Fatalf("ELOOP status = %d, want 400", got)
	}
	if got := workspaceFileErrorStatus(unix.ENOSPC); got != 500 {
		t.Fatalf("ENOSPC status = %d, want 500", got)
	}
}

func TestSecureRemoveAllClearsAllowedRoot(t *testing.T) {
	root := t.TempDir()
	previousAllowedRoots := allowedRoots
	allowedRoots = []string{root}
	t.Cleanup(func() { allowedRoots = previousAllowedRoots })

	child := filepath.Join(root, "child")
	if err := os.WriteFile(child, []byte("data"), 0o600); err != nil {
		t.Fatalf("write child: %v", err)
	}
	if err := secureRemoveAll(root); err != nil {
		t.Fatalf("secureRemoveAll(root): %v", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("allowed root was removed: %v", err)
	}
	if _, err := os.Stat(child); !os.IsNotExist(err) {
		t.Fatalf("root child remains: %v", err)
	}
}
