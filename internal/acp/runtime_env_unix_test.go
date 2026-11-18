//go:build darwin || linux

package acp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestSessionDirectoriesWithPrivateUmask(t *testing.T) {
	if os.Getenv("ORKA_PRIVATE_UMASK_TEST") != "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSessionDirectoriesWithPrivateUmask$", "-test.timeout=20s")
		command.Env = append(os.Environ(), "ORKA_PRIVATE_UMASK_TEST=1")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("private umask subprocess: %v\n%s", err, output)
		}
		return
	}
	// The production supervisor hardens its process before creating directories.
	// Keep that process-wide setting out of the main test process.
	previous := unix.Umask(0o077)
	defer unix.Umask(previous)
	for _, tc := range []struct {
		name string
		mode os.FileMode
	}{
		{name: "new"},
		{name: "identity-initialized", mode: 0o700},
		{name: "writable-volume-root", mode: 0o777},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := filepath.Join(t.TempDir(), "sessions")
			if tc.mode != 0 {
				// Identity-state initialization creates this directory before
				// the first session; an existing volume may instead be writable.
				if err := os.Mkdir(base, tc.mode); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(base, tc.mode); err != nil {
					t.Fatal(err)
				}
			}
			paths, err := PrepareSessionPaths(base, "session")
			if err != nil {
				t.Fatal(err)
			}
			assertDirectoryMode(t, base, 0o711)
			for _, path := range []string{paths.Root, paths.Home, paths.Temp, paths.Workspace, paths.Config, paths.Cache, paths.Data, paths.State} {
				assertDirectoryMode(t, path, 0o700)
			}
		})
	}
	t.Run("durable", func(t *testing.T) {
		base := filepath.Join(t.TempDir(), "durable")
		workspace, _, err := PrepareDurableSessionWorkspace(base, "session", 1)
		if err != nil {
			t.Fatal(err)
		}
		assertDirectoryMode(t, base, 0o711)
		assertDirectoryMode(t, workspace, 0o700)
	})
}

func assertDirectoryMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode().Perm() != want {
		t.Fatalf("%s mode = %v, want directory %o", path, info.Mode(), want)
	}
}
