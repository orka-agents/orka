package supervisor

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

// runFirstPromptThenAwaitAdapterExit creates session-1, completes prompt-1
// against a helper that exits right after answering, waits for that exit, and
// returns the session state to Idle the way completed workspace validation
// would.
func runFirstPromptThenAwaitAdapterExit(t *testing.T, mode string) (*Server, Config, harnessv2.CreateRuntimeSessionRequest, *sessionState) {
	t.Helper()
	server, cfg, profile := newTestServer(t, mode)
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

func TestSupervisorRetiresSessionWhenIdleAdapterCannotResume(t *testing.T) {
	server, cfg, create, state := runFirstPromptThenAwaitAdapterExit(t, adapterExitAfterPromptMode)
	next := testSecondPromptRequest(t, cfg, create.Metadata.Fence)
	response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1/prompts/prompt-2", next, cfg)
	if response.Code != http.StatusConflict {
		t.Fatalf("prompt after unresumable adapter exit status = %d body=%s", response.Code, response.Body.String())
	}
	var failure harnessv2.ErrorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil {
		t.Fatalf("decode error response: %v body=%s", err, response.Body.String())
	}
	if failure.Code != harnessv2.ErrorCodeSessionPoisoned || failure.Retryable {
		t.Fatalf("error response = %#v, want non-retryable %s", failure, harnessv2.ErrorCodeSessionPoisoned)
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
