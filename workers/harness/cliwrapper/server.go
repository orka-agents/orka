package cliwrapper

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/harness"
)

const maxTerminalResultBytes = 512 * 1024

type Server struct {
	config  Config
	adapter RuntimeAdapter
	runner  CommandRunner
	now     func() time.Time

	mu          sync.RWMutex
	turns       map[harness.HarnessTurnID]*turnState
	activeTurns int
}

type RuntimeSupportProvider interface {
	SupportedRuntimes() []string
}

type ServerOption func(*Server)

func WithClock(now func() time.Time) ServerOption {
	return func(s *Server) {
		if now != nil {
			s.now = now
		}
	}
}

func WithCommandRunner(runner CommandRunner) ServerOption {
	return func(s *Server) { s.runner = runner }
}

func NewServer(cfg Config, adapter RuntimeAdapter, opts ...ServerOption) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if adapter == nil {
		var err error
		adapter, err = NewRuntimeAdapter(cfg)
		if err != nil {
			return nil, err
		}
	}
	s := &Server{
		config:  cfg,
		adapter: adapter,
		runner:  NewCommandRunner(cfg),
		now:     time.Now,
		turns:   map[harness.HarnessTurnID]*turnState{},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	return s, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(harness.HealthPath, s.handleHealth)
	mux.HandleFunc(harness.CapabilitiesPath, s.handleCapabilities)
	mux.HandleFunc(harness.TurnsPath, s.handleStartTurn)
	mux.HandleFunc(harness.TurnsPath+"/", s.handleTurn)
	return mux
}

func (s *Server) authorized(w http.ResponseWriter, r *http.Request) bool {
	if s.config.AllowUnauthenticated {
		return true
	}
	want := strings.TrimSpace(s.config.AuthValue)
	if want == "" {
		writeSafeError(w, http.StatusServiceUnavailable, "wrapper auth token is not configured")
		return false
	}
	got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if got == "" || got != want {
		writeSafeError(w, http.StatusUnauthorized, "unauthorized")
		return false
	}
	return true
}

func (s *Server) finishTurn(turn *turnState) {
	turn.close()
	s.mu.Lock()
	if s.activeTurns > 0 {
		s.activeTurns--
	}
	s.mu.Unlock()
	s.scheduleTurnEviction(turn)
}

func (s *Server) scheduleTurnEviction(turn *turnState) {
	retention := s.config.TurnRetention
	if retention <= 0 {
		s.evictTurn(turn)
		return
	}
	time.AfterFunc(retention, func() { s.evictTurn(turn) })
}

func (s *Server) evictTurn(turn *turnState) {
	if turn == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if current := s.turns[turn.request.TurnID]; current == turn {
		delete(s.turns, turn.request.TurnID)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeSafeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	harness.WriteJSON(w, http.StatusOK, harness.HealthResponse{
		Version:   harness.ProtocolVersion,
		Status:    harness.HealthStatusOK,
		Ready:     true,
		CheckedAt: s.now().UTC(),
		Metadata: map[string]string{
			"runtime": s.adapter.Name(),
			"mode":    "observed",
		},
	})
}

func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeSafeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	harness.WriteJSON(w, http.StatusOK, harness.CapabilitiesResponse{
		Version:                 harness.ProtocolVersion,
		ProtocolVersion:         harness.ProtocolVersion,
		Transport:               harness.HTTPTransport,
		RuntimeName:             s.adapter.Name(),
		ProviderKind:            harness.ProviderKindKubernetesService,
		ToolExecutionModes:      []harness.ToolExecutionMode{harness.ToolExecutionModeObserved},
		SupportsCancel:          true,
		SupportsRuntimeSessions: true,
		MaxConcurrentTurns:      1,
		Metadata:                s.capabilitiesMetadata(),
	})
}

func (s *Server) capabilitiesMetadata() map[string]string {
	metadata := map[string]string{
		"wrapper": "cli",
		"mode":    "observed",
	}
	if provider, ok := s.adapter.(RuntimeSupportProvider); ok {
		if runtimes := provider.SupportedRuntimes(); len(runtimes) > 0 {
			metadata["supportedRuntimes"] = strings.Join(runtimes, ",")
		}
	}
	return metadata
}

func (s *Server) handleStartTurn(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeSafeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var request harness.StartTurnRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeSafeError(w, http.StatusBadRequest, "invalid JSON request")
		return
	}
	if err := request.Validate(); err != nil {
		writeSafeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateTurnPathSegment(request.TurnID); err != nil {
		writeSafeError(w, http.StatusBadRequest, err.Error())
		return
	}

	state := newTurnState(request, s.now)
	s.mu.Lock()
	if _, exists := s.turns[request.TurnID]; exists {
		s.mu.Unlock()
		writeSafeError(w, http.StatusConflict, "turn already exists")
		return
	}
	if s.activeTurns >= 1 {
		s.mu.Unlock()
		writeSafeError(w, http.StatusConflict, "maximum concurrent turns reached")
		return
	}
	s.turns[request.TurnID] = state
	s.activeTurns++
	s.mu.Unlock()

	go s.runTurn(state)
	harness.WriteJSON(w, http.StatusAccepted, harness.StartTurnResponse{
		Version:          harness.ProtocolVersion,
		Accepted:         true,
		RuntimeSessionID: request.RuntimeSessionID,
		TurnID:           request.TurnID,
		CorrelationID:    request.CorrelationID,
		EventStreamPath:  harness.TurnsPath + "/" + path.Clean(string(request.TurnID)) + "/events",
	})
}

func (s *Server) handleTurn(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	trimmed := strings.TrimPrefix(r.URL.Path, harness.TurnsPath+"/")
	parts := strings.Split(strings.Trim(trimmed, "/"), "/")
	if len(parts) != 2 {
		writeSafeError(w, http.StatusNotFound, "not found")
		return
	}
	turnID := harness.HarnessTurnID(parts[0])
	if err := validateTurnPathSegment(turnID); err != nil {
		writeSafeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.mu.RLock()
	turn := s.turns[turnID]
	s.mu.RUnlock()
	if turn == nil {
		writeSafeError(w, http.StatusNotFound, "turn not found")
		return
	}
	switch parts[1] {
	case "events":
		if r.Method != http.MethodGet {
			writeSafeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.handleEvents(w, r, turn)
	case "cancel":
		if r.Method != http.MethodPost {
			writeSafeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.handleCancel(w, r, turn)
	default:
		writeSafeError(w, http.StatusNotFound, "not found")
	}
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request, turn *turnState) {
	var request harness.CancelTurnRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeSafeError(w, http.StatusBadRequest, "invalid JSON request")
		return
	}
	if err := request.Validate(); err != nil {
		writeSafeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := turn.matchesCancel(request); err != nil {
		writeSafeError(w, http.StatusBadRequest, err.Error())
		return
	}
	turn.cancel()
	harness.WriteJSON(w, http.StatusAccepted, harness.CancelTurnResponse{
		Version:          harness.ProtocolVersion,
		Accepted:         true,
		RuntimeSessionID: request.RuntimeSessionID,
		TurnID:           request.TurnID,
		CorrelationID:    request.CorrelationID,
		Message:          "cancel accepted",
	})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request, turn *turnState) {
	afterSeq, err := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("afterSeq")), 10, 64)
	if err != nil && strings.TrimSpace(r.URL.Query().Get("afterSeq")) != "" {
		writeSafeError(w, http.StatusBadRequest, "afterSeq must be an integer")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	nextSeq := afterSeq + 1
	for {
		frames, closed := turn.framesFrom(nextSeq)
		for _, frame := range frames {
			if err := harness.WriteSSEFrame(w, frame); err != nil {
				return
			}
			nextSeq = frame.Seq + 1
		}
		if closed {
			_ = harness.WriteSSEDone(w)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (s *Server) runTurn(turn *turnState) {
	defer s.finishTurn(turn)
	ctx := turn.ctx
	if !turn.request.Deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, turn.request.Deadline)
		defer cancel()
	}
	turnCtx := turnContextFromRequest(s.adapter.Name(), s.config, turn.request)
	if eventing, ok := s.adapter.(EventingAdapter); ok {
		_, err := eventing.RunTurn(ctx, turnCtx, func(frame harness.HarnessEventFrame) error {
			turn.appendFrame(s.normalizeFrame(turn, frame))
			return nil
		})
		if err != nil && !turn.hasTerminal() {
			turn.appendFrame(s.failedFrame(turn, "adapter_error", err.Error(), false))
		}
		return
	}

	turn.appendFrame(s.frame(turn, harness.FrameTurnStarted, "turn started", nil))
	ClearTurnArtifacts()
	defer ClearTurnArtifacts()
	restoreWorkspaceEnv := setTemporaryEnvEntries(turnCtx.Env)
	preparedWorkspace, err := prepareTurnWorkspace(ctx, turnCtx)
	restoreWorkspaceEnv()
	if err != nil {
		turn.appendFrame(s.failedFrame(turn, "workspace_prepare_failed", err.Error(), false))
		return
	}
	defer preparedWorkspace.cleanup()
	turnCtx.WorkDir = preparedWorkspace.workDir
	agentCfg, err := PrepareTurnContext(ctx, &turnCtx, preparedWorkspace.rootDir)
	if err != nil {
		turn.appendFrame(s.failedFrame(turn, "workspace_prepare_failed", err.Error(), false))
		return
	}
	spec, err := s.adapter.BuildCommand(ctx, turnCtx)
	if err != nil {
		turn.appendFrame(s.failedFrame(turn, "build_command_failed", err.Error(), false))
		return
	}
	defer removeTempFiles(spec.TempFiles)
	if spec.Dir != "" {
		turnCtx.WorkDir = spec.Dir
	}
	turn.appendFrame(s.runtimeLogFrame(turn, "runtime command started", map[string]any{
		"runtime": s.adapter.Name(),
		"command": path.Base(spec.Path),
	}))
	run, runErr := s.runner.Run(ctx, spec)
	if strings.TrimSpace(run.Stdout) != "" {
		turn.appendFrame(s.outputFrame(turn, "stdout", run.Stdout))
	}
	if strings.TrimSpace(run.Stderr) != "" {
		turn.appendFrame(s.runtimeLogTextFrame(turn, "stderr", run.Stderr, events.ExecutionEventSeverityWarning))
	}
	finalizedWorkDir := ""
	switch {
	case run.Cancelled:
		turn.appendFrame(s.frame(turn, harness.FrameTurnCancelled, "turn cancelled", nil))
	case run.TimedOut:
		turn.appendFrame(s.failedFrame(turn, "timeout", "runtime command timed out", true))
	case runErr != nil:
		msg := runErr.Error()
		if strings.TrimSpace(run.Stderr) != "" {
			msg = run.Stderr
		}
		turn.appendFrame(s.failedFrame(turn, "command_failed", msg, false))
	default:
		restoreTurnEnv := setTemporaryEnvEntries(turnCtx.Env)
		defer restoreTurnEnv()
		parsed, parseErr := s.adapter.ParseResult(ctx, turnCtx, run)
		if parseErr != nil {
			if strings.Contains(parseErr.Error(), "terminal frame limit") {
				turn.appendFrame(s.failedFrame(turn, "result_too_large", parseErr.Error(), false))
				return
			}
			turn.appendFrame(s.failedFrame(turn, "result_parse_failed", parseErr.Error(), false))
			return
		}
		if result, artifactErr := EnsureTurnRequiredSecurityArtifacts(ctx, agentCfg, parsed.Result); artifactErr != nil {
			turn.appendFrame(s.failedFrame(turn, "required_security_artifacts_missing", artifactErr.Error(), false))
			return
		} else {
			parsed.Result = result
		}
		if ShouldFinalizeWorkDir(turnCtx.WorkDir) {
			finalized, finalizeErr := FinalizeTurnResult(turnCtx.WorkDir, parsed.Result)
			if finalizeErr != nil {
				turn.appendFrame(s.failedFrame(turn, "result_finalize_failed", finalizeErr.Error(), false))
				return
			}
			parsed.Result = string(finalized)
			finalizedWorkDir = turnCtx.WorkDir
		}
		if len([]byte(parsed.Result)) > maxTerminalResultBytes {
			turn.appendFrame(s.failedFrame(
				turn,
				"result_too_large",
				"runtime result exceeded harness terminal frame limit",
				false,
			))
			return
		}
		if artifactErr := UploadTurnArtifacts(turnCtx); artifactErr != nil {
			turn.appendFrame(s.runtimeLogTextFrame(
				turn,
				"artifact-upload",
				artifactErr.Error(),
				events.ExecutionEventSeverityWarning,
			))
		}
		if finalizedWorkDir != "" {
			if cleanErr := CleanFinalizedWorkDir(finalizedWorkDir); cleanErr != nil {
				turn.appendFrame(s.failedFrame(turn, "workdir_cleanup_failed", cleanErr.Error(), false))
				return
			}
		}
		turn.appendFrame(s.completedFrame(turn, parsed))
	}
}

func (s *Server) frame(turn *turnState, typ harness.FrameType, summary string, terminal any) harness.HarnessEventFrame {
	frame := harness.HarnessEventFrame{
		Version:          harness.ProtocolVersion,
		Type:             typ,
		RuntimeSessionID: turn.request.RuntimeSessionID,
		TurnID:           turn.request.TurnID,
		CorrelationID:    turn.request.CorrelationID,
		CreatedAt:        s.now().UTC(),
		Severity:         events.ExecutionEventSeverityInfo,
		Summary:          events.RedactExecutionEventText(summary),
		Metadata: map[string]string{
			"runtime": s.adapter.Name(),
			"mode":    "observed",
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

func (s *Server) normalizeFrame(turn *turnState, frame harness.HarnessEventFrame) harness.HarnessEventFrame {
	if frame.Version == "" {
		frame.Version = harness.ProtocolVersion
	}
	if frame.RuntimeSessionID == "" {
		frame.RuntimeSessionID = turn.request.RuntimeSessionID
	}
	if frame.TurnID == "" {
		frame.TurnID = turn.request.TurnID
	}
	if frame.CorrelationID == "" {
		frame.CorrelationID = turn.request.CorrelationID
	}
	if frame.CreatedAt.IsZero() {
		frame.CreatedAt = s.now().UTC()
	}
	if frame.Severity == "" {
		frame.Severity = events.ExecutionEventSeverityInfo
	}
	frame.Summary = events.RedactExecutionEventText(frame.Summary)
	frame.ContentText = redactAndTruncate(frame.ContentText, events.MaxExecutionEventContentTextChars)
	if len(frame.Metadata) > 0 {
		metadata := make(map[string]string, len(frame.Metadata))
		for key, value := range frame.Metadata {
			metadata[key] = events.RedactExecutionEventText(value)
		}
		frame.Metadata = metadata
	}
	if frame.Completed != nil {
		completed := *frame.Completed
		completed.Result = redactAndTruncateBytes(completed.Result, maxTerminalResultBytes)
		completed.OutputRef = events.RedactExecutionEventText(completed.OutputRef)
		frame.Completed = &completed
	}
	if frame.Failed != nil {
		failed := *frame.Failed
		failed.Reason = events.RedactExecutionEventText(failed.Reason)
		failed.Message = redactAndTruncate(failed.Message, events.MaxExecutionEventSummaryChars)
		frame.Failed = &failed
	}
	if frame.Error != nil {
		errorInfo := *frame.Error
		errorInfo.Code = events.RedactExecutionEventText(errorInfo.Code)
		errorInfo.Message = redactAndTruncate(errorInfo.Message, events.MaxExecutionEventSummaryChars)
		frame.Error = &errorInfo
	}
	if len(frame.Content) > 0 {
		if sanitized, _, err := events.SanitizeExecutionEventJSON(frame.Content); err == nil {
			frame.Content = sanitized
		} else {
			frame.Content = nil
		}
	}
	return frame
}

func (s *Server) runtimeLogFrame(turn *turnState, summary string, content map[string]any) harness.HarnessEventFrame {
	encoded, _ := json.Marshal(content)
	frame := s.frame(turn, harness.FrameRuntimeLog, summary, nil)
	frame.Content = encoded
	return frame
}

func (s *Server) runtimeLogTextFrame(turn *turnState, stream, text, severity string) harness.HarnessEventFrame {
	frame := s.runtimeLogFrame(turn, "runtime "+stream, map[string]any{"stream": stream})
	frame.ContentText = redactAndTruncate(text, events.MaxExecutionEventContentTextChars)
	frame.Severity = events.NormalizeExecutionEventSeverity(severity)
	return frame
}

func (s *Server) outputFrame(turn *turnState, stream, text string) harness.HarnessEventFrame {
	frame := s.frame(turn, harness.FrameRuntimeOutput, "runtime "+stream, nil)
	frame.ContentText = redactAndTruncate(text, events.MaxExecutionEventContentTextChars)
	encoded, _ := json.Marshal(map[string]any{"stream": stream, "preview": frame.ContentText})
	frame.Content = encoded
	return frame
}

func (s *Server) completedFrame(turn *turnState, result TurnResult) harness.HarnessEventFrame {
	frame := s.frame(turn, harness.FrameTurnCompleted, "turn completed", &harness.TurnCompleted{
		Result:        redactAndTruncateBytes(result.Result, maxTerminalResultBytes),
		OutputRef:     events.RedactExecutionEventText(result.OutputRef),
		RetainSession: false,
	})
	if len(result.Metadata) > 0 {
		for k, v := range result.Metadata {
			frame.Metadata[k] = events.RedactExecutionEventText(v)
		}
	}
	return frame
}

func (s *Server) failedFrame(turn *turnState, reason, message string, retryable bool) harness.HarnessEventFrame {
	return s.frame(turn, harness.FrameTurnFailed, "turn failed", &harness.TurnFailed{
		Reason:    events.RedactExecutionEventText(reason),
		Message:   redactAndTruncate(message, events.MaxExecutionEventSummaryChars),
		Retryable: retryable,
	})
}

func redactAndTruncate(value string, maxChars int) string {
	out, _, _ := events.RedactAndTruncateExecutionEventText(value, maxChars)
	return out
}

func redactAndTruncateBytes(value string, maxBytes int) string {
	redacted := events.RedactExecutionEventText(value)
	if maxBytes <= 0 {
		return ""
	}
	if len([]byte(redacted)) <= maxBytes {
		return redacted
	}
	if maxBytes <= utf8.RuneLen('…') {
		return "…"
	}
	limit := maxBytes - utf8.RuneLen('…')
	var out strings.Builder
	out.Grow(limit + utf8.RuneLen('…'))
	for _, r := range redacted {
		w := utf8.RuneLen(r)
		if w < 0 {
			w = len(string(r))
		}
		if out.Len()+w > limit {
			break
		}
		out.WriteRune(r)
	}
	out.WriteRune('…')
	return out.String()
}

func writeSafeError(w http.ResponseWriter, status int, message string) {
	harness.WriteError(w, status, events.RedactExecutionEventText(message))
}

func validateTurnPathSegment(turnID harness.HarnessTurnID) error {
	value := strings.TrimSpace(string(turnID))
	if value == "" {
		return fmt.Errorf("turn id is required")
	}
	if value == "." || value == ".." || strings.Contains(value, "/") || strings.Contains(value, "\\") {
		return fmt.Errorf("turn id must be a single safe path segment")
	}
	return nil
}

type turnState struct {
	request harness.StartTurnRequest
	ctx     context.Context
	cancel  context.CancelFunc
	now     func() time.Time

	mu       sync.Mutex
	frames   []harness.HarnessEventFrame
	terminal bool
	closed   bool
}

func newTurnState(request harness.StartTurnRequest, now func() time.Time) *turnState {
	ctx, cancel := context.WithCancel(context.Background())
	return &turnState{request: request, ctx: ctx, cancel: cancel, now: now}
}

func (t *turnState) appendFrame(frame harness.HarnessEventFrame) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if frame.Seq <= 0 {
		frame.Seq = int64(len(t.frames) + 1)
	}
	if frame.Completed != nil && frame.Completed.FinalEventSeq == 0 {
		frame.Completed.FinalEventSeq = frame.Seq
	}
	t.frames = append(t.frames, frame)
	switch frame.Type {
	case harness.FrameTurnCompleted, harness.FrameTurnFailed, harness.FrameTurnCancelled:
		t.terminal = true
	}
}

func (t *turnState) framesFrom(seq int64) ([]harness.HarnessEventFrame, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	frames := make([]harness.HarnessEventFrame, 0)
	for _, frame := range t.frames {
		if frame.Seq >= seq {
			frames = append(frames, frame)
		}
	}
	return frames, t.closed
}

func (t *turnState) close() {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
}

func (t *turnState) hasTerminal() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.terminal
}

func (t *turnState) matchesCancel(request harness.CancelTurnRequest) error {
	if request.RuntimeSessionID != t.request.RuntimeSessionID {
		return fmt.Errorf("cancel runtime session %q does not match turn runtime session", request.RuntimeSessionID)
	}
	if request.TurnID != t.request.TurnID {
		return fmt.Errorf("cancel turn %q does not match started turn", request.TurnID)
	}
	if request.Namespace != t.request.Namespace ||
		request.TaskName != t.request.TaskName ||
		request.SessionName != t.request.SessionName {
		return fmt.Errorf("cancel request does not match started turn owner")
	}
	return nil
}
