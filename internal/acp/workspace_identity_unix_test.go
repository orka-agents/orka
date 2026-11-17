//go:build unix

package acp

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func directoryInode(t *testing.T, path string) uint64 {
	t.Helper()
	var stat syscall.Stat_t
	if err := syscall.Lstat(path, &stat); err != nil {
		t.Fatal(err)
	}
	return uint64(stat.Ino) //nolint:unconvert // Ino width differs across Unix platforms.
}

// recreateWithInode deletes nothing itself: it creates directories under root
// until one is given the wanted inode number, and reports whether one was.
func recreateWithInode(t *testing.T, root, prefix string, want uint64) (string, bool) {
	t.Helper()
	for attempt := range 64 {
		dir := filepath.Join(root, fmt.Sprintf("%s-%d", prefix, attempt))
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if directoryInode(t, dir) == want {
			return dir, true
		}
	}
	return "", false
}

// A device and inode pair names a directory only while the inode stays
// allocated. The session child owns its workspace, so it can delete it and
// create directories until one is given the recorded number. The identity must
// therefore hold the directory open.
func TestRecordWorkspaceIdentityHoldsTheDirectoryAgainstInodeReuse(t *testing.T) {
	root := t.TempDir()
	probe := filepath.Join(root, "probe")
	if err := os.Mkdir(probe, 0o700); err != nil {
		t.Fatal(err)
	}
	probeInode := directoryInode(t, probe)
	if err := os.Remove(probe); err != nil {
		t.Fatal(err)
	}
	if _, reused := recreateWithInode(t, root, "probe", probeInode); !reused {
		t.Skip("this filesystem does not hand a deleted directory's inode number to the next one")
	}

	paths := SessionPaths{Workspace: filepath.Join(root, "workspace")}
	if err := os.Mkdir(paths.Workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := RecordWorkspaceIdentity(&paths); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(paths.ReleaseWorkspaceIdentity)
	copied := paths
	if err := os.Remove(paths.Workspace); err != nil {
		t.Fatal(err)
	}
	if dir, reused := recreateWithInode(t, root, "replacement", paths.WorkspaceInode); reused {
		t.Fatalf("%s was given the recorded workspace inode %d while the identity was held", dir, paths.WorkspaceInode)
	}
	if _, ino, err := copied.PinnedWorkspaceIdentity(); err != nil || ino != paths.WorkspaceInode {
		t.Fatalf("PinnedWorkspaceIdentity = %d, %v; want the recorded inode %d", ino, err, paths.WorkspaceInode)
	}

	paths.ReleaseWorkspaceIdentity()
	if _, _, err := copied.PinnedWorkspaceIdentity(); err == nil {
		t.Fatal("a released identity must be refused: its inode number may already name another directory")
	}
	if _, _, err := (SessionPaths{WorkspaceDevice: paths.WorkspaceDevice, WorkspaceInode: paths.WorkspaceInode}).PinnedWorkspaceIdentity(); err == nil {
		t.Fatal("recorded numbers without a held directory must be refused")
	}
}
