package supervisor

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func TestCancellationPublishesSettledStatusBeforePromptStreamFinishes(t *testing.T) {
	for _, outcome := range []acp.PromptOutcome{
		acp.PromptOutcomeCompleted, acp.PromptOutcomeCancelled,
		acp.PromptOutcomeFailed, acp.PromptOutcomeOutcomeUnknown,
	} {
		t.Run(string(outcome), func(t *testing.T) {
			server, cfg, profile := newTestServer(t, "immediate")
			create := testCreateSessionRequest(t, cfg, profile)
			created := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg)
			if created.Code != http.StatusCreated {
				t.Fatalf("create status = %d", created.Code)
			}
			request := testStartPromptRequest(t, cfg, create.Metadata.Fence)
			mutations := newBlockingPromptMutator()
			mutations.cancelResult = acp.PromptResult{Outcome: outcome, Accepted: true, SettledAt: time.Now().UTC()}
			server.mu.Lock()
			state := server.sessions[create.RuntimeSessionID]
			state.prompt = &promptState{
				request: request, lease: request.Lease, acceptedAt: time.Now().UTC(), startedAt: time.Now().UTC(),
			}
			state.descriptor.State = harnessv2.RuntimeSessionStatePromptRunning
			state.promptMutations = mutations
			server.mu.Unlock()

			cancel := testLateCancellation(t, create.Metadata.Fence)
			done := serveMutationAsync(server.Handler(), mutationHTTPRequest(t, http.MethodPut,
				"/v2/runtime-sessions/session-1/prompts/prompt-1/cancel", cancel, cfg))
			awaitSignal(t, mutations.cancelEntered, "cancellation did not start")
			before := readPromptTestStatus(t, server, cfg)
			if len(before.ActivePrompts) != 1 {
				t.Fatal("unsettled prompt lost its active status")
			}
			close(mutations.cancelRelease)
			response := awaitRecorder(t, done, "cancellation did not settle")
			if response.Code != http.StatusOK {
				t.Fatalf("cancel status = %d", response.Code)
			}
			var cancellation harnessv2.CancelPromptResponse
			decodeResponse(t, response, &cancellation)
			if err := cancellation.Validate(); err != nil {
				t.Fatal(err)
			}
			// The original prompt HTTP writer can still be blocked. Status must
			// publish the cancellation's settlement without waiting for it.
			after := readPromptTestStatus(t, server, cfg)
			wantState := harnessv2.RuntimeSessionStatePoisoned
			if outcome == acp.PromptOutcomeCompleted {
				wantState = harnessv2.RuntimeSessionStateValidating
			}
			if len(after.Sessions) != 1 || after.Sessions[0].State != wantState || len(after.ActivePrompts) != 0 {
				t.Fatalf("settled status = %#v", after)
			}
		})
	}
}

func TestSupervisorStatusDuringUnsettledPromptTransitions(t *testing.T) {
	for _, phase := range []harnessv2.RuntimeSessionState{
		harnessv2.RuntimeSessionStateIdle, harnessv2.RuntimeSessionStatePromptRunning,
		harnessv2.RuntimeSessionStateCancelling, harnessv2.RuntimeSessionStatePoisoned,
	} {
		t.Run(string(phase), func(t *testing.T) {
			server, cfg, profile := newTestServer(t, "immediate")
			create := testCreateSessionRequest(t, cfg, profile)
			created := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg)
			if created.Code != http.StatusCreated {
				t.Fatalf("create status = %d", created.Code)
			}
			request := testStartPromptRequest(t, cfg, create.Metadata.Fence)
			now := time.Now().UTC()
			server.mu.Lock()
			state := server.sessions[create.RuntimeSessionID]
			state.prompt = &promptState{request: request, lease: request.Lease, startedAt: now}
			state.descriptor.State = phase
			if phase != harnessv2.RuntimeSessionStateIdle {
				state.permissions["permission-1"] = permissionState{
					requestID: "permission-1", requestedAt: now, expiresAt: now.Add(time.Minute),
				}
			}
			server.mu.Unlock()
			status := readPromptTestStatus(t, server, cfg)
			wantActive := 0
			if phase == harnessv2.RuntimeSessionStatePromptRunning {
				wantActive = 1
			}
			if len(status.ActivePrompts) != wantActive || len(status.PendingPermissions) != wantActive ||
				status.Pressure.ActivePrompts != uint32(wantActive) || status.Pressure.PendingPermissions != uint32(wantActive) {
				t.Fatalf("prompt authority status = %#v", status)
			}
		})
	}
}

func readPromptTestStatus(t *testing.T, server *Server, cfg Config) harnessv2.StatusResponse {
	t.Helper()
	nonce, err := harnessv2.NewCapabilityNonce()
	if err != nil {
		t.Fatal(err)
	}
	capability, err := harnessv2.SignStatusCapability(cfg.CapabilitySecret, harnessv2.NewStatusCapabilityClaims(
		harnessv2.StatusCapabilityBinding{RuntimeProfileDigest: cfg.Fence.RuntimeProfileDigest}, nonce, time.Now().UTC().Add(time.Minute),
	))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, harnessv2.StatusPath, nil)
	request.Header.Set("Authorization", "Bearer "+cfg.ControllerBearerToken)
	request.Header.Set(OperationCapabilityHeader, capability)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status HTTP code = %d", response.Code)
	}
	var status harnessv2.StatusResponse
	decodeResponse(t, response, &status)
	if err := status.Validate(); err != nil {
		t.Fatalf("status failed the harness v2 contract: %v", err)
	}
	return status
}

func TestPoisonedPoolPublishesValidStatusAndUnhealthyHealth(t *testing.T) {
	for _, draining := range []bool{false, true} {
		name := "ready"
		if draining {
			name = "draining"
		}
		t.Run(name, func(t *testing.T) {
			server, cfg, profile := newTestServer(t, "immediate")
			if draining {
				server.BeginDrain("test drain")
			}
			server.poisonPool("drain_session_cleanup_unproven")
			server.BeginDrain("later drain must not revive a failed pool")
			status := readPromptTestStatus(t, server, cfg)
			if status.Lifecycle != harnessv2.SupervisorLifecycleTerminating || !status.Drain.Requested || status.Drain.AcceptingNewSessions {
				t.Fatalf("poisoned pool status = %#v", status)
			}
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, harnessv2.HealthPath, nil))
			var health harnessv2.HealthResponse
			decodeResponse(t, response, &health)
			if health.Status != harnessv2.HealthStatusUnhealthy {
				t.Fatalf("poisoned pool health = %q", health.Status)
			}
			create := testCreateSessionRequest(t, cfg, profile)
			created := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg)
			if created.Code != http.StatusTooManyRequests || len(server.sessions) != 0 {
				t.Fatalf("poisoned pool admitted a session: HTTP %d", created.Code)
			}
		})
	}
}

func TestDelayedPromptFinisherPreservesLaterSessionPhase(t *testing.T) {
	for _, phase := range []harnessv2.RuntimeSessionState{
		harnessv2.RuntimeSessionStateIdle, harnessv2.RuntimeSessionStateDeleting, harnessv2.RuntimeSessionStatePromptRunning,
	} {
		t.Run(string(phase), func(t *testing.T) {
			server, cfg, profile := newTestServer(t, "immediate")
			create := testCreateSessionRequest(t, cfg, profile)
			request := testStartPromptRequest(t, cfg, create.Metadata.Fence)
			settledAt := time.Now().UTC().Add(-time.Second)
			result := acp.PromptResult{Outcome: acp.PromptOutcomeCompleted, Accepted: true, SettledAt: settledAt}
			settlement := settlementFromResult(result, settledAt)
			oldPrompt := &promptState{request: request, settlement: &settlement, acceptedAt: settledAt}
			transitionAt := time.Now().UTC()
			state := &sessionState{
				prompt:     oldPrompt,
				descriptor: harnessv2.RuntimeSessionDescriptor{State: phase, LastTransitionAt: transitionAt},
				operations: make(map[harnessv2.OperationID]harnessv2.OperationRecord),
			}
			if phase == harnessv2.RuntimeSessionStatePromptRunning {
				state.prompt = &promptState{request: request}
				state.prompt.request.Metadata.PromptID = "continuation"
				state.permissions = map[harnessv2.PermissionRequestID]permissionState{"current-permission": {requestID: "current-permission"}}
			}
			server.finishPrompt(state, oldPrompt, result, settledAt)
			if state.descriptor.State != phase || !state.descriptor.LastTransitionAt.Equal(transitionAt) || state.drainCleanupScheduled {
				t.Fatalf("late finisher regressed session phase: %#v", state.descriptor)
			}
			if phase == harnessv2.RuntimeSessionStatePromptRunning && (state.prompt.settlement != nil || len(state.permissions) != 1) {
				t.Fatal("late finisher changed a continuation's authority")
			}
		})
	}
}
