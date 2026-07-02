package harnesstest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/harness"
)

type FakeBehavior string

const (
	BehaviorSuccess         FakeBehavior = "success"
	BehaviorFailure         FakeBehavior = "failure"
	BehaviorDelayed         FakeBehavior = "delayed-output"
	BehaviorLongRunning     FakeBehavior = "long-running"
	BehaviorCancellation    FakeBehavior = "cancellation"
	BehaviorInvalidFrame    FakeBehavior = "invalid-frame"
	BehaviorRedactionOutput FakeBehavior = "secret-output"
	BehaviorMissingTerminal FakeBehavior = "missing-terminal"
	BehaviorResidentState   FakeBehavior = "resident-state"
	BehaviorStartTurnError  FakeBehavior = "start-turn-error"
	BehaviorUnhealthy       FakeBehavior = "unhealthy"
)

type FakeHarnessConfig struct {
	Behavior        FakeBehavior
	Delay           time.Duration
	RuntimeName     string
	RuntimeVersion  string
	ProtocolVersion string
	AuthToken       string
	ProviderKind    harness.ProviderKind
	RedactionOutput string
	Now             func() time.Time
}

type FakeHarnessServer struct {
	server *httptest.Server
	config FakeHarnessConfig
	now    func() time.Time

	mu           sync.Mutex
	turns        map[harness.HarnessTurnID]*fakeTurn
	runtimeFiles map[harness.RuntimeSessionID]map[string]string
}

type fakeTurn struct {
	request   harness.StartTurnRequest
	cancelled chan struct{}
	once      sync.Once
}

func NewFakeHarnessServer(config FakeHarnessConfig) *FakeHarnessServer {
	if config.Behavior == "" {
		config.Behavior = BehaviorSuccess
	}
	if config.Delay <= 0 {
		config.Delay = 5 * time.Millisecond
	}
	if config.RuntimeName == "" {
		config.RuntimeName = "fake-harness"
	}
	if config.RuntimeVersion == "" {
		config.RuntimeVersion = "test"
	}
	if config.ProtocolVersion == "" {
		config.ProtocolVersion = harness.ProtocolVersion
	}
	if config.ProviderKind == "" {
		config.ProviderKind = harness.ProviderKindKubernetesService
	}
	if config.RedactionOutput == "" {
		config.RedactionOutput = strings.Join([]string{
			"Authorization:", "Bearer", "bearer-value-for-redaction",
			"api" + "_key=" + "sk-" + "test12345678901234567890",
		}, " ")
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	s := &FakeHarnessServer{config: config, now: now, turns: map[harness.HarnessTurnID]*fakeTurn{}, runtimeFiles: map[harness.RuntimeSessionID]map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc(harness.HealthPath, s.handleHealth)
	mux.HandleFunc(harness.CapabilitiesPath, s.handleCapabilities)
	mux.HandleFunc(harness.TurnsPath, s.handleStartTurn)
	mux.HandleFunc(harness.TurnsPath+"/", s.handleTurn)
	s.server = httptest.NewServer(mux)
	return s
}

func (s *FakeHarnessServer) URL() string { return s.server.URL }

func (s *FakeHarnessServer) Close() {
	if s != nil && s.server != nil {
		s.server.Close()
	}
}

// StartTurnRequests returns a snapshot of accepted turn requests in insertion-agnostic order.
func (s *FakeHarnessServer) StartTurnRequests() []harness.StartTurnRequest {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]harness.StartTurnRequest, 0, len(s.turns))
	for _, turn := range s.turns {
		out = append(out, turn.request)
	}
	return out
}

func (s *FakeHarnessServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		harness.WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	status := harness.HealthStatusOK
	ready := true
	if s.config.Behavior == BehaviorUnhealthy {
		status = harness.HealthStatusUnhealthy
		ready = false
	}
	harness.WriteJSON(w, http.StatusOK, harness.HealthResponse{
		Version:   harness.ProtocolVersion,
		Status:    status,
		Ready:     ready,
		CheckedAt: s.now().UTC(),
	})
}

func (s *FakeHarnessServer) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		harness.WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	harness.WriteJSON(w, http.StatusOK, harness.CapabilitiesResponse{
		Version:                 harness.ProtocolVersion,
		ProtocolVersion:         s.config.ProtocolVersion,
		Transport:               harness.HTTPTransport,
		RuntimeName:             s.config.RuntimeName,
		RuntimeVersion:          s.config.RuntimeVersion,
		ProviderKind:            s.config.ProviderKind,
		ToolExecutionModes:      []harness.ToolExecutionMode{harness.ToolExecutionModeObserved, harness.ToolExecutionModeBrokered},
		SupportsCancel:          true,
		SupportsRuntimeSessions: true,
		MaxConcurrentTurns:      1,
	})
}

func (s *FakeHarnessServer) authorized(w http.ResponseWriter, r *http.Request) bool {
	want := strings.TrimSpace(s.config.AuthToken)
	if want == "" {
		return true
	}
	got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if got != want {
		harness.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return false
	}
	return true
}

func (s *FakeHarnessServer) handleStartTurn(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		harness.WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var request harness.StartTurnRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		harness.WriteError(w, http.StatusBadRequest, "invalid JSON request")
		return
	}
	if err := request.Validate(); err != nil {
		harness.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	eventStreamPath, err := harness.EventStreamPath(request.TurnID)
	if err != nil {
		harness.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.config.Behavior == BehaviorStartTurnError {
		harness.WriteError(w, http.StatusServiceUnavailable, "start turn unavailable")
		return
	}
	turn := &fakeTurn{request: request, cancelled: make(chan struct{})}
	s.mu.Lock()
	if _, exists := s.turns[request.TurnID]; exists {
		s.mu.Unlock()
		harness.WriteError(w, http.StatusConflict, "turn already exists")
		return
	}
	s.turns[request.TurnID] = turn
	s.mu.Unlock()
	harness.WriteJSON(w, http.StatusAccepted, harness.StartTurnResponse{
		Version:          harness.ProtocolVersion,
		Accepted:         true,
		RuntimeSessionID: request.RuntimeSessionID,
		TurnID:           request.TurnID,
		CorrelationID:    request.CorrelationID,
		EventStreamPath:  eventStreamPath,
	})
}

func (s *FakeHarnessServer) handleTurn(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	turnID, resource, err := harness.ParseTurnResourcePath(r.URL.EscapedPath())
	if err != nil {
		if errors.Is(err, harness.ErrTurnPathNotFound) {
			harness.WriteError(w, http.StatusNotFound, "not found")
		} else {
			harness.WriteError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	s.mu.Lock()
	turn := s.turns[turnID]
	s.mu.Unlock()
	if turn == nil {
		harness.WriteError(w, http.StatusNotFound, "turn not found")
		return
	}
	switch resource {
	case "events":
		if r.Method != http.MethodGet {
			harness.WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.handleEvents(w, r, turn)
	case "cancel":
		if r.Method != http.MethodPost {
			harness.WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.handleCancel(w, r, turn)
	default:
		harness.WriteError(w, http.StatusNotFound, "not found")
	}
}

func (s *FakeHarnessServer) handleCancel(w http.ResponseWriter, r *http.Request, turn *fakeTurn) {
	var request harness.CancelTurnRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		harness.WriteError(w, http.StatusBadRequest, "invalid JSON request")
		return
	}
	if err := request.Validate(); err != nil {
		harness.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	turn.once.Do(func() { close(turn.cancelled) })
	harness.WriteJSON(w, http.StatusAccepted, harness.CancelTurnResponse{
		Version:          harness.ProtocolVersion,
		Accepted:         true,
		RuntimeSessionID: request.RuntimeSessionID,
		TurnID:           request.TurnID,
		CorrelationID:    request.CorrelationID,
		Message:          "cancel accepted",
	})
}

func (s *FakeHarnessServer) handleEvents(w http.ResponseWriter, r *http.Request, turn *fakeTurn) {
	afterSeq, _ := strconv.ParseInt(r.URL.Query().Get("afterSeq"), 10, 64)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	emit := func(frame harness.HarnessEventFrame) bool {
		if frame.Seq <= afterSeq {
			return true
		}
		if err := harness.WriteSSEFrame(w, frame); err != nil {
			return false
		}
		return true
	}

	frames := s.framesFor(turn)
	for _, frame := range frames {
		if !emit(frame) {
			return
		}
		if s.config.Behavior == BehaviorDelayed {
			if !sleepContext(r.Context(), s.config.Delay) {
				return
			}
		}
	}
	if s.config.Behavior == BehaviorLongRunning || s.config.Behavior == BehaviorCancellation {
		select {
		case <-turn.cancelled:
			_ = emit(s.frame(turn, 2, harness.FrameTurnCancelled, "cancelled", nil))
		case <-r.Context().Done():
			return
		case <-time.After(10 * time.Second):
			_ = emit(s.frame(turn, 2, harness.FrameTurnFailed, "turn timeout", &harness.TurnFailed{Reason: "timeout", Message: "fake long-running turn timed out"}))
		}
	}
	_ = harness.WriteSSEDone(w)
}

func (s *FakeHarnessServer) framesFor(turn *fakeTurn) []harness.HarnessEventFrame {
	start := s.frame(turn, 1, harness.FrameTurnStarted, "turn started", nil)
	switch s.config.Behavior {
	case BehaviorFailure:
		failed := s.frame(turn, 2, harness.FrameTurnFailed, "turn failed", &harness.TurnFailed{Reason: "fake_failure", Message: "simulated failure"})
		failed.Error = &harness.ErrorInfo{Code: "fake_failure", Message: "simulated failure"}
		return []harness.HarnessEventFrame{start, failed}
	case BehaviorInvalidFrame:
		invalid := s.frame(turn, 2, harness.FrameType("BogusFrame"), "bogus", nil)
		return []harness.HarnessEventFrame{start, invalid}
	case BehaviorRedactionOutput:
		output := s.frame(turn, 2, harness.FrameRuntimeOutput, "secret output", nil)
		output.ContentText = s.config.RedactionOutput
		output.Content = json.RawMessage(fmt.Sprintf(`{"message":%q}`, s.config.RedactionOutput))
		completed := s.frame(turn, 3, harness.FrameTurnCompleted, "turn completed", &harness.TurnCompleted{Result: "ok", FinalEventSeq: 3})
		return []harness.HarnessEventFrame{start, output, completed}
	case BehaviorMissingTerminal:
		output := s.frame(turn, 2, harness.FrameRuntimeOutput, "echo", nil)
		output.ContentText = "echo: " + turn.request.Input.Prompt
		return []harness.HarnessEventFrame{start, output}
	case BehaviorLongRunning, BehaviorCancellation:
		return []harness.HarnessEventFrame{start}
	case BehaviorResidentState:
		message := s.applyResidentStatePrompt(turn.request.RuntimeSessionID, turn.request.Input.Prompt)
		output := s.frame(turn, 2, harness.FrameRuntimeOutput, "resident state", nil)
		output.Content = json.RawMessage(fmt.Sprintf(`{"message":%q}`, message))
		output.ContentText = message
		completed := s.frame(turn, 3, harness.FrameTurnCompleted, "turn completed", &harness.TurnCompleted{Result: message, FinalEventSeq: 3, RetainSession: true})
		return []harness.HarnessEventFrame{start, output, completed}
	default:
		output := s.frame(turn, 2, harness.FrameRuntimeOutput, "echo", nil)
		output.Content = json.RawMessage(fmt.Sprintf(`{"message":%q}`, "echo: "+turn.request.Input.Prompt))
		output.ContentText = "echo: " + turn.request.Input.Prompt
		tool := s.frame(turn, 3, harness.FrameToolCallRequested, "tool requested", nil)
		tool.ToolName = "echo"
		tool.ToolCallID = "tool-1"
		tool.Content = json.RawMessage(`{"input":"hello"}`)
		result := s.frame(turn, 4, harness.FrameToolResultReceived, "tool completed", nil)
		result.ToolName = "echo"
		result.ToolCallID = "tool-1"
		result.Content = json.RawMessage(`{"output":"hello"}`)
		completed := s.frame(turn, 5, harness.FrameTurnCompleted, "turn completed", &harness.TurnCompleted{Result: "ok", FinalEventSeq: 5})
		return []harness.HarnessEventFrame{start, output, tool, result, completed}
	}
}

func (s *FakeHarnessServer) applyResidentStatePrompt(runtimeID harness.RuntimeSessionID, prompt string) string {
	fields := strings.Fields(prompt)
	if len(fields) < 2 {
		return "resident noop"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	files := s.runtimeFiles[runtimeID]
	if files == nil {
		files = map[string]string{}
		s.runtimeFiles[runtimeID] = files
	}
	switch fields[0] {
	case "write":
		if len(fields) < 3 {
			return "resident write requires path and content"
		}
		path := fields[1]
		value := strings.Join(fields[2:], " ")
		files[path] = value
		return fmt.Sprintf("wrote %s", path)
	case "read":
		value, ok := files[fields[1]]
		if !ok {
			return fmt.Sprintf("missing %s", fields[1])
		}
		return fmt.Sprintf("read %s: %s", fields[1], value)
	default:
		return "resident noop"
	}
}

func (s *FakeHarnessServer) frame(turn *fakeTurn, seq int64, typ harness.FrameType, summary string, terminal any) harness.HarnessEventFrame {
	frame := harness.HarnessEventFrame{
		Version:          harness.ProtocolVersion,
		Type:             typ,
		RuntimeSessionID: turn.request.RuntimeSessionID,
		TurnID:           turn.request.TurnID,
		CorrelationID:    turn.request.CorrelationID,
		Seq:              seq,
		CreatedAt:        s.now().UTC().Add(time.Duration(seq) * time.Millisecond),
		Severity:         events.ExecutionEventSeverityInfo,
		Summary:          summary,
		Metadata: map[string]string{
			"fakeBehavior": string(s.config.Behavior),
		},
	}
	switch value := terminal.(type) {
	case *harness.TurnCompleted:
		frame.Completed = value
	case *harness.TurnFailed:
		frame.Failed = value
		frame.Severity = events.ExecutionEventSeverityError
	}
	return frame
}

func sleepContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
