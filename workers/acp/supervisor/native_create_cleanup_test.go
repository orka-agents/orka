package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/acp"
	"github.com/orka-agents/orka/internal/codexstate"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

type nativeCreateFixture struct {
	server  *Server
	cfg     Config
	request harnessv2.CreateRuntimeSessionRequest
	paths   acp.SessionPaths
	journal string
}

func newNativeCreateFixture(t *testing.T, mode string) *nativeCreateFixture {
	t.Helper()
	source, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeTestNativeRollout(t, source)
	data, err := codexstate.Capture(t.Context(), source, testNativeThreadID)
	if err != nil {
		t.Fatal(err)
	}
	server, cfg, profile := newTestServer(t, mode)
	parent, err := filepath.EvalSymlinks(filepath.Dir(cfg.SessionBaseDir))
	if err != nil {
		t.Fatal(err)
	}
	cfg.SessionBaseDir = filepath.Join(parent, "sessions")
	server.cfg.SessionBaseDir = cfg.SessionBaseDir
	request := testCreateSessionRequest(t, cfg, profile)
	request.NativeRestore = &harnessv2.NativeSessionRestore{Snapshot: harnessv2.NativeSessionSnapshot{
		Data: data, DataDigest: codexstate.DataDigest(data), ProviderSessionID: testNativeThreadID,
		ProviderKind: providerKindCodex, ProviderVersion: acp.CodexCLIVersion,
		RuntimeSessionUID: request.Metadata.Fence.RuntimeSessionUID, RuntimeProfileDigest: request.Metadata.Fence.RuntimeProfileDigest, WorkingDirectory: "/source/work",
	}}
	sealRequest(t, &request.Metadata.RequestDigest, request)
	fixture := &nativeCreateFixture{server: server, cfg: cfg, request: request,
		journal: filepath.Join(cfg.SessionBaseDir, ".native-install", sessionPathID(request.Metadata.Fence.RuntimeSessionUID, request.Metadata.Fence.RuntimeSessionGeneration)),
	}
	server.cfg.Provider.PrepareSession = func(paths acp.SessionPaths) error {
		fixture.paths = paths
		return prepareCodexHome(paths)
	}
	return fixture
}

func TestSupervisorNativeCreateKnownFailureRemovesPrivateJournal(t *testing.T) {
	for _, stage := range []string{"install", "environment", "start", "load"} {
		t.Run(stage, func(t *testing.T) {
			if stage == "load" && runtime.GOOS != "linux" {
				t.Skip("proven child exit requires Linux")
			}
			fixture := newNativeCreateFixture(t, "native-resume-reject")
			switch stage {
			case "install":
				fixture.server.cfg.Provider.PrepareSession = func(paths acp.SessionPaths) error {
					fixture.paths = paths
					if err := prepareCodexHome(paths); err != nil {
						return err
					}
					return os.Mkdir(filepath.Join(paths.Home, ".codex", "sessions"), 0o700)
				}
			case "environment":
				fixture.server.cfg.Provider.Environment["BAD=NAME"] = "invalid"
			case "start":
				fixture.server.cfg.Provider.Command = "relative-command"
			}
			response := performMutation(t, fixture.server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", fixture.request, fixture.cfg)
			if response.Code != http.StatusInternalServerError {
				t.Fatalf("known %s failure: %d %s", stage, response.Code, response.Body.String())
			}
			assertNativeCreatePathsAbsent(t, fixture)
			remaining := fixture.cfg.UIDAllocator.Remaining()
			replay := performMutation(t, fixture.server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", fixture.request, fixture.cfg)
			if replay.Code != http.StatusConflict || fixture.cfg.UIDAllocator.Remaining() != remaining {
				t.Fatalf("failed create retry allocated a new identity: %d %s", replay.Code, replay.Body.String())
			}
			assertNativeCreatePathsAbsent(t, fixture)
			if len(fixture.server.status().Sessions) != 0 {
				t.Fatal("known failure left a resident session")
			}
		})
	}
}

func assertNativeCreatePathsAbsent(t *testing.T, fixture *nativeCreateFixture) {
	t.Helper()
	if fixture.paths.Root == "" {
		t.Fatal("create did not reach private home preparation")
	}
	for _, name := range []string{fixture.paths.Root, fixture.journal} {
		if _, err := os.Lstat(name); !os.IsNotExist(err) {
			t.Fatalf("known failure retained %s: %v", name, err)
		}
	}
}

func TestSupervisorNativeCreateRejectPreservesPreexistingFrozenJournal(t *testing.T) {
	fixture := newNativeCreateFixture(t, "native-resume")
	oldHome, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	oldCWD, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := fixture.request.NativeRestore.Snapshot.Data
	receipt, err := codexstate.Install(t.Context(), data, oldHome, oldCWD, fixture.journal)
	if err != nil {
		t.Fatal(err)
	}
	plan := readNativeJournalFile(t, fixture.journal, "plan.json")
	savedReceipt := readNativeJournalFile(t, fixture.journal, "receipt.json")
	target := filepath.Join(oldHome, receipt.TargetPath)
	published, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	remaining := fixture.cfg.UIDAllocator.Remaining()
	for attempt, wantCode := range []int{http.StatusInternalServerError, http.StatusConflict} {
		response := performMutation(t, fixture.server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", fixture.request, fixture.cfg)
		if response.Code != wantCode {
			t.Fatalf("mismatched frozen install: %d %s", response.Code, response.Body.String())
		}
		if fixture.cfg.UIDAllocator.Remaining() != remaining-1 {
			t.Fatalf("frozen journal rejection allocated another identity on attempt %d", attempt)
		}
		if _, err := os.Lstat(fixture.paths.Root); !os.IsNotExist(err) {
			t.Fatal("rejected retry retained its fresh private home")
		}
		if !bytes.Equal(plan, readNativeJournalFile(t, fixture.journal, "plan.json")) || !bytes.Equal(savedReceipt, readNativeJournalFile(t, fixture.journal, "receipt.json")) {
			t.Fatal("rejected retry changed the existing frozen journal")
		}
		retained, err := os.Stat(target)
		if err != nil || !os.SameFile(published, retained) {
			t.Fatal("rejected retry deleted the original publication witness")
		}
	}
	reconciled, err := codexstate.Install(t.Context(), data, oldHome, oldCWD, fixture.journal)
	if err != nil || reconciled.OperationID != receipt.OperationID || reconciled.Outcome != receipt.Outcome {
		t.Fatalf("original frozen plan no longer reconciles: %#v %v", reconciled, err)
	}
}

func TestSupervisorNativeCreateUnprovenExitRetainsAndRetriesCleanup(t *testing.T) {
	fixture := newNativeCreateFixture(t, "native-resume-reject-held")
	fixture.server.cfg.CancelGrace = time.Nanosecond
	response := performMutation(t, fixture.server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", fixture.request, fixture.cfg)
	var failure harnessv2.ErrorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil || response.Code != http.StatusConflict || failure.Code != harnessv2.ErrorCodeCleanupUnproven || failure.Retryable {
		t.Fatalf("unproven native create: %d %s", response.Code, response.Body.String())
	}
	fixture.server.mu.Lock()
	state := fixture.server.sessions[fixture.request.RuntimeSessionID]
	fixture.server.mu.Unlock()
	if state == nil || state.runtime == nil || state.nativeInstallUnresolved == nil || state.creating || isDrainCleanupState(state) {
		t.Fatal("unproven initialization lost its stopped runtime or retained evidence")
	}
	if _, err := state.runtime.StartPrompt(t.Context(), "after-failed-create", "digest", []acp.ContentBlock{acp.Text("continue")}); err == nil {
		t.Fatal("unproven initialization admitted another prompt")
	}
	plan := readNativeJournalFile(t, fixture.journal, "plan.json")
	remaining := fixture.cfg.UIDAllocator.Remaining()
	replay := performMutation(t, fixture.server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", fixture.request, fixture.cfg)
	if replay.Code != response.Code || replay.Body.String() != response.Body.String() || fixture.cfg.UIDAllocator.Remaining() != remaining {
		t.Fatal("unproven create retry changed the operation or reused an identity")
	}
	select {
	case <-state.runtime.Process().Done():
	case <-time.After(3 * time.Second):
		t.Fatal("failed initialization did not kill its adapter")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	closeErr := fixture.server.Close(ctx)
	if runtime.GOOS == "linux" && closeErr != nil {
		t.Fatalf("close did not retry the retained child stop: %v", closeErr)
	}
	if runtime.GOOS != "linux" && closeErr == nil {
		t.Fatal("close skipped the retained child instead of checking exit proof")
	}
	if _, err := os.Stat(fixture.paths.Root); err != nil || !bytes.Equal(plan, readNativeJournalFile(t, fixture.journal, "plan.json")) {
		t.Fatal("shutdown discarded unproven create evidence")
	}
	if runtime.GOOS == "linux" {
		deletion := harnessv2.DeleteRuntimeSessionRequest{Protocol: harnessv2.ProtocolVersion, Metadata: testMetadata(fixture.request.Metadata.Fence, "delete-failed-native", false), Reason: "cleanup after proven exit"}
		sealRequest(t, &deletion.Metadata.RequestDigest, deletion)
		deleted := performMutation(t, fixture.server.Handler(), http.MethodDelete, "/v2/runtime-sessions/session-1", deletion, fixture.cfg)
		if deleted.Code != http.StatusOK {
			t.Fatalf("explicit retained runtime delete: %d %s", deleted.Code, deleted.Body.String())
		}
		assertNativeCreatePathsAbsent(t, fixture)
	}
}

func readNativeJournalFile(t *testing.T, journal, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(journal, name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}
