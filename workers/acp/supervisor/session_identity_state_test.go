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
	"sync"
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

func TestE2EPromptWriteAmbiguityLedgerSurvivesDurableSupervisorRestart(t *testing.T) {
	cfg, _ := newSessionIdentityTestConfig(t)
	cfg.DurableWorkspaceDir = t.TempDir()
	cfg.E2EPromptWriteAmbiguityMarker = testE2EPromptWriteAmbiguityMarker
	server, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	request := harnessv2.StartPromptRequest{
		Metadata: harnessv2.MutationMetadata{OperationID: "operation-across-cold-boot"},
		Input: harnessv2.PromptInput{Content: []harnessv2.ContentBlock{{
			Type: harnessv2.ContentBlockText,
			Text: "Reply exactly: " + testE2EPromptWriteAmbiguityMarker,
		}}},
	}
	server.mu.Lock()
	consumed, err := server.consumeE2EPromptWriteAmbiguityLocked(context.Background(), request, testE2EPromptWriteAmbiguityMarker)
	server.mu.Unlock()
	if err != nil || !consumed {
		t.Fatalf("initial durable fault consumption = (%v, %v), want consumed", consumed, err)
	}
	expectedLedgerDir := filepath.Join(cfg.DurableWorkspaceDir, ".session-identity", e2ePromptWriteAmbiguityLedgerDir)
	if server.e2ePromptWriteFaultDir != expectedLedgerDir {
		t.Fatalf("durable ledger directory = %q, want %q", server.e2ePromptWriteFaultDir, expectedLedgerDir)
	}
	closeIdentityTestSupervisor(t, server)

	restartedCfg, _ := newSessionIdentityTestConfig(t)
	restartedCfg.DurableWorkspaceDir = cfg.DurableWorkspaceDir
	restartedCfg.E2EPromptWriteAmbiguityMarker = testE2EPromptWriteAmbiguityMarker
	restarted, err := New(restartedCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeIdentityTestSupervisor(t, restarted) })
	restarted.mu.Lock()
	consumed, err = restarted.consumeE2EPromptWriteAmbiguityLocked(context.Background(), request, testE2EPromptWriteAmbiguityMarker)
	restarted.mu.Unlock()
	if err != nil || consumed {
		t.Fatalf("recreated durable fault consumption = (%v, %v), want already consumed", consumed, err)
	}
	entries, err := os.ReadDir(expectedLedgerDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || strings.Contains(entries[0].Name(), string(request.Metadata.OperationID)) {
		t.Fatalf("durable operation records = %v, want one digest-keyed record", entries)
	}
}

func TestE2EPromptWriteAmbiguityLedgerSurvivesDirectRuntimeReplacement(t *testing.T) {
	recorder := &sharedE2EPromptWriteFaultRecorder{consumed: make(map[harnessv2.OperationID]struct{})}
	cfg, _ := newSessionIdentityTestConfig(t)
	cfg.E2EPromptWriteAmbiguityMarker = testE2EPromptWriteAmbiguityMarker
	cfg.E2EPromptWriteFaultRecorder = recorder
	server, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	request := harnessv2.StartPromptRequest{
		Metadata: harnessv2.MutationMetadata{OperationID: "operation-across-runtime-replacement"},
		Input: harnessv2.PromptInput{Content: []harnessv2.ContentBlock{{
			Type: harnessv2.ContentBlockText,
			Text: "Reply exactly: " + testE2EPromptWriteAmbiguityMarker,
		}}},
	}
	server.mu.Lock()
	consumed, err := server.consumeE2EPromptWriteAmbiguityLocked(context.Background(), request, testE2EPromptWriteAmbiguityMarker)
	server.mu.Unlock()
	if err != nil || !consumed {
		t.Fatalf("initial direct-pool fault consumption = (%v, %v), want consumed", consumed, err)
	}
	if server.e2ePromptWriteFaultDir != "" || server.e2ePromptWriteRecorder != recorder {
		t.Fatalf("direct-pool recorder = dir %q recorder %T, want external recorder", server.e2ePromptWriteFaultDir, server.e2ePromptWriteRecorder)
	}
	closeIdentityTestSupervisor(t, server)

	// A physical replacement receives a fresh emptyDir-backed SessionBaseDir.
	// Only the controller-owned recorder survives that boundary.
	restartedCfg, _ := newSessionIdentityTestConfig(t)
	restartedCfg.E2EPromptWriteAmbiguityMarker = testE2EPromptWriteAmbiguityMarker
	restartedCfg.E2EPromptWriteFaultRecorder = recorder
	restarted, err := New(restartedCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeIdentityTestSupervisor(t, restarted) })
	restarted.mu.Lock()
	consumed, err = restarted.consumeE2EPromptWriteAmbiguityLocked(context.Background(), request, testE2EPromptWriteAmbiguityMarker)
	restarted.mu.Unlock()
	if err != nil || consumed {
		t.Fatalf("replacement direct-pool fault consumption = (%v, %v), want already consumed", consumed, err)
	}
}

type sharedE2EPromptWriteFaultRecorder struct {
	mu       sync.Mutex
	consumed map[harnessv2.OperationID]struct{}
}

func (r *sharedE2EPromptWriteFaultRecorder) Consume(_ context.Context, metadata harnessv2.MutationMetadata) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.consumed[metadata.OperationID]; ok {
		return false, nil
	}
	r.consumed[metadata.OperationID] = struct{}{}
	return true, nil
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
	if _, _, err := healthy.UIDAllocator.AllocateAboveReserve(0); err != nil {
		t.Fatalf("allocate durable session identity: %v", err)
	}
	closeIdentityTestSupervisor(t, first)
	if _, _, err := acp.PrepareDurableSessionWorkspace(healthy.DurableWorkspaceDir, "session-a", 1); err != nil {
		t.Fatalf("prepare durable checkpoint: %v", err)
	}
	if err := acp.CommitDurableSessionWorkspace(
		healthy.DurableWorkspaceDir,
		"session-a",
		acp.DurableWorkspaceBinding{
			RepositoryIdentity:       "github.com/o/r",
			Revision:                 "abc",
			SessionIdentityHighWater: 1,
		},
	); err != nil {
		t.Fatalf("commit durable checkpoint: %v", err)
	}
	restarted, _ := newSessionIdentityTestConfig(t)
	restarted.DurableWorkspaceDir = healthy.DurableWorkspaceDir
	second, err := New(restarted)
	if err != nil {
		t.Fatalf("checkpoints WITH identity state must boot: %v", err)
	}
	closeIdentityTestSupervisor(t, second)
}

// A partial restore can preserve the latest committed checkpoint while
// rolling the allocator file back to an older, otherwise valid count. The
// checkpoint's recorded high-water floor must reject that inconsistent state
// before the next child can reuse an existing UID/GID.
func TestNewRefusesDurableCheckpointNewerThanIdentityState(t *testing.T) {
	cfg, _ := newSessionIdentityTestConfig(t)
	cfg.DurableWorkspaceDir = t.TempDir()
	server, err := New(cfg)
	if err != nil {
		t.Fatalf("bootstrap durable supervisor: %v", err)
	}
	for i := range 2 {
		if _, _, err := cfg.UIDAllocator.AllocateAboveReserve(0); err != nil {
			t.Fatalf("allocate session identity %d: %v", i+1, err)
		}
	}
	closeIdentityTestSupervisor(t, server)

	const sessionUID = "session-newer-than-allocator"
	if _, _, err := acp.PrepareDurableSessionWorkspace(cfg.DurableWorkspaceDir, sessionUID, 2); err != nil {
		t.Fatalf("prepare durable checkpoint: %v", err)
	}
	if err := acp.CommitDurableSessionWorkspace(
		cfg.DurableWorkspaceDir,
		sessionUID,
		acp.DurableWorkspaceBinding{
			RepositoryIdentity:       "github.com/o/r",
			Revision:                 "abc",
			SessionIdentityHighWater: 2,
		},
	); err != nil {
		t.Fatalf("commit durable checkpoint: %v", err)
	}
	identityStateDir := filepath.Join(cfg.DurableWorkspaceDir, ".session-identity")
	state, exists, err := readSessionIdentityState(filepath.Join(identityStateDir, sessionIdentityStateFile))
	if err != nil || !exists || state.Allocated != 2 {
		t.Fatalf("identity state before rollback = %#v exists=%v err=%v, want allocated=2", state, exists, err)
	}
	state.Allocated = 1
	if err := persistSessionIdentityState(identityStateDir, state); err != nil {
		t.Fatalf("restore stale identity state: %v", err)
	}

	restartedCfg, _ := newSessionIdentityTestConfig(t)
	restartedCfg.DurableWorkspaceDir = cfg.DurableWorkspaceDir
	restarted, err := New(restartedCfg)
	if restarted != nil {
		closeIdentityTestSupervisor(t, restarted)
	}
	if err == nil || !strings.Contains(err.Error(), "requires session identity high-water 2 but allocator state records 1") {
		t.Fatalf("New error = %v, want stale allocator refusal", err)
	}
}

// A surviving workspace tree still proves that a pre-suspension session
// received an identity even when both of its checkpoint sidecars were lost.
func TestNewRefusesDurableWorkspaceTreeWithoutIdentityState(t *testing.T) {
	cfg, _ := newSessionIdentityTestConfig(t)
	cfg.DurableWorkspaceDir = t.TempDir()
	if err := os.Mkdir(
		filepath.Join(cfg.DurableWorkspaceDir, "ws-session-tree"),
		0o700,
	); err != nil {
		t.Fatal(err)
	}
	server, err := New(cfg)
	if server != nil {
		closeIdentityTestSupervisor(t, server)
	}
	if err == nil || !strings.Contains(err.Error(), "no session identity allocator state") {
		t.Fatalf("New error = %v, want the orphaned-workspace refusal", err)
	}
}

func TestNewRefusesMarkerlessDurableWorkspaceWithIdentityState(t *testing.T) {
	cfg, _ := newSessionIdentityTestConfig(t)
	cfg.DurableWorkspaceDir = t.TempDir()
	server, err := New(cfg)
	if err != nil {
		t.Fatalf("bootstrap durable supervisor: %v", err)
	}
	for i := range 2 {
		if _, _, err := cfg.UIDAllocator.AllocateAboveReserve(0); err != nil {
			t.Fatalf("allocate session identity %d: %v", i+1, err)
		}
	}
	closeIdentityTestSupervisor(t, server)
	if err := os.Mkdir(filepath.Join(cfg.DurableWorkspaceDir, "ws-session-without-marker"), 0o700); err != nil {
		t.Fatalf("create markerless durable workspace: %v", err)
	}

	restartedCfg, _ := newSessionIdentityTestConfig(t)
	restartedCfg.DurableWorkspaceDir = cfg.DurableWorkspaceDir
	restarted, err := New(restartedCfg)
	if restarted != nil {
		closeIdentityTestSupervisor(t, restarted)
	}
	if err == nil || !strings.Contains(err.Error(), "has no checkpoint identity high-water mark") {
		t.Fatalf("New error = %v, want markerless workspace refusal", err)
	}
}

// Fresh preparation publishes a pending marker before creating the workspace
// tree. A restart can therefore validate the allocator floor, then the next
// creation wipes the partial tree instead of getting stranded at startup.
func TestNewRecoversFreshDurableWorkspacePendingMarker(t *testing.T) {
	cfg, _ := newSessionIdentityTestConfig(t)
	cfg.DurableWorkspaceDir = t.TempDir()
	server, err := New(cfg)
	if err != nil {
		t.Fatalf("bootstrap durable supervisor: %v", err)
	}
	if _, _, err := cfg.UIDAllocator.AllocateAboveReserve(0); err != nil {
		t.Fatalf("allocate session identity: %v", err)
	}
	const sessionUID = "session-fresh-pending"
	workspaceDir, committed, err := acp.PrepareDurableSessionWorkspace(
		cfg.DurableWorkspaceDir, sessionUID, 1,
	)
	if err != nil || committed != nil {
		t.Fatalf("prepare fresh workspace = committed %+v err=%v", committed, err)
	}
	partialPath := filepath.Join(workspaceDir, "partial.txt")
	if err := os.WriteFile(partialPath, []byte("partial"), 0o600); err != nil {
		t.Fatalf("write partial workspace: %v", err)
	}
	closeIdentityTestSupervisor(t, server)

	restartedCfg, _ := newSessionIdentityTestConfig(t)
	restartedCfg.DurableWorkspaceDir = cfg.DurableWorkspaceDir
	restarted, err := New(restartedCfg)
	if err != nil {
		t.Fatalf("restart with fresh pending workspace: %v", err)
	}
	t.Cleanup(func() { closeIdentityTestSupervisor(t, restarted) })
	if _, _, err := restartedCfg.UIDAllocator.AllocateAboveReserve(0); err != nil {
		t.Fatalf("allocate retry identity: %v", err)
	}
	freshDir, committed, err := acp.PrepareDurableSessionWorkspace(
		restartedCfg.DurableWorkspaceDir, sessionUID, 2,
	)
	if err != nil || committed != nil {
		t.Fatalf("retry fresh workspace = committed %+v err=%v", committed, err)
	}
	if freshDir != workspaceDir {
		t.Fatalf("retry workspace dir = %q, want %q", freshDir, workspaceDir)
	}
	if _, err := os.Stat(partialPath); !os.IsNotExist(err) {
		t.Fatalf("partial workspace survived retry wipe: %v", err)
	}
}

// A staged repository transition proves that this durable volume has prior
// session identity history even if the committed binding was wiped before a
// crash. Losing the allocator state in that window must also fail closed.
func TestNewRefusesDurableTransitionWithoutIdentityState(t *testing.T) {
	cfg, _ := newSessionIdentityTestConfig(t)
	cfg.DurableWorkspaceDir = t.TempDir()
	if err := acp.MarkDurableWorkspaceTransitionAuthorized(
		cfg.DurableWorkspaceDir,
		"session-uid-transition",
		acp.DurableWorkspaceBinding{
			RepositoryIdentity: "github.com/o/fork",
			Revision:           "def",
			SessionGeneration:  4,
		},
	); err != nil {
		t.Fatalf("stage durable transition: %v", err)
	}
	server, err := New(cfg)
	if server != nil {
		closeIdentityTestSupervisor(t, server)
	}
	if err == nil || !strings.Contains(err.Error(), "no session identity allocator state") {
		t.Fatalf("New error = %v, want the orphaned-transition refusal", err)
	}
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
