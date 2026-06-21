package harness_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/harness"
	"github.com/sozercan/orka/internal/harness/harnesstest"
	"github.com/sozercan/orka/internal/store"
)

func TestTurnRunnerAppendsMappedEventsForSuccessfulTurn(t *testing.T) {
	runner, request, eventStore, cleanup := newRunner(t, harnesstest.BehaviorSuccess)
	defer cleanup()
	result, err := runner.Run(context.Background(), request)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Completed == nil || result.Failed != nil || result.Cancelled {
		t.Fatalf("result = %#v, want completed", result)
	}
	listed, err := eventStore.ListExecutionEvents(context.Background(), store.ExecutionEventFilter{
		Namespace:  "default",
		StreamID:   "task-a",
		EventTypes: []string{events.ExecutionEventTypeAgentRuntimeCompleted},
	})
	if err != nil {
		t.Fatalf("ListExecutionEvents() error = %v", err)
	}
	if len(listed) != 1 || listed[0].Seq == 0 || listed[0].Type != events.ExecutionEventTypeAgentRuntimeCompleted {
		t.Fatalf("listed = %#v, want one completed event", listed)
	}
}

func TestTurnRunnerDoesNotSendTaskEventCursorAsHarnessFrameCursor(t *testing.T) {
	runner, request, _, cleanup := newRunner(t, harnesstest.BehaviorSuccess)
	defer cleanup()
	request.EventCursor = 99
	result, err := runner.Run(context.Background(), request)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Completed == nil || len(result.Frames) == 0 || result.Frames[0].Seq != 1 {
		t.Fatalf("result = %#v, want complete turn streamed from frame 1", result)
	}
}

func TestTurnRunnerRejectsMismatchedHarnessFrameIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == harness.TurnsPath:
			harness.WriteJSON(w, http.StatusAccepted, harness.StartTurnResponse{
				Version:          harness.ProtocolVersion,
				Accepted:         true,
				RuntimeSessionID: "runtime-a",
				TurnID:           "turn-a",
				CorrelationID:    "corr-a",
			})
		case r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			_ = harness.WriteSSEFrame(w, harness.HarnessEventFrame{
				Version:          harness.ProtocolVersion,
				Type:             harness.FrameTurnStarted,
				RuntimeSessionID: "other-runtime",
				TurnID:           "turn-a",
				CorrelationID:    "corr-a",
				Seq:              1,
			})
			_ = harness.WriteSSEDone(w)
		default:
			harness.WriteError(w, http.StatusNotFound, "not found")
		}
	}))
	defer server.Close()
	client, err := harness.NewClient(server.URL)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	eventStore := store.NewFakeExecutionEventStore()
	runner := harness.TurnRunner{Client: client, EventStore: eventStore, MapContext: harness.EventMapContext{Namespace: "default", TaskName: "task-a"}}
	_, err = runner.Run(context.Background(), harness.StartTurnRequest{
		Version:          harness.ProtocolVersion,
		Namespace:        "default",
		TaskName:         "task-a",
		SessionName:      "session-a",
		RuntimeSessionID: "runtime-a",
		TurnID:           "turn-a",
		CorrelationID:    "corr-a",
		Deadline:         time.Now().UTC().Add(time.Minute),
		AuthIdentity:     harness.AuthIdentity{Subject: "user:test"},
	})
	if err == nil || !strings.Contains(err.Error(), "frame runtime session") {
		t.Fatalf("Run() error = %v, want frame identity mismatch", err)
	}
	listed, listErr := eventStore.ListExecutionEvents(context.Background(), store.ExecutionEventFilter{Namespace: "default", StreamID: "task-a"})
	if listErr != nil {
		t.Fatalf("ListExecutionEvents() error = %v", listErr)
	}
	if len(listed) != 0 {
		t.Fatalf("listed = %#v, want no appended mismatched frames", listed)
	}
}

func TestTurnRunnerRejectsNonMonotonicHarnessFrameSeq(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == harness.TurnsPath:
			harness.WriteJSON(w, http.StatusAccepted, harness.StartTurnResponse{
				Version:          harness.ProtocolVersion,
				Accepted:         true,
				RuntimeSessionID: "runtime-a",
				TurnID:           "turn-a",
				CorrelationID:    "corr-a",
			})
		case r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			for _, seq := range []int64{1, 1} {
				_ = harness.WriteSSEFrame(w, harness.HarnessEventFrame{
					Version:          harness.ProtocolVersion,
					Type:             harness.FrameRuntimeOutput,
					RuntimeSessionID: "runtime-a",
					TurnID:           "turn-a",
					CorrelationID:    "corr-a",
					Seq:              seq,
				})
			}
			_ = harness.WriteSSEDone(w)
		default:
			harness.WriteError(w, http.StatusNotFound, "not found")
		}
	}))
	defer server.Close()
	client, err := harness.NewClient(server.URL)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	runner := harness.TurnRunner{
		Client:     client,
		EventStore: store.NewFakeExecutionEventStore(),
		MapContext: harness.EventMapContext{Namespace: "default", TaskName: "task-a"},
	}
	_, err = runner.Run(context.Background(), harness.StartTurnRequest{
		Version:          harness.ProtocolVersion,
		Namespace:        "default",
		TaskName:         "task-a",
		SessionName:      "session-a",
		RuntimeSessionID: "runtime-a",
		TurnID:           "turn-a",
		CorrelationID:    "corr-a",
		Deadline:         time.Now().UTC().Add(time.Minute),
		AuthIdentity:     harness.AuthIdentity{Subject: "user:test"},
	})
	if err == nil || !strings.Contains(err.Error(), "must be greater than previous") {
		t.Fatalf("Run() error = %v, want non-monotonic seq error", err)
	}
}

func TestTurnRunnerReturnsFailedTurnAfterAppendingFailureEvent(t *testing.T) {
	runner, request, eventStore, cleanup := newRunner(t, harnesstest.BehaviorFailure)
	defer cleanup()
	result, err := runner.Run(context.Background(), request)
	if err == nil || !strings.Contains(err.Error(), "harness turn failed") {
		t.Fatalf("Run() error = %v, want failed turn", err)
	}
	if result.Failed == nil {
		t.Fatalf("result = %#v, want failed payload", result)
	}
	listed, err := eventStore.ListExecutionEvents(context.Background(), store.ExecutionEventFilter{
		Namespace: "default",
		StreamID:  "task-a",
	})
	if err != nil {
		t.Fatalf("ListExecutionEvents() error = %v", err)
	}
	last := listed[len(listed)-1]
	if last.Type != events.ExecutionEventTypeAgentRuntimeFailed || last.Severity != events.ExecutionEventSeverityError {
		t.Fatalf("last event = %#v, want runtime failed", last)
	}
}

func TestTurnRunnerUnknownFrameAppendsDiagnostic(t *testing.T) {
	runner, request, eventStore, cleanup := newRunner(t, harnesstest.BehaviorInvalidFrame)
	defer cleanup()
	_, err := runner.Run(context.Background(), request)
	if err == nil || !strings.Contains(err.Error(), "without terminal frame") {
		t.Fatalf("Run() error = %v, want incomplete turn error", err)
	}
	listed, err := eventStore.ListExecutionEvents(context.Background(), store.ExecutionEventFilter{
		Namespace: "default",
		StreamID:  "task-a",
	})
	if err != nil {
		t.Fatalf("ListExecutionEvents() error = %v", err)
	}
	last := listed[len(listed)-1]
	if last.Severity != events.ExecutionEventSeverityWarning || !strings.Contains(last.Summary, "unknown harness frame") {
		t.Fatalf("last event = %#v, want warning diagnostic", last)
	}
}

func TestTurnRunnerPropagatesAppendFailure(t *testing.T) {
	runner, request, _, cleanup := newRunner(t, harnesstest.BehaviorSuccess)
	defer cleanup()
	runner.EventStore = &failingExecutionEventStore{failAfter: 1, delegate: store.NewFakeExecutionEventStore()}
	_, err := runner.Run(context.Background(), request)
	if err == nil || !strings.Contains(err.Error(), "append mapped harness event") {
		t.Fatalf("Run() error = %v, want append failure", err)
	}
}

func TestTurnRunnerTimeout(t *testing.T) {
	runner, request, _, cleanup := newRunner(t, harnesstest.BehaviorDelayed)
	defer cleanup()
	runner.TurnTimeout = time.Millisecond
	_, err := runner.Run(context.Background(), request)
	if err == nil {
		t.Fatal("Run() error = nil, want timeout")
	}
}

func newRunner(
	t *testing.T,
	behavior harnesstest.FakeBehavior,
) (harness.TurnRunner, harness.StartTurnRequest, *store.FakeExecutionEventStore, func()) {
	t.Helper()
	server := harnesstest.NewFakeHarnessServer(harnesstest.FakeHarnessConfig{Behavior: behavior})
	client, err := harness.NewClient(server.URL())
	if err != nil {
		server.Close()
		t.Fatalf("NewClient() error = %v", err)
	}
	eventStore := store.NewFakeExecutionEventStore()
	request := harness.StartTurnRequest{
		Version:           harness.ProtocolVersion,
		Namespace:         "default",
		TaskName:          "task-a",
		SessionName:       "session-a",
		RuntimeSessionID:  "runtime-a",
		TurnID:            harness.HarnessTurnID("turn-" + string(behavior)),
		CorrelationID:     "corr-a",
		Deadline:          time.Now().UTC().Add(time.Minute),
		AuthIdentity:      harness.AuthIdentity{Subject: "user:test"},
		Input:             harness.TurnInput{Prompt: "hello"},
		ToolExecutionMode: harness.ToolExecutionModeObserved,
	}
	runner := harness.TurnRunner{
		Client:     client,
		EventStore: eventStore,
		MapContext: harness.EventMapContext{
			Namespace:   "default",
			TaskName:    "task-a",
			SessionName: "session-a",
			AgentName:   "agent-a",
		},
	}
	return runner, request, eventStore, server.Close
}

type failingExecutionEventStore struct {
	failAfter int
	count     int
	delegate  store.ExecutionEventStore
}

func (s *failingExecutionEventStore) AppendExecutionEvent(
	ctx context.Context,
	event *store.ExecutionEvent,
) (*store.ExecutionEvent, error) {
	s.count++
	if s.count > s.failAfter {
		return nil, fmt.Errorf("injected append failure")
	}
	return s.delegate.AppendExecutionEvent(ctx, event)
}

func (s *failingExecutionEventStore) ListExecutionEvents(
	ctx context.Context,
	filter store.ExecutionEventFilter,
) ([]store.ExecutionEvent, error) {
	return s.delegate.ListExecutionEvents(ctx, filter)
}

func (s *failingExecutionEventStore) ListSessionExecutionEvents(
	ctx context.Context,
	filter store.SessionExecutionEventFilter,
) ([]store.SessionExecutionEvent, int64, error) {
	return s.delegate.ListSessionExecutionEvents(ctx, filter)
}

func (s *failingExecutionEventStore) GetLatestExecutionEventSeq(
	ctx context.Context,
	namespace string,
	streamType string,
	streamID string,
) (int64, error) {
	return s.delegate.GetLatestExecutionEventSeq(ctx, namespace, streamType, streamID)
}

func (s *failingExecutionEventStore) DeleteExecutionEvents(
	ctx context.Context,
	namespace string,
	streamType string,
	streamID string,
) error {
	return s.delegate.DeleteExecutionEvents(ctx, namespace, streamType, streamID)
}
