//go:build unix

package supervisor

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/orka-agents/orka/internal/acp"
)

// Managed runtime Pods run the supervisor without CAP_DAC_OVERRIDE, so once the
// session root is handed to the child (0700, child-owned) the supervisor can no
// longer traverse it. The workspace identity operator copies rely on must be
// pinned before that handoff, for every session creation including native
// restores and continuations.
func TestCreateSessionPinsWorkspaceIdentityBeforeOwnershipHandoff(t *testing.T) {
	previousRecord, previousFinalize := recordSessionWorkspaceIdentity, finalizeSessionOwnership
	t.Cleanup(func() { recordSessionWorkspaceIdentity, finalizeSessionOwnership = previousRecord, previousFinalize })
	var handedOff atomic.Bool
	finalizeSessionOwnership = func(root string, uid, gid int) error {
		handedOff.Store(true)
		return previousFinalize(root, uid, gid)
	}
	recordSessionWorkspaceIdentity = func(paths *acp.SessionPaths) error {
		if handedOff.Load() {
			return fmt.Errorf("inspect session workspace: %w", os.ErrPermission)
		}
		return previousRecord(paths)
	}
	server, cfg, profile := newTestServer(t, "immediate")
	create := testCreateSessionRequest(t, cfg, profile)
	created := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", created.Code, created.Body.String())
	}
	server.mu.Lock()
	paths := server.sessions[create.RuntimeSessionID].paths
	server.mu.Unlock()
	if paths.WorkspaceInode == 0 {
		t.Fatal("the session has no pinned workspace identity")
	}
	var stat syscall.Stat_t
	if err := syscall.Stat(paths.Workspace, &stat); err != nil || stat.Ino != paths.WorkspaceInode {
		t.Fatalf("pinned identity %d does not match the workspace (%d, %v)", paths.WorkspaceInode, stat.Ino, err)
	}
	// The directory stays held open for the session's lifetime, so its inode
	// number cannot be given to a directory the child creates after deleting
	// its workspace.
	if _, _, err := paths.PinnedWorkspaceIdentity(); err != nil {
		t.Fatalf("a live session does not hold its workspace directory: %v", err)
	}
}

// The hold lasts exactly as long as the session: removing a stopped session's
// files lets go of the directory, and its recorded identity is then refused.
func TestRemovingSessionFilesReleasesTheWorkspaceHold(t *testing.T) {
	root := filepath.Join(t.TempDir(), "session")
	state := &sessionState{paths: acp.SessionPaths{Root: root, Workspace: filepath.Join(root, "workspace")}}
	if err := os.MkdirAll(state.paths.Workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := acp.RecordWorkspaceIdentity(&state.paths); err != nil {
		t.Fatal(err)
	}
	if _, _, err := state.paths.PinnedWorkspaceIdentity(); err != nil {
		t.Fatal(err)
	}
	if err := (&Server{}).removeSessionPrivateFiles(state); err != nil {
		t.Fatal(err)
	}
	if _, _, err := state.paths.PinnedWorkspaceIdentity(); err == nil {
		t.Fatal("a removed session still holds its workspace directory open")
	}
}

// A creation that fails after the identity was recorded must not keep the
// directory open: nothing would ever release it.
func TestFailedCreateReleasesTheWorkspaceHold(t *testing.T) {
	previousRecord, previousFinalize := recordSessionWorkspaceIdentity, finalizeSessionOwnership
	t.Cleanup(func() { recordSessionWorkspaceIdentity, finalizeSessionOwnership = previousRecord, previousFinalize })
	var recorded acp.SessionPaths
	recordSessionWorkspaceIdentity = func(paths *acp.SessionPaths) error {
		err := previousRecord(paths)
		recorded = *paths
		return err
	}
	finalizeSessionOwnership = func(string, int, int) error { return fmt.Errorf("handoff refused") }
	server, cfg, profile := newTestServer(t, "immediate")
	create := testCreateSessionRequest(t, cfg, profile)
	created := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg)
	if created.Code == http.StatusCreated {
		t.Fatalf("create status = %d, want a failure", created.Code)
	}
	if recorded.WorkspaceInode == 0 {
		t.Fatal("the creation failed before it recorded a workspace identity")
	}
	if _, _, err := recorded.PinnedWorkspaceIdentity(); err == nil {
		t.Fatal("a failed creation still holds its workspace directory open")
	}
}
