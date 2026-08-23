package supervisor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func TestSessionIdentityHighWaterSurvivesSupervisorRestart(t *testing.T) {
	cfg, _ := newSessionIdentityTestConfig(t)
	server, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	firstUID, firstGID, err := cfg.UIDAllocator.AllocateAboveReserve(0)
	if err != nil {
		t.Fatal(err)
	}
	closeIdentityTestSupervisor(t, server)

	staleRoot := filepath.Join(cfg.SessionBaseDir, "retained-session")
	staleMarker := filepath.Join(staleRoot, "workspace", "private-marker")
	if err := os.MkdirAll(filepath.Dir(staleMarker), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staleMarker, []byte("retained"), 0o600); err != nil {
		t.Fatal(err)
	}

	restartedCfg, _ := newSessionIdentityTestConfig(t)
	restartedCfg.SessionBaseDir = cfg.SessionBaseDir
	restarted, err := New(restartedCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeIdentityTestSupervisor(t, restarted) })
	secondUID, secondGID, err := restartedCfg.UIDAllocator.AllocateAboveReserve(0)
	if err != nil {
		t.Fatal(err)
	}
	if secondUID != firstUID+1 || secondGID != firstGID+1 {
		t.Fatalf("restarted allocation = %d:%d, want %d:%d", secondUID, secondGID, firstUID+1, firstGID+1)
	}
	if got, err := os.ReadFile(staleMarker); err != nil || string(got) != "retained" {
		t.Fatalf("restart unexpectedly modified retained session data: content=%q err=%v", got, err)
	}
	state, exists, err := readSessionIdentityState(filepath.Join(cfg.SessionBaseDir, sessionIdentityStateFile))
	if err != nil || !exists || state.Allocated != 2 {
		t.Fatalf("identity state = %#v exists=%v err=%v, want allocated=2", state, exists, err)
	}
}

func TestSessionIdentityStateMissingWithStaleEntriesFailsClosed(t *testing.T) {
	cfg, _ := newSessionIdentityTestConfig(t)
	if err := os.MkdirAll(filepath.Join(cfg.SessionBaseDir, "stale-session"), 0o700); err != nil {
		t.Fatal(err)
	}
	server, err := New(cfg)
	if server != nil {
		closeIdentityTestSupervisor(t, server)
	}
	if err == nil || !strings.Contains(err.Error(), "state is missing") {
		t.Fatalf("New error = %v, want missing-state failure", err)
	}
}

// A durable volume whose committed checkpoints survived while the allocator
// state was lost (a partial restore) must refuse startup: a fresh allocator
// at zero could hand a continuation a UID/GID a pre-suspension session used.
func TestNewRefusesDurableCheckpointsWithoutIdentityState(t *testing.T) {
	cfg, _ := newSessionIdentityTestConfig(t)
	cfg.DurableWorkspaceDir = t.TempDir()
	if err := os.WriteFile(
		filepath.Join(cfg.DurableWorkspaceDir, "ws-session-a.binding.json"),
		[]byte(`{"repositoryIdentity":"github.com/o/r","revision":"abc"}`), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	server, err := New(cfg)
	if server != nil {
		closeIdentityTestSupervisor(t, server)
	}
	if err == nil || !strings.Contains(err.Error(), "no session identity allocator state") {
		t.Fatalf("New error = %v, want the orphaned-checkpoint refusal", err)
	}

	// With the allocator state present alongside the checkpoint, startup
	// proceeds normally.
	healthy, _ := newSessionIdentityTestConfig(t)
	healthy.DurableWorkspaceDir = t.TempDir()
	first, err := New(healthy)
	if err != nil {
		t.Fatalf("bootstrap durable supervisor: %v", err)
	}
	closeIdentityTestSupervisor(t, first)
	if err := os.WriteFile(
		filepath.Join(healthy.DurableWorkspaceDir, "ws-session-a.binding.json"),
		[]byte(`{"repositoryIdentity":"github.com/o/r","revision":"abc"}`), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	restarted, _ := newSessionIdentityTestConfig(t)
	restarted.DurableWorkspaceDir = healthy.DurableWorkspaceDir
	second, err := New(restarted)
	if err != nil {
		t.Fatalf("checkpoints WITH identity state must boot: %v", err)
	}
	closeIdentityTestSupervisor(t, second)
}

func TestSessionIdentityStateRejectsSymlink(t *testing.T) {
	cfg, _ := newSessionIdentityTestConfig(t)
	if err := os.MkdirAll(cfg.SessionBaseDir, 0o711); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(outside, []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(cfg.SessionBaseDir, sessionIdentityStateFile)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	server, err := New(cfg)
	if server != nil {
		closeIdentityTestSupervisor(t, server)
	}
	if err == nil || !strings.Contains(err.Error(), "bounded regular file") {
		t.Fatalf("New error = %v, want symlink-state rejection", err)
	}
}

func TestNewFailsClosedWhenSessionIdentityStatePreparationFails(t *testing.T) {
	cfg, _ := newSessionIdentityTestConfig(t)
	prepareErr := errors.New("injected identity state failure")
	server, err := newServer(cfg, func(string, *acp.UIDAllocator) (io.Closer, error) { return nil, prepareErr })
	if server != nil {
		closeIdentityTestSupervisor(t, server)
		t.Fatal("newServer returned a server after identity state preparation failed")
	}
	if !errors.Is(err, prepareErr) {
		t.Fatalf("newServer error = %v, want injected preparation failure", err)
	}
	if got, want := cfg.UIDAllocator.Remaining(), cfg.UIDAllocator.Capacity(); got != want {
		t.Fatalf("preparation failure consumed identity capacity: remaining=%d capacity=%d", got, want)
	}
}

func TestSessionIdentityStateRejectsConcurrentSupervisor(t *testing.T) {
	cfg, _ := newSessionIdentityTestConfig(t)
	first, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeIdentityTestSupervisor(t, first) })

	secondCfg, _ := newSessionIdentityTestConfig(t)
	secondCfg.SessionBaseDir = cfg.SessionBaseDir
	second, err := New(secondCfg)
	if second != nil {
		closeIdentityTestSupervisor(t, second)
	}
	if err == nil || !strings.Contains(err.Error(), "lock session identity state") {
		t.Fatalf("concurrent New error = %v, want lock failure", err)
	}
}

func TestSessionIdentityStateRejectsRangeChange(t *testing.T) {
	cfg, _ := newSessionIdentityTestConfig(t)
	server, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	closeIdentityTestSupervisor(t, server)
	firstUID, lastUID, firstGID, _ := cfg.UIDAllocator.Range()

	restartedCfg, _ := newSessionIdentityTestConfig(t)
	restartedCfg.SessionBaseDir = cfg.SessionBaseDir
	restartedCfg.UIDAllocator, err = acp.NewUIDAllocator(firstUID-1, lastUID-1, firstGID-1)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := New(restartedCfg)
	if restarted != nil {
		closeIdentityTestSupervisor(t, restarted)
	}
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("range-change New error = %v, want range mismatch", err)
	}
}

func TestSessionIdentityStateRecoversOrphanedInitializationFile(t *testing.T) {
	cfg, _ := newSessionIdentityTestConfig(t)
	if err := os.MkdirAll(cfg.SessionBaseDir, 0o711); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(cfg.SessionBaseDir, sessionIdentityStateTempPrefix+"orphan")
	if err := os.WriteFile(orphan, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	server, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	closeIdentityTestSupervisor(t, server)
	if _, err := os.Lstat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphaned identity state remained: %v", err)
	}
	if _, exists, err := readSessionIdentityState(filepath.Join(cfg.SessionBaseDir, sessionIdentityStateFile)); err != nil || !exists {
		t.Fatalf("canonical identity state was not initialized: exists=%v err=%v", exists, err)
	}
}

func TestNewRejectsNonFreshSessionIdentityAllocator(t *testing.T) {
	cfg, _ := newSessionIdentityTestConfig(t)
	if _, _, err := cfg.UIDAllocator.AllocateAboveReserve(0); err != nil {
		t.Fatal(err)
	}
	server, err := New(cfg)
	if server != nil {
		closeIdentityTestSupervisor(t, server)
	}
	if err == nil || !strings.Contains(err.Error(), "fresh") {
		t.Fatalf("New error = %v, want fresh-allocator rejection", err)
	}
}

func newSessionIdentityTestConfig(t *testing.T) (Config, harnessv2.RuntimeProfile) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}))
	t.Cleanup(upstream.Close)
	cfg, profile := newTestConfigWithUpstream(t, "immediate", upstream.URL, strings.Repeat("p", 32))
	return cfg, profile
}

func closeIdentityTestSupervisor(t *testing.T, server *Server) {
	t.Helper()
	if server == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = server.Close(ctx)
}
