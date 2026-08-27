/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package acp

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/orka-agents/orka/internal/taskterminal"
)

const (
	testDurableContent    = "durable"
	testDurableRepository = "github.com/example/repo"
	testDurableRevision   = "abc123"
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
	if err := os.WriteFile(filepath.Join(workspaceDir, "marker.txt"), []byte(testDurableContent), 0o600); err != nil {
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

	binding := DurableWorkspaceBinding{RepositoryIdentity: testDurableRepository, Revision: testDurableRevision}
	if err := os.WriteFile(filepath.Join(again, "marker.txt"), []byte(testDurableContent), 0o600); err != nil {
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
	if err != nil || string(content) != testDurableContent {
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

func TestStableDurableWorkspaceIdentity(t *testing.T) {
	t.Parallel()
	noWorkspace := taskterminal.NoWorkspaceRevision
	if got := StableDurableWorkspaceIdentity(noWorkspace+":task-a", noWorkspace); got != noWorkspace {
		t.Fatalf("no-workspace identity = %q, want %q", got, noWorkspace)
	}
	// Two no-repository continuations with different Task-scoped identities
	// reduce to the same stable identity.
	first := StableDurableWorkspaceIdentity(noWorkspace+":task-a", noWorkspace)
	second := StableDurableWorkspaceIdentity(noWorkspace+":task-b", noWorkspace)
	if first != second {
		t.Fatalf("task-scoped no-workspace identities must reduce equally: %q vs %q", first, second)
	}
	// Repository workspaces are identified by the repository identity alone;
	// verified revision advances stay within the same stable identity.
	if StableDurableWorkspaceIdentity(testDurableRepository, "abc") !=
		StableDurableWorkspaceIdentity(testDurableRepository, "def") {
		t.Fatal("revision advance must not change the stable repository identity")
	}
	if StableDurableWorkspaceIdentity(testDurableRepository, "abc") ==
		StableDurableWorkspaceIdentity("github.com/example/other", "abc") {
		t.Fatal("distinct repositories must never share a stable identity")
	}
}

// A resume marks its tree pending before initialization; an interrupted
// creation must wipe the tree, while a recommit preserves it.
func TestDurableSessionWorkspaceResumePendingLifecycle(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	binding := DurableWorkspaceBinding{RepositoryIdentity: testDurableRepository, Revision: testDurableRevision}
	dir, _, err := PrepareDurableSessionWorkspace(root, "session-pending-1")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.txt"), []byte(testDurableContent), 0o600); err != nil {
		t.Fatalf("write content: %v", err)
	}
	if err := CommitDurableSessionWorkspace(root, "session-pending-1", binding); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Resume marks pending, then recommits: the tree survives.
	if err := MarkDurableSessionWorkspaceResumePending(root, "session-pending-1"); err != nil {
		t.Fatalf("mark pending: %v", err)
	}
	if err := CommitDurableSessionWorkspace(root, "session-pending-1", binding); err != nil {
		t.Fatalf("recommit: %v", err)
	}
	if _, resumed, err := PrepareDurableSessionWorkspace(root, "session-pending-1"); err != nil || resumed == nil {
		t.Fatalf("recommitted tree must resume, got (%v, %v)", resumed, err)
	}
	if content, err := os.ReadFile(filepath.Join(dir, "state.txt")); err != nil || string(content) != testDurableContent {
		t.Fatalf("recommitted content = %q err=%v, want it preserved", content, err)
	}

	// Resume marks pending and the creation dies: the next preparation wipes
	// the tree instead of reusing possibly-modified state.
	if err := MarkDurableSessionWorkspaceResumePending(root, "session-pending-1"); err != nil {
		t.Fatalf("mark pending again: %v", err)
	}
	freshDir, resumed, err := PrepareDurableSessionWorkspace(root, "session-pending-1")
	if err != nil {
		t.Fatalf("prepare after interrupted resume: %v", err)
	}
	if resumed != nil {
		t.Fatalf("an interrupted resume must never report committed content, got %+v", resumed)
	}
	if _, err := os.Stat(filepath.Join(freshDir, "state.txt")); !os.IsNotExist(err) {
		t.Fatalf("interrupted-resume content survived the wipe: %v", err)
	}
}

func TestMarkDurableSessionWorkspaceResumePendingRestoresMarkerWhenSyncFails(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	const sessionUID = "session-pending-sync-error"
	binding := DurableWorkspaceBinding{RepositoryIdentity: testDurableRepository, Revision: testDurableRevision}
	workspaceDir, _, err := PrepareDurableSessionWorkspace(root, sessionUID)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspaceDir, "state.txt"), []byte(testDurableContent), 0o600); err != nil {
		t.Fatalf("write content: %v", err)
	}
	if err := CommitDurableSessionWorkspace(root, sessionUID, binding); err != nil {
		t.Fatalf("commit: %v", err)
	}

	injected := errors.New("injected directory sync failure")
	syncCalls := 0
	err = markDurableSessionWorkspaceResumePending(root, sessionUID, func(string) error {
		syncCalls++
		if syncCalls == 1 {
			return injected
		}
		return nil
	})
	if !errors.Is(err, injected) {
		t.Fatalf("mark pending error = %v, want injected sync failure", err)
	}
	if syncCalls != 2 {
		t.Fatalf("directory sync calls = %d, want failed publish plus rollback sync", syncCalls)
	}
	if _, err := os.Lstat(durableWorkspacePendingMarkerPath(root, sessionUID)); !os.IsNotExist(err) {
		t.Fatalf("pending marker remained after rollback: %v", err)
	}
	resumedDir, resumed, err := PrepareDurableSessionWorkspace(root, sessionUID)
	if err != nil || resumed == nil || *resumed != binding {
		t.Fatalf("restored checkpoint = (%+v, %v), want committed binding", resumed, err)
	}
	if content, err := os.ReadFile(filepath.Join(resumedDir, "state.txt")); err != nil || string(content) != testDurableContent {
		t.Fatalf("restored content = %q err=%v, want preserved checkpoint", content, err)
	}
}

// A verified publication transition wipes the tree so the continuation
// re-materializes from the newly declared baseline.
func TestWipeDurableSessionWorkspaceClearsTreeAndMarkers(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir, _, err := PrepareDurableSessionWorkspace(root, "session-wipe-1")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.txt"), []byte(testDurableContent), 0o600); err != nil {
		t.Fatalf("write content: %v", err)
	}
	if err := CommitDurableSessionWorkspace(root, "session-wipe-1", DurableWorkspaceBinding{
		RepositoryIdentity: "github.com/example/source", Revision: "abc",
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := WipeDurableSessionWorkspace(root, "session-wipe-1"); err != nil {
		t.Fatalf("wipe: %v", err)
	}
	freshDir, resumed, err := PrepareDurableSessionWorkspace(root, "session-wipe-1")
	if err != nil || resumed != nil {
		t.Fatalf("wiped session must prepare fresh, got (%v, %v)", resumed, err)
	}
	if _, err := os.Stat(filepath.Join(freshDir, "state.txt")); !os.IsNotExist(err) {
		t.Fatalf("wiped content survived: %v", err)
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
		RepositoryIdentity: testDurableRepository, Revision: testDurableRevision,
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

// A crash between the commit rename and the pending-marker retirement leaves
// both markers on disk. The committed marker proves the commit completed, so
// the next preparation must retire the stale pending marker and resume the
// tree instead of wiping it.
func TestPrepareDurableSessionWorkspaceSurvivesInterruptedCommitRetirement(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sessionUID := "session-uid-commit-crash"

	workspaceDir, _, err := PrepareDurableSessionWorkspace(root, sessionUID)
	if err != nil {
		t.Fatalf("prepare fresh: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspaceDir, "marker.txt"), []byte(testDurableContent), 0o600); err != nil {
		t.Fatalf("write workspace content: %v", err)
	}
	binding := DurableWorkspaceBinding{RepositoryIdentity: testDurableRepository, Revision: testDurableRevision}
	if err := CommitDurableSessionWorkspace(root, sessionUID, binding); err != nil {
		t.Fatalf("commit: %v", err)
	}
	// Simulate the crash window: a resume marked the tree pending, the
	// recommit renamed the fresh marker into place, and the process died
	// before removing the pending marker.
	if err := os.WriteFile(durableWorkspacePendingMarkerPath(root, sessionUID), []byte("{}"), 0o600); err != nil {
		t.Fatalf("materialize stale pending marker: %v", err)
	}

	resumedDir, resumed, err := PrepareDurableSessionWorkspace(root, sessionUID)
	if err != nil {
		t.Fatalf("prepare after interrupted commit retirement: %v", err)
	}
	if resumed == nil || *resumed != binding {
		t.Fatalf("resumed binding = %+v, want the committed binding preserved", resumed)
	}
	if content, err := os.ReadFile(filepath.Join(resumedDir, "marker.txt")); err != nil || string(content) != testDurableContent {
		t.Fatalf("committed content = %q err=%v, want it preserved", content, err)
	}
	if _, err := os.Lstat(durableWorkspacePendingMarkerPath(root, sessionUID)); !os.IsNotExist(err) {
		t.Fatalf("stale pending marker must be retired, err=%v", err)
	}
}

// A repository-identity transition stages its authorization durably before
// the old checkpoint is wiped, survives a mid-transition crash for the retry
// to read, and is retired by the next successful commit.
func TestDurableWorkspaceTransitionLifecycle(t *testing.T) {
	root := t.TempDir()
	target := DurableWorkspaceBinding{RepositoryIdentity: "github.com/o/fork", Revision: "def", SessionGeneration: 4}
	if err := MarkDurableWorkspaceTransitionAuthorized(root, "session-uid-t", target); err != nil {
		t.Fatalf("stage transition: %v", err)
	}
	read, err := DurableWorkspaceTransitionTarget(root, "session-uid-t")
	if err != nil || read == nil || read.RepositoryIdentity != target.RepositoryIdentity || read.SessionGeneration != 4 {
		t.Fatalf("transition target = %+v err=%v, want the staged record", read, err)
	}
	if _, _, err := PrepareDurableSessionWorkspace(root, "session-uid-t"); err != nil {
		t.Fatalf("prepare after staged transition: %v", err)
	}
	if err := CommitDurableSessionWorkspace(root, "session-uid-t", target); err != nil {
		t.Fatalf("commit: %v", err)
	}
	read, err = DurableWorkspaceTransitionTarget(root, "session-uid-t")
	if err != nil {
		t.Fatalf("re-read transition target: %v", err)
	}
	if read != nil {
		t.Fatal("a successful commit must retire the staged transition record")
	}
}
