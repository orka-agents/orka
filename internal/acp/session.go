package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

const (
	DefaultPromptLease        = 2 * time.Minute
	DefaultPermissionTimeout  = 5 * time.Minute
	DefaultBufferedEvents     = 256
	DefaultBufferedEventBytes = 32 << 20
	DefaultInitializeTimeout  = 60 * time.Second

	MinPermissionTimeout = time.Minute
	MaxPermissionTimeout = time.Hour
)

func ValidatePermissionTimeout(timeout time.Duration) error {
	if timeout < MinPermissionTimeout || timeout > MaxPermissionTimeout {
		return fmt.Errorf("permission timeout must be between %s and %s", MinPermissionTimeout, MaxPermissionTimeout)
	}
	return nil
}

var ErrPromptCancellationPending = errors.New("runtime session has a pending prompt cancellation write")

var ErrPromptCancellationTimedOut = errors.New("runtime session cancellation transport timed out")

var ErrPromptLeaseExpired = errors.New("prompt lease deadline must be in the future")

var ErrPromptActivationExpired = errors.New("prompt lease deadline expired during capability activation")

type RuntimeSessionConfig struct {
	ID                      string
	Generation              int64
	ProfileDigest           string
	Process                 ProcessConfig
	MCPServers              []MCPServer
	NewSessionMeta          Meta
	RequiredModeID          string
	ResumeSessionID         string
	AuthMethodID            string
	ClientInfo              Implementation
	ValidateAdapterRelaunch func() error

	InitializeTimeout     time.Duration
	PromptLease           time.Duration
	PermissionTimeout     time.Duration
	CancelGrace           time.Duration
	MaxBufferedEvents     int
	MaxBufferedEventBytes int
}

type StartupTimings struct {
	ProcessStart    time.Duration
	Initialize      time.Duration
	Authenticate    time.Duration
	ProviderSession time.Duration
}

type RuntimeSession struct {
	id                string
	generation        int64
	profileDigest     string
	providerSessionID string
	config            RuntimeSessionConfig
	startup           StartupTimings

	mu                  sync.Mutex
	process             *Process
	capabilities        AgentCapabilities
	resuming            *runtimeSessionRestart
	adapterRestarts     int
	frozen              bool
	active              *activePrompt
	pendingCancelWrites int
	cancelWriteTimedOut bool
	tombstones          map[string]PromptTombstone
	deleted             bool
	deletion            *runtimeSessionDeletion
}

type AdapterLostError struct {
	Attempted bool
	Cause     error
}

func (e *AdapterLostError) Error() string {
	if e.Attempted {
		return fmt.Sprintf("ACP adapter exited while idle and session/resume failed: %v", e.Cause)
	}
	return fmt.Sprintf("ACP adapter exited while idle and the agent does not support session/resume: %v", e.Cause)
}

func (e *AdapterLostError) Unwrap() error { return e.Cause }

const closeSessionGraceCap = 5 * time.Second

type runtimeSessionRestart struct {
	promptID        string
	requestDigest   string
	done            chan struct{}
	cancel          context.CancelFunc
	cancelRequested bool
}

type runtimeSessionDeletion struct {
	done   chan struct{}
	status CleanupStatus
	err    error
}

type InitializationError struct {
	Err     error
	Cleanup CleanupStatus
	session *RuntimeSession
}

func (e *InitializationError) Error() string                   { return e.Err.Error() }
func (e *InitializationError) Unwrap() error                   { return e.Err }
func (e *InitializationError) RuntimeSession() *RuntimeSession { return e.session }

type PromptEventType string

const (
	PromptEventAccepted            PromptEventType = "accepted"
	PromptEventUpdate              PromptEventType = "update"
	PromptEventPermissionRequested PromptEventType = "permission_requested"
)

type PromptEvent struct {
	Type       PromptEventType
	Sequence   int64
	Timestamp  time.Time
	ReceivedAt time.Time
	Update     *SessionNotification
	Permission *PermissionRequestEvent
	Size       int
}

type PermissionRequestEvent struct {
	RequestID string
	Request   RequestPermissionRequest
}

type PromptOutcome string

const (
	PromptOutcomeCompleted      PromptOutcome = "completed"
	PromptOutcomeCancelled      PromptOutcome = "cancelled"
	PromptOutcomeFailed         PromptOutcome = "failed"
	PromptOutcomeOutcomeUnknown PromptOutcome = "outcome_unknown"
)

type PromptResult struct {
	Outcome    PromptOutcome
	StopReason StopReason
	Err        error
	Accepted   bool
	SettledAt  time.Time
	Usage      *PromptUsage
}

type PromptRun struct {
	Events  <-chan PromptEvent
	Release func(PromptEvent)
	Result  <-chan PromptResult
}

type PromptTombstone struct {
	PromptID      string
	RequestDigest string
	Result        PromptResult
}

type DuplicatePromptError struct {
	PromptID string
	Active   bool
	Result   *PromptResult
}

func (e *DuplicatePromptError) Error() string {
	if e.Active {
		return fmt.Sprintf("ACP prompt %s was already accepted and remains active", e.PromptID)
	}
	return fmt.Sprintf("ACP prompt %s already settled", e.PromptID)
}

type DigestConflictError struct{ PromptID string }

func (e *DigestConflictError) Error() string {
	return fmt.Sprintf("ACP prompt %s identity was reused with a different request digest", e.PromptID)
}

type StalePromptError struct{ PromptID string }

func (e *StalePromptError) Error() string {
	return fmt.Sprintf("ACP prompt %s is no longer active", e.PromptID)
}

type activePrompt struct {
	id                    string
	requestDigest         string
	request               PromptRequest
	events                chan PromptEvent
	result                chan PromptResult
	done                  chan struct{}
	seq                   int64
	accepted              bool
	settled               bool
	overflowed            bool
	bufferedBytes         int
	cancelRequested       bool
	courtesyCancelStarted bool
	lease                 *time.Timer
	leaseDeadline         time.Time
	leaseVersion          uint64
	permissions           map[string]*pendingPermission
	preAccepted           []PromptEvent
}

type pendingPermission struct {
	options map[string]struct{}
	result  chan RequestPermissionOutcome
}

func NewRuntimeSession(ctx context.Context, cfg RuntimeSessionConfig) (*RuntimeSession, error) {
	cfg.ID = strings.TrimSpace(cfg.ID)
	cfg.ProfileDigest = strings.TrimSpace(cfg.ProfileDigest)
	if cfg.ID == "" || cfg.Generation <= 0 || cfg.ProfileDigest == "" {
		return nil, fmt.Errorf("runtime session ID, positive generation, and profile digest are required")
	}
	newSessionMeta, err := MergeNewSessionMeta(cfg.NewSessionMeta, Meta{
		sessionMetaRuntimeSessionID:     cfg.ID,
		sessionMetaGeneration:           cfg.Generation,
		sessionMetaRuntimeProfileDigest: cfg.ProfileDigest,
	})
	if err != nil {
		return nil, err
	}
	if cfg.InitializeTimeout <= 0 {
		cfg.InitializeTimeout = DefaultInitializeTimeout
	}
	if cfg.PromptLease <= 0 {
		cfg.PromptLease = DefaultPromptLease
	}
	if cfg.PermissionTimeout <= 0 {
		cfg.PermissionTimeout = DefaultPermissionTimeout
	}
	if cfg.CancelGrace <= 0 {
		cfg.CancelGrace = DefaultStopGrace
	}
	if cfg.MaxBufferedEvents <= 0 {
		cfg.MaxBufferedEvents = DefaultBufferedEvents
	}
	if cfg.MaxBufferedEventBytes <= 0 {
		cfg.MaxBufferedEventBytes = DefaultBufferedEventBytes
	}
	session := &RuntimeSession{
		id:            cfg.ID,
		generation:    cfg.Generation,
		profileDigest: cfg.ProfileDigest,
		config:        cfg,
		tombstones:    make(map[string]PromptTombstone),
	}
	session.config.Process.ClientOptions.RequestHandler = session.handleRequest
	session.config.Process.ClientOptions.NotificationHandler = session.handleNotification

	initCtx, cancel := context.WithTimeout(ctx, cfg.InitializeTimeout)
	defer cancel()
	process, capabilities, err := session.launchAdapter(initCtx, &session.startup)
	if process == nil {
		return nil, err
	}
	session.process = process
	if err != nil {
		return nil, initializationFailed(session, err)
	}
	if cfg.ResumeSessionID != "" {
		if !capabilities.SessionCapability(SessionCapabilityResume) {
			return nil, initializationFailed(session, fmt.Errorf("ACP adapter did not advertise session/resume support"))
		}
		resumeStarted := time.Now()
		resumed, err := process.Client().ResumeSession(initCtx, ResumeSessionRequest{
			SessionID: cfg.ResumeSessionID, CWD: cfg.Process.Paths.Workspace,
			MCPServers: append([]MCPServer{}, cfg.MCPServers...), Meta: newSessionMeta,
		})
		session.startup.ProviderSession = time.Since(resumeStarted)
		if err != nil {
			return nil, initializationFailed(session, fmt.Errorf("resume ACP provider session: %w", err))
		}
		session.capabilities = capabilities
		session.providerSessionID = cfg.ResumeSessionID
		if err := verifySessionMode(resumed.Modes, cfg.RequiredModeID); err != nil {
			return nil, initializationFailed(session, err)
		}
		return session, nil
	}
	newSessionStarted := time.Now()
	newSession, err := process.Client().NewSession(initCtx, NewSessionRequest{
		CWD:        cfg.Process.Paths.Workspace,
		MCPServers: append([]MCPServer{}, cfg.MCPServers...),
		Meta:       newSessionMeta,
	})
	session.startup.ProviderSession = time.Since(newSessionStarted)
	if err != nil {
		return nil, initializationFailed(session, fmt.Errorf("create ACP provider session: %w", err))
	}
	session.capabilities = capabilities
	session.providerSessionID = newSession.SessionID
	if err := verifySessionMode(newSession.Modes, cfg.RequiredModeID); err != nil {
		return nil, initializationFailed(session, err)
	}
	return session, nil
}

func verifySessionMode(modes json.RawMessage, required string) error {
	if required == "" {
		return nil
	}
	var state struct {
		CurrentModeID string `json:"currentModeId"`
	}
	if len(modes) == 0 || json.Unmarshal(modes, &state) != nil || state.CurrentModeID != required {
		return fmt.Errorf("ACP adapter did not start the provider session in the required %q mode", required)
	}
	return nil
}

func (s *RuntimeSession) launchAdapter(ctx context.Context, timings *StartupTimings) (*Process, AgentCapabilities, error) {
	cfg := s.config
	var discard StartupTimings
	if timings == nil {
		timings = &discard
	}
	stepStarted := time.Now()
	process, err := StartProcess(cfg.Process)
	timings.ProcessStart = time.Since(stepStarted)
	if err != nil {
		return nil, AgentCapabilities{}, err
	}
	clientInfo := cfg.ClientInfo
	if clientInfo.Name == "" {
		clientInfo = Implementation{Name: "orka-acp-runtime", Version: "development"}
	}
	stepStarted = time.Now()
	initialized, err := process.Client().Initialize(ctx, InitializeRequest{
		ProtocolVersion: ProtocolVersion,
		ClientInfo:      &clientInfo,
		ClientCapabilities: ClientCapabilities{
			FS:       FileSystemCapabilities{},
			Terminal: false,
			Session:  map[string]any{"notices": map[string]any{}},
		},
	})
	timings.Initialize = time.Since(stepStarted)
	if err != nil {
		return process, AgentCapabilities{}, fmt.Errorf("initialize ACP adapter: %w", err)
	}
	if len(cfg.MCPServers) > 0 && !acpMCPCapabilityEnabled(initialized.AgentCapabilities.MCPCapabilities, "http") {
		return process, AgentCapabilities{}, fmt.Errorf("ACP adapter did not advertise HTTP MCP server support")
	}
	if cfg.AuthMethodID != "" {
		if !containsAuthMethod(initialized.AuthMethods, cfg.AuthMethodID) {
			return process, AgentCapabilities{}, fmt.Errorf("ACP adapter did not advertise authentication method %q", cfg.AuthMethodID)
		}
		stepStarted = time.Now()
		err := process.Client().Authenticate(ctx, cfg.AuthMethodID)
		timings.Authenticate = time.Since(stepStarted)
		if err != nil {
			return process, AgentCapabilities{}, fmt.Errorf("authenticate ACP adapter: %w", err)
		}
	}
	return process, initialized.AgentCapabilities, nil
}

func (s *RuntimeSession) ID() string                { return s.id }
func (s *RuntimeSession) Generation() int64         { return s.generation }
func (s *RuntimeSession) ProviderSessionID() string { return s.providerSessionID }

func (s *RuntimeSession) StartupTimings() StartupTimings { return s.startup }

func (s *RuntimeSession) Process() *Process {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.process
}

func (s *RuntimeSession) AgentCapabilities() AgentCapabilities {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.capabilities
}

func (s *RuntimeSession) AdapterRestarts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.adapterRestarts
}

func processExited(process *Process) bool {
	if process == nil {
		return true
	}
	select {
	case <-process.Done():
		return true
	default:
		return false
	}
}

func (s *RuntimeSession) recoverExitedAdapterLocked(ctx context.Context, promptID, requestDigest string, leaseDeadline time.Time) error {
	if s.resuming != nil {
		return fmt.Errorf("runtime session adapter restart is in flight")
	}
	exited := s.process
	if !processExited(exited) {
		return nil
	}
	exitErr := exited.Client().Err()
	if exitErr == nil {
		exitErr = errors.New("adapter process exited")
	}
	if !s.capabilities.SessionCapability(SessionCapabilityResume) {
		return &AdapterLostError{Cause: exitErr}
	}
	resumeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	resuming := &runtimeSessionRestart{
		promptID: promptID, requestDigest: requestDigest,
		done: make(chan struct{}), cancel: cancel,
	}
	s.resuming = resuming
	defer func() {
		s.resuming = nil
		close(resuming.done)
	}()
	s.mu.Unlock()

	process, capabilities, err := s.resumeAdapter(resumeCtx, exited, leaseDeadline)

	s.mu.Lock()
	if resuming.cancelRequested {
		if process != nil {
			s.mu.Unlock()
			_ = stopProcessBestEffort(process, s.config.CancelGrace)
			s.mu.Lock()
		}
		s.tombstones[promptID] = PromptTombstone{
			PromptID: promptID, RequestDigest: requestDigest,
			Result: PromptResult{
				Outcome: PromptOutcomeCancelled, StopReason: StopReasonCancelled,
				SettledAt: time.Now().UTC(),
			},
		}
		if err == nil {
			err = context.Canceled
		}
	}
	if err != nil {
		return &AdapterLostError{Attempted: true, Cause: err}
	}
	if s.deleted {
		s.mu.Unlock()
		_ = stopProcessBestEffort(process, s.config.CancelGrace)
		s.mu.Lock()
		return fmt.Errorf("runtime session is deleted")
	}
	s.process = process
	s.capabilities = capabilities
	s.adapterRestarts++
	slog.Info(
		"ACP adapter resumed in place after idle exit",
		"runtimeSessionID", s.id,
		"generation", s.generation,
		"adapterRestarts", s.adapterRestarts,
	)
	return nil
}

func (s *RuntimeSession) resumeAdapter(ctx context.Context, exited *Process, leaseDeadline time.Time) (*Process, AgentCapabilities, error) {
	cfg := s.config
	deadline := leaseDeadline
	if initializeDeadline := time.Now().Add(cfg.InitializeTimeout); initializeDeadline.Before(deadline) {
		deadline = initializeDeadline
	}
	resumeCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	if exited.usesUIDProcessScope() {
		status, err := exited.Stop(resumeCtx, cfg.CancelGrace)
		if err != nil {
			return nil, AgentCapabilities{}, fmt.Errorf("prove exited adapter cleanup: %w", err)
		}
		if !status.Proven {
			return nil, AgentCapabilities{}, fmt.Errorf("exited adapter cleanup could not be proven; remaining pids %v", status.RemainingPIDs)
		}
	}
	select {
	case <-exited.Client().Done():
	case <-resumeCtx.Done():
		return nil, AgentCapabilities{}, fmt.Errorf("drain exited adapter output: %w", resumeCtx.Err())
	}
	if cfg.ValidateAdapterRelaunch != nil {
		if err := cfg.ValidateAdapterRelaunch(); err != nil {
			return nil, AgentCapabilities{}, fmt.Errorf("revalidate workspace before adapter relaunch: %w", err)
		}
	}
	process, capabilities, err := s.launchAdapter(resumeCtx, nil)
	if err != nil {
		if process != nil {
			_ = stopProcessBestEffort(process, cfg.CancelGrace)
		}
		return nil, AgentCapabilities{}, err
	}
	if !capabilities.SessionCapability(SessionCapabilityResume) {
		_ = stopProcessBestEffort(process, cfg.CancelGrace)
		return nil, AgentCapabilities{}, fmt.Errorf("replacement adapter no longer advertises session/resume")
	}
	meta, err := MergeNewSessionMeta(cfg.NewSessionMeta, Meta{
		sessionMetaRuntimeSessionID:     s.id,
		sessionMetaGeneration:           s.generation,
		sessionMetaRuntimeProfileDigest: s.profileDigest,
	})
	if err != nil {
		_ = stopProcessBestEffort(process, cfg.CancelGrace)
		return nil, AgentCapabilities{}, err
	}
	resumed, err := process.Client().ResumeSession(resumeCtx, ResumeSessionRequest{
		SessionID:  s.providerSessionID,
		CWD:        cfg.Process.Paths.Workspace,
		MCPServers: append([]MCPServer{}, cfg.MCPServers...),
		Meta:       meta,
	})
	if err != nil {
		_ = stopProcessBestEffort(process, cfg.CancelGrace)
		return nil, AgentCapabilities{}, fmt.Errorf("resume ACP provider session: %w", err)
	}
	if resumed.SessionID != "" && resumed.SessionID != s.providerSessionID {
		_ = stopProcessBestEffort(process, cfg.CancelGrace)
		return nil, AgentCapabilities{}, fmt.Errorf("ACP session/resume bound a different provider session")
	}
	if err := verifySessionMode(resumed.Modes, cfg.RequiredModeID); err != nil {
		_ = stopProcessBestEffort(process, cfg.CancelGrace)
		return nil, AgentCapabilities{}, err
	}
	return process, capabilities, nil
}

func (s *RuntimeSession) closeProviderSession(ctx context.Context, process *Process) {
	grace := s.config.CancelGrace
	if grace <= 0 {
		grace = DefaultStopGrace
	}
	grace = min(grace, closeSessionGraceCap)
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), grace)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- process.Client().CloseSession(closeCtx, s.providerSessionID)
	}()
	select {
	case err := <-done:
		if err != nil {
			code := 0
			if rpcErr, ok := errors.AsType[*RPCError](err); ok {
				code = rpcErr.Code
			}
			slog.Debug("ACP session/close before deletion failed", "runtimeSessionID", s.id, "rpcErrorCode", code)
		}
	case <-closeCtx.Done():
		slog.Debug("ACP session/close before deletion timed out", "runtimeSessionID", s.id)
	case <-ctx.Done():
	}
}

func (s *RuntimeSession) StartPrompt(ctx context.Context, promptID, requestDigest string, prompt []ContentBlock) (PromptRun, error) {
	if s.config.PromptLease <= 0 {
		return PromptRun{}, fmt.Errorf("prompt lease duration must be positive")
	}
	return s.StartPromptWithLeaseDeadline(ctx, promptID, requestDigest, prompt, time.Now().Add(s.config.PromptLease))
}

func (s *RuntimeSession) RecoverExitedAdapter(ctx context.Context, promptID, requestDigest string, leaseDeadline time.Time) error {
	promptID = strings.TrimSpace(promptID)
	requestDigest = strings.TrimSpace(requestDigest)
	s.mu.Lock()
	if s.deleted || s.active != nil || s.resuming != nil || s.pendingCancelWrites != 0 || s.cancelWriteTimedOut || !leaseDeadline.After(time.Now()) {
		s.mu.Unlock()
		return nil
	}
	if _, settled := s.tombstones[promptID]; settled {
		s.mu.Unlock()
		return nil
	}
	restartsBefore := s.adapterRestarts
	if err := s.recoverExitedAdapterLocked(ctx, promptID, requestDigest, leaseDeadline); err != nil {
		s.mu.Unlock()
		return err
	}
	if s.adapterRestarts == restartsBefore || s.deleted || leaseDeadline.After(time.Now()) {
		s.mu.Unlock()
		return nil
	}
	s.deleted = true
	process := s.process
	s.mu.Unlock()
	_ = stopProcessBestEffort(process, s.config.CancelGrace)
	return &AdapterLostError{Attempted: true, Cause: fmt.Errorf("prompt lease expired before admission: %w", context.DeadlineExceeded)}
}

func (s *RuntimeSession) StartPromptWithLeaseDeadline(ctx context.Context, promptID, requestDigest string, prompt []ContentBlock, leaseDeadline time.Time) (PromptRun, error) {
	return s.StartPromptWithLeaseDeadlineAndActivation(ctx, promptID, requestDigest, prompt, leaseDeadline, nil)
}

func (s *RuntimeSession) StartPromptWithLeaseDeadlineAndActivation(ctx context.Context, promptID, requestDigest string, prompt []ContentBlock, leaseDeadline time.Time, activate func() error) (PromptRun, error) {
	promptID = strings.TrimSpace(promptID)
	requestDigest = strings.TrimSpace(requestDigest)
	if promptID == "" || requestDigest == "" || len(prompt) == 0 {
		return PromptRun{}, fmt.Errorf("prompt ID, request digest, and content are required")
	}
	s.mu.Lock()
	if !leaseDeadline.After(time.Now()) {
		s.mu.Unlock()
		return PromptRun{}, ErrPromptLeaseExpired
	}
	if s.deleted {
		s.mu.Unlock()
		return PromptRun{}, fmt.Errorf("runtime session is deleted")
	}
	if s.cancelWriteTimedOut {
		s.mu.Unlock()
		return PromptRun{}, ErrPromptCancellationTimedOut
	}
	if active := s.active; active != nil {
		if active.id != promptID {
			s.mu.Unlock()
			return PromptRun{}, fmt.Errorf("runtime session already has active prompt %s", active.id)
		}
		if active.requestDigest != requestDigest {
			s.mu.Unlock()
			return PromptRun{}, &DigestConflictError{PromptID: promptID}
		}
		s.mu.Unlock()
		return PromptRun{}, &DuplicatePromptError{PromptID: promptID, Active: true}
	}
	if tombstone, ok := s.tombstones[promptID]; ok {
		if tombstone.RequestDigest != requestDigest {
			s.mu.Unlock()
			return PromptRun{}, &DigestConflictError{PromptID: promptID}
		}
		result := tombstone.Result
		s.mu.Unlock()
		return PromptRun{}, &DuplicatePromptError{PromptID: promptID, Result: &result}
	}
	if resuming := s.resuming; resuming != nil {
		if resuming.promptID != promptID {
			s.mu.Unlock()
			return PromptRun{}, fmt.Errorf("runtime session already has active prompt %s", resuming.promptID)
		}
		if resuming.requestDigest != requestDigest {
			s.mu.Unlock()
			return PromptRun{}, &DigestConflictError{PromptID: promptID}
		}
		s.mu.Unlock()
		return PromptRun{}, &DuplicatePromptError{PromptID: promptID, Active: true}
	}
	if s.pendingCancelWrites != 0 {
		s.mu.Unlock()
		return PromptRun{}, ErrPromptCancellationPending
	}
	if exited := s.process; processExited(exited) {
		exitErr := errors.New("adapter process exited")
		if exited != nil {
			if err := exited.Client().Err(); err != nil {
				exitErr = err
			}
		}
		s.mu.Unlock()
		return PromptRun{}, &AdapterLostError{Cause: exitErr}
	}
	process := s.process
	if activate != nil {
		if err := activate(); err != nil {
			s.mu.Unlock()
			return PromptRun{}, err
		}
	}
	if !leaseDeadline.After(time.Now()) {
		s.mu.Unlock()
		if activate != nil {
			return PromptRun{}, ErrPromptActivationExpired
		}
		return PromptRun{}, ErrPromptLeaseExpired
	}
	active := &activePrompt{
		id:            promptID,
		requestDigest: requestDigest,
		request:       PromptRequest{SessionID: s.providerSessionID, Prompt: append([]ContentBlock(nil), prompt...)},
		events:        make(chan PromptEvent, s.config.MaxBufferedEvents),
		result:        make(chan PromptResult, 1),
		done:          make(chan struct{}),
		leaseDeadline: leaseDeadline,
		permissions:   make(map[string]*pendingPermission),
	}
	s.resetPromptLeaseLocked(active)
	s.active = active
	s.mu.Unlock()

	go s.runPrompt(active, process)
	go func() {
		select {
		case <-ctx.Done():
			cancelCtx, cancel := context.WithTimeout(context.Background(), s.config.CancelGrace*2)
			defer cancel()
			_, _ = s.CancelPrompt(cancelCtx, promptID)
		case <-active.done:
		}
	}()
	return PromptRun{Events: active.events, Result: active.result, Release: func(event PromptEvent) { s.releaseBufferedEvent(active, event) }}, nil
}

func (s *RuntimeSession) RenewPromptLease(promptID string) error {
	if s.config.PromptLease <= 0 {
		return fmt.Errorf("prompt lease duration must be positive")
	}
	return s.RenewPromptLeaseUntil(promptID, time.Now().Add(s.config.PromptLease))
}

func (s *RuntimeSession) RenewPromptLeaseUntil(promptID string, leaseDeadline time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if !leaseDeadline.After(now) {
		return fmt.Errorf("prompt lease deadline must be in the future")
	}
	if s.active == nil || s.active.id != promptID || s.active.settled || s.active.cancelRequested {
		return &StalePromptError{PromptID: promptID}
	}
	if !s.active.leaseDeadline.After(now) {
		return &StalePromptError{PromptID: promptID}
	}
	s.active.leaseDeadline = leaseDeadline
	s.resetPromptLeaseLocked(s.active)
	return nil
}

func (s *RuntimeSession) ResolvePermission(promptID, requestID string, outcome RequestPermissionOutcome) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active == nil || s.active.id != promptID || s.active.settled {
		return &StalePromptError{PromptID: promptID}
	}
	pending, ok := s.active.permissions[requestID]
	if !ok {
		return fmt.Errorf("permission request %s is not pending", requestID)
	}
	if outcome.Outcome == permissionOutcomeSelected {
		if _, ok := pending.options[outcome.OptionID]; !ok {
			return fmt.Errorf("permission option %q was not offered", outcome.OptionID)
		}
	} else if outcome.Outcome != permissionOutcomeCancelled {
		return fmt.Errorf("unsupported permission outcome %q", outcome.Outcome)
	}
	delete(s.active.permissions, requestID)
	pending.result <- outcome
	return nil
}

func (s *RuntimeSession) CancelPrompt(ctx context.Context, promptID string) (PromptResult, error) {
	s.mu.Lock()
	if resuming := s.resuming; resuming != nil && resuming.promptID == promptID {
		resuming.cancelRequested = true
		resuming.cancel()
		s.mu.Unlock()
		select {
		case <-resuming.done:
			return s.tombstoneResult(promptID)
		case <-ctx.Done():
			return PromptResult{}, ctx.Err()
		}
	}
	active := s.active
	if active == nil || active.id != promptID || active.settled {
		tombstone, settled := s.tombstones[promptID]
		s.mu.Unlock()
		if settled {
			return tombstone.Result, nil
		}
		return PromptResult{}, &StalePromptError{PromptID: promptID}
	}
	s.startPromptCancellationLocked(active)
	process := s.process
	s.mu.Unlock()
	return s.cancelPrompt(ctx, active, process)
}

func (s *RuntimeSession) startPromptCancellationLocked(active *activePrompt) {
	active.cancelRequested = true
	cancelPendingPermissions(active)
	if active.courtesyCancelStarted {
		return
	}
	active.courtesyCancelStarted = true
	s.pendingCancelWrites++
	process := s.process
	go func() {
		writeCtx, cancelWrite := context.WithTimeout(context.Background(), 2*s.config.CancelGrace)
		err := process.Client().Cancel(writeCtx, s.providerSessionID)
		cancelWrite()
		if errors.Is(err, context.DeadlineExceeded) {
			s.mu.Lock()
			s.cancelWriteTimedOut = true
			s.mu.Unlock()
			stopCtx, cancelStop := context.WithTimeout(context.Background(), 2*s.config.CancelGrace)
			_, _ = process.Stop(stopCtx, s.config.CancelGrace)
			cancelStop()
		}
		s.mu.Lock()
		s.pendingCancelWrites--
		s.mu.Unlock()
	}()
}

func (s *RuntimeSession) cancelPrompt(ctx context.Context, active *activePrompt, process *Process) (PromptResult, error) {
	promptID := active.id
	done := active.done
	timer := time.NewTimer(s.config.CancelGrace)
	defer timer.Stop()
	select {
	case <-done:
		return s.tombstoneResult(promptID)
	case <-timer.C:
		_, _ = process.Stop(ctx, s.config.CancelGrace)
		select {
		case <-done:
			return s.tombstoneResult(promptID)
		case <-ctx.Done():
			return PromptResult{}, ctx.Err()
		}
	case <-ctx.Done():
		return PromptResult{}, ctx.Err()
	}
}

func (s *RuntimeSession) WaitPromptSettlement(ctx context.Context, promptID string) error {
	s.mu.Lock()
	active := s.active
	if active == nil || active.id != promptID || active.settled {
		_, settled := s.tombstones[promptID]
		s.mu.Unlock()
		if settled {
			return nil
		}
		return &StalePromptError{PromptID: promptID}
	}
	done := active.done
	grace := s.config.CancelGrace
	s.mu.Unlock()
	if grace <= 0 {
		grace = DefaultStopGrace
	}
	waitCtx, cancel := context.WithTimeout(ctx, 2*grace)
	defer cancel()
	select {
	case <-done:
		return nil
	case <-waitCtx.Done():
		return waitCtx.Err()
	}
}

func (s *RuntimeSession) Delete(ctx context.Context) (CleanupStatus, error) {
	s.mu.Lock()
	firstDeletion := !s.deleted
	s.deleted = true
	for s.resuming != nil {
		s.resuming.cancelRequested = true
		s.resuming.cancel()
		resuming := s.resuming.done
		s.mu.Unlock()
		select {
		case <-resuming:
		case <-ctx.Done():
			return CleanupStatus{}, ctx.Err()
		}
		s.mu.Lock()
	}
	deletion := s.deletion
	if deletion != nil {
		select {
		case <-deletion.done:
			if deletion.err == nil && deletion.status.Proven {
				s.mu.Unlock()
				return deletion.status, nil
			}
		default:
			s.mu.Unlock()
			select {
			case <-deletion.done:
				return deletion.status, deletion.err
			case <-ctx.Done():
				return CleanupStatus{}, ctx.Err()
			}
		}
	}
	deletion = &runtimeSessionDeletion{done: make(chan struct{})}
	s.deletion = deletion
	active := s.active
	if active != nil && !active.settled {
		active.cancelRequested = true
		cancelPendingPermissions(active)
	}
	process := s.process
	closeSupported := s.capabilities.SessionCapability(SessionCapabilityClose) && !s.frozen
	s.mu.Unlock()
	if firstDeletion && active != nil {
		go func() {
			_ = process.Client().Cancel(context.Background(), s.providerSessionID)
		}()
	}
	if firstDeletion && closeSupported && !processExited(process) {
		s.closeProviderSession(ctx, process)
	}
	status, err := process.Stop(ctx, s.config.CancelGrace)
	s.mu.Lock()
	deletion.status = status
	deletion.err = err
	close(deletion.done)
	s.mu.Unlock()
	return status, err
}

func (s *RuntimeSession) Tombstone(promptID string) (PromptTombstone, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.tombstones[promptID]
	return value, ok
}

func (s *RuntimeSession) runPrompt(active *activePrompt, process *Process) {
	var written bool
	response, err := process.Client().PromptWithWritten(context.Background(), active.request, func() {
		s.mu.Lock()
		written = true
		active.accepted = true
		s.emitLocked(active, PromptEvent{Type: PromptEventAccepted})
		queued := append([]PromptEvent(nil), active.preAccepted...)
		active.preAccepted = nil
		for _, event := range queued {
			active.bufferedBytes -= event.Size
			s.emitLocked(active, event)
		}
		s.mu.Unlock()
	})
	result := PromptResult{Accepted: written, SettledAt: time.Now().UTC()}
	if err == nil {
		result.Usage = ParsePromptUsage(response.Usage)
	}
	switch {
	case err != nil:
		result.Outcome = classifyPromptErrorOutcome(err, written)
		result.Err = err
	case response.StopReason == StopReasonEndTurn:
		result.Outcome = PromptOutcomeCompleted
		result.StopReason = response.StopReason
	case response.StopReason == StopReasonCancelled:
		result.Outcome = PromptOutcomeCancelled
		result.StopReason = response.StopReason
	default:
		result.Outcome = PromptOutcomeFailed
		result.StopReason = response.StopReason
	}
	s.finishPrompt(active, result)
}

func classifyPromptErrorOutcome(err error, written bool) PromptOutcome {
	if !written {
		return PromptOutcomeFailed
	}
	if _, ok := errors.AsType[*RPCError](err); ok {
		return PromptOutcomeFailed
	}
	return PromptOutcomeOutcomeUnknown
}

func (s *RuntimeSession) finishPrompt(active *activePrompt, result PromptResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if active.settled {
		return
	}
	if result.Accepted && !active.leaseDeadline.IsZero() && !result.SettledAt.IsZero() &&
		!result.SettledAt.Before(active.leaseDeadline) {
		switch result.Outcome {
		case PromptOutcomeCompleted, PromptOutcomeCancelled, PromptOutcomeFailed:
			result.Outcome = PromptOutcomeCancelled
			result.StopReason = StopReasonCancelled
			result.Err = nil
		}
	}
	active.settled = true
	if active.lease != nil {
		active.lease.Stop()
	}
	cancelPendingPermissions(active)
	if active.overflowed && result.Outcome != PromptOutcomeOutcomeUnknown {
		result.Outcome = PromptOutcomeFailed
		result.Err = ErrPromptEventBufferOverflow
	}
	s.tombstones[active.id] = PromptTombstone{PromptID: active.id, RequestDigest: active.requestDigest, Result: result}
	if s.active == active {
		s.active = nil
	}
	active.result <- result
	close(active.result)
	close(active.events)
	close(active.done)
}

func (s *RuntimeSession) handleNotification(_ context.Context, notification IncomingNotification) {
	if notification.Method != MethodSessionUpdate {
		return
	}
	var update SessionNotification
	if err := json.Unmarshal(notification.Params, &update); err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	active := s.active
	if active == nil || active.settled || update.SessionID != s.providerSessionID {
		return
	}
	s.emitLocked(active, PromptEvent{Type: PromptEventUpdate, Update: &update, Size: len(notification.Params)})
}

func (s *RuntimeSession) handleRequest(ctx context.Context, request IncomingRequest) (any, *RPCError) {
	if request.Method != MethodRequestPermission {
		return nil, &RPCError{Code: -32601, Message: "client method is not supported"}
	}
	var permission RequestPermissionRequest
	if err := json.Unmarshal(request.Params, &permission); err != nil {
		return nil, &RPCError{Code: -32602, Message: "invalid permission request"}
	}
	requestID := canonicalID(request.ID)
	s.mu.Lock()
	active := s.active
	if active == nil || active.settled || permission.SessionID != s.providerSessionID {
		s.mu.Unlock()
		return RequestPermissionResponse{Outcome: CancelledPermissionOutcome()}, nil
	}
	if _, exists := active.permissions[requestID]; exists {
		s.mu.Unlock()
		return nil, &RPCError{Code: -32600, Message: "duplicate permission request"}
	}
	pending := &pendingPermission{options: make(map[string]struct{}), result: make(chan RequestPermissionOutcome, 1)}
	for _, option := range permission.Options {
		pending.options[option.OptionID] = struct{}{}
	}
	active.permissions[requestID] = pending
	s.emitLocked(active, PromptEvent{Type: PromptEventPermissionRequested, Permission: &PermissionRequestEvent{RequestID: requestID, Request: permission}, Size: len(request.Params)})
	done := active.done
	s.mu.Unlock()

	timer := time.NewTimer(s.config.PermissionTimeout)
	defer timer.Stop()
	select {
	case outcome := <-pending.result:
		return RequestPermissionResponse{Outcome: outcome}, nil
	case <-done:
		return RequestPermissionResponse{Outcome: CancelledPermissionOutcome()}, nil
	case <-timer.C:
		s.removePermission(requestID, pending)
		return RequestPermissionResponse{Outcome: CancelledPermissionOutcome()}, nil
	case <-ctx.Done():
		s.removePermission(requestID, pending)
		return RequestPermissionResponse{Outcome: CancelledPermissionOutcome()}, nil
	}
}

func (s *RuntimeSession) emitLocked(active *activePrompt, event PromptEvent) {
	if event.ReceivedAt.IsZero() {
		event.ReceivedAt = time.Now().UTC()
	}
	if !active.accepted && event.Type != PromptEventAccepted {
		if len(active.preAccepted) >= s.config.MaxBufferedEvents || active.bufferedBytes+event.Size > s.config.MaxBufferedEventBytes {
			s.markOverflowedLocked(active)
			return
		}
		active.bufferedBytes += event.Size
		active.preAccepted = append(active.preAccepted, event)
		return
	}
	if event.Size > 0 && active.bufferedBytes+event.Size > s.config.MaxBufferedEventBytes {
		s.markOverflowedLocked(active)
		return
	}
	active.seq++
	event.Sequence = active.seq
	event.Timestamp = time.Now().UTC()
	select {
	case active.events <- event:
		active.bufferedBytes += event.Size
	default:
		s.markOverflowedLocked(active)
	}
}

func (s *RuntimeSession) releaseBufferedEvent(active *activePrompt, event PromptEvent) {
	if active == nil || event.Size <= 0 {
		return
	}
	s.mu.Lock()
	active.bufferedBytes -= event.Size
	if active.bufferedBytes < 0 {
		active.bufferedBytes = 0
	}
	s.mu.Unlock()
}

func (s *RuntimeSession) markOverflowedLocked(active *activePrompt) {
	if active.overflowed {
		return
	}
	active.overflowed = true
	slog.Warn("ACP prompt event buffer overflowed; cancelling the prompt",
		"promptID", active.id, "bufferedEvents", s.config.MaxBufferedEvents, "bufferedBytes", active.bufferedBytes,
		"maxBufferedEventBytes", s.config.MaxBufferedEventBytes, "lastSequence", active.seq, "accepted", active.accepted)
	go func(promptID string) {
		ctx, cancel := context.WithTimeout(context.Background(), s.config.CancelGrace*2)
		defer cancel()
		_, _ = s.CancelPrompt(ctx, promptID)
	}(active.id)
}

func (s *RuntimeSession) resetPromptLeaseLocked(active *activePrompt) {
	if active.lease != nil {
		active.lease.Stop()
	}
	active.leaseVersion++
	version := active.leaseVersion
	active.lease = time.AfterFunc(time.Until(active.leaseDeadline), func() { s.expirePrompt(active, version) })
}

func (s *RuntimeSession) expirePrompt(active *activePrompt, version uint64) {
	s.mu.Lock()
	if s.active != active || active.settled || active.leaseVersion != version {
		s.mu.Unlock()
		return
	}
	if remaining := time.Until(active.leaseDeadline); remaining > 0 {
		active.lease.Reset(remaining)
		s.mu.Unlock()
		return
	}
	s.startPromptCancellationLocked(active)
	process := s.process
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), s.config.CancelGrace*2)
	defer cancel()
	_, _ = s.cancelPrompt(ctx, active, process)
}

func (s *RuntimeSession) removePermission(requestID string, pending *pendingPermission) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != nil && s.active.permissions[requestID] == pending {
		delete(s.active.permissions, requestID)
	}
}

func (s *RuntimeSession) tombstoneResult(promptID string) (PromptResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tombstone, ok := s.tombstones[promptID]
	if !ok {
		return PromptResult{}, fmt.Errorf("prompt %s settled without tombstone", promptID)
	}
	return tombstone.Result, nil
}

func cancelPendingPermissions(active *activePrompt) {
	for id, pending := range active.permissions {
		delete(active.permissions, id)
		select {
		case pending.result <- CancelledPermissionOutcome():
		default:
		}
	}
}

func acpMCPCapabilityEnabled(capabilities map[string]any, name string) bool {
	value, ok := capabilities[name]
	if !ok {
		return false
	}
	enabled, ok := value.(bool)
	return ok && enabled
}

func containsAuthMethod(methods []AuthMethod, id string) bool {
	for _, method := range methods {
		if method.ID == id {
			return true
		}
	}
	return false
}

func stopProcessBestEffort(process *Process, grace time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), grace*2)
	defer cancel()
	_, err := process.Stop(ctx, grace)
	return err
}

func initializationFailed(session *RuntimeSession, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), session.config.CancelGrace*2)
	defer cancel()
	cleanup, err := session.Delete(ctx)
	return &InitializationError{Err: errors.Join(cause, err), Cleanup: cleanup, session: session}
}

func (s *RuntimeSession) Freeze(ctx context.Context) error {
	s.mu.Lock()
	if s.deleted || s.active != nil || s.resuming != nil {
		s.mu.Unlock()
		return fmt.Errorf("runtime session must be idle before workspace freeze")
	}
	process := s.process
	s.frozen = true
	s.mu.Unlock()
	return process.Freeze(ctx)
}

func (s *RuntimeSession) ChildIdentity() (int, int) {
	if s == nil {
		return 0, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.config.Process.UID, s.config.Process.GID
}

func (s *RuntimeSession) Thaw() error {
	s.mu.Lock()
	if s.deleted || s.active != nil || s.resuming != nil {
		s.mu.Unlock()
		return fmt.Errorf("runtime session cannot thaw while deleted or prompt-active")
	}
	process := s.process
	s.mu.Unlock()
	if err := process.Thaw(); err != nil {
		return err
	}
	s.mu.Lock()
	s.frozen = false
	s.mu.Unlock()
	return nil
}
