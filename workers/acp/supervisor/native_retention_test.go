package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/orka-agents/sessionkit"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func TestSupervisorRestoredNativeFailureRetainsEvidenceUntilProvenDelete(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("descendant exit proof requires Linux")
	}
	for _, failure := range []string{"provider-prompt", "workspace-validation"} {
		t.Run(failure, func(t *testing.T) {
			mode := "native-resume-held"
			if failure == "provider-prompt" {
				mode = "native-prompt-error-held"
			}
			fixture := newNativeCreateFixture(t, mode)
			response := performMutation(t, fixture.server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", fixture.request, fixture.cfg)
			if response.Code != http.StatusCreated {
				t.Fatalf("native create: %d %s", response.Code, response.Body.String())
			}
			fixture.server.mu.Lock()
			state := fixture.server.sessions[fixture.request.RuntimeSessionID]
			fixture.server.mu.Unlock()
			if state.descriptor.NativeRestoration == nil || !state.descriptor.NativeRestoration.Loaded {
				t.Fatal("fixture did not load the native continuation")
			}
			evidence := readRestoredNativeEvidence(t, fixture)
			prompt := testStartPromptRequest(t, fixture.cfg, fixture.request.Metadata.Fence)
			response = performMutation(t, fixture.server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1/prompts/prompt-1", prompt, fixture.cfg)
			if response.Code != http.StatusOK {
				t.Fatalf("native prompt: %d %s", response.Code, response.Body.String())
			}
			if failure == "provider-prompt" {
				if !bytes.Contains(response.Body.Bytes(), []byte("json-rpc error -32603")) {
					t.Fatal("prompt did not expose the provider RPC failure classification")
				}
				fixture.server.mu.Lock()
				failed := state.prompt.settlement != nil && state.prompt.settlement.TerminalEvent == harnessv2.EventFailed
				fixture.server.mu.Unlock()
				if !failed {
					t.Fatal("provider rejection did not settle as failed")
				}
			} else {
				poisonRestoredNativeWorkspace(t, fixture, state, prompt)
			}
			capture := nativeCaptureRequest(t, fixture.request.Metadata.Fence)
			response = performMutation(t, fixture.server.Handler(), http.MethodPost, "/v2/runtime-sessions/session-1/native-session", capture, fixture.cfg)
			if response.Code != http.StatusConflict {
				t.Fatalf("failed continuation allowed fresh capture: %d %s", response.Code, response.Body.String())
			}
			assertRestoredNativeEvidence(t, fixture, state, evidence)
			drainRestoredNativeSession(t, fixture)
			assertRestoredNativeEvidence(t, fixture, state, evidence)
			deleteRestoredNativeAfterProof(t, fixture, state, evidence)
		})
	}
}

func TestSupervisorOrdinaryNativeFailureRetiresWithoutCheckpoint(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("descendant exit proof requires Linux")
	}
	fixture := newNativeCreateFixture(t, "native-prompt-error")
	fixture.request.NativeRestore = nil
	sealRequest(t, &fixture.request.Metadata.RequestDigest, fixture.request)
	response := performMutation(t, fixture.server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", fixture.request, fixture.cfg)
	if response.Code != http.StatusCreated {
		t.Fatalf("ordinary create: %d %s", response.Code, response.Body.String())
	}
	prompt := testStartPromptRequest(t, fixture.cfg, fixture.request.Metadata.Fence)
	response = performMutation(t, fixture.server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1/prompts/prompt-1", prompt, fixture.cfg)
	if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte("json-rpc error -32603")) {
		t.Fatalf("ordinary failed prompt: %d %s", response.Code, response.Body.String())
	}
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		fixture.server.mu.Lock()
		_, resident := fixture.server.sessions[fixture.request.RuntimeSessionID]
		_, retired := fixture.server.tombstones[fixture.request.Metadata.Fence.RuntimeSessionUID]
		fixture.server.mu.Unlock()
		if !resident && retired {
			assertNativeCreatePathsAbsent(t, fixture)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("ordinary failed runtime without native continuity was retained")
}

type restoredNativeEvidence struct {
	plan      []byte
	receipt   []byte
	target    string
	published os.FileInfo
}

func readRestoredNativeEvidence(t *testing.T, fixture *nativeCreateFixture) restoredNativeEvidence {
	t.Helper()
	evidence := restoredNativeEvidence{
		plan:    readNativeJournalFile(t, fixture.journal, "plan.json"),
		receipt: readNativeJournalFile(t, fixture.journal, "receipt.json"),
	}
	var receipt sessionkit.Receipt
	if err := json.Unmarshal(evidence.receipt, &receipt); err != nil {
		t.Fatal(err)
	}
	evidence.target = filepath.Join(fixture.paths.Home, ".codex", receipt.TargetPath)
	var err error
	evidence.published, err = os.Stat(evidence.target)
	if err != nil {
		t.Fatal(err)
	}
	return evidence
}

func assertRestoredNativeEvidence(t *testing.T, fixture *nativeCreateFixture, state *sessionState, evidence restoredNativeEvidence) {
	t.Helper()
	fixture.server.mu.Lock()
	retained := fixture.server.sessions[fixture.request.RuntimeSessionID] == state &&
		state.descriptor.State == harnessv2.RuntimeSessionStatePoisoned && state.nativeCapture == nil
	fixture.server.mu.Unlock()
	if !retained {
		t.Fatal("failed native continuation was retired or recaptured")
	}
	if _, err := os.Stat(fixture.paths.Home); err != nil {
		t.Fatal("failed native continuation lost its exact private home")
	}
	if !bytes.Equal(evidence.plan, readNativeJournalFile(t, fixture.journal, "plan.json")) || !bytes.Equal(evidence.receipt, readNativeJournalFile(t, fixture.journal, "receipt.json")) {
		t.Fatal("failed native continuation changed its frozen installation evidence")
	}
	retainedTarget, err := os.Stat(evidence.target)
	if err != nil || !os.SameFile(evidence.published, retainedTarget) {
		t.Fatal("failed native continuation lost its original installed rollout")
	}
}

func poisonRestoredNativeWorkspace(t *testing.T, fixture *nativeCreateFixture, state *sessionState, prompt harnessv2.StartPromptRequest) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(fixture.paths.Workspace, "unexpected.txt"), []byte("read-only workspace changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.server.mu.Lock()
	settlementDigest := state.prompt.settlementDigest
	fixture.server.mu.Unlock()
	request := harnessv2.CreateWorkspaceDeltaRequest{
		Protocol: harnessv2.ProtocolVersion, Metadata: prompt.Metadata, DeltaID: "native-poisoned-delta",
		Intent: fixture.request.Workspace.Intent, VerifiedBaseline: fixture.request.Workspace.Baseline,
		PromptSettlementDigest: settlementDigest, Limits: harnessv2.WorkspaceDeltaLimits{MaxBytes: 1 << 20, MaxEntries: 100},
	}
	request.Metadata.OperationID = "native-poisoned-delta"
	sealRequest(t, &request.Metadata.RequestDigest, request)
	response := performMutation(t, fixture.server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1/workspace-deltas/native-poisoned-delta", request, fixture.cfg)
	var delta harnessv2.CreateWorkspaceDeltaResponse
	if err := json.Unmarshal(response.Body.Bytes(), &delta); err != nil || response.Code != http.StatusOK || delta.Delta.State != harnessv2.WorkspaceDeltaReadOnlyModified {
		t.Fatalf("read-only native workspace validation: %d %s", response.Code, response.Body.String())
	}
}

func drainRestoredNativeSession(t *testing.T, fixture *nativeCreateFixture) {
	t.Helper()
	drain := harnessv2.DrainRequest{Protocol: harnessv2.ProtocolVersion, Metadata: harnessv2.MutationMetadata{
		Fence: fixture.cfg.Fence, OperationID: "drain-failed-native", RequestDigestSchemaVersion: harnessv2.RequestDigestSchemaVersion,
		ExpiresAt: time.Now().UTC().Add(time.Minute),
	}, Reason: "runtime replacement"}
	sealRequest(t, &drain.Metadata.RequestDigest, drain)
	response := performMutation(t, fixture.server.Handler(), http.MethodPut, harnessv2.DrainPath, drain, fixture.cfg)
	if response.Code != http.StatusOK {
		t.Fatalf("native drain: %d %s", response.Code, response.Body.String())
	}
}

func deleteRestoredNativeAfterProof(t *testing.T, fixture *nativeCreateFixture, state *sessionState, evidence restoredNativeEvidence) {
	t.Helper()
	deletion := harnessv2.DeleteRuntimeSessionRequest{Protocol: harnessv2.ProtocolVersion,
		Metadata: testMetadata(fixture.request.Metadata.Fence, "delete-retained-native", false), Reason: "explicit cleanup"}
	sealRequest(t, &deletion.Metadata.RequestDigest, deletion)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	request := mutationHTTPRequest(t, http.MethodDelete, "/v2/runtime-sessions/session-1", deletion, fixture.cfg).WithContext(ctx)
	response := httptest.NewRecorder()
	fixture.server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("unproven native deletion: %d %s", response.Code, response.Body.String())
	}
	assertRestoredNativeEvidence(t, fixture, state, evidence)
	closeCtx, closeCancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer closeCancel()
	if err := fixture.server.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-state.runtime.Process().Done():
	default:
		t.Fatal("shutdown retained a live native writer")
	}
	if cleanup, err := state.runtime.Delete(closeCtx); err != nil || !cleanup.Proven {
		t.Fatalf("shutdown failed to prove native writer exit: %#v %v", cleanup, err)
	}
	assertRestoredNativeEvidence(t, fixture, state, evidence)
	deleted := performMutation(t, fixture.server.Handler(), http.MethodDelete, "/v2/runtime-sessions/session-1", deletion, fixture.cfg)
	if deleted.Code != http.StatusOK {
		t.Fatalf("proven explicit native deletion: %d %s", deleted.Code, deleted.Body.String())
	}
	assertNativeCreatePathsAbsent(t, fixture)
}
