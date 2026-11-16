package supervisor

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

// runFirstPromptThenAwaitAdapterExit creates session-1, completes prompt-1
// against a helper that exits right after answering, waits for that exit, and
// returns the session state to Idle the way completed workspace validation
// would.
func runFirstPromptThenAwaitAdapterExit(t *testing.T, mode string) (*Server, Config, harnessv2.CreateRuntimeSessionRequest, *sessionState) {
	t.Helper()
	return runFirstPromptThenAwaitAdapterExitWith(t, mode, nil)
}

func runFirstPromptThenAwaitAdapterExitWith(t *testing.T, mode string, configure func(*Server)) (*Server, Config, harnessv2.CreateRuntimeSessionRequest, *sessionState) {
	t.Helper()
	server, cfg, profile := newTestServer(t, mode)
	if configure != nil {
		configure(server)
	}
	create := testCreateSessionRequest(t, cfg, profile)
	created := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", created.Code, created.Body.String())
	}
	first := testStartPromptRequest(t, cfg, create.Metadata.Fence)
	completed := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1/prompts/prompt-1", first, cfg)
	if completed.Code != http.StatusOK {
		t.Fatalf("first prompt status = %d body=%s", completed.Code, completed.Body.String())
	}
	server.mu.Lock()
	state := server.sessions[create.RuntimeSessionID]
	server.mu.Unlock()
	if state == nil || state.runtime == nil {
		t.Fatal("session state missing after first prompt")
	}
	select {
	case <-state.runtime.Process().Done():
	case <-time.After(10 * time.Second):
		t.Fatal("helper adapter did not exit after the first prompt")
	}
	server.mu.Lock()
	state.descriptor.State = harnessv2.RuntimeSessionStateIdle
	server.mu.Unlock()
	deactivatePromptCapabilities(state, first.Metadata.PromptID, harnessv2.RuntimeSessionStateIdle)
	return server, cfg, create, state
}

func testSecondPromptRequest(t *testing.T, cfg Config, fence harnessv2.Fence) harnessv2.StartPromptRequest {
	t.Helper()
	next := testStartPromptRequest(t, cfg, fence)
	next.Metadata.OperationID = testPromptOperationTwo
	next.Metadata.PromptID = testPromptTwoID
	next.MCPAuthorization.TaskUID = next.Metadata.TaskUID
	next.MCPAuthorization.TaskAttempt = next.Metadata.TaskAttempt
	next.MCPAuthorization.PromptID = next.Metadata.PromptID
	next.Metadata.RequestDigest = ""
	sealRequest(t, &next.Metadata.RequestDigest, next)
	return next
}

func TestSupervisorResumesIdleAdapterInPlace(t *testing.T) {
	server, cfg, create, state := runFirstPromptThenAwaitAdapterExit(t, adapterResumeMode)
	next := testSecondPromptRequest(t, cfg, create.Metadata.Fence)
	response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1/prompts/prompt-2", next, cfg)
	if response.Code != http.StatusOK {
		t.Fatalf("prompt after adapter exit status = %d body=%s", response.Code, response.Body.String())
	}
	decoder, err := harnessv2.NewEventDecoder(bytes.NewReader(response.Body.Bytes()), eventLimits(cfg.Capabilities.Limits), harnessv2.EventExpectationFromMetadata(next.Metadata))
	if err != nil {
		t.Fatal(err)
	}
	events, err := decoder.DecodeAll()
	if err != nil {
		t.Fatalf("decode resumed prompt events: %v\n%s", err, response.Body.String())
	}
	if len(events) == 0 || events[len(events)-1].Type != harnessv2.EventCompleted {
		t.Fatalf("resumed prompt did not complete: %#v", events)
	}
	if state.runtime.AdapterRestarts() != 1 {
		t.Fatalf("adapter restarts = %d, want 1", state.runtime.AdapterRestarts())
	}
	server.mu.Lock()
	sessionState := state.descriptor.State
	server.mu.Unlock()
	if sessionState == harnessv2.RuntimeSessionStatePoisoned {
		t.Fatal("resumed session was poisoned")
	}
}

// An agent may call the model while session/resume reconnects it (Claude Agent
// ACP 0.89.1 sends one request). The restart runs before the next prompt gets
// provider access, so the proxy refuses that call rather than spending one of
// the prompt's turns on it.
func TestSupervisorRefusesModelCallsDuringIdleAdapterResume(t *testing.T) {
	server, cfg, create, state := runFirstPromptThenAwaitAdapterExit(t, adapterResumeModelCallMode)
	next := testSecondPromptRequest(t, cfg, create.Metadata.Fence)
	response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1/prompts/prompt-2", next, cfg)
	if response.Code != http.StatusOK {
		t.Fatalf("prompt after adapter exit status = %d body=%s", response.Code, response.Body.String())
	}
	if state.runtime.AdapterRestarts() != 1 {
		t.Fatalf("adapter restarts = %d, want 1", state.runtime.AdapterRestarts())
	}
	status, err := os.ReadFile(filepath.Join(state.paths.Home, helperResumeModelCallFile))
	if err != nil {
		t.Fatalf("read resume model-call status: %v", err)
	}
	if string(status) != strconv.Itoa(http.StatusForbidden) {
		t.Fatalf("model call during resume returned %q, want %d", status, http.StatusForbidden)
	}
}

func TestSupervisorRetiresSessionWhenIdleAdapterCannotResume(t *testing.T) {
	for _, mode := range []string{adapterExitAfterPromptMode, adapterResumeFailureMode} {
		t.Run(mode, func(t *testing.T) {
			assertSupervisorRetiresUnresumableAdapter(t, mode)
		})
	}
}

func assertSupervisorRetiresUnresumableAdapter(t *testing.T, mode string) {
	t.Helper()
	server, cfg, create, state := runFirstPromptThenAwaitAdapterExit(t, mode)
	next := testSecondPromptRequest(t, cfg, create.Metadata.Fence)
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	client, err := harnessv2.NewClient(httpServer.URL,
		harnessv2.WithControllerBearerToken(cfg.ControllerBearerToken),
		harnessv2.WithOperationCapabilitySecret(cfg.CapabilitySecret),
	)
	if err != nil {
		t.Fatal(err)
	}
	emitted := false
	summary, err := client.StreamPrompt(t.Context(), create.RuntimeSessionID, next, func(harnessv2.Event) error {
		emitted = true
		return nil
	})
	var failure *harnessv2.ClientError
	if !errors.As(err, &failure) || failure.Kind != harnessv2.ClientErrorHTTP ||
		failure.Code != harnessv2.ErrorCodePromptNotAccepted || failure.StatusCode != http.StatusConflict || failure.Retryable {
		t.Fatalf("error = %v, want non-retryable prompt-not-accepted HTTP rejection", err)
	}
	if emitted || summary.Accepted || summary.WriteEvidence.State != harnessv2.RequestWriteComplete ||
		failure.WriteEvidence.SafeToResendSameIdentity() || failure.WriteEvidence.RequestBodyBytesRead == 0 {
		t.Fatalf("idle resume failure altered acceptance or write evidence: summary=%#v error=%#v", summary, failure)
	}
	if _, exists := state.runtime.Tombstone(string(next.Metadata.PromptID)); exists {
		t.Fatal("failed idle resume submitted the next ACP prompt")
	}
	if state.runtime.AdapterRestarts() != 0 {
		t.Fatalf("adapter restarts = %d, want 0", state.runtime.AdapterRestarts())
	}
	// The poisoned session is retired by the supervisor's own cleanup; the
	// controller recreates a fresh generation from the canonical transcript.
	deadline := time.Now().Add(10 * time.Second)
	for {
		status := server.status()
		retired := len(status.Sessions) == 0
		for _, session := range status.Sessions {
			if session.RuntimeSessionUID == create.Metadata.Fence.RuntimeSessionUID {
				retired = session.State == harnessv2.RuntimeSessionStatePoisoned || session.State == harnessv2.RuntimeSessionStateDeleting
			}
		}
		if retired {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("lost-adapter session was not retired: %#v", status.Sessions)
		}
		time.Sleep(20 * time.Millisecond)
	}
	server.mu.Lock()
	_, resident := server.sessions[create.RuntimeSessionID]
	_, tombstoned := server.tombstones[create.Metadata.Fence.RuntimeSessionUID]
	server.mu.Unlock()
	if resident && !tombstoned {
		// Cleanup may still be in flight; it must at least have been scheduled.
		server.mu.Lock()
		scheduled := state.drainCleanupScheduled || state.descriptor.State == harnessv2.RuntimeSessionStatePoisoned
		server.mu.Unlock()
		if !scheduled {
			t.Fatal("lost-adapter session cleanup was not scheduled")
		}
	}
}

// Creation validates the workspace once, but the agent can write it between
// prompts. An idle adapter restart must re-run that check before launching a
// replacement, and retire the session rather than start the provider over
// configuration that creation would have refused.
func TestSupervisorRevalidatesWorkspaceBeforeIdleAdapterRelaunch(t *testing.T) {
	var validations atomic.Int32
	server, cfg, create, state := runFirstPromptThenAwaitAdapterExitWith(t, adapterResumeMode, func(server *Server) {
		server.cfg.Provider.ValidateWorkspace = func(paths acp.SessionPaths) error {
			validations.Add(1)
			return rejectOpenCodeProjectConfiguration(paths)
		}
	})
	if got := validations.Load(); got != 1 {
		t.Fatalf("workspace validations after create = %d, want 1", got)
	}
	writeTestFile(t, filepath.Join(state.paths.Workspace, "opencode.json"), "{}")
	exited := state.runtime.Process()

	next := testSecondPromptRequest(t, cfg, create.Metadata.Fence)
	response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1/prompts/prompt-2", next, cfg)
	if response.Code != http.StatusConflict {
		t.Fatalf("prompt after unsafe workspace change status = %d body=%s", response.Code, response.Body.String())
	}
	var body harnessv2.ErrorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != harnessv2.ErrorCodePromptNotAccepted || body.Retryable {
		t.Fatalf("error = %#v, want non-retryable prompt-not-accepted", body)
	}
	if got := validations.Load(); got != 2 {
		t.Fatalf("workspace validations after relaunch attempt = %d, want 2", got)
	}
	if state.runtime.AdapterRestarts() != 0 || state.runtime.Process() != exited {
		t.Fatal("idle adapter restart launched a replacement over a rejected workspace")
	}
	if _, exists := state.runtime.Tombstone(string(next.Metadata.PromptID)); exists {
		t.Fatal("rejected relaunch submitted the next ACP prompt")
	}
}
