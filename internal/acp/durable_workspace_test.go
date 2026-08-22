/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package acp

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareDurableSessionWorkspaceLifecycle(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	workspaceDir, committed, err := PrepareDurableSessionWorkspace(root, "session-uid-1")
	if err != nil {
		t.Fatalf("prepare fresh: %v", err)
	}
	if committed != nil {
		t.Fatalf("fresh preparation reported committed content: %+v", committed)
	}
	if filepath.Dir(workspaceDir) != root {
		t.Fatalf("workspace dir = %q, want a child of the durable root", workspaceDir)
	}
	if err := os.WriteFile(filepath.Join(workspaceDir, "marker.txt"), []byte("durable"), 0o600); err != nil {
		t.Fatalf("write workspace content: %v", err)
	}

	// Uncommitted content (no marker) is wiped by the next preparation.
	again, committed, err := PrepareDurableSessionWorkspace(root, "session-uid-1")
	if err != nil {
		t.Fatalf("prepare after uncommitted content: %v", err)
	}
	if committed != nil {
		t.Fatal("uncommitted content must never resume")
	}
	if _, err := os.Stat(filepath.Join(again, "marker.txt")); !os.IsNotExist(err) {
		t.Fatalf("uncommitted content survived preparation: %v", err)
	}

	binding := DurableWorkspaceBinding{RepositoryIdentity: "github.com/example/repo", Revision: "abc123"}
	if err := os.WriteFile(filepath.Join(again, "marker.txt"), []byte("durable"), 0o600); err != nil {
		t.Fatalf("write workspace content: %v", err)
	}
	if err := CommitDurableSessionWorkspace(root, "session-uid-1", binding); err != nil {
		t.Fatalf("commit: %v", err)
	}

	resumedDir, resumed, err := PrepareDurableSessionWorkspace(root, "session-uid-1")
	if err != nil {
		t.Fatalf("prepare committed: %v", err)
	}
	if resumed == nil || *resumed != binding {
		t.Fatalf("resumed binding = %+v, want %+v", resumed, binding)
	}
	if resumedDir != again {
		t.Fatalf("resumed dir = %q, want the committed dir %q", resumedDir, again)
	}
	content, err := os.ReadFile(filepath.Join(resumedDir, "marker.txt"))
	if err != nil || string(content) != "durable" {
		t.Fatalf("committed content = %q err=%v, want it preserved across preparation", content, err)
	}

	// Distinct logical sessions never share a durable directory.
	otherDir, otherCommitted, err := PrepareDurableSessionWorkspace(root, "session-uid-2")
	if err != nil {
		t.Fatalf("prepare second session: %v", err)
	}
	if otherCommitted != nil || otherDir == resumedDir {
		t.Fatalf("second session dir = %q committed=%v", otherDir, otherCommitted)
	}
}

func TestPrepareDurableSessionWorkspaceRejectsInvalidInput(t *testing.T) {
	t.Parallel()
	if _, _, err := PrepareDurableSessionWorkspace("relative/root", "session"); err == nil {
		t.Fatal("relative durable root must be rejected")
	}
	if _, _, err := PrepareDurableSessionWorkspace(t.TempDir(), "../escape"); err == nil {
		t.Fatal("path-escaping session component must be rejected")
	}
	if err := CommitDurableSessionWorkspace("relative", "session", DurableWorkspaceBinding{}); err == nil {
		t.Fatal("relative durable root must be rejected on commit")
	}
}

func TestPrepareDurableSessionWorkspaceRejectsMarkerWithoutDirectory(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if _, _, err := PrepareDurableSessionWorkspace(root, "session-uid-1"); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := CommitDurableSessionWorkspace(root, "session-uid-1", DurableWorkspaceBinding{
		RepositoryIdentity: "github.com/example/repo", Revision: "abc123",
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(root, "ws-session-uid-1")); err != nil {
		t.Fatalf("remove workspace dir: %v", err)
	}
	if _, _, err := PrepareDurableSessionWorkspace(root, "session-uid-1"); err == nil {
		t.Fatal("a marker without its workspace directory must fail closed")
	}
}
