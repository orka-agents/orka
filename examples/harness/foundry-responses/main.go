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
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/orka-agents/orka/internal/harness"
)

const (
	defaultAddr              = ":8090"
	defaultAPIVersion        = "v1"
	defaultRequestTimeout    = 20 * time.Second
	defaultStateRetention    = 10 * time.Minute
	defaultMaxApprovalWait   = 30 * time.Minute
	defaultReadHeaderTimeout = 5 * time.Second
	defaultReadTimeout       = 30 * time.Second
	defaultIdleTimeout       = 60 * time.Second
	maxFoundryOutputBytes    = 1 << 20
	maxFoundryBodyBytes      = 4 << 20
	readinessPath            = "/v1/ready"
	foundryInitialUnknown    = "foundry_initial_unknown"

	envAddr                = "ORKA_FOUNDRY_RESPONSES_ADAPTER_ADDR"
	envRuntimeName         = "ORKA_FOUNDRY_RESPONSES_RUNTIME_NAME"
	envAdapterBearer       = "ORKA_FOUNDRY_RESPONSES_ADAPTER_BEARER_" + "TOKEN"
	envEndpoint            = "ORKA_FOUNDRY_RESPONSES_ENDPOINT"
	envProjectEndpoint     = "ORKA_FOUNDRY_RESPONSES_PROJECT_ENDPOINT"
	envAgentName           = "ORKA_FOUNDRY_RESPONSES_AGENT_NAME"
	envAuthBearer          = "ORKA_FOUNDRY_RESPONSES_AUTH_BEARER"
	envFoundryAuth         = "ORKA_FOUNDRY_RESPONSES_API_" + "KEY"
	envAPIVersion          = "ORKA_FOUNDRY_RESPONSES_API_VERSION"
	envRequestTimeout      = "ORKA_FOUNDRY_RESPONSES_POLL_TIMEOUT"
	envStateRetention      = "ORKA_FOUNDRY_RESPONSES_STATE_RETENTION"
	envMaxApprovalWait     = "ORKA_FOUNDRY_RESPONSES_MAX_APPROVAL_WAIT"
	envContinuationProof   = "ORKA_FOUNDRY_RESPONSES_BROKERED_CONTINUATION_PROOF"
	envBrokeredToolClasses = "ORKA_FOUNDRY_RESPONSES_BROKERED_TOOL_CLASSES"
	envAudience            = "ORKA_FOUNDRY_RESPONSES_TOKEN_AUDIENCE"
)

type config struct {
	addr                string
	runtimeName         string
	adapterBearer       string
	endpoint            string
	projectEndpoint     string
	agentName           string
	authBearer          string
	foundryAuth         string
	apiVersion          string
	requestTimeout      time.Duration
	stateRetention      time.Duration
	maxApprovalWait     time.Duration
	continuationProof   string
	brokeredToolClasses []harness.BrokeredToolClass
	configError         string
}

type server struct {
	cfg    config
	client *http.Client

	mu              sync.Mutex
	turns           map[harness.HarnessTurnID]*turnState
	runtimeSessions map[harness.RuntimeSessionID]foundrySession
}

type foundrySession struct {
	ID       string
	LastSeen time.Time
}

type turnState struct {
	request           harness.StartTurnRequest
	initializing      bool
	responseID        string
	foundrySessionID  string
	pendingTools      map[string]string
	pendingSince      map[string]time.Time
	bufferedResults   map[string]harness.ToolCallResult
	bufferedPayloads  map[string]string
	submittedPayloads map[string]string
	frames            []harness.HarnessEventFrame
	completed         bool
	frameUpdates      chan struct{}
	continueMu        sync.Mutex
}

type responsesRequest struct {
	Input              any    `json:"input"`
	PreviousResponseID string `json:"previous_response_id,omitempty"`
	AgentSessionID     string `json:"agent_session_id,omitempty"`
	// BrokeredContinuationProof carries the continuation proof in the request
	// BODY in addition to the X-AgentKit-Brokered-Continuation-Proof header.
	// Some hosted-agent gateways (e.g. Microsoft Foundry) strip custom request
	// headers before forwarding to the container, which would reject the
	// function_call_output continuation; the body survives, so a
	// gateway-tolerant runtime can recover the proof from here. Only set on
	// continuation requests (PreviousResponseID present).
	BrokeredContinuationProof string `json:"brokered_continuation_proof,omitempty"`
}

type responsesFunctionCallOutput struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Output string `json:"output"`
	Status string `json:"status,omitempty"`
}

type responsesResponse struct {
	ID             string            `json:"id"`
	AgentSessionID string            `json:"agent_session_id,omitempty"`
	Status         string            `json:"status,omitempty"`
	Output         []responsesOutput `json:"output,omitempty"`
	Error          *responsesError   `json:"error,omitempty"`
}

type responsesOutput struct {
	Type      string          `json:"type"`
	Role      string          `json:"role,omitempty"`
	Name      string          `json:"name,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Content   any             `json:"content,omitempty"`
	Text      string          `json:"text,omitempty"`
	Status    string          `json:"status,omitempty"`
}

type responsesError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

type pendingFunctionCall struct {
	callID string
	name   string
	args   json.RawMessage
}

const responsesEndpointRequirement = "foundry hosted Responses endpoint must use https " +
	"(http allowed only for loopback), target /responses, and must not include credentials, " +
	"fragments, or query parameters other than api-version"

func main() {
	cfg := loadConfig()
	s := newServer(cfg, &http.Client{Timeout: cfg.requestTimeout})
	log.Printf(
		"Foundry hosted Responses AgentRuntime adapter listening on %s runtime=%s endpoint=%s",
		cfg.addr,
		cfg.runtimeName,
		sanitizeEndpoint(cfg.endpoint),
	)
	if err := newAdapterHTTPServer(cfg.addr, s.handler()).ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func newAdapterHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: defaultReadHeaderTimeout,
		ReadTimeout:       defaultReadTimeout,
		IdleTimeout:       defaultIdleTimeout,
	}
}

func loadConfig() config {
	classes, classErr := parseBrokeredToolClasses(os.Getenv(envBrokeredToolClasses))
	cfg := config{
		addr:                firstNonBlank(os.Getenv(envAddr), defaultAddr),
		runtimeName:         firstNonBlank(os.Getenv(envRuntimeName), "foundry-agentkit-responses"),
		adapterBearer:       strings.TrimSpace(os.Getenv(envAdapterBearer)),
		endpoint:            strings.TrimSpace(os.Getenv(envEndpoint)),
		projectEndpoint:     strings.TrimRight(strings.TrimSpace(os.Getenv(envProjectEndpoint)), "/"),
		agentName:           strings.TrimSpace(os.Getenv(envAgentName)),
		authBearer:          strings.TrimSpace(os.Getenv(envAuthBearer)),
		foundryAuth:         strings.TrimSpace(os.Getenv(envFoundryAuth)),
		apiVersion:          firstNonBlank(os.Getenv(envAPIVersion), defaultAPIVersion),
		requestTimeout:      parseDurationEnv(envRequestTimeout, defaultRequestTimeout),
		stateRetention:      parseDurationEnv(envStateRetention, defaultStateRetention),
		maxApprovalWait:     parseDurationEnv(envMaxApprovalWait, defaultMaxApprovalWait),
		continuationProof:   strings.TrimSpace(os.Getenv(envContinuationProof)),
		brokeredToolClasses: classes,
	}
	_ = os.Getenv(envAudience) // Reserved for a future workload-identity token provider; never logged.
	if classErr != nil {
		cfg.configError = classErr.Error()
	}
	return cfg
}

func newServer(cfg config, client *http.Client) *server {
	if client == nil {
		client = &http.Client{Timeout: cfg.requestTimeout}
	}
	clientCopy := *client
	if clientCopy.CheckRedirect == nil {
		clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}
	return &server{
		cfg:             cfg,
		client:          &clientCopy,
		turns:           map[harness.HarnessTurnID]*turnState{},
		runtimeSessions: map[harness.RuntimeSessionID]foundrySession{},
	}
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(harness.HealthPath, s.health)
	mux.HandleFunc(readinessPath, s.ready)
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
	_, endpointErr := s.responsesEndpoint()
	ready := s.cfg.configError == "" && s.cfg.adapterBearer != "" && endpointErr == nil && exactlyOneFoundryAuth(s.cfg)
	status := harness.HealthStatusOK
	msg := "ready"
	if !ready {
		status = harness.HealthStatusDegraded
		parts := []string{
			"adapter bearer, safe Foundry hosted Responses endpoint, and exactly one Foundry auth mode are required",
		}
		if s.cfg.configError != "" {
			parts = append(parts, s.cfg.configError)
		}
		if endpointErr != nil {
			parts = append(parts, endpointErr.Error())
		}
		msg = strings.Join(parts, "; ")
	}
	harness.WriteJSON(w, http.StatusOK, harness.HealthResponse{
		Version:   harness.ProtocolVersion,
		Status:    status,
		Ready:     ready,
		CheckedAt: time.Now().UTC(),
		Message:   msg,
		Metadata:  map[string]string{"backend": "foundry-responses"},
	})
}

func (s *server) ready(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		harness.WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	_, endpointErr := s.responsesEndpoint()
	ready := s.cfg.configError == "" && s.cfg.adapterBearer != "" && endpointErr == nil && exactlyOneFoundryAuth(s.cfg)
	if !ready {
		harness.WriteError(w, http.StatusServiceUnavailable, "adapter is not ready")
		return
	}
	harness.WriteJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *server) capabilities(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		harness.WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	modes := []harness.ToolExecutionMode{harness.ToolExecutionModeObserved}
	maxTurnSeconds := int(s.cfg.requestTimeout.Seconds())
	if len(s.cfg.brokeredToolClasses) > 0 {
		modes = append(modes, harness.ToolExecutionModeBrokered)
		// A brokered turn can contain multiple hosted request/tool-result rounds,
		// each with its own request timeout and approval wait. The harness contract
		// has only one runtime-wide ceiling, so advertise unknown rather than an
		// understated duration when brokered mode is available.
		maxTurnSeconds = 0
	}
	harness.WriteJSON(w, http.StatusOK, harness.CapabilitiesResponse{
		Version:                 harness.ProtocolVersion,
		ProtocolVersion:         harness.ProtocolVersion,
		Transport:               harness.HTTPTransport,
		RuntimeName:             s.cfg.runtimeName,
		RuntimeVersion:          "foundry-responses-adapter",
		ProviderKind:            harness.ProviderKindRemote,
		ToolExecutionModes:      modes,
		BrokeredToolClasses:     append([]harness.BrokeredToolClass(nil), s.cfg.brokeredToolClasses...),
		SupportsCancel:          true,
		SupportsRuntimeSessions: true,
		SupportsContinuation:    len(s.cfg.brokeredToolClasses) > 0,
		SupportsArtifacts:       false,
		MaxConcurrentTurns:      1,
		MaxTurnSeconds:          maxTurnSeconds,
		MaxOutputBytes:          maxFoundryOutputBytes,
		Metadata:                map[string]string{"backend": "foundry-responses"},
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
	if err := s.validateStartRequest(req); err != nil {
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
		response := startTurnResponse(existing.request, eventsPath)
		if !sameStartTurnRequest(existing.request, req) {
			s.mu.Unlock()
			harness.WriteError(w, http.StatusConflict, "turn already exists")
			return
		}
		if existing.initializing {
			s.mu.Unlock()
			harness.WriteError(w, http.StatusConflict, "turn initialization in progress")
			return
		}
		s.mu.Unlock()
		harness.WriteJSON(w, http.StatusAccepted, response)
		return
	}
	turn := &turnState{
		request:           req,
		initializing:      true,
		pendingTools:      map[string]string{},
		pendingSince:      map[string]time.Time{},
		bufferedResults:   map[string]harness.ToolCallResult{},
		bufferedPayloads:  map[string]string{},
		submittedPayloads: map[string]string{},
		frameUpdates:      make(chan struct{}),
	}
	s.appendFrameLocked(turn, harness.FrameTurnStarted, "foundry hosted response started", nil)
	s.turns[req.TurnID] = turn
	s.mu.Unlock()

	ctx, cancel := s.foundryRequestContext(r.Context(), req.Deadline)
	defer cancel()
	var response responsesResponse
	initialRequest := responsesRequest{Input: req.Input.Prompt}
	foundrySessionID, err := s.postResponses(ctx, req.RuntimeSessionID, initialRequest, &response)
	if err != nil {
		s.mu.Lock()
		turn.initializing = false
		s.appendFailedLocked(
			turn,
			foundryInitialUnknown,
			"initial hosted request failed after submission was attempted; "+
				"failing closed to avoid a duplicate hosted turn",
		)
		s.mu.Unlock()
		log.Printf(
			"Foundry hosted Responses initial request failed after submission for turn %q (error type %T)",
			req.TurnID,
			err,
		)
		// Accept the turn so Orka consumes and persists the terminal failure frame.
		// Returning a control-plane error here would cause the caller to retry a
		// submission whose outcome is unknown at Foundry.
		harness.WriteJSON(w, http.StatusAccepted, startTurnResponse(req, eventsPath))
		return
	}
	s.mu.Lock()
	s.recordTurnSessionLocked(turn, foundrySessionID)
	s.mu.Unlock()
	s.handleResponsesResponse(turn, response)
	s.mu.Lock()
	turn.initializing = false
	s.mu.Unlock()
	harness.WriteJSON(w, http.StatusAccepted, startTurnResponse(req, eventsPath))
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
	afterSeq := parseAfterSeq(r.URL.Query().Get("afterSeq"))
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	nextSeq := afterSeq
	for {
		s.mu.Lock()
		frames := append([]harness.HarnessEventFrame(nil), turn.frames...)
		completed := turn.completed
		updates := turn.frameUpdates
		if updates == nil {
			updates = make(chan struct{})
			turn.frameUpdates = updates
		}
		s.mu.Unlock()
		for _, frame := range frames {
			if frame.Seq <= nextSeq {
				continue
			}
			if err := harness.WriteSSEFrame(w, frame); err != nil {
				return
			}
			nextSeq = frame.Seq
		}
		if completed {
			_ = harness.WriteSSEDone(w)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-updates:
		}
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
	if !sameContinueIdentity(turn.request, req) {
		harness.WriteError(w, http.StatusBadRequest, "continue request does not match started turn")
		return
	}
	turn.continueMu.Lock()
	defer turn.continueMu.Unlock()

	if err := s.ensureTerminalContinueIsDuplicate(turn, req.ToolResults); err != nil {
		harness.WriteError(w, http.StatusConflict, err.Error())
		return
	}
	s.mu.Lock()
	completed := turn.completed
	s.mu.Unlock()
	if completed {
		harness.WriteJSON(
			w,
			http.StatusAccepted,
			continueResponse(req, "duplicate continue accepted for terminal turn"),
		)
		return
	}
	s.mu.Lock()
	previousResponseID := turn.responseID
	foundrySessionID := turn.foundrySessionID
	s.mu.Unlock()
	if strings.TrimSpace(previousResponseID) == "" {
		harness.WriteError(w, http.StatusConflict, "cannot continue before Foundry response id is known")
		return
	}

	resultsToSubmit, err := s.recordContinueResults(turn, req.ToolResults)
	if err != nil {
		harness.WriteError(w, http.StatusConflict, err.Error())
		return
	}
	if len(resultsToSubmit) == 0 {
		harness.WriteJSON(w, http.StatusAccepted, continueResponse(req, "continue accepted"))
		return
	}
	outputs, err := functionCallOutputs(resultsToSubmit)
	if err != nil {
		harness.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := s.foundryRequestContext(r.Context(), turn.request.Deadline)
	defer cancel()
	var response responsesResponse
	continuation := responsesRequest{
		PreviousResponseID: previousResponseID,
		AgentSessionID:     foundrySessionID,
		Input:              outputs,
	}
	updatedSessionID, err := s.postResponses(ctx, req.RuntimeSessionID, continuation, &response)
	if err != nil {
		s.mu.Lock()
		s.appendFailedLocked(
			turn,
			"foundry_continuation_unknown",
			"hosted continuation failed after submission was attempted; "+
				"failing closed to avoid duplicate continuation",
		)
		s.mu.Unlock()
		log.Printf(
			"Foundry hosted Responses continuation failed after submission for turn %q (error type %T)",
			req.TurnID,
			err,
		)
		harness.WriteError(w, http.StatusBadGateway, "hosted continuation failed after submission was attempted")
		return
	}
	s.mu.Lock()
	if turn.completed {
		s.mu.Unlock()
		harness.WriteError(w, http.StatusConflict, "turn completed while hosted continuation was in flight")
		return
	}
	for _, result := range resultsToSubmit {
		toolName := turn.pendingTools[result.ToolCallID]
		if toolName == "" {
			toolName = result.ToolCallID
		}
		frame := s.newFrame(
			turn,
			int64(len(turn.frames)+1),
			harness.FrameToolResultReceived,
			"brokered tool result received",
			func(f *harness.HarnessEventFrame) {
				f.ToolName = toolName
				f.ToolCallID = result.ToolCallID
				f.Content = result.Output
				f.Error = result.Error
			},
		)
		if !harnessFrameFitsSSE(frame) {
			s.appendFailedLocked(
				turn,
				"brokered_tool_result_frame_too_large",
				"brokered tool result exceeded the harness SSE frame limit",
			)
			s.mu.Unlock()
			harness.WriteError(w, http.StatusBadGateway, "brokered tool result exceeds harness SSE frame limit")
			return
		}
		s.appendPreparedFrameLocked(turn, frame)
		delete(turn.pendingTools, result.ToolCallID)
		delete(turn.pendingSince, result.ToolCallID)
		delete(turn.bufferedResults, result.ToolCallID)
		delete(turn.bufferedPayloads, result.ToolCallID)
	}
	s.recordTurnSessionLocked(turn, updatedSessionID)
	s.mu.Unlock()
	s.handleResponsesResponse(turn, response)
	harness.WriteJSON(w, http.StatusAccepted, continueResponse(req, "continue accepted"))
}

func (s *server) cancelTurn(w http.ResponseWriter, r *http.Request, turn *turnState) {
	if r.Method != http.MethodPost {
		harness.WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	turn.continueMu.Lock()
	defer turn.continueMu.Unlock()
	s.mu.Lock()
	if !turn.completed {
		s.appendFrameLocked(turn, harness.FrameTurnCancelled, "turn cancelled", nil)
		turn.completed = true
		s.scheduleTurnCleanupLocked(turn)
	}
	req := turn.request
	s.mu.Unlock()
	harness.WriteJSON(
		w,
		http.StatusAccepted,
		harness.CancelTurnResponse{
			Version:          harness.ProtocolVersion,
			Accepted:         true,
			RuntimeSessionID: req.RuntimeSessionID,
			TurnID:           req.TurnID,
			CorrelationID:    req.CorrelationID,
		},
	)
}

func (s *server) handleResponsesResponse(turn *turnState, response responsesResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if turn.completed {
		return
	}
	responseIDPresent := strings.TrimSpace(response.ID) != ""
	if responseIDPresent {
		turn.responseID = response.ID
	}
	if response.Error != nil {
		s.appendFailedLocked(
			turn,
			"foundry_response_error",
			firstNonBlank(response.Error.Message, response.Error.Code, "Foundry hosted Responses returned an error"),
		)
		return
	}
	if isFailureStatus(response.Status) {
		s.appendFailedLocked(turn, "foundry_"+response.Status, "Foundry hosted Responses status "+response.Status)
		return
	}
	if strings.TrimSpace(response.Status) == "" {
		s.appendFailedLocked(turn, "foundry_status_missing", "Foundry hosted Responses status is missing")
		return
	}
	if !isCompletionStatus(response.Status) {
		s.appendFailedLocked(turn, "foundry_"+response.Status, "Foundry hosted Responses status "+response.Status)
		return
	}
	calls, err := s.extractFunctionCalls(turn.request, response.Output)
	if err != nil {
		s.appendFailedLocked(turn, "foundry_function_call_invalid", err.Error())
		return
	}
	if len(calls) > 0 {
		if !responseIDPresent {
			s.appendFailedLocked(
				turn,
				"foundry_response_id_missing",
				"hosted response returned a function_call without an id needed for continuation",
			)
			return
		}
		seenInResponse := map[string]struct{}{}
		for _, call := range calls {
			if _, seen := seenInResponse[call.callID]; seen {
				s.appendFailedLocked(
					turn,
					"foundry_repeated_function_call",
					"hosted response repeated a function call id",
				)
				return
			}
			seenInResponse[call.callID] = struct{}{}
			if _, submitted := turn.submittedPayloads[call.callID]; submitted {
				s.appendFailedLocked(
					turn,
					"foundry_repeated_function_call",
					"hosted response repeated an already-submitted function call",
				)
				return
			}
			if _, pending := turn.pendingTools[call.callID]; pending {
				s.appendFailedLocked(
					turn,
					"foundry_repeated_function_call",
					"hosted response repeated an already-pending function call",
				)
				return
			}
		}
		frames := make([]harness.HarnessEventFrame, 0, len(calls))
		baseSeq := int64(len(turn.frames) + 1)
		for index, call := range calls {
			frame := s.newFrame(
				turn,
				baseSeq+int64(index),
				harness.FrameToolCallRequested,
				"foundry hosted tool call requested",
				func(f *harness.HarnessEventFrame) {
					f.ToolName = call.name
					f.ToolCallID = call.callID
					f.Content = call.args
				},
			)
			if !harnessFrameFitsSSE(frame) {
				s.appendFailedLocked(
					turn,
					"foundry_tool_call_frame_too_large",
					"hosted function call exceeded the harness SSE frame limit",
				)
				return
			}
			frames = append(frames, frame)
		}
		now := time.Now().UTC()
		for index, call := range calls {
			turn.pendingTools[call.callID] = call.name
			turn.pendingSince[call.callID] = now
			s.schedulePendingToolTimeoutLocked(turn, call.callID)
			s.appendPreparedFrameLocked(turn, frames[index])
		}
		return
	}
	result := responsesMessageText(response.Output)
	if len([]byte(result)) > maxFoundryOutputBytes {
		s.appendFailedLocked(turn, "foundry_output_too_large", "foundry completion exceeded advertised output limit")
		return
	}
	completedFrame := s.newFrame(
		turn,
		int64(len(turn.frames)+1),
		harness.FrameTurnCompleted,
		"foundry hosted response completed",
		func(f *harness.HarnessEventFrame) {
			f.Completed = &harness.TurnCompleted{Result: result, FinalEventSeq: f.Seq}
		},
	)
	if !harnessFrameFitsSSE(completedFrame) {
		s.appendFailedLocked(
			turn,
			"foundry_output_frame_too_large",
			"foundry completion exceeded the harness SSE frame limit",
		)
		return
	}
	s.appendPreparedFrameLocked(turn, completedFrame)
	turn.completed = true
	s.scheduleTurnCleanupLocked(turn)
}

func (s *server) extractFunctionCalls(
	request harness.StartTurnRequest,
	output []responsesOutput,
) ([]pendingFunctionCall, error) {
	calls := []responsesOutput{}
	for _, item := range output {
		if strings.EqualFold(strings.TrimSpace(item.Type), "function_call") {
			calls = append(calls, item)
		}
	}
	if len(calls) == 0 {
		return nil, nil
	}
	if request.ToolExecutionMode != harness.ToolExecutionModeBrokered {
		return nil, fmt.Errorf(
			"hosted response requested a function_call while Orka turn is not in brokered mode",
		)
	}
	pending := make([]pendingFunctionCall, 0, len(calls))
	seenCallIDs := map[string]struct{}{}
	for _, call := range calls {
		callID := strings.TrimSpace(call.CallID)
		name := strings.TrimSpace(call.Name)
		if callID == "" {
			return nil, fmt.Errorf("hosted response function_call missing call_id")
		}
		if _, exists := seenCallIDs[callID]; exists {
			return nil, fmt.Errorf("hosted response repeated function_call call_id %q", callID)
		}
		seenCallIDs[callID] = struct{}{}
		if name == "" {
			return nil, fmt.Errorf("hosted response function_call %q missing name", callID)
		}
		definition, ok := findToolDefinition(request.Input.Tools, name)
		if !ok {
			return nil, fmt.Errorf(
				"hosted response requested tool %q that Orka did not expose for this turn",
				name,
			)
		}
		if !s.supportsBrokeredClass(definition.BrokeredClass) {
			return nil, fmt.Errorf(
				"hosted response requested tool %q with unsupported brokered class %q",
				name,
				definition.BrokeredClass,
			)
		}
		args, err := normalizeResponsesToolArguments(call.Arguments)
		if err != nil {
			return nil, err
		}
		pending = append(pending, pendingFunctionCall{callID: callID, name: name, args: args})
	}
	return pending, nil
}

func (s *server) recordContinueResults(
	turn *turnState,
	results []harness.ToolCallResult,
) ([]harness.ToolCallResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(turn.pendingTools) == 0 {
		return nil, fmt.Errorf("no tool calls are pending for this turn")
	}
	now := time.Now().UTC()
	newResults := map[string]harness.ToolCallResult{}
	newPayloads := map[string]string{}
	for _, result := range results {
		payload, err := canonicalToolResultOutput(result)
		if err != nil {
			return nil, err
		}
		if submitted, done := turn.submittedPayloads[result.ToolCallID]; done {
			if submitted == payload {
				continue
			}
			return nil, fmt.Errorf("conflicting duplicate result for tool call %q", result.ToolCallID)
		}
		if _, pending := turn.pendingTools[result.ToolCallID]; !pending {
			return nil, fmt.Errorf("tool result %q is not pending for this turn", result.ToolCallID)
		}
		if pendingAt := turn.pendingSince[result.ToolCallID]; !pendingAt.IsZero() && s.cfg.maxApprovalWait > 0 &&
			now.Sub(pendingAt) > s.cfg.maxApprovalWait {
			turn.completed = true
			s.appendFrameLocked(
				turn,
				harness.FrameTurnFailed,
				"approval wait exceeded",
				func(f *harness.HarnessEventFrame) {
					f.Failed = &harness.TurnFailed{
						Reason:  "approval_wait_exceeded",
						Message: "maximum brokered tool wait exceeded",
					}
					f.Error = &harness.ErrorInfo{
						Code:    "approval_wait_exceeded",
						Message: "maximum brokered tool wait exceeded",
					}
				},
			)
			s.scheduleTurnCleanupLocked(turn)
			return nil, fmt.Errorf("maximum brokered tool wait exceeded")
		}
		if buffered, exists := turn.bufferedPayloads[result.ToolCallID]; exists {
			if buffered == payload {
				continue
			}
			return nil, fmt.Errorf("conflicting duplicate result for tool call %q", result.ToolCallID)
		}
		if buffered, exists := newPayloads[result.ToolCallID]; exists {
			if buffered == payload {
				continue
			}
			return nil, fmt.Errorf("conflicting duplicate result for tool call %q", result.ToolCallID)
		}
		newResults[result.ToolCallID] = result
		newPayloads[result.ToolCallID] = payload
	}
	for id, result := range newResults {
		turn.bufferedResults[id] = result
		turn.bufferedPayloads[id] = newPayloads[id]
	}
	readyCount := 0
	for id := range turn.pendingTools {
		if _, submitted := turn.submittedPayloads[id]; submitted {
			readyCount++
			continue
		}
		if _, buffered := turn.bufferedResults[id]; buffered {
			readyCount++
		}
	}
	if readyCount < len(turn.pendingTools) {
		return nil, nil
	}
	ids := make([]string, 0, len(turn.pendingTools))
	for id := range turn.pendingTools {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	toSubmit := make([]harness.ToolCallResult, 0, len(ids))
	for _, id := range ids {
		if _, submitted := turn.submittedPayloads[id]; submitted {
			continue
		}
		toSubmit = append(toSubmit, turn.bufferedResults[id])
	}
	if len(toSubmit) == 0 {
		return nil, nil
	}
	baseSeq := int64(len(turn.frames) + 1)
	for index, result := range toSubmit {
		toolName := firstNonBlank(turn.pendingTools[result.ToolCallID], result.ToolCallID)
		frame := s.newFrame(
			turn,
			baseSeq+int64(index),
			harness.FrameToolResultReceived,
			"brokered tool result received",
			func(f *harness.HarnessEventFrame) {
				f.ToolName = toolName
				f.ToolCallID = result.ToolCallID
				f.Content = result.Output
				f.Error = result.Error
			},
		)
		if !harnessFrameFitsSSE(frame) {
			s.appendFailedLocked(
				turn,
				"brokered_tool_result_frame_too_large",
				"brokered tool result exceeded the harness SSE frame limit",
			)
			return nil, fmt.Errorf("brokered tool result %q exceeds harness SSE frame limit", result.ToolCallID)
		}
	}
	for _, result := range toSubmit {
		turn.submittedPayloads[result.ToolCallID] = turn.bufferedPayloads[result.ToolCallID]
	}
	return toSubmit, nil
}

func (s *server) ensureTerminalContinueIsDuplicate(turn *turnState, results []harness.ToolCallResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !turn.completed {
		return nil
	}
	for _, result := range results {
		payload, err := canonicalToolResultOutput(result)
		if err != nil {
			return err
		}
		submitted, done := turn.submittedPayloads[result.ToolCallID]
		if !done {
			return fmt.Errorf("terminal turn cannot accept new tool result %q", result.ToolCallID)
		}
		if submitted != payload {
			return fmt.Errorf("conflicting duplicate result for tool call %q", result.ToolCallID)
		}
	}
	return nil
}

func functionCallOutputs(results []harness.ToolCallResult) ([]responsesFunctionCallOutput, error) {
	outputs := make([]responsesFunctionCallOutput, 0, len(results))
	for _, result := range results {
		payload, err := canonicalToolResultOutput(result)
		if err != nil {
			return nil, err
		}
		outputs = append(
			outputs,
			responsesFunctionCallOutput{
				Type:   "function_call_output",
				CallID: result.ToolCallID,
				Output: payload,
				Status: "completed",
			},
		)
	}
	return outputs, nil
}

func canonicalToolResultOutput(result harness.ToolCallResult) (string, error) {
	if result.Error != nil {
		return compactJSON(struct {
			Approved bool               `json:"approved"`
			Error    *harness.ErrorInfo `json:"error"`
		}{Approved: false, Error: result.Error})
	}
	if !result.Approved {
		return compactJSON(struct {
			Approved bool               `json:"approved"`
			Error    *harness.ErrorInfo `json:"error"`
		}{Approved: false, Error: &harness.ErrorInfo{Code: "approval_declined", Message: "tool call was not approved"}})
	}
	output := json.RawMessage(`{}`)
	if len(result.Output) > 0 {
		compacted, err := compactRawJSON(result.Output)
		if err != nil {
			return "", fmt.Errorf("tool result %q output must be valid JSON: %w", result.ToolCallID, err)
		}
		output = compacted
	}
	return compactJSON(struct {
		Approved bool            `json:"approved"`
		Output   json.RawMessage `json:"output"`
	}{Approved: true, Output: output})
}

func compactRawJSON(raw json.RawMessage) (json.RawMessage, error) {
	var compacted bytes.Buffer
	if err := json.Compact(&compacted, raw); err != nil {
		return nil, err
	}
	return json.RawMessage(compacted.Bytes()), nil
}

func compactJSON(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func (s *server) postResponses(
	ctx context.Context,
	runtimeSessionID harness.RuntimeSessionID,
	body responsesRequest,
	out *responsesResponse,
) (string, error) {
	if !exactlyOneFoundryAuth(s.cfg) {
		return "", fmt.Errorf("exactly one Foundry auth mode is required")
	}
	endpoint, err := s.responsesEndpoint()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	session := s.runtimeSessions[runtimeSessionID]
	s.mu.Unlock()
	sessionID := firstNonBlank(body.AgentSessionID, session.ID)
	if body.AgentSessionID == "" && sessionID != "" {
		body.AgentSessionID = sessionID
	}
	// Carry the continuation proof in the body as well as the header, so it
	// survives hosted-agent gateways (e.g. Foundry) that strip custom request
	// headers before forwarding to the runtime container. Only on continuations.
	if body.PreviousResponseID != "" && s.cfg.continuationProof != "" {
		body.BrokeredContinuationProof = s.cfg.continuationProof
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Foundry-Features", "HostedAgents=V1Preview")
	if s.cfg.foundryAuth != "" {
		req.Header.Set("api-key", s.cfg.foundryAuth)
	}
	if s.cfg.authBearer != "" {
		req.Header.Set("Authorization", "Bearer "+s.cfg.authBearer)
	}
	if body.PreviousResponseID != "" && s.cfg.continuationProof != "" {
		req.Header.Set("X-AgentKit-Brokered-Continuation-Proof", s.cfg.continuationProof)
	}
	if sessionID != "" {
		req.Header.Set("x-agent-session-id", sessionID)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close() //nolint:errcheck
	sessionID = firstNonBlank(
		resp.Header.Get("x-agent-session-id"),
		resp.Header.Get("x-ms-agent-session-id"),
		sessionID,
	)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf(
			"foundry hosted Responses request failed: HTTP %d: %s",
			resp.StatusCode,
			strings.TrimSpace(string(data)),
		)
	}
	if out != nil {
		decoder := json.NewDecoder(io.LimitReader(resp.Body, maxFoundryBodyBytes))
		if err := decoder.Decode(out); err != nil {
			return "", fmt.Errorf("decode Foundry hosted Responses response: %w", err)
		}
		sessionID = firstNonBlank(out.AgentSessionID, sessionID)
	}
	return sessionID, nil
}

func (s *server) responsesEndpoint() (string, error) {
	if strings.TrimSpace(s.cfg.endpoint) != "" {
		return responsesEndpointWithVersion(s.cfg.endpoint, s.cfg.apiVersion)
	}
	if strings.TrimSpace(s.cfg.projectEndpoint) == "" || strings.TrimSpace(s.cfg.agentName) == "" {
		return "", fmt.Errorf(
			"%s; set %s or %s plus %s",
			responsesEndpointRequirement,
			envEndpoint,
			envProjectEndpoint,
			envAgentName,
		)
	}
	if !projectEndpointIsSafe(s.cfg.projectEndpoint) {
		return "", fmt.Errorf("%s; project endpoint is unsafe", responsesEndpointRequirement)
	}
	base := strings.TrimRight(
		s.cfg.projectEndpoint,
		"/",
	) + "/agents/" + url.PathEscape(
		s.cfg.agentName,
	) + "/endpoint/protocols/openai/responses"
	return responsesEndpointWithVersion(base, s.cfg.apiVersion)
}

func responsesEndpointWithVersion(raw, apiVersion string) (string, error) {
	if !responsesEndpointIsSafe(raw) {
		return "", errors.New(responsesEndpointRequirement)
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", errors.New(responsesEndpointRequirement)
	}
	if strings.TrimSpace(apiVersion) != "" {
		q := u.Query()
		if q.Get("api-version") == "" {
			q.Set("api-version", strings.TrimSpace(apiVersion))
			u.RawQuery = q.Encode()
		}
	}
	return u.String(), nil
}

func responsesEndpointIsSafe(raw string) bool {
	trimmed := strings.TrimSpace(raw)
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || strings.TrimSpace(parsed.Path) == "" {
		return false
	}
	if parsed.User != nil || parsed.ForceQuery || parsed.Fragment != "" || strings.Contains(trimmed, "#") {
		return false
	}
	if !strings.HasSuffix(strings.TrimRight(parsed.Path, "/"), "/responses") {
		return false
	}
	if parsed.RawQuery != "" {
		values, err := url.ParseQuery(parsed.RawQuery)
		if err != nil {
			return false
		}
		for key, vals := range values {
			if key != "api-version" || len(vals) == 0 {
				return false
			}
			for _, val := range vals {
				if strings.TrimSpace(val) == "" {
					return false
				}
			}
		}
	}
	if strings.EqualFold(parsed.Scheme, "https") {
		return true
	}
	if !strings.EqualFold(parsed.Scheme, "http") {
		return false
	}
	host := strings.Trim(strings.ToLower(parsed.Hostname()), "[]")
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func projectEndpointIsSafe(raw string) bool {
	trimmed := strings.TrimSpace(raw)
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return false
	}
	if parsed.User != nil || parsed.ForceQuery || parsed.RawQuery != "" || parsed.Fragment != "" ||
		strings.Contains(trimmed, "#") {
		return false
	}
	if strings.EqualFold(parsed.Scheme, "https") {
		return true
	}
	if !strings.EqualFold(parsed.Scheme, "http") {
		return false
	}
	host := strings.Trim(strings.ToLower(parsed.Hostname()), "[]")
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func exactlyOneFoundryAuth(cfg config) bool {
	hasKey := strings.TrimSpace(cfg.foundryAuth) != ""
	hasBearer := strings.TrimSpace(cfg.authBearer) != ""
	return hasKey != hasBearer
}

func (s *server) foundryRequestContext(
	parent context.Context,
	turnDeadline time.Time,
) (context.Context, context.CancelFunc) {
	timeout := s.cfg.requestTimeout
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}
	deadline := time.Now().Add(timeout)
	if !turnDeadline.IsZero() && turnDeadline.Before(deadline) {
		deadline = turnDeadline
	}
	return context.WithDeadline(context.WithoutCancel(parent), deadline)
}

func (s *server) validateStartRequest(req harness.StartTurnRequest) error {
	if s.cfg.configError != "" {
		return errors.New(s.cfg.configError)
	}
	if _, err := s.responsesEndpoint(); err != nil {
		return err
	}
	if !exactlyOneFoundryAuth(s.cfg) {
		return fmt.Errorf("exactly one Foundry auth mode is required")
	}
	if req.ToolExecutionMode == harness.ToolExecutionModeBrokered {
		if len(s.cfg.brokeredToolClasses) == 0 {
			return fmt.Errorf("brokered mode is not enabled for this hosted Responses adapter")
		}
		for _, tool := range req.Input.Tools {
			if !s.supportsBrokeredClass(tool.BrokeredClass) {
				return fmt.Errorf(
					"brokered tool %q class %q is not advertised by this adapter",
					tool.Name,
					tool.BrokeredClass,
				)
			}
		}
	}
	return nil
}

func (s *server) supportsBrokeredClass(class harness.BrokeredToolClass) bool {
	return slices.Contains(s.cfg.brokeredToolClasses, class)
}

func findToolDefinition(definitions []harness.ToolDefinition, name string) (harness.ToolDefinition, bool) {
	name = strings.TrimSpace(name)
	for _, definition := range definitions {
		if strings.TrimSpace(definition.Name) == name {
			return definition, true
		}
	}
	return harness.ToolDefinition{}, false
}

func normalizeResponsesToolArguments(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return json.RawMessage(`{}`), nil
	}
	var encoded string
	if err := json.Unmarshal(raw, &encoded); err == nil {
		encoded = strings.TrimSpace(encoded)
		if encoded == "" {
			return json.RawMessage(`{}`), nil
		}
		return normalizeResponsesToolArguments(json.RawMessage(encoded))
	}
	compacted, err := compactRawJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("hosted response function_call arguments must be a valid JSON object")
	}
	trimmed := bytes.TrimSpace(compacted)
	if len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' {
		return nil, fmt.Errorf("hosted response function_call arguments must be a valid JSON object")
	}
	return json.RawMessage(append([]byte(nil), trimmed...)), nil
}

func responsesMessageText(output []responsesOutput) string {
	parts := []string{}
	for _, item := range output {
		typeName := strings.TrimSpace(item.Type)
		if typeName == "message" || typeName == "output_text" || typeName == "text" || typeName == "" {
			if text := outputItemText(item); text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func outputItemText(item responsesOutput) string {
	if strings.TrimSpace(item.Text) != "" {
		return strings.TrimSpace(item.Text)
	}
	switch content := item.Content.(type) {
	case string:
		return strings.TrimSpace(content)
	case []any:
		parts := []string{}
		for _, entry := range content {
			if m, ok := entry.(map[string]any); ok {
				if text, ok := m["text"].(string); ok && strings.TrimSpace(text) != "" {
					parts = append(parts, strings.TrimSpace(text))
				}
				if textMap, ok := m["text"].(map[string]any); ok {
					if value, ok := textMap["value"].(string); ok && strings.TrimSpace(value) != "" {
						parts = append(parts, strings.TrimSpace(value))
					}
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func isCompletionStatus(status string) bool {
	return strings.EqualFold(strings.TrimSpace(status), "completed")
}

func isFailureStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "failed", "cancelled", "expired", "incomplete":
		return true
	default:
		return false
	}
}

func (s *server) recordTurnSessionLocked(turn *turnState, sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}
	turn.foundrySessionID = sessionID
	s.runtimeSessions[turn.request.RuntimeSessionID] = foundrySession{ID: sessionID, LastSeen: time.Now().UTC()}
}

func (s *server) schedulePendingToolTimeoutLocked(turn *turnState, toolCallID string) {
	wait := s.cfg.maxApprovalWait
	if wait <= 0 {
		return
	}
	turnID := turn.request.TurnID
	time.AfterFunc(wait, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		current := s.turns[turnID]
		if current != turn || turn.completed {
			return
		}
		if _, pending := turn.pendingTools[toolCallID]; !pending {
			return
		}
		if _, submitted := turn.submittedPayloads[toolCallID]; submitted {
			return
		}
		s.appendFailedLocked(
			turn,
			"approval_wait_exceeded",
			"maximum brokered tool wait exceeded",
		)
	})
}

func (s *server) appendFailedLocked(turn *turnState, reason, msg string) {
	if turn.completed {
		return
	}
	failedFrame := s.newFrame(
		turn,
		int64(len(turn.frames)+1),
		harness.FrameTurnFailed,
		"foundry hosted response failed",
		func(f *harness.HarnessEventFrame) {
			f.Failed = &harness.TurnFailed{Reason: reason, Message: msg}
			f.Error = &harness.ErrorInfo{Code: reason, Message: msg}
		},
	)
	if !harnessFrameFitsSSE(failedFrame) {
		failedFrame = s.newFrame(
			turn,
			int64(len(turn.frames)+1),
			harness.FrameTurnFailed,
			"foundry hosted response failed",
			func(f *harness.HarnessEventFrame) {
				fallbackReason := "foundry_failure_frame_too_large"
				message := "failure detail exceeded the harness SSE frame limit"
				f.Failed = &harness.TurnFailed{Reason: fallbackReason, Message: message}
				f.Error = &harness.ErrorInfo{Code: fallbackReason, Message: message}
			},
		)
	}
	s.appendPreparedFrameLocked(turn, failedFrame)
	turn.completed = true
	s.scheduleTurnCleanupLocked(turn)
}

func (s *server) scheduleTurnCleanupLocked(turn *turnState) {
	turnID := turn.request.TurnID
	retention := s.cfg.stateRetention
	if retention <= 0 {
		retention = defaultStateRetention
	}
	time.AfterFunc(retention, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.turns, turnID)
		activeSessions := s.activeRuntimeSessionsLocked()
		cutoff := time.Now().UTC().Add(-retention)
		for sessionID, session := range s.runtimeSessions {
			if activeSessions[sessionID] {
				continue
			}
			if session.LastSeen.Before(cutoff) {
				delete(s.runtimeSessions, sessionID)
			}
		}
	})
}

func (s *server) activeRuntimeSessionsLocked() map[harness.RuntimeSessionID]bool {
	active := map[harness.RuntimeSessionID]bool{}
	for _, turn := range s.turns {
		if turn != nil && !turn.completed {
			active[turn.request.RuntimeSessionID] = true
		}
	}
	return active
}

func (s *server) appendFrameLocked(
	turn *turnState,
	typ harness.FrameType,
	summary string,
	mutate func(*harness.HarnessEventFrame),
) {
	frame := s.newFrame(turn, int64(len(turn.frames)+1), typ, summary, mutate)
	s.appendPreparedFrameLocked(turn, frame)
}

func (s *server) newFrame(
	turn *turnState,
	seq int64,
	typ harness.FrameType,
	summary string,
	mutate func(*harness.HarnessEventFrame),
) harness.HarnessEventFrame {
	frame := harness.HarnessEventFrame{
		Version:          harness.ProtocolVersion,
		Type:             typ,
		RuntimeSessionID: turn.request.RuntimeSessionID,
		TurnID:           turn.request.TurnID,
		CorrelationID:    turn.request.CorrelationID,
		Seq:              seq,
		CreatedAt:        time.Now().UTC(),
		Summary:          summary,
		Metadata:         map[string]string{"backend": "foundry-responses"},
	}
	if mutate != nil {
		mutate(&frame)
	}
	return frame
}

func harnessFrameFitsSSE(frame harness.HarnessEventFrame) bool {
	payload, err := json.Marshal(frame)
	return err == nil && len("data: ")+len(payload) < harness.MaxSSEFrameBytes
}

func (s *server) appendPreparedFrameLocked(turn *turnState, frame harness.HarnessEventFrame) {
	turn.frames = append(turn.frames, frame)
	if turn.frameUpdates != nil {
		close(turn.frameUpdates)
	}
	turn.frameUpdates = make(chan struct{})
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

func sameStartTurnRequest(existing, retry harness.StartTurnRequest) bool {
	return reflect.DeepEqual(existing, retry)
}

func sameContinueIdentity(start harness.StartTurnRequest, cont harness.ContinueTurnRequest) bool {
	return start.Namespace == cont.Namespace &&
		start.TaskName == cont.TaskName &&
		start.SessionName == cont.SessionName &&
		start.RuntimeSessionID == cont.RuntimeSessionID &&
		start.TurnID == cont.TurnID &&
		start.CorrelationID == cont.CorrelationID
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

func continueResponse(req harness.ContinueTurnRequest, msg string) harness.ContinueTurnResponse {
	return harness.ContinueTurnResponse{
		Version:          harness.ProtocolVersion,
		Accepted:         true,
		RuntimeSessionID: req.RuntimeSessionID,
		TurnID:           req.TurnID,
		CorrelationID:    req.CorrelationID,
		Message:          msg,
	}
}

func parseAfterSeq(value string) int64 {
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || parsed < 0 {
		return 0
	}
	return parsed
}

func parseBrokeredToolClasses(raw string) ([]harness.BrokeredToolClass, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	classes := []harness.BrokeredToolClass{}
	seen := map[harness.BrokeredToolClass]struct{}{}
	for part := range strings.SplitSeq(raw, ",") {
		class := harness.BrokeredToolClass(strings.TrimSpace(part))
		if class == "" {
			continue
		}
		switch class {
		case harness.BrokeredToolClassRead, harness.BrokeredToolClassWrite:
		default:
			return nil, fmt.Errorf(
				"unsupported %s value %q; supported values are read,write",
				envBrokeredToolClasses,
				class,
			)
		}
		if _, ok := seen[class]; ok {
			continue
		}
		seen[class] = struct{}{}
		classes = append(classes, class)
	}
	return classes, nil
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func parseDurationEnv(name string, fallback time.Duration) time.Duration {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		if parsed, err := time.ParseDuration(value); err == nil && parsed > 0 {
			return parsed
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
		return "[invalid endpoint]"
	}
	u.User = nil
	q := u.Query()
	for key := range q {
		if key != "api-version" {
			q.Del(key)
		}
	}
	u.RawQuery = q.Encode()
	u.Fragment = ""
	return u.String()
}
