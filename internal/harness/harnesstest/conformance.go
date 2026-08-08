package harnesstest

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/harness"
)

type HarnessFactory func(t *testing.T, behavior FakeBehavior) (baseURL string, cleanup func())

func RunHarnessConformance(t *testing.T, factory HarnessFactory) {
	t.Helper()
	t.Run("health", func(t *testing.T) { assertHealth(t, factory) })
	t.Run("capabilities", func(t *testing.T) { assertCapabilities(t, factory) })
	t.Run("successful turn", func(t *testing.T) { assertSuccessfulTurn(t, factory) })
	t.Run("failed turn", func(t *testing.T) { assertFailedTurn(t, factory) })
	t.Run("cancellation", func(t *testing.T) { assertCancellation(t, factory) })
	t.Run("invalid frame becomes diagnostic", func(t *testing.T) { assertInvalidFrameDiagnostic(t, factory) })
	t.Run("redaction", func(t *testing.T) { assertRedaction(t, factory) })
	t.Run("timeout", func(t *testing.T) { assertTimeout(t, factory) })
}

func assertHealth(t *testing.T, factory HarnessFactory) {
	t.Helper()
	client := newClientForBehavior(t, factory, BehaviorSuccess)
	resp, err := client.Health(context.Background())
	if err != nil {
		t.Fatalf("Health() error = %v", err)
	}
	if !resp.Ready || resp.Status != harness.HealthStatusOK {
		t.Fatalf("Health() = %#v, want ready ok", resp)
	}
}

func assertCapabilities(t *testing.T, factory HarnessFactory) {
	t.Helper()
	client := newClientForBehavior(t, factory, BehaviorSuccess)
	resp, err := client.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities() error = %v", err)
	}
	if !resp.SupportsCancel || !resp.SupportsRuntimeSessions {
		t.Fatalf("Capabilities() = %#v, want cancel/runtime sessions", resp)
	}
}

func assertSuccessfulTurn(t *testing.T, factory HarnessFactory) {
	t.Helper()
	client := newClientForBehavior(t, factory, BehaviorSuccess)
	request := validStartTurnRequest()
	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}
	frames := collectFrames(t, client, request.TurnID)
	want := []harness.FrameType{
		harness.FrameTurnStarted,
		harness.FrameRuntimeOutput,
		harness.FrameToolCallRequested,
		harness.FrameToolResultReceived,
		harness.FrameTurnCompleted,
	}
	if len(frames) != len(want) {
		t.Fatalf("frames = %#v, want %d frames", frames, len(want))
	}
	for i, frame := range frames {
		if frame.Type != want[i] {
			t.Fatalf("frame[%d].Type = %s, want %s", i, frame.Type, want[i])
		}
		if _, err := harness.MapFrameToExecutionEvent(frame, eventMapContext()); err != nil {
			t.Fatalf("MapFrameToExecutionEvent(%s) error = %v", frame.Type, err)
		}
	}
}

func assertFailedTurn(t *testing.T, factory HarnessFactory) {
	t.Helper()
	client := newClientForBehavior(t, factory, BehaviorFailure)
	request := validStartTurnRequest()
	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}
	frames := collectFrames(t, client, request.TurnID)
	last := frames[len(frames)-1]
	if last.Type != harness.FrameTurnFailed || last.Failed == nil {
		t.Fatalf("last frame = %#v, want failed", last)
	}
	event, err := harness.MapFrameToExecutionEvent(last, eventMapContext())
	if err != nil {
		t.Fatalf("MapFrameToExecutionEvent() error = %v", err)
	}
	if event.Type != events.ExecutionEventTypeAgentRuntimeFailed || event.Severity != events.ExecutionEventSeverityError {
		t.Fatalf("mapped event = %#v, want runtime failed error", event)
	}
}

func assertCancellation(t *testing.T, factory HarnessFactory) {
	t.Helper()
	client := newClientForBehavior(t, factory, BehaviorCancellation)
	request := validStartTurnRequest()
	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	framesCh := make(chan []harness.HarnessEventFrame, 1)
	errCh := make(chan error, 1)
	go streamFramesForCancellation(ctx, client, request.TurnID, framesCh, errCh)
	time.Sleep(20 * time.Millisecond)
	if _, err := client.CancelTurn(context.Background(), validCancelTurnRequest(request)); err != nil {
		t.Fatalf("CancelTurn() error = %v", err)
	}
	var frames []harness.HarnessEventFrame
	select {
	case frames = <-framesCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for cancellation stream")
	}
	if err := <-errCh; err != nil {
		t.Fatalf("StreamFrames() error = %v", err)
	}
	if len(frames) < 2 || frames[len(frames)-1].Type != harness.FrameTurnCancelled {
		t.Fatalf("frames = %#v, want cancellation terminal frame", frames)
	}
}

func streamFramesForCancellation(
	ctx context.Context,
	client *harness.Client,
	turnID harness.HarnessTurnID,
	framesCh chan<- []harness.HarnessEventFrame,
	errCh chan<- error,
) {
	frames := []harness.HarnessEventFrame{}
	err := client.StreamFrames(ctx, turnID, 0, func(frame harness.HarnessEventFrame) error {
		frames = append(frames, frame)
		return nil
	})
	framesCh <- frames
	errCh <- err
}

func assertInvalidFrameDiagnostic(t *testing.T, factory HarnessFactory) {
	t.Helper()
	client := newClientForBehavior(t, factory, BehaviorInvalidFrame)
	request := validStartTurnRequest()
	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}
	frames := collectFrames(t, client, request.TurnID)
	last := frames[len(frames)-1]
	if harness.IsKnownFrameType(last.Type) {
		t.Fatalf("last.Type = %s, want unknown frame", last.Type)
	}
	event, err := harness.MapFrameToExecutionEvent(last, eventMapContext())
	if err != nil {
		t.Fatalf("MapFrameToExecutionEvent() error = %v", err)
	}
	if event.Severity != events.ExecutionEventSeverityWarning || !strings.Contains(event.Summary, "unknown harness frame") {
		t.Fatalf("mapped event = %#v, want warning diagnostic", event)
	}
}

func assertRedaction(t *testing.T, factory HarnessFactory) {
	t.Helper()
	client := newClientForBehavior(t, factory, BehaviorRedactionOutput)
	request := validStartTurnRequest()
	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}
	frames := collectFrames(t, client, request.TurnID)
	var output harness.HarnessEventFrame
	for _, frame := range frames {
		if frame.Type == harness.FrameRuntimeOutput {
			output = frame
		}
	}
	event, err := harness.MapFrameToExecutionEvent(output, eventMapContext())
	if err != nil {
		t.Fatalf("MapFrameToExecutionEvent() error = %v", err)
	}
	encoded := string(event.Content) + event.ContentText + event.Summary
	if strings.Contains(encoded, "sk-test") || strings.Contains(encoded, "bearer-value-for-redaction") ||
		!strings.Contains(encoded, events.ExecutionEventRedactedValue) {
		t.Fatalf("mapped event leaked secret or missed redaction: %s", encoded)
	}
}

func assertTimeout(t *testing.T, factory HarnessFactory) {
	t.Helper()
	baseURL, cleanup := factory(t, BehaviorDelayed)
	defer cleanup()
	client, err := harness.NewClient(baseURL)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	request := validStartTurnRequest()
	if _, err := client.StartTurn(context.Background(), request); err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}
	streamClient, err := harness.NewClient(baseURL, harness.WithHTTPClient(&http.Client{Timeout: 1 * time.Millisecond}))
	if err != nil {
		t.Fatalf("NewClient(stream) error = %v", err)
	}
	err = streamClient.StreamFrames(context.Background(), request.TurnID, 0, func(frame harness.HarnessEventFrame) error {
		return nil
	})
	if err == nil {
		t.Fatal("StreamFrames() error = nil, want client timeout")
	}
}

func newClientForBehavior(t *testing.T, factory HarnessFactory, behavior FakeBehavior) *harness.Client {
	t.Helper()
	baseURL, cleanup := factory(t, behavior)
	t.Cleanup(cleanup)
	client, err := harness.NewClient(baseURL)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	return client
}

func collectFrames(t *testing.T, client *harness.Client, turnID harness.HarnessTurnID) []harness.HarnessEventFrame {
	t.Helper()
	frames := []harness.HarnessEventFrame{}
	if err := client.StreamFrames(context.Background(), turnID, 0, func(frame harness.HarnessEventFrame) error {
		frames = append(frames, frame)
		return nil
	}); err != nil {
		t.Fatalf("StreamFrames() error = %v", err)
	}
	if len(frames) == 0 {
		t.Fatal("StreamFrames() returned no frames")
	}
	return frames
}

func validStartTurnRequest() harness.StartTurnRequest {
	return harness.StartTurnRequest{
		Version:           harness.ProtocolVersion,
		Namespace:         "default",
		TaskName:          "task-a",
		SessionName:       "session-a",
		RuntimeSessionID:  "runtime-a",
		TurnID:            "turn-a",
		CorrelationID:     "corr-a",
		Deadline:          time.Now().UTC().Add(time.Minute),
		AuthIdentity:      harness.AuthIdentity{Subject: "user:test"},
		ToolPolicyRef:     &harness.PolicyRef{Name: "default-tools"},
		Input:             harness.TurnInput{Prompt: "hello"},
		ToolExecutionMode: harness.ToolExecutionModeObserved,
	}
}

func validCancelTurnRequest(start harness.StartTurnRequest) harness.CancelTurnRequest {
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

func eventMapContext() harness.EventMapContext {
	return harness.EventMapContext{Namespace: "default", TaskName: "task-a", SessionName: "session-a", AgentName: "agent-a"}
}
