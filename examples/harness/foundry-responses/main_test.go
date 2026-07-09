package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/harness"
	"github.com/orka-agents/orka/internal/harness/conformance"
)

const fakeSessionID = "session-1"

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
	if _, err := client.StartTurn(context.Background(), responsesStartTurnRequest("foundry-redirect")); err == nil {
		t.Fatalf("StartTurn followed redirect and succeeded, want rejection")
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
	if err := client.StreamFrames(context.Background(), request.TurnID, 0, func(frame harness.HarnessEventFrame) error {
		frames = append(frames, frame)
		return nil
	}); err != nil {
		t.Fatalf("StreamFrames before continue: %v", err)
	}
	requested := findFrame(frames, harness.FrameToolCallRequested)
	if requested == nil {
		t.Fatalf("frames = %#v, want tool request", frames)
	}
	if requested.ToolName != "support-ticket-lookup" || requested.ToolCallID != "call-1" {
		t.Fatalf("tool request = %#v", requested)
	}
	assertJSONFileEqual(t, "testdata/golden/03_tool_call_requested_frame.json", scrubFrameForGolden(*requested))

	continueRequest := goldenContinueRequest(request, requested.ToolCallID, json.RawMessage(`{"success":true}`))
	assertJSONFileEqual(t, "testdata/golden/04_orka_continue_request.json", continueRequest)
	if _, err := client.ContinueTurn(context.Background(), continueRequest); err != nil {
		t.Fatalf("ContinueTurn: %v", err)
	}
	assertJSONFileEqual(t, "testdata/golden/05_hosted_continuation_request.json", foundry.requestBody(1))
	if got := foundry.requestHeader(1).Get("x-agent-session-id"); got != fakeSessionID {
		t.Fatalf("continuation x-agent-session-id = %q, want session-1", got)
	}

	frames = nil
	if err := client.StreamFrames(
		context.Background(),
		request.TurnID,
		requested.Seq,
		func(frame harness.HarnessEventFrame) error {
			frames = append(frames, frame)
			return nil
		},
	); err != nil {
		t.Fatalf("StreamFrames after continue: %v", err)
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
	frames := streamAllFrames(t, client, request.TurnID)
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
	frames := streamAllFrames(t, client, request.TurnID)
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
	frames = streamAllFrames(t, client, request.TurnID)
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
	frames := streamAllFrames(t, client, request.TurnID)
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
	frames := streamAllFrames(t, client, request.TurnID)
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
	frames := streamAllFrames(t, client, request.TurnID)
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
	frames = streamAllFrames(t, client, request.TurnID)
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
	frames := streamAllFrames(t, client, request.TurnID)
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

func TestResponsesAdapterSendsBrokeredContinuationProofHeader(t *testing.T) {
	foundry := newFakeResponses(t, fakeResponsesConfig{
		scenario:      "function_call",
		toolName:      "support-ticket-lookup",
		requiredProof: "proof-for-test",
	})
	s := newServer(config{
		runtimeName:         "foundry-responses-test",
		adapterBearer:       "adapter-auth-value",
		endpoint:            foundry.endpoint(),
		foundryAuth:         "foundry-auth-value",
		requestTimeout:      time.Second,
		stateRetention:      time.Minute,
		maxApprovalWait:     time.Minute,
		continuationProof:   "proof-for-test",
		brokeredToolClasses: []harness.BrokeredToolClass{harness.BrokeredToolClassRead},
	}, &http.Client{Timeout: time.Second})
	adapter := httptest.NewServer(s.handler())
	t.Cleanup(adapter.Close)
	client := newHarnessClient(t, adapter)
	request := brokeredReadRequest("foundry-continuation-proof")

	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	frames := streamAllFrames(t, client, request.TurnID)
	requested := findFrame(frames, harness.FrameToolCallRequested)
	if requested == nil {
		t.Fatalf("frames = %#v, want tool request", frames)
	}
	continueRequest := goldenContinueRequest(request, requested.ToolCallID, json.RawMessage(`{"success":true}`))
	if _, err := client.ContinueTurn(context.Background(), continueRequest); err != nil {
		t.Fatalf("ContinueTurn: %v", err)
	}
	if got := foundry.requestHeader(1).Get("X-AgentKit-Brokered-Continuation-Proof"); got != "proof-for-test" {
		t.Fatalf("continuation proof header = %q, want proof-for-test", got)
	}
	if got := foundry.requestHeader(0).Get("X-AgentKit-Brokered-Continuation-Proof"); got != "" {
		t.Fatalf("initial proof header = %q, want empty", got)
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
		maxApprovalWait:     time.Minute,
		brokeredToolClasses: []harness.BrokeredToolClass{harness.BrokeredToolClassRead},
	}, &http.Client{Timeout: time.Second})
	request := brokeredReadRequest("foundry-already-submitted")
	result := toolResultForRequest(request, "call-1", true, json.RawMessage(`{"success":true}`), nil)
	payload, err := canonicalToolResultOutput(result)
	if err != nil {
		t.Fatalf("canonicalToolResultOutput: %v", err)
	}
	turn := &turnState{
		request:           request,
		pendingTools:      map[string]string{"call-1": "support-ticket-lookup"},
		pendingSince:      map[string]time.Time{"call-1": time.Now().UTC()},
		bufferedResults:   map[string]harness.ToolCallResult{"call-1": result},
		bufferedPayloads:  map[string]string{"call-1": payload},
		submittedPayloads: map[string]string{"call-1": payload},
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
	frames := streamAllFrames(t, client, request.TurnID)
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
	frames = streamAllFrames(t, client, request.TurnID)
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
	adapter := newTestResponsesAdapter(
		t,
		foundry.endpoint(),
		[]harness.BrokeredToolClass{harness.BrokeredToolClassRead},
	)
	client := newHarnessClient(t, adapter)
	request := brokeredReadRequest("foundry-continuation-failure")

	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	frames := streamAllFrames(t, client, request.TurnID)
	requested := findFrame(frames, harness.FrameToolCallRequested)
	if requested == nil {
		t.Fatalf("frames = %#v, want tool request", frames)
	}
	continueRequest := goldenContinueRequest(request, requested.ToolCallID, json.RawMessage(`{"success":true}`))
	if _, err := client.ContinueTurn(context.Background(), continueRequest); err == nil {
		t.Fatalf("ContinueTurn succeeded, want hosted continuation failure")
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
	frames = streamAllFrames(t, client, request.TurnID)
	failed := findFrame(frames, harness.FrameTurnFailed)
	if failed == nil || failed.Failed.Reason != "foundry_continuation_unknown" {
		t.Fatalf("failed frame = %#v, want fail-closed continuation failure", failed)
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
		maxApprovalWait:     time.Minute,
		brokeredToolClasses: []harness.BrokeredToolClass{harness.BrokeredToolClassRead},
	}, &http.Client{Timeout: time.Second})
	request := brokeredReadRequest("foundry-repeated-call")
	turn := &turnState{
		request:           request,
		pendingTools:      map[string]string{},
		pendingSince:      map[string]time.Time{},
		bufferedResults:   map[string]harness.ToolCallResult{},
		bufferedPayloads:  map[string]string{},
		submittedPayloads: map[string]string{"call-1": `{"approved":true}`},
	}
	server.appendFrameLocked(turn, harness.FrameTurnStarted, "foundry hosted response started", nil)
	server.handleResponsesResponse(turn, responsesResponse{
		ID: "resp-repeat",
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
		maxApprovalWait:     time.Minute,
		brokeredToolClasses: []harness.BrokeredToolClass{harness.BrokeredToolClassRead},
	}, &http.Client{Timeout: time.Second})
	request := brokeredReadRequest("foundry-mixed-repeated-call")
	turn := &turnState{
		request:           request,
		pendingTools:      map[string]string{"call-1": "support-ticket-lookup"},
		pendingSince:      map[string]time.Time{"call-1": time.Now().UTC()},
		bufferedResults:   map[string]harness.ToolCallResult{},
		bufferedPayloads:  map[string]string{},
		submittedPayloads: map[string]string{},
	}
	server.appendFrameLocked(turn, harness.FrameTurnStarted, "foundry hosted response started", nil)
	server.handleResponsesResponse(turn, responsesResponse{
		ID: "resp-repeat",
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

func TestResponsesAdapterPendingToolTimesOutWithoutContinuation(t *testing.T) {
	server := newServer(config{
		runtimeName:         "test",
		adapterBearer:       "adapter-auth-value",
		endpoint:            "http://127.0.0.1/agents/test-agent/endpoint/protocols/openai/responses?api-version=v1",
		foundryAuth:         "foundry-auth-value",
		requestTimeout:      time.Second,
		stateRetention:      time.Minute,
		maxApprovalWait:     10 * time.Millisecond,
		brokeredToolClasses: []harness.BrokeredToolClass{harness.BrokeredToolClassRead},
	}, &http.Client{Timeout: time.Second})
	turn := &turnState{
		request:           brokeredReadRequest("foundry-timeout"),
		pendingTools:      map[string]string{},
		pendingSince:      map[string]time.Time{},
		bufferedResults:   map[string]harness.ToolCallResult{},
		bufferedPayloads:  map[string]string{},
		submittedPayloads: map[string]string{},
	}
	server.turns[turn.request.TurnID] = turn
	server.appendFrameLocked(turn, harness.FrameTurnStarted, "foundry hosted response started", nil)
	server.handleResponsesResponse(turn, responsesResponse{
		ID: "resp-1",
		Output: []responsesOutput{{
			Type:      "function_call",
			CallID:    "call-1",
			Name:      "support-ticket-lookup",
			Arguments: json.RawMessage(`{"incident":"inc-1"}`),
		}},
	})
	time.Sleep(50 * time.Millisecond)
	server.mu.Lock()
	defer server.mu.Unlock()
	failed := findFrame(turn.frames, harness.FrameTurnFailed)
	if failed == nil || failed.Failed.Reason != "approval_wait_exceeded" {
		t.Fatalf("failed frame = %#v, want approval_wait_exceeded", failed)
	}
}

func TestResponsesAdapterAlreadySubmittedPendingResultIsNoop(t *testing.T) {
	server := newServer(config{maxApprovalWait: time.Minute}, &http.Client{Timeout: time.Second})
	request := brokeredReadRequest("foundry-submitted-noop")
	turn := &turnState{
		request:           request,
		pendingTools:      map[string]string{"call-1": "support-ticket-lookup"},
		pendingSince:      map[string]time.Time{"call-1": time.Now().UTC()},
		bufferedResults:   map[string]harness.ToolCallResult{},
		bufferedPayloads:  map[string]string{},
		submittedPayloads: map[string]string{},
	}
	result := toolResultForRequest(request, "call-1", true, json.RawMessage(`{"success":true}`), nil)
	payload, err := canonicalToolResultOutput(result)
	if err != nil {
		t.Fatalf("canonicalToolResultOutput: %v", err)
	}
	turn.submittedPayloads["call-1"] = payload

	toSubmit, err := server.recordContinueResults(turn, []harness.ToolCallResult{result})
	if err != nil {
		t.Fatalf("recordContinueResults: %v", err)
	}
	if len(toSubmit) != 0 {
		t.Fatalf("toSubmit = %#v, want duplicate submitted result to be a no-op", toSubmit)
	}
}

func TestResponsesAdapterPendingTimeoutSkipsSubmittedCall(t *testing.T) {
	server := newServer(config{
		runtimeName:         "test",
		adapterBearer:       "adapter-auth-value",
		endpoint:            "http://127.0.0.1/agents/test-agent/endpoint/protocols/openai/responses?api-version=v1",
		foundryAuth:         "foundry-auth-value",
		requestTimeout:      time.Second,
		stateRetention:      time.Minute,
		maxApprovalWait:     10 * time.Millisecond,
		brokeredToolClasses: []harness.BrokeredToolClass{harness.BrokeredToolClassRead},
	}, &http.Client{Timeout: time.Second})
	turn := &turnState{
		request:           brokeredReadRequest("foundry-timeout-submitted"),
		pendingTools:      map[string]string{"call-1": "support-ticket-lookup"},
		pendingSince:      map[string]time.Time{"call-1": time.Now().UTC()},
		bufferedResults:   map[string]harness.ToolCallResult{},
		bufferedPayloads:  map[string]string{},
		submittedPayloads: map[string]string{"call-1": `{"approved":true}`},
	}
	server.turns[turn.request.TurnID] = turn
	server.appendFrameLocked(turn, harness.FrameTurnStarted, "foundry hosted response started", nil)
	server.schedulePendingToolTimeoutLocked(turn, "call-1")
	time.Sleep(50 * time.Millisecond)
	server.mu.Lock()
	defer server.mu.Unlock()
	if failed := findFrame(turn.frames, harness.FrameTurnFailed); failed != nil {
		t.Fatalf("failed frame = %#v, submitted call should not time out", failed)
	}
}

func TestResponsesAdapterBrokeredMaxTurnIncludesApprovalWait(t *testing.T) {
	foundry := newFakeResponses(t, fakeResponsesConfig{scenario: "observed"})
	s := newServer(config{
		runtimeName:         "foundry-responses-test",
		adapterBearer:       "adapter-auth-value",
		endpoint:            foundry.endpoint(),
		foundryAuth:         "foundry-auth-value",
		requestTimeout:      2 * time.Second,
		stateRetention:      time.Minute,
		maxApprovalWait:     30 * time.Minute,
		brokeredToolClasses: []harness.BrokeredToolClass{harness.BrokeredToolClassRead},
	}, &http.Client{Timeout: time.Second})
	adapter := httptest.NewServer(s.handler())
	t.Cleanup(adapter.Close)
	client := newHarnessClient(t, adapter)

	caps, err := client.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if caps.MaxTurnSeconds < int((30*time.Minute + 2*time.Second).Seconds()) {
		t.Fatalf("MaxTurnSeconds = %d, want approval wait included", caps.MaxTurnSeconds)
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
	frames := streamAllFrames(t, client, request.TurnID)
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
	if err == nil || !strings.Contains(err.Error(), "turn not found") {
		t.Fatalf("restart continue error = %v, want clear turn not found", err)
	}
	if foundry.postCount.Load() != 1 {
		t.Fatalf(
			"hosted post count after state-loss continue = %d, want no duplicate continuation",
			foundry.postCount.Load(),
		)
	}
}

func TestResponsesEndpointSafety(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		want     bool
	}{
		{
			name:     "https responses",
			endpoint: "https://example.openai.azure.com/agents/a/endpoint/protocols/openai/responses?api-version=v1",
			want:     true,
		},
		{
			name:     "loopback http",
			endpoint: "http://127.0.0.1:8080/agents/a/endpoint/protocols/openai/responses?api-version=v1",
			want:     true,
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

func TestResponsesParserConsumesGoldenFixtures(t *testing.T) {
	server := newServer(
		config{
			runtimeName:         "test",
			adapterBearer:       "adapter-auth-value",
			endpoint:            "http://127.0.0.1/agents/test-agent/endpoint/protocols/openai/responses?api-version=v1",
			foundryAuth:         "foundry-auth-value",
			requestTimeout:      time.Second,
			stateRetention:      time.Minute,
			maxApprovalWait:     time.Minute,
			brokeredToolClasses: []harness.BrokeredToolClass{harness.BrokeredToolClassRead},
		},
		&http.Client{Timeout: time.Second},
	)
	request := brokeredReadRequest("foundry-brokered")
	turn := &turnState{
		request:           request,
		pendingTools:      map[string]string{},
		pendingSince:      map[string]time.Time{},
		bufferedResults:   map[string]harness.ToolCallResult{},
		bufferedPayloads:  map[string]string{},
		submittedPayloads: map[string]string{},
	}
	server.appendFrameLocked(turn, harness.FrameTurnStarted, "foundry hosted response started", nil)
	var functionCall responsesResponse
	decodeFixtureInto(t, "testdata/golden/02_function_call_response.json", &functionCall)
	server.handleResponsesResponse(turn, functionCall)
	requested := findFrame(turn.frames, harness.FrameToolCallRequested)
	if requested == nil {
		t.Fatalf("frames = %#v, want tool request from fixture", turn.frames)
	}
	assertJSONFileEqual(t, "testdata/golden/03_tool_call_requested_frame.json", scrubFrameForGolden(*requested))

	finalTurn := &turnState{
		request:           responsesStartTurnRequest("foundry-final"),
		pendingTools:      map[string]string{},
		pendingSince:      map[string]time.Time{},
		bufferedResults:   map[string]harness.ToolCallResult{},
		bufferedPayloads:  map[string]string{},
		submittedPayloads: map[string]string{},
	}
	server.appendFrameLocked(finalTurn, harness.FrameTurnStarted, "foundry hosted response started", nil)
	var finalMessage responsesResponse
	decodeFixtureInto(t, "testdata/golden/06_final_message_response.json", &finalMessage)
	server.handleResponsesResponse(finalTurn, finalMessage)
	completed := findFrame(finalTurn.frames, harness.FrameTurnCompleted)
	if completed == nil || completed.Completed.Result != "foundry final answer" {
		t.Fatalf("completed frame = %#v, want final answer", completed)
	}

	multipleTurn := &turnState{
		request:           request,
		pendingTools:      map[string]string{},
		pendingSince:      map[string]time.Time{},
		bufferedResults:   map[string]harness.ToolCallResult{},
		bufferedPayloads:  map[string]string{},
		submittedPayloads: map[string]string{},
	}
	server.appendFrameLocked(multipleTurn, harness.FrameTurnStarted, "foundry hosted response started", nil)
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
			maxApprovalWait:     time.Minute,
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
		request:           request,
		pendingTools:      map[string]string{},
		pendingSince:      map[string]time.Time{},
		bufferedResults:   map[string]harness.ToolCallResult{},
		bufferedPayloads:  map[string]string{},
		submittedPayloads: map[string]string{},
	}
	server.appendFrameLocked(turn, harness.FrameTurnStarted, "foundry hosted response started", nil)
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
		request:           responsesStartTurnRequest("agentkit-final-fixture"),
		pendingTools:      map[string]string{},
		pendingSince:      map[string]time.Time{},
		bufferedResults:   map[string]harness.ToolCallResult{},
		bufferedPayloads:  map[string]string{},
		submittedPayloads: map[string]string{},
	}
	server.appendFrameLocked(finalTurn, harness.FrameTurnStarted, "foundry hosted response started", nil)
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
		runtimeName:     "test",
		adapterBearer:   "adapter-auth-value",
		endpoint:        "http://127.0.0.1/agents/test-agent/endpoint/protocols/openai/responses?api-version=v1",
		foundryAuth:     "foundry-auth-value",
		requestTimeout:  time.Second,
		stateRetention:  time.Minute,
		maxApprovalWait: time.Minute,
	}, &http.Client{Timeout: time.Second})
	turn := &turnState{
		request:           responsesStartTurnRequest("foundry-large-output"),
		pendingTools:      map[string]string{},
		pendingSince:      map[string]time.Time{},
		bufferedResults:   map[string]harness.ToolCallResult{},
		bufferedPayloads:  map[string]string{},
		submittedPayloads: map[string]string{},
	}
	server.appendFrameLocked(turn, harness.FrameTurnStarted, "foundry hosted response started", nil)
	server.handleResponsesResponse(turn, responsesResponse{
		ID: "resp-large",
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

func TestResponsesInitialPlatformErrorDoesNotRetainTurn(t *testing.T) {
	foundry := newFakeResponses(t, fakeResponsesConfig{scenario: "platform_error"})
	adapter, server := newTestResponsesAdapterWithServer(t, foundry.endpoint(), nil)
	client := newHarnessClient(t, adapter)
	request := responsesStartTurnRequest("foundry-platform-error")

	if _, err := client.StartTurn(context.Background(), request); err == nil {
		t.Fatal("StartTurn succeeded, want platform error")
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if _, exists := server.turns[request.TurnID]; exists {
		t.Fatalf("turn %q retained after failed initial hosted response", request.TurnID)
	}
}

func TestResponsesFailureStatusDoesNotCompleteWithPartialText(t *testing.T) {
	server := newServer(config{
		runtimeName:     "test",
		adapterBearer:   "adapter-auth-value",
		endpoint:        "http://127.0.0.1/agents/test-agent/endpoint/protocols/openai/responses?api-version=v1",
		foundryAuth:     "foundry-auth-value",
		requestTimeout:  time.Second,
		stateRetention:  time.Minute,
		maxApprovalWait: time.Minute,
	}, &http.Client{Timeout: time.Second})
	turn := &turnState{
		request:           responsesStartTurnRequest("foundry-failed-status"),
		pendingTools:      map[string]string{},
		pendingSince:      map[string]time.Time{},
		bufferedResults:   map[string]harness.ToolCallResult{},
		bufferedPayloads:  map[string]string{},
		submittedPayloads: map[string]string{},
	}
	server.appendFrameLocked(turn, harness.FrameTurnStarted, "foundry hosted response started", nil)
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
		maxApprovalWait:     time.Minute,
		brokeredToolClasses: []harness.BrokeredToolClass{harness.BrokeredToolClassRead},
	}, &http.Client{Timeout: time.Second})
	turn := &turnState{
		request:           brokeredReadRequest("foundry-failed-function-call"),
		pendingTools:      map[string]string{},
		pendingSince:      map[string]time.Time{},
		bufferedResults:   map[string]harness.ToolCallResult{},
		bufferedPayloads:  map[string]string{},
		submittedPayloads: map[string]string{},
	}
	server.appendFrameLocked(turn, harness.FrameTurnStarted, "foundry hosted response started", nil)
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

func TestResponsesFunctionCallWithoutResponseIDFailsBeforeToolRequest(t *testing.T) {
	server := newServer(config{
		runtimeName:         "test",
		adapterBearer:       "adapter-auth-value",
		endpoint:            "http://127.0.0.1/agents/test-agent/endpoint/protocols/openai/responses?api-version=v1",
		foundryAuth:         "foundry-auth-value",
		requestTimeout:      time.Second,
		stateRetention:      time.Minute,
		maxApprovalWait:     time.Minute,
		brokeredToolClasses: []harness.BrokeredToolClass{harness.BrokeredToolClassRead},
	}, &http.Client{Timeout: time.Second})
	turn := &turnState{
		request:           brokeredReadRequest("foundry-missing-id"),
		pendingTools:      map[string]string{},
		pendingSince:      map[string]time.Time{},
		bufferedResults:   map[string]harness.ToolCallResult{},
		bufferedPayloads:  map[string]string{},
		submittedPayloads: map[string]string{},
	}
	server.appendFrameLocked(turn, harness.FrameTurnStarted, "foundry hosted response started", nil)
	server.handleResponsesResponse(turn, responsesResponse{
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
			case "malformed_arguments":
				writeJSON(
					w,
					map[string]any{
						"id": "resp-1",
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
	return map[string]any{"id": "resp-1", "agent_session_id": fakeSessionID, "output": []any{
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
	s := newServer(config{
		addr:                ":0",
		runtimeName:         "foundry-responses-test",
		adapterBearer:       "adapter-auth-value",
		endpoint:            endpoint,
		foundryAuth:         "foundry-auth-value",
		apiVersion:          "v1",
		requestTimeout:      time.Second,
		stateRetention:      time.Minute,
		maxApprovalWait:     time.Minute,
		brokeredToolClasses: append([]harness.BrokeredToolClass(nil), classes...),
	}, &http.Client{Timeout: time.Second})
	adapter := httptest.NewServer(s.handler())
	t.Cleanup(adapter.Close)
	return adapter, s
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

func streamAllFrames(
	t *testing.T,
	client *harness.Client,
	turnID harness.HarnessTurnID,
) []harness.HarnessEventFrame {
	t.Helper()
	var frames []harness.HarnessEventFrame
	if err := client.StreamFrames(context.Background(), turnID, 0, func(frame harness.HarnessEventFrame) error {
		frames = append(frames, frame)
		return nil
	}); err != nil {
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
