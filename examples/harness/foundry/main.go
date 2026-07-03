package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/orka-agents/orka/internal/harness"
)

const (
	defaultAddr            = ":8090"
	defaultAPIVersion      = "v1"
	defaultPollTimeout     = 20 * time.Second
	defaultPollPeriod      = 250 * time.Millisecond
	defaultStateRetention  = 10 * time.Minute
	maxFoundryPollFailures = 3
	maxFoundryOutputBytes  = 1 << 20

	envAddr          = "ORKA_FOUNDRY_ADAPTER_ADDR"
	envRuntimeName   = "ORKA_FOUNDRY_RUNTIME_NAME"
	envAdapterBearer = "ORKA_FOUNDRY_ADAPTER_BEARER_" + "TOKEN"
	envEndpoint      = "ORKA_FOUNDRY_ENDPOINT"
	envFoundryKey    = "ORKA_FOUNDRY_API_" + "KEY"
	envAuthBearer    = "ORKA_FOUNDRY_AUTH_BEARER"
	envAgentID       = "ORKA_FOUNDRY_AGENT_ID"
	envAPIVersion    = "ORKA_FOUNDRY_API_VERSION"
	envPollTimeout   = "ORKA_FOUNDRY_POLL_TIMEOUT"
	envPollInterval  = "ORKA_FOUNDRY_POLL_INTERVAL"
)

type config struct {
	addr          string
	runtimeName   string
	adapterBearer string
	endpoint      string
	foundryKey    string
	authBearer    string
	agentID       string
	apiVersion    string
	pollTimeout   time.Duration
	pollInterval  time.Duration
}

type server struct {
	cfg                config
	client             *http.Client
	mu                 sync.Mutex
	sessionMu          sync.Mutex
	turns              map[harness.HarnessTurnID]*turnState
	runtimeThreads     map[harness.RuntimeSessionID]string
	runtimeThreadSeen  map[harness.RuntimeSessionID]time.Time
	turnMessageThreads map[harness.HarnessTurnID]string
}

type turnState struct {
	request         harness.StartTurnRequest
	threadID        string
	runID           string
	frames          []harness.HarnessEventFrame
	continued       bool
	completed       bool
	pollFailures    int
	initializing    bool
	pollMu          sync.Mutex
	continueMu      sync.Mutex
	pendingTools    map[string]string
	bufferedResults map[string]harness.ToolCallResult
	emittedResults  map[string]struct{}
	submittedTools  map[string]struct{}
	initDone        chan struct{}
	initErr         error
}

type foundryCreateThreadResponse struct {
	ID string `json:"id"`
}

type foundryCreateRunResponse struct {
	ID     string `json:"id"`
	Status string `json:"status,omitempty"`
}
type foundryRun struct {
	ID             string                 `json:"id"`
	Status         string                 `json:"status"`
	RequiredAction *foundryRequiredAction `json:"required_action,omitempty"`
	LastError      *foundryRunError       `json:"last_error,omitempty"`
}
type foundryRunError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

type foundryRequiredAction struct {
	SubmitToolOutputs *foundrySubmitToolOutputs `json:"submit_tool_outputs,omitempty"`
}

type foundrySubmitToolOutputs struct {
	ToolCalls []foundryToolCall `json:"tool_calls,omitempty"`
}

type foundryToolCall struct {
	ID       string              `json:"id"`
	Type     string              `json:"type,omitempty"`
	Function foundryFunctionCall `json:"function"`
}

type foundryFunctionCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type foundryMessagesResponse struct {
	Data []foundryMessage `json:"data"`
}

type foundryMessage struct {
	Role    string `json:"role,omitempty"`
	Content any    `json:"content,omitempty"`
}

type fatalFoundryPollError struct {
	err error
}

func (e fatalFoundryPollError) Error() string { return e.err.Error() }
func (e fatalFoundryPollError) Unwrap() error { return e.err }

func exactlyOneFoundryAuth(cfg config) bool {
	hasKey := strings.TrimSpace(cfg.foundryKey) != ""
	hasBearer := strings.TrimSpace(cfg.authBearer) != ""
	return hasKey != hasBearer
}

func main() {
	cfg := loadConfig()
	s := &server{
		cfg:                cfg,
		client:             &http.Client{Timeout: 30 * time.Second},
		turns:              map[harness.HarnessTurnID]*turnState{},
		runtimeThreads:     map[harness.RuntimeSessionID]string{},
		runtimeThreadSeen:  map[harness.RuntimeSessionID]time.Time{},
		turnMessageThreads: map[harness.HarnessTurnID]string{},
	}
	mux := s.handler()
	log.Printf("Foundry AgentRuntime adapter listening on %s (runtime=%s endpoint=%s)", cfg.addr, cfg.runtimeName, sanitizeEndpoint(cfg.endpoint)) //nolint:lll
	server := &http.Server{
		Addr:              cfg.addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Fatal(server.ListenAndServe())
}

func loadConfig() config {
	return config{
		addr:          firstNonBlank(os.Getenv(envAddr), defaultAddr),
		runtimeName:   firstNonBlank(os.Getenv(envRuntimeName), "foundry-runtime"),
		adapterBearer: strings.TrimSpace(os.Getenv(envAdapterBearer)),
		endpoint:      strings.TrimRight(strings.TrimSpace(os.Getenv(envEndpoint)), "/"),
		foundryKey:    strings.TrimSpace(os.Getenv(envFoundryKey)),
		authBearer:    strings.TrimSpace(os.Getenv(envAuthBearer)),
		agentID:       strings.TrimSpace(os.Getenv(envAgentID)),
		apiVersion:    firstNonBlank(os.Getenv(envAPIVersion), defaultAPIVersion),
		pollTimeout:   parseDurationEnv(envPollTimeout, defaultPollTimeout),
		pollInterval:  parseDurationEnv(envPollInterval, defaultPollPeriod),
	}
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(harness.HealthPath, s.health)
	mux.HandleFunc(harness.CapabilitiesPath, s.capabilities)
	mux.HandleFunc(harness.TurnsPath, s.startTurn)
	mux.HandleFunc(harness.TurnsPath+"/", s.turn)
	return mux
}

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		harness.WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ready := s.cfg.adapterBearer != "" &&
		s.cfg.endpoint != "" &&
		s.cfg.agentID != "" &&
		exactlyOneFoundryAuth(s.cfg)
	status := harness.HealthStatusOK
	msg := "ready"
	if !ready {
		status = harness.HealthStatusDegraded
		msg = "adapter bearer, endpoint, agent id, and exactly one Foundry auth mode are required"
	}
	harness.WriteJSON(w, http.StatusOK, harness.HealthResponse{Version: harness.ProtocolVersion, Status: status, Ready: ready, CheckedAt: time.Now().UTC(), Message: msg, Metadata: map[string]string{"backend": "foundry"}}) //nolint:lll
}

func (s *server) capabilities(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		harness.WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	harness.WriteJSON(w, http.StatusOK, harness.CapabilitiesResponse{
		Version: harness.ProtocolVersion, ProtocolVersion: harness.ProtocolVersion, Transport: harness.HTTPTransport,
		RuntimeName: s.cfg.runtimeName, RuntimeVersion: "foundry-adapter", ProviderKind: harness.ProviderKindRemote,
		ToolExecutionModes:  []harness.ToolExecutionMode{harness.ToolExecutionModeObserved, harness.ToolExecutionModeBrokered}, //nolint:lll
		BrokeredToolClasses: []harness.BrokeredToolClass{harness.BrokeredToolClassRead, harness.BrokeredToolClassWrite},
		SupportsCancel:      true, SupportsRuntimeSessions: true, SupportsContinuation: true, SupportsArtifacts: false,
		MaxConcurrentTurns: 1, MaxTurnSeconds: int(s.cfg.pollTimeout.Seconds()), MaxOutputBytes: maxFoundryOutputBytes,
		Metadata: map[string]string{"backend": "foundry"},
	})
}

func (s *server) startTurn(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		harness.WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req harness.StartTurnRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		harness.WriteError(w, http.StatusBadRequest, "invalid JSON request")
		return
	}
	if err := req.Validate(); err != nil {
		harness.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	eventsPath, err := harness.EventStreamPath(req.TurnID)
	if err != nil {
		harness.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.mu.Lock()
	if existing := s.turns[req.TurnID]; existing != nil {
		if existing.initializing {
			s.mu.Unlock()
			harness.WriteError(w, http.StatusConflict, "turn initialization in progress")
			return
		}
		response := startTurnResponse(existing.request, eventsPath)
		if !sameStartTurnRequest(existing.request, req) {
			s.mu.Unlock()
			harness.WriteError(w, http.StatusConflict, "turn already exists")
			return
		}
		s.mu.Unlock()
		harness.WriteJSON(w, http.StatusAccepted, response)
		return
	}
	turn := &turnState{
		request:         req,
		initializing:    true,
		pendingTools:    map[string]string{},
		bufferedResults: map[string]harness.ToolCallResult{},
		emittedResults:  map[string]struct{}{},
		submittedTools:  map[string]struct{}{},
		initDone:        make(chan struct{}),
	}
	s.turns[req.TurnID] = turn
	s.mu.Unlock()
	backendCtx, cancel := context.WithTimeout(context.Background(), s.cfg.pollTimeout)
	defer cancel()
	if err := s.ensureFoundryRun(backendCtx, turn); err != nil {
		s.completeTurnInitialization(turn, err)
		harness.WriteError(w, http.StatusBadGateway, err.Error())
		return
	}
	s.completeTurnInitialization(turn, nil)
	harness.WriteJSON(w, http.StatusAccepted, startTurnResponse(req, eventsPath))
}

func (s *server) completeTurnInitialization(turn *turnState, err error) {
	s.mu.Lock()
	turn.initErr = err
	if err != nil {
		delete(s.turns, turn.request.TurnID)
	}
	close(turn.initDone)
	s.mu.Unlock()
}

func (s *server) waitTurnInitialized(ctx context.Context, w http.ResponseWriter, turn *turnState) bool {
	select {
	case <-turn.initDone:
		if turn.initErr != nil {
			harness.WriteError(w, http.StatusBadGateway, turn.initErr.Error())
			return false
		}
		return true
	case <-ctx.Done():
		return false
	}
}

func sameStartTurnRequest(existing, retry harness.StartTurnRequest) bool {
	return reflect.DeepEqual(existing, retry)
}

func startTurnResponse(req harness.StartTurnRequest, eventsPath string) harness.StartTurnResponse {
	return harness.StartTurnResponse{
		Version:          harness.ProtocolVersion,
		Accepted:         true,
		RuntimeSessionID: req.RuntimeSessionID,
		TurnID:           req.TurnID,
		CorrelationID:    req.CorrelationID,
		EventStreamPath:  eventsPath,
	}
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
	if !s.waitTurnInitialized(r.Context(), w, turn) {
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
	ctx := r.Context()
	afterSeq := parseAfterSeq(r.URL.Query().Get("afterSeq"))
	w.Header().Set("Content-Type", "text/event-stream")
	s.mu.Lock()
	frames := append([]harness.HarnessEventFrame(nil), turn.frames...)
	completed := turn.completed
	s.mu.Unlock()
	lastWritten := afterSeq
	for _, frame := range frames {
		if frame.Seq > afterSeq {
			_ = harness.WriteSSEFrame(w, frame)
			lastWritten = frame.Seq
		}
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	if !completed {
		if err := s.pollFoundry(ctx, turn); err != nil && ctx.Err() == nil {
			var fatal fatalFoundryPollError
			if errors.As(err, &fatal) || s.recordPollFailure(turn) >= maxFoundryPollFailures {
				s.appendFailed(turn, "foundry_poll_failed", err.Error())
			}
		}
		s.mu.Lock()
		frames = append([]harness.HarnessEventFrame(nil), turn.frames...)
		completed = turn.completed
		s.mu.Unlock()
		for _, frame := range frames {
			if frame.Seq > lastWritten {
				_ = harness.WriteSSEFrame(w, frame)
			}
		}
	}
	if completed {
		_ = harness.WriteSSEDone(w)
	}
}

func (s *server) continueTurn(w http.ResponseWriter, r *http.Request, turn *turnState) {
	var req harness.ContinueTurnRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		harness.WriteError(w, http.StatusBadRequest, "invalid JSON request")
		return
	}
	if err := req.Validate(); err != nil {
		harness.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.RuntimeSessionID != turn.request.RuntimeSessionID || req.TurnID != turn.request.TurnID || req.CorrelationID != turn.request.CorrelationID { //nolint:lll
		harness.WriteError(w, http.StatusBadRequest, "continue request does not match started turn")
		return
	}
	turn.continueMu.Lock()
	defer turn.continueMu.Unlock()
	turn.pollMu.Lock()
	defer turn.pollMu.Unlock()
	s.mu.Lock()
	if turn.completed {
		s.mu.Unlock()
		harness.WriteJSON(w, http.StatusAccepted, harness.ContinueTurnResponse{Version: harness.ProtocolVersion, Accepted: true, RuntimeSessionID: req.RuntimeSessionID, TurnID: req.TurnID, CorrelationID: req.CorrelationID, Message: "continue ignored for terminal turn"}) //nolint:lll
		return
	}
	s.mu.Unlock()
	resultsToSubmit, _, err := s.recordContinueResults(turn, req.ToolResults)
	if err != nil {
		harness.WriteError(w, http.StatusConflict, err.Error())
		return
	}
	if len(resultsToSubmit) > 0 {
		if err := s.submitToolOutputs(r.Context(), turn, resultsToSubmit); err != nil {
			harness.WriteError(w, http.StatusBadGateway, err.Error())
			return
		}
	}
	s.mu.Lock()
	turn.continued = true
	for _, result := range resultsToSubmit {
		if _, emitted := turn.emittedResults[result.ToolCallID]; emitted {
			continue
		}
		toolName := turn.pendingTools[result.ToolCallID]
		if toolName == "" {
			toolName = result.ToolCallID
		}
		s.appendFrameLocked(turn, harness.FrameToolResultReceived, "brokered tool result received", func(f *harness.HarnessEventFrame) { //nolint:lll
			f.ToolName = toolName
			f.ToolCallID = result.ToolCallID
			f.Content = result.Output
			f.Error = result.Error
		})
		turn.emittedResults[result.ToolCallID] = struct{}{}
	}
	for _, result := range resultsToSubmit {
		delete(turn.pendingTools, result.ToolCallID)
		delete(turn.bufferedResults, result.ToolCallID)
		turn.submittedTools[result.ToolCallID] = struct{}{}
	}
	s.mu.Unlock()
	harness.WriteJSON(w, http.StatusAccepted, harness.ContinueTurnResponse{Version: harness.ProtocolVersion, Accepted: true, RuntimeSessionID: req.RuntimeSessionID, TurnID: req.TurnID, CorrelationID: req.CorrelationID, Message: "continue accepted"}) //nolint:lll
}

func (s *server) cancelTurn(w http.ResponseWriter, r *http.Request, turn *turnState) {
	if r.Method != http.MethodPost {
		harness.WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	turn.continueMu.Lock()
	defer turn.continueMu.Unlock()
	turn.pollMu.Lock()
	defer turn.pollMu.Unlock()
	s.mu.Lock()
	if turn.completed {
		s.mu.Unlock()
		harness.WriteJSON(w, http.StatusAccepted, harness.CancelTurnResponse{Version: harness.ProtocolVersion, Accepted: true, RuntimeSessionID: turn.request.RuntimeSessionID, TurnID: turn.request.TurnID, CorrelationID: turn.request.CorrelationID}) //nolint:lll
		return
	}
	threadID, runID := turn.threadID, turn.runID
	s.mu.Unlock()
	if threadID != "" && runID != "" {
		if err := s.doJSON(r.Context(), http.MethodPost, fmt.Sprintf("/threads/%s/runs/%s/cancel", url.PathEscape(threadID), url.PathEscape(runID)), nil, nil); err != nil { //nolint:lll
			harness.WriteError(w, http.StatusBadGateway, err.Error())
			return
		}
	}
	s.mu.Lock()
	if !turn.completed {
		s.appendFrameLocked(turn, harness.FrameTurnCancelled, "turn cancelled", nil)
		turn.completed = true
		s.scheduleTurnCleanupLocked(turn)
	}
	s.mu.Unlock()
	harness.WriteJSON(w, http.StatusAccepted, harness.CancelTurnResponse{Version: harness.ProtocolVersion, Accepted: true, RuntimeSessionID: turn.request.RuntimeSessionID, TurnID: turn.request.TurnID, CorrelationID: turn.request.CorrelationID}) //nolint:lll
}

func (s *server) ensureFoundryRun(ctx context.Context, turn *turnState) error {
	s.mu.Lock()
	already := turn.threadID != "" && turn.runID != ""
	s.mu.Unlock()
	if already {
		return nil
	}
	foundryTools := []map[string]any{}
	if turn.request.ToolExecutionMode == harness.ToolExecutionModeBrokered {
		if err := validateFoundryToolDefinitions(turn.request.Input.Tools); err != nil {
			return err
		}
		foundryTools = foundryToolSchemas(turn.request.Input.Tools)
	}
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	s.mu.Lock()
	if turn.threadID != "" && turn.runID != "" {
		s.mu.Unlock()
		return nil
	}
	threadID := s.runtimeThreads[turn.request.RuntimeSessionID]
	s.mu.Unlock()
	newThread := false
	if threadID == "" {
		threadBody := map[string]any{"messages": []map[string]any{{"role": "user", "content": turn.request.Input.Prompt}}}
		var thread foundryCreateThreadResponse
		if err := s.doJSON(ctx, http.MethodPost, "/threads", threadBody, &thread); err != nil {
			return err
		}
		threadID = thread.ID
		newThread = true
	}
	runBody := map[string]any{
		"assistant_id": s.cfg.agentID,
		"metadata":     map[string]string{"orkaTask": turn.request.TaskName, "orkaTurn": string(turn.request.TurnID)},
		"tools":        foundryTools,
	}
	if !newThread {
		runBody["additional_messages"] = []map[string]any{{"role": "user", "content": turn.request.Input.Prompt}}
	}
	var run foundryCreateRunResponse
	if err := s.doJSON(ctx, http.MethodPost, fmt.Sprintf("/threads/%s/runs", url.PathEscape(threadID)), runBody, &run); err != nil { //nolint:lll
		if newThread {
			deletePath := fmt.Sprintf("/threads/%s", url.PathEscape(threadID))
			_ = s.doJSON(context.WithoutCancel(ctx), http.MethodDelete, deletePath, nil, nil)
		}
		return err
	}
	s.mu.Lock()
	if newThread {
		s.runtimeThreads[turn.request.RuntimeSessionID] = threadID
	}
	delete(s.turnMessageThreads, turn.request.TurnID)
	s.runtimeThreadSeen[turn.request.RuntimeSessionID] = time.Now().UTC()
	turn.threadID, turn.runID = threadID, run.ID
	turn.initializing = false
	s.appendFrameLocked(turn, harness.FrameTurnStarted, "foundry run started", nil)
	s.mu.Unlock()
	return nil
}

func (s *server) recordPollFailure(turn *turnState) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	turn.pollFailures++
	return turn.pollFailures
}

func (s *server) resetPollFailures(turn *turnState) {
	s.mu.Lock()
	turn.pollFailures = 0
	s.mu.Unlock()
}

func (s *server) pollFoundry(ctx context.Context, turn *turnState) error {
	turn.pollMu.Lock()
	defer turn.pollMu.Unlock()
	deadline := time.Now().Add(s.cfg.pollTimeout)
	for time.Now().Before(deadline) {
		var run foundryRun
		if err := s.doJSON(ctx, http.MethodGet, fmt.Sprintf("/threads/%s/runs/%s", url.PathEscape(turn.threadID), url.PathEscape(turn.runID)), nil, &run); err != nil { //nolint:lll
			return err
		}
		switch run.Status {
		case "requires_action":
			if run.RequiredAction == nil || run.RequiredAction.SubmitToolOutputs == nil || len(run.RequiredAction.SubmitToolOutputs.ToolCalls) == 0 { //nolint:lll
				return fatalFoundryPollError{err: fmt.Errorf("foundry run requires action without tool calls")}
			}
			s.mu.Lock()
			type pendingFoundryCall struct {
				id   string
				name string
				args json.RawMessage
			}
			newCalls := []pendingFoundryCall{}
			pendingRequests := 0
			for _, call := range run.RequiredAction.SubmitToolOutputs.ToolCalls {
				if call.ID == "" {
					continue
				}
				if !turnAllowsFoundryTool(turn.request, call.Function.Name) {
					s.mu.Unlock()
					return fatalFoundryPollError{
						err: fmt.Errorf("foundry requested tool %q that is not exposed for this turn", call.Function.Name),
					}
				}
				if !foundryToolAllowed(turn.request.Input.Tools, call.Function.Name) {
					s.mu.Unlock()
					return fmt.Errorf("foundry requested unapproved tool %q", call.Function.Name)
				}
				if _, done := turn.submittedTools[call.ID]; done {
					continue
				}
				if _, pending := turn.pendingTools[call.ID]; pending {
					pendingRequests++
					continue
				}
				args, err := normalizeFoundryToolArguments(call.Function.Arguments)
				if err != nil {
					s.mu.Unlock()
					return fatalFoundryPollError{err: err}
				}
				newCalls = append(newCalls, pendingFoundryCall{id: call.ID, name: call.Function.Name, args: args})
			}
			if turn.completed {
				s.mu.Unlock()
				return nil
			}
			for _, call := range newCalls {
				turn.pendingTools[call.id] = call.name
				s.appendFrameLocked(turn, harness.FrameToolCallRequested, "foundry tool call requested", func(f *harness.HarnessEventFrame) { //nolint:lll
					f.ToolName = call.name
					f.ToolCallID = call.id
					f.Content = call.args
				})
			}
			s.mu.Unlock()
			if len(newCalls) > 0 || pendingRequests > 0 {
				s.resetPollFailures(turn)
				return nil
			}
			// Foundry can briefly echo submitted tool calls while the run transitions; keep polling.

		case "completed":
			result, err := s.fetchFinalMessage(ctx, turn)
			if err != nil {
				return err
			}
			if len([]byte(result)) > maxFoundryOutputBytes {
				s.appendFailed(turn, "foundry_output_too_large", "foundry completion exceeded advertised output limit")
				return nil
			}
			s.mu.Lock()
			turn.pollFailures = 0
			if !turn.completed {
				s.appendFrameLocked(turn, harness.FrameTurnCompleted, "foundry run completed", func(f *harness.HarnessEventFrame) {
					f.Completed = &harness.TurnCompleted{Result: result, FinalEventSeq: f.Seq}
				})
				turn.completed = true
				s.scheduleTurnCleanupLocked(turn)
			}
			s.mu.Unlock()
			return nil
		case "cancelled":
			s.mu.Lock()
			if !turn.completed {
				s.appendFrameLocked(turn, harness.FrameTurnCancelled, "foundry run cancelled", nil)
				turn.completed = true
				s.scheduleTurnCleanupLocked(turn)
			}
			s.mu.Unlock()
			return nil
		case "failed", "expired", "incomplete":
			msg := run.Status
			if run.LastError != nil && run.LastError.Message != "" {
				msg = run.LastError.Message
			}
			s.appendFailed(turn, "foundry_"+run.Status, msg)
			return nil
		}
		s.resetPollFailures(turn)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.cfg.pollInterval):
		}
	}
	return nil
}

func (s *server) recordContinueResults(
	turn *turnState,
	results []harness.ToolCallResult,
) ([]harness.ToolCallResult, []harness.ToolCallResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, result := range results {
		if _, done := turn.submittedTools[result.ToolCallID]; done {
			continue
		}
		if _, pending := turn.pendingTools[result.ToolCallID]; !pending {
			return nil, nil, fmt.Errorf("tool result %q is not pending for this turn", result.ToolCallID)
		}
	}
	received := make([]harness.ToolCallResult, 0, len(results))
	for _, result := range results {
		if _, done := turn.submittedTools[result.ToolCallID]; done {
			continue
		}
		if _, exists := turn.bufferedResults[result.ToolCallID]; exists {
			continue
		}
		received = append(received, result)
		turn.bufferedResults[result.ToolCallID] = result
	}
	if len(turn.pendingTools) == 0 || len(turn.bufferedResults) < len(turn.pendingTools) {
		return nil, received, nil
	}
	toSubmit := make([]harness.ToolCallResult, 0, len(turn.pendingTools))
	for toolCallID := range turn.pendingTools {
		toSubmit = append(toSubmit, turn.bufferedResults[toolCallID])
	}
	return toSubmit, received, nil
}

func turnAllowsFoundryTool(request harness.StartTurnRequest, name string) bool {
	name = strings.TrimSpace(name)
	for _, definition := range request.Input.Tools {
		if strings.TrimSpace(definition.Name) == name {
			return true
		}
	}
	return false
}

func (s *server) submitToolOutputs(ctx context.Context, turn *turnState, results []harness.ToolCallResult) error {
	outs := make([]map[string]string, 0, len(results))
	for _, result := range results {
		payload := string(result.Output)
		if result.Error != nil {
			b, _ := json.Marshal(result.Error)
			payload = string(b)
		}
		outs = append(outs, map[string]string{"tool_call_id": result.ToolCallID, "output": payload})
	}
	return s.doJSON(ctx, http.MethodPost, fmt.Sprintf("/threads/%s/runs/%s/submit_tool_outputs", url.PathEscape(turn.threadID), url.PathEscape(turn.runID)), map[string]any{"tool_outputs": outs}, nil) //nolint:lll
}

func (s *server) fetchFinalMessage(ctx context.Context, turn *turnState) (string, error) {
	var messages foundryMessagesResponse
	path := fmt.Sprintf("/threads/%s/messages?run_id=%s&order=desc&limit=1",
		url.PathEscape(turn.threadID),
		url.QueryEscape(turn.runID),
	)
	if err := s.doJSON(ctx, http.MethodGet, path, nil, &messages); err != nil {
		return "", err
	}
	for _, msg := range messages.Data {
		if strings.EqualFold(msg.Role, "assistant") {
			if text := foundryMessageText(msg.Content); text != "" {
				return text, nil
			}
		}
	}
	return "", fatalFoundryPollError{
		err: fmt.Errorf("foundry run completed without assistant message for run %s", turn.runID),
	}
}

func (s *server) doJSON(ctx context.Context, method, path string, body any, out any) error {
	if s.cfg.endpoint == "" {
		return fmt.Errorf("foundry endpoint is required")
	}
	if !exactlyOneFoundryAuth(s.cfg) {
		return fmt.Errorf("exactly one Foundry auth mode is required")
	}
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reqBody = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.foundryURL(path), reqBody)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if s.cfg.foundryKey != "" {
		req.Header.Set("api-key", s.cfg.foundryKey)
	}
	if s.cfg.authBearer != "" {
		req.Header.Set("Authorization", "Bearer "+s.cfg.authBearer)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("foundry %s %s failed: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (s *server) foundryURL(path string) string {
	u := s.cfg.endpoint + path
	if s.cfg.apiVersion == "" {
		return u
	}
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	return u + sep + "api-version=" + url.QueryEscape(s.cfg.apiVersion)
}

func (s *server) appendFailed(turn *turnState, reason, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if turn.completed {
		return
	}
	s.appendFrameLocked(turn, harness.FrameTurnFailed, "foundry run failed", func(f *harness.HarnessEventFrame) {
		f.Failed = &harness.TurnFailed{Reason: reason, Message: msg}
		f.Error = &harness.ErrorInfo{Code: reason, Message: msg}
	})
	turn.completed = true
	s.scheduleTurnCleanupLocked(turn)
}

func (s *server) scheduleTurnCleanupLocked(turn *turnState) {
	turnID := turn.request.TurnID
	time.AfterFunc(defaultStateRetention, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.turns, turnID)
		cutoff := time.Now().UTC().Add(-defaultStateRetention)
		for sessionID, seen := range s.runtimeThreadSeen {
			if seen.Before(cutoff) {
				delete(s.runtimeThreadSeen, sessionID)
				delete(s.runtimeThreads, sessionID)
			}
		}
	})
}

func (s *server) appendFrameLocked(turn *turnState, typ harness.FrameType, summary string, mutate func(*harness.HarnessEventFrame)) { //nolint:lll
	seq := int64(len(turn.frames) + 1)
	frame := harness.HarnessEventFrame{Version: harness.ProtocolVersion, Type: typ, RuntimeSessionID: turn.request.RuntimeSessionID, TurnID: turn.request.TurnID, CorrelationID: turn.request.CorrelationID, Seq: seq, CreatedAt: time.Now().UTC(), Summary: summary, Metadata: map[string]string{"backend": "foundry"}} //nolint:lll
	if mutate != nil {
		mutate(&frame)
	}
	turn.frames = append(turn.frames, frame)
}

func (s *server) authorized(w http.ResponseWriter, r *http.Request) bool {
	if s.cfg.adapterBearer == "" {
		harness.WriteError(w, http.StatusUnauthorized, "adapter bearer token is required")
		return false
	}
	got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.adapterBearer)) != 1 {
		harness.WriteError(w, http.StatusUnauthorized, "unauthorized")
		return false
	}
	return true
}

func parseAfterSeq(value string) int64 {
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || parsed < 0 {
		return 0
	}
	return parsed
}

func validateFoundryToolDefinitions(definitions []harness.ToolDefinition) error {
	for _, definition := range definitions {
		if !foundryToolClassSupported(definition.BrokeredClass) {
			return fmt.Errorf(
				"unsupported Foundry brokered tool class %q for tool %q",
				definition.BrokeredClass,
				definition.Name,
			)
		}
	}
	return nil
}

func foundryToolClassSupported(class harness.BrokeredToolClass) bool {
	return class == harness.BrokeredToolClassRead || class == harness.BrokeredToolClassWrite
}

func supportedFoundryToolDefinitions(definitions []harness.ToolDefinition) []harness.ToolDefinition {
	out := make([]harness.ToolDefinition, 0, len(definitions))
	for _, definition := range definitions {
		if foundryToolClassSupported(definition.BrokeredClass) {
			out = append(out, definition)
		}
	}
	return out
}

func foundryToolAllowed(definitions []harness.ToolDefinition, name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	for _, definition := range definitions {
		if !foundryToolClassSupported(definition.BrokeredClass) {
			continue
		}
		if strings.TrimSpace(definition.Name) == name {
			return true
		}
	}
	return false
}

func foundryToolSchemas(definitions []harness.ToolDefinition) []map[string]any {
	definitions = supportedFoundryToolDefinitions(definitions)
	tools := make([]map[string]any, 0, len(definitions))
	for _, definition := range definitions {
		name := strings.TrimSpace(definition.Name)
		if name == "" {
			continue
		}
		parameters := json.RawMessage(`{"type":"object","additionalProperties":true}`)
		if len(definition.Parameters) > 0 {
			parameters = definition.Parameters
		}
		tools = append(tools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": strings.TrimSpace(definition.Description),
				"parameters":  parameters,
			},
		})
	}
	return tools
}

func normalizeFoundryToolArguments(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage(`{}`), nil
	}
	var encoded string
	if err := json.Unmarshal(raw, &encoded); err == nil {
		encoded = strings.TrimSpace(encoded)
		if encoded != "" && json.Valid([]byte(encoded)) {
			return json.RawMessage(encoded), nil
		}
		return nil, fmt.Errorf("foundry tool arguments are not valid JSON")
	}
	if json.Valid(raw) {
		return raw, nil
	}
	return nil, fmt.Errorf("foundry tool arguments are not valid JSON")
}

func foundryMessageText(content any) string {
	switch v := content.(type) {
	case string:
		return strings.TrimSpace(v)
	case []any:
		parts := []string{}
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				if t, ok := m["text"].(map[string]any); ok {
					if val, ok := t["value"].(string); ok && strings.TrimSpace(val) != "" {
						parts = append(parts, strings.TrimSpace(val))
					}
				}
				if val, ok := m["text"].(string); ok && strings.TrimSpace(val) != "" {
					parts = append(parts, strings.TrimSpace(val))
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}
func firstNonBlank(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
func parseDurationEnv(name string, fallback time.Duration) time.Duration {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return fallback
}
func sanitizeEndpoint(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}
