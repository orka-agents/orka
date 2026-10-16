package supervisor

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func TestFoundryPendingCancellationAdmissionPreservesIdenticalRetry(t *testing.T) {
	testFoundryPreActivationAdmissionRetry(t, acp.ErrPromptCancellationPending)
}

func TestFoundryExpiredLeaseAdmissionPreservesFreshRetry(t *testing.T) {
	testFoundryPreActivationAdmissionRetry(t, acp.ErrPromptLeaseExpired)
}

func testFoundryPreActivationAdmissionRetry(t *testing.T, rejection error) {
	t.Helper()
	expired := errors.Is(rejection, acp.ErrPromptLeaseExpired)
	var remoteMu sync.Mutex
	var controlCalls int
	var settlements []foundryBrokerContext
	retryStarted := false
	settled := make(map[harnessv2.PromptID]bool)
	server, cfg, create := newFoundryTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		value, proof := foundryTestProof(t, r)
		remoteMu.Lock()
		controlCalls++
		switch r.URL.Path {
		case "/internal/v1/settle":
			if !retryStarted {
				t.Error("Foundry settled an identity before its admission retry")
			}
			if settled[value.PromptID] {
				t.Error("Foundry settled the same prompt identity more than once")
			}
			settled[value.PromptID] = true
			settlements = append(settlements, value)
		case "/internal/v1/retire": // Fixture shutdown, after the assertions below.
		default:
			t.Error("unexpected Foundry request from the native adapter fixture")
		}
		remoteMu.Unlock()
		_ = json.NewEncoder(w).Encode(proof)
	}))
	created := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", create, cfg)
	if created.Code != http.StatusCreated {
		t.Fatalf("Foundry session creation status = %d", created.Code)
	}
	request := testStartPromptRequest(t, cfg, create.Metadata.Fence)
	now := time.Now().UTC()
	if expired {
		request.Lease.ExpiresAt = now.Add(-time.Nanosecond)
		request.Lease.IssuedAt = request.Lease.ExpiresAt.Add(-time.Minute)
		request.MCPAuthorization.ExpiresAt = request.Lease.ExpiresAt
		request.Metadata.ExpiresAt = request.Lease.ExpiresAt
		digest, err := harnessv2.CanonicalRequestDigest(request)
		if err != nil {
			t.Fatal(err)
		}
		request.Metadata.RequestDigest = digest
	}

	// Install only the unaccepted supervisor journal entry. The runtime's
	// pre-activation fences precede the non-I/O activation callback,
	// so neither provider/MCP activation nor Foundry beginPrompt may have run.
	// Rejection is exercised at the production failure helper, complemented
	// by the ACP blocked-writer test of activation callback ordering; this is
	// deliberately not another private-pipe fault-injection fixture.
	server.mu.Lock()
	state := server.sessions[create.RuntimeSessionID]
	prompt := &promptState{
		request: request, lease: request.Lease, startedAt: now,
		operation: operationRecord(request.Metadata, harnessv2.OperationPhaseRecorded, "", now),
	}
	state.prompt = prompt
	recordSessionOperationLocked(state, request.Metadata, harnessv2.OperationPhaseRecorded, "", now)
	server.mu.Unlock()
	assertFoundryPromptAdmissionInactive(t, state)

	rejected := httptest.NewRecorder()
	server.mu.Lock()
	server.handlePromptStartFailureLocked(rejected, state, prompt, rejection)
	var failure harnessv2.ErrorResponse
	decodeResponse(t, rejected, &failure)
	wantStatus, wantCode, wantRetryable := http.StatusTooManyRequests, harnessv2.ErrorCodeRateLimited, true
	if expired {
		wantStatus, wantCode, wantRetryable = http.StatusGone, harnessv2.ErrorCodeExpired, false
	}
	if rejected.Code != wantStatus || failure.Code != wantCode || failure.Retryable != wantRetryable {
		t.Fatalf("Foundry admission rejection = HTTP %d, code %s, retryable %t", rejected.Code, failure.Code, failure.Retryable)
	}
	server.mu.Lock()
	promptRemoved := state.prompt == nil
	_, recorded := state.operations[request.Metadata.OperationID]
	_, retained := state.operationRetention[request.Metadata.OperationID]
	_, replayed := state.operationReplays[request.Metadata.OperationID]
	_, creationRecorded := state.operations[create.Metadata.OperationID]
	idle := state.descriptor.State == harnessv2.RuntimeSessionStateIdle
	server.mu.Unlock()
	if !promptRemoved || recorded || retained || replayed || !creationRecorded || !idle {
		t.Fatal("Foundry rejection retained unaccepted bookkeeping or changed the session's existing admission state")
	}
	assertFoundryPromptAdmissionInactive(t, state)
	remoteMu.Lock()
	callsBeforeRetry := controlCalls
	retryStarted = true
	remoteMu.Unlock()
	if callsBeforeRetry != 0 {
		t.Fatalf("never-accepted Foundry identity caused %d remote control calls", callsBeforeRetry)
	}

	// Pending cancellation permits an identical retry. An expired request
	// needs a fresh lease, but may reuse its never-accepted prompt identity.
	retryRequest := request
	if expired {
		retryRequest = testStartPromptRequest(t, cfg, create.Metadata.Fence)
	}
	retry := performMutation(t, server.Handler(), http.MethodPut,
		"/v2/runtime-sessions/session-1/prompts/prompt-1", retryRequest, cfg)
	if retry.Code != http.StatusOK {
		t.Fatalf("identical Foundry retry status = %d", retry.Code)
	}
	decoder, err := harnessv2.NewEventDecoder(bytes.NewReader(retry.Body.Bytes()), eventLimits(cfg.Capabilities.Limits),
		harnessv2.EventExpectationFromMetadata(retryRequest.Metadata))
	if err != nil {
		t.Fatal(err)
	}
	events, err := decoder.DecodeAll()
	if err != nil || len(events) == 0 || events[0].Type != harnessv2.EventAccepted || events[len(events)-1].Type != harnessv2.EventCompleted {
		t.Fatalf("identical Foundry retry did not complete: events = %d, error = %v", len(events), err)
	}
	remoteMu.Lock()
	callsAfterRetry := controlCalls
	observed := append([]foundryBrokerContext(nil), settlements...)
	remoteMu.Unlock()
	if callsAfterRetry != 1 || len(observed) != 1 {
		t.Fatalf("Foundry retry control calls = %d, settlements = %d; want one settlement only", callsAfterRetry, len(observed))
	}
	assertFoundryPromptRetrySettlement(t, state, retryRequest, observed[0])
}

func assertFoundryPromptRetrySettlement(t *testing.T, state *sessionState, request harnessv2.StartPromptRequest, value foundryBrokerContext) {
	t.Helper()
	if value.Owner != request.Metadata.Fence || value.PromptID != request.Metadata.PromptID ||
		value.PromptRequestDigest != request.Metadata.RequestDigest || value.TaskUID != request.Metadata.TaskUID ||
		value.TaskAttempt != request.Metadata.TaskAttempt || value.LeaseGeneration != request.Lease.Generation {
		t.Fatal("Foundry settlement lost the identical retried prompt's identity or lease")
	}
	state.providerProxy.foundry.mu.Lock()
	remotePrompt := state.providerProxy.foundry.prompt
	settlementProven := remotePrompt != nil && remotePrompt.closed && remotePrompt.settlementProven
	state.providerProxy.foundry.mu.Unlock()
	if !settlementProven {
		t.Fatal("completed retry did not retain proven Foundry settlement")
	}
}

func assertFoundryPromptAdmissionInactive(t *testing.T, state *sessionState) {
	t.Helper()
	state.providerProxy.mu.Lock()
	providerInactive := state.providerProxy.activePromptID == "" && state.providerProxy.turnPromptID == "" &&
		state.providerProxy.gateContext == nil && state.providerProxy.leaseTimer == nil && state.providerProxy.leaseVersion == 0
	state.providerProxy.mu.Unlock()
	state.providerProxy.foundry.mu.Lock()
	foundryInactive := state.providerProxy.foundry.prompt == nil && !state.providerProxy.foundry.retiring &&
		state.providerProxy.foundry.retireDone == nil
	state.providerProxy.foundry.mu.Unlock()
	state.mcpProxy.mu.Lock()
	mcpInactive := state.mcpProxy.state == harnessv2.RuntimeSessionStateIdle && state.mcpProxy.authorization == nil &&
		state.mcpProxy.gateContext == nil && state.mcpProxy.leaseTimer == nil && state.mcpProxy.leaseVersion == 0
	state.mcpProxy.mu.Unlock()
	if !providerInactive || !foundryInactive || !mcpInactive {
		t.Fatal("pending cancellation activated provider/MCP authority or created/closed a Foundry prompt identity")
	}
}
