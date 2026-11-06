package acp

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestRecordWorkspaceIdentity(t *testing.T) {
	root := t.TempDir()
	paths := SessionPaths{Workspace: filepath.Join(root, "workspace")}
	if err := os.Mkdir(paths.Workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	err := RecordWorkspaceIdentity(&paths)
	if runtime.GOOS == "windows" {
		if err == nil || paths.WorkspaceInode != 0 {
			t.Fatal("identity pinning must refuse where it is unsupported")
		}
		return
	}
	if err != nil || paths.WorkspaceInode == 0 {
		t.Fatalf("RecordWorkspaceIdentity = %v, inode %d", err, paths.WorkspaceInode)
	}
	file := SessionPaths{Workspace: filepath.Join(root, "file")}
	if err := os.WriteFile(file.Workspace, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if RecordWorkspaceIdentity(&file) == nil {
		t.Fatal("a non-directory workspace must be refused")
	}
	link := SessionPaths{Workspace: filepath.Join(root, "link")}
	if err := os.Symlink(paths.Workspace, link.Workspace); err != nil {
		t.Fatal(err)
	}
	if RecordWorkspaceIdentity(&link) == nil {
		t.Fatal("a symbolic link workspace must be refused")
	}
}
