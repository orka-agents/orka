package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/harness"
	"github.com/orka-agents/orka/internal/harness/conformance"
)

const (
	fakeSessionID         = "session-1"
	testContinuationProof = "proof-for-test"
)

func TestResponsesAdapterObservedTurnCompletes(t *testing.T) {
	foundry := newFakeResponses(t, fakeResponsesConfig{scenario: "observed"})
	adapter := newTestResponsesAdapter(t, foundry.endpoint(), nil)
	client := newHarnessClient(t, adapter)

	request := responsesStartTurnRequest("foundry-observed")
	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	var frames []harness.HarnessEventFrame
	if err := client.StreamFrames(context.Background(), request.TurnID, 0, func(frame harness.HarnessEventFrame) error {
		frames = append(frames, frame)
		return nil
	}); err != nil {
		t.Fatalf("StreamFrames: %v", err)
	}
	if !hasFrameType(frames, harness.FrameTurnStarted) || !hasFrameType(frames, harness.FrameTurnCompleted) {
		t.Fatalf("frames = %#v, want started and completed", frames)
	}
	if got := frames[len(frames)-1].Completed.Result; got != "foundry final answer" {
		t.Fatalf("result = %q", got)
	}
	if foundry.sawRequestLevelTools.Load() {
		t.Fatalf("hosted Responses request included request-level tools")
	}
	if got := foundry.requestHeader(0).Get("Foundry-Features"); got != "HostedAgents=V1Preview" {
		t.Fatalf("Foundry-Features = %q, want HostedAgents=V1Preview", got)
	}
	assertJSONFileEqual(t, "testdata/golden/01_initial_hosted_request.json", foundry.requestBody(0))
}

func TestResponsesAdapterDoesNotFollowCredentialedRedirects(t *testing.T) {
	redirectTargetHit := atomic.Bool{}
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirectTargetHit.Store(true)
	}))
	t.Cleanup(redirectTarget.Close)

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTarget.URL+"/capture", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirector.Close)

	endpoint := redirector.URL +
		"/agents/test-agent/endpoint/protocols/openai/responses?api-version=v1"
	adapter := newTestResponsesAdapter(t, endpoint, nil)
	client := newHarnessClient(t, adapter)
	request := responsesStartTurnRequest("foundry-redirect")
	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn should accept terminal redirect failure: %v", err)
	}
	frames := streamCurrentFrames(t, client, request.TurnID)
	if failed := findFrame(frames, harness.FrameTurnFailed); failed == nil ||
		failed.Failed.Reason != foundryInitialUnknown {
		t.Fatalf("failed frame = %#v, want foundry_initial_unknown", failed)
	}
	if redirectTargetHit.Load() {
		t.Fatal("redirect target was called; credentialed Foundry request followed an unvalidated redirect")
	}
}

func TestResponsesAdapterBrokeredReadContinuationAndGoldenFixtures(t *testing.T) {
	foundry := newFakeResponses(t, fakeResponsesConfig{scenario: "function_call", toolName: "support-ticket-lookup"})
	adapter := newTestResponsesAdapter(
		t,
		foundry.endpoint(),
		[]harness.BrokeredToolClass{harness.BrokeredToolClassRead},
	)
	client := newHarnessClient(t, adapter)

	request := responsesStartTurnRequest("foundry-brokered")
	request.ToolExecutionMode = harness.ToolExecutionModeBrokered
	request.Input.Tools = []harness.ToolDefinition{{
		Name:          "support-ticket-lookup",
		Description:   "Look up support ticket",
		BrokeredClass: harness.BrokeredToolClassRead,
		Parameters:    json.RawMessage(`{"type":"object","properties":{"incident":{"type":"string"}}}`),
	}}
	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	if foundry.sawRequestLevelTools.Load() {
		t.Fatalf("hosted Responses request included request-level tools")
	}
	assertJSONFileEqual(t, "testdata/golden/01_initial_hosted_request.json", foundry.requestBody(0))

	var frames []harness.HarnessEventFrame
	var continueRequest harness.ContinueTurnRequest
	streamCtx, streamCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer streamCancel()
	if err := client.StreamFrames(streamCtx, request.TurnID, 0, func(frame harness.HarnessEventFrame) error {
		frames = append(frames, frame)
		if frame.Type != harness.FrameToolCallRequested {
			return nil
		}
		continueRequest = goldenContinueRequest(request, frame.ToolCallID, json.RawMessage(`{"success":true}`))
		_, err := client.ContinueTurn(context.Background(), continueRequest)
		return err
	}); err != nil {
		t.Fatalf("StreamFrames through continue: %v", err)
	}
	requested := findFrame(frames, harness.FrameToolCallRequested)
	if requested == nil {
		t.Fatalf("frames = %#v, want tool request", frames)
	}
	if requested.ToolName != "support-ticket-lookup" || requested.ToolCallID != "call-1" {
		t.Fatalf("tool request = %#v", requested)
	}
	assertJSONFileEqual(t, "testdata/golden/03_tool_call_requested_frame.json", scrubFrameForGolden(*requested))

	assertJSONFileEqual(t, "testdata/golden/04_orka_continue_request.json", continueRequest)
	assertJSONFileEqual(t, "testdata/golden/05_hosted_continuation_request.json", foundry.requestBody(1))
	if got := foundry.requestHeader(1).Get("x-agent-session-id"); got != fakeSessionID {
		t.Fatalf("continuation x-agent-session-id = %q, want session-1", got)
	}
	if !hasFrameType(frames, harness.FrameToolResultReceived) || !hasFrameType(frames, harness.FrameTurnCompleted) {
		t.Fatalf("frames = %#v, want tool result and completion", frames)
	}
	if foundry.postCount.Load() != 2 {
		t.Fatalf("hosted post count = %d, want 2", foundry.postCount.Load())
	}
}

func TestResponsesAdapterRuntimeSessionHeaderReuse(t *testing.T) {
	foundry := newFakeResponses(t, fakeResponsesConfig{scenario: "observed"})
	adapter := newTestResponsesAdapter(t, foundry.endpoint(), nil)
	client := newHarnessClient(t, adapter)

	first := responsesStartTurnRequest("foundry-session-one")
	if _, err := client.StartTurn(context.Background(), first); err != nil {
		t.Fatalf("StartTurn first: %v", err)
	}
	second := responsesStartTurnRequest("foundry-session-two")
	second.RuntimeSessionID = first.RuntimeSessionID
	if _, err := client.StartTurn(context.Background(), second); err != nil {
		t.Fatalf("StartTurn second: %v", err)
	}
	if got := foundry.requestHeader(1).Get("x-agent-session-id"); got != fakeSessionID {
		t.Fatalf("same runtimeSessionID header = %q, want session-1", got)
	}
	if got := requestMap(t, foundry.requestBody(1))["agent_session_id"]; got != fakeSessionID {
		t.Fatalf("same runtimeSessionID body agent_session_id = %#v, want %q", got, fakeSessionID)
	}

	third := responsesStartTurnRequest("foundry-session-three")
	if _, err := client.StartTurn(context.Background(), third); err != nil {
		t.Fatalf("StartTurn third: %v", err)
	}
	if got := foundry.requestHeader(2).Get("x-agent-session-id"); got != "" {
		t.Fatalf("new runtimeSessionID header = %q, want empty", got)
	}
}

func TestResponsesAdapterInterleavedResponsesRetainResponseSpecificSession(t *testing.T) {
	foundry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := ioReadAll(r.Body)
		var request responsesRequest
		if err := json.Unmarshal(body, &request); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		prompt, _ := request.Input.(string)
		writeJSON(w, map[string]any{
			"id":               "response-" + prompt,
			"agent_session_id": "session-" + prompt,
			"status":           "completed",
			"output": []any{map[string]any{
				"type":    "message",
				"role":    "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": "done"}},
			}},
		})
	}))
	t.Cleanup(foundry.Close)
	server := newServer(config{
		runtimeName:    "foundry-responses-test",
		adapterBearer:  "adapter-auth-value",
		endpoint:       foundry.URL + "/agents/test-agent/endpoint/protocols/openai/responses?api-version=v1",
		foundryAuth:    "foundry-auth-value",
		requestTimeout: time.Second,
		stateRetention: time.Minute,
	}, &http.Client{Timeout: time.Second})
	firstRequest := responsesStartTurnRequest("foundry-interleaved-session-a")
	secondRequest := responsesStartTurnRequest("foundry-interleaved-session-b")
	secondRequest.RuntimeSessionID = firstRequest.RuntimeSessionID

	var firstResponse responsesResponse
	firstSession, err := server.postResponses(
		context.Background(),
		responsesRequest{Input: "a"},
		&firstResponse,
	)
	if err != nil {
		t.Fatalf("first postResponses: %v", err)
	}
	var secondResponse responsesResponse
	secondSession, err := server.postResponses(
		context.Background(),
		responsesRequest{Input: "b"},
		&secondResponse,
	)
	if err != nil {
		t.Fatalf("second postResponses: %v", err)
	}

	firstTurn := &turnState{request: firstRequest}
	secondTurn := &turnState{request: secondRequest}
	server.mu.Lock()
	server.setTurnSessionLocked(secondTurn, secondSession)
	server.setTurnSessionLocked(firstTurn, firstSession)
	server.mu.Unlock()
	if firstTurn.foundrySessionID != "session-a" {
		t.Fatalf("first turn session = %q, want session-a", firstTurn.foundrySessionID)
	}
	if secondTurn.foundrySessionID != "session-b" {
		t.Fatalf("second turn session = %q, want session-b", secondTurn.foundrySessionID)
	}
}

func TestResponsesAdapterContinuationUsesTurnSessionAfterSessionMapCleanup(t *testing.T) {
	foundry := newFakeResponses(t, fakeResponsesConfig{scenario: "function_call", toolName: "support-ticket-lookup"})
	adapter, server := newTestResponsesAdapterWithServer(
		t,
		foundry.endpoint(),
		[]harness.BrokeredToolClass{harness.BrokeredToolClassRead},
	)
	client := newHarnessClient(t, adapter)
	request := brokeredReadRequest("foundry-session-cleanup")

	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	frames := streamCurrentFrames(t, client, request.TurnID)
	requested := findFrame(frames, harness.FrameToolCallRequested)
	if requested == nil {
		t.Fatalf("frames = %#v, want tool request", frames)
	}

	server.mu.Lock()
	delete(server.runtimeSessions, request.RuntimeSessionID)
	server.mu.Unlock()

	continueRequest := goldenContinueRequest(request, requested.ToolCallID, json.RawMessage(`{"success":true}`))
	if _, err := client.ContinueTurn(context.Background(), continueRequest); err != nil {
		t.Fatalf("ContinueTurn after runtime session cleanup: %v", err)
	}
	if got := requestMap(t, foundry.requestBody(1))["agent_session_id"]; got != fakeSessionID {
		t.Fatalf("agent_session_id after runtime session cleanup = %#v, want %q", got, fakeSessionID)
	}
}

func TestResponsesAdapterDuplicateStartDuringInitializationRejected(t *testing.T) {
	received := make(chan struct{})
	release := make(chan struct{})
	foundry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		select {
		case <-received:
		default:
			close(received)
		}
		<-release
		writeJSON(w, finalResponsesMessage())
	}))
	t.Cleanup(foundry.Close)
	endpoint := foundry.URL + "/agents/test-agent/endpoint/protocols/openai/responses?api-version=v1"
	adapter := newTestResponsesAdapter(t, endpoint, nil)
	client := newHarnessClient(t, adapter)
	request := responsesStartTurnRequest("foundry-duplicate-start")

	firstErr := make(chan error, 1)
	go func() {
		_, err := client.StartTurn(context.Background(), request)
		firstErr <- err
	}()
	<-received
	if _, err := client.StartTurn(context.Background(), request); err == nil ||
		!strings.Contains(err.Error(), "initialization in progress") {
		t.Fatalf("duplicate StartTurn error = %v, want initialization conflict", err)
	}
	close(release)
	if err := <-firstErr; err != nil {
		t.Fatalf("initial StartTurn: %v", err)
	}
}

func TestResponsesAdapterEnforcesAdvertisedConcurrentTurnLimit(t *testing.T) {
	received := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseFoundry := func() { releaseOnce.Do(func() { close(release) }) }
	foundry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case <-received:
		default:
			close(received)
		}
		<-release
		writeJSON(w, finalResponsesMessage())
	}))
	t.Cleanup(foundry.Close)
	endpoint := foundry.URL + "/agents/test-agent/endpoint/protocols/openai/responses?api-version=v1"
	adapter := newTestResponsesAdapter(t, endpoint, nil)
	t.Cleanup(releaseFoundry)
	client := newHarnessClient(t, adapter)
	first := responsesStartTurnRequest("foundry-concurrent-first")
	second := responsesStartTurnRequest("foundry-concurrent-second")
	second.RuntimeSessionID = first.RuntimeSessionID

	firstErr := make(chan error, 1)
	go func() {
		_, err := client.StartTurn(context.Background(), first)
		firstErr <- err
	}()
	select {
	case <-received:
	case <-time.After(time.Second):
		t.Fatal("first Foundry request did not arrive")
	}
	if _, err := client.StartTurn(context.Background(), second); err == nil ||
		!strings.Contains(err.Error(), "maximum concurrent turns reached") {
		t.Fatalf("second concurrent StartTurn error = %v, want admission rejection", err)
	}

	releaseFoundry()
	select {
	case err := <-firstErr:
		if err != nil {
			t.Fatalf("first StartTurn: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("first StartTurn did not finish")
	}
	if _, err := client.StartTurn(context.Background(), second); err != nil {
		t.Fatalf("second StartTurn after first completed: %v", err)
	}
}

func TestResponsesAdapterDiscardsUnusedTurnEnvironment(t *testing.T) {
	foundry := newFakeResponses(t, fakeResponsesConfig{scenario: "observed"})
	adapter, server := newTestResponsesAdapterWithServer(t, foundry.endpoint(), nil)
	client := newHarnessClient(t, adapter)
	request := responsesStartTurnRequest("foundry-env-redaction")
	request.Input.Env = []harness.TurnEnvVar{{Name: "SENSITIVE_VALUE", Value: "do-not-retain"}}

	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	server.mu.Lock()
	turn := server.turns[request.TurnID]
	var retainedRequest harness.StartTurnRequest
	if turn != nil {
		retainedRequest = turn.request
	}
	server.mu.Unlock()
	if turn == nil {
		t.Fatal("turn was not retained for duplicate handling")
	}
	if len(retainedRequest.Input.Env) != 0 {
		t.Fatalf("retained turn env = %#v, want discarded credentials", retainedRequest.Input.Env)
	}
	encoded, err := json.Marshal(retainedRequest)
	if err != nil {
		t.Fatalf("marshal retained request: %v", err)
	}
	if bytes.Contains(encoded, []byte("do-not-retain")) {
		t.Fatalf("retained request contains discarded environment value: %s", encoded)
	}
	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("duplicate StartTurn with original environment: %v", err)
	}
}

func TestResponsesAdapterRejectedResponseDoesNotRetainSession(t *testing.T) {
	foundry := newFakeResponses(t, fakeResponsesConfig{scenario: "failed_with_session"})
	adapter, server := newTestResponsesAdapterWithServer(t, foundry.endpoint(), nil)
	client := newHarnessClient(t, adapter)
	first := responsesStartTurnRequest("foundry-rejected-session-first")

	if _, err := client.StartTurn(context.Background(), first); err != nil {
		t.Fatalf("first StartTurn: %v", err)
	}
	frames := streamCurrentFrames(t, client, first.TurnID)
	if failed := findFrame(frames, harness.FrameTurnFailed); failed == nil || failed.Failed.Reason != "foundry_failed" {
		t.Fatalf("failed frame = %#v, want foundry_failed", failed)
	}
	server.mu.Lock()
	_, retained := server.runtimeSessions[first.RuntimeSessionID]
	server.mu.Unlock()
	if retained {
		t.Fatal("rejected hosted response retained runtime session")
	}

	second := responsesStartTurnRequest("foundry-rejected-session-second")
	second.RuntimeSessionID = first.RuntimeSessionID
	if _, err := client.StartTurn(context.Background(), second); err != nil {
		t.Fatalf("second StartTurn: %v", err)
	}
	if got := requestMap(t, foundry.requestBody(1))["agent_session_id"]; got != nil {
		t.Fatalf("second request agent_session_id = %#v, want no rejected session reuse", got)
	}
}

func TestResponsesAdapterCancelDuringInitialPostCancelsHostedRequest(t *testing.T) {
	received := make(chan struct{})
	hostedRequestCancelled := make(chan struct{})
	releaseHostedResponse := make(chan struct{})
	var releaseOnce sync.Once
	releaseResponse := func() { releaseOnce.Do(func() { close(releaseHostedResponse) }) }
	var postCount atomic.Int32
	backendClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if postCount.Add(1) > 1 {
			return jsonHTTPResponse(r, finalResponsesMessage())
		}
		body, err := ioReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		var decoded map[string]any
		if err := json.Unmarshal(body, &decoded); err != nil {
			return nil, err
		}
		if got := decoded["agent_session_id"]; got != "existing-session" {
			return nil, fmt.Errorf("agent_session_id = %#v, want existing-session", got)
		}
		close(received)
		<-r.Context().Done()
		close(hostedRequestCancelled)
		// Model a hosted service that accepted the request before client-side
		// cancellation and later returns success despite the canceled context.
		<-releaseHostedResponse
		return jsonHTTPResponseWithSession(r, finalResponsesMessage(), "cancelled-session")
	})}
	endpoint := "https://foundry.example/agents/test-agent/endpoint/protocols/openai/responses?api-version=v1"
	adapter, server := newTestResponsesAdapterWithHTTPClient(t, endpoint, nil, backendClient)
	t.Cleanup(releaseResponse)
	client := newHarnessClient(t, adapter)
	request := responsesStartTurnRequest("foundry-cancel-initial")
	server.mu.Lock()
	server.runtimeSessions[request.RuntimeSessionID] = foundrySession{
		ID:       "existing-session",
		LastSeen: time.Now().UTC(),
	}
	server.mu.Unlock()

	startErr := make(chan error, 1)
	go func() {
		_, err := client.StartTurn(context.Background(), request)
		startErr <- err
	}()
	select {
	case <-received:
	case <-time.After(time.Second):
		t.Fatal("initial Foundry request did not arrive")
	}
	if _, err := client.CancelTurn(context.Background(), cancelRequestForStart(request)); err != nil {
		t.Fatalf("CancelTurn: %v", err)
	}
	select {
	case <-hostedRequestCancelled:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not cancel the initial hosted request context")
	}
	releaseResponse()
	select {
	case err := <-startErr:
		if err != nil {
			t.Fatalf("StartTurn after cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("StartTurn did not finish after the late hosted response")
	}

	server.mu.Lock()
	turn := server.turns[request.TurnID]
	_, sessionRetained := server.runtimeSessions[request.RuntimeSessionID]
	_, sessionQuarantined := server.quarantinedSessions[request.RuntimeSessionID]
	foundrySessionID := ""
	if turn != nil {
		foundrySessionID = turn.foundrySessionID
	}
	server.mu.Unlock()
	if turn == nil || !turn.completed || !hasFrameType(turn.frames, harness.FrameTurnCancelled) ||
		hasFrameType(turn.frames, harness.FrameTurnCompleted) {
		t.Fatalf("turn = %#v, want retained terminal cancellation", turn)
	}
	if sessionRetained || foundrySessionID != "" || !sessionQuarantined {
		t.Fatalf(
			"cancelled initial post retained session map=%v turnSession=%q quarantined=%v",
			sessionRetained,
			foundrySessionID,
			sessionQuarantined,
		)
	}

	quarantined := responsesStartTurnRequest("foundry-quarantined-after-cancel-initial")
	quarantined.RuntimeSessionID = request.RuntimeSessionID
	if _, err := client.StartTurn(context.Background(), quarantined); err == nil ||
		!strings.Contains(err.Error(), "runtime session unavailable after unconfirmed hosted cancellation") {
		t.Fatalf("StartTurn with quarantined runtime session error = %v", err)
	}
	second := responsesStartTurnRequest("foundry-after-cancel-initial")
	if _, err := client.StartTurn(context.Background(), second); err != nil {
		t.Fatalf("StartTurn with a different runtime session after cancellation: %v", err)
	}
}

func TestResponsesAdapterInitialPostSurvivesControlDisconnect(t *testing.T) {
	received := make(chan struct{})
	release := make(chan struct{})
	var postCount atomic.Int32
	foundry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		postCount.Add(1)
		select {
		case <-received:
		default:
			close(received)
		}
		select {
		case <-release:
			writeJSON(w, finalResponsesMessage())
		case <-r.Context().Done():
			return
		}
	}))
	t.Cleanup(foundry.Close)
	endpoint := foundry.URL + "/agents/test-agent/endpoint/protocols/openai/responses?api-version=v1"
	adapter, adapterServer := newTestResponsesAdapterWithServer(t, endpoint, nil)
	client := newHarnessClient(t, adapter)
	request := responsesStartTurnRequest("foundry-control-disconnect")

	ctx, cancel := context.WithCancel(context.Background())
	startErr := make(chan error, 1)
	go func() {
		_, err := client.StartTurn(ctx, request)
		startErr <- err
	}()
	<-received
	cancel()
	if err := <-startErr; err == nil {
		t.Fatal("StartTurn after control disconnect error = nil, want client cancellation")
	}
	close(release)

	deadline := time.Now().Add(time.Second)
	for {
		adapterServer.mu.Lock()
		turn := adapterServer.turns[request.TurnID]
		completed := turn != nil && turn.completed && !turn.initializing
		adapterServer.mu.Unlock()
		if completed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("adapter did not retain and complete the initial hosted response after control disconnect")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("idempotent StartTurn retry: %v", err)
	}
	if got := postCount.Load(); got != 1 {
		t.Fatalf("hosted post count = %d, want 1 after control disconnect and retry", got)
	}
}

func TestResponsesAdapterQuarantineCapacityFailsClosed(t *testing.T) {
	server := newServer(config{
		runtimeName:    "foundry-responses-test",
		adapterBearer:  "adapter-auth-value",
		endpoint:       "https://foundry.example/agents/test-agent/endpoint/protocols/openai/responses?api-version=v1",
		foundryAuth:    "foundry-auth-value",
		requestTimeout: time.Second,
		stateRetention: time.Minute,
	}, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("hosted request should not be attempted after quarantine saturation")
	})})
	server.mu.Lock()
	for i := range maxQuarantinedRuntimeSessions {
		server.quarantineRuntimeSessionLocked(&turnState{request: responsesStartTurnRequest(fmt.Sprintf("quarantine-%d", i))})
	}
	if got := len(server.quarantinedSessions); got != maxQuarantinedRuntimeSessions {
		server.mu.Unlock()
		t.Fatalf("quarantine count = %d, want %d", got, maxQuarantinedRuntimeSessions)
	}
	if server.quarantineSaturated {
		server.mu.Unlock()
		t.Fatal("quarantine saturated before exceeding the bounded tombstone capacity")
	}
	server.quarantineRuntimeSessionLocked(&turnState{request: responsesStartTurnRequest("quarantine-overflow")})
	gotCount := len(server.quarantinedSessions)
	saturated := server.quarantineSaturated
	server.mu.Unlock()
	if gotCount != maxQuarantinedRuntimeSessions || !saturated {
		t.Fatalf("quarantine count=%d saturated=%v, want bounded saturated state", gotCount, saturated)
	}

	adapter := httptest.NewServer(server.handler())
	t.Cleanup(adapter.Close)
	client := newHarnessClient(t, adapter)
	afterSaturation := responsesStartTurnRequest("after-quarantine-saturation")
	if _, err := client.StartTurn(context.Background(), afterSaturation); err == nil ||
		!strings.Contains(err.Error(), "quarantine capacity exhausted") {
		t.Fatalf("StartTurn after quarantine saturation error = %v", err)
	}
	response, err := http.Get(adapter.URL + readinessPath) //nolint:gosec // Test-only loopback server.
	if err != nil {
		t.Fatalf("GET readiness: %v", err)
	}
	defer response.Body.Close() //nolint:errcheck
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readiness status = %d, want 503 after quarantine saturation", response.StatusCode)
	}
}

func TestResponsesAdapterFoundryRequestContextDetachesAndUsesEarlierTurnDeadline(t *testing.T) {
	server := newServer(config{requestTimeout: time.Second}, &http.Client{Timeout: time.Second})
	parent, cancelParent := context.WithCancel(context.Background())
	cancelParent()
	turnDeadline := time.Now().Add(100 * time.Millisecond)
	ctx, cancel := server.foundryRequestContext(parent, turnDeadline)
	defer cancel()
	if err := ctx.Err(); err != nil {
		t.Fatalf("detached context inherited control cancellation: %v", err)
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("detached context has no deadline")
	}
	if delta := deadline.Sub(turnDeadline); delta < -10*time.Millisecond || delta > 10*time.Millisecond {
		t.Fatalf("deadline = %v, want turn deadline %v", deadline, turnDeadline)
	}
	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("detached context error = %v, want deadline exceeded", ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("detached context did not stop at turn deadline")
	}
}

func TestResponsesAdapterInitialPostHonorsTurnDeadline(t *testing.T) {
	foundry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(900 * time.Millisecond)
		writeJSON(w, finalResponsesMessage())
	}))
	t.Cleanup(foundry.Close)
	endpoint := foundry.URL + "/agents/test-agent/endpoint/protocols/openai/responses?api-version=v1"
	adapter, server := newTestResponsesAdapterWithServer(t, endpoint, nil)
	client := newHarnessClient(t, adapter)
	request := responsesStartTurnRequest("foundry-initial-deadline")
	request.Deadline = time.Now().Add(150 * time.Millisecond)

	started := time.Now()
	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn should accept terminal deadline failure: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 800*time.Millisecond {
		t.Fatalf("StartTurn elapsed = %v, want turn deadline to beat request timeout", elapsed)
	}
	frames := streamCurrentFrames(t, client, request.TurnID)
	if failed := findFrame(frames, harness.FrameTurnFailed); failed == nil ||
		failed.Failed.Reason != foundryInitialUnknown {
		t.Fatalf("failed frame = %#v, want foundry_initial_unknown", failed)
	}
	server.mu.Lock()
	_, quarantined := server.quarantinedSessions[request.RuntimeSessionID]
	server.mu.Unlock()
	if !quarantined {
		t.Fatal("deadline-uncertain initial request did not quarantine its runtime session")
	}
	retry := responsesStartTurnRequest("foundry-initial-deadline-retry")
	retry.RuntimeSessionID = request.RuntimeSessionID
	if _, err := client.StartTurn(context.Background(), retry); err == nil ||
		!strings.Contains(err.Error(), "runtime session unavailable after unconfirmed hosted cancellation") {
		t.Fatalf("StartTurn with deadline-quarantined runtime session error = %v", err)
	}
}

func TestResponsesAdapterRequiresProofForBrokeredReadinessAndStart(t *testing.T) {
	foundry := newFakeResponses(t, fakeResponsesConfig{scenario: "observed"})
	server := newServer(config{
		runtimeName:         "foundry-responses-test",
		adapterBearer:       "adapter-auth-value",
		endpoint:            foundry.endpoint(),
		foundryAuth:         "foundry-auth-value",
		requestTimeout:      time.Second,
		stateRetention:      time.Minute,
		brokeredToolClasses: []harness.BrokeredToolClass{harness.BrokeredToolClassRead},
	}, &http.Client{Timeout: time.Second})
	adapter := httptest.NewServer(server.handler())
	t.Cleanup(adapter.Close)

	response, err := http.Get(adapter.URL + readinessPath)
	if err != nil {
		t.Fatalf("GET ready: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("ready status = %d, want 503", response.StatusCode)
	}
	client := newHarnessClient(t, adapter)
	capabilities, err := client.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if slices.Contains(capabilities.ToolExecutionModes, harness.ToolExecutionModeBrokered) ||
		capabilities.SupportsContinuation || len(capabilities.BrokeredToolClasses) != 0 {
		t.Fatalf("capabilities = %#v, missing proof must disable brokered advertisement", capabilities)
	}
	request := brokeredReadRequest("foundry-missing-proof")
	if _, err := client.StartTurn(context.Background(), request); err == nil ||
		!strings.Contains(err.Error(), envContinuationProof) {
		t.Fatalf("StartTurn missing proof error = %v", err)
	}
	if got := foundry.postCount.Load(); got != 0 {
		t.Fatalf("hosted post count = %d, want no request without continuation proof", got)
	}
}

func TestResponsesAdapterReadyEndpointReflectsConfigReadiness(t *testing.T) {
	unready := httptest.NewServer(newServer(config{
		runtimeName:    "foundry-responses-test",
		adapterBearer:  "adapter-auth-value",
		endpoint:       "https://example.com/agents/a/endpoint/protocols/openai/responses?api-version=v1",
		requestTimeout: time.Second,
	}, &http.Client{Timeout: time.Second}).handler())
	t.Cleanup(unready.Close)
	resp, err := http.Get(unready.URL + readinessPath) //nolint:gosec,noctx // local test server
	if err != nil {
		t.Fatalf("GET unready: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unready status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}

	foundry := newFakeResponses(t, fakeResponsesConfig{scenario: "observed"})
	ready := newTestResponsesAdapter(t, foundry.endpoint(), nil)
	readyResp, err := http.Get(ready.URL + readinessPath) //nolint:gosec,noctx // local test server
	if err != nil {
		t.Fatalf("GET ready: %v", err)
	}
	defer readyResp.Body.Close() //nolint:errcheck
	if readyResp.StatusCode != http.StatusOK {
		t.Fatalf("ready status = %d, want %d", readyResp.StatusCode, http.StatusOK)
	}
}

func TestResponsesAdapterHTTPServerHasBoundedReadTimeouts(t *testing.T) {
	httpServer := newAdapterHTTPServer(":0", http.NewServeMux())
	if got := httpServer.ReadHeaderTimeout; got != defaultReadHeaderTimeout {
		t.Fatalf("ReadHeaderTimeout = %v, want %v", got, defaultReadHeaderTimeout)
	}
	if got := httpServer.ReadTimeout; got != defaultReadTimeout {
		t.Fatalf("ReadTimeout = %v, want %v", got, defaultReadTimeout)
	}
	if got := httpServer.IdleTimeout; got != defaultIdleTimeout {
		t.Fatalf("IdleTimeout = %v, want %v", got, defaultIdleTimeout)
	}
}

func TestResponsesAdapterPassesObservedConformanceByDefault(t *testing.T) {
	foundry := newFakeResponses(t, fakeResponsesConfig{scenario: "observed"})
	adapter := newTestResponsesAdapter(t, foundry.endpoint(), nil)
	defer adapter.Close()

	result := conformance.Check(context.Background(), conformance.Target{
		BaseURL:        adapter.URL,
		BearerToken:    "adapter-auth-value",
		ControlTimeout: 2 * time.Second,
		ProbeTurn:      true,
		RequireAuth:    true,
	})
	if !result.Passed {
		t.Fatalf("observed conformance failed: %s failures=%v", result.Message, result.Failures)
	}
	caps := result.ObservedCapabilities
	if caps == nil {
		t.Fatal("ObservedCapabilities = nil")
	}
	if !reflect.DeepEqual(caps.ToolExecutionModes, []harness.ToolExecutionMode{harness.ToolExecutionModeObserved}) {
		t.Fatalf("ToolExecutionModes = %#v, want observed only", caps.ToolExecutionModes)
	}
	if len(caps.BrokeredToolClasses) != 0 {
		t.Fatalf("BrokeredToolClasses = %#v, want none", caps.BrokeredToolClasses)
	}
	if caps.SupportsContinuation {
		t.Fatal("SupportsContinuation = true, want false when no brokered classes are configured")
	}
}

func TestResponsesAdapterPassesBrokeredReadConformance(t *testing.T) {
	foundry := newFakeResponses(t, fakeResponsesConfig{scenario: "function_call", toolName: "conformance_read"})
	adapter := newTestResponsesAdapter(
		t,
		foundry.endpoint(),
		[]harness.BrokeredToolClass{harness.BrokeredToolClassRead},
	)
	defer adapter.Close()

	result := conformance.Check(context.Background(), conformance.Target{
		BaseURL:           adapter.URL,
		BearerToken:       "adapter-auth-value",
		ControlTimeout:    2 * time.Second,
		ProbeBrokeredRead: true,
		RequireAuth:       true,
	})
	if !result.Passed {
		t.Fatalf("brokered read conformance failed: %s failures=%v", result.Message, result.Failures)
	}
	if foundry.sawRequestLevelTools.Load() {
		t.Fatalf("hosted Responses request included request-level tools")
	}
}

func TestResponsesAdapterPassesBrokeredWriteConformance(t *testing.T) {
	foundry := newFakeResponses(t, fakeResponsesConfig{scenario: "function_call", toolName: "conformance_write"})
	adapter := newTestResponsesAdapter(
		t,
		foundry.endpoint(),
		[]harness.BrokeredToolClass{harness.BrokeredToolClassWrite},
	)
	defer adapter.Close()

	result := conformance.Check(context.Background(), conformance.Target{
		BaseURL:            adapter.URL,
		BearerToken:        "adapter-auth-value",
		ControlTimeout:     2 * time.Second,
		ProbeBrokeredWrite: true,
		RequireAuth:        true,
	})
	if !result.Passed {
		t.Fatalf("brokered write conformance failed: %s failures=%v", result.Message, result.Failures)
	}
}

func TestResponsesAdapterWriteParksUntilDeclinedApprovalContinue(t *testing.T) {
	foundry := newFakeResponses(t, fakeResponsesConfig{scenario: "function_call", toolName: "dispatch-work-order"})
	adapter := newTestResponsesAdapter(
		t,
		foundry.endpoint(),
		[]harness.BrokeredToolClass{harness.BrokeredToolClassWrite},
	)
	client := newHarnessClient(t, adapter)
	request := brokeredWriteRequest("foundry-write-declined")

	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	frames := streamCurrentFrames(t, client, request.TurnID)
	requested := findFrame(frames, harness.FrameToolCallRequested)
	if requested == nil {
		t.Fatalf("frames = %#v, want write tool request", frames)
	}
	if requested.ToolName != "dispatch-work-order" {
		t.Fatalf("ToolName = %q, want dispatch-work-order", requested.ToolName)
	}
	if hasFrameType(frames, harness.FrameToolResultReceived) || hasFrameType(frames, harness.FrameTurnCompleted) {
		t.Fatalf("frames = %#v, write should park until Orka continuation", frames)
	}
	if foundry.postCount.Load() != 1 {
		t.Fatalf("hosted post count before approval = %d, want 1", foundry.postCount.Load())
	}

	declined := harness.ContinueTurnRequest{
		Version:          harness.ProtocolVersion,
		Namespace:        request.Namespace,
		TaskName:         request.TaskName,
		SessionName:      request.SessionName,
		RuntimeSessionID: request.RuntimeSessionID,
		TurnID:           request.TurnID,
		CorrelationID:    request.CorrelationID,
		ToolResults: []harness.ToolCallResult{toolResultForRequest(
			request,
			requested.ToolCallID,
			false,
			nil,
			&harness.ErrorInfo{Code: "approval_declined", Message: "human declined"},
		)},
	}
	if _, err := client.ContinueTurn(context.Background(), declined); err != nil {
		t.Fatalf("ContinueTurn declined approval: %v", err)
	}
	continuation := requestMap(t, foundry.requestBody(1))
	items, ok := continuation["input"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("continuation input = %#v, want one item", continuation["input"])
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("continuation item = %#v, want object", items[0])
	}
	wantOutput := `{"approved":false,"error":{"code":"approval_declined","message":"human declined"}}`
	if got := item["output"]; got != wantOutput {
		t.Fatalf("declined output = %#v, want %s", got, wantOutput)
	}
	frames = streamCurrentFrames(t, client, request.TurnID)
	toolResult := findFrame(frames, harness.FrameToolResultReceived)
	if toolResult == nil || toolResult.Error == nil || toolResult.Error.Code != "approval_declined" {
		t.Fatalf("tool result frame = %#v, want approval_declined", toolResult)
	}
	if !hasFrameType(frames, harness.FrameTurnCompleted) {
		t.Fatalf("frames = %#v, want final completion after declined continuation", frames)
	}
}

func TestResponsesAdapterRejectsUnknownToolBeforeOrkaExecution(t *testing.T) {
	foundry := newFakeResponses(t, fakeResponsesConfig{scenario: "function_call", toolName: "unknown-tool"})
	adapter := newTestResponsesAdapter(
		t,
		foundry.endpoint(),
		[]harness.BrokeredToolClass{harness.BrokeredToolClassRead},
	)
	client := newHarnessClient(t, adapter)
	request := brokeredReadRequest("foundry-unknown-tool")

	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	frames := streamCurrentFrames(t, client, request.TurnID)
	if hasFrameType(frames, harness.FrameToolCallRequested) {
		t.Fatalf("frames = %#v, should not request Orka execution for an unknown tool", frames)
	}
	failed := findFrame(frames, harness.FrameTurnFailed)
	if failed == nil || !strings.Contains(failed.Failed.Message, "did not expose") {
		t.Fatalf("failed frame = %#v, want unknown-tool rejection", failed)
	}
}

func TestResponsesAdapterRejectsMalformedArguments(t *testing.T) {
	foundry := newFakeResponses(
		t,
		fakeResponsesConfig{scenario: "malformed_arguments", toolName: "support-ticket-lookup"},
	)
	adapter := newTestResponsesAdapter(
		t,
		foundry.endpoint(),
		[]harness.BrokeredToolClass{harness.BrokeredToolClassRead},
	)
	client := newHarnessClient(t, adapter)
	request := brokeredReadRequest("foundry-malformed-args")

	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	frames := streamCurrentFrames(t, client, request.TurnID)
	if hasFrameType(frames, harness.FrameToolCallRequested) {
		t.Fatalf("frames = %#v, should not request Orka execution for malformed arguments", frames)
	}
	failed := findFrame(frames, harness.FrameTurnFailed)
	if failed == nil || !strings.Contains(failed.Failed.Message, "arguments") {
		t.Fatalf("failed frame = %#v, want malformed arguments rejection", failed)
	}
}

func TestResponsesAdapterMultipleFunctionCallsBufferedAndContinued(t *testing.T) {
	foundry := newFakeResponses(t, fakeResponsesConfig{scenario: "multiple_calls", toolName: "support-ticket-lookup"})
	adapter := newTestResponsesAdapter(
		t,
		foundry.endpoint(),
		[]harness.BrokeredToolClass{harness.BrokeredToolClassRead},
	)
	client := newHarnessClient(t, adapter)
	request := brokeredReadRequest("foundry-multiple")

	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	frames := streamCurrentFrames(t, client, request.TurnID)
	requests := findFrames(frames, harness.FrameToolCallRequested)
	if len(requests) != 2 {
		t.Fatalf("tool request frames = %#v, want 2", requests)
	}
	continueRequest := harness.ContinueTurnRequest{
		Version:          harness.ProtocolVersion,
		Namespace:        request.Namespace,
		TaskName:         request.TaskName,
		SessionName:      request.SessionName,
		RuntimeSessionID: request.RuntimeSessionID,
		TurnID:           request.TurnID,
		CorrelationID:    request.CorrelationID,
		ToolResults: []harness.ToolCallResult{
			toolResultForRequest(request, "call-1", true, json.RawMessage(`{"success":true,"call":1}`), nil),
			toolResultForRequest(request, "call-2", true, json.RawMessage(`{"success":true,"call":2}`), nil),
		},
	}
	if _, err := client.ContinueTurn(context.Background(), continueRequest); err != nil {
		t.Fatalf("ContinueTurn: %v", err)
	}
	continuation := requestMap(t, foundry.requestBody(1))
	items, ok := continuation["input"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("continuation input = %#v, want two function_call_output items", continuation["input"])
	}
	if got := continuation["agent_session_id"]; got != fakeSessionID {
		t.Fatalf("agent_session_id = %#v, want %q", got, fakeSessionID)
	}
	frames = streamCurrentFrames(t, client, request.TurnID)
	if !hasFrameType(frames, harness.FrameToolResultReceived) || !hasFrameType(frames, harness.FrameTurnCompleted) {
		t.Fatalf("frames = %#v, want tool results and completion", frames)
	}
}

func TestResponsesAdapterDuplicateContinueIsIdempotentAndConflictsReject(t *testing.T) {
	foundry := newFakeResponses(t, fakeResponsesConfig{scenario: "function_call", toolName: "support-ticket-lookup"})
	adapter := newTestResponsesAdapter(
		t,
		foundry.endpoint(),
		[]harness.BrokeredToolClass{harness.BrokeredToolClassRead},
	)
	client := newHarnessClient(t, adapter)
	request := brokeredReadRequest("foundry-duplicate")

	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	frames := streamCurrentFrames(t, client, request.TurnID)
	requested := findFrame(frames, harness.FrameToolCallRequested)
	if requested == nil {
		t.Fatalf("frames = %#v, want tool request", frames)
	}
	continueRequest := goldenContinueRequest(request, requested.ToolCallID, json.RawMessage(`{"success":true}`))
	if _, err := client.ContinueTurn(context.Background(), continueRequest); err != nil {
		t.Fatalf("ContinueTurn first: %v", err)
	}
	if _, err := client.ContinueTurn(context.Background(), continueRequest); err != nil {
		t.Fatalf("ContinueTurn duplicate: %v", err)
	}
	if foundry.postCount.Load() != 2 {
		t.Fatalf("hosted post count after duplicate = %d, want 2", foundry.postCount.Load())
	}
	conflicting := goldenContinueRequest(request, requested.ToolCallID, json.RawMessage(`{"success":false}`))
	if _, err := client.ContinueTurn(context.Background(), conflicting); err == nil {
		t.Fatalf("conflicting duplicate continue succeeded, want conflict")
	}
}

func TestResponsesAdapterSendsBrokeredContinuationProof(t *testing.T) {
	foundry := newFakeResponses(t, fakeResponsesConfig{
		scenario:      "function_call",
		toolName:      "support-ticket-lookup",
		requiredProof: testContinuationProof,
	})
	s := newServer(config{
		runtimeName:         "foundry-responses-test",
		adapterBearer:       "adapter-auth-value",
		endpoint:            foundry.endpoint(),
		foundryAuth:         "foundry-auth-value",
		requestTimeout:      time.Second,
		stateRetention:      time.Minute,
		continuationProof:   testContinuationProof,
		brokeredToolClasses: []harness.BrokeredToolClass{harness.BrokeredToolClassRead},
	}, &http.Client{Timeout: time.Second})
	adapter := httptest.NewServer(s.handler())
	t.Cleanup(adapter.Close)
	client := newHarnessClient(t, adapter)
	request := brokeredReadRequest("foundry-continuation-proof")

	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	frames := streamCurrentFrames(t, client, request.TurnID)
	requested := findFrame(frames, harness.FrameToolCallRequested)
	if requested == nil {
		t.Fatalf("frames = %#v, want tool request", frames)
	}
	continueRequest := goldenContinueRequest(request, requested.ToolCallID, json.RawMessage(`{"success":true}`))
	if _, err := client.ContinueTurn(context.Background(), continueRequest); err != nil {
		t.Fatalf("ContinueTurn: %v", err)
	}
	if got := foundry.requestHeader(1).Get("X-AgentKit-Brokered-Continuation-Proof"); got != testContinuationProof {
		t.Fatalf("continuation proof header = %q, want proof-for-test", got)
	}
	if got := requestMap(t, foundry.requestBody(1))["brokered_continuation_proof"]; got != testContinuationProof {
		t.Fatalf("continuation proof body = %#v, want proof-for-test", got)
	}
	if got := foundry.requestHeader(0).Get("X-AgentKit-Brokered-Continuation-Proof"); got != "" {
		t.Fatalf("initial proof header = %q, want empty", got)
	}
	if _, ok := requestMap(t, foundry.requestBody(0))["brokered_continuation_proof"]; ok {
		t.Fatal("initial request included brokered continuation proof body")
	}
}

func TestResponsesAdapterCancelDuringHostedContinuationCancelsRequest(t *testing.T) {
	continuationReceived := make(chan struct{})
	hostedRequestCancelled := make(chan struct{})
	releaseHostedResponse := make(chan struct{})
	var releaseOnce sync.Once
	releaseResponse := func() { releaseOnce.Do(func() { close(releaseHostedResponse) }) }
	backendClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, err := ioReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		var decoded map[string]any
		if err := json.Unmarshal(body, &decoded); err != nil {
			return nil, err
		}
		if _, continuing := decoded["previous_response_id"]; !continuing {
			return jsonHTTPResponseWithSession(
				r,
				functionCallResponse("support-ticket-lookup"),
				"existing-session",
			)
		}
		close(continuationReceived)
		<-r.Context().Done()
		close(hostedRequestCancelled)
		// Model a hosted continuation that ignores client cancellation and
		// eventually reports success with a replacement session identifier.
		<-releaseHostedResponse
		return jsonHTTPResponseWithSession(r, finalResponsesMessage(), "session-after-cancel")
	})}
	endpoint := "https://foundry.example/agents/test-agent/endpoint/protocols/openai/responses?api-version=v1"
	adapter, server := newTestResponsesAdapterWithHTTPClient(
		t,
		endpoint,
		[]harness.BrokeredToolClass{harness.BrokeredToolClassRead},
		backendClient,
	)
	t.Cleanup(releaseResponse)
	client := newHarnessClient(t, adapter)
	request := brokeredReadRequest("foundry-cancel-continuation")
	server.mu.Lock()
	server.runtimeSessions[request.RuntimeSessionID] = foundrySession{
		ID:       "existing-session",
		LastSeen: time.Now().UTC(),
	}
	server.mu.Unlock()
	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	frames := streamCurrentFrames(t, client, request.TurnID)
	requested := findFrame(frames, harness.FrameToolCallRequested)
	if requested == nil {
		t.Fatalf("frames = %#v, want tool request", frames)
	}
	server.mu.Lock()
	publishedBeforeCompletion := server.runtimeSessions[request.RuntimeSessionID]
	server.mu.Unlock()
	if publishedBeforeCompletion.ID != "existing-session" {
		t.Fatalf("runtime session before continuation = %#v, want existing session", publishedBeforeCompletion)
	}

	continueRequest := goldenContinueRequest(request, requested.ToolCallID, json.RawMessage(`{"success":true}`))
	continueErr := make(chan error, 1)
	go func() {
		_, err := client.ContinueTurn(context.Background(), continueRequest)
		continueErr <- err
	}()
	select {
	case <-continuationReceived:
	case <-time.After(time.Second):
		t.Fatal("hosted continuation did not arrive")
	}
	if _, err := client.CancelTurn(context.Background(), cancelRequestForStart(request)); err != nil {
		t.Fatalf("CancelTurn during continuation: %v", err)
	}
	select {
	case <-hostedRequestCancelled:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not cancel the hosted continuation request context")
	}
	server.mu.Lock()
	_, publishedAfterCancellation := server.runtimeSessions[request.RuntimeSessionID]
	_, sessionQuarantined := server.quarantinedSessions[request.RuntimeSessionID]
	server.mu.Unlock()
	if publishedAfterCancellation || !sessionQuarantined {
		t.Fatalf(
			"runtime session after cancellation published=%v quarantined=%v",
			publishedAfterCancellation,
			sessionQuarantined,
		)
	}
	releaseResponse()
	select {
	case err := <-continueErr:
		if err == nil || !strings.Contains(err.Error(), "turn completed while hosted continuation was in flight") {
			t.Fatalf("ContinueTurn after cancellation error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ContinueTurn did not finish after the late hosted response")
	}

	quarantined := responsesStartTurnRequest("foundry-quarantined-after-cancel-continuation")
	quarantined.RuntimeSessionID = request.RuntimeSessionID
	if _, err := client.StartTurn(context.Background(), quarantined); err == nil ||
		!strings.Contains(err.Error(), "runtime session unavailable after unconfirmed hosted cancellation") {
		t.Fatalf("StartTurn with quarantined runtime session error = %v", err)
	}
	second := responsesStartTurnRequest("foundry-after-cancel-continuation")
	if _, err := client.StartTurn(context.Background(), second); err != nil {
		t.Fatalf("StartTurn with a different runtime session after cancellation: %v", err)
	}
	frames = streamCurrentFrames(t, client, request.TurnID)
	if !hasFrameType(frames, harness.FrameTurnCancelled) || hasFrameType(frames, harness.FrameTurnCompleted) {
		t.Fatalf("frames = %#v, want cancellation to win continuation race", frames)
	}
	server.mu.Lock()
	_, publishedAfterLateResponse := server.runtimeSessions[request.RuntimeSessionID]
	server.mu.Unlock()
	if publishedAfterLateResponse {
		t.Fatal("late continuation response restored the cancelled runtime session")
	}
}

func TestResponsesAdapterContinuationHonorsOriginalTurnDeadline(t *testing.T) {
	foundry := newFakeResponses(t, fakeResponsesConfig{scenario: "function_call", toolName: "support-ticket-lookup"})
	adapter := newTestResponsesAdapter(
		t,
		foundry.endpoint(),
		[]harness.BrokeredToolClass{harness.BrokeredToolClassRead},
	)
	client := newHarnessClient(t, adapter)
	request := brokeredReadRequest("foundry-continuation-deadline")
	request.Deadline = time.Now().Add(300 * time.Millisecond)

	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	frames := streamCurrentFrames(t, client, request.TurnID)
	requested := findFrame(frames, harness.FrameToolCallRequested)
	if requested == nil {
		t.Fatalf("frames = %#v, want tool request", frames)
	}
	if wait := time.Until(request.Deadline); wait > 0 {
		time.Sleep(wait + 10*time.Millisecond)
	}
	continueRequest := goldenContinueRequest(request, requested.ToolCallID, json.RawMessage(`{"success":true}`))
	if _, err := client.ContinueTurn(context.Background(), continueRequest); err == nil {
		t.Fatal("ContinueTurn after original deadline succeeded, want failure")
	}
	if got := foundry.postCount.Load(); got != 1 {
		t.Fatalf("hosted post count = %d, want no continuation after turn deadline", got)
	}
	frames = streamCurrentFrames(t, client, request.TurnID)
	failed := findFrame(frames, harness.FrameTurnFailed)
	if failed == nil || failed.Failed == nil || failed.Failed.Reason != "foundry_continuation_unknown" {
		t.Fatalf("failed frame = %#v, want foundry_continuation_unknown", failed)
	}
}

func TestResponsesAdapterRejectedContinueDoesNotBufferPartialResults(t *testing.T) {
	server := newServer(config{
		runtimeName:         "test",
		adapterBearer:       "adapter-auth-value",
		endpoint:            "http://127.0.0.1/agents/test-agent/endpoint/protocols/openai/responses?api-version=v1",
		foundryAuth:         "foundry-auth-value",
		requestTimeout:      time.Second,
		stateRetention:      time.Minute,
		brokeredToolClasses: []harness.BrokeredToolClass{harness.BrokeredToolClassRead},
	}, &http.Client{Timeout: time.Second})
	request := brokeredReadRequest("foundry-partial-reject")
	valid := toolResultForRequest(request, "call-1", true, json.RawMessage(`{"success":true}`), nil)
	unknown := toolResultForRequest(request, "call-missing", true, json.RawMessage(`{"success":true}`), nil)
	turn := &turnState{
		request:          request,
		pendingTools:     map[string]string{"call-1": "support-ticket-lookup"},
		bufferedResults:  map[string]harness.ToolCallResult{},
		bufferedDigests:  map[string]toolResultDigest{},
		submittedDigests: map[string]toolResultDigest{},
	}
	if _, err := server.recordContinueResults(turn, []harness.ToolCallResult{valid, unknown}); err == nil {
		t.Fatalf("recordContinueResults succeeded, want unknown tool result error")
	}
	if len(turn.bufferedResults) != 0 || len(turn.bufferedDigests) != 0 {
		t.Fatalf("buffered state = %#v/%#v, want no partial buffering", turn.bufferedResults, turn.bufferedDigests)
	}
}

func TestResponsesAdapterAlreadySubmittedContinueDoesNotResubmit(t *testing.T) {
	server := newServer(config{
		runtimeName:         "test",
		adapterBearer:       "adapter-auth-value",
		endpoint:            "http://127.0.0.1/agents/test-agent/endpoint/protocols/openai/responses?api-version=v1",
		foundryAuth:         "foundry-auth-value",
		requestTimeout:      time.Second,
		stateRetention:      time.Minute,
		brokeredToolClasses: []harness.BrokeredToolClass{harness.BrokeredToolClassRead},
	}, &http.Client{Timeout: time.Second})
	request := brokeredReadRequest("foundry-already-submitted")
	result := toolResultForRequest(request, "call-1", true, json.RawMessage(`{"success":true}`), nil)
	payload, err := canonicalToolResultOutput(result)
	if err != nil {
		t.Fatalf("canonicalToolResultOutput: %v", err)
	}
	turn := &turnState{
		request:          request,
		pendingTools:     map[string]string{"call-1": "support-ticket-lookup"},
		bufferedResults:  map[string]harness.ToolCallResult{"call-1": result},
		bufferedDigests:  map[string]toolResultDigest{"call-1": digestToolResultPayload(payload)},
		submittedDigests: map[string]toolResultDigest{"call-1": digestToolResultPayload(payload)},
	}
	toSubmit, err := server.recordContinueResults(turn, []harness.ToolCallResult{result})
	if err != nil {
		t.Fatalf("recordContinueResults: %v", err)
	}
	if toSubmit != nil {
		t.Fatalf("toSubmit = %#v, want nil for already submitted duplicate", toSubmit)
	}
}

func TestResponsesAdapterContinuesToolExecutionFailurePayload(t *testing.T) {
	foundry := newFakeResponses(t, fakeResponsesConfig{scenario: "function_call", toolName: "support-ticket-lookup"})
	adapter := newTestResponsesAdapter(
		t,
		foundry.endpoint(),
		[]harness.BrokeredToolClass{harness.BrokeredToolClassRead},
	)
	client := newHarnessClient(t, adapter)
	request := brokeredReadRequest("foundry-tool-failure")

	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	frames := streamCurrentFrames(t, client, request.TurnID)
	requested := findFrame(frames, harness.FrameToolCallRequested)
	if requested == nil {
		t.Fatalf("frames = %#v, want tool request", frames)
	}
	failure := harness.ContinueTurnRequest{
		Version:          harness.ProtocolVersion,
		Namespace:        request.Namespace,
		TaskName:         request.TaskName,
		SessionName:      request.SessionName,
		RuntimeSessionID: request.RuntimeSessionID,
		TurnID:           request.TurnID,
		CorrelationID:    request.CorrelationID,
		ToolResults: []harness.ToolCallResult{toolResultForRequest(
			request,
			requested.ToolCallID,
			true,
			nil,
			&harness.ErrorInfo{Code: "tool_execution_failed", Message: "downstream failed"},
		)},
	}
	if _, err := client.ContinueTurn(context.Background(), failure); err != nil {
		t.Fatalf("ContinueTurn tool failure: %v", err)
	}
	continuation := requestMap(t, foundry.requestBody(1))
	items, ok := continuation["input"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("continuation input = %#v, want one item", continuation["input"])
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("continuation item = %#v, want object", items[0])
	}
	wantOutput := `{"approved":false,"error":{"code":"tool_execution_failed","message":"downstream failed"}}`
	if got := item["output"]; got != wantOutput {
		t.Fatalf("failure output = %#v, want %s", got, wantOutput)
	}
	frames = streamCurrentFrames(t, client, request.TurnID)
	toolResult := findFrame(frames, harness.FrameToolResultReceived)
	if toolResult == nil || toolResult.Error == nil || toolResult.Error.Code != "tool_execution_failed" {
		t.Fatalf("tool result frame = %#v, want tool_execution_failed", toolResult)
	}
	if !hasFrameType(frames, harness.FrameTurnCompleted) {
		t.Fatalf("frames = %#v, want final completion after tool failure continuation", frames)
	}
}

func TestResponsesAdapterContinuationFailureFailsClosedWithoutDuplicatePost(t *testing.T) {
	foundry := newFakeResponses(t, fakeResponsesConfig{
		scenario:           "function_call",
		toolName:           "support-ticket-lookup",
		continuationStatus: http.StatusInternalServerError,
	})
	adapter, server := newTestResponsesAdapterWithServer(
		t,
		foundry.endpoint(),
		[]harness.BrokeredToolClass{harness.BrokeredToolClassRead},
	)
	client := newHarnessClient(t, adapter)
	request := brokeredReadRequest("foundry-continuation-failure")

	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	frames := streamCurrentFrames(t, client, request.TurnID)
	requested := findFrame(frames, harness.FrameToolCallRequested)
	if requested == nil {
		t.Fatalf("frames = %#v, want tool request", frames)
	}
	continueRequest := goldenContinueRequest(request, requested.ToolCallID, json.RawMessage(`{"success":true}`))
	_, continueErr := client.ContinueTurn(context.Background(), continueRequest)
	if continueErr == nil {
		t.Fatalf("ContinueTurn succeeded, want hosted continuation failure")
	}
	if strings.Contains(continueErr.Error(), foundry.URL) || strings.Contains(continueErr.Error(), "HTTP 500") {
		t.Fatalf("ContinueTurn error leaked upstream detail: %v", continueErr)
	}
	if foundry.postCount.Load() != 2 {
		t.Fatalf("hosted post count after failed continue = %d, want 2", foundry.postCount.Load())
	}
	if _, err := client.ContinueTurn(context.Background(), continueRequest); err != nil {
		t.Fatalf("duplicate ContinueTurn after fail-closed terminal state: %v", err)
	}
	if foundry.postCount.Load() != 2 {
		t.Fatalf("hosted post count after duplicate = %d, want no second continuation", foundry.postCount.Load())
	}
	frames = streamCurrentFrames(t, client, request.TurnID)
	failed := findFrame(frames, harness.FrameTurnFailed)
	if failed == nil || failed.Failed.Reason != "foundry_continuation_unknown" {
		t.Fatalf("failed frame = %#v, want fail-closed continuation failure", failed)
	}
	if strings.Contains(failed.Failed.Message, foundry.URL) ||
		strings.Contains(failed.Failed.Message, "HTTP 500") {
		t.Fatalf("failed frame leaked upstream detail: %#v", failed.Failed)
	}
	server.mu.Lock()
	_, quarantined := server.quarantinedSessions[request.RuntimeSessionID]
	server.mu.Unlock()
	if !quarantined {
		t.Fatal("uncertain continuation failure did not quarantine its runtime session")
	}
	retry := responsesStartTurnRequest("foundry-continuation-failure-retry")
	retry.RuntimeSessionID = request.RuntimeSessionID
	if _, err := client.StartTurn(context.Background(), retry); err == nil ||
		!strings.Contains(err.Error(), "runtime session unavailable after unconfirmed hosted cancellation") {
		t.Fatalf("StartTurn with continuation-quarantined runtime session error = %v", err)
	}
}

func TestResponsesAdapterHostedErrorDoesNotExposeUpstreamDiagnostics(t *testing.T) {
	server := newServer(config{}, &http.Client{Timeout: time.Second})
	request := responsesStartTurnRequest("foundry-safe-hosted-error")
	turn := &turnState{request: request, frameUpdates: make(chan struct{})}
	server.appendFrameLocked(turn, harness.FrameTurnStarted, "foundry hosted response started")
	secret := "proof-that-must-not-reach-task-events"
	server.handleResponsesResponse(turn, responsesResponse{
		ID:     "resp-error",
		Status: "failed",
		Error: &responsesError{
			Code:    secret,
			Message: "upstream echoed brokered_continuation_proof=" + secret,
		},
	})
	failed := findFrame(turn.frames, harness.FrameTurnFailed)
	if failed == nil || failed.Failed == nil || failed.Error == nil {
		t.Fatalf("failed frame = %#v, want safe terminal failure", failed)
	}
	if failed.Failed.Reason != "foundry_response_error" ||
		failed.Failed.Message != "Foundry hosted Responses returned an error" {
		t.Fatalf("failed frame = %#v, want fixed safe upstream error", failed)
	}
	payload, err := json.Marshal(failed)
	if err != nil {
		t.Fatalf("marshal failed frame: %v", err)
	}
	if strings.Contains(string(payload), secret) {
		t.Fatalf("failed frame leaked upstream diagnostics: %s", payload)
	}
}

func TestResponsesRepeatedSubmittedFunctionCallFailsTurn(t *testing.T) {
	server := newServer(config{
		runtimeName:         "test",
		adapterBearer:       "adapter-auth-value",
		endpoint:            "http://127.0.0.1/agents/test-agent/endpoint/protocols/openai/responses?api-version=v1",
		foundryAuth:         "foundry-auth-value",
		requestTimeout:      time.Second,
		stateRetention:      time.Minute,
		brokeredToolClasses: []harness.BrokeredToolClass{harness.BrokeredToolClassRead},
	}, &http.Client{Timeout: time.Second})
	request := brokeredReadRequest("foundry-repeated-call")
	turn := &turnState{
		request:          request,
		pendingTools:     map[string]string{},
		bufferedResults:  map[string]harness.ToolCallResult{},
		bufferedDigests:  map[string]toolResultDigest{},
		submittedDigests: map[string]toolResultDigest{"call-1": digestToolResultPayload(`{"approved":true}`)},
	}
	server.appendFrameLocked(turn, harness.FrameTurnStarted, "foundry hosted response started")
	server.handleResponsesResponse(turn, responsesResponse{
		ID:     "resp-repeat",
		Status: "completed",
		Output: []responsesOutput{{
			Type:      "function_call",
			CallID:    "call-1",
			Name:      "support-ticket-lookup",
			Arguments: json.RawMessage(`{"incident":"inc-1"}`),
		}},
	})
	failed := findFrame(turn.frames, harness.FrameTurnFailed)
	if failed == nil || failed.Failed.Reason != "foundry_repeated_function_call" {
		t.Fatalf("failed frame = %#v, want foundry_repeated_function_call", failed)
	}
}

func TestResponsesMixedRepeatedFunctionCallFailsTurn(t *testing.T) {
	server := newServer(config{
		runtimeName:         "test",
		adapterBearer:       "adapter-auth-value",
		endpoint:            "http://127.0.0.1/agents/test-agent/endpoint/protocols/openai/responses?api-version=v1",
		foundryAuth:         "foundry-auth-value",
		requestTimeout:      time.Second,
		stateRetention:      time.Minute,
		brokeredToolClasses: []harness.BrokeredToolClass{harness.BrokeredToolClassRead},
	}, &http.Client{Timeout: time.Second})
	request := brokeredReadRequest("foundry-mixed-repeated-call")
	turn := &turnState{
		request:          request,
		pendingTools:     map[string]string{"call-1": "support-ticket-lookup"},
		bufferedResults:  map[string]harness.ToolCallResult{},
		bufferedDigests:  map[string]toolResultDigest{},
		submittedDigests: map[string]toolResultDigest{},
	}
	server.appendFrameLocked(turn, harness.FrameTurnStarted, "foundry hosted response started")
	server.handleResponsesResponse(turn, responsesResponse{
		ID:     "resp-repeat",
		Status: "completed",
		Output: []responsesOutput{
			{
				Type:      "function_call",
				CallID:    "call-2",
				Name:      "support-ticket-lookup",
				Arguments: json.RawMessage(`{"incident":"inc-2"}`),
			},
			{
				Type:      "function_call",
				CallID:    "call-1",
				Name:      "support-ticket-lookup",
				Arguments: json.RawMessage(`{"incident":"inc-1"}`),
			},
		},
	})
	failed := findFrame(turn.frames, harness.FrameTurnFailed)
	if failed == nil || failed.Failed.Reason != "foundry_repeated_function_call" {
		t.Fatalf("failed frame = %#v, want foundry_repeated_function_call", failed)
	}
	if _, exists := turn.pendingTools["call-2"]; exists {
		t.Fatal("new call was accepted after repeated pending call")
	}
}

func TestResponsesRecordContinueResultsRejectsTerminalTurn(t *testing.T) {
	server := newServer(config{}, &http.Client{Timeout: time.Second})
	request := brokeredReadRequest("foundry-terminal-record")
	turn := &turnState{
		request:          request,
		pendingTools:     map[string]string{"call-1": "support-ticket-lookup"},
		bufferedResults:  map[string]harness.ToolCallResult{},
		bufferedDigests:  map[string]toolResultDigest{},
		submittedDigests: map[string]toolResultDigest{},
		completed:        true,
	}
	result := toolResultForRequest(request, "call-1", true, json.RawMessage(`{"success":true}`), nil)
	if _, err := server.recordContinueResults(turn, []harness.ToolCallResult{result}); err == nil ||
		!strings.Contains(err.Error(), "already terminal") {
		t.Fatalf("recordContinueResults terminal error = %v", err)
	}
	if len(turn.submittedDigests) != 0 || len(turn.bufferedResults) != 0 {
		t.Fatalf("terminal result mutation = %#v/%#v", turn.submittedDigests, turn.bufferedResults)
	}
}

func TestResponsesAdapterAlreadySubmittedPendingResultIsNoop(t *testing.T) {
	server := newServer(config{}, &http.Client{Timeout: time.Second})
	request := brokeredReadRequest("foundry-submitted-noop")
	turn := &turnState{
		request:          request,
		pendingTools:     map[string]string{"call-1": "support-ticket-lookup"},
		bufferedResults:  map[string]harness.ToolCallResult{},
		bufferedDigests:  map[string]toolResultDigest{},
		submittedDigests: map[string]toolResultDigest{},
	}
	result := toolResultForRequest(request, "call-1", true, json.RawMessage(`{"success":true}`), nil)
	payload, err := canonicalToolResultOutput(result)
	if err != nil {
		t.Fatalf("canonicalToolResultOutput: %v", err)
	}
	turn.submittedDigests["call-1"] = digestToolResultPayload(payload)

	toSubmit, err := server.recordContinueResults(turn, []harness.ToolCallResult{result})
	if err != nil {
		t.Fatalf("recordContinueResults: %v", err)
	}
	if len(toSubmit) != 0 {
		t.Fatalf("toSubmit = %#v, want duplicate submitted result to be a no-op", toSubmit)
	}
}

func TestResponsesAdapterBrokeredMaxTurnIsUnknown(t *testing.T) {
	foundry := newFakeResponses(t, fakeResponsesConfig{scenario: "observed"})
	s := newServer(config{
		runtimeName:         "foundry-responses-test",
		adapterBearer:       "adapter-auth-value",
		endpoint:            foundry.endpoint(),
		foundryAuth:         "foundry-auth-value",
		requestTimeout:      2 * time.Second,
		stateRetention:      time.Minute,
		continuationProof:   testContinuationProof,
		brokeredToolClasses: []harness.BrokeredToolClass{harness.BrokeredToolClassRead},
	}, &http.Client{Timeout: time.Second})
	adapter := httptest.NewServer(s.handler())
	t.Cleanup(adapter.Close)
	client := newHarnessClient(t, adapter)

	caps, err := client.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if caps.MaxTurnSeconds != 0 {
		t.Fatalf("MaxTurnSeconds = %d, want unknown ceiling for brokered turns", caps.MaxTurnSeconds)
	}
}

func TestResponsesAdapterStateLossContinueFailsSafely(t *testing.T) {
	foundry := newFakeResponses(t, fakeResponsesConfig{scenario: "function_call", toolName: "support-ticket-lookup"})
	adapter := newTestResponsesAdapter(
		t,
		foundry.endpoint(),
		[]harness.BrokeredToolClass{harness.BrokeredToolClassRead},
	)
	client := newHarnessClient(t, adapter)
	request := brokeredReadRequest("foundry-state-loss")
	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	frames := streamCurrentFrames(t, client, request.TurnID)
	requested := findFrame(frames, harness.FrameToolCallRequested)
	if requested == nil {
		t.Fatalf("frames = %#v, want tool request", frames)
	}

	restarted := newTestResponsesAdapter(
		t,
		foundry.endpoint(),
		[]harness.BrokeredToolClass{harness.BrokeredToolClassRead},
	)
	restartedClient, err := harness.NewClient(
		restarted.URL,
		harness.WithBearerToken("adapter-auth-value"),
		harness.WithControlTimeout(2*time.Second),
	)
	if err != nil {
		t.Fatalf("NewClient restarted: %v", err)
	}
	_, err = restartedClient.ContinueTurn(
		context.Background(),
		goldenContinueRequest(request, requested.ToolCallID, json.RawMessage(`{"success":true}`)),
	)
	if err == nil || !strings.Contains(err.Error(), "410") {
		t.Fatalf("restart continue error = %v, want non-retryable gone state", err)
	}
	if foundry.postCount.Load() != 1 {
		t.Fatalf(
			"hosted post count after state-loss continue = %d, want no duplicate continuation",
			foundry.postCount.Load(),
		)
	}
}

func TestPostResponsesRejectsOversizedAndTrailingBodies(t *testing.T) {
	valid, err := json.Marshal(finalResponsesMessage())
	if err != nil {
		t.Fatalf("marshal valid response: %v", err)
	}
	tests := []struct {
		name    string
		body    []byte
		wantErr string
	}{
		{
			name:    "oversized",
			body:    append(append([]byte(nil), valid...), bytes.Repeat([]byte(" "), maxFoundryBodyBytes+1-len(valid))...),
			wantErr: "exceeded",
		},
		{
			name:    "trailing data",
			body:    append(append([]byte(nil), valid...), []byte("trailing")...),
			wantErr: "decode",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(tt.body)
			}))
			t.Cleanup(upstream.Close)
			server := newServer(config{
				endpoint:       upstream.URL + "/agents/test/endpoint/protocols/openai/responses?api-version=v1",
				foundryAuth:    "foundry-auth-value",
				requestTimeout: time.Second,
			}, &http.Client{Timeout: time.Second})
			var response responsesResponse
			_, err := server.postResponses(
				context.Background(),
				responsesRequest{Input: "test"},
				&response,
			)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("postResponses error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestResponsesMessageTextPreservesRefusal(t *testing.T) {
	got := responsesMessageText([]responsesOutput{{
		Type: "message",
		Content: []any{map[string]any{
			"type":    "refusal",
			"refusal": "I cannot perform that request.",
		}},
	}})
	if got != "I cannot perform that request." {
		t.Fatalf("responsesMessageText refusal = %q", got)
	}
}

func TestResponsesAPIVersionDefaultsToSDKValue(t *testing.T) {
	t.Setenv(envAPIVersion, "")
	if got := loadConfig().apiVersion; got != defaultAPIVersion {
		t.Fatalf("loadConfig apiVersion = %q, want %q", got, defaultAPIVersion)
	}
	t.Setenv(envAPIVersion, "2025-11-15-preview")
	if got := loadConfig().apiVersion; got != "2025-11-15-preview" {
		t.Fatalf("loadConfig apiVersion override = %q", got)
	}
}

func TestResponsesAdapterRejectsConsumedTurnAfterCleanup(t *testing.T) {
	foundry := newFakeResponses(t, fakeResponsesConfig{scenario: "platform_error"})
	server := newServer(config{
		runtimeName:    "foundry-responses-test",
		adapterBearer:  "adapter-auth-value",
		endpoint:       foundry.endpoint(),
		foundryAuth:    "foundry-auth-value",
		requestTimeout: time.Second,
		stateRetention: 50 * time.Millisecond,
	}, &http.Client{Timeout: time.Second})
	adapter := httptest.NewServer(server.handler())
	t.Cleanup(adapter.Close)
	client := newHarnessClient(t, adapter)
	request := responsesStartTurnRequest("foundry-consumed-turn")

	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	server.mu.Lock()
	_, retainedBeforeCleanup := server.turns[request.TurnID]
	_, consumedBeforeCleanup := server.consumedTurns[request.TurnID]
	server.mu.Unlock()
	if !retainedBeforeCleanup || consumedBeforeCleanup {
		t.Fatalf(
			"pre-cleanup state retained=%v consumed=%v, want retained state to own admission",
			retainedBeforeCleanup,
			consumedBeforeCleanup,
		)
	}
	deadline := time.Now().Add(time.Second)
	for {
		server.mu.Lock()
		_, retained := server.turns[request.TurnID]
		_, consumed := server.consumedTurns[request.TurnID]
		server.mu.Unlock()
		if !retained && consumed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("terminal turn was not evicted into the consumed-turn tombstone")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := client.StreamFrames(context.Background(), request.TurnID, 0, func(harness.HarnessEventFrame) error {
		return nil
	}); err == nil || !strings.Contains(err.Error(), "410") {
		t.Fatalf("expired terminal stream error = %v, want non-retryable 410", err)
	}
	if _, err := client.StartTurn(context.Background(), request); err == nil ||
		!strings.Contains(err.Error(), "turn already completed") {
		t.Fatalf("retry after cleanup error = %v, want consumed-turn conflict", err)
	}
	if got := foundry.postCount.Load(); got != 1 {
		t.Fatalf("hosted post count after consumed retry = %d, want 1", got)
	}
}

func TestResponsesConsumedTurnTombstonesAreBounded(t *testing.T) {
	server := newServer(config{}, nil)
	server.mu.Lock()
	for i := 0; i <= maxConsumedTurnIDs; i++ {
		server.markTurnConsumedLocked(harness.HarnessTurnID(fmt.Sprintf("turn-%d", i)))
	}
	_, oldestRetained := server.consumedTurns["turn-0"]
	_, newestRetained := server.consumedTurns[harness.HarnessTurnID(fmt.Sprintf("turn-%d", maxConsumedTurnIDs))]
	count := len(server.consumedTurns)
	server.mu.Unlock()
	if count != maxConsumedTurnIDs || oldestRetained || !newestRetained {
		t.Fatalf(
			"consumed tombstone count=%d oldest=%v newest=%v, want bounded FIFO",
			count,
			oldestRetained,
			newestRetained,
		)
	}
}

func TestResponsesAdapterValidatesCancelIdentityBeforeMutation(t *testing.T) {
	foundry := newFakeResponses(t, fakeResponsesConfig{scenario: "function_call", toolName: "support-ticket-lookup"})
	adapter, server := newTestResponsesAdapterWithServer(
		t,
		foundry.endpoint(),
		[]harness.BrokeredToolClass{harness.BrokeredToolClassRead},
	)
	client := newHarnessClient(t, adapter)
	request := brokeredReadRequest("foundry-cancel-identity")
	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}

	cancelPath, err := harness.CancelTurnPath(request.TurnID)
	if err != nil {
		t.Fatalf("CancelTurnPath: %v", err)
	}
	malformed, err := http.NewRequest(http.MethodPost, adapter.URL+cancelPath, strings.NewReader("{"))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	malformed.Header.Set("Authorization", "Bearer adapter-auth-value")
	response, err := http.DefaultClient.Do(malformed)
	if err != nil {
		t.Fatalf("malformed cancel request: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed cancel status = %d, want 400", response.StatusCode)
	}

	mismatched := cancelRequestForStart(request)
	mismatched.CorrelationID = "other-correlation"
	if _, err := client.CancelTurn(context.Background(), mismatched); err == nil ||
		!strings.Contains(err.Error(), "does not match started turn") {
		t.Fatalf("mismatched cancel error = %v, want identity rejection", err)
	}
	server.mu.Lock()
	turn := server.turns[request.TurnID]
	completedAfterReject := turn == nil || turn.completed
	server.mu.Unlock()
	if completedAfterReject {
		t.Fatal("malformed or mismatched cancel mutated the active turn")
	}

	if _, err := client.CancelTurn(context.Background(), cancelRequestForStart(request)); err != nil {
		t.Fatalf("valid CancelTurn: %v", err)
	}
	frames := streamCurrentFrames(t, client, request.TurnID)
	if !hasFrameType(frames, harness.FrameTurnCancelled) {
		t.Fatalf("frames = %#v, want TurnCancelled", frames)
	}
}

func TestResponsesEndpointComposesProjectAndAgent(t *testing.T) {
	server := newServer(config{
		projectEndpoint: "https://example.services.ai.azure.com/api/projects/project-a/",
		agentName:       "agent/name",
		apiVersion:      "v1",
	}, nil)
	endpoint, err := server.responsesEndpoint()
	if err != nil {
		t.Fatalf("responsesEndpoint: %v", err)
	}
	want := "https://example.services.ai.azure.com/api/projects/project-a/agents/" +
		"agent%2Fname/endpoint/protocols/openai/responses?api-version=v1"
	if endpoint != want {
		t.Fatalf("responsesEndpoint = %q, want %q", endpoint, want)
	}
}

func TestResponsesEndpointSafety(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		want     bool
	}{
		{
			name:     "https responses without version",
			endpoint: "https://example.openai.azure.com/agents/a/endpoint/protocols/openai/responses",
			want:     true,
		},
		{
			name:     "https responses with explicit version",
			endpoint: "https://example.openai.azure.com/agents/a/endpoint/protocols/openai/responses?api-version=v1",
			want:     true,
		},
		{
			name:     "loopback http",
			endpoint: "http://127.0.0.1:8080/agents/a/endpoint/protocols/openai/responses?api-version=v1",
			want:     true,
		},
		{
			name:     "empty hostname",
			endpoint: "https://:443/agents/a/endpoint/protocols/openai/responses?api-version=v1",
			want:     false,
		},
		{
			name:     "http non-loopback",
			endpoint: "http://example.com/agents/a/endpoint/protocols/openai/responses?api-version=v1",
			want:     false,
		},
		{
			name:     "userinfo",
			endpoint: "https://user:pass@example.com/agents/a/endpoint/protocols/openai/responses?api-version=v1",
			want:     false,
		},
		{
			name:     "fragment",
			endpoint: "https://example.com/agents/a/endpoint/protocols/openai/responses?api-version=v1#fragment",
			want:     false,
		},
		{
			name:     "secret query",
			endpoint: "https://example.com/agents/a/endpoint/protocols/openai/responses?api-version=v1&unsafe=x",
			want:     false,
		},
		{name: "not responses", endpoint: "https://example.com/threads", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := responsesEndpointIsSafe(tt.endpoint); got != tt.want {
				t.Fatalf("responsesEndpointIsSafe(%q) = %v, want %v", tt.endpoint, got, tt.want)
			}
		})
	}
}

func TestProjectEndpointSafety(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		want     bool
	}{
		{name: "https", endpoint: "https://example.services.ai.azure.com", want: true},
		{name: "empty hostname", endpoint: "https://:443", want: false},
		{name: "query", endpoint: "https://example.services.ai.azure.com?unsafe=x", want: false},
		{name: "loopback", endpoint: "http://127.0.0.1:8080", want: true},
		{name: "non-loopback http", endpoint: "http://example.com", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := projectEndpointIsSafe(tt.endpoint); got != tt.want {
				t.Fatalf("projectEndpointIsSafe(%q) = %v, want %v", tt.endpoint, got, tt.want)
			}
		})
	}
}

func TestResponsesRejectsOversizedHostedFunctionCallBatch(t *testing.T) {
	server := newServer(config{
		brokeredToolClasses: []harness.BrokeredToolClass{harness.BrokeredToolClassRead},
	}, nil)
	turn := &turnState{
		request:          brokeredReadRequest("foundry-tool-call-cap"),
		pendingTools:     map[string]string{},
		bufferedResults:  map[string]harness.ToolCallResult{},
		bufferedDigests:  map[string]toolResultDigest{},
		submittedDigests: map[string]toolResultDigest{},
	}
	server.appendFrameLocked(turn, harness.FrameTurnStarted, "foundry hosted response started")
	output := make([]responsesOutput, 0, maxBrokeredToolCalls+1)
	for i := 0; i <= maxBrokeredToolCalls; i++ {
		output = append(output, responsesOutput{
			Type:      "function_call",
			CallID:    fmt.Sprintf("call-%d", i),
			Name:      "support-ticket-lookup",
			Arguments: json.RawMessage(`{"incident":"inc-1"}`),
		})
	}
	server.handleResponsesResponse(turn, responsesResponse{ID: "resp-cap", Status: "completed", Output: output})
	failed := findFrame(turn.frames, harness.FrameTurnFailed)
	if failed == nil || failed.Failed.Reason != "foundry_tool_call_limit_exceeded" {
		t.Fatalf("failed frame = %#v, want foundry_tool_call_limit_exceeded", failed)
	}
	if hasFrameType(turn.frames, harness.FrameToolCallRequested) || turn.requestedToolCalls != 0 {
		t.Fatalf("oversized hosted batch was partially accepted: frames=%#v count=%d", turn.frames, turn.requestedToolCalls)
	}
}

func TestResponsesParserConsumesGoldenFixtures(t *testing.T) {
	server := newServer(
		config{
			runtimeName:         "test",
			adapterBearer:       "adapter-auth-value",
			endpoint:            "http://127.0.0.1/agents/test-agent/endpoint/protocols/openai/responses?api-version=v1",
			foundryAuth:         "foundry-auth-value",
			requestTimeout:      time.Second,
			stateRetention:      time.Minute,
			brokeredToolClasses: []harness.BrokeredToolClass{harness.BrokeredToolClassRead},
		},
		&http.Client{Timeout: time.Second},
	)
	request := brokeredReadRequest("foundry-brokered")
	turn := &turnState{
		request:          request,
		pendingTools:     map[string]string{},
		bufferedResults:  map[string]harness.ToolCallResult{},
		bufferedDigests:  map[string]toolResultDigest{},
		submittedDigests: map[string]toolResultDigest{},
	}
	server.appendFrameLocked(turn, harness.FrameTurnStarted, "foundry hosted response started")
	var functionCall responsesResponse
	decodeFixtureInto(t, "testdata/golden/02_function_call_response.json", &functionCall)
	server.handleResponsesResponse(turn, functionCall)
	requested := findFrame(turn.frames, harness.FrameToolCallRequested)
	if requested == nil {
		t.Fatalf("frames = %#v, want tool request from fixture", turn.frames)
	}
	assertJSONFileEqual(t, "testdata/golden/03_tool_call_requested_frame.json", scrubFrameForGolden(*requested))

	finalTurn := &turnState{
		request:          responsesStartTurnRequest("foundry-final"),
		pendingTools:     map[string]string{},
		bufferedResults:  map[string]harness.ToolCallResult{},
		bufferedDigests:  map[string]toolResultDigest{},
		submittedDigests: map[string]toolResultDigest{},
	}
	server.appendFrameLocked(finalTurn, harness.FrameTurnStarted, "foundry hosted response started")
	var finalMessage responsesResponse
	decodeFixtureInto(t, "testdata/golden/06_final_message_response.json", &finalMessage)
	server.handleResponsesResponse(finalTurn, finalMessage)
	completed := findFrame(finalTurn.frames, harness.FrameTurnCompleted)
	if completed == nil || completed.Completed.Result != "foundry final answer" {
		t.Fatalf("completed frame = %#v, want final answer", completed)
	}

	multipleTurn := &turnState{
		request:          request,
		pendingTools:     map[string]string{},
		bufferedResults:  map[string]harness.ToolCallResult{},
		bufferedDigests:  map[string]toolResultDigest{},
		submittedDigests: map[string]toolResultDigest{},
	}
	server.appendFrameLocked(multipleTurn, harness.FrameTurnStarted, "foundry hosted response started")
	var multiple responsesResponse
	decodeFixtureInto(t, "testdata/golden/10_multiple_calls_response.json", &multiple)
	server.handleResponsesResponse(multipleTurn, multiple)
	requests := findFrames(multipleTurn.frames, harness.FrameToolCallRequested)
	if len(requests) != 2 {
		t.Fatalf("frames = %#v, want two tool requests from multiple-call fixture", multipleTurn.frames)
	}
}

func TestResponsesConsumesAgentKitBrokeredFixtures(t *testing.T) {
	server := newServer(
		config{
			runtimeName:         "test",
			adapterBearer:       "adapter-auth-value",
			endpoint:            "http://127.0.0.1/agents/test-agent/endpoint/protocols/openai/responses?api-version=v1",
			foundryAuth:         "foundry-auth-value",
			requestTimeout:      time.Second,
			stateRetention:      time.Minute,
			brokeredToolClasses: []harness.BrokeredToolClass{harness.BrokeredToolClassRead},
		},
		&http.Client{Timeout: time.Second},
	)
	request := responsesStartTurnRequest("agentkit-brokered-fixture")
	request.ToolExecutionMode = harness.ToolExecutionModeBrokered
	request.Input.Tools = []harness.ToolDefinition{{
		Name:          "conformance_read",
		Description:   "Synthetic AgentKit brokered fixture tool",
		BrokeredClass: harness.BrokeredToolClassRead,
		Parameters:    json.RawMessage(`{"type":"object","properties":{"probe":{"type":"boolean"}}}`),
	}}
	turn := &turnState{
		request:          request,
		pendingTools:     map[string]string{},
		bufferedResults:  map[string]harness.ToolCallResult{},
		bufferedDigests:  map[string]toolResultDigest{},
		submittedDigests: map[string]toolResultDigest{},
	}
	server.appendFrameLocked(turn, harness.FrameTurnStarted, "foundry hosted response started")
	var functionCall responsesResponse
	decodeFixtureInto(
		t,
		"testdata/agentkit-foundry-brokered/function_call_response.json",
		&functionCall,
	)
	server.handleResponsesResponse(turn, functionCall)
	requested := findFrame(turn.frames, harness.FrameToolCallRequested)
	if requested == nil {
		t.Fatalf("frames = %#v, want tool request from AgentKit fixture", turn.frames)
	}
	if requested.ToolName != "conformance_read" || requested.ToolCallID != "call_caresp_test_1" {
		t.Fatalf("tool request = %#v, want AgentKit fixture call", requested)
	}
	assertJSONFileEqual(t, "testdata/agentkit-foundry-brokered/initial_request.json", responsesRequest{
		Input: "please read telemetry",
	})

	outputs, err := functionCallOutputs([]harness.ToolCallResult{{
		Version:          harness.ProtocolVersion,
		RuntimeSessionID: request.RuntimeSessionID,
		TurnID:           request.TurnID,
		ToolCallID:       "call_caresp_test_1",
		IdempotencyKey:   harness.ToolRequestIdempotencyKey(request.RuntimeSessionID, request.TurnID, "call_caresp_test_1"),
		Approved:         true,
		Output:           json.RawMessage(`{"success":true}`),
	}})
	if err != nil {
		t.Fatalf("functionCallOutputs: %v", err)
	}
	assertJSONFileEqual(t, "testdata/agentkit-foundry-brokered/continuation_request.json", responsesRequest{
		PreviousResponseID: "caresp_test",
		Input:              outputs,
	})

	finalTurn := &turnState{
		request:          responsesStartTurnRequest("agentkit-final-fixture"),
		pendingTools:     map[string]string{},
		bufferedResults:  map[string]harness.ToolCallResult{},
		bufferedDigests:  map[string]toolResultDigest{},
		submittedDigests: map[string]toolResultDigest{},
	}
	server.appendFrameLocked(finalTurn, harness.FrameTurnStarted, "foundry hosted response started")
	var finalMessage responsesResponse
	decodeFixtureInto(t, "testdata/agentkit-foundry-brokered/final_message_response.json", &finalMessage)
	server.handleResponsesResponse(finalTurn, finalMessage)
	completed := findFrame(finalTurn.frames, harness.FrameTurnCompleted)
	want := `Brokered tool conformance_read completed with output: {"success":true}`
	if completed == nil || completed.Completed.Result != want {
		t.Fatalf("completed frame = %#v, want %q", completed, want)
	}
}

func TestResponsesAgentKitErrorPayloadFixturesMatchCanonicalEncoding(t *testing.T) {
	tests := []struct {
		name    string
		result  harness.ToolCallResult
		fixture string
	}{
		{
			name: "approval declined",
			result: baseToolResult("call-agentkit-1", false, nil, &harness.ErrorInfo{
				Code: "approval_declined", Message: "Human declined dispatch-work-order",
			}),
			fixture: "testdata/agentkit-foundry-brokered/approval_declined_payload.json",
		},
		{
			name: "policy rejection",
			result: baseToolResult("call-agentkit-2", false, nil, &harness.ErrorInfo{
				Code: "tool_policy_rejected", Message: "writes are disabled",
			}),
			fixture: "testdata/agentkit-foundry-brokered/tool_policy_rejection_payload.json",
		},
		{
			name: "execution failure",
			result: baseToolResult("call-agentkit-3", true, nil, &harness.ErrorInfo{
				Code: "tool_execution_failed", Message: "downstream timed out",
			}),
			fixture: "testdata/agentkit-foundry-brokered/tool_execution_failure_payload.json",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload, err := canonicalToolResultOutput(tt.result)
			if err != nil {
				t.Fatalf("canonicalToolResultOutput: %v", err)
			}
			assertJSONFileEqual(t, tt.fixture, json.RawMessage(payload))
		})
	}
}

func TestResponsesGoldenFixturesDoNotContainEndpointsOrSecrets(t *testing.T) {
	for _, root := range []string{"testdata/golden", "testdata/agentkit-foundry-brokered"} {
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatalf("ReadDir(%s): %v", root, err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			path := root + "/" + entry.Name()
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile(%s): %v", path, err)
			}
			text := strings.ToLower(string(data))
			for _, forbidden := range []string{"http://", "https://", "authorization", "api_key", "api-key", "bearer "} {
				if strings.Contains(text, forbidden) {
					t.Fatalf("fixture %s contains forbidden %q", path, forbidden)
				}
			}
		}
	}
}

func TestResponsesPreservesNumericJSONTokens(t *testing.T) {
	args, err := normalizeResponsesToolArguments(json.RawMessage(`{"id":9007199254740993}`))
	if err != nil {
		t.Fatalf("normalizeResponsesToolArguments: %v", err)
	}
	if got := string(args); got != `{"id":9007199254740993}` {
		t.Fatalf("arguments = %s, want numeric token preserved", got)
	}
	result := baseToolResult("call-2", true, json.RawMessage(`{"id":9007199254740993}`), nil)
	payload, err := canonicalToolResultOutput(result)
	if err != nil {
		t.Fatalf("canonicalToolResultOutput: %v", err)
	}
	want := `{"approved":true,"output":{"id":9007199254740993}}`
	if payload != want {
		t.Fatalf("payload = %s, want %s", payload, want)
	}
}

func TestResponsesLargeOutputFails(t *testing.T) {
	server := newServer(config{
		runtimeName:    "test",
		adapterBearer:  "adapter-auth-value",
		endpoint:       "http://127.0.0.1/agents/test-agent/endpoint/protocols/openai/responses?api-version=v1",
		foundryAuth:    "foundry-auth-value",
		requestTimeout: time.Second,
		stateRetention: time.Minute,
	}, &http.Client{Timeout: time.Second})
	turn := &turnState{
		request:          responsesStartTurnRequest("foundry-large-output"),
		pendingTools:     map[string]string{},
		bufferedResults:  map[string]harness.ToolCallResult{},
		bufferedDigests:  map[string]toolResultDigest{},
		submittedDigests: map[string]toolResultDigest{},
	}
	server.appendFrameLocked(turn, harness.FrameTurnStarted, "foundry hosted response started")
	server.handleResponsesResponse(turn, responsesResponse{
		ID:     "resp-large",
		Status: "completed",
		Output: []responsesOutput{{
			Type:    "message",
			Content: strings.Repeat("x", maxFoundryOutputBytes+1),
		}},
	})
	if hasFrameType(turn.frames, harness.FrameTurnCompleted) {
		t.Fatalf("frames = %#v, oversized response should not complete", turn.frames)
	}
	failed := findFrame(turn.frames, harness.FrameTurnFailed)
	if failed == nil || failed.Failed.Reason != "foundry_output_too_large" {
		t.Fatalf("failed frame = %#v, want foundry_output_too_large", failed)
	}
}

func TestResponsesOutputThatExceedsSSEFrameLimitFails(t *testing.T) {
	server := newServer(config{
		runtimeName:    "test",
		adapterBearer:  "adapter-auth-value",
		endpoint:       "http://127.0.0.1/agents/test-agent/endpoint/protocols/openai/responses?api-version=v1",
		foundryAuth:    "foundry-auth-value",
		requestTimeout: time.Second,
		stateRetention: time.Minute,
	}, &http.Client{Timeout: time.Second})
	turn := &turnState{
		request:          responsesStartTurnRequest("foundry-large-frame-output"),
		pendingTools:     map[string]string{},
		bufferedResults:  map[string]harness.ToolCallResult{},
		bufferedDigests:  map[string]toolResultDigest{},
		submittedDigests: map[string]toolResultDigest{},
	}
	server.appendFrameLocked(turn, harness.FrameTurnStarted, "foundry hosted response started")
	server.handleResponsesResponse(turn, responsesResponse{
		ID:     "resp-large-frame",
		Status: "completed",
		Output: []responsesOutput{{
			Type:    "message",
			Content: strings.Repeat("\"", maxFoundryOutputBytes),
		}},
	})
	if hasFrameType(turn.frames, harness.FrameTurnCompleted) {
		t.Fatalf("frames = %#v, oversized SSE frame should not complete", turn.frames)
	}
	failed := findFrame(turn.frames, harness.FrameTurnFailed)
	if failed == nil || failed.Failed.Reason != "foundry_output_frame_too_large" {
		t.Fatalf("failed frame = %#v, want foundry_output_frame_too_large", failed)
	}
}

func TestResponsesOversizedToolCallFrameFailsBeforeRequestingTool(t *testing.T) {
	server := newServer(config{
		runtimeName:         "test",
		adapterBearer:       "adapter-auth-value",
		endpoint:            "http://127.0.0.1/agents/test-agent/endpoint/protocols/openai/responses?api-version=v1",
		foundryAuth:         "foundry-auth-value",
		requestTimeout:      time.Second,
		stateRetention:      time.Minute,
		brokeredToolClasses: []harness.BrokeredToolClass{harness.BrokeredToolClassRead},
	}, &http.Client{Timeout: time.Second})
	turn := &turnState{
		request:          brokeredReadRequest("foundry-large-tool-call-frame"),
		pendingTools:     map[string]string{},
		bufferedResults:  map[string]harness.ToolCallResult{},
		bufferedDigests:  map[string]toolResultDigest{},
		submittedDigests: map[string]toolResultDigest{},
	}
	server.appendFrameLocked(turn, harness.FrameTurnStarted, "foundry hosted response started")
	arguments, err := json.Marshal(map[string]any{"payload": strings.Repeat("x", harness.MaxSSEFrameBytes)})
	if err != nil {
		t.Fatalf("marshal arguments: %v", err)
	}
	server.handleResponsesResponse(turn, responsesResponse{
		ID:     "resp-large-tool-call",
		Status: "completed",
		Output: []responsesOutput{{
			Type:      "function_call",
			CallID:    "call-1",
			Name:      "support-ticket-lookup",
			Arguments: arguments,
		}},
	})
	if hasFrameType(turn.frames, harness.FrameToolCallRequested) {
		t.Fatalf("frames = %#v, oversized tool call should not be requested", turn.frames)
	}
	failed := findFrame(turn.frames, harness.FrameTurnFailed)
	if failed == nil || failed.Failed.Reason != "foundry_tool_call_frame_too_large" {
		t.Fatalf("failed frame = %#v, want foundry_tool_call_frame_too_large", failed)
	}
}

func TestResponsesToolResultPreflightUsesExactEventualSequence(t *testing.T) {
	server := newServer(config{}, &http.Client{Timeout: time.Second})
	request := brokeredReadRequest("foundry-tool-result-boundary")
	turn := &turnState{
		request:    request,
		responseID: "resp-1",
		pendingTools: map[string]string{
			"call-1": "support-ticket-lookup",
			"call-2": "support-ticket-lookup",
		},
		bufferedResults:  map[string]harness.ToolCallResult{},
		bufferedDigests:  map[string]toolResultDigest{},
		submittedDigests: map[string]toolResultDigest{},
	}
	for range 8 {
		server.appendFrameLocked(turn, harness.FrameTurnStarted, "foundry hosted response started")
	}
	emptyResult := toolResultForRequest(request, "call-1", true, json.RawMessage(`{"payload":""}`), nil)
	seqNineFrame := server.newToolResultFrame(turn, 9, "support-ticket-lookup", emptyResult)
	seqNineFrame.CreatedAt = sseSizeProbeTime
	seqTenFrame := server.newToolResultFrame(turn, 10, "support-ticket-lookup", emptyResult)
	seqTenFrame.CreatedAt = sseSizeProbeTime
	seqNineJSON, err := json.Marshal(seqNineFrame)
	if err != nil {
		t.Fatalf("marshal sequence-nine frame: %v", err)
	}
	seqTenJSON, err := json.Marshal(seqTenFrame)
	if err != nil {
		t.Fatalf("marshal sequence-ten frame: %v", err)
	}
	if len(seqTenJSON) <= len(seqNineJSON) {
		t.Fatalf("frame sizes seq10=%d seq9=%d, want decimal-boundary overhead", len(seqTenJSON), len(seqNineJSON))
	}
	payloadSize := harness.MaxSSEFrameBytes - len("data: ") - len(seqNineJSON) - 1
	if payloadSize <= 0 {
		t.Fatalf("payload boundary = %d, want positive", payloadSize)
	}
	output := json.RawMessage(`{"payload":"` + strings.Repeat("x", payloadSize) + `"}`)
	result := toolResultForRequest(request, "call-1", true, output, nil)
	if !toolResultFrameFitsSSE(server.newToolResultFrame(turn, 9, "support-ticket-lookup", result)) {
		t.Fatal("eventual sequence-nine frame should fit")
	}
	if toolResultFrameFitsSSE(server.newToolResultFrame(turn, 10, "support-ticket-lookup", result)) {
		t.Fatal("sequence-ten probe unexpectedly fits boundary frame")
	}
	toSubmit, err := server.recordContinueResults(turn, []harness.ToolCallResult{result})
	if err != nil {
		t.Fatalf("recordContinueResults rejected exact eventual sequence: %v", err)
	}
	if len(toSubmit) != 0 || len(turn.bufferedResults) != 1 {
		t.Fatalf("toSubmit=%#v buffered=%#v, want one accepted partial result", toSubmit, turn.bufferedResults)
	}
}

func TestResponsesOversizedBatchValidatesAllResultsBeforeFailure(t *testing.T) {
	server := newServer(config{}, &http.Client{Timeout: time.Second})
	request := brokeredReadRequest("foundry-oversized-batch")
	turn := &turnState{
		request: request,
		pendingTools: map[string]string{
			"call-1": "support-ticket-lookup",
		},
		bufferedResults:  map[string]harness.ToolCallResult{},
		bufferedDigests:  map[string]toolResultDigest{},
		submittedDigests: map[string]toolResultDigest{},
	}
	server.appendFrameLocked(turn, harness.FrameTurnStarted, "foundry hosted response started")
	oversizedOutput, err := json.Marshal(map[string]any{"payload": strings.Repeat("x", harness.MaxSSEFrameBytes)})
	if err != nil {
		t.Fatalf("marshal oversized output: %v", err)
	}
	oversized := toolResultForRequest(request, "call-1", true, oversizedOutput, nil)
	unknown := toolResultForRequest(request, "call-missing", true, json.RawMessage(`{"success":true}`), nil)

	if _, err := server.recordContinueResults(turn, []harness.ToolCallResult{oversized, unknown}); err == nil ||
		!strings.Contains(err.Error(), "not pending") {
		t.Fatalf("recordContinueResults error = %v, want structural rejection", err)
	}
	if turn.completed || len(turn.bufferedResults) != 0 || len(turn.bufferedDigests) != 0 {
		t.Fatalf(
			"invalid batch mutated turn: completed=%v buffers=%#v/%#v",
			turn.completed,
			turn.bufferedResults,
			turn.bufferedDigests,
		)
	}
}

func TestResponsesTerminalFailureSuppressesPendingToolFrames(t *testing.T) {
	server := newServer(config{
		adapterBearer:       "adapter-auth-value",
		brokeredToolClasses: []harness.BrokeredToolClass{harness.BrokeredToolClassRead},
	}, &http.Client{Timeout: time.Second})
	request := brokeredReadRequest("foundry-suppress-pending")
	turn := &turnState{
		request:             request,
		pendingTools:        map[string]string{},
		suppressedToolCalls: map[string]struct{}{},
		bufferedResults:     map[string]harness.ToolCallResult{},
		bufferedDigests:     map[string]toolResultDigest{},
		submittedDigests:    map[string]toolResultDigest{},
	}
	server.turns[request.TurnID] = turn
	server.appendFrameLocked(turn, harness.FrameTurnStarted, "foundry hosted response started")
	server.handleResponsesResponse(turn, responsesResponse{
		ID:     "resp-1",
		Status: "completed",
		Output: []responsesOutput{
			{
				Type: "function_call", CallID: "call-1", Name: "support-ticket-lookup",
				Arguments: json.RawMessage(`{"incident":"inc-1"}`),
			},
			{
				Type: "function_call", CallID: "call-2", Name: "support-ticket-lookup",
				Arguments: json.RawMessage(`{"incident":"inc-2"}`),
			},
		},
	})
	first := toolResultForRequest(request, "call-1", true, json.RawMessage(`{"success":true}`), nil)
	toSubmit, err := server.recordContinueResults(turn, []harness.ToolCallResult{first})
	if err != nil || len(toSubmit) != 0 {
		t.Fatalf("buffer first result = %#v, %v", toSubmit, err)
	}
	oversizedOutput, err := json.Marshal(map[string]any{"payload": strings.Repeat("x", harness.MaxSSEFrameBytes)})
	if err != nil {
		t.Fatalf("marshal oversized output: %v", err)
	}
	second := toolResultForRequest(request, "call-2", true, oversizedOutput, nil)
	if _, err := server.recordContinueResults(turn, []harness.ToolCallResult{second}); err == nil {
		t.Fatal("oversized second result error = nil")
	}

	adapter := httptest.NewServer(server.handler())
	t.Cleanup(adapter.Close)
	client := newHarnessClient(t, adapter)
	frames := streamCurrentFrames(t, client, request.TurnID)
	if hasFrameType(frames, harness.FrameToolCallRequested) || !hasFrameType(frames, harness.FrameTurnFailed) {
		t.Fatalf("replayed frames = %#v, want pending tool requests suppressed before terminal failure", frames)
	}
}

func TestResponsesOversizedResultTombstonesPreviouslyBufferedResults(t *testing.T) {
	server := newServer(config{}, &http.Client{Timeout: time.Second})
	request := brokeredReadRequest("foundry-partial-then-oversized")
	turn := &turnState{
		request: request,
		pendingTools: map[string]string{
			"call-1": "support-ticket-lookup",
			"call-2": "support-ticket-lookup",
		},
		bufferedResults:  map[string]harness.ToolCallResult{},
		bufferedDigests:  map[string]toolResultDigest{},
		submittedDigests: map[string]toolResultDigest{},
	}
	server.appendFrameLocked(turn, harness.FrameTurnStarted, "foundry hosted response started")
	first := toolResultForRequest(request, "call-1", true, json.RawMessage(`{"success":true}`), nil)
	toSubmit, err := server.recordContinueResults(turn, []harness.ToolCallResult{first})
	if err != nil || len(toSubmit) != 0 {
		t.Fatalf("buffer first result = %#v, %v", toSubmit, err)
	}
	oversizedOutput, err := json.Marshal(map[string]any{"payload": strings.Repeat("x", harness.MaxSSEFrameBytes)})
	if err != nil {
		t.Fatalf("marshal oversized output: %v", err)
	}
	second := toolResultForRequest(request, "call-2", true, oversizedOutput, nil)
	if _, err := server.recordContinueResults(turn, []harness.ToolCallResult{second}); err == nil {
		t.Fatal("oversized second result error = nil")
	}
	if !turn.completed || len(turn.bufferedResults) != 0 || len(turn.bufferedDigests) != 0 {
		t.Fatalf(
			"terminal oversized state = completed:%v buffers:%#v/%#v",
			turn.completed,
			turn.bufferedResults,
			turn.bufferedDigests,
		)
	}
	for _, result := range []harness.ToolCallResult{first, second} {
		if err := server.ensureTerminalContinueIsDuplicate(turn, []harness.ToolCallResult{result}); err != nil {
			t.Fatalf("terminal duplicate %s rejected: %v", result.ToolCallID, err)
		}
	}
}

func TestResponsesOversizedToolResultFrameFailsBeforeContinuation(t *testing.T) {
	server := newServer(config{
		runtimeName:         "test",
		adapterBearer:       "adapter-auth-value",
		endpoint:            "http://127.0.0.1/agents/test-agent/endpoint/protocols/openai/responses?api-version=v1",
		foundryAuth:         "foundry-auth-value",
		requestTimeout:      time.Second,
		stateRetention:      time.Minute,
		brokeredToolClasses: []harness.BrokeredToolClass{harness.BrokeredToolClassRead},
	}, &http.Client{Timeout: time.Second})
	request := brokeredReadRequest("foundry-large-tool-result-frame")
	turn := &turnState{
		request:          request,
		responseID:       "resp-1",
		pendingTools:     map[string]string{"call-1": "support-ticket-lookup"},
		bufferedResults:  map[string]harness.ToolCallResult{},
		bufferedDigests:  map[string]toolResultDigest{},
		submittedDigests: map[string]toolResultDigest{},
	}
	server.appendFrameLocked(turn, harness.FrameTurnStarted, "foundry hosted response started")
	output, err := json.Marshal(map[string]any{"payload": strings.Repeat("x", harness.MaxSSEFrameBytes)})
	if err != nil {
		t.Fatalf("marshal output: %v", err)
	}
	result := toolResultForRequest(request, "call-1", true, output, nil)
	if _, err := server.recordContinueResults(turn, []harness.ToolCallResult{result}); err == nil {
		t.Fatal("recordContinueResults oversized output error = nil")
	}
	if hasFrameType(turn.frames, harness.FrameToolResultReceived) {
		t.Fatalf("frames = %#v, oversized tool result should not be streamed", turn.frames)
	}
	failed := findFrame(turn.frames, harness.FrameTurnFailed)
	if failed == nil || failed.Failed.Reason != "brokered_tool_result_frame_too_large" {
		t.Fatalf("failed frame = %#v, want brokered_tool_result_frame_too_large", failed)
	}
	if len(turn.bufferedResults) != 0 || len(turn.bufferedDigests) != 0 {
		t.Fatalf("oversized result retained buffered state: %#v/%#v", turn.bufferedResults, turn.bufferedDigests)
	}
	if len(turn.submittedDigests) != 1 {
		t.Fatalf("oversized result submitted digests = %#v, want duplicate tombstone", turn.submittedDigests)
	}
	if err := server.ensureTerminalContinueIsDuplicate(turn, []harness.ToolCallResult{result}); err != nil {
		t.Fatalf("identical oversized result retry was not accepted as duplicate: %v", err)
	}
}

func TestResponsesInitialPlatformErrorRetainsFailedTurn(t *testing.T) {
	foundry := newFakeResponses(t, fakeResponsesConfig{scenario: "platform_error"})
	adapter, server := newTestResponsesAdapterWithServer(t, foundry.endpoint(), nil)
	client := newHarnessClient(t, adapter)
	request := responsesStartTurnRequest("foundry-platform-error")

	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn should accept the terminal failed turn: %v", err)
	}
	server.mu.Lock()
	turn := server.turns[request.TurnID]
	if turn == nil {
		server.mu.Unlock()
		t.Fatalf("turn %q was discarded after an uncertain initial hosted response", request.TurnID)
	}
	if turn.initializing || !turn.completed {
		server.mu.Unlock()
		t.Fatalf("turn state = initializing:%v completed:%v, want terminal failed turn", turn.initializing, turn.completed)
	}
	failed := findFrame(turn.frames, harness.FrameTurnFailed)
	server.mu.Unlock()
	if failed == nil || failed.Failed.Reason != foundryInitialUnknown {
		t.Fatalf("failed frame = %#v, want foundry_initial_unknown", failed)
	}
	if strings.Contains(failed.Failed.Message, foundry.URL) ||
		strings.Contains(failed.Failed.Message, "unknown scenario") {
		t.Fatalf("failed frame leaked upstream detail: %#v", failed.Failed)
	}
	frames := streamCurrentFrames(t, client, request.TurnID)
	if streamed := findFrame(frames, harness.FrameTurnFailed); streamed == nil ||
		streamed.Failed.Reason != foundryInitialUnknown {
		t.Fatalf("streamed failed frame = %#v, want foundry_initial_unknown", streamed)
	}
	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("retrying retained TurnID: %v", err)
	}
	if foundry.postCount.Load() != 1 {
		t.Fatalf("hosted post count after retry = %d, want 1", foundry.postCount.Load())
	}
}

//nolint:dupl // Mirrors failure-status regression with a distinct non-terminal status.
func TestResponsesNonTerminalStatusDoesNotCompleteWithPartialText(t *testing.T) {
	server := newServer(config{
		runtimeName:    "test",
		adapterBearer:  "adapter-auth-value",
		endpoint:       "http://127.0.0.1/agents/test-agent/endpoint/protocols/openai/responses?api-version=v1",
		foundryAuth:    "foundry-auth-value",
		requestTimeout: time.Second,
		stateRetention: time.Minute,
	}, &http.Client{Timeout: time.Second})
	turn := &turnState{
		request:          responsesStartTurnRequest("foundry-in-progress"),
		pendingTools:     map[string]string{},
		bufferedResults:  map[string]harness.ToolCallResult{},
		bufferedDigests:  map[string]toolResultDigest{},
		submittedDigests: map[string]toolResultDigest{},
	}
	server.appendFrameLocked(turn, harness.FrameTurnStarted, "foundry hosted response started")
	server.handleResponsesResponse(turn, responsesResponse{
		ID:     "resp-in-progress",
		Status: "in_progress",
		Output: []responsesOutput{{Type: "message", Content: "partial text"}},
	})
	if hasFrameType(turn.frames, harness.FrameTurnCompleted) {
		t.Fatalf("frames = %#v, in-progress response should not complete", turn.frames)
	}
	failed := findFrame(turn.frames, harness.FrameTurnFailed)
	if failed == nil || failed.Failed.Reason != "foundry_in_progress" {
		t.Fatalf("failed frame = %#v, want foundry_in_progress", failed)
	}
}

//nolint:dupl // Mirrors non-terminal-status regression with a distinct failed status.
func TestResponsesFailureStatusDoesNotCompleteWithPartialText(t *testing.T) {
	server := newServer(config{
		runtimeName:    "test",
		adapterBearer:  "adapter-auth-value",
		endpoint:       "http://127.0.0.1/agents/test-agent/endpoint/protocols/openai/responses?api-version=v1",
		foundryAuth:    "foundry-auth-value",
		requestTimeout: time.Second,
		stateRetention: time.Minute,
	}, &http.Client{Timeout: time.Second})
	turn := &turnState{
		request:          responsesStartTurnRequest("foundry-failed-status"),
		pendingTools:     map[string]string{},
		bufferedResults:  map[string]harness.ToolCallResult{},
		bufferedDigests:  map[string]toolResultDigest{},
		submittedDigests: map[string]toolResultDigest{},
	}
	server.appendFrameLocked(turn, harness.FrameTurnStarted, "foundry hosted response started")
	server.handleResponsesResponse(turn, responsesResponse{
		ID:     "resp-failed",
		Status: "failed",
		Output: []responsesOutput{{
			Type:    "message",
			Content: "partial text should not be completed",
		}},
	})
	if hasFrameType(turn.frames, harness.FrameTurnCompleted) {
		t.Fatalf("frames = %#v, failed response should not complete", turn.frames)
	}
	failed := findFrame(turn.frames, harness.FrameTurnFailed)
	if failed == nil || failed.Failed.Reason != "foundry_failed" {
		t.Fatalf("failed frame = %#v, want foundry_failed", failed)
	}
}

func TestResponsesFailureStatusWithFunctionCallFailsBeforeToolRequest(t *testing.T) {
	server := newServer(config{
		runtimeName:         "test",
		adapterBearer:       "adapter-auth-value",
		endpoint:            "http://127.0.0.1/agents/test-agent/endpoint/protocols/openai/responses?api-version=v1",
		foundryAuth:         "foundry-auth-value",
		requestTimeout:      time.Second,
		stateRetention:      time.Minute,
		brokeredToolClasses: []harness.BrokeredToolClass{harness.BrokeredToolClassRead},
	}, &http.Client{Timeout: time.Second})
	turn := &turnState{
		request:          brokeredReadRequest("foundry-failed-function-call"),
		pendingTools:     map[string]string{},
		bufferedResults:  map[string]harness.ToolCallResult{},
		bufferedDigests:  map[string]toolResultDigest{},
		submittedDigests: map[string]toolResultDigest{},
	}
	server.appendFrameLocked(turn, harness.FrameTurnStarted, "foundry hosted response started")
	server.handleResponsesResponse(turn, responsesResponse{
		ID:     "resp-failed",
		Status: "incomplete",
		Output: []responsesOutput{{
			Type:      "function_call",
			CallID:    "call-1",
			Name:      "support-ticket-lookup",
			Arguments: json.RawMessage(`{"incident":"inc-1"}`),
		}},
	})
	if hasFrameType(turn.frames, harness.FrameToolCallRequested) {
		t.Fatalf("frames = %#v, failed response should not request a tool", turn.frames)
	}
	failed := findFrame(turn.frames, harness.FrameTurnFailed)
	if failed == nil || failed.Failed.Reason != "foundry_incomplete" {
		t.Fatalf("failed frame = %#v, want foundry_incomplete", failed)
	}
}

func TestSanitizeEndpointDoesNotReturnRawMalformedURL(t *testing.T) {
	raw := "http://[::1" + "?unsafe=do-not-log"
	if got := sanitizeEndpoint(raw); got == raw || strings.Contains(got, "do-not-log") {
		t.Fatalf("sanitizeEndpoint(%q) = %q, want redacted placeholder", raw, got)
	}
}

func TestResponsesMissingStatusDoesNotCompleteWithPartialText(t *testing.T) {
	server := newServer(config{
		runtimeName:    "test",
		adapterBearer:  "adapter-auth-value",
		endpoint:       "http://127.0.0.1/agents/test-agent/endpoint/protocols/openai/responses?api-version=v1",
		foundryAuth:    "foundry-auth-value",
		requestTimeout: time.Second,
		stateRetention: time.Minute,
	}, &http.Client{Timeout: time.Second})
	turn := &turnState{
		request:          responsesStartTurnRequest("foundry-missing-status"),
		pendingTools:     map[string]string{},
		bufferedResults:  map[string]harness.ToolCallResult{},
		bufferedDigests:  map[string]toolResultDigest{},
		submittedDigests: map[string]toolResultDigest{},
	}
	server.appendFrameLocked(turn, harness.FrameTurnStarted, "foundry hosted response started")
	server.handleResponsesResponse(turn, responsesResponse{
		ID:     "resp-missing-status",
		Output: []responsesOutput{{Type: "message", Content: "partial text"}},
	})
	if hasFrameType(turn.frames, harness.FrameTurnCompleted) {
		t.Fatalf("frames = %#v, missing status response should not complete", turn.frames)
	}
	failed := findFrame(turn.frames, harness.FrameTurnFailed)
	if failed == nil || failed.Failed.Reason != "foundry_status_missing" {
		t.Fatalf("failed frame = %#v, want foundry_status_missing", failed)
	}
}

func TestResponsesFunctionCallWithoutResponseIDFailsBeforeToolRequest(t *testing.T) {
	server := newServer(config{
		runtimeName:         "test",
		adapterBearer:       "adapter-auth-value",
		endpoint:            "http://127.0.0.1/agents/test-agent/endpoint/protocols/openai/responses?api-version=v1",
		foundryAuth:         "foundry-auth-value",
		requestTimeout:      time.Second,
		stateRetention:      time.Minute,
		brokeredToolClasses: []harness.BrokeredToolClass{harness.BrokeredToolClassRead},
	}, &http.Client{Timeout: time.Second})
	turn := &turnState{
		request:          brokeredReadRequest("foundry-missing-id"),
		pendingTools:     map[string]string{},
		bufferedResults:  map[string]harness.ToolCallResult{},
		bufferedDigests:  map[string]toolResultDigest{},
		submittedDigests: map[string]toolResultDigest{},
	}
	server.appendFrameLocked(turn, harness.FrameTurnStarted, "foundry hosted response started")
	server.handleResponsesResponse(turn, responsesResponse{
		Status: "completed",
		Output: []responsesOutput{{
			Type:      "function_call",
			CallID:    "call-1",
			Name:      "support-ticket-lookup",
			Arguments: json.RawMessage(`{"incident":"inc-1"}`),
		}},
	})
	if hasFrameType(turn.frames, harness.FrameToolCallRequested) {
		t.Fatalf("frames = %#v, should not request Orka execution without response id", turn.frames)
	}
	failed := findFrame(turn.frames, harness.FrameTurnFailed)
	if failed == nil || failed.Failed.Reason != "foundry_response_id_missing" {
		t.Fatalf("failed frame = %#v, want missing response id failure", failed)
	}
}

func TestCanonicalToolResultOutputWrapsNonObjectJSON(t *testing.T) {
	tests := []struct {
		name   string
		output json.RawMessage
		want   string
	}{
		{name: "object", output: json.RawMessage(`{"value":1}`), want: `{"approved":true,"output":{"value":1}}`},
		{name: "array", output: json.RawMessage(`[1,2]`), want: `{"approved":true,"output":{"result":[1,2]}}`},
		{name: "string", output: json.RawMessage(`"ok"`), want: `{"approved":true,"output":{"result":"ok"}}`},
		{name: "boolean", output: json.RawMessage(`true`), want: `{"approved":true,"output":{"result":true}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := baseToolResult("call-1", true, tt.output, nil)
			got, err := canonicalToolResultOutput(result)
			if err != nil {
				t.Fatalf("canonicalToolResultOutput: %v", err)
			}
			if got != tt.want {
				t.Fatalf("canonical output = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestCanonicalErrorAndDeclineOutputFixtures(t *testing.T) {
	tests := []struct {
		name    string
		result  harness.ToolCallResult
		fixture string
	}{
		{
			name: "approval declined",
			result: baseToolResult("call-1", false, nil, &harness.ErrorInfo{
				Code: "approval_declined", Message: "human declined",
			}),
			fixture: "testdata/golden/07_approval_declined_output.json",
		},
		{
			name: "policy rejection",
			result: baseToolResult("call-1", false, nil, &harness.ErrorInfo{
				Code: "tool_policy_rejected", Message: "tool is not allowed",
			}),
			fixture: "testdata/golden/08_tool_policy_rejection_output.json",
		},
		{
			name: "execution failure",
			result: baseToolResult("call-1", true, nil, &harness.ErrorInfo{
				Code: "tool_execution_failed", Message: "downstream failed",
			}),
			fixture: "testdata/golden/09_tool_execution_failure_output.json",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outputs, err := functionCallOutputs([]harness.ToolCallResult{tt.result})
			if err != nil {
				t.Fatalf("functionCallOutputs: %v", err)
			}
			assertJSONFileEqual(t, tt.fixture, outputs[0])
		})
	}
}

type fakeResponsesConfig struct {
	scenario           string
	toolName           string
	continuationStatus int
	requiredProof      string
}

type fakeResponses struct {
	*httptest.Server
	cfg                  fakeResponsesConfig
	mu                   sync.Mutex
	requests             []json.RawMessage
	headers              []http.Header
	postCount            atomic.Int32
	sawRequestLevelTools atomic.Bool
}

func newFakeResponses(t *testing.T, cfg fakeResponsesConfig) *fakeResponses {
	t.Helper()
	if cfg.scenario == "" {
		cfg.scenario = "observed"
	}
	if cfg.toolName == "" {
		cfg.toolName = "support-ticket-lookup"
	}
	f := &fakeResponses{cfg: cfg}
	mux := http.NewServeMux()
	mux.HandleFunc(
		"/agents/test-agent/endpoint/protocols/openai/responses",
		func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.NotFound(w, r)
				return
			}
			if got := r.URL.Query().Get("api-version"); got != "v1" {
				http.Error(w, "missing api-version", http.StatusBadRequest)
				return
			}
			body, _ := ioReadAll(r.Body)
			var decoded map[string]any
			_ = json.Unmarshal(body, &decoded)
			if _, ok := decoded["tools"]; ok {
				f.sawRequestLevelTools.Store(true)
			}
			f.mu.Lock()
			f.requests = append(f.requests, append(json.RawMessage(nil), body...))
			f.headers = append(f.headers, r.Header.Clone())
			f.mu.Unlock()
			f.postCount.Add(1)
			if _, continuing := decoded["previous_response_id"]; continuing {
				if got := r.Header.Get("x-agent-session-id"); got != fakeSessionID {
					http.Error(w, "missing session header", http.StatusBadRequest)
					return
				}
				if got := decoded["agent_session_id"]; got != fakeSessionID {
					http.Error(w, "missing body session", http.StatusBadRequest)
					return
				}
				if f.cfg.continuationStatus != 0 {
					http.Error(w, "continuation failed", f.cfg.continuationStatus)
					return
				}
				writeJSON(w, finalResponsesMessage())
				return
			}
			w.Header().Set("x-agent-session-id", fakeSessionID)
			switch f.cfg.scenario {
			case "observed":
				writeJSON(w, finalResponsesMessage())
			case "function_call":
				writeJSON(w, functionCallResponse(f.cfg.toolName))
			case "failed_with_session":
				writeJSON(w, map[string]any{
					"id":               "resp-failed",
					"agent_session_id": fakeSessionID,
					"status":           "failed",
				})
			case "malformed_arguments":
				writeJSON(
					w,
					map[string]any{
						"id":     "resp-1",
						"status": "completed",
						"output": []any{
							map[string]any{
								"type":      "function_call",
								"call_id":   "call-1",
								"name":      f.cfg.toolName,
								"arguments": "not-json",
							},
						},
					},
				)
			case "multiple_calls":
				writeJSON(w, multipleCallsResponse())
			default:
				http.Error(w, "unknown scenario", http.StatusInternalServerError)
			}
		},
	)
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func (f *fakeResponses) endpoint() string {
	return f.URL + "/agents/test-agent/endpoint/protocols/openai/responses?api-version=v1"
}

func (f *fakeResponses) requestBody(i int) json.RawMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append(json.RawMessage(nil), f.requests[i]...)
}

func (f *fakeResponses) requestHeader(i int) http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.headers[i].Clone()
}

func functionCallResponse(toolName string) map[string]any {
	return map[string]any{
		"id":               "resp-1",
		"agent_session_id": fakeSessionID,
		"status":           "completed",
		"output": []any{
			map[string]any{
				"type":      "function_call",
				"call_id":   "call-1",
				"name":      toolName,
				"arguments": `{"incident":"inc-1"}`,
			},
		},
	}
}

func multipleCallsResponse() map[string]any {
	return map[string]any{"id": "resp-1", "agent_session_id": fakeSessionID, "status": "completed", "output": []any{
		map[string]any{
			"type":      "function_call",
			"call_id":   "call-1",
			"name":      "support-ticket-lookup",
			"arguments": `{"incident":"inc-1"}`,
		},
		map[string]any{
			"type":      "function_call",
			"call_id":   "call-2",
			"name":      "support-ticket-lookup",
			"arguments": `{"incident":"inc-2"}`,
		},
	}}
}

func finalResponsesMessage() map[string]any {
	return map[string]any{
		"id":               "resp-2",
		"agent_session_id": fakeSessionID,
		"status":           "completed",
		"output": []any{
			map[string]any{
				"type":    "message",
				"role":    "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": "foundry final answer"}},
			},
		},
	}
}

func newTestResponsesAdapter(t *testing.T, endpoint string, classes []harness.BrokeredToolClass) *httptest.Server {
	t.Helper()
	adapter, _ := newTestResponsesAdapterWithServer(t, endpoint, classes)
	return adapter
}

func newTestResponsesAdapterWithServer(
	t *testing.T,
	endpoint string,
	classes []harness.BrokeredToolClass,
) (*httptest.Server, *server) {
	t.Helper()
	return newTestResponsesAdapterWithHTTPClient(
		t,
		endpoint,
		classes,
		&http.Client{Timeout: time.Second},
	)
}

func newTestResponsesAdapterWithHTTPClient(
	t *testing.T,
	endpoint string,
	classes []harness.BrokeredToolClass,
	backendClient *http.Client,
) (*httptest.Server, *server) {
	t.Helper()
	continuationProof := ""
	if len(classes) > 0 {
		continuationProof = testContinuationProof
	}
	s := newServer(config{
		addr:                ":0",
		runtimeName:         "foundry-responses-test",
		adapterBearer:       "adapter-auth-value",
		endpoint:            endpoint,
		foundryAuth:         "foundry-auth-value",
		apiVersion:          "v1",
		requestTimeout:      time.Second,
		stateRetention:      time.Minute,
		continuationProof:   continuationProof,
		brokeredToolClasses: append([]harness.BrokeredToolClass(nil), classes...),
	}, backendClient)
	adapter := httptest.NewServer(s.handler())
	t.Cleanup(adapter.Close)
	return adapter, s
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func jsonHTTPResponse(r *http.Request, value any) (*http.Response, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(payload)),
		Request:    r,
	}, nil
}

func jsonHTTPResponseWithSession(
	r *http.Request,
	value any,
	sessionID string,
) (*http.Response, error) {
	response, err := jsonHTTPResponse(r, value)
	if err != nil {
		return nil, err
	}
	response.Header.Set("x-agent-session-id", sessionID)
	return response, nil
}

func newHarnessClient(t *testing.T, adapter *httptest.Server) *harness.Client {
	t.Helper()
	client, err := harness.NewClient(
		adapter.URL,
		harness.WithBearerToken("adapter-auth-value"),
		harness.WithControlTimeout(2*time.Second),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}

func responsesStartTurnRequest(name string) harness.StartTurnRequest {
	return harness.StartTurnRequest{
		Version:           harness.ProtocolVersion,
		Namespace:         "default",
		TaskName:          name,
		SessionName:       name,
		RuntimeSessionID:  harness.RuntimeSessionID(name + "-runtime"),
		TurnID:            harness.HarnessTurnID(name + "-turn"),
		CorrelationID:     name + "-corr",
		Deadline:          time.Now().UTC().Add(time.Minute),
		AuthIdentity:      harness.AuthIdentity{Subject: "task:default/" + name},
		ToolExecutionMode: harness.ToolExecutionModeObserved,
		Input:             harness.TurnInput{Prompt: "Investigate incident"},
	}
}

func cancelRequestForStart(start harness.StartTurnRequest) harness.CancelTurnRequest {
	return harness.CancelTurnRequest{
		Version:          harness.ProtocolVersion,
		Namespace:        start.Namespace,
		TaskName:         start.TaskName,
		SessionName:      start.SessionName,
		RuntimeSessionID: start.RuntimeSessionID,
		TurnID:           start.TurnID,
		CorrelationID:    start.CorrelationID,
		Reason:           "test cancellation",
	}
}

func brokeredReadRequest(name string) harness.StartTurnRequest {
	request := responsesStartTurnRequest(name)
	request.ToolExecutionMode = harness.ToolExecutionModeBrokered
	request.Input.Tools = []harness.ToolDefinition{{
		Name:          "support-ticket-lookup",
		Description:   "Look up support ticket",
		BrokeredClass: harness.BrokeredToolClassRead,
		Parameters:    json.RawMessage(`{"type":"object","properties":{"incident":{"type":"string"}}}`),
	}}
	return request
}

func brokeredWriteRequest(name string) harness.StartTurnRequest {
	request := responsesStartTurnRequest(name)
	request.ToolExecutionMode = harness.ToolExecutionModeBrokered
	request.Input.Tools = []harness.ToolDefinition{{
		Name:          "dispatch-work-order",
		Description:   "Dispatch a work order",
		BrokeredClass: harness.BrokeredToolClassWrite,
		Parameters: json.RawMessage(
			`{"type":"object","properties":{"incident":{"type":"string"}},"required":["incident"]}`,
		),
	}}
	return request
}

func goldenContinueRequest(
	request harness.StartTurnRequest,
	callID string,
	output json.RawMessage,
) harness.ContinueTurnRequest {
	return harness.ContinueTurnRequest{
		Version:          harness.ProtocolVersion,
		Namespace:        request.Namespace,
		TaskName:         request.TaskName,
		SessionName:      request.SessionName,
		RuntimeSessionID: request.RuntimeSessionID,
		TurnID:           request.TurnID,
		CorrelationID:    request.CorrelationID,
		ToolResults:      []harness.ToolCallResult{toolResultForRequest(request, callID, true, output, nil)},
	}
}

func toolResultForRequest(
	request harness.StartTurnRequest,
	callID string,
	approved bool,
	output json.RawMessage,
	errInfo *harness.ErrorInfo,
) harness.ToolCallResult {
	return harness.ToolCallResult{
		Version:          harness.ProtocolVersion,
		RuntimeSessionID: request.RuntimeSessionID,
		TurnID:           request.TurnID,
		ToolCallID:       callID,
		IdempotencyKey:   harness.ToolRequestIdempotencyKey(request.RuntimeSessionID, request.TurnID, callID),
		Approved:         approved,
		Output:           output,
		Error:            errInfo,
	}
}

func baseToolResult(
	callID string,
	approved bool,
	output json.RawMessage,
	errInfo *harness.ErrorInfo,
) harness.ToolCallResult {
	return harness.ToolCallResult{
		Version:          harness.ProtocolVersion,
		RuntimeSessionID: "foundry-brokered-runtime",
		TurnID:           "foundry-brokered-turn",
		ToolCallID:       callID,
		IdempotencyKey: harness.ToolRequestIdempotencyKey(
			"foundry-brokered-runtime",
			"foundry-brokered-turn",
			callID,
		),
		Approved: approved,
		Output:   output,
		Error:    errInfo,
	}
}

func streamCurrentFrames(
	t *testing.T,
	client *harness.Client,
	turnID harness.HarnessTurnID,
) []harness.HarnessEventFrame {
	t.Helper()
	var frames []harness.HarnessEventFrame
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := client.StreamFrames(ctx, turnID, 0, func(frame harness.HarnessEventFrame) error {
		frames = append(frames, frame)
		return nil
	})
	if err != nil && ctx.Err() == nil {
		t.Fatalf("StreamFrames: %v", err)
	}
	return frames
}

func hasFrameType(frames []harness.HarnessEventFrame, typ harness.FrameType) bool {
	return findFrame(frames, typ) != nil
}

func findFrame(frames []harness.HarnessEventFrame, typ harness.FrameType) *harness.HarnessEventFrame {
	for i := range frames {
		if frames[i].Type == typ {
			return &frames[i]
		}
	}
	return nil
}

func findFrames(frames []harness.HarnessEventFrame, typ harness.FrameType) []harness.HarnessEventFrame {
	out := []harness.HarnessEventFrame{}
	for _, frame := range frames {
		if frame.Type == typ {
			out = append(out, frame)
		}
	}
	return out
}

func scrubFrameForGolden(frame harness.HarnessEventFrame) map[string]any {
	encoded, _ := json.Marshal(frame)
	var decoded map[string]any
	_ = json.Unmarshal(encoded, &decoded)
	delete(decoded, "createdAt")
	return decoded
}

func assertJSONFileEqual(t *testing.T, path string, actual any) {
	t.Helper()
	expectedBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	expected := decodeJSONForCompare(t, expectedBytes)
	actualBytes, err := json.Marshal(actual)
	if err != nil {
		t.Fatalf("marshal actual: %v", err)
	}
	actualValue := decodeJSONForCompare(t, actualBytes)
	if !reflect.DeepEqual(expected, actualValue) {
		expectedPretty, _ := json.MarshalIndent(expected, "", "  ")
		actualPretty, _ := json.MarshalIndent(actualValue, "", "  ")
		t.Fatalf("JSON mismatch for %s\nexpected: %s\nactual:   %s", path, expectedPretty, actualPretty)
	}
}

func requestMap(t *testing.T, data json.RawMessage) map[string]any {
	t.Helper()
	decoded, ok := decodeJSONForCompare(t, data).(map[string]any)
	if !ok {
		t.Fatalf("request body is not a JSON object: %s", string(data))
	}
	return decoded
}

func decodeJSONForCompare(t *testing.T, data []byte) any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("decode JSON: %v\n%s", err, string(data))
	}
	return value
}

func decodeFixtureInto(t *testing.T, path string, out any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		t.Fatalf("decode fixture %s: %v", path, err)
	}
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func ioReadAll(r io.Reader) ([]byte, error) {
	return io.ReadAll(r)
}
