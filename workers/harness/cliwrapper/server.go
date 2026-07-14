package cliwrapper

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/harness"
	"github.com/orka-agents/orka/internal/tracing"
	"github.com/orka-agents/orka/internal/workerenv"
	"github.com/orka-agents/orka/workers/common"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const (
	maxTerminalResultBytes = 512 * 1024
	localOutputRef         = "cliwrapper-result-v1"
)

type Server struct {
	config  Config
	adapter RuntimeAdapter
	runner  CommandRunner
	now     func() time.Time

	turnRegistry *turnRegistry
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
		config:       cfg,
		adapter:      adapter,
		runner:       NewCommandRunner(cfg),
		now:          time.Now,
		turnRegistry: newTurnRegistry(),
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
	want, err := s.currentAuthValue()
	if err != nil {
		writeSafeError(w, http.StatusServiceUnavailable, "wrapper auth token is not configured")
		return false
	}
	if want == "" {
		writeSafeError(w, http.StatusServiceUnavailable, "wrapper auth token is not configured")
		return false
	}
	got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		writeSafeError(w, http.StatusUnauthorized, "unauthorized")
		return false
	}
	return true
}

func (s *Server) finishTurn(turn *turnState) {
	turn.close()
	s.turnRegistry.finishActive()
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
	if s.config.TurnRetention > 0 && turn.hasUnfetchedOutput() && turn.outputRetentionActive() {
		s.scheduleTurnEviction(turn)
		return
	}
	s.turnRegistry.evict(turn)
	turn.cleanupOutput()
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

func (s *Server) currentAuthValue() (string, error) {
	if file := strings.TrimSpace(s.config.AuthValueFile); file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(data)), nil
	}
	return strings.TrimSpace(s.config.AuthValue), nil
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
	eventStreamPath, err := harness.EventStreamPath(request.TurnID)
	if err != nil {
		writeSafeError(w, http.StatusBadRequest, err.Error())
		return
	}

	state, err := s.turnRegistry.admit(request, s.now)
	if err != nil {
		switch {
		case errors.Is(err, errTurnAlreadyExists):
			writeSafeError(w, http.StatusConflict, "turn already exists")
		case errors.Is(err, errTurnAlreadyCompleted):
			// This turn ID was already accepted and run to completion (then evicted).
			// Re-accepting it would duplicate external side effects (branch push, PR
			// creation, token spend), so reject deterministically.
			writeSafeError(w, http.StatusConflict, "turn already completed")
		case errors.Is(err, errMaximumConcurrentTurns):
			writeSafeError(w, http.StatusConflict, "maximum concurrent turns reached")
		default:
			writeSafeError(w, http.StatusInternalServerError, "failed to admit turn")
		}
		return
	}

	go s.runTurn(state)
	harness.WriteJSON(w, http.StatusAccepted, harness.StartTurnResponse{
		Version:          harness.ProtocolVersion,
		Accepted:         true,
		RuntimeSessionID: request.RuntimeSessionID,
		TurnID:           request.TurnID,
		CorrelationID:    request.CorrelationID,
		EventStreamPath:  eventStreamPath,
	})
}

func (s *Server) handleTurn(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	turnID, resource, err := harness.ParseTurnResourcePath(r.URL.EscapedPath())
	if err != nil {
		if errors.Is(err, harness.ErrTurnPathNotFound) {
			writeSafeError(w, http.StatusNotFound, "not found")
		} else {
			writeSafeError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	turn := s.turnRegistry.lookup(turnID)
	if turn == nil {
		writeSafeError(w, http.StatusNotFound, "turn not found")
		return
	}
	switch resource {
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
	case "output":
		if r.Method != http.MethodGet {
			writeSafeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.handleOutput(w, r, turn)
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

func (s *Server) handleOutput(w http.ResponseWriter, r *http.Request, turn *turnState) {
	if ref := strings.TrimSpace(r.URL.Query().Get("ref")); ref != localOutputRef {
		writeSafeError(w, http.StatusNotFound, "output not found")
		return
	}
	data, ok, err := turn.output()
	if err != nil {
		writeSafeError(w, http.StatusInternalServerError, "failed to read turn output")
		return
	}
	if !ok {
		writeSafeError(w, http.StatusNotFound, "output not found")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	if _, err := w.Write(data); err == nil {
		turn.markOutputFetched()
	}
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

func (s *Server) runTurn(turn *turnState) { //nolint:gocyclo
	defer s.finishTurn(turn)
	ctx := extractHarnessTurnTraceContext(turn.ctx, turn.request)
	if deadline := turn.deadline(); !deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	agentName := strings.TrimSpace(turn.request.Metadata["agentName"])
	if agentName == "" {
		agentName = s.adapter.Name()
	}
	ctx, taskSpan := tracing.Tracer("orka.harness").Start(ctx, "task.run", trace.WithAttributes(
		tracing.TaskAttributes(turn.request.TaskName, turn.request.Namespace, turn.request.Namespace, agentName, "")...,
	))
	defer func() {
		if failed, errType := turnTerminalFailure(turn); failed {
			taskSpan.SetStatus(codes.Error, errType)
			taskSpan.SetAttributes(attribute.String("error.type", errType))
		}
		taskSpan.End()
	}()
	turnCtx := turn.materializeContext(s.adapter.Name(), s.config)
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
	restoreWorkspaceEnv := setTemporaryEnvEntries(turnCtx.Env)
	preparedWorkspace, err := prepareTurnWorkspace(ctx, turnCtx)
	restoreWorkspaceEnv()
	if err != nil {
		appendWorkspacePreparationFailure(s, turn, ctx, err)
		return
	}
	defer preparedWorkspace.cleanup()
	turnCtx.WorkDir = preparedWorkspace.workDir
	turnCtx.RootDir = preparedWorkspace.rootDir
	turnArtifactsDir := turnArtifactDir(preparedWorkspace)
	defer ClearTurnArtifacts(turnArtifactsDir)
	if err := prepareTurnArtifactsDirForWrapper(turnArtifactsDir); err != nil {
		turn.appendFrame(s.failedFrame(turn, "workspace_prepare_failed", err.Error(), false))
		return
	}
	if preparedWorkspace.baseDir != "" {
		turnCtx.SkillsRoot = filepath.Join(preparedWorkspace.baseDir, "skills")
	}
	restoreChildIdentity := suspendChildIdentity()
	agentCfg, err := PrepareTurnContext(ctx, &turnCtx, preparedWorkspace.rootDir, turnArtifactsDir)
	restoreChildIdentity()
	if err != nil {
		appendWorkspacePreparationFailure(s, turn, ctx, err)
		return
	}
	if err := ensureWorkspaceArtifactsLinkForTurn(
		preparedWorkspace.rootDir,
		turnCtx.WorkDir,
		turnArtifactsDir,
	); err != nil {
		turn.appendFrame(s.failedFrame(turn, "workspace_prepare_failed", err.Error(), false))
		return
	}
	turnHomeRoot, err := os.MkdirTemp("/tmp", "orka-harness-home-*")
	if err != nil {
		turn.appendFrame(s.failedFrame(turn, "workspace_prepare_failed", err.Error(), false))
		return
	}
	if _, _, ok := childCredentialIDs(); ok {
		if err := os.Chmod(turnHomeRoot, 0o711); err != nil {
			turn.appendFrame(s.failedFrame(turn, "workspace_prepare_failed", err.Error(), false))
			return
		}
	}
	turnHome := filepath.Join(turnHomeRoot, "home")
	defer func() { _ = cleanupTurnWorkspacePath(turnHomeRoot) }()
	if err := os.MkdirAll(turnHome, 0o700); err != nil {
		turn.appendFrame(s.failedFrame(turn, "workspace_prepare_failed", err.Error(), false))
		return
	}
	if err := prepareHomeForChild(turnHome); err != nil {
		turn.appendFrame(s.failedFrame(turn, "workspace_prepare_failed", err.Error(), false))
		return
	}
	turnCtx.Env = setEnv(turnCtx.Env, "HOME", turnHome)
	if strings.EqualFold(strings.TrimSpace(turnCtx.Metadata["readOnly"]), "true") {
		turnCtx.Env = removeTurnEnv(
			turnCtx.Env,
			workerenv.GitToken,
			workerenv.GitHubToken,
			workerenv.GitAskpass,
			workerenv.GitUsername,
		)
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
	if preparedWorkspace.baseDir != "" &&
		(preparedWorkspace.baseDir != preparedWorkspace.rootDir || preparedWorkspace.ownedBaseDir) {
		if err := ensureDirectoryTraversable(preparedWorkspace.baseDir); err != nil {
			turn.appendFrame(s.failedFrame(turn, "workspace_prepare_failed", err.Error(), false))
			return
		}
	}
	if err := chownTreeForChild(preparedWorkspace.rootDir, turnArtifactsDir); err != nil {
		turn.appendFrame(s.failedFrame(turn, "workspace_prepare_failed", err.Error(), false))
		return
	}
	if err := prepareArtifactsForChild(turnArtifactsDir); err != nil {
		turn.appendFrame(s.failedFrame(turn, "workspace_prepare_failed", err.Error(), false))
		return
	}
	turn.appendFrame(s.runtimeLogFrame(turn, "runtime command started", map[string]any{
		"runtime": s.adapter.Name(),
		"command": path.Base(spec.Path),
	}))
	run, runErr := s.runner.Run(ctx, spec)
	if run.FullStdoutTruncated && strings.TrimSpace(spec.ResultFile) == "" {
		turn.appendFrame(s.failedFrame(turn, "result_too_large", "runtime stdout exceeded harness storage limit", false))
		return
	}
	if strings.TrimSpace(run.Stdout) != "" {
		turn.appendFrame(s.outputFrame(turn, "stdout", run.Stdout))
	}
	if strings.TrimSpace(run.Stderr) != "" {
		turn.appendFrame(s.runtimeLogTextFrame(turn, "stderr", run.Stderr))
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
		partial, hasAdapterResult := s.failedTurnPartialResult(ctx, turnCtx, run)
		removeControlFiles(turnCtx.WorkDir, append(spec.TempFiles, spec.ResultFile)...)
		if run.FullStdoutTruncated && !hasAdapterResult {
			turn.appendFrame(s.failedFrame(turn, "result_too_large", "runtime stdout exceeded harness storage limit", false))
			return
		}
		finalizeWorkDir := turnCtx.WorkDir
		if preparedWorkspace.rootDir != "" {
			finalizeWorkDir = preparedWorkspace.rootDir
		}
		if !envEntryIsTrue(turnCtx.Env, workerenv.ResultStdout) && ShouldFinalizeWorkDir(finalizeWorkDir) {
			restoreTurnEnv := setTemporaryEnvEntries(turnCtx.Env)
			if finalized, finalizeErr := FinalizeTurnResult(finalizeWorkDir, partial); finalizeErr == nil {
				partial = string(finalized)
				finalizedWorkDir = finalizeWorkDir
			}
			restoreTurnEnv()
		}
		if artifactErr := UploadTurnArtifacts(turnCtx, turnArtifactsDir); artifactErr != nil {
			turn.appendFrame(s.runtimeLogTextFrame(
				turn,
				"artifact-upload",
				artifactErr.Error(),
			))
		}
		if finalizedWorkDir != "" {
			if cleanErr := CleanFinalizedWorkDir(finalizedWorkDir); cleanErr != nil {
				turn.appendFrame(s.runtimeLogTextFrame(turn, "workdir-cleanup", cleanErr.Error()))
			}
		}
		if len([]byte(partial)) > maxStoredResultBytes {
			turn.appendFrame(s.failedFrame(turn, "result_too_large", "runtime result exceeded harness storage limit", false))
			return
		}
		turn.appendFrame(s.failedFrameWithResult(turn, "command_failed", msg, partial, false))
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
		if result, artifactErr := EnsureTurnRequiredSecurityArtifacts(
			ctx,
			agentCfg,
			parsed.Result,
			s.securityArtifactFollowUp(turn, turnCtx),
			turnArtifactsDir,
		); artifactErr != nil {
			turn.appendFrame(s.failedFrame(turn, "required_security_artifacts_missing", artifactErr.Error(), false))
			return
		} else {
			parsed.Result = result
		}
		removeControlFiles(turnCtx.WorkDir, append(spec.TempFiles, spec.ResultFile)...)
		finalizeWorkDir := turnCtx.WorkDir
		if preparedWorkspace.rootDir != "" {
			finalizeWorkDir = preparedWorkspace.rootDir
		}
		if !envEntryIsTrue(turnCtx.Env, workerenv.ResultStdout) && ShouldFinalizeWorkDir(finalizeWorkDir) {
			finalized, finalizeErr := FinalizeTurnResult(finalizeWorkDir, parsed.Result)
			if finalizeErr != nil {
				turn.appendFrame(s.failedFrame(turn, "result_finalize_failed", finalizeErr.Error(), false))
				return
			}
			parsed.Result = string(finalized)
			finalizedWorkDir = finalizeWorkDir
		}
		if len([]byte(parsed.Result)) > maxStoredResultBytes {
			turn.appendFrame(s.failedFrame(
				turn,
				"result_too_large",
				"runtime result exceeded harness storage limit",
				false,
			))
			return
		}
		if artifactErr := UploadTurnArtifacts(turnCtx, turnArtifactsDir); artifactErr != nil {
			turn.appendFrame(s.runtimeLogTextFrame(
				turn,
				"artifact-upload",
				artifactErr.Error(),
			))
		}
		if finalizedWorkDir != "" {
			if cleanErr := CleanFinalizedWorkDir(finalizedWorkDir); cleanErr != nil {
				turn.appendFrame(s.runtimeLogTextFrame(
					turn,
					"workdir-cleanup",
					cleanErr.Error(),
				))
			}
		}
		if frameErr := s.appendCompletedFrame(turn, parsed); frameErr != nil {
			turn.appendFrame(s.failedFrame(turn, "result_store_failed", frameErr.Error(), false))
			return
		}
	}
}

func extractHarnessTurnTraceContext(ctx context.Context, request harness.StartTurnRequest) context.Context {
	carrier := tracing.MapCarrier{}
	if request.Metadata != nil {
		carrier["traceparent"] = request.Metadata["traceparent"]
		carrier["tracestate"] = request.Metadata["tracestate"]
	}
	if carrier["traceparent"] == "" {
		return ctx
	}
	return tracing.ExtractContext(ctx, carrier)
}

func turnTerminalFailure(turn *turnState) (bool, string) {
	if turn == nil {
		return false, ""
	}
	turn.mu.Lock()
	defer turn.mu.Unlock()
	for i := len(turn.frames) - 1; i >= 0; i-- {
		if turn.frames[i].Type == harness.FrameTurnFailed {
			return true, "turn_failed"
		}
	}
	return false, ""
}

func (s *Server) failedTurnPartialResult(ctx context.Context, turnCtx TurnContext, run CommandResult) (string, bool) {
	partial := strings.TrimSpace(run.ExactStdout())
	hasAdapterResult := false
	if resultPath := strings.TrimSpace(run.ResultFile); resultPath != "" {
		if data, err := readBoundedResultFile(resultPath, turnCtx.WorkDir); err == nil &&
			!resultFileUnwritten(data.info) && strings.TrimSpace(data.contents) != "" {
			partial = strings.TrimSpace(data.contents)
			hasAdapterResult = true
		}
	}
	restoreTurnEnv := setTemporaryEnvEntries(turnCtx.Env)
	defer restoreTurnEnv()
	if parsed, err := s.adapter.ParseResult(ctx, turnCtx, run); err == nil && strings.TrimSpace(parsed.Result) != "" {
		partial = strings.TrimSpace(parsed.Result)
		hasAdapterResult = true
	}
	return partial, hasAdapterResult
}

func (s *Server) securityArtifactFollowUp(turn *turnState, base TurnContext) common.SecurityArtifactFollowUp {
	return func(ctx context.Context, prompt string) (string, error) {
		followTurn := base
		followTurn.Prompt = prompt
		followTurn.Env = setEnv(followTurn.Env, workerenv.Prompt, prompt)
		restoreFollowEnv := setTemporaryEnvEntries(followTurn.Env)
		defer restoreFollowEnv()
		spec, err := s.adapter.BuildCommand(ctx, followTurn)
		if err != nil {
			return "", err
		}
		defer removeTempFiles(spec.TempFiles)
		if spec.Dir != "" {
			followTurn.WorkDir = spec.Dir
		}
		turn.appendFrame(s.runtimeLogFrame(turn, "security artifact follow-up started", map[string]any{
			"runtime": s.adapter.Name(),
			"command": path.Base(spec.Path),
		}))
		run, runErr := s.runner.Run(ctx, spec)
		if strings.TrimSpace(run.Stdout) != "" {
			turn.appendFrame(s.outputFrame(turn, "stdout", run.Stdout))
		}
		if strings.TrimSpace(run.Stderr) != "" {
			turn.appendFrame(s.runtimeLogTextFrame(turn, "stderr", run.Stderr))
		}
		if runErr != nil {
			return strings.TrimSpace(run.Stdout), runErr
		}
		parsed, parseErr := s.adapter.ParseResult(ctx, followTurn, run)
		if parseErr != nil {
			return strings.TrimSpace(run.Stdout), parseErr
		}
		return parsed.Result, nil
	}
}

func turnArtifactDir(workspace preparedWorkspace) string {
	if workspace.baseDir != "" {
		return filepath.Join(workspace.baseDir, "artifacts")
	}
	if workspace.rootDir != "" {
		return filepath.Join(workspace.rootDir, ".orka-runtime-artifacts")
	}
	return filepath.Join(os.TempDir(), "orka-runtime-artifacts")
}

// prepareTurnArtifactsDirForWrapper creates the per-turn artifact directory for
// root-side workspace prep. Child ownership is applied later after the workspace
// tree has been chowned for the child.
func prepareTurnArtifactsDirForWrapper(artifactDir string) error {
	if err := os.MkdirAll(artifactDir, 0o770); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		if err := os.Lchown(artifactDir, 0, 0); err != nil {
			return err
		}
	}
	return os.Chmod(artifactDir, 0o770)
}

func ensureDirectoryTraversable(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	mode := info.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
	return os.Chmod(dir, mode|0o111)
}

func ensureWorkspaceArtifactsLinkForTurn(rootDir, workDir, artifactDir string) error {
	if workDir == "" || rootDir == "" || workDir == rootDir {
		return nil
	}
	restoreArtifactDir := setTemporaryEnv("ORKA_ARTIFACTS_DIR", artifactDir)
	defer restoreArtifactDir()
	return common.EnsureWorkspaceArtifactsLink(workDir)
}

func (s *Server) frame(turn *turnState, typ harness.FrameType, summary string, terminal any) harness.HarnessEventFrame {
	frame := harness.HarnessEventFrame{
		Version:          harness.ProtocolVersion,
		Type:             typ,
		RuntimeSessionID: turn.runtimeSessionID(),
		TurnID:           turn.id(),
		CorrelationID:    turn.correlationID(),
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
		frame.RuntimeSessionID = turn.runtimeSessionID()
	}
	if frame.TurnID == "" {
		frame.TurnID = turn.id()
	}
	if frame.CorrelationID == "" {
		frame.CorrelationID = turn.correlationID()
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
		failed.Result = redactAndTruncateBytes(failed.Result, 64<<10)
		failed.OutputRef = events.RedactExecutionEventText(failed.OutputRef)
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

func (s *Server) runtimeLogTextFrame(turn *turnState, stream, text string) harness.HarnessEventFrame {
	frame := s.runtimeLogFrame(turn, "runtime "+stream, map[string]any{"stream": stream})
	frame.ContentText = redactAndTruncate(text, events.MaxExecutionEventContentTextChars)
	frame.Severity = events.ExecutionEventSeverityWarning
	return frame
}

func (s *Server) outputFrame(turn *turnState, stream, text string) harness.HarnessEventFrame {
	frame := s.frame(turn, harness.FrameRuntimeOutput, "runtime "+stream, nil)
	frame.ContentText = redactAndTruncate(text, events.MaxExecutionEventContentTextChars)
	encoded, _ := json.Marshal(map[string]any{"stream": stream, "preview": frame.ContentText})
	frame.Content = encoded
	return frame
}

func (s *Server) appendCompletedFrame(turn *turnState, result TurnResult) error {
	completed, err := s.completedFrame(turn, result)
	if err != nil {
		return err
	}
	turn.appendFrame(completed)
	return nil
}

func (s *Server) completedFrame(turn *turnState, result TurnResult) (harness.HarnessEventFrame, error) {
	outputRef := strings.TrimSpace(result.OutputRef)
	if result.Result != "" && outputRef == "" {
		var err error
		outputRef, err = turn.storeOutput(result.Result)
		if err != nil {
			return harness.HarnessEventFrame{}, err
		}
	}
	previewLimit := maxTerminalResultBytes
	if outputRef != "" {
		previewLimit = 64 << 10
	}
	preview := redactAndTruncateBytes(result.Result, previewLimit)
	frame := s.frame(turn, harness.FrameTurnCompleted, "turn completed", &harness.TurnCompleted{
		Result:        preview,
		OutputRef:     events.RedactExecutionEventText(outputRef),
		RetainSession: false,
	})
	if len(result.Metadata) > 0 {
		for k, v := range result.Metadata {
			frame.Metadata[k] = events.RedactExecutionEventText(v)
		}
	}
	return frame, nil
}

func (s *Server) failedFrame(turn *turnState, reason, message string, retryable bool) harness.HarnessEventFrame {
	return s.frame(turn, harness.FrameTurnFailed, "turn failed", &harness.TurnFailed{
		Reason:    events.RedactExecutionEventText(reason),
		Message:   redactAndTruncate(message, events.MaxExecutionEventSummaryChars),
		Retryable: retryable,
	})
}

func (s *Server) failedFrameWithResult(
	turn *turnState,
	reason string,
	message string,
	result string,
	retryable bool,
) harness.HarnessEventFrame {
	frame := s.failedFrame(turn, reason, message, retryable)
	if strings.TrimSpace(result) == "" || frame.Failed == nil {
		return frame
	}
	outputRef, err := turn.storeOutput(result)
	if err != nil {
		frame.Metadata["outputRefError"] = events.RedactExecutionEventText(err.Error())
		return frame
	}
	frame.Failed.Result = redactAndTruncateBytes(result, 64<<10)
	frame.Failed.OutputRef = events.RedactExecutionEventText(outputRef)
	return frame
}

func redactAndTruncate(value string, maxChars int) string {
	out, _, _ := events.RedactAndTruncateExecutionEventText(value, maxChars)
	return out
}

func redactAndTruncateBytes(value string, maxBytes int) string {
	return truncateBytes(events.RedactExecutionEventText(value), maxBytes)
}

func truncateBytes(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len([]byte(value)) <= maxBytes {
		return value
	}
	if maxBytes <= utf8.RuneLen('…') {
		return "…"
	}
	limit := maxBytes - utf8.RuneLen('…')
	var out strings.Builder
	out.Grow(limit + utf8.RuneLen('…'))
	for _, r := range value {
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

func removeControlFiles(workDir string, controlFiles ...string) {
	workDir = strings.TrimSpace(workDir)
	if workDir == "" {
		return
	}
	absWorkDir, err := filepath.Abs(workDir)
	if err != nil {
		return
	}
	for _, controlFile := range controlFiles {
		if strings.TrimSpace(controlFile) == "" {
			continue
		}
		absControlFile, err := filepath.Abs(controlFile)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(absWorkDir, absControlFile)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
			continue
		}
		_ = os.Remove(absControlFile)
	}
}

func removeTurnEnv(env []string, names ...string) []string {
	if len(env) == 0 || len(names) == 0 {
		return env
	}
	remove := make(map[string]struct{}, len(names))
	for _, name := range names {
		remove[strings.TrimSpace(name)] = struct{}{}
	}
	out := make([]string, 0, len(env))
	for _, entry := range env {
		name, _, ok := strings.Cut(entry, "=")
		if !ok {
			out = append(out, entry)
			continue
		}
		if _, shouldRemove := remove[name]; shouldRemove {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func writeSafeError(w http.ResponseWriter, status int, message string) {
	harness.WriteError(w, status, events.RedactExecutionEventText(message))
}

type turnIdentity struct {
	namespace        string
	taskName         string
	sessionName      string
	runtimeSessionID harness.RuntimeSessionID
	turnID           harness.HarnessTurnID
	correlationID    string
	deadline         time.Time
}

type turnState struct {
	request  harness.StartTurnRequest
	identity turnIdentity
	ctx      context.Context
	cancel   context.CancelFunc
	now      func() time.Time

	mu              sync.Mutex
	frames          []harness.HarnessEventFrame
	terminal        bool
	closed          bool
	resultPath      string
	resultRead      bool
	resultKeepUntil time.Time
}

func newTurnState(request harness.StartTurnRequest, now func() time.Time) *turnState {
	ctx, cancel := context.WithCancel(context.Background())
	return &turnState{
		request:  request,
		identity: identityFromStartTurnRequest(request),
		ctx:      ctx,
		cancel:   cancel,
		now:      now,
	}
}

func identityFromStartTurnRequest(request harness.StartTurnRequest) turnIdentity {
	return turnIdentity{
		namespace:        request.Namespace,
		taskName:         request.TaskName,
		sessionName:      request.SessionName,
		runtimeSessionID: request.RuntimeSessionID,
		turnID:           request.TurnID,
		correlationID:    request.CorrelationID,
		deadline:         request.Deadline,
	}
}

func (t *turnState) id() harness.HarnessTurnID {
	return t.identity.turnID
}

func (t *turnState) runtimeSessionID() harness.RuntimeSessionID {
	return t.identity.runtimeSessionID
}

func (t *turnState) correlationID() string {
	return t.identity.correlationID
}

func (t *turnState) deadline() time.Time {
	return t.identity.deadline
}

func (t *turnState) materializeContext(runtimeName string, cfg Config) TurnContext {
	t.mu.Lock()
	request := t.request
	t.request.Input.Env = nil
	t.mu.Unlock()
	return turnContextFromRequest(runtimeName, cfg, request)
}

func (t *turnState) storeOutput(result string) (string, error) {
	file, err := os.CreateTemp("", "harness-turn-output-*")
	if err != nil {
		return "", fmt.Errorf("create turn output file: %w", err)
	}
	outputPath := file.Name()
	if _, err := file.WriteString(result); err != nil {
		_ = file.Close()
		_ = os.Remove(outputPath)
		return "", fmt.Errorf("write turn output file: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(outputPath)
		return "", fmt.Errorf("close turn output file: %w", err)
	}
	t.mu.Lock()
	oldPath := t.resultPath
	t.resultPath = outputPath
	t.resultRead = false
	t.resultKeepUntil = t.now().Add(max(30*time.Minute, 6*DefaultTurnRetention))
	t.mu.Unlock()
	if oldPath != "" {
		_ = os.Remove(oldPath)
	}
	return localOutputRef, nil
}

func (t *turnState) output() ([]byte, bool, error) {
	t.mu.Lock()
	outputPath := t.resultPath
	t.mu.Unlock()
	if outputPath == "" {
		return nil, false, nil
	}
	file, err := os.Open(outputPath)
	if err != nil {
		return nil, false, fmt.Errorf("open turn output file: %w", err)
	}
	defer file.Close() //nolint:errcheck
	data, err := io.ReadAll(io.LimitReader(file, int64(maxStoredResultBytes)+1))
	if err != nil {
		return nil, false, fmt.Errorf("read turn output file: %w", err)
	}
	if len(data) > maxStoredResultBytes {
		return nil, false, fmt.Errorf("turn output exceeds harness storage limit")
	}
	return data, true, nil
}

func (t *turnState) hasUnfetchedOutput() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.resultPath != "" && !t.resultRead
}

func (t *turnState) outputRetentionActive() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return !t.resultKeepUntil.IsZero() && t.now().Before(t.resultKeepUntil)
}

func (t *turnState) markOutputFetched() {
	t.mu.Lock()
	t.resultRead = true
	t.mu.Unlock()
}

func (t *turnState) cleanupOutput() {
	t.mu.Lock()
	outputPath := t.resultPath
	t.resultPath = ""
	t.mu.Unlock()
	if outputPath != "" {
		_ = os.Remove(outputPath)
	}
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
	if request.RuntimeSessionID != t.identity.runtimeSessionID {
		return fmt.Errorf("cancel runtime session %q does not match turn runtime session", request.RuntimeSessionID)
	}
	if request.TurnID != t.identity.turnID {
		return fmt.Errorf("cancel turn %q does not match started turn", request.TurnID)
	}
	if request.Namespace != t.identity.namespace ||
		request.TaskName != t.identity.taskName ||
		request.SessionName != t.identity.sessionName {
		return fmt.Errorf("cancel request does not match started turn owner")
	}
	return nil
}
