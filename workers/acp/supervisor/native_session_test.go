package supervisor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/acp"
	"github.com/orka-agents/orka/internal/codexstate"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const testNativeThreadID = "01970b26-25ad-71ef-bd22-9374ec0b741b"

func writeTestNativeRollout(t *testing.T, home string) {
	t.Helper()
	dir := filepath.Join(home, "sessions", "2026", "10", "05")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"ordinal":0,"timestamp":"2026-10-05T12:00:00Z","type":"session_meta","payload":{"id":"` + testNativeThreadID + `","cli_version":"0.160.0","history_mode":"paginated","history_base":null,"cwd":"/source/work","source":"exec","model_provider":"source-provider","runtime_workspace_roots":["/source/work"]}}
{"ordinal":1,"timestamp":"2026-10-05T12:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"private history"}]}}
`)
	if err := os.WriteFile(filepath.Join(dir, "rollout-2026-10-05T12-00-00-"+testNativeThreadID+".jsonl"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func nativeCaptureRequest(t *testing.T, fence harnessv2.Fence) harnessv2.CaptureNativeSessionRequest {
	t.Helper()
	request := harnessv2.CaptureNativeSessionRequest{Protocol: harnessv2.ProtocolVersion, Metadata: testMetadata(fence, "native-capture", false)}
	sealRequest(t, &request.Metadata.RequestDigest, request)
	return request
}

func TestSupervisorNativeCaptureStopsChildReplaysAndRetainsUntilDelete(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("descendant exit proof requires Linux")
	}
	server, cfg, profile := newTestServer(t, "native-capture")
	create := testCreateSessionRequest(t, cfg, profile)
	if response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg); response.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", response.Code, response.Body.String())
	}
	request := nativeCaptureRequest(t, create.Metadata.Fence)
	path := "/v2/runtime-sessions/session-1/native-session"
	if response := performMutation(t, server.Handler(), http.MethodPost, path, request, cfg); response.Code != http.StatusConflict {
		t.Fatalf("unsettled capture: %d", response.Code)
	}
	prompt := testStartPromptRequest(t, cfg, create.Metadata.Fence)
	if response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1/prompts/prompt-1", prompt, cfg); response.Code != http.StatusOK {
		t.Fatalf("prompt: %d %s", response.Code, response.Body.String())
	}
	server.mu.Lock()
	state := server.sessions[create.RuntimeSessionID]
	state.descriptor.State = harnessv2.RuntimeSessionStateIdle
	server.mu.Unlock()
	writeTestNativeRollout(t, filepath.Join(state.paths.Home, ".codex"))
	foreign := request
	foreign.Metadata.TaskUID = "foreign-task"
	sealRequest(t, &foreign.Metadata.RequestDigest, foreign)
	if response := performMutation(t, server.Handler(), http.MethodPost, path, foreign, cfg); response.Code != http.StatusConflict {
		t.Fatalf("foreign Task capture: %d", response.Code)
	}
	response := performMutation(t, server.Handler(), http.MethodPost, path, request, cfg)
	if response.Code != http.StatusOK {
		t.Fatalf("capture: %d %s", response.Code, response.Body.String())
	}
	var captured harnessv2.CaptureNativeSessionResponse
	if err := json.Unmarshal(response.Body.Bytes(), &captured); err != nil {
		t.Fatal(err)
	}
	if err := captured.ValidateFor(request); err != nil {
		t.Fatal(err)
	}
	select {
	case <-state.runtime.Process().Done():
	default:
		t.Fatal("capture returned before provider exit")
	}
	if cleanup, err := state.runtime.Delete(context.Background()); err != nil || !cleanup.Proven {
		t.Fatal("capture returned without descendant exit proof")
	}
	if isDrainCleanupState(state) {
		t.Fatal("drain would delete unacknowledged snapshot")
	}
	persisted, err := os.ReadFile(filepath.Join(state.paths.Root, ".native-session-snapshot.json"))
	if err != nil {
		t.Fatal(err)
	}
	var persistedSnapshot harnessv2.NativeSessionSnapshot
	if err := json.Unmarshal(persisted, &persistedSnapshot); err != nil || !bytes.Equal(persistedSnapshot.Data, captured.Snapshot.Data) {
		t.Fatal("exact captured data was not persisted")
	}
	replay := performMutation(t, server.Handler(), http.MethodPost, path, request, cfg)
	var replayed harnessv2.CaptureNativeSessionResponse
	if err := json.Unmarshal(replay.Body.Bytes(), &replayed); err != nil || replay.Code != http.StatusOK || replayed.Classification.Class != harnessv2.RequestClassificationDuplicate || !bytes.Equal(replayed.Snapshot.Data, captured.Snapshot.Data) {
		t.Fatalf("capture replay: %d %s", replay.Code, replay.Body.String())
	}
	if _, err := state.runtime.StartPrompt(context.Background(), "new-prompt", "digest", []acp.ContentBlock{acp.Text("continue")}); err == nil {
		t.Fatal("captured provider admitted another prompt")
	}
	// Rebinding controller authority cannot replay the old fence. A fresh
	// receipt-only operation names the original immutable capture explicitly.
	cfg.Fence.ControllerEpoch++
	server.cfg.Fence.ControllerEpoch = cfg.Fence.ControllerEpoch
	reconcile := request
	reconcile.Metadata.Fence.ControllerEpoch = cfg.Fence.ControllerEpoch
	reconcile.Metadata.OperationID = "reconcile-native-capture"
	reconcile.OriginalOperationID = request.Metadata.OperationID
	reconcile.OriginalRequestDigest = request.Metadata.RequestDigest
	sealRequest(t, &reconcile.Metadata.RequestDigest, reconcile)
	recovered := performMutation(t, server.Handler(), http.MethodPost, path, reconcile, cfg)
	var recoveredSnapshot harnessv2.CaptureNativeSessionResponse
	if err := json.Unmarshal(recovered.Body.Bytes(), &recoveredSnapshot); err != nil || recovered.Code != http.StatusOK || !bytes.Equal(recoveredSnapshot.Snapshot.Data, captured.Snapshot.Data) {
		t.Fatalf("current-authority native reconciliation: %d %s", recovered.Code, recovered.Body.String())
	}
	create.Metadata.Fence.ControllerEpoch = cfg.Fence.ControllerEpoch
	deletion := harnessv2.DeleteRuntimeSessionRequest{Protocol: harnessv2.ProtocolVersion, Metadata: testMetadata(create.Metadata.Fence, "delete-captured", false), Reason: "snapshot saved"}
	sealRequest(t, &deletion.Metadata.RequestDigest, deletion)
	if response := performMutation(t, server.Handler(), http.MethodDelete, "/v2/runtime-sessions/session-1", deletion, cfg); response.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(state.paths.Root); !os.IsNotExist(err) {
		t.Fatal("explicit delete retained captured home")
	}
}

func TestSupervisorNativeReconciliationNeverStartsAnotherCapture(t *testing.T) {
	server, cfg, profile := newTestServer(t, "native-capture")
	create := testCreateSessionRequest(t, cfg, profile)
	performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg)
	prompt := testStartPromptRequest(t, cfg, create.Metadata.Fence)
	performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1/prompts/prompt-1", prompt, cfg)
	server.mu.Lock()
	state := server.sessions[create.RuntimeSessionID]
	state.descriptor.State = harnessv2.RuntimeSessionStateIdle
	server.mu.Unlock()
	request := nativeCaptureRequest(t, create.Metadata.Fence)
	request.Metadata.OperationID = "reconcile-capture"
	request.OriginalOperationID = "missing-original"
	request.OriginalRequestDigest = harnessv2.RequestDigest(testDigest("missing-original"))
	sealRequest(t, &request.Metadata.RequestDigest, request)
	response := performMutation(t, server.Handler(), http.MethodPost, "/v2/runtime-sessions/session-1/native-session", request, cfg)
	if response.Code != http.StatusConflict || !bytes.Contains(response.Body.Bytes(), []byte(string(harnessv2.ErrorCodeNativeCaptureNotStarted))) || bytes.Contains(response.Body.Bytes(), []byte(`"retryable":true`)) {
		t.Fatalf("missing capture reconciliation must report that no capture started: %d %s", response.Code, response.Body.String())
	}
	select {
	case <-state.runtime.Process().Done():
		t.Fatal("reconciliation stopped the provider without an original capture")
	default:
	}
	if state.nativeCapture != nil || state.descriptor.State != harnessv2.RuntimeSessionStateIdle {
		t.Fatal("reconciliation started a new capture")
	}
}

func TestSupervisorNativeSuccessfulTurnRetainsEvidenceOnShutdown(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("descendant exit proof requires Linux")
	}
	server, cfg, profile := newTestServer(t, "native-capture")
	create := testCreateSessionRequest(t, cfg, profile)
	if response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg); response.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", response.Code, response.Body.String())
	}
	prompt := testStartPromptRequest(t, cfg, create.Metadata.Fence)
	if response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1/prompts/prompt-1", prompt, cfg); response.Code != http.StatusOK {
		t.Fatalf("prompt: %d %s", response.Code, response.Body.String())
	}
	server.mu.Lock()
	state := server.sessions[create.RuntimeSessionID]
	state.descriptor.State = harnessv2.RuntimeSessionStateIdle
	cleanup := server.beginDrainLocked("rollout", time.Now().UTC())
	server.mu.Unlock()
	if len(cleanup) != 0 || state.drainCleanupScheduled {
		t.Fatal("drain admitted native cleanup before controller capture")
	}
	writeTestNativeRollout(t, filepath.Join(state.paths.Home, ".codex"))
	if err := server.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-state.runtime.Process().Done():
	default:
		t.Fatal("shutdown retained a live provider")
	}
	if _, err := os.Stat(state.paths.Home); err != nil {
		t.Fatal("shutdown deleted native capture admission evidence")
	}
}

func TestNativeCaptureRequiresSucceededTaskAndPreservesOtherCleanup(t *testing.T) {
	state := &sessionState{
		supportsNativeSessions: true,
		runtime:                &acp.RuntimeSession{},
		profile:                harnessv2.RuntimeProfile{ProviderKind: providerKindCodex},
		descriptor:             harnessv2.RuntimeSessionDescriptor{State: harnessv2.RuntimeSessionStateIdle},
		prompt:                 &promptState{settlement: &harnessv2.PromptSettlement{TerminalEvent: harnessv2.EventCompleted, Outcome: harnessv2.PromptOutcomeSucceeded}},
	}
	if isDrainCleanupState(state) || !matchesTerminalNativeTask(state, harnessv2.MutationMetadata{}) {
		t.Fatal("successful native turn was blocked from capture or eligible for automatic cleanup")
	}
	state.prompt.settlement.TerminalEvent = harnessv2.EventCancelled
	state.prompt.settlement.Outcome = harnessv2.PromptOutcomeCancelled
	if !isDrainCleanupState(state) || matchesTerminalNativeTask(state, harnessv2.MutationMetadata{}) {
		t.Fatal("cancelled turn was eligible for capture or blocked cleanup")
	}
	state.prompt.settlement.TerminalEvent = harnessv2.EventFailed
	state.prompt.settlement.Outcome = harnessv2.PromptOutcomeFailed
	if !isDrainCleanupState(state) || matchesTerminalNativeTask(state, harnessv2.MutationMetadata{}) {
		t.Fatal("failed turn was eligible for capture or blocked cleanup")
	}
	state.prompt.settlement.TerminalEvent = harnessv2.EventCompleted
	state.prompt.settlement.Outcome = harnessv2.PromptOutcomeSucceeded
	state.supportsNativeSessions = false
	if !isDrainCleanupState(state) {
		t.Fatal("provider without native support was blocked from cleanup")
	}
}

func TestSupervisorNativeCaptureFailurePreservesPrivateHome(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("descendant exit proof requires Linux")
	}
	server, cfg, profile := newTestServer(t, "native-capture")
	create := testCreateSessionRequest(t, cfg, profile)
	performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg)
	prompt := testStartPromptRequest(t, cfg, create.Metadata.Fence)
	performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1/prompts/prompt-1", prompt, cfg)
	server.mu.Lock()
	state := server.sessions[create.RuntimeSessionID]
	state.descriptor.State = harnessv2.RuntimeSessionStateIdle
	server.mu.Unlock()
	request := nativeCaptureRequest(t, create.Metadata.Fence)
	response := performMutation(t, server.Handler(), http.MethodPost, "/v2/runtime-sessions/session-1/native-session", request, cfg)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("missing source capture: %d %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(state.paths.Home); err != nil {
		t.Fatal("capture failure deleted private home")
	}
	if err := server.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(state.paths.Home); err != nil {
		t.Fatal("shutdown deleted failed capture evidence")
	}
}

func TestSupervisorNativeUnsupportedCaptureProvesExitAndRetainsHome(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("descendant exit proof requires Linux")
	}
	server, cfg, profile := newTestServer(t, "native-capture")
	create := testCreateSessionRequest(t, cfg, profile)
	performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg)
	prompt := testStartPromptRequest(t, cfg, create.Metadata.Fence)
	performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1/prompts/prompt-1", prompt, cfg)
	server.mu.Lock()
	state := server.sessions[create.RuntimeSessionID]
	state.descriptor.State = harnessv2.RuntimeSessionStateIdle
	server.mu.Unlock()
	home := filepath.Join(state.paths.Home, ".codex")
	writeTestNativeRollout(t, home)
	name := filepath.Join(home, "sessions", "2026", "10", "05", "rollout-2026-10-05T12-00-00-"+testNativeThreadID+".jsonl")
	file, err := os.OpenFile(name, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.WriteString("{\"ordinal\":2,\"timestamp\":\"2026-10-05T12:00:02Z\",\"type\":\"response_item\",\"payload\":{\"type\":\"reasoning\",\"encrypted_content\":\"opaque\",\"summary\":[]}}\n")
	if err := file.Close(); err != nil || writeErr != nil {
		t.Fatal("write encrypted fixture")
	}
	request := nativeCaptureRequest(t, create.Metadata.Fence)
	response := performMutation(t, server.Handler(), http.MethodPost, "/v2/runtime-sessions/session-1/native-session", request, cfg)
	var failure harnessv2.ErrorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil || response.Code != http.StatusUnprocessableEntity || failure.Code != harnessv2.ErrorCodeNativeCaptureUnsupported || failure.Retryable {
		t.Fatalf("unsupported capture: %d %s", response.Code, response.Body.String())
	}
	cleanup, err := state.runtime.Delete(t.Context())
	if err != nil || !cleanup.Proven {
		t.Fatal("unsupported capture did not prove child exit")
	}
	if _, err := os.Stat(name); err != nil {
		t.Fatal("unsupported capture lost private evidence")
	}
}

func TestSupervisorInstallsNativeBundleBeforeACPResumeAndReportsRestoration(t *testing.T) {
	source, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeTestNativeRollout(t, source)
	data, err := codexstate.Capture(context.Background(), source, testNativeThreadID)
	if err != nil {
		t.Fatal(err)
	}
	server, cfg, profile := newTestServer(t, "native-resume")
	server.cfg.Provider.PrepareSession = prepareCodexHome
	parent, err := filepath.EvalSymlinks(filepath.Dir(cfg.SessionBaseDir))
	if err != nil {
		t.Fatal(err)
	}
	server.cfg.SessionBaseDir = filepath.Join(parent, "sessions")
	request := testCreateSessionRequest(t, cfg, profile)
	request.NativeRestore = &harnessv2.NativeSessionRestore{Snapshot: harnessv2.NativeSessionSnapshot{
		Data: data, DataDigest: codexstate.DataDigest(data), ProviderSessionID: testNativeThreadID, ProviderKind: "codex", ProviderVersion: acp.CodexCLIVersion,
		RuntimeSessionUID: request.Metadata.Fence.RuntimeSessionUID, RuntimeProfileDigest: request.Metadata.Fence.RuntimeProfileDigest, WorkingDirectory: "/source/work",
	}}
	sealRequest(t, &request.Metadata.RequestDigest, request)
	response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", request, cfg)
	if response.Code != http.StatusCreated {
		t.Fatalf("native create: %d %s", response.Code, response.Body.String())
	}
	var created harnessv2.CreateRuntimeSessionResponse
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if err := created.ValidateFor(request); err != nil {
		t.Fatal(err)
	}
	prompt := testStartPromptRequest(t, cfg, request.Metadata.Fence)
	result := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1/prompts/prompt-1", prompt, cfg)
	if result.Code != http.StatusOK || bytes.Contains(result.Body.Bytes(), []byte("imported history")) {
		t.Fatalf("continued native prompt: %d %s", result.Code, result.Body.String())
	}
	replay := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", request, cfg)
	if replay.Code != http.StatusOK || !bytes.Contains(replay.Body.Bytes(), []byte(`"nativeRestoration"`)) {
		t.Fatalf("native create replay: %d %s", replay.Code, replay.Body.String())
	}
}

func TestSupervisorUnknownNativeInstallRetainsFrozenPublicationAndStopsAdmission(t *testing.T) {
	source, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeTestNativeRollout(t, source)
	data, err := codexstate.Capture(t.Context(), source, testNativeThreadID)
	if err != nil {
		t.Fatal(err)
	}
	server, cfg, profile := newTestServer(t, "native-resume")
	parent, err := filepath.EvalSymlinks(filepath.Dir(cfg.SessionBaseDir))
	if err != nil {
		t.Fatal(err)
	}
	server.cfg.SessionBaseDir = filepath.Join(parent, "sessions")
	create := testCreateSessionRequest(t, cfg, profile)
	create.NativeRestore = &harnessv2.NativeSessionRestore{Snapshot: harnessv2.NativeSessionSnapshot{
		Data: data, DataDigest: codexstate.DataDigest(data), ProviderSessionID: testNativeThreadID, ProviderKind: providerKindCodex, ProviderVersion: acp.CodexCLIVersion,
		RuntimeSessionUID: create.Metadata.Fence.RuntimeSessionUID, RuntimeProfileDigest: create.Metadata.Fence.RuntimeProfileDigest, WorkingDirectory: "/source/work",
	}}
	sealRequest(t, &create.Metadata.RequestDigest, create)
	journal := filepath.Join(server.cfg.SessionBaseDir, ".native-install", sessionPathID(create.Metadata.Fence.RuntimeSessionUID, create.Metadata.Fence.RuntimeSessionGeneration))
	var target string
	var published os.FileInfo
	server.cfg.Provider.PrepareSession = func(paths acp.SessionPaths) error {
		target, published, err = forceNativeReceiptPersistenceFailure(t.Context(), paths, data, journal)
		return err
	}
	response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg)
	var failure harnessv2.ErrorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil || response.Code != http.StatusConflict || failure.Code != harnessv2.ErrorCodeCleanupUnproven || failure.Retryable {
		t.Fatalf("unknown native installation: %d %s", response.Code, response.Body.String())
	}
	remaining := cfg.UIDAllocator.Remaining()
	plan, err := os.ReadFile(filepath.Join(journal, "plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	replay := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg)
	if replay.Code != response.Code || replay.Body.String() != response.Body.String() || cfg.UIDAllocator.Remaining() != remaining {
		t.Fatal("unknown create replay changed the result or allocated another identity")
	}
	status := server.status()
	if err := status.Validate(); err != nil {
		t.Fatal(err)
	}
	if status.Lifecycle != harnessv2.SupervisorLifecycleTerminating || len(status.Sessions) != 1 || !status.Sessions[0].NativeInstallUnresolved || status.Sessions[0].LiveDescendantCount != 0 {
		t.Fatal("unknown native install did not expose its blocked resident operation")
	}
	server.mu.Lock()
	state := server.sessions[create.RuntimeSessionID]
	server.mu.Unlock()
	if state.runtime != nil || state.creating || state.drainCleanupScheduled || isDrainCleanupState(state) {
		t.Fatal("unknown installation started a child or admitted destructive cleanup")
	}
	deletion := harnessv2.DeleteRuntimeSessionRequest{Protocol: harnessv2.ProtocolVersion, Metadata: testMetadata(create.Metadata.Fence, "delete-unknown-native", false), Reason: "cleanup"}
	sealRequest(t, &deletion.Metadata.RequestDigest, deletion)
	if deleted := performMutation(t, server.Handler(), http.MethodDelete, "/v2/runtime-sessions/session-1", deletion, cfg); deleted.Code != http.StatusConflict {
		t.Fatal("unreconciled native publication was deleted")
	}
	if err := server.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	retained, err := os.Stat(target)
	if err != nil || !os.SameFile(published, retained) {
		t.Fatal("unknown installation lost its original publication witness")
	}
	retainedPlan, err := os.ReadFile(filepath.Join(journal, "plan.json"))
	if err != nil || !bytes.Equal(plan, retainedPlan) {
		t.Fatal("unknown installation lost or changed its frozen plan")
	}
}

func TestCodexNativeRestoreUsesCurrentProviderAndPolicy(t *testing.T) {
	request := testProviderProjectionRequest(t, "codex", "current-model", "current instructions", "high", nil, nil, true)
	request.NativeRestore = &harnessv2.NativeSessionRestore{}
	proxy := ProviderProxyBinding{BaseURL: "http://current-provider.example/v1"}
	projection, err := codexSessionProjection(request, acp.SessionPaths{}, proxy, "current-model")
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal([]byte(projection.Environment["CODEX_CONFIG"]), &config); err != nil {
		t.Fatal(err)
	}
	if config["model_provider"] != codexProviderID || config["model"] != "current-model" || config["developer_instructions"] != "current instructions" || config["approval_policy"] != "on-request" || config["sandbox_mode"] != "read-only" {
		t.Fatalf("native current config = %#v", config)
	}
}

func validSupervisorProjection(params json.RawMessage) bool {
	var request acp.NewSessionRequest
	return json.Unmarshal(params, &request) == nil && request.Meta["provider.canary"] == providerProjectionCanaryValue &&
		request.Meta["orka.runtimeSessionID"] == "session-1" && request.Meta["orka.profileDigest"] != ""
}

func handleSupervisorNativeResume(writer *bufio.Writer, id, params json.RawMessage) {
	var request acp.ResumeSessionRequest
	cwd, _ := os.Getwd()
	installed := false
	_ = filepath.WalkDir(filepath.Join(os.Getenv("HOME"), ".codex", "sessions"), func(path string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && strings.Contains(entry.Name(), testNativeThreadID) {
			data, readErr := os.ReadFile(path)
			installed = readErr == nil && bytes.Contains(data, []byte(testNativeThreadID))
		}
		return nil
	})
	if json.Unmarshal(params, &request) != nil || request.SessionID != testNativeThreadID || request.CWD != cwd || len(request.MCPServers) != 1 || !installed {
		writeHelperMessage(writer, map[string]any{testJSONRPCKey: testJSONRPCVersion, "id": rawID(id), "error": map[string]any{"code": -32602, "message": "native resume did not find installed current-cwd rollout and MCP"}})
		return
	}
	mode := os.Getenv("SUPERVISOR_ACP_HELPER_MODE")
	if strings.HasSuffix(mode, "-held") {
		signal.Ignore(syscall.SIGTERM)
	}
	if strings.HasPrefix(mode, "native-resume-reject") {
		writeHelperMessage(writer, map[string]any{testJSONRPCKey: testJSONRPCVersion, "id": rawID(id), "error": map[string]any{"code": -32602, "message": "native resume rejected"}})
		return
	}
	writeHelperMessage(writer, map[string]any{testJSONRPCKey: testJSONRPCVersion, "id": rawID(id), "result": map[string]any{}})
}

func forceNativeReceiptPersistenceFailure(ctx context.Context, paths acp.SessionPaths, data []byte, journal string) (string, os.FileInfo, error) {
	if err := prepareCodexHome(paths); err != nil {
		return "", nil, err
	}
	receipt, err := codexstate.Install(ctx, data, filepath.Join(paths.Home, ".codex"), paths.Workspace, journal)
	if err != nil {
		return "", nil, err
	}
	target := filepath.Join(paths.Home, ".codex", receipt.TargetPath)
	published, err := os.Stat(target)
	if err != nil {
		return "", nil, err
	}
	// SessionKit has published and verified, but its exact receipt cannot be
	// durably renamed. This exercises the actual codec unknown boundary.
	if err := os.Remove(filepath.Join(journal, "receipt.json")); err != nil {
		return "", nil, err
	}
	return target, published, os.Mkdir(filepath.Join(journal, "receipt.json"), 0o700)
}

func TestSupervisorTransientNativeCaptureCanRetryRetainedHome(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("descendant exit proof requires Linux")
	}
	for _, failPersistence := range []bool{false, true} {
		t.Run(fmt.Sprint(failPersistence), func(t *testing.T) {
			server, cfg, profile := newTestServer(t, "native-capture")
			create := testCreateSessionRequest(t, cfg, profile)
			performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg)
			prompt := testStartPromptRequest(t, cfg, create.Metadata.Fence)
			performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1/prompts/prompt-1", prompt, cfg)
			server.mu.Lock()
			state := server.sessions[create.RuntimeSessionID]
			state.descriptor.State = harnessv2.RuntimeSessionStateIdle
			server.mu.Unlock()
			home := filepath.Join(state.paths.Home, ".codex")
			blocked := filepath.Join(state.paths.Root, ".native-session-snapshot.json")
			if failPersistence {
				writeTestNativeRollout(t, home)
				if err := os.Mkdir(blocked, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			request := nativeCaptureRequest(t, create.Metadata.Fence)
			path := "/v2/runtime-sessions/session-1/native-session"
			first := performMutation(t, server.Handler(), http.MethodPost, path, request, cfg)
			var failure harnessv2.ErrorResponse
			if err := json.Unmarshal(first.Body.Bytes(), &failure); err != nil || failure.Code != harnessv2.ErrorCodeNativeCaptureRetryReady || !failure.Retryable {
				t.Fatalf("retry proof: %d %s", first.Code, first.Body.String())
			}
			old := state.nativeCapture
			if failPersistence {
				if err := os.Remove(blocked); err != nil {
					t.Fatal(err)
				}
			} else {
				writeTestNativeRollout(t, home)
			}
			duplicate := performMutation(t, server.Handler(), http.MethodPost, path, request, cfg)
			if !bytes.Equal(first.Body.Bytes(), duplicate.Body.Bytes()) || state.nativeCapture != old {
				t.Fatal("original capture receipt changed on replay")
			}
			reconcile := request
			reconcile.Metadata = testMetadata(create.Metadata.Fence, "reconcile-native-transient", false)
			reconcile.OriginalOperationID, reconcile.OriginalRequestDigest = request.Metadata.OperationID, request.Metadata.RequestDigest
			sealRequest(t, &reconcile.Metadata.RequestDigest, reconcile)
			proof := performMutation(t, server.Handler(), http.MethodPost, path, reconcile, cfg)
			if !bytes.Equal(first.Body.Bytes(), proof.Body.Bytes()) || state.nativeCapture != old {
				t.Fatal("reconciliation began another capture")
			}
			retry := request
			retry.Metadata = testMetadata(create.Metadata.Fence, "retry-native-transient", false)
			sealRequest(t, &retry.Metadata.RequestDigest, retry)
			response := performMutation(t, server.Handler(), http.MethodPost, path, retry, cfg)
			if response.Code != http.StatusOK {
				t.Fatalf("retry capture: %d %s", response.Code, response.Body.String())
			}
			var captured harnessv2.CaptureNativeSessionResponse
			if err := json.Unmarshal(response.Body.Bytes(), &captured); err != nil || captured.Snapshot.ProviderSessionID != testNativeThreadID || state.nativeCapture == old {
				t.Fatal("retry did not preserve the native thread")
			}
			current := state.nativeCapture
			lateReplay := performMutation(t, server.Handler(), http.MethodPost, path, request, cfg)
			if !bytes.Equal(first.Body.Bytes(), lateReplay.Body.Bytes()) || state.nativeCapture != current {
				t.Fatal("successful retry discarded the original failed receipt")
			}
			lateReconciliation := performMutation(t, server.Handler(), http.MethodPost, path, reconcile, cfg)
			if !bytes.Equal(first.Body.Bytes(), lateReconciliation.Body.Bytes()) || state.nativeCapture != current {
				t.Fatal("late reconciliation lost the original immutable receipt")
			}
			changed := request
			changed.Metadata.RequestDigest = harnessv2.RequestDigest(testDigest("different capture"))
			conflict := performMutation(t, server.Handler(), http.MethodPost, path, changed, cfg)
			if conflict.Code == http.StatusOK {
				t.Fatal("old operation accepted a changed digest")
			}

		})
	}
}

func TestSupervisorNativeCaptureRetryRejectsDeletionReservation(t *testing.T) {
	server, cfg, profile := newTestServer(t, "native-capture")
	create := testCreateSessionRequest(t, cfg, profile)
	performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg)
	prompt := testStartPromptRequest(t, cfg, create.Metadata.Fence)
	performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1/prompts/prompt-1", prompt, cfg)
	original := nativeCaptureRequest(t, create.Metadata.Fence)
	done := make(chan struct{})
	close(done)
	capture := &nativeSessionCapture{request: original, done: done, finished: true, retryable: true,
		failure: "transient capture failure", failureCode: harnessv2.ErrorCodeNativeCaptureRetryReady}
	server.mu.Lock()
	state := server.sessions[create.RuntimeSessionID]
	state.nativeCapture = capture
	state.nativeCaptureReceipts = map[harnessv2.OperationID]*nativeSessionCapture{original.Metadata.OperationID: capture}
	state.descriptor.State = harnessv2.RuntimeSessionStatePoisoned
	// DELETE reserves Deleting under the same mutex before touching the home.
	ready, err := prepareSessionDeletionLocked(state, false, time.Now().UTC())
	server.mu.Unlock()
	if err != nil || !ready {
		t.Fatalf("deletion reservation: %v %v", ready, err)
	}
	retry := original
	retry.Metadata = testMetadata(create.Metadata.Fence, "retry-after-delete-reservation", false)
	sealRequest(t, &retry.Metadata.RequestDigest, retry)
	response := performMutation(t, server.Handler(), http.MethodPost, "/v2/runtime-sessions/session-1/native-session", retry, cfg)
	if response.Code != http.StatusConflict {
		t.Fatalf("retry after deletion: %d %s", response.Code, response.Body.String())
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if state.nativeCapture != capture || len(state.nativeCaptureReceipts) != 1 || state.descriptor.State != harnessv2.RuntimeSessionStateDeleting {
		t.Fatal("retry replaced capture evidence or escaped deletion reservation")
	}
}
