package supervisor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func TestPendingCancellationAdmissionRollbackAllowsExactRetry(t *testing.T) {
	server, cfg, profile := newTestServer(t, "immediate")
	create := testCreateSessionRequest(t, cfg, profile)
	created := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d", created.Code)
	}
	server.mu.Lock()
	state := server.sessions[create.RuntimeSessionID]
	server.mu.Unlock()
	request := testStartPromptRequest(t, cfg, create.Metadata.Fence)
	prompt := stageUnacceptedPromptAdmission(t, server, state, request)
	providerGate, mcpGate := promptAdmissionGates(state)

	// Exercise the production rejection path directly: injecting a blocked
	// private ACP cancellation writer would require a new runtime test API.
	rejected := httptest.NewRecorder()
	server.mu.Lock()
	server.handlePromptStartFailureLocked(rejected, state, prompt, fmt.Errorf("admission: %w", acp.ErrPromptCancellationPending))
	assertPendingCancellationRejection(t, rejected)
	assertUnacceptedPromptOperationRemoved(t, server, state, prompt)
	server.mu.Lock()
	restoredIdle := state.descriptor.State == harnessv2.RuntimeSessionStateIdle
	_, creationRecorded := state.operations[create.Metadata.OperationID]
	_, creationRetained := state.operationRetention[create.Metadata.OperationID]
	server.mu.Unlock()
	if !restoredIdle || !creationRecorded || !creationRetained {
		t.Fatal("rollback changed session lifecycle or an earlier operation")
	}
	if providerGate.Err() == nil || mcpGate.Err() == nil {
		t.Fatal("unaccepted prompt retained provider or MCP authority")
	}
	state.providerProxy.mu.Lock()
	providerIdle := state.providerProxy.activePromptID == "" && state.providerProxy.gateCancel == nil && state.providerProxy.leaseTimer == nil
	state.providerProxy.mu.Unlock()
	state.mcpProxy.mu.Lock()
	mcpIdle := state.mcpProxy.state == harnessv2.RuntimeSessionStateIdle && state.mcpProxy.authorization == nil &&
		state.mcpProxy.gateCancel == nil && state.mcpProxy.leaseTimer == nil
	state.mcpProxy.mu.Unlock()
	if !providerIdle || !mcpIdle {
		t.Fatal("rejection stranded provider or MCP admission")
	}

	// Identical metadata must classify as fresh, not already_accepted. Use the
	// real authenticated HTTP path and adapter subprocess for the retry.
	retry := performMutation(t, server.Handler(), http.MethodPut,
		"/v2/runtime-sessions/session-1/prompts/prompt-1", request, cfg)
	if retry.Code != http.StatusOK {
		t.Fatalf("exact retry status = %d", retry.Code)
	}
	decoder, err := harnessv2.NewEventDecoder(bytes.NewReader(retry.Body.Bytes()), eventLimits(cfg.Capabilities.Limits),
		harnessv2.EventExpectationFromMetadata(request.Metadata))
	if err != nil {
		t.Fatal(err)
	}
	events, err := decoder.DecodeAll()
	if err != nil || len(events) == 0 || events[0].Type != harnessv2.EventAccepted || events[len(events)-1].Type != harnessv2.EventCompleted {
		t.Fatalf("exact retry did not execute: events = %d, error = %v", len(events), err)
	}
	if len(server.promptSlots) != 0 {
		t.Fatal("completed retry leaked prompt capacity")
	}
}

func TestPendingCancellationRollbackFencesRequestCallback(t *testing.T) {
	for _, scenario := range []string{"callback-before-rollback", "callback-running-at-rollback", "callback-running-at-same-ID-retry"} {
		t.Run(scenario, func(t *testing.T) {
			server, state, prompt := newPromptAdmissionRollbackState(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			stop := context.AfterFunc(ctx, func() {
				close(entered)
				<-release
				server.revokePromptRequestCapabilities(state, prompt)
				close(done)
			})
			defer stop()
			t.Cleanup(func() {
				select {
				case <-release:
				default:
					close(release)
				}
				awaitSignal(t, done, "request callback did not exit")
			})
			cancel()
			awaitSignal(t, entered, "request callback did not start")
			if stop() {
				t.Fatal("already-running callback was stopped")
			}
			if scenario == "callback-before-rollback" {
				close(release)
				awaitSignal(t, done, "request callback did not revoke capabilities")
				state.mcpProxy.mu.Lock()
				cancelled := state.mcpProxy.state == harnessv2.RuntimeSessionStateCancelling
				state.mcpProxy.mu.Unlock()
				if !cancelled {
					t.Fatal("request cancellation did not close MCP admission")
				}
			}

			rejected := httptest.NewRecorder()
			server.mu.Lock()
			server.handlePromptStartFailureLocked(rejected, state, prompt, acp.ErrPromptCancellationPending)
			assertPendingCancellationRejection(t, rejected)
			assertUnacceptedPromptOperationRemoved(t, server, state, prompt)
			wantState := harnessv2.RuntimeSessionStateIdle
			var retry *promptState
			var providerGate, mcpGate context.Context
			if scenario == "callback-running-at-same-ID-retry" {
				retry = stageUnacceptedPromptAdmission(t, server, state, prompt.request)
				if err := state.mcpProxy.markRunning(retry.request.Metadata.PromptID, time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
				server.mu.Lock()
				state.descriptor.State = harnessv2.RuntimeSessionStatePromptRunning
				server.mu.Unlock()
				wantState = harnessv2.RuntimeSessionStatePromptRunning
				providerGate, mcpGate = promptAdmissionGates(state)
			}
			if scenario != "callback-before-rollback" {
				close(release)
				awaitSignal(t, done, "old request callback did not exit")
			}
			state.mcpProxy.mu.Lock()
			gotState := state.mcpProxy.state
			state.mcpProxy.mu.Unlock()
			if gotState != wantState {
				t.Fatalf("old callback overwrote MCP rollback: state = %s, want %s", gotState, wantState)
			}
			if retry != nil && (providerGate.Err() != nil || mcpGate.Err() != nil) {
				t.Fatal("old callback revoked a same-ID retry's authority")
			}
		})
	}
}

func TestPendingCancellationRollbackPreservesClosedAdmission(t *testing.T) {
	for _, scenario := range []struct {
		name       string
		state      harnessv2.RuntimeSessionState
		poolPoison bool
		mcpClosed  bool
		rejection  error
		want       harnessv2.RuntimeSessionState
	}{
		{name: "deleting", state: harnessv2.RuntimeSessionStateDeleting, want: harnessv2.RuntimeSessionStateDeleting},
		{name: "poisoned-session", state: harnessv2.RuntimeSessionStatePoisoned, want: harnessv2.RuntimeSessionStatePoisoned},
		{name: "poisoned-pool", state: harnessv2.RuntimeSessionStateIdle, poolPoison: true, want: harnessv2.RuntimeSessionStatePoisoned},
		{name: "closed-MCP", state: harnessv2.RuntimeSessionStateIdle, mcpClosed: true, want: harnessv2.RuntimeSessionStateDeleted},
		{name: "expired-deleting", state: harnessv2.RuntimeSessionStateDeleting, rejection: acp.ErrPromptLeaseExpired, want: harnessv2.RuntimeSessionStateDeleting},
		{name: "expired-poisoned-session", state: harnessv2.RuntimeSessionStatePoisoned, rejection: acp.ErrPromptLeaseExpired, want: harnessv2.RuntimeSessionStatePoisoned},
		{name: "expired-poisoned-pool", state: harnessv2.RuntimeSessionStateIdle, poolPoison: true, rejection: acp.ErrPromptLeaseExpired, want: harnessv2.RuntimeSessionStatePoisoned},
		{name: "expired-closed-MCP", state: harnessv2.RuntimeSessionStateIdle, mcpClosed: true, rejection: fmt.Errorf("admission: %w", acp.ErrPromptLeaseExpired), want: harnessv2.RuntimeSessionStateDeleted},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			server, state, prompt := newPromptAdmissionRollbackState(t)
			server.mu.Lock()
			state.descriptor.State = scenario.state
			server.poisoned = scenario.poolPoison
			server.mu.Unlock()
			if scenario.mcpClosed {
				state.mcpProxy.mu.Lock()
				state.mcpProxy.closed = true
				state.mcpProxy.revokeLocked(harnessv2.RuntimeSessionStateDeleted)
				state.mcpProxy.mu.Unlock()
			}
			server.revokePromptRequestCapabilities(state, prompt)
			rejection := scenario.rejection
			if rejection == nil {
				rejection = acp.ErrPromptCancellationPending
			}
			server.mu.Lock()
			server.handlePromptStartFailureLocked(httptest.NewRecorder(), state, prompt, rejection)
			assertUnacceptedPromptOperationRemoved(t, server, state, prompt)
			server.mu.Lock()
			gotDescriptor := state.descriptor.State
			server.mu.Unlock()
			if gotDescriptor != scenario.state {
				t.Fatalf("rollback changed supervisor lifecycle: %s", gotDescriptor)
			}
			state.mcpProxy.mu.Lock()
			gotMCP := state.mcpProxy.state
			state.mcpProxy.mu.Unlock()
			if gotMCP != scenario.want {
				t.Fatalf("rollback reopened closed MCP admission: %s, want %s", gotMCP, scenario.want)
			}
			if err := state.mcpProxy.activate(t.Context(), prompt.request.MCPAuthorization, prompt.lease, time.Now().UTC()); err == nil {
				t.Fatal("closed session accepted new MCP authority")
			}
		})
	}
}

func TestPromptAdmissionRollbackIgnoresReplacedOwner(t *testing.T) {
	for _, scenario := range []string{"prompt-replaced", "session-replaced", "session-removed"} {
		t.Run(scenario, func(t *testing.T) {
			server, state, prompt := newPromptAdmissionRollbackState(t)
			providerGate, mcpGate := promptAdmissionGates(state)
			server.mu.Lock()
			switch scenario {
			case "prompt-replaced":
				state.prompt = &promptState{request: prompt.request}
			case "session-replaced":
				server.sessions[state.id] = &sessionState{id: state.id}
			case "session-removed":
				delete(server.sessions, state.id)
			}
			owner := state.prompt
			server.mu.Unlock()
			server.revokePromptRequestCapabilities(state, prompt)
			server.mu.Lock()
			server.handlePromptStartFailureLocked(httptest.NewRecorder(), state, prompt, acp.ErrPromptCancellationPending)
			server.mu.Lock()
			unchanged := state.prompt == owner
			_, recorded := state.operations[prompt.request.Metadata.OperationID]
			server.mu.Unlock()
			if !unchanged || !recorded || providerGate.Err() != nil || mcpGate.Err() != nil {
				t.Fatal("stale rejection or request callback mutated a replaced owner")
			}
		})
	}
}

func TestPromptLeaseExpiredRollbackAllowsFreshAdmission(t *testing.T) {
	server, state, prompt := newPromptAdmissionRollbackState(t)
	providerGate, mcpGate := promptAdmissionGates(state)
	// Request cancellation may already have closed the unaccepted MCP gate.
	server.revokePromptRequestCapabilities(state, prompt)
	response := httptest.NewRecorder()
	server.mu.Lock()
	server.handlePromptStartFailureLocked(response, state, prompt, fmt.Errorf("admission: %w", acp.ErrPromptLeaseExpired))
	var failure harnessv2.ErrorResponse
	decodeResponse(t, response, &failure)
	if response.Code != http.StatusGone || failure.Code != harnessv2.ErrorCodeExpired || failure.Retryable {
		t.Errorf("pre-activation expiry = HTTP %d, code %s, retryable %t", response.Code, failure.Code, failure.Retryable)
	}
	assertUnacceptedPromptOperationRemoved(t, server, state, prompt)
	server.mu.Lock()
	idle := state.descriptor.State == harnessv2.RuntimeSessionStateIdle
	server.mu.Unlock()
	state.mcpProxy.mu.Lock()
	mcpIdle := state.mcpProxy.state == harnessv2.RuntimeSessionStateIdle && state.mcpProxy.authorization == nil &&
		state.mcpProxy.gateCancel == nil && state.mcpProxy.leaseTimer == nil
	state.mcpProxy.mu.Unlock()
	if !idle || !mcpIdle || providerGate.Err() == nil || mcpGate.Err() == nil {
		t.Fatal("pre-activation expiry stranded admission or retained authority")
	}
	retry := stageUnacceptedPromptAdmission(t, server, state, prompt.request)
	if err := state.mcpProxy.markRunning(retry.request.Metadata.PromptID, time.Now().UTC()); err != nil {
		t.Fatalf("fresh admission after pre-activation expiry: %v", err)
	}
}

func TestPromptStartOtherErrorsRemainFailClosed(t *testing.T) {
	server, state, prompt := newPromptAdmissionRollbackState(t)
	response := httptest.NewRecorder()
	server.mu.Lock()
	server.handlePromptStartFailureLocked(response, state, prompt, errors.New("ambiguous runtime start failure"))
	var failure harnessv2.ErrorResponse
	decodeResponse(t, response, &failure)
	if response.Code != http.StatusConflict || failure.Code != harnessv2.ErrorCodeAlreadyAccepted || failure.Retryable {
		t.Fatalf("other start error changed semantics: status = %d, code = %s, retryable = %t", response.Code, failure.Code, failure.Retryable)
	}
	assertUnacceptedPromptOperationRemoved(t, server, state, prompt)
	state.mcpProxy.mu.Lock()
	mcpState := state.mcpProxy.state
	state.mcpProxy.mu.Unlock()
	if mcpState != harnessv2.RuntimeSessionStateCancelling {
		t.Fatalf("other start error reopened MCP admission: %s", mcpState)
	}
	if err := state.mcpProxy.activate(t.Context(), prompt.request.MCPAuthorization, prompt.lease, time.Now().UTC()); err == nil {
		t.Fatal("other start error allowed a new MCP admission")
	}
}

func newPromptAdmissionRollbackState(t *testing.T) (*Server, *sessionState, *promptState) {
	t.Helper()
	fence := harnessv2.Fence{RuntimeSessionUID: "session-uid", RuntimeSessionGeneration: 1}
	request := testStartPromptRequest(t, Config{}, fence)
	state := &sessionState{
		id: "session-1", descriptor: harnessv2.RuntimeSessionDescriptor{State: harnessv2.RuntimeSessionStateIdle},
		providerProxy:      &providerProxySession{},
		mcpProxy:           &mcpProxySession{fence: fence, configuration: request.MCPAuthorization.Configuration(), state: harnessv2.RuntimeSessionStateIdle},
		agentConfiguration: harnessv2.AgentSessionConfiguration{MaxTurns: 1},
	}
	server := &Server{sessions: map[harnessv2.RuntimeSessionID]*sessionState{state.id: state}}
	prompt := stageUnacceptedPromptAdmission(t, server, state, request)
	t.Cleanup(func() {
		deactivatePromptCapabilities(state, request.Metadata.PromptID, harnessv2.RuntimeSessionStateDeleted)
	})
	return server, state, prompt
}

func stageUnacceptedPromptAdmission(t *testing.T, server *Server, state *sessionState, request harnessv2.StartPromptRequest) *promptState {
	t.Helper()
	server.mu.Lock()
	defer server.mu.Unlock()
	now := time.Now().UTC()
	prompt := &promptState{request: request, lease: request.Lease, operation: operationRecord(request.Metadata, harnessv2.OperationPhaseRecorded, "", now)}
	state.prompt = prompt
	recordSessionOperationLocked(state, request.Metadata, harnessv2.OperationPhaseRecorded, "", now)
	if err := state.providerProxy.activatePrompt(request, state.agentConfiguration.MaxTurns, now); err != nil {
		t.Fatal(err)
	}
	if err := state.mcpProxy.activate(t.Context(), request.MCPAuthorization, request.Lease, now); err != nil {
		t.Fatal(err)
	}
	return prompt
}

func promptAdmissionGates(state *sessionState) (context.Context, context.Context) {
	state.providerProxy.mu.Lock()
	provider := state.providerProxy.gateContext
	state.providerProxy.mu.Unlock()
	state.mcpProxy.mu.Lock()
	mcp := state.mcpProxy.gateContext
	state.mcpProxy.mu.Unlock()
	return provider, mcp
}

func assertPendingCancellationRejection(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	var failure harnessv2.ErrorResponse
	decodeResponse(t, response, &failure)
	if response.Code != http.StatusTooManyRequests || failure.Code != harnessv2.ErrorCodeRateLimited || !failure.Retryable {
		t.Fatalf("pending cancellation rejection = status %d, code %s, retryable %t", response.Code, failure.Code, failure.Retryable)
	}
}

func assertUnacceptedPromptOperationRemoved(t *testing.T, server *Server, state *sessionState, prompt *promptState) {
	t.Helper()
	server.mu.Lock()
	defer server.mu.Unlock()
	if state.prompt != nil {
		t.Fatal("unaccepted prompt remained in supervisor state")
	}
	operationID := prompt.request.Metadata.OperationID
	_, recorded := state.operations[operationID]
	_, retained := state.operationRetention[operationID]
	_, replayed := state.operationReplays[operationID]
	if recorded || retained || replayed {
		t.Fatal("unaccepted operation remained recorded, retained, or replayable")
	}
}

// An automatic cleanup can defer to a prompt that the runtime has not accepted.
// Once rollback removes that owner, there is no stream finisher to retry cleanup.
func TestPendingCancellationRollbackResumesDeferredCleanup(t *testing.T) {
	for _, disposition := range []string{"drain", "poison-pool", "poison-session"} {
		t.Run(disposition, func(t *testing.T) {
			server, cfg, profile := newTestServer(t, "immediate")
			create := testCreateSessionRequest(t, cfg, profile)
			created := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg)
			if created.Code != http.StatusCreated {
				t.Fatalf("create status = %d", created.Code)
			}
			server.mu.Lock()
			state := server.sessions[create.RuntimeSessionID]
			server.mu.Unlock()
			request := testStartPromptRequest(t, cfg, create.Metadata.Fence)
			prompt := stageUnacceptedPromptAdmission(t, server, state, request)
			if disposition == "poison-session" {
				server.poisonSession(state, "test-admission-rollback")
			} else {
				server.mu.Lock()
				var cleanup []drainCleanupCandidate
				if disposition == "drain" {
					cleanup = server.beginDrainLocked("test-admission-rollback", time.Now().UTC())
				} else {
					cleanup = server.poisonPoolLocked("test-admission-rollback")
				}
				server.mu.Unlock()
				if len(cleanup) != 1 {
					t.Fatalf("cleanup candidates = %d, want one", len(cleanup))
				}
				server.cleanupDrainedSession(state.id, state)
			}
			deferredBound := time.Now().Add(5 * time.Second)
			for {
				server.mu.Lock()
				deferred := !state.drainCleanupScheduled && state.prompt == prompt
				server.mu.Unlock()
				if deferred {
					break
				}
				if time.Now().After(deferredBound) {
					t.Fatal("cleanup did not defer to the unsettled admission")
				}
				time.Sleep(10 * time.Millisecond)
			}
			response := httptest.NewRecorder()
			server.mu.Lock()
			server.handlePromptStartFailureLocked(response, state, prompt, acp.ErrPromptCancellationPending)
			assertPendingCancellationRejection(t, response)
			cleanupBound := time.Now().Add(5 * time.Second)
			for {
				server.mu.Lock()
				_, resident := server.sessions[state.id]
				_, retired := server.tombstones[create.Metadata.Fence.RuntimeSessionUID]
				server.mu.Unlock()
				if !resident && retired {
					break
				}
				server.mu.Lock()
				// An unproven native cleanup must remain closed and retained,
				// not be mistaken for retirement (notably on non-Linux hosts).
				attempted := server.poisoned && state.descriptor.State == harnessv2.RuntimeSessionStatePoisoned &&
					!state.drainCleanupScheduled && server.drain.Reason == "drain_session_cleanup_unproven"
				server.mu.Unlock()
				if attempted {
					break
				}
				if time.Now().After(cleanupBound) {
					t.Fatal("rollback did not retry automatic cleanup")
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}

func TestCancellationWriteTimeoutPoisonsAdmission(t *testing.T) {
	server, state, prompt := newPromptAdmissionRollbackState(t)
	response := httptest.NewRecorder()
	server.mu.Lock()
	server.handlePromptStartFailureLocked(response, state, prompt, acp.ErrPromptCancellationTimedOut)
	var failure harnessv2.ErrorResponse
	decodeResponse(t, response, &failure)
	if response.Code != http.StatusConflict || failure.Code != harnessv2.ErrorCodeSessionPoisoned || failure.Retryable {
		t.Fatalf("cancellation timeout = HTTP %d, code %s, retryable %t", response.Code, failure.Code, failure.Retryable)
	}
	assertUnacceptedPromptOperationRemoved(t, server, state, prompt)
	server.mu.Lock()
	poisoned := state.descriptor.State == harnessv2.RuntimeSessionStatePoisoned
	server.mu.Unlock()
	state.mcpProxy.mu.Lock()
	mcpPoisoned := state.mcpProxy.state == harnessv2.RuntimeSessionStatePoisoned
	state.mcpProxy.mu.Unlock()
	if !poisoned || !mcpPoisoned {
		t.Fatal("unresponsive transport left session admission reusable")
	}
}

func TestPromptActivationFailurePoisonsAdmission(t *testing.T) {
	for _, scenario := range []struct {
		name string
		err  error
	}{
		{name: "provider", err: errPromptProviderActivation},
		{name: "mcp", err: errPromptMCPActivation},
		{name: "expired-after-activation", err: acp.ErrPromptActivationExpired},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			server, state, prompt := newPromptAdmissionRollbackState(t)
			providerGate, mcpGate := promptAdmissionGates(state)
			response := httptest.NewRecorder()
			server.mu.Lock()
			server.handlePromptStartFailureLocked(response, state, prompt, fmt.Errorf("activation: %w", scenario.err))
			var failure harnessv2.ErrorResponse
			decodeResponse(t, response, &failure)
			if response.Code != http.StatusInternalServerError || failure.Code != harnessv2.ErrorCodeSessionPoisoned || failure.Retryable {
				t.Fatalf("activation failure = HTTP %d, code %s, retryable %t", response.Code, failure.Code, failure.Retryable)
			}
			if failure.Message != scenario.err.Error() {
				t.Fatalf("post-activation expiry message = %q", failure.Message)
			}
			assertUnacceptedPromptOperationRemoved(t, server, state, prompt)
			server.mu.Lock()
			poisoned := state.descriptor.State == harnessv2.RuntimeSessionStatePoisoned && !state.descriptor.LastTransitionAt.IsZero()
			server.mu.Unlock()
			state.mcpProxy.mu.Lock()
			mcpPoisoned := state.mcpProxy.state == harnessv2.RuntimeSessionStatePoisoned
			state.mcpProxy.mu.Unlock()
			if !poisoned || !mcpPoisoned || providerGate.Err() == nil || mcpGate.Err() == nil {
				t.Fatal("terminal activation failure left session admission reusable")
			}
		})
	}
}

func TestPromptActivationFailureSchedulesCleanup(t *testing.T) {
	for _, scenario := range []struct {
		name string
		err  error
	}{
		{name: "provider", err: errPromptProviderActivation},
		{name: "mcp", err: errPromptMCPActivation},
		{name: "expired-after-activation", err: acp.ErrPromptActivationExpired},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			server, cfg, profile := newTestServer(t, "immediate")
			create := testCreateSessionRequest(t, cfg, profile)
			created := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg)
			if created.Code != http.StatusCreated {
				t.Fatalf("create status = %d", created.Code)
			}
			server.mu.Lock()
			state := server.sessions[create.RuntimeSessionID]
			server.mu.Unlock()
			request := testStartPromptRequest(t, cfg, create.Metadata.Fence)
			prompt := stageUnacceptedPromptAdmission(t, server, state, request)
			response := httptest.NewRecorder()
			server.mu.Lock()
			server.handlePromptStartFailureLocked(response, state, prompt, scenario.err)
			if response.Code != http.StatusInternalServerError {
				t.Fatalf("activation failure status = %d", response.Code)
			}
			assertUnacceptedPromptOperationRemoved(t, server, state, prompt)
			cleanupBound := time.Now().Add(5 * time.Second)
			for {
				server.mu.Lock()
				resident := server.sessions[state.id] == state
				_, retired := server.tombstones[create.Metadata.Fence.RuntimeSessionUID]
				// Unproven native cleanup stays sealed and retained rather
				// than falsely claiming retirement, including on macOS.
				retained := resident && server.poisoned && state.descriptor.State == harnessv2.RuntimeSessionStatePoisoned && !state.drainCleanupScheduled
				server.mu.Unlock()
				if (!resident && retired) || retained {
					break
				}
				if time.Now().After(cleanupBound) {
					t.Fatal("terminal activation failure did not initiate automatic cleanup")
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}
