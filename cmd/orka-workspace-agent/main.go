/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/orka-agents/orka/internal/workspace/daemonprotocol"
	workspaceagent "github.com/orka-agents/orka/pkg/workspaceagent"
)

const (
	defaultListenAddr              = ":8080"
	defaultCommandTimeout          = 30 * time.Minute
	attachmentRevocationTimeout    = 30 * time.Second
	processGroupDrainTimeout       = 2 * time.Second
	completedExecutionRetention    = 15 * time.Minute
	operationTombstoneRetention    = 15 * time.Minute
	resetOperationRetention        = 24 * time.Hour
	defaultMaxRetainedOperations   = 256
	defaultMaxOperationTombstones  = 4096
	defaultMaxOperationIDsPerEpoch = 4096
	defaultMaxResetOperations      = 4096
	defaultMaxOutputBytes          = 1 << 20
	defaultMaxRequestBytes         = 256 << 20
	defaultMaxDownloadBytes        = 64 << 20
	maxWorkspaceTreeEntries        = 4096
	maxWorkspaceTreeDepth          = 64
	workspaceDirectoryBatchSize    = 128
	defaultHandoffFile             = "/app/orka-workspace-handoff-token"
	defaultWorkspaceRoot           = "/workspace"
	defaultHandoffUploadAlias      = "orka-workspace-handoff-token"
	commandConfinementWrapperArg   = "__orka_workspace_agent_confined_exec"
	envListenAddr                  = "ORKA_WORKSPACE_AGENT_LISTEN_ADDR"
	envHandoffAuth                 = "ORKA_WORKSPACE_HANDOFF_TOKEN"
	envHandoffAuthFile             = "ORKA_WORKSPACE_HANDOFF_TOKEN_FILE"
	envBootstrapAuth               = "ORKA_WORKSPACE_BOOTSTRAP_TOKEN"
	envControlAuthFile             = "ORKA_WORKSPACE_AGENT_CONTROL_AUTH_FILE"
	envTLSCertFile                 = "ORKA_WORKSPACE_AGENT_TLS_CERT_FILE"
	envTLSKeyFile                  = "ORKA_WORKSPACE_AGENT_TLS_KEY_FILE"
	envCommandUID                  = "ORKA_WORKSPACE_AGENT_COMMAND_UID"
	envCommandGID                  = "ORKA_WORKSPACE_AGENT_COMMAND_GID"
	envDefaultCommandTimeoutSecs   = "ORKA_WORKSPACE_AGENT_DEFAULT_COMMAND_TIMEOUT_SECONDS"
	envDefaultMaxOutputBytes       = "ORKA_WORKSPACE_AGENT_MAX_OUTPUT_BYTES"
	envMaxRequestBytes             = "ORKA_WORKSPACE_AGENT_MAX_REQUEST_BYTES"
	envMaxDownloadBytes            = "ORKA_WORKSPACE_AGENT_MAX_DOWNLOAD_BYTES"
)

var allowedRoots = []string{"/app", defaultWorkspaceRoot, "/home/worker", "/tmp", "/dev/shm"}

// commandWritableRoots is the complete write allowlist applied to secured v1
// command processes. Every root is also cleared by reset before rebinding.
var commandWritableRoots = []string{defaultWorkspaceRoot, "/home/worker", "/tmp", "/dev/shm"}

var (
	errHandoffAuthMissing    = errors.New("handoff token file is missing")
	errHandoffAuthEmpty      = errors.New("handoff token file is empty")
	errWorkspaceFileTooLarge = errors.New("workspace file exceeds download limit")
	errWorkspaceTreeTooLarge = errors.New("workspace tree exceeds entry limit")
	errWorkspaceTreeTooDeep  = errors.New("workspace tree exceeds depth limit")
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == commandConfinementWrapperArg {
		if err := runWriteConfinedCommand(os.Args[2:]); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "workspace command confinement failed:", err)
			os.Exit(126)
		}
		return
	}
	if err := run(); err != nil {
		slog.Error("workspace agent failed", "err", err)
		os.Exit(1)
	}
}

func run() error {
	addr := strings.TrimSpace(os.Getenv(envListenAddr))
	if addr == "" {
		addr = defaultListenAddr
	}
	server := newWorkspaceAgentServer()
	if server.startupErr != nil {
		return server.startupErr
	}
	certFile := strings.TrimSpace(os.Getenv(envTLSCertFile))
	keyFile := strings.TrimSpace(os.Getenv(envTLSKeyFile))
	if (certFile == "") != (keyFile == "") {
		return fmt.Errorf("workspace-agent TLS certificate and key must be configured together")
	}
	if keyFile != "" {
		if !filepath.IsAbs(certFile) || !filepath.IsAbs(keyFile) {
			return fmt.Errorf("workspace-agent TLS certificate and key paths must be absolute")
		}
		if err := validatePrivateKeyFile(keyFile); err != nil {
			return fmt.Errorf("validate workspace-agent TLS key: %w", err)
		}
	}
	if certFile != "" {
		slog.Info("starting workspace agent with TLS", "addr", addr)
		return http.ListenAndServeTLS(addr, certFile, keyFile, server.routes())
	}
	slog.Info("starting workspace agent", "addr", addr)
	return http.ListenAndServe(addr, server.routes())
}

type workspaceAgentServer struct {
	defaultCommandTimeout time.Duration
	defaultMaxOutputBytes int64
	maxRequestBytes       int64
	maxDownloadBytes      int64
	bootstrapAuth         string
	controlAuth           string
	controlAuthPath       string
	controlAuthConfigured bool
	writeConfinement      bool
	processTerminator     func(context.Context) error
	startupErr            error
	legacyAuthEnabled     bool
	commandUID            uint32
	commandGID            uint32

	mu                     sync.Mutex
	executions             map[string]execResponse
	executionCancels       map[string]context.CancelFunc
	executionEpochs        map[string]int64
	executionFingerprints  map[string]string
	executionTombstones    map[string]time.Time
	operationRunning       map[string]bool
	inflightByEpoch        map[int64]int
	runningByEpoch         map[int64]int
	activeAttachment       *attachmentState
	attachmentExpiryCancel context.CancelFunc
	expiringEpoch          int64
	lastExpiredEpoch       int64
	revokingEpoch          int64
	revocationDone         chan struct{}
	lastRevokedEpoch       int64
	lastRevokedWorkspace   string
	lastRevokedBinding     string
	cleanupInProgress      bool
	boundWorkspaceUID      string
	bindingGeneration      string
	resetRequired          bool
	resetOperations        map[string]resetOperationRecord
	lastEpoch              int64
}

func newWorkspaceAgentServer() *workspaceAgentServer {
	bootstrapAuth := strings.TrimSpace(os.Getenv(envBootstrapAuth))
	controlAuth, controlPath, controlConfigured, controlErr := readControlAuthFile()
	if bootstrapAuth != "" {
		_ = os.Unsetenv(envBootstrapAuth)
	}
	bindingGeneration, generationErr := newBindingGeneration()
	startupErr := controlErr
	if startupErr == nil && generationErr != nil {
		startupErr = fmt.Errorf("generate workspace binding generation: %w", generationErr)
	}
	commandUID, uidErr := uint32Env(envCommandUID, 1000)
	if startupErr == nil && uidErr != nil {
		startupErr = uidErr
	}
	commandGID, gidErr := uint32Env(envCommandGID, 1000)
	if startupErr == nil && gidErr != nil {
		startupErr = gidErr
	}
	writeConfinement := controlConfigured && commandWriteConfinementSupported()
	if startupErr == nil {
		startupErr = validateCommandWriteConfinement(controlConfigured, writeConfinement)
	}
	var processTerminator func(context.Context) error
	if controlConfigured {
		processTerminator = terminateAttachmentProcesses
	}
	return &workspaceAgentServer{
		defaultCommandTimeout: durationEnvSeconds(envDefaultCommandTimeoutSecs, defaultCommandTimeout),
		defaultMaxOutputBytes: int64Env(envDefaultMaxOutputBytes, defaultMaxOutputBytes),
		maxRequestBytes:       int64Env(envMaxRequestBytes, defaultMaxRequestBytes),
		maxDownloadBytes:      int64Env(envMaxDownloadBytes, defaultMaxDownloadBytes),
		bootstrapAuth:         bootstrapAuth,
		controlAuth:           controlAuth,
		controlAuthPath:       controlPath,
		controlAuthConfigured: controlConfigured,
		writeConfinement:      writeConfinement,
		processTerminator:     processTerminator,
		startupErr:            startupErr,
		legacyAuthEnabled:     !controlConfigured,
		commandUID:            commandUID,
		commandGID:            commandGID,
		executions:            make(map[string]execResponse),
		executionCancels:      make(map[string]context.CancelFunc),
		executionEpochs:       make(map[string]int64),
		executionFingerprints: make(map[string]string),
		executionTombstones:   make(map[string]time.Time),
		operationRunning:      make(map[string]bool),
		inflightByEpoch:       make(map[int64]int),
		runningByEpoch:        make(map[int64]int),
		bindingGeneration:     bindingGeneration,
		resetRequired:         controlConfigured,
		resetOperations:       make(map[string]resetOperationRecord),
	}
}

func validateCommandWriteConfinement(controlConfigured, writeConfinement bool) error {
	if controlConfigured && !writeConfinement {
		return errCommandWriteConfinementUnavailable
	}
	return nil
}

func readControlAuthFile() (string, string, bool, error) {
	path := strings.TrimSpace(os.Getenv(envControlAuthFile))
	if path == "" {
		return "", "", false, nil
	}
	if !filepath.IsAbs(path) {
		return "", "", true, fmt.Errorf("workspace-agent control auth file path must be absolute")
	}
	if err := validateControlAuthFile(path); err != nil {
		return "", "", true, fmt.Errorf("validate workspace-agent control auth file: %w", err)
	}
	canonicalPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", "", true, fmt.Errorf("resolve workspace-agent control auth file: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", true, fmt.Errorf("read workspace-agent control auth file: %w", err)
	}
	auth := strings.TrimSpace(string(data))
	if auth == "" {
		return "", "", true, fmt.Errorf("workspace-agent control auth file is empty")
	}
	return auth, filepath.Clean(canonicalPath), true, nil
}

type attachmentState struct {
	workspaceUID string
	taskUID      string
	epoch        int64
	authDigest   [sha256.Size]byte
	expiresAt    time.Time
}

var (
	errOperationCapacity                  = errors.New("workspace-agent operation retention capacity is full")
	errOperationBusy                      = errors.New("another secured workspace operation is still running")
	errOperationResultExpired             = errors.New("workspace-agent operation result has expired")
	errOperationConflict                  = errors.New("operationID conflicts with a different request")
	errResetOperationCapacity             = errors.New("workspace-agent reset operation capacity is full")
	errAttachmentRevoked                  = errors.New("workspace attachment is no longer active")
	errAttachmentExpired                  = errors.New("workspace attachment has expired")
	errCommandWriteConfinementUnavailable = errors.New("secured command write confinement is unavailable")
)

type resetOperationRecord struct {
	Fingerprint string
	Generation  string
	CompletedAt time.Time
}

type requestContextKey int

const (
	legacyAuthContextKey requestContextKey = iota
	attachmentEpochContextKey
)

func (s *workspaceAgentServer) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(workspaceagent.LegacyHealthPath, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc(workspaceagent.HealthPath, s.handleHealth)
	mux.HandleFunc(workspaceagent.CapabilitiesPath, s.handleCapabilities)
	mux.HandleFunc(workspaceagent.AttachmentControlPath, s.requireControlAuth(s.handleAttachmentControl))
	mux.HandleFunc(workspaceagent.AttachmentControlPrefix, s.requireControlAuth(s.handleAttachmentControl))
	mux.HandleFunc(workspaceagent.ExecPath, s.requireDataAuth(s.handleExec))
	mux.HandleFunc(workspaceagent.ExecStatusPrefix, s.requireDataAuth(s.handleExecStatus))
	mux.HandleFunc(workspaceagent.FilesPath, s.requireDataAuth(s.handleFiles))
	mux.HandleFunc(workspaceagent.FilesDownloadPath, s.requireDataAuth(s.handleDownload))
	mux.HandleFunc(workspaceagent.ScrubPath, s.requireControlOrLegacyAuth(s.handleScrub))
	mux.HandleFunc(workspaceagent.ResetPath, s.requireControlAuth(s.handleReset))
	return mux
}

func (s *workspaceAgentServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, workspaceagent.HealthResponse{Versioned: workspaceagent.NewVersioned(), Status: "ok"})
}

func (s *workspaceAgentServer) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	bindingGeneration := s.bindingGenerationSnapshot()
	features := []string{"exec", "files", "scrub"}
	if s.controlAuthConfigured {
		features = append(features, "attachment-fencing", "exec-idempotency", "exec-cancel")
		if s.writeConfinement {
			features = append(features, "reset")
		}
	}
	writeJSON(w, workspaceagent.CapabilitiesResponse{
		Versioned:               workspaceagent.NewVersioned(),
		Features:                features,
		MaxRequestBytes:         s.maxRequestBytes,
		MaxOutputBytes:          s.defaultMaxOutputBytes,
		MaxDownloadBytes:        s.maxDownloadBytes,
		OperationRetentionSec:   int64(completedExecutionRetention / time.Second),
		MaxRetainedOperations:   defaultMaxRetainedOperations,
		MaxOperationIDsPerEpoch: defaultMaxOperationIDsPerEpoch,
		BindingGeneration:       bindingGeneration,
	})
}

func (s *workspaceAgentServer) requireDataAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		active := s.activeAttachment
		legacyEnabled := s.legacyAuthEnabled
		if active != nil {
			if time.Now().UTC().After(active.expiresAt) {
				s.mu.Unlock()
				http.Error(w, "attachment expired", http.StatusUnauthorized)
				return
			}
			epoch, err := strconv.ParseInt(
				strings.TrimSpace(r.Header.Get(workspaceagent.AttachmentEpochHeader)),
				10,
				64,
			)
			wrongWorkspace := strings.TrimSpace(r.Header.Get(workspaceagent.WorkspaceUIDHeader)) != active.workspaceUID
			if err != nil || epoch != active.epoch || wrongWorkspace {
				s.mu.Unlock()
				http.Error(w, "attachment rejected", http.StatusUnauthorized)
				return
			}
			bearer, ok := bearerToken(r.Header.Get("Authorization"))
			if !ok || bearer == "" {
				s.mu.Unlock()
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			gotDigest := sha256.Sum256([]byte(bearer))
			if subtle.ConstantTimeCompare(gotDigest[:], active.authDigest[:]) != 1 {
				s.mu.Unlock()
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			s.inflightByEpoch[epoch]++
			s.mu.Unlock()

			r = r.WithContext(context.WithValue(r.Context(), attachmentEpochContextKey, epoch))
			defer s.finishDataRequest(epoch)
			next(w, r)
			return
		}
		expiredEpoch, _ := strconv.ParseInt(
			strings.TrimSpace(r.Header.Get(workspaceagent.AttachmentEpochHeader)),
			10,
			64,
		)
		expired := expiredEpoch > 0 &&
			(s.expiringEpoch == expiredEpoch || s.lastExpiredEpoch == expiredEpoch)
		s.mu.Unlock()

		if expired {
			http.Error(w, "attachment expired", http.StatusUnauthorized)
			return
		}
		if !legacyEnabled {
			http.Error(w, "no active attachment", http.StatusUnauthorized)
			return
		}
		s.requireLegacyAuth(func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(context.WithValue(r.Context(), legacyAuthContextKey, true))
			next(w, r)
		})(w, r)
	}
}

func (s *workspaceAgentServer) finishDataRequest(epoch int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inflightByEpoch[epoch] <= 1 {
		delete(s.inflightByEpoch, epoch)
		return
	}
	s.inflightByEpoch[epoch]--
}

func (s *workspaceAgentServer) requireControlAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.controlAuth == "" {
			http.Error(w, "workspace control credential unavailable", http.StatusServiceUnavailable)
			return
		}
		if !validHandoffBearer(r.Header.Get("Authorization"), s.controlAuth) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *workspaceAgentServer) requireControlOrLegacyAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.controlAuth != "" {
			s.requireControlAuth(next)(w, r)
			return
		}
		s.requireLegacyAuth(func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(context.WithValue(r.Context(), legacyAuthContextKey, true))
			next(w, r)
		})(w, r)
	}
}

func (s *workspaceAgentServer) requireLegacyAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, err := handoffToken()
		if err != nil {
			if handoffBootstrapAllowedForTokenError(err) {
				allowBootstrap, handled := s.allowHandoffBootstrap(w, r)
				if allowBootstrap {
					next(w, r)
					return
				}
				if handled {
					return
				}
			}
			http.Error(w, "handoff credential unavailable", http.StatusServiceUnavailable)
			return
		}
		if !validHandoffBearer(r.Header.Get("Authorization"), token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func requestUsesLegacyAuth(r *http.Request) bool {
	legacy, _ := r.Context().Value(legacyAuthContextKey).(bool)
	return legacy
}

func requestAttachmentEpoch(r *http.Request) int64 {
	epoch, _ := r.Context().Value(attachmentEpochContextKey).(int64)
	return epoch
}

func (s *workspaceAgentServer) revalidateDataRequest(w http.ResponseWriter, r *http.Request) bool {
	if requestUsesLegacyAuth(r) {
		return true
	}
	epoch := requestAttachmentEpoch(r)
	s.mu.Lock()
	expiring := s.expiringEpoch == epoch
	revoking := s.revokingEpoch == epoch && !expiring
	active := s.activeAttachment != nil && s.activeAttachment.epoch == epoch &&
		time.Now().UTC().Before(s.activeAttachment.expiresAt)
	s.mu.Unlock()
	if expiring {
		http.Error(w, "attachment expired", http.StatusUnauthorized)
		return false
	}
	if revoking {
		http.Error(w, "attachment revocation is in progress", http.StatusConflict)
		return false
	}
	if !active {
		http.Error(w, "attachment expired or revoked", http.StatusUnauthorized)
	}
	return active
}

func (s *workspaceAgentServer) handleAttachmentControl(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPut:
		s.handleAttachmentActivation(w, r)
	case http.MethodDelete:
		s.handleAttachmentRevocation(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *workspaceAgentServer) handleAttachmentActivation(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != workspaceagent.AttachmentControlPath {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	var req workspaceagent.AttachmentControlRequest
	if err := s.decodeJSON(r, &req); err != nil || validateProtocolVersion(req.ProtocolVersion, false) != nil {
		http.Error(w, "invalid attachment request", http.StatusBadRequest)
		return
	}
	workspaceUID := strings.TrimSpace(req.WorkspaceUID)
	taskUID := strings.TrimSpace(req.TaskUID)
	bindingGeneration := strings.TrimSpace(req.BindingGeneration)
	if workspaceUID == "" || taskUID == "" || bindingGeneration == "" ||
		req.Epoch <= 0 || !req.ExpiresAt.After(time.Now().UTC()) {
		http.Error(w, "invalid attachment request", http.StatusBadRequest)
		return
	}
	authDigest, err := parseSHA256Digest(req.TokenSHA256)
	if err != nil {
		http.Error(w, "invalid attachment token digest", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if bindingGeneration != s.bindingGeneration {
		http.Error(w, "workspace binding generation is stale", http.StatusConflict)
		return
	}
	if s.revokingEpoch != 0 {
		http.Error(w, "attachment revocation is still draining", http.StatusConflict)
		return
	}
	if s.cleanupInProgress {
		http.Error(w, "workspace cleanup is in progress", http.StatusConflict)
		return
	}
	if s.resetRequired {
		http.Error(w, "workspace reset is required after agent startup", http.StatusConflict)
		return
	}
	if s.activeAttachment != nil && req.Epoch != s.activeAttachment.epoch {
		http.Error(w, "active attachment must be revoked before epoch rollover", http.StatusConflict)
		return
	}
	if s.boundWorkspaceUID != "" && s.boundWorkspaceUID != workspaceUID {
		http.Error(w, "workspace reset is required before binding a different workspace UID", http.StatusConflict)
		return
	}
	if req.Epoch < s.lastEpoch {
		http.Error(w, "attachment epoch is stale", http.StatusConflict)
		return
	}
	if req.Epoch == s.lastEpoch {
		if !attachmentStateMatches(s.activeAttachment, workspaceUID, taskUID, authDigest) {
			http.Error(w, "attachment epoch conflicts with prior activation", http.StatusConflict)
			return
		}
		if !req.ExpiresAt.UTC().Equal(s.activeAttachment.expiresAt) {
			http.Error(w, "same-epoch attachment request differs from active attachment", http.StatusConflict)
			return
		}
		writeAttachmentControlResponse(w, workspaceUID, s.bindingGeneration, req.Epoch, true)
		return
	}
	s.boundWorkspaceUID = workspaceUID
	s.lastEpoch = req.Epoch
	s.activeAttachment = &attachmentState{
		workspaceUID: workspaceUID,
		taskUID:      taskUID,
		epoch:        req.Epoch,
		authDigest:   authDigest,
		expiresAt:    req.ExpiresAt.UTC(),
	}
	s.scheduleAttachmentExpiryLocked(s.activeAttachment)
	writeAttachmentControlResponse(w, workspaceUID, s.bindingGeneration, req.Epoch, true)
}

func (s *workspaceAgentServer) scheduleAttachmentExpiryLocked(active *attachmentState) {
	if s.attachmentExpiryCancel != nil {
		s.attachmentExpiryCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.attachmentExpiryCancel = cancel
	workspaceUID := active.workspaceUID
	bindingGeneration := s.bindingGeneration
	epoch := active.epoch
	expiresAt := active.expiresAt
	go func() {
		timer := time.NewTimer(max(time.Until(expiresAt), 0))
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			s.expireAttachment(workspaceUID, bindingGeneration, epoch, expiresAt)
		}
	}()
}

func (s *workspaceAgentServer) expireAttachment(
	workspaceUID string,
	bindingGeneration string,
	epoch int64,
	expiresAt time.Time,
) {
	s.mu.Lock()
	active := s.activeAttachment
	stale := active == nil || active.workspaceUID != workspaceUID || active.epoch != epoch ||
		s.bindingGeneration != bindingGeneration || s.revokingEpoch != 0 ||
		time.Now().UTC().Before(expiresAt)
	if stale {
		s.mu.Unlock()
		return
	}
	s.expiringEpoch = epoch
	s.revokingEpoch = epoch
	s.revocationDone = make(chan struct{})
	done := s.revocationDone
	s.attachmentExpiryCancel = nil
	s.activeAttachment = nil
	cancels := s.epochExecutionCancelsLocked(epoch)
	terminator := s.processTerminator
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), attachmentRevocationTimeout)
	defer cancel()
	var terminationErr error
	if terminator != nil {
		terminationErr = terminator(ctx)
	}
	for _, cancel := range cancels {
		cancel()
	}
	if terminationErr != nil {
		s.failRevocationAttempt(epoch, done)
		slog.Error("automatic attachment expiry failed to terminate processes", "epoch", epoch, "err", terminationErr)
		return
	}
	if err := s.waitForEpochDrain(ctx, epoch); err != nil {
		s.failRevocationAttempt(epoch, done)
		slog.Error("automatic attachment expiry did not drain active operations", "epoch", epoch, "err", err)
		return
	}
	s.completeRevocation(epoch, done, workspaceUID, bindingGeneration)
}

//nolint:gocyclo // Revocation serializes owner, waiter, retry, and fail-closed paths explicitly.
func (s *workspaceAgentServer) handleAttachmentRevocation(w http.ResponseWriter, r *http.Request) {
	rawEpoch := strings.Trim(strings.TrimPrefix(r.URL.Path, workspaceagent.AttachmentControlPrefix), "/")
	epoch, err := strconv.ParseInt(rawEpoch, 10, 64)
	if err != nil || epoch <= 0 {
		http.Error(w, "invalid attachment epoch", http.StatusBadRequest)
		return
	}
	var request workspaceagent.AttachmentRevocationRequest
	if err := s.decodeJSON(r, &request); err != nil ||
		validateProtocolVersion(request.ProtocolVersion, false) != nil ||
		strings.TrimSpace(request.WorkspaceUID) == "" ||
		strings.TrimSpace(request.BindingGeneration) == "" {
		http.Error(w, "invalid attachment revocation request", http.StatusBadRequest)
		return
	}

	for {
		s.mu.Lock()
		if s.cleanupInProgress {
			s.mu.Unlock()
			http.Error(w, "workspace cleanup is in progress", http.StatusConflict)
			return
		}
		if request.BindingGeneration != s.bindingGeneration || request.WorkspaceUID != s.boundWorkspaceUID {
			s.mu.Unlock()
			http.Error(w, "workspace binding identity is stale", http.StatusConflict)
			return
		}
		if s.lastRevokedEpoch == epoch && s.activeAttachment == nil && s.revokingEpoch == 0 {
			workspaceUID := s.lastRevokedWorkspace
			bindingGeneration := s.lastRevokedBinding
			s.mu.Unlock()
			writeAttachmentControlResponse(w, workspaceUID, bindingGeneration, epoch, false)
			return
		}
		if s.revokingEpoch != 0 {
			if s.revokingEpoch != epoch {
				s.mu.Unlock()
				http.Error(w, "another attachment epoch is already revoking", http.StatusConflict)
				return
			}
			if s.revocationDone != nil {
				done := s.revocationDone
				s.mu.Unlock()
				select {
				case <-r.Context().Done():
					http.Error(w, "attachment revocation wait cancelled", http.StatusRequestTimeout)
					return
				case <-done:
					continue
				}
			}
		} else {
			if epoch < s.lastEpoch {
				s.mu.Unlock()
				http.Error(w, "attachment epoch is stale", http.StatusConflict)
				return
			}
			if epoch > s.lastEpoch {
				s.mu.Unlock()
				http.Error(w, "attachment epoch has not been activated", http.StatusConflict)
				return
			}
			if s.activeAttachment != nil && s.activeAttachment.epoch != epoch {
				activeEpoch := s.activeAttachment.epoch
				s.mu.Unlock()
				http.Error(
					w,
					fmt.Sprintf("attachment epoch conflicts with active epoch %d", activeEpoch),
					http.StatusConflict,
				)
				return
			}
			s.revokingEpoch = epoch
		}
		if s.attachmentExpiryCancel != nil {
			s.attachmentExpiryCancel()
			s.attachmentExpiryCancel = nil
		}
		s.revocationDone = make(chan struct{})
		done := s.revocationDone
		workspaceUID := request.WorkspaceUID
		if s.activeAttachment != nil {
			workspaceUID = s.activeAttachment.workspaceUID
			s.activeAttachment = nil
		}
		cancels := s.epochExecutionCancelsLocked(epoch)
		terminator := s.processTerminator
		s.mu.Unlock()

		var terminationErr error
		if terminator != nil {
			terminationErr = terminator(r.Context())
		}
		for _, cancel := range cancels {
			cancel()
		}
		if terminationErr != nil {
			s.failRevocationAttempt(epoch, done)
			http.Error(w, "failed to terminate attachment processes", http.StatusInternalServerError)
			return
		}
		if err := s.waitForEpochDrain(r.Context(), epoch); err != nil {
			s.failRevocationAttempt(epoch, done)
			http.Error(w, "attachment revocation did not drain active operations", http.StatusRequestTimeout)
			return
		}
		bindingGeneration := s.bindingGenerationSnapshot()
		s.completeRevocation(epoch, done, workspaceUID, bindingGeneration)
		writeAttachmentControlResponse(w, workspaceUID, bindingGeneration, epoch, false)
		return
	}
}

func (s *workspaceAgentServer) epochExecutionCancelsLocked(epoch int64) []context.CancelFunc {
	cancels := make([]context.CancelFunc, 0, len(s.executionCancels))
	for operationID, cancel := range s.executionCancels {
		if s.executionEpochs[operationID] == epoch && s.operationRunning[operationID] {
			cancels = append(cancels, cancel)
		}
	}
	return cancels
}

func (s *workspaceAgentServer) completeRevocation(
	epoch int64,
	done chan struct{},
	workspaceUID string,
	bindingGeneration string,
) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.revokingEpoch != epoch || s.revocationDone != done {
		return
	}
	s.purgeEpochOperationsLocked(epoch)
	s.revokingEpoch = 0
	s.revocationDone = nil
	s.lastRevokedEpoch = epoch
	s.lastRevokedWorkspace = workspaceUID
	s.lastRevokedBinding = bindingGeneration
	if s.expiringEpoch == epoch {
		s.lastExpiredEpoch = epoch
		s.expiringEpoch = 0
	}
	close(done)
}

func (s *workspaceAgentServer) failRevocationAttempt(epoch int64, done chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.revokingEpoch == epoch && s.revocationDone == done {
		s.revocationDone = nil
		close(done)
	}
}

func (s *workspaceAgentServer) purgeEpochOperationsLocked(epoch int64) {
	for operationID, operationEpoch := range s.executionEpochs {
		if operationEpoch != epoch {
			continue
		}
		delete(s.executions, operationID)
		delete(s.executionCancels, operationID)
		delete(s.executionEpochs, operationID)
		delete(s.executionFingerprints, operationID)
		delete(s.executionTombstones, operationID)
		delete(s.operationRunning, operationID)
	}
	delete(s.runningByEpoch, epoch)
}

func (s *workspaceAgentServer) waitForEpochDrain(ctx context.Context, epoch int64) error {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		s.mu.Lock()
		drained := s.inflightByEpoch[epoch] == 0 && s.runningByEpoch[epoch] == 0
		s.mu.Unlock()
		if drained {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func attachmentStateMatches(
	active *attachmentState,
	workspaceUID string,
	taskUID string,
	authDigest [sha256.Size]byte,
) bool {
	return active != nil &&
		active.workspaceUID == workspaceUID &&
		active.taskUID == taskUID &&
		subtle.ConstantTimeCompare(active.authDigest[:], authDigest[:]) == 1
}

func writeAttachmentControlResponse(
	w http.ResponseWriter,
	workspaceUID string,
	bindingGeneration string,
	epoch int64,
	active bool,
) {
	writeJSON(w, workspaceagent.AttachmentControlResponse{
		Versioned:         workspaceagent.NewVersioned(),
		WorkspaceUID:      workspaceUID,
		BindingGeneration: bindingGeneration,
		ActiveEpoch:       epoch,
		Active:            active,
	})
}

func validateProtocolVersion(version string, allowEmpty bool) error {
	version = strings.TrimSpace(version)
	if version == "" && allowEmpty {
		return nil
	}
	if version != workspaceagent.ProtocolVersion {
		return fmt.Errorf("unsupported protocol version")
	}
	return nil
}

func requestProtocolValid(r *http.Request, version string) bool {
	return validateProtocolVersion(version, requestUsesLegacyAuth(r)) == nil
}

func parseSHA256Digest(value string) ([sha256.Size]byte, error) {
	var out [sha256.Size]byte
	encoded, ok := strings.CutPrefix(strings.TrimSpace(value), "sha256:")
	if !ok {
		return out, fmt.Errorf("digest must use sha256")
	}
	decoded, err := hex.DecodeString(encoded)
	if err != nil || len(decoded) != sha256.Size {
		return out, fmt.Errorf("invalid sha256 digest")
	}
	copy(out[:], decoded)
	return out, nil
}

func handoffBootstrapAllowedForTokenError(err error) bool {
	return errors.Is(err, errHandoffAuthMissing) || errors.Is(err, errHandoffAuthEmpty)
}

func (s *workspaceAgentServer) allowHandoffBootstrap(w http.ResponseWriter, r *http.Request) (bool, bool) {
	if r.Method != http.MethodPut || r.URL.Path != daemonprotocol.FilesPath {
		return false, false
	}
	if s.bootstrapAuth == "" {
		http.Error(w, "handoff bootstrap credential unavailable", http.StatusServiceUnavailable)
		return false, true
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "invalid handoff bootstrap request", http.StatusBadRequest)
		return false, true
	}

	var req uploadRequest
	if err := json.Unmarshal(data, &req); err != nil || len(req.Files) != 1 {
		http.Error(w, "invalid handoff bootstrap request", http.StatusBadRequest)
		return false, true
	}
	path := filepath.Clean(req.Files[0].Path)
	requestedPath, err := normalizeAgentPath(path)
	if err != nil {
		http.Error(w, "invalid handoff bootstrap path", http.StatusUnauthorized)
		return false, true
	}
	tokenPath := handoffTokenFilePath()
	isDefaultUploadAlias := path == defaultHandoffUploadAlias || requestedPath == defaultHandoffFile
	if !isDefaultUploadAlias && requestedPath != tokenPath {
		http.Error(w, "invalid handoff bootstrap path", http.StatusUnauthorized)
		return false, true
	}
	tokenValue := strings.TrimSpace(string(req.Files[0].Data))
	if tokenValue == "" {
		http.Error(w, "empty handoff bootstrap token", http.StatusBadRequest)
		return false, true
	}
	if !validHandoffBearer(r.Header.Get("Authorization"), s.bootstrapAuth) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false, true
	}
	req.Files[0].Data = []byte(tokenValue)
	req.Files[0].Path = tokenPath
	data, err = json.Marshal(req)
	if err != nil {
		http.Error(w, "invalid handoff bootstrap request", http.StatusBadRequest)
		return false, true
	}
	r.Body.Close() //nolint:errcheck
	r.Body = io.NopCloser(bytes.NewReader(data))
	return true, true
}

func handoffToken() (string, error) {
	if token := strings.TrimSpace(os.Getenv(envHandoffAuth)); token != "" {
		return token, nil
	}
	data, err := os.ReadFile(handoffTokenFilePath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: %w", errHandoffAuthMissing, err)
		}
		return "", err
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", errHandoffAuthEmpty
	}
	return token, nil
}

func handoffTokenFilePath() string {
	path := strings.TrimSpace(os.Getenv(envHandoffAuthFile))
	if path == "" {
		path = defaultHandoffFile
	}
	normalized, err := normalizeAgentPath(path)
	if err != nil {
		return defaultHandoffFile
	}
	return normalized
}

func normalizeAgentPath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("path is required")
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join("/app", value)
	}
	return filepath.Clean(value), nil
}

func validHandoffBearer(header, token string) bool {
	got, ok := bearerToken(header)
	if !ok {
		return false
	}
	if got == "" || strings.TrimSpace(token) == "" {
		return false
	}
	gotHash := sha256.Sum256([]byte(got))
	authDigest := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(gotHash[:], authDigest[:]) == 1
}

func bearerToken(header string) (string, bool) {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(header, prefix)), true
}

func (s *workspaceAgentServer) handleExec(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req execRequest
	if err := s.decodeJSON(r, &req); err != nil || !requestProtocolValid(r, req.ProtocolVersion) {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if !s.revalidateDataRequest(w, r) {
		return
	}
	if s.controlAuthConfigured && !s.writeConfinement {
		http.Error(w, errCommandWriteConfinementUnavailable.Error(), http.StatusServiceUnavailable)
		return
	}
	if len(req.Command) == 0 || strings.TrimSpace(req.Command[0]) == "" {
		http.Error(w, "command is required", http.StatusBadRequest)
		return
	}
	normalized, err := s.normalizeExecRequest(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Resident {
		http.Error(w, "resident exec is not supported yet", http.StatusBadRequest)
		return
	}

	operationID := strings.TrimSpace(req.OperationID)
	legacyRequest := requestUsesLegacyAuth(r)
	if operationID == "" && req.Detach && legacyRequest {
		operationID, err = newExecID()
		if err != nil {
			http.Error(w, "failed to create execution", http.StatusInternalServerError)
			return
		}
	}
	if operationID == "" && !legacyRequest {
		http.Error(w, "operationID is required", http.StatusBadRequest)
		return
	}
	if operationID != "" {
		if !validOperationID(operationID) {
			http.Error(w, "invalid operationID", http.StatusBadRequest)
			return
		}
		req.OperationID = operationID
		resp, err := s.startExecution(req, normalized, requestAttachmentEpoch(r))
		if err != nil {
			http.Error(w, err.Error(), executionStartErrorStatus(err))
			return
		}
		writeJSON(w, resp)
		return
	}

	// Legacy callers without operation IDs retain synchronous behavior during migration.
	writeJSON(w, s.runExec(r.Context(), req, normalized))
}

func executionStartErrorStatus(err error) int {
	switch {
	case errors.Is(err, errOperationCapacity), errors.Is(err, errOperationBusy):
		return http.StatusTooManyRequests
	case errors.Is(err, errOperationResultExpired):
		return http.StatusGone
	case errors.Is(err, errAttachmentExpired):
		return http.StatusUnauthorized
	default:
		return http.StatusConflict
	}
}

func (s *workspaceAgentServer) handleExecStatus(w http.ResponseWriter, r *http.Request) {
	relative := strings.Trim(strings.TrimPrefix(r.URL.Path, workspaceagent.ExecStatusPrefix), "/")
	isCancel := strings.HasSuffix(relative, "/cancel")
	if isCancel {
		relative = strings.TrimSuffix(relative, "/cancel")
	}
	operationID := strings.TrimSpace(relative)
	if !validOperationID(operationID) {
		http.Error(w, "operationID is required", http.StatusBadRequest)
		return
	}

	if isCancel {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var version workspaceagent.Versioned
		if err := s.decodeJSON(r, &version); err != nil || !requestProtocolValid(r, version.ProtocolVersion) {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if !s.revalidateDataRequest(w, r) {
			return
		}
		state, ok, conflict := s.cancelExecution(operationID, requestAttachmentEpoch(r))
		if conflict {
			http.Error(w, "operation belongs to a different attachment epoch", http.StatusConflict)
			return
		}
		if !ok {
			http.Error(w, "execution not found", http.StatusNotFound)
			return
		}
		writeJSON(w, workspaceagent.CancelResponse{
			Versioned:   workspaceagent.NewVersioned(),
			OperationID: operationID,
			State:       state,
		})
		return
	}

	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	resp, ok, conflict, gone := s.loadExecution(operationID, requestAttachmentEpoch(r))
	if conflict {
		http.Error(w, "operation belongs to a different attachment epoch", http.StatusConflict)
		return
	}
	if gone {
		http.Error(w, "operation result expired", http.StatusGone)
		return
	}
	if !ok {
		http.Error(w, "execution not found", http.StatusNotFound)
		return
	}
	writeJSON(w, resp)
}

type normalizedExecRequest struct {
	workDir   string
	timeout   time.Duration
	maxOutput int64
}

func (s *workspaceAgentServer) normalizeExecRequest(req execRequest) (normalizedExecRequest, error) {
	workDir := strings.TrimSpace(req.WorkDir)
	if workDir == "" {
		workDir = defaultWorkspaceRoot
	}
	safeWorkDir, err := safePath(workDir)
	if err != nil {
		return normalizedExecRequest{}, fmt.Errorf("invalid workDir")
	}
	timeout := s.defaultCommandTimeout
	if req.TimeoutSeconds > 0 {
		timeout = time.Duration(req.TimeoutSeconds) * time.Second
	}
	maxOutput := req.MaxOutputBytes
	if maxOutput <= 0 || maxOutput > s.defaultMaxOutputBytes {
		maxOutput = s.defaultMaxOutputBytes
	}
	return normalizedExecRequest{workDir: safeWorkDir, timeout: timeout, maxOutput: maxOutput}, nil
}

func (s *workspaceAgentServer) startExecution(
	req execRequest,
	normalized normalizedExecRequest,
	epoch int64,
) (execResponse, error) {
	operationID := req.OperationID
	fingerprint, err := executionRequestFingerprint(req, normalized)
	if err != nil {
		return execResponse{}, fmt.Errorf("fingerprint operation request: %w", err)
	}
	s.mu.Lock()
	now := time.Now().UTC()
	s.evictCompletedExecutionsLocked(now)
	if ownerEpoch, used := s.executionEpochs[operationID]; used {
		if ownerEpoch != epoch || s.executionFingerprints[operationID] != fingerprint {
			s.mu.Unlock()
			return execResponse{}, errOperationConflict
		}
		if existing, ok := s.executions[operationID]; ok {
			s.mu.Unlock()
			return existing, nil
		}
		s.mu.Unlock()
		return execResponse{}, errOperationResultExpired
	}
	if epoch > 0 {
		active := s.activeAttachment
		if active == nil || active.epoch != epoch {
			expired := s.expiringEpoch == epoch || s.lastExpiredEpoch == epoch
			s.mu.Unlock()
			if expired {
				return execResponse{}, errAttachmentExpired
			}
			return execResponse{}, errAttachmentRevoked
		}
		if !now.Before(active.expiresAt) {
			s.mu.Unlock()
			return execResponse{}, errAttachmentExpired
		}
	}
	if s.controlAuthConfigured && s.hasRunningOperationLocked() {
		s.mu.Unlock()
		return execResponse{}, errOperationBusy
	}
	if epoch > 0 && s.operationIDCountForEpochLocked(epoch) >= defaultMaxOperationIDsPerEpoch {
		s.mu.Unlock()
		return execResponse{}, errOperationCapacity
	}
	if len(s.executions) >= defaultMaxRetainedOperations {
		s.mu.Unlock()
		return execResponse{}, errOperationCapacity
	}

	ctx, cancel := context.WithCancel(context.Background())
	started := now
	initial := execResponse{
		Versioned:   workspaceagent.NewVersioned(),
		OperationID: operationID,
		ExecID:      operationID,
		State:       workspaceagent.OperationStateRunning,
		Running:     true,
		StartedAt:   started,
	}
	s.executions[operationID] = initial
	s.executionCancels[operationID] = cancel
	s.executionEpochs[operationID] = epoch
	s.executionFingerprints[operationID] = fingerprint
	s.operationRunning[operationID] = true
	s.runningByEpoch[epoch]++
	s.mu.Unlock()

	go func() {
		defer cancel()
		resp := s.runExec(ctx, req, normalized)
		resp.OperationID = operationID
		resp.ExecID = operationID
		s.storeExecution(resp)
	}()
	return initial, nil
}

func (s *workspaceAgentServer) operationIDCountForEpochLocked(epoch int64) int {
	count := 0
	for _, operationEpoch := range s.executionEpochs {
		if operationEpoch == epoch {
			count++
		}
	}
	return count
}

func (s *workspaceAgentServer) hasRunningOperationLocked() bool {
	for _, running := range s.operationRunning {
		if running {
			return true
		}
	}
	return false
}

func executionRequestFingerprint(
	req execRequest, normalized normalizedExecRequest,
) (string, error) {
	payload := struct {
		Command        []string          `json:"command"`
		Env            map[string]string `json:"env,omitempty"`
		WorkDir        string            `json:"workDir"`
		Stdin          []byte            `json:"stdin,omitempty"`
		TimeoutNanos   int64             `json:"timeoutNanos"`
		MaxOutputBytes int64             `json:"maxOutputBytes"`
		Resident       bool              `json:"resident"`
		ResidentKey    string            `json:"residentKey,omitempty"`
	}{
		Command:        append([]string(nil), req.Command...),
		Env:            req.Env,
		WorkDir:        normalized.workDir,
		Stdin:          append([]byte(nil), req.Stdin...),
		TimeoutNanos:   int64(normalized.timeout),
		MaxOutputBytes: normalized.maxOutput,
		Resident:       req.Resident,
		ResidentKey:    req.ResidentKey,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return digest(data), nil
}

func (s *workspaceAgentServer) runExec(
	ctx context.Context,
	req execRequest,
	normalized normalizedExecRequest,
) execResponse {
	ctx, cancel := context.WithTimeout(ctx, normalized.timeout)
	defer cancel()
	startedAt := time.Now().UTC()
	var cmd *exec.Cmd
	var err error
	if s.controlAuthConfigured && s.writeConfinement {
		var executable string
		executable, err = os.Executable()
		if err == nil {
			args := append([]string{commandConfinementWrapperArg}, req.Command...)
			cmd = exec.CommandContext(ctx, executable, args...)
		}
	} else {
		cmd = exec.CommandContext(ctx, req.Command[0], req.Command[1:]...)
	}
	if err == nil {
		configureCommandCancellation(cmd, os.Geteuid() == 0, s.commandUID, s.commandGID)
		cmd.Dir = normalized.workDir
		cmd.Env = mergeEnv(commandBaseEnv(os.Environ()), req.Env)
	}
	if err == nil && len(req.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(req.Stdin)
	}
	stdout := &boundedBuffer{limit: normalized.maxOutput}
	stderr := &boundedBuffer{limit: normalized.maxOutput}
	if err == nil {
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		err = cmd.Start()
	}
	groupID := commandProcessGroupID(cmd)
	if err == nil {
		err = cmd.Wait()
	}
	groupCtx, groupCancel := context.WithTimeout(context.Background(), processGroupDrainTimeout)
	groupErr := terminateAndWaitForProcessGroup(groupCtx, groupID)
	groupCancel()
	isolationErr := groupErr
	if s.controlAuthConfigured || s.processTerminator != nil {
		if s.processTerminator == nil {
			isolationErr = errors.Join(groupErr, errors.New("workspace process terminator is unavailable"))
		} else {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), attachmentRevocationTimeout)
			namespaceErr := s.processTerminator(cleanupCtx)
			cleanupCancel()
			if namespaceErr == nil {
				isolationErr = nil
			} else {
				isolationErr = errors.Join(groupErr, namespaceErr)
			}
		}
	}
	finishedAt := time.Now().UTC()
	exitCode := 0
	state := workspaceagent.OperationStateSucceeded
	if err != nil {
		var exitErr *exec.ExitError
		switch {
		case errors.Is(ctx.Err(), context.Canceled):
			exitCode = 130
			state = workspaceagent.OperationStateCancelled
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			exitCode = 124
			state = workspaceagent.OperationStateFailed
		case errors.As(err, &exitErr):
			exitCode = exitErr.ExitCode()
			state = workspaceagent.OperationStateFailed
		default:
			exitCode = 1
			state = workspaceagent.OperationStateFailed
		}
	}
	return execResponse{
		Versioned:       workspaceagent.NewVersioned(),
		State:           state,
		Running:         false,
		Stdout:          stdout.String(),
		Stderr:          stderr.String(),
		ExitCode:        exitCode,
		StdoutTruncated: stdout.truncated,
		StderrTruncated: stderr.truncated,
		IsolationFailed: isolationErr != nil,
		StartedAt:       startedAt,
		FinishedAt:      finishedAt,
	}
}

func terminateAndWaitForProcessGroup(ctx context.Context, groupID int) error {
	if groupID <= 0 || !processGroupAlive(groupID) {
		return nil
	}
	if err := terminateProcessGroup(groupID); err != nil {
		return err
	}
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for processGroupAlive(groupID) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	return nil
}

func (s *workspaceAgentServer) storeExecution(resp execResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if resp.OperationID == "" {
		resp.OperationID = resp.ExecID
	}
	if resp.ExecID == "" {
		resp.ExecID = resp.OperationID
	}
	if _, exists := s.executions[resp.OperationID]; !exists {
		return
	}
	s.executions[resp.OperationID] = resp
	delete(s.executionCancels, resp.OperationID)
	if s.operationRunning[resp.OperationID] {
		epoch := s.executionEpochs[resp.OperationID]
		delete(s.operationRunning, resp.OperationID)
		if s.runningByEpoch[epoch] <= 1 {
			delete(s.runningByEpoch, epoch)
		} else {
			s.runningByEpoch[epoch]--
		}
	}
	s.evictCompletedExecutionsLocked(time.Now().UTC())
}

func (s *workspaceAgentServer) loadExecution(
	operationID string,
	epoch int64,
) (execResponse, bool, bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictCompletedExecutionsLocked(time.Now().UTC())
	ownerEpoch, used := s.executionEpochs[operationID]
	if used && ownerEpoch != epoch {
		return execResponse{}, false, true, false
	}
	resp, ok := s.executions[operationID]
	return resp, ok, false, used && !ok
}

func (s *workspaceAgentServer) cancelExecution(
	operationID string,
	epoch int64,
) (workspaceagent.OperationState, bool, bool) {
	s.mu.Lock()
	resp, ok := s.executions[operationID]
	if !ok {
		s.mu.Unlock()
		return "", false, false
	}
	if s.executionEpochs[operationID] != epoch {
		s.mu.Unlock()
		return "", false, true
	}
	if !resp.Running {
		state := resp.State
		s.mu.Unlock()
		return state, true, false
	}
	cancel := s.executionCancels[operationID]
	state := resp.State
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return state, true, false
}

func (s *workspaceAgentServer) evictCompletedExecutionsLocked(now time.Time) {
	for id, resp := range s.executions {
		if resp.Running || resp.FinishedAt.IsZero() || now.Sub(resp.FinishedAt) < completedExecutionRetention {
			continue
		}
		delete(s.executions, id)
		delete(s.executionCancels, id)
		if s.executionEpochs[id] == 0 {
			delete(s.executionEpochs, id)
			delete(s.executionFingerprints, id)
			continue
		}
		s.executionTombstones[id] = now
	}
	for id, expiredAt := range s.executionTombstones {
		if now.Sub(expiredAt) < operationTombstoneRetention {
			continue
		}
		delete(s.executionTombstones, id)
	}
	for len(s.executionTombstones) > defaultMaxOperationTombstones {
		oldestID := ""
		var oldest time.Time
		for id, expiredAt := range s.executionTombstones {
			if oldestID == "" || expiredAt.Before(oldest) {
				oldestID = id
				oldest = expiredAt
			}
		}
		delete(s.executionTombstones, oldestID)
	}
}

func validOperationID(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || value == "." || value == ".." || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func newBindingGeneration() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(random[:]), nil
}

func newExecID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func (s *workspaceAgentServer) handleFiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req uploadRequest
	if err := s.decodeJSON(r, &req); err != nil || len(req.Files) == 0 || !requestProtocolValid(r, req.ProtocolVersion) {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if !s.revalidateDataRequest(w, r) {
		return
	}
	artifacts := make([]artifact, 0, len(req.Files))
	for _, file := range req.Files {
		path, err := normalizeAgentPath(file.Path)
		if err == nil && !requestUsesLegacyAuth(r) && !v1DataPathAllowed(path) {
			err = fmt.Errorf("path is outside v1 data roots")
		}
		if err != nil || (!requestUsesLegacyAuth(r) && s.pathConflictsProtected(path)) {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}
		mode := os.FileMode(file.Mode)
		if mode&^os.ModePerm != 0 {
			http.Error(w, "unsupported file mode", http.StatusBadRequest)
			return
		}
		mode = mode.Perm()
		if mode == 0 {
			mode = 0o644
		}
		mode, setOwner := uploadWritePolicy(path, requestUsesLegacyAuth(r), os.Geteuid() == 0, mode)
		computedDigest := digest(file.Data)
		if strings.TrimSpace(file.Digest) != "" && !subtleStringEqual(file.Digest, computedDigest) {
			http.Error(w, "file digest mismatch", http.StatusBadRequest)
			return
		}
		metadata, err := secureWriteFile(
			file.Path,
			file.Data,
			mode,
			setOwner,
			s.commandUID,
			s.commandGID,
			file.ModTime,
		)
		if err != nil {
			http.Error(w, "failed to write file", workspaceFileErrorStatus(err))
			return
		}
		artifacts = append(artifacts, artifact{
			Path:    cleanReportedPath(file.Path),
			Size:    metadata.Size,
			Digest:  computedDigest,
			Mode:    metadata.Mode,
			ModTime: metadata.ModTime,
		})
	}
	writeJSON(w, uploadResponse{Versioned: workspaceagent.NewVersioned(), Artifacts: artifacts})
}

func uploadWritePolicy(
	path string,
	legacyRequest bool,
	supervisorRoot bool,
	mode os.FileMode,
) (os.FileMode, bool) {
	if legacyRequest && filepath.Clean(path) == filepath.Clean(handoffTokenFilePath()) {
		return 0o600, false
	}
	return mode.Perm(), supervisorRoot
}

func (s *workspaceAgentServer) handleDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req downloadRequest
	if err := s.decodeJSON(r, &req); err != nil || !requestProtocolValid(r, req.ProtocolVersion) {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if !s.revalidateDataRequest(w, r) {
		return
	}
	paths := req.Paths
	listAll := len(paths) == 0
	if listAll {
		listed, err := secureListFiles(defaultDownloadRoot())
		if err != nil {
			http.Error(w, "failed to list files", workspaceFileErrorStatus(err))
			return
		}
		paths = listed
	}
	artifacts := make([]downloadedArtifact, 0, len(paths))
	remaining := s.maxDownloadBytes
	if remaining <= 0 {
		remaining = defaultMaxDownloadBytes
	}
	for _, requested := range paths {
		if !requestUsesLegacyAuth(r) && s.pathConflictsProtected(requested) {
			if listAll {
				continue
			}
			http.Error(w, "failed to read file", http.StatusBadRequest)
			return
		}
		if !requestUsesLegacyAuth(r) && !v1DataPathAllowed(requested) {
			http.Error(w, "failed to read file", http.StatusBadRequest)
			return
		}
		data, metadata, err := secureReadFile(requested, remaining)
		if err != nil {
			status := workspaceFileErrorStatus(err)
			if errors.Is(err, os.ErrNotExist) {
				status = http.StatusNotFound
			}
			http.Error(w, "failed to read file", status)
			return
		}
		remaining -= int64(len(data))
		artifacts = append(artifacts, downloadedArtifact{
			Artifact: artifact{
				Path:    cleanReportedPath(requested),
				Size:    metadata.Size,
				Digest:  digest(data),
				Mode:    metadata.Mode,
				ModTime: metadata.ModTime,
			},
			Data: data,
		})
	}
	writeJSON(w, downloadResponse{Versioned: workspaceagent.NewVersioned(), Artifacts: artifacts})
}

func (s *workspaceAgentServer) handleScrub(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req scrubRequest
	if err := s.decodeJSON(r, &req); err != nil || len(req.Paths) == 0 ||
		!requestProtocolValid(r, req.ProtocolVersion) {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if !requestUsesLegacyAuth(r) {
		if strings.TrimSpace(req.WorkspaceUID) == "" || strings.TrimSpace(req.BindingGeneration) == "" {
			http.Error(w, "workspace binding is required", http.StatusBadRequest)
			return
		}
		if err := s.beginCleanupTransition(req.WorkspaceUID, req.BindingGeneration); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		defer s.endCleanupTransition()
	}
	legacyRequest := requestUsesLegacyAuth(r)
	if legacyRequest {
		req.Paths = appendUniquePath(req.Paths, handoffTokenFilePath())
		for _, requested := range req.Paths {
			if err := secureRemoveAllContext(r.Context(), requested); err != nil {
				http.Error(w, "failed to scrub path", workspaceFileErrorStatus(err))
				return
			}
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	type scrubPlan struct {
		path          string
		preserveFiles bool
	}
	protected := s.workspaceProtectedPaths()
	plans := make([]scrubPlan, 0, len(req.Paths))
	for _, requested := range req.Paths {
		if !v1DataPathAllowed(requested) || s.pathIsDataProtected(requested) {
			http.Error(w, "failed to scrub path", http.StatusBadRequest)
			return
		}
		normalized, err := normalizeAgentPath(requested)
		if err != nil {
			http.Error(w, "failed to scrub path", http.StatusBadRequest)
			return
		}
		plans = append(plans, scrubPlan{
			path:          normalized,
			preserveFiles: s.pathHasProtectedDescendant(normalized, protected),
		})
	}
	for _, plan := range plans {
		var err error
		if plan.preserveFiles {
			err = secureResetDirectoryContext(
				r.Context(), plan.path, s.controlAuthConfigured, s.commandUID, s.commandGID, protected,
			)
		} else {
			err = secureRemoveAllContext(r.Context(), plan.path)
		}
		if err != nil {
			http.Error(w, "failed to scrub path", workspaceFileErrorStatus(err))
			return
		}
	}
	if err := s.removeLegacyHandoffCredential(r.Context()); err != nil {
		http.Error(w, "failed to remove legacy handoff credential", workspaceFileErrorStatus(err))
		return
	}
	writeJSON(w, workspaceagent.ScrubResponse{Versioned: workspaceagent.NewVersioned(), Scrubbed: true})
}

func (s *workspaceAgentServer) handleReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.controlAuthConfigured && !s.writeConfinement {
		http.Error(w, "reset requires command write confinement", http.StatusServiceUnavailable)
		return
	}
	var req workspaceagent.ResetRequest
	if err := s.decodeJSON(r, &req); err != nil ||
		validateProtocolVersion(req.ProtocolVersion, false) != nil ||
		!validOperationID(req.OperationID) || strings.TrimSpace(req.WorkspaceUID) == "" ||
		strings.TrimSpace(req.BindingGeneration) == "" {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	paths := req.Paths
	if len(paths) == 0 {
		paths = append(paths, allowedRoots...)
	}
	if !resetCoversAllRoots(paths) {
		http.Error(w, "reset must cover every task-writable root", http.StatusBadRequest)
		return
	}
	fingerprint := resetRequestFingerprint(paths, req.WorkspaceUID, req.BindingGeneration)
	if generation, found, err := s.lookupResetOperation(req.OperationID, fingerprint); err != nil {
		status := http.StatusConflict
		if errors.Is(err, errResetOperationCapacity) {
			status = http.StatusTooManyRequests
		}
		http.Error(w, err.Error(), status)
		return
	} else if found {
		writeJSON(w, workspaceagent.ResetResponse{
			Versioned: workspaceagent.NewVersioned(), Reset: true, BindingGeneration: generation,
		})
		return
	}
	validatedPaths := make([]string, 0, len(paths))
	for _, requested := range paths {
		if !v1DataPathAllowed(requested) {
			http.Error(w, "invalid reset path", http.StatusBadRequest)
			return
		}
		normalized, err := normalizeAgentPath(requested)
		if err != nil {
			http.Error(w, "invalid reset path", http.StatusBadRequest)
			return
		}
		validatedPaths = append(validatedPaths, normalized)
	}
	if err := s.beginResetTransition(req.WorkspaceUID, req.BindingGeneration); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	defer s.endCleanupTransition()
	if err := s.removeLegacyHandoffCredential(r.Context()); err != nil {
		http.Error(w, "failed to remove legacy handoff credential", workspaceFileErrorStatus(err))
		return
	}
	protected := s.workspaceProtectedPaths()
	for _, requested := range validatedPaths {
		if err := secureResetDirectoryContext(
			r.Context(), requested, s.controlAuthConfigured, s.commandUID, s.commandGID, protected,
		); err != nil {
			http.Error(w, "failed to reset path", workspaceFileErrorStatus(err))
			return
		}
	}
	bindingGeneration, err := s.resetWorkspaceBinding(req.OperationID, fingerprint)
	if err != nil {
		http.Error(w, "failed to rotate workspace binding", http.StatusInternalServerError)
		return
	}
	writeJSON(w, workspaceagent.ResetResponse{
		Versioned:         workspaceagent.NewVersioned(),
		Reset:             true,
		BindingGeneration: bindingGeneration,
	})
}

func (s *workspaceAgentServer) resetWorkspaceBinding(operationID, fingerprint string) (string, error) {
	generation, err := newBindingGeneration()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.executions)
	clear(s.executionCancels)
	clear(s.executionEpochs)
	clear(s.executionFingerprints)
	clear(s.executionTombstones)
	clear(s.operationRunning)
	clear(s.inflightByEpoch)
	clear(s.runningByEpoch)
	s.boundWorkspaceUID = ""
	s.bindingGeneration = generation
	s.resetRequired = false
	s.evictResetOperationsLocked(time.Now().UTC())
	s.resetOperations[operationID] = resetOperationRecord{
		Fingerprint: fingerprint,
		Generation:  generation,
		CompletedAt: time.Now().UTC(),
	}
	s.lastEpoch = 0
	s.expiringEpoch = 0
	s.lastExpiredEpoch = 0
	return generation, nil
}

func (s *workspaceAgentServer) bindingGenerationSnapshot() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bindingGeneration
}

func (s *workspaceAgentServer) lookupResetOperation(
	operationID, fingerprint string,
) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictResetOperationsLocked(time.Now().UTC())
	if record, ok := s.resetOperations[operationID]; ok {
		if record.Fingerprint != fingerprint {
			return "", false, fmt.Errorf("reset operationID conflicts with a prior request")
		}
		return record.Generation, true, nil
	}
	if len(s.resetOperations) >= defaultMaxResetOperations {
		return "", false, errResetOperationCapacity
	}
	return "", false, nil
}

func (s *workspaceAgentServer) evictResetOperationsLocked(now time.Time) {
	for operationID, record := range s.resetOperations {
		if now.Sub(record.CompletedAt) >= resetOperationRetention {
			delete(s.resetOperations, operationID)
		}
	}
}

func resetRequestFingerprint(paths []string, workspaceUID, bindingGeneration string) string {
	canonical := make([]string, 0, len(paths))
	for _, path := range paths {
		normalized, _ := normalizeAgentPath(path)
		canonical = append(canonical, filepath.Clean(normalized))
	}
	slices.Sort(canonical)
	canonical = append(canonical, strings.TrimSpace(workspaceUID), strings.TrimSpace(bindingGeneration))
	return digest([]byte(strings.Join(canonical, "\x00")))
}

func resetCoversAllRoots(paths []string) bool {
	requested := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		normalized, err := normalizeAgentPath(path)
		if err != nil {
			return false
		}
		requested[filepath.Clean(normalized)] = struct{}{}
	}
	for _, root := range allowedRoots {
		if _, ok := requested[filepath.Clean(root)]; !ok {
			return false
		}
	}
	return true
}

func defaultDownloadRoot() string {
	for _, root := range allowedRoots {
		if filepath.Clean(root) == defaultWorkspaceRoot {
			return root
		}
	}
	for _, root := range allowedRoots {
		if filepath.Clean(root) != "/app" {
			return root
		}
	}
	return defaultWorkspaceRoot
}

func v1DataPathAllowed(path string) bool {
	return securePathAllowed(path)
}

func (s *workspaceAgentServer) pathConflictsProtected(path string) bool {
	return s.pathIsDataProtected(path) || s.pathHasProtectedDescendant(path, s.workspaceDataProtectedPaths())
}

func (s *workspaceAgentServer) pathIsDataProtected(path string) bool {
	normalized := normalizedProtectedPath(path)
	for _, protected := range s.workspaceDataProtectedPaths() {
		if normalized == normalizedProtectedPath(protected) {
			return true
		}
	}
	return false
}

func (s *workspaceAgentServer) pathHasProtectedDescendant(path string, protectedPaths []string) bool {
	normalized := normalizedProtectedPath(path)
	for _, protected := range protectedPaths {
		if pathWithinRoot(normalizedProtectedPath(protected), normalized) &&
			normalizedProtectedPath(protected) != normalized {
			return true
		}
	}
	return false
}

func normalizedProtectedPath(path string) string {
	normalized, err := normalizeAgentPath(path)
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(normalized); err == nil {
		normalized = resolved
	}
	return filepath.Clean(normalized)
}

func (s *workspaceAgentServer) workspaceProtectedPaths() []string {
	paths := make([]string, 0, 4)
	appendPath := func(path string) {
		path = strings.TrimSpace(path)
		if path == "" || !filepath.IsAbs(path) {
			return
		}
		path = filepath.Clean(path)
		if !slices.Contains(paths, path) {
			paths = append(paths, path)
		}
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			resolved = filepath.Clean(resolved)
			if !slices.Contains(paths, resolved) {
				paths = append(paths, resolved)
			}
		}
	}
	if executable, err := os.Executable(); err == nil {
		appendPath(executable)
	}
	appendPath(os.Getenv(envControlAuthFile))
	appendPath(s.controlAuthPath)
	appendPath(os.Getenv(envTLSCertFile))
	appendPath(os.Getenv(envTLSKeyFile))
	return paths
}

func (s *workspaceAgentServer) workspaceDataProtectedPaths() []string {
	paths := append([]string(nil), s.workspaceProtectedPaths()...)
	for _, path := range []string{os.Getenv(envHandoffAuthFile), handoffTokenFilePath()} {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			path = resolved
		}
		path = filepath.Clean(path)
		if !slices.Contains(paths, path) {
			paths = append(paths, path)
		}
	}
	return paths
}

func (s *workspaceAgentServer) removeLegacyHandoffCredential(ctx context.Context) error {
	path := handoffTokenFilePath()
	if !securePathAllowed(path) {
		return nil
	}
	for _, protected := range s.workspaceProtectedPaths() {
		if filepath.Clean(path) == filepath.Clean(protected) {
			return nil
		}
	}
	return secureRemoveAllContext(ctx, path)
}

func subtleStringEqual(left, right string) bool {
	leftHash := sha256.Sum256([]byte(strings.TrimSpace(left)))
	rightHash := sha256.Sum256([]byte(strings.TrimSpace(right)))
	return subtle.ConstantTimeCompare(leftHash[:], rightHash[:]) == 1
}

func (s *workspaceAgentServer) beginCleanupTransition(
	workspaceUID, bindingGeneration string,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeAttachment != nil || s.revokingEpoch != 0 || s.cleanupInProgress {
		return fmt.Errorf("attachment must be revoked before workspace cleanup")
	}
	if workspaceUID != s.boundWorkspaceUID || bindingGeneration != s.bindingGeneration {
		return fmt.Errorf("workspace cleanup binding is stale")
	}
	s.cleanupInProgress = true
	return nil
}

func (s *workspaceAgentServer) beginResetTransition(
	workspaceUID, bindingGeneration string,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeAttachment != nil || s.revokingEpoch != 0 || s.cleanupInProgress {
		return fmt.Errorf("attachment must be revoked before workspace cleanup")
	}
	if bindingGeneration != s.bindingGeneration {
		return fmt.Errorf("workspace cleanup binding is stale")
	}
	if s.boundWorkspaceUID != "" {
		if workspaceUID != s.boundWorkspaceUID {
			return fmt.Errorf("workspace cleanup binding is stale")
		}
	} else if !s.resetRequired {
		return fmt.Errorf("workspace cleanup binding is stale")
	}
	s.cleanupInProgress = true
	return nil
}

func (s *workspaceAgentServer) endCleanupTransition() {
	s.mu.Lock()
	s.cleanupInProgress = false
	s.mu.Unlock()
}

func appendUniquePath(paths []string, path string) []string {
	for _, existing := range paths {
		if filepath.Clean(existing) == filepath.Clean(path) {
			return paths
		}
	}
	return append(paths, path)
}

type execRequest = daemonprotocol.ExecRequest
type execResponse = daemonprotocol.ExecResponse
type uploadRequest = daemonprotocol.UploadRequest
type uploadFile = daemonprotocol.UploadFile
type uploadResponse = daemonprotocol.UploadResponse
type downloadRequest = daemonprotocol.DownloadRequest
type downloadResponse = daemonprotocol.DownloadResponse
type scrubRequest = daemonprotocol.ScrubRequest
type artifact = daemonprotocol.Artifact
type downloadedArtifact = daemonprotocol.DownloadedArtifact
type Artifact = daemonprotocol.Artifact

func (s *workspaceAgentServer) decodeJSON(r *http.Request, out any) error {
	limit := s.maxRequestBytes
	if limit <= 0 {
		limit = defaultMaxRequestBytes
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > limit {
		return fmt.Errorf("request exceeds maximum size")
	}
	if err := json.Unmarshal(data, out); err != nil {
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		slog.Error("failed to encode response", "err", err)
	}
}

func safePath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("path is required")
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join("/app", value)
	}
	clean := filepath.Clean(value)
	for _, root := range allowedRoots {
		root = filepath.Clean(root)
		if pathWithinRoot(clean, root) {
			if err := validateResolvedPath(clean, root); err != nil {
				return "", err
			}
			return clean, nil
		}
	}
	return "", fmt.Errorf("path escapes allowed roots")
}

func validateResolvedPath(path, root string) error {
	resolved, err := resolveExistingPrefix(path)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		resolvedRoot = root
	}
	if pathWithinRoot(resolved, root) || pathWithinRoot(resolved, resolvedRoot) {
		return nil
	}
	return fmt.Errorf("path escapes allowed root")
}

func resolveExistingPrefix(path string) (string, error) {
	path = filepath.Clean(path)
	current := path
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			parts := append([]string{resolved}, suffix...)
			return filepath.Clean(filepath.Join(parts...)), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		if info, lstatErr := os.Lstat(current); lstatErr == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return "", fmt.Errorf("dangling symlink %s", current)
			}
			return "", err
		} else if !os.IsNotExist(lstatErr) {
			return "", lstatErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return path, nil
		}
		suffix = append([]string{filepath.Base(current)}, suffix...)
		current = parent
	}
}

func pathWithinRoot(path, root string) bool {
	path = filepath.Clean(path)
	root = filepath.Clean(root)
	return path == root || strings.HasPrefix(path, root+string(os.PathSeparator))
}

func cleanReportedPath(path string) string {
	path = strings.TrimSpace(path)
	if filepath.IsAbs(path) {
		for _, root := range allowedRoots {
			if rel, err := filepath.Rel(root, path); err == nil && !strings.HasPrefix(rel, "..") {
				return rel
			}
		}
	}
	return strings.TrimPrefix(filepath.Clean(path), "/")
}

func mergeEnv(base []string, overrides map[string]string) []string {
	out := make([]string, 0, len(base)+len(overrides))
	seen := make(map[string]struct{}, len(base)+len(overrides))
	for _, item := range base {
		name, _, ok := strings.Cut(item, "=")
		if !ok || name == "" {
			continue
		}
		if _, exists := overrides[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, item)
	}
	for name, value := range overrides {
		if name == "" {
			continue
		}
		if _, exists := seen[name]; exists {
			continue
		}
		out = append(out, name+"="+value)
	}
	return out
}

func commandBaseEnv(environ []string) []string {
	filtered := make([]string, 0, len(environ))
	for _, item := range environ {
		name, _, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		switch name {
		case envHandoffAuth, envBootstrapAuth, envControlAuthFile, envTLSCertFile, envTLSKeyFile:
			continue
		default:
			filtered = append(filtered, item)
		}
	}
	return filtered
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

type boundedBuffer struct {
	buf       bytes.Buffer
	limit     int64
	written   int64
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.limit <= 0 {
		b.limit = defaultMaxOutputBytes
	}
	remaining := b.limit - b.written
	if remaining > 0 {
		chunk := p
		if int64(len(chunk)) > remaining {
			chunk = chunk[:remaining]
			b.truncated = true
		}
		_, _ = b.buf.Write(chunk)
		b.written += int64(len(chunk))
	} else {
		b.truncated = true
	}
	if int64(len(p)) > remaining {
		b.truncated = true
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	return b.buf.String()
}

func durationEnvSeconds(name string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil || seconds <= 0 {
		return fallback
	}
	return time.Duration(seconds) * time.Second
}

func uint32Env(name string, fallback uint32) (uint32, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		if fallback == 0 {
			return 0, fmt.Errorf("%s fallback must be non-zero", name)
		}
		return fallback, nil
	}
	parsed, err := strconv.ParseUint(value, 10, 32)
	if err != nil || parsed == 0 {
		return 0, fmt.Errorf("%s must be a non-zero 32-bit unsigned integer", name)
	}
	return uint32(parsed), nil
}

func int64Env(name string, fallback int64) int64 {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}
