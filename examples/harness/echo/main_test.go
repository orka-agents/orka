package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/orka/internal/harness"
	"github.com/sozercan/orka/internal/harness/conformance"
)

func TestEchoHarnessCancelEndpointMatchesCapabilities(t *testing.T) {
	s := newTestServer(behaviorSuccess)
	srv := httptest.NewServer(s.handler())
	defer srv.Close()
	client, err := harness.NewClient(srv.URL)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	ctx := context.Background()
	caps, err := client.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities() error = %v", err)
	}
	if !caps.SupportsCancel {
		t.Fatal("generic HTTP runtime should advertise cancellation")
	}
	request := validStartTurnRequest()
	if _, err := client.StartTurn(ctx, request); err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}
	cancelled, err := client.CancelTurn(ctx, harness.CancelTurnRequest{
		Version:          harness.ProtocolVersion,
		Namespace:        request.Namespace,
		TaskName:         request.TaskName,
		SessionName:      request.SessionName,
		RuntimeSessionID: request.RuntimeSessionID,
		TurnID:           request.TurnID,
		CorrelationID:    request.CorrelationID,
		Reason:           "test",
	})
	if err != nil {
		t.Fatalf("CancelTurn() error = %v", err)
	}
	if !cancelled.Accepted ||
		cancelled.TurnID != request.TurnID ||
		cancelled.RuntimeSessionID != request.RuntimeSessionID {
		t.Fatalf("CancelTurn() = %#v", cancelled)
	}
}

func TestEchoHarnessRejectsDuplicateStartTurn(t *testing.T) {
	s := newTestServer(behaviorSuccess)
	srv := httptest.NewServer(s.handler())
	defer srv.Close()
	client, err := harness.NewClient(srv.URL)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	request := validStartTurnRequest()
	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("first StartTurn() error = %v", err)
	}
	_, err = client.StartTurn(context.Background(), request)
	if err == nil || !strings.Contains(err.Error(), "turn already exists") {
		t.Fatalf("second StartTurn() error = %v, want duplicate rejection", err)
	}
}

func TestSupportLookupEndpoint(t *testing.T) {
	s := newTestServer(behaviorSuccess)
	srv := httptest.NewServer(s.handler())
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/lookup", "application/json", bytes.NewBufferString(`{"incident":"case-1"}`))
	if err != nil {
		t.Fatalf("POST /lookup: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["success"] != true {
		t.Fatalf("body = %#v", body)
	}
}

func TestGenericHTTPRuntimeApprovalContinuation(t *testing.T) {
	s := newTestServer(behaviorApprovalTool)
	srv := httptest.NewServer(s.handler())
	defer srv.Close()
	client, err := harness.NewClient(srv.URL)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	request := validStartTurnRequest()
	request.ToolExecutionMode = harness.ToolExecutionModeBrokered
	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}
	firstCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	var firstFrames []harness.HarnessEventFrame
	_ = client.StreamFrames(firstCtx, request.TurnID, 0, func(frame harness.HarnessEventFrame) error {
		firstFrames = append(firstFrames, frame)
		return nil
	})
	if len(firstFrames) != 4 || firstFrames[len(firstFrames)-1].Type != harness.FrameApprovalRequested {
		t.Fatalf("first frames = %#v, want approval-pending frame", firstFrames)
	}
	continued, err := client.ContinueTurn(context.Background(), harness.ContinueTurnRequest{
		Version:          harness.ProtocolVersion,
		Namespace:        request.Namespace,
		TaskName:         request.TaskName,
		SessionName:      request.SessionName,
		RuntimeSessionID: request.RuntimeSessionID,
		TurnID:           request.TurnID,
		CorrelationID:    request.CorrelationID,
		ToolResults: []harness.ToolCallResult{{
			Version:          harness.ProtocolVersion,
			RuntimeSessionID: request.RuntimeSessionID,
			TurnID:           request.TurnID,
			ToolCallID:       "tool-write-1",
			IdempotencyKey:   harness.ToolRequestIdempotencyKey(request.RuntimeSessionID, request.TurnID, "tool-write-1"),
			Approved:         true,
			Output:           json.RawMessage(`{"success":true,"data":{"dispatched":true}}`),
		}},
	})
	if err != nil {
		t.Fatalf("ContinueTurn() error = %v", err)
	}
	if !continued.Accepted {
		t.Fatalf("ContinueTurn() = %#v, want accepted", continued)
	}
	var finalFrames []harness.HarnessEventFrame
	if err := client.StreamFrames(context.Background(), request.TurnID, 4, func(frame harness.HarnessEventFrame) error {
		finalFrames = append(finalFrames, frame)
		return nil
	}); err != nil {
		t.Fatalf("StreamFrames(after continue) error = %v", err)
	}
	if len(finalFrames) != 2 ||
		finalFrames[0].Type != harness.FrameToolResultReceived ||
		finalFrames[1].Type != harness.FrameTurnCompleted {
		t.Fatalf("final frames = %#v, want tool result then completion", finalFrames)
	}
}

func TestGenericHTTPRuntimePassesBrokeredReadConformance(t *testing.T) {
	s := newTestServer(behaviorReadTool)
	srv := httptest.NewServer(s.handler())
	defer srv.Close()
	observed := conformance.Check(context.Background(), conformance.Target{BaseURL: srv.URL, ProbeTurn: true})
	if !observed.Passed {
		t.Fatalf("observed conformance failed: %v", observed.Failures)
	}
	result := conformance.Check(context.Background(), conformance.Target{BaseURL: srv.URL, ProbeBrokeredRead: true})
	if !result.Passed {
		t.Fatalf("conformance failed: %v", result.Failures)
	}
}

func TestGenericHTTPRuntimePassesBrokeredWriteConformance(t *testing.T) {
	s := newTestServer(behaviorApprovalTool)
	srv := httptest.NewServer(s.handler())
	defer srv.Close()
	observed := conformance.Check(context.Background(), conformance.Target{BaseURL: srv.URL, ProbeTurn: true})
	if !observed.Passed {
		t.Fatalf("observed conformance failed: %v", observed.Failures)
	}
	result := conformance.Check(context.Background(), conformance.Target{BaseURL: srv.URL, ProbeBrokeredWrite: true})
	if !result.Passed {
		t.Fatalf("conformance failed: %v", result.Failures)
	}
}

func newTestServer(behavior string) *server {
	return &server{
		runtimeName: "orka-generic-http-runtime",
		behavior:    normalizeBehavior(behavior),
		turns:       map[harness.HarnessTurnID]*turnState{},
	}
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(harness.HealthPath, s.health)
	mux.HandleFunc(harness.CapabilitiesPath, s.capabilities)
	mux.HandleFunc(harness.TurnsPath, s.startTurn)
	mux.HandleFunc(harness.TurnsPath+"/", s.turn)
	mux.HandleFunc("/lookup", s.supportLookup)
	return mux
}

func validStartTurnRequest() harness.StartTurnRequest {
	return harness.StartTurnRequest{
		Version:          harness.ProtocolVersion,
		Namespace:        "default",
		TaskName:         "task",
		SessionName:      "session",
		RuntimeSessionID: "runtime",
		TurnID:           "turn",
		CorrelationID:    "corr",
		Deadline:         time.Now().UTC().Add(time.Minute),
		AuthIdentity:     harness.AuthIdentity{Subject: "user:test"},
		Input:            harness.TurnInput{Prompt: "hello"},
	}
}
