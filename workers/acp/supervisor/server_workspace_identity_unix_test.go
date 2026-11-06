//go:build unix

package supervisor

import (
	"fmt"
	"net/http"
	"os"
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
	defer server.mu.Unlock()
	paths := server.sessions[create.RuntimeSessionID].paths
	if paths.WorkspaceInode == 0 {
		t.Fatal("the session has no pinned workspace identity")
	}
	var stat syscall.Stat_t
	if err := syscall.Stat(paths.Workspace, &stat); err != nil || stat.Ino != paths.WorkspaceInode {
		t.Fatalf("pinned identity %d does not match the workspace (%d, %v)", paths.WorkspaceInode, stat.Ino, err)
	}
}
