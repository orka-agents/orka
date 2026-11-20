package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/workspacedelta"
)

type rebaseFixture struct {
	server  *Server
	cfg     Config
	create  harnessv2.CreateRuntimeSessionRequest
	prompt  harnessv2.StartPromptRequest
	state   *sessionState
	root    string
	request harnessv2.RebaseRuntimeSessionRequest
}

// newRebaseFixture builds a write session that finalized a verified-exact
// publication of a real prepared delta, which is what ADR 0059 rebases.
func newRebaseFixture(t *testing.T) *rebaseFixture {
	t.Helper()
	server, cfg, profile := newRebaseTestServer(t)
	create := testCreateSessionRequest(t, cfg, profile)
	root := t.TempDir()
	writeRebaseFile(t, root, "main.go", "package main\n")
	baseline, err := workspacedelta.Capture(root, workspacedelta.Options{})
	if err != nil {
		t.Fatal(err)
	}
	writeRebaseFile(t, root, "main.go", "package main\n\nfunc main() {}\n")
	result, err := workspacedelta.BuildWithLimits(baseline, root, workspacedelta.IntentWrite, workspacedelta.BuildLimits{})
	if err != nil || result.Classification != workspacedelta.ClassificationWriteDelta {
		t.Fatalf("build delta = %v, %v", result.Classification, err)
	}
	prompt := testStartPromptRequest(t, cfg, create.Metadata.Fence)
	now := time.Now().UTC()
	settlement := harnessv2.PromptSettlement{
		TerminalEvent: harnessv2.EventCompleted, Outcome: harnessv2.PromptOutcomeSucceeded,
		StopReason: harnessv2.ACPStopReasonEndTurn, SettledAt: now,
	}
	receiptDigest := testDigest("publication-receipt")
	state := &sessionState{
		id: create.RuntimeSessionID,
		descriptor: harnessv2.RuntimeSessionDescriptor{
			RuntimeSessionID: create.RuntimeSessionID, RuntimeSessionUID: create.Metadata.Fence.RuntimeSessionUID,
			Generation: create.Metadata.Fence.RuntimeSessionGeneration, RuntimeInstanceID: cfg.Fence.RuntimeInstanceID,
			SupervisorBootID: cfg.Fence.SupervisorBootID, RuntimeProfileDigest: cfg.Fence.RuntimeProfileDigest,
			State: harnessv2.RuntimeSessionStateFinalizing, ProviderSessionID: "provider-session",
			WorkspaceBaseline: create.Workspace.Baseline, CreatedAt: now, LastTransitionAt: now,
		},
		runtime:         &acp.RuntimeSession{},
		operations:      make(map[harnessv2.OperationID]harnessv2.OperationRecord),
		permissions:     make(map[harnessv2.PermissionRequestID]permissionState),
		deltas:          make(map[harnessv2.WorkspaceDeltaID]harnessv2.CreateWorkspaceDeltaResponse),
		workspaceIntent: harnessv2.WorkspaceIntentWrite,
		baseline:        baseline,
		prompt:          &promptState{request: prompt, settlement: &settlement},
		publicationFinalization: &harnessv2.PublicationFinalizationReceipt{
			WorkspaceDeltaID: "delta-1", PublicationID: "publication-1", PublicationGeneration: 1, PublicationVersion: 1,
			TerminalState: harnessv2.PublicationTerminalVerifiedExact, TerminalReceiptDigest: receiptDigest, AppliedAt: now,
		},
		preparedDeltas: map[harnessv2.WorkspaceDeltaID]preparedDeltaChanges{"delta-1": newPreparedDeltaChanges(result)},
	}
	state.deltas["delta-1"] = harnessv2.CreateWorkspaceDeltaResponse{Delta: harnessv2.WorkspaceDeltaDescriptor{
		DeltaID: "delta-1", RuntimeSessionUID: state.descriptor.RuntimeSessionUID, SessionGeneration: state.descriptor.Generation,
		State: harnessv2.WorkspaceDeltaPrepared, PublicationSafe: true, ManifestDigest: result.ManifestDigest,
	}}
	server.mu.Lock()
	server.sessions[create.RuntimeSessionID] = state
	server.mu.Unlock()

	nextRevision := "fedcba9876543210"
	tree, err := harnessv2.RebasedWorkspaceTreeDigest(create.Workspace.Baseline.TreeDigest, result.ManifestDigest, nextRevision)
	if err != nil {
		t.Fatal(err)
	}
	metadata := prompt.Metadata
	metadata.OperationID = "rebase-session-g1"
	metadata.ExpiresAt = now.Add(time.Minute)
	metadata.RequestDigest = ""
	request := harnessv2.RebaseRuntimeSessionRequest{
		Protocol: harnessv2.ProtocolVersion, Metadata: metadata, WorkspaceDeltaID: "delta-1",
		PublicationID: "publication-1", TerminalReceiptDigest: receiptDigest, PublishedManifestDigest: result.ManifestDigest,
		NextBaseline: harnessv2.WorkspaceBaseline{RepositoryIdentity: "github.com/orka-agents/orka", Revision: nextRevision, TreeDigest: tree},
	}
	sealRequest(t, &request.Metadata.RequestDigest, request)
	return &rebaseFixture{server: server, cfg: cfg, create: create, prompt: prompt, state: state, root: root, request: request}
}

// newRebaseTestServer is a supervisor that never starts an ACP child, so the
// rebase tests run without identity-isolation privileges.
func newRebaseTestServer(t *testing.T) (*Server, Config, harnessv2.RuntimeProfile) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}))
	t.Cleanup(upstream.Close)
	cfg, profile := newTestConfigWithoutChildren(t, "immediate", upstream.URL, strings.Repeat("p", 32))
	cfg.Capabilities.SupportsRuntimeSessionRebase = true
	server, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	server.thawRuntime = func(*acp.RuntimeSession) error { return nil }
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Close(ctx)
	})
	return server, cfg, profile
}

func writeRebaseFile(t *testing.T, root, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *rebaseFixture) rebase(t *testing.T, request harnessv2.RebaseRuntimeSessionRequest) *httptest.ResponseRecorder {
	t.Helper()
	return performMutation(t, f.server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1/rebase", request, f.cfg)
}

func (f *rebaseFixture) reseal(t *testing.T, mutate func(*harnessv2.RebaseRuntimeSessionRequest)) harnessv2.RebaseRuntimeSessionRequest {
	t.Helper()
	request := f.request
	mutate(&request)
	request.Metadata.RequestDigest = ""
	sealRequest(t, &request.Metadata.RequestDigest, request)
	return request
}

func TestSupervisorRebaseFoldsThePublishedDeltaAndReturnsTheSessionToIdle(t *testing.T) {
	f := newRebaseFixture(t)
	response := f.rebase(t, f.request)
	if response.Code != http.StatusOK {
		t.Fatalf("rebase status=%d body=%s", response.Code, response.Body.String())
	}
	var rebased harnessv2.RebaseRuntimeSessionResponse
	if err := json.Unmarshal(response.Body.Bytes(), &rebased); err != nil {
		t.Fatal(err)
	}
	if err := rebased.ValidateFor(f.request); err != nil {
		t.Fatalf("rebase response validation: %v", err)
	}
	f.server.mu.Lock()
	state, baseline := f.state.descriptor.State, f.state.baseline
	finalization := f.state.publicationFinalization
	f.server.mu.Unlock()
	if state != harnessv2.RuntimeSessionStateIdle || finalization != nil {
		t.Fatalf("session after rebase = %s with finalization %v, want idle", state, finalization)
	}
	// The derived baseline is the published tree: no delta remains.
	next, err := workspacedelta.BuildWithLimits(baseline, f.root, workspacedelta.IntentWrite, workspacedelta.BuildLimits{})
	if err != nil || next.Classification != workspacedelta.ClassificationNoChange {
		t.Fatalf("delta from the rebased baseline = %v, %v", next.Classification, err)
	}
	f.server.mu.Lock()
	quiescent := sessionQuiescentLocked(f.state, 1)
	f.server.mu.Unlock()
	status := f.server.status()
	if !quiescent || len(status.Sessions) != 1 || status.Sessions[0].Validate() != nil {
		t.Fatalf("rebased session quiescent=%v status=%#v", quiescent, status.Sessions)
	}

	duplicate := f.rebase(t, f.request)
	if duplicate.Code != http.StatusOK {
		t.Fatalf("duplicate rebase status=%d body=%s", duplicate.Code, duplicate.Body.String())
	}
	if err := json.Unmarshal(duplicate.Body.Bytes(), &rebased); err != nil {
		t.Fatal(err)
	}
	if rebased.Classification.Class != harnessv2.RequestClassificationDuplicate {
		t.Fatalf("duplicate rebase classification = %#v", rebased.Classification)
	}
	// A second fresh rebase of the same publication is refused: the session
	// is no longer finalizing.
	again := f.reseal(t, func(r *harnessv2.RebaseRuntimeSessionRequest) { r.Metadata.OperationID = "rebase-again" })
	if response := f.rebase(t, again); response.Code != http.StatusConflict {
		t.Fatalf("second rebase status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestSupervisorRebaseRefusesEverythingButTheExactVerifiedPublication(t *testing.T) {
	cases := map[string]struct {
		mutateState   func(*sessionState)
		mutateRequest func(*harnessv2.RebaseRuntimeSessionRequest)
		code          harnessv2.ErrorCode
	}{
		"not finalizing": {
			mutateState: func(s *sessionState) { s.descriptor.State = harnessv2.RuntimeSessionStatePublicationPrepared },
			code:        harnessv2.ErrorCodeSessionPoisoned,
		},
		"read intent": {
			mutateState: func(s *sessionState) { s.workspaceIntent = harnessv2.WorkspaceIntentRead },
			code:        harnessv2.ErrorCodeSessionPoisoned,
		},
		"superseded publication": {
			mutateState: func(s *sessionState) {
				s.publicationFinalization.TerminalState = harnessv2.PublicationTerminalDeliveredSuperseded
			},
			code: harnessv2.ErrorCodeDigestConflict,
		},
		"receipt mismatch": {
			mutateRequest: func(r *harnessv2.RebaseRuntimeSessionRequest) { r.TerminalReceiptDigest = testDigest("other-receipt") },
			code:          harnessv2.ErrorCodeDigestConflict,
		},
		"manifest mismatch": {
			mutateRequest: func(r *harnessv2.RebaseRuntimeSessionRequest) {
				r.PublishedManifestDigest = testDigest("other-manifest")
			},
			code: harnessv2.ErrorCodeDigestConflict,
		},
		"forged next tree": {
			mutateRequest: func(r *harnessv2.RebaseRuntimeSessionRequest) { r.NextBaseline.TreeDigest = testDigest("forged-tree") },
			code:          harnessv2.ErrorCodeDigestConflict,
		},
		"pending permission": {
			mutateState: func(s *sessionState) {
				s.permissions["permission-1"] = permissionState{requestID: "permission-1"}
			},
			code: harnessv2.ErrorCodeSessionPoisoned,
		},
		"other prompt": {
			mutateState: func(s *sessionState) { s.prompt.request.Metadata.PromptID = "prompt-other" },
			code:        harnessv2.ErrorCodeSessionPoisoned,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newRebaseFixture(t)
			if tc.mutateState != nil {
				f.server.mu.Lock()
				tc.mutateState(f.state)
				f.server.mu.Unlock()
			}
			request := f.request
			if tc.mutateRequest != nil {
				request = f.reseal(t, tc.mutateRequest)
			}
			response := f.rebase(t, request)
			if response.Code != http.StatusConflict {
				t.Fatalf("rebase status=%d body=%s, want 409", response.Code, response.Body.String())
			}
			var decoded harnessv2.ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.Code != tc.code {
				t.Fatalf("rebase error = %#v, want %s", decoded, tc.code)
			}
			f.server.mu.Lock()
			defer f.server.mu.Unlock()
			if f.state.descriptor.State == harnessv2.RuntimeSessionStateIdle {
				t.Fatal("a refused rebase must never return the session to idle")
			}
		})
	}
}

func TestSupervisorRebaseRefusesADeltaThatNoLongerApplies(t *testing.T) {
	f := newRebaseFixture(t)
	f.server.mu.Lock()
	prepared := f.state.preparedDeltas["delta-1"]
	prepared.changes = append(prepared.changes, workspacedelta.Change{
		Path: "main.go", Operation: workspacedelta.ChangeAdded, Kind: workspacedelta.EntryFile, Mode: 0o644, Size: 1,
		Digest: testDigest("x"),
	})
	f.state.preparedDeltas["delta-1"] = prepared
	f.server.mu.Unlock()
	if response := f.rebase(t, f.request); response.Code != http.StatusConflict {
		t.Fatalf("rebase status=%d body=%s, want 409", response.Code, response.Body.String())
	}
	f.server.mu.Lock()
	defer f.server.mu.Unlock()
	if f.state.descriptor.State != harnessv2.RuntimeSessionStateFinalizing {
		t.Fatalf("session after a refused rebase = %s, want still finalizing and deletable", f.state.descriptor.State)
	}
}

func TestSupervisorRebaseThawFailurePoisonsTheSession(t *testing.T) {
	f := newRebaseFixture(t)
	f.server.thawRuntime = func(*acp.RuntimeSession) error { return errors.New("thaw unproven") }
	if response := f.rebase(t, f.request); response.Code != http.StatusInternalServerError {
		t.Fatalf("rebase status=%d body=%s, want 500", response.Code, response.Body.String())
	}
	f.server.mu.Lock()
	defer f.server.mu.Unlock()
	if f.state.descriptor.State != harnessv2.RuntimeSessionStatePoisoned {
		t.Fatalf("session after a failed thaw = %s, want poisoned", f.state.descriptor.State)
	}
}

func TestSupervisorAdvertisesRebaseAndQuiescenceOnlyOnRequest(t *testing.T) {
	f := newRebaseFixture(t)
	for _, optIn := range []bool{false, true} {
		req := httptest.NewRequest(http.MethodGet, harnessv2.CapabilitiesPath, nil)
		if optIn {
			req.Header.Set(harnessv2.RuntimeSessionRebaseHeader, "1")
		}
		resp := httptest.NewRecorder()
		f.server.Handler().ServeHTTP(resp, req)
		var capabilities harnessv2.CapabilitiesResponse
		if err := json.Unmarshal(resp.Body.Bytes(), &capabilities); err != nil {
			t.Fatal(err)
		}
		if capabilities.SupportsRuntimeSessionRebase != optIn {
			t.Fatalf("capabilities with opt-in=%v advertise rebase=%v", optIn, capabilities.SupportsRuntimeSessionRebase)
		}
	}
	f.server.mu.Lock()
	defer f.server.mu.Unlock()
	f.state.descriptor.State = harnessv2.RuntimeSessionStateIdle
	if !sessionQuiescentLocked(f.state, 1) {
		t.Fatal("an idle settled session with a live process is quiescent")
	}
	if sessionQuiescentLocked(f.state, 0) {
		t.Fatal("a session whose provider process exited is not quiescent")
	}
	f.state.permissions["permission-1"] = permissionState{requestID: "permission-1"}
	if sessionQuiescentLocked(f.state, 1) {
		t.Fatal("a pending permission is not quiescent")
	}
	delete(f.state.permissions, "permission-1")
	f.state.prompt.settlement = nil
	if sessionQuiescentLocked(f.state, 1) {
		t.Fatal("an unsettled prompt is not quiescent")
	}
}
