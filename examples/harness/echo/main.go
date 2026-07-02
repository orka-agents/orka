package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/orka-agents/orka/internal/harness"
)

const (
	behaviorSuccess              = "success"
	behaviorReadTool             = "read-tool"
	behaviorApprovalTool         = "approval-tool"
	behaviorFailure              = "failure"
	behaviorTimeout              = "timeout"
	behaviorCancellation         = "cancellation"
	remoteRuntimeNameEnv         = "ORKA_REMOTE_HTTP_RUNTIME_NAME"
	remoteRuntimeBearerEnv       = "ORKA_REMOTE_HTTP_RUNTIME_BEARER_TOKEN"
	remoteRuntimeAddrEnv         = "ORKA_REMOTE_HTTP_RUNTIME_ADDR"
	remoteRuntimeScriptEnv       = "ORKA_REMOTE_HTTP_RUNTIME_BEHAVIOR"
	remoteRuntimeReadToolEnv     = "ORKA_REMOTE_HTTP_RUNTIME_READ_TOOL_NAME"
	remoteRuntimeWriteToolEnv    = "ORKA_REMOTE_HTTP_RUNTIME_WRITE_TOOL_NAME"
	remoteRuntimeBrokeredOnlyEnv = "ORKA_REMOTE_HTTP_RUNTIME_BROKERED_ONLY"
	brokeredReadCallID           = "tool-read-1"
	brokeredWriteCallID          = "tool-write-1"
)

type server struct {
	runtimeName string
	bearerValue string
	behavior    string
	mu          sync.Mutex
	turns       map[harness.HarnessTurnID]*turnState
}

type turnState struct {
	request    harness.StartTurnRequest
	cancelled  chan struct{}
	continued  chan struct{}
	onceCancel sync.Once
	onceCont   sync.Once
	results    []harness.ToolCallResult
}

func main() {
	addr := firstNonBlank(os.Getenv(remoteRuntimeAddrEnv), os.Getenv("ORKA_EXAMPLE_HARNESS_ADDR"), ":8090")
	runtimeName := firstNonBlank(
		os.Getenv(remoteRuntimeNameEnv),
		os.Getenv("ORKA_EXAMPLE_HARNESS_RUNTIME_NAME"),
		"orka-generic-http-runtime",
	)
	behavior := normalizeBehavior(firstNonBlank(
		os.Getenv(remoteRuntimeScriptEnv),
		os.Getenv("ORKA_EXAMPLE_HARNESS_BEHAVIOR"),
		behaviorSuccess,
	))
	s := &server{
		runtimeName: runtimeName,
		bearerValue: strings.TrimSpace(firstNonBlank(
			os.Getenv(remoteRuntimeBearerEnv),
			os.Getenv("ORKA_EXAMPLE_HARNESS_BEARER_TOKEN"),
		)),
		behavior: behavior,
		turns:    map[harness.HarnessTurnID]*turnState{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc(harness.HealthPath, s.health)
	mux.HandleFunc(harness.CapabilitiesPath, s.capabilities)
	mux.HandleFunc(harness.TurnsPath, s.startTurn)
	mux.HandleFunc(harness.TurnsPath+"/", s.turn)
	mux.HandleFunc("/lookup", s.supportLookup)
	log.Printf("generic HTTP AgentRuntime fixture listening on %s (runtime=%s behavior=%s)", addr, runtimeName, behavior)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func (s *server) supportLookup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		harness.WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	incident, _ := body["incident"].(string)
	if strings.TrimSpace(incident) == "" {
		incident = "unknown"
	}
	harness.WriteJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"data": map[string]any{
			"incident": incident,
			"status":   "investigating",
			"source":   "mock-support-tool",
		},
	})
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func envBool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func normalizeBehavior(value string) string {
	switch strings.TrimSpace(value) {
	case behaviorReadTool, behaviorApprovalTool, behaviorFailure, behaviorTimeout, behaviorCancellation:
		return strings.TrimSpace(value)
	default:
		return behaviorSuccess
	}
}

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		harness.WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	harness.WriteJSON(w, http.StatusOK, harness.HealthResponse{
		Version:   harness.ProtocolVersion,
		Status:    harness.HealthStatusOK,
		Ready:     true,
		CheckedAt: time.Now().UTC(),
		Metadata: map[string]string{
			"runtime": s.runtimeName,
			"backend": "generic-http",
		},
	})
}

func (s *server) capabilities(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		harness.WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	modes := []harness.ToolExecutionMode{harness.ToolExecutionModeObserved}
	classes := []harness.BrokeredToolClass(nil)
	brokeredOnly := envBool(remoteRuntimeBrokeredOnlyEnv)
	if brokeredOnly {
		modes = nil
	}
	if s.behavior == behaviorReadTool {
		modes = append(modes, harness.ToolExecutionModeBrokered)
		classes = append(classes, harness.BrokeredToolClassRead)
	}
	if s.behavior == behaviorApprovalTool {
		modes = append(modes, harness.ToolExecutionModeBrokered)
		classes = append(classes, harness.BrokeredToolClassWrite)
	}
	harness.WriteJSON(w, http.StatusOK, harness.CapabilitiesResponse{
		Version:                 harness.ProtocolVersion,
		ProtocolVersion:         harness.ProtocolVersion,
		Transport:               harness.HTTPTransport,
		RuntimeName:             s.runtimeName,
		RuntimeVersion:          "generic-http-fixture",
		ProviderKind:            harness.ProviderKindRemote,
		ToolExecutionModes:      modes,
		BrokeredToolClasses:     classes,
		SupportsCancel:          true,
		SupportsRuntimeSessions: true,
		SupportsContinuation:    s.behavior == behaviorReadTool || s.behavior == behaviorApprovalTool,
		SupportsArtifacts:       true,
		MaxConcurrentTurns:      1,
		MaxTurnSeconds:          600,
		MaxOutputBytes:          1 << 20,
		Metadata: map[string]string{
			"backend":  "generic-http",
			"behavior": s.behavior,
		},
	})
}

func (s *server) authorized(w http.ResponseWriter, r *http.Request) bool {
	if strings.TrimSpace(s.bearerValue) == "" {
		return true
	}
	got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.bearerValue)) != 1 {
		harness.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return false
	}
	return true
}

func (s *server) startTurn(w http.ResponseWriter, r *http.Request) {
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
	turn := &turnState{request: request, cancelled: make(chan struct{}), continued: make(chan struct{})}
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

func (s *server) turn(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	turnID, resource, err := harness.ParseTurnResourcePath(r.URL.EscapedPath())
	if err != nil {
		harness.WriteError(w, http.StatusNotFound, "not found")
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
	case harness.TurnResourceEvents:
		if r.Method != http.MethodGet {
			harness.WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.streamEvents(w, r, turn)
	case harness.TurnResourceContinue:
		if r.Method != http.MethodPost {
			harness.WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.continueTurn(w, r, turn)
	case harness.TurnResourceCancel:
		s.cancelTurn(w, r, turn)
	default:
		harness.WriteError(w, http.StatusNotFound, "not found")
	}
}

func (s *server) streamEvents(w http.ResponseWriter, r *http.Request, turn *turnState) {
	afterSeq := int64(0)
	if raw := strings.TrimSpace(r.URL.Query().Get("afterSeq")); raw != "" {
		_, _ = fmt.Sscanf(raw, "%d", &afterSeq)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	write := func(frame harness.HarnessEventFrame) bool {
		if frame.Seq <= afterSeq {
			return true
		}
		return harness.WriteSSEFrame(w, frame) == nil
	}
	frames := s.initialFrames(turn)
	for _, frame := range frames {
		if !write(frame) {
			return
		}
	}
	if turn.request.ToolExecutionMode != harness.ToolExecutionModeBrokered &&
		(s.behavior == behaviorReadTool || s.behavior == behaviorApprovalTool) {
		_ = harness.WriteSSEDone(w)
		return
	}
	switch s.behavior {
	case behaviorReadTool:
		if afterSeq >= 5 {
			_ = harness.WriteSSEDone(w)
			return
		}
		select {
		case <-turn.continued:
			for _, frame := range s.continuedReadFrames(turn) {
				if !write(frame) {
					return
				}
			}
			_ = harness.WriteSSEDone(w)
		case <-turn.cancelled:
			_ = write(frame(turn.request, 4, harness.FrameTurnCancelled, "turn cancelled", nil))
			_ = harness.WriteSSEDone(w)
		case <-r.Context().Done():
			return
		}
	case behaviorApprovalTool:
		if afterSeq >= 6 {
			_ = harness.WriteSSEDone(w)
			return
		}
		select {
		case <-turn.continued:
			for _, frame := range s.continuedFrames(turn) {
				if !write(frame) {
					return
				}
			}
			_ = harness.WriteSSEDone(w)
		case <-turn.cancelled:
			_ = write(frame(turn.request, 5, harness.FrameTurnCancelled, "turn cancelled", nil))
			_ = harness.WriteSSEDone(w)
		case <-r.Context().Done():
			return
		}
	case behaviorCancellation:
		select {
		case <-turn.cancelled:
			_ = write(frame(turn.request, 2, harness.FrameTurnCancelled, "turn cancelled", nil))
			_ = harness.WriteSSEDone(w)
		case <-r.Context().Done():
			return
		}
	default:
		_ = harness.WriteSSEDone(w)
	}
}

func (s *server) initialFrames(turn *turnState) []harness.HarnessEventFrame {
	request := turn.request
	start := frame(request, 1, harness.FrameTurnStarted, "turn started", nil)
	switch s.behavior {
	case behaviorFailure:
		failed := frame(request, 2, harness.FrameTurnFailed, "turn failed", nil)
		failed.Failed = &harness.TurnFailed{Reason: "simulated_failure", Message: "generic HTTP runtime simulated failure"}
		failed.Error = &harness.ErrorInfo{Code: "simulated_failure", Message: "generic HTTP runtime simulated failure"}
		return []harness.HarnessEventFrame{start, failed}
	case behaviorTimeout:
		failed := frame(request, 2, harness.FrameTurnFailed, "turn timeout", nil)
		failed.Failed = &harness.TurnFailed{
			Reason:    "timeout",
			Message:   "generic HTTP runtime simulated timeout",
			Retryable: true,
		}
		failed.Error = &harness.ErrorInfo{Code: "timeout", Message: "generic HTTP runtime simulated timeout", Retryable: true}
		return []harness.HarnessEventFrame{start, failed}
	case behaviorCancellation:
		return []harness.HarnessEventFrame{start}
	case behaviorReadTool:
		if request.ToolExecutionMode != harness.ToolExecutionModeBrokered {
			return observedSuccessFrames(request, start)
		}
		output := runtimeOutput(request, 2, "generic HTTP runtime requesting read-only tool")
		toolName := brokeredToolNameForRequest(request, defaultReadToolName())
		if !requestIncludesBrokeredToolSchema(request, toolName) {
			failed := missingToolSchemaFrame(request, 3)
			return []harness.HarnessEventFrame{start, output, failed}
		}
		tool := toolRequested(request, 3, toolName, brokeredReadCallID, `{"incident":"quincy-north"}`)
		return []harness.HarnessEventFrame{start, output, tool}
	case behaviorApprovalTool:
		if request.ToolExecutionMode != harness.ToolExecutionModeBrokered {
			return observedSuccessFrames(request, start)
		}
		output := runtimeOutput(request, 2, "generic HTTP runtime requesting approval-gated tool")
		toolName := brokeredToolNameForRequest(request, defaultWriteToolName())
		if !requestIncludesBrokeredToolSchema(request, toolName) {
			failed := missingToolSchemaFrame(request, 3)
			return []harness.HarnessEventFrame{start, output, failed}
		}

		tool := toolRequested(
			request,
			3,
			toolName,
			brokeredWriteCallID,
			`{"incident":"quincy-north","action":"dispatch technician"}`,
		)
		waiting := frame(request, 4, harness.FrameRuntimeLog, "waiting for Orka brokered approval", nil)
		waiting.Content = json.RawMessage(fmt.Sprintf(`{"status":"waiting_for_orka_approval","targetTool":%q}`, toolName))
		return []harness.HarnessEventFrame{start, output, tool, waiting}
	default:
		return observedSuccessFrames(request, start)
	}
}

func missingToolSchemaFrame(request harness.StartTurnRequest, seq int64) harness.HarnessEventFrame {
	const message = "brokered tool schema was not supplied by Orka"
	failed := frame(request, seq, harness.FrameTurnFailed, "brokered tool schema missing", nil)
	failed.Failed = &harness.TurnFailed{Reason: "missing_tool_schema", Message: message}
	failed.Error = &harness.ErrorInfo{Code: "missing_tool_schema", Message: message}
	return failed
}

func requestIncludesBrokeredToolSchema(request harness.StartTurnRequest, name string) bool {
	name = strings.TrimSpace(name)
	for _, definition := range request.Input.Tools {
		if strings.TrimSpace(definition.Name) == name && definition.BrokeredClass != "" {
			return true
		}
	}
	return false
}

func brokeredToolNameForRequest(request harness.StartTurnRequest, fallback string) string {
	if class := strings.TrimSpace(request.Metadata["brokeredToolClass"]); class != "" {
		return "conformance_" + class
	}
	return fallback
}

func defaultReadToolName() string {
	return firstNonBlank(os.Getenv(remoteRuntimeReadToolEnv), "read_incident")
}

func defaultWriteToolName() string {
	return firstNonBlank(os.Getenv(remoteRuntimeWriteToolEnv), "dispatch_work_order")
}

func observedSuccessFrames(
	request harness.StartTurnRequest,
	start harness.HarnessEventFrame,
) []harness.HarnessEventFrame {
	output := runtimeOutput(request, 2, "echo: "+request.Input.Prompt)
	completed := frame(request, 3, harness.FrameTurnCompleted, "turn completed", &harness.TurnCompleted{
		Result:        "ok",
		FinalEventSeq: 3,
	})
	return []harness.HarnessEventFrame{start, output, completed}
}

func (s *server) continuedReadFrames(turn *turnState) []harness.HarnessEventFrame {
	request := turn.request
	result := frame(request, 4, harness.FrameToolResultReceived, "brokered read-only tool result received", nil)
	result.ToolName = brokeredToolNameForRequest(request, defaultReadToolName())
	result.ToolCallID = brokeredReadCallID
	if len(turn.results) > 0 {
		if turn.results[0].Error != nil {
			result.Error = turn.results[0].Error
			encoded, _ := json.Marshal(map[string]any{"error": turn.results[0].Error})
			result.Content = encoded
		} else {
			result.Content = turn.results[0].Output
		}
	}
	completed := frame(request, 5, harness.FrameTurnCompleted, "turn completed", &harness.TurnCompleted{
		Result:        "read-only investigation complete",
		FinalEventSeq: 5,
	})
	return []harness.HarnessEventFrame{result, completed}
}

func (s *server) continuedFrames(turn *turnState) []harness.HarnessEventFrame {
	request := turn.request
	result := frame(request, 5, harness.FrameToolResultReceived, "brokered approval-gated tool result received", nil)
	result.ToolName = brokeredToolNameForRequest(request, defaultWriteToolName())
	result.ToolCallID = brokeredWriteCallID
	if len(turn.results) > 0 {
		if turn.results[0].Error != nil {
			result.Error = turn.results[0].Error
			encoded, _ := json.Marshal(map[string]any{"error": turn.results[0].Error})
			result.Content = encoded
		} else {
			result.Content = turn.results[0].Output
		}
	} else {
		result.Error = &harness.ErrorInfo{Code: "missing_tool_result", Message: "continue request had no tool result"}
		result.Content = json.RawMessage(`{"error":{"code":"missing_tool_result"}}`)
	}
	completed := frame(request, 6, harness.FrameTurnCompleted, "turn completed", &harness.TurnCompleted{
		Result:        "approval-gated action completed",
		FinalEventSeq: 6,
	})
	return []harness.HarnessEventFrame{result, completed}
}

func runtimeOutput(request harness.StartTurnRequest, seq int64, text string) harness.HarnessEventFrame {
	output := frame(request, seq, harness.FrameRuntimeOutput, "runtime output", nil)
	output.ContentText = text
	output.Content = json.RawMessage(fmt.Sprintf(`{"message":%q}`, text))
	return output
}

func toolRequested(
	request harness.StartTurnRequest,
	seq int64,
	name string,
	toolCallID string,
	args string,
) harness.HarnessEventFrame {
	tool := frame(request, seq, harness.FrameToolCallRequested, "brokered tool requested", nil)
	tool.ToolName = name
	tool.ToolCallID = toolCallID
	tool.Content = json.RawMessage(args)
	return tool
}

func (s *server) continueTurn(w http.ResponseWriter, r *http.Request, turn *turnState) {
	var request harness.ContinueTurnRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		harness.WriteError(w, http.StatusBadRequest, "invalid JSON request")
		return
	}
	if err := request.Validate(); err != nil {
		harness.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if request.RuntimeSessionID != turn.request.RuntimeSessionID ||
		request.TurnID != turn.request.TurnID ||
		request.CorrelationID != turn.request.CorrelationID {
		harness.WriteError(w, http.StatusBadRequest, "continue request does not match started turn")
		return
	}
	turn.results = append([]harness.ToolCallResult(nil), request.ToolResults...)
	turn.onceCont.Do(func() { close(turn.continued) })
	harness.WriteJSON(w, http.StatusAccepted, harness.ContinueTurnResponse{
		Version:          harness.ProtocolVersion,
		Accepted:         true,
		RuntimeSessionID: request.RuntimeSessionID,
		TurnID:           request.TurnID,
		CorrelationID:    request.CorrelationID,
		Message:          "continue accepted",
	})
}

func (s *server) cancelTurn(w http.ResponseWriter, r *http.Request, turn *turnState) {
	if r.Method != http.MethodPost {
		harness.WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var request harness.CancelTurnRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		harness.WriteError(w, http.StatusBadRequest, "invalid JSON request")
		return
	}
	if err := request.Validate(); err != nil {
		harness.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if request.RuntimeSessionID != turn.request.RuntimeSessionID || request.TurnID != turn.request.TurnID {
		harness.WriteError(w, http.StatusBadRequest, "cancel request does not match started turn")
		return
	}
	turn.onceCancel.Do(func() { close(turn.cancelled) })
	harness.WriteJSON(w, http.StatusAccepted, harness.CancelTurnResponse{
		Version:          harness.ProtocolVersion,
		Accepted:         true,
		RuntimeSessionID: request.RuntimeSessionID,
		TurnID:           request.TurnID,
		CorrelationID:    request.CorrelationID,
		Message:          "cancel accepted",
	})
}

func frame(
	request harness.StartTurnRequest,
	seq int64,
	typ harness.FrameType,
	summary string,
	completed *harness.TurnCompleted,
) harness.HarnessEventFrame {
	metadata := map[string]string{"backend": "generic-http"}
	return harness.HarnessEventFrame{
		Version:          harness.ProtocolVersion,
		Type:             typ,
		RuntimeSessionID: request.RuntimeSessionID,
		TurnID:           request.TurnID,
		CorrelationID:    request.CorrelationID,
		Seq:              seq,
		CreatedAt:        time.Now().UTC(),
		Summary:          summary,
		Completed:        completed,
		Metadata:         metadata,
	}
}
