package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/orka-agents/sessionkit"

	"github.com/orka-agents/orka/internal/acp"
	"github.com/orka-agents/orka/internal/codexstate"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

type nativeSessionCapture struct {
	request     harnessv2.CaptureNativeSessionRequest
	done        chan struct{}
	finished    bool
	descriptor  harnessv2.RuntimeSessionDescriptor
	snapshot    harnessv2.NativeSessionSnapshot
	failure     string
	failureCode harnessv2.ErrorCode
	retryable   bool
}

type nativeCaptureUnsupportedError struct{}

func (*nativeCaptureUnsupportedError) Error() string {
	return "native Codex context is unsupported or exceeds the transport budget; stopped private home retained"
}

// A retry is safe only after the writer and all descendants are proven gone.
type nativeCaptureRetryableError struct{ message string }

func (e *nativeCaptureRetryableError) Error() string { return e.message }

// Receipts remain immutable until runtime deletion, even after a safe retry.
func nativeCaptureReceiptLocked(state *sessionState, operationID harnessv2.OperationID) *nativeSessionCapture {
	if receipt := state.nativeCaptureReceipts[operationID]; receipt != nil {
		return receipt
	}
	if state.nativeCapture != nil && state.nativeCapture.request.Metadata.OperationID == operationID {
		return state.nativeCapture
	}
	return nil
}

func (s *Server) handleCaptureNativeSession(w http.ResponseWriter, r *http.Request) {
	var request harnessv2.CaptureNativeSessionRequest
	if !s.decodeAuthenticatedJSON(w, r, &request) {
		return
	}
	now := time.Now().UTC()
	if err := request.ValidateAt(now); err != nil {
		writeError(w, http.StatusBadRequest, harnessv2.ErrorCodeInvalidRequest, err.Error(), nil, false)
		return
	}
	if !s.authorizeMutation(w, r, request.Metadata, true) {
		return
	}
	r, span := s.traceOperation(r, request.Metadata, "session.capture-native")
	defer span.End()
	sessionID := harnessv2.RuntimeSessionID(r.PathValue("sessionID"))
	s.mu.Lock()
	state := s.sessions[sessionID]
	if state == nil {
		s.mu.Unlock()
		writeError(w, http.StatusNotFound, harnessv2.ErrorCodeInvalidRequest, "runtime session not found", nil, false)
		return
	}
	if mismatch := s.sessionFenceMismatch(state, request.Metadata); mismatch != harnessv2.FenceMatch {
		s.mu.Unlock()
		writeClassificationError(w, harnessv2.Classification{Class: harnessv2.RequestClassificationStaleFence, FenceMismatch: mismatch})
		return
	}
	classification, err := harnessv2.ClassifyOperation(s.expectedFence(state.descriptor.RuntimeSessionUID, state.descriptor.Generation), request.Metadata, sessionOperationPtrLocked(state, request.Metadata.OperationID, now), true, now)
	if err != nil || (classification.Class != harnessv2.RequestClassificationFresh && classification.Class != harnessv2.RequestClassificationDuplicate) {
		s.mu.Unlock()
		if err != nil {
			writeError(w, http.StatusBadRequest, harnessv2.ErrorCodeInvalidRequest, err.Error(), nil, false)
		} else {
			writeClassificationError(w, classification)
		}
		return
	}
	// Capture belongs to the exact last terminal Task. A runtime fence alone
	// must not authorize exporting another Task's private conversation state.
	if !matchesTerminalNativeTask(state, request.Metadata) {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, harnessv2.ErrorCodeSessionPoisoned, "native capture requires the current terminal Codex Task", nil, false)
		return
	}
	if request.OriginalOperationID != "" {
		capture := nativeCaptureReceiptLocked(state, request.OriginalOperationID)
		if capture == nil {
			// The fence already proved this is the same supervisor boot. Captures are
			// recorded under the lock before they start, so no record means the
			// original never arrived and the writer is untouched. Reconciliation
			// still never starts a capture itself.
			s.mu.Unlock()
			writeError(w, http.StatusConflict, harnessv2.ErrorCodeNativeCaptureNotStarted, "original native capture was never started by this runtime", nil, false)
			return
		}
		if !capture.finished {
			s.mu.Unlock()
			writeError(w, http.StatusConflict, harnessv2.ErrorCodeAlreadyAccepted, "original native capture has not completed", nil, true)
			return
		}
		if capture.request.Metadata.OperationID != request.OriginalOperationID || capture.request.Metadata.RequestDigest != request.OriginalRequestDigest {
			s.mu.Unlock()
			writeError(w, http.StatusConflict, harnessv2.ErrorCodeDigestConflict, "native capture reconciliation does not match the original operation", nil, false)
			return
		}
		if classification.Class == harnessv2.RequestClassificationFresh {
			if err := ensureSessionOperationCapacityLocked(state, sessionDeletionOperationReserve); err != nil {
				s.mu.Unlock()
				writeError(w, http.StatusConflict, harnessv2.ErrorCodeSessionPoisoned, err.Error(), nil, false)
				return
			}
			recordSessionOperationLocked(state, request.Metadata, harnessv2.OperationPhaseApplied, "", now)
		}
		s.mu.Unlock()
		s.writeNativeCapture(w, capture, classification)
		return
	}
	retryReady := state.nativeCapture != nil && state.nativeCapture.finished && state.nativeCapture.retryable &&
		classification.Class == harnessv2.RequestClassificationFresh && !state.drainCleanupScheduled &&
		state.descriptor.State == harnessv2.RuntimeSessionStatePoisoned
	if capture := nativeCaptureReceiptLocked(state, request.Metadata.OperationID); capture != nil {
		if capture.request.Metadata.OperationID != request.Metadata.OperationID ||
			capture.request.Metadata.RequestDigest != request.Metadata.RequestDigest {
			s.mu.Unlock()
			writeError(w, http.StatusConflict, harnessv2.ErrorCodeDigestConflict, "native session capture already belongs to another operation", nil, false)
			return
		}
		done := capture.done
		s.mu.Unlock()
		select {
		case <-done:
			s.writeNativeCapture(w, capture, harnessv2.Classification{Class: harnessv2.RequestClassificationDuplicate, Phase: harnessv2.OperationPhaseApplied})
		case <-r.Context().Done():
			writeError(w, http.StatusConflict, harnessv2.ErrorCodeAlreadyAccepted, "native capture is in progress", nil, true)
		}
		return
	}
	if state.nativeCapture != nil && !retryReady {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, harnessv2.ErrorCodeDigestConflict, "native session capture already belongs to another operation", nil, false)
		return
	}
	if !retryReady && !canStartNativeCapture(state, classification) {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, harnessv2.ErrorCodeSessionPoisoned, "runtime session cannot enter native capture", nil, false)
		return
	}
	if err := ensureSessionOperationCapacityLocked(state, sessionDeletionOperationReserve); err != nil {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, harnessv2.ErrorCodeSessionPoisoned, err.Error(), nil, false)
		return
	}
	if len(state.nativeCaptureReceipts) >= harnessv2.MaxRuntimeSessionTombstoneOperations-sessionDeletionOperationReserve {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, harnessv2.ErrorCodeSessionPoisoned, "native capture receipt journal is full; private evidence retained", nil, false)
		return
	}
	capture := &nativeSessionCapture{request: request, done: make(chan struct{})}
	state.nativeCapture = capture
	if state.nativeCaptureReceipts == nil {
		state.nativeCaptureReceipts = make(map[harnessv2.OperationID]*nativeSessionCapture)
	}
	state.nativeCaptureReceipts[request.Metadata.OperationID] = capture
	state.descriptor.State = harnessv2.RuntimeSessionStatePoisoned
	state.descriptor.LastTransitionAt = now
	recordSessionOperationLocked(state, request.Metadata, harnessv2.OperationPhaseRecorded, "", now)
	s.mu.Unlock()

	// Once recorded, a disconnected controller must not leave a live child or
	// ambiguous half-capture. The result remains private until explicit delete.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), defaultDuration(s.cfg.CancelGrace, acp.DefaultStopGrace)*2+15*time.Second)
	defer cancel()
	snapshot, captureErr := s.captureNativeSession(ctx, state)
	s.mu.Lock()
	capture.snapshot = snapshot
	capture.descriptor = state.descriptor
	if captureErr != nil {
		capture.failure = captureErr.Error()
		capture.failureCode = harnessv2.ErrorCodeSessionPoisoned
		if _, ok := errors.AsType[*nativeCaptureRetryableError](captureErr); ok {
			capture.failureCode = harnessv2.ErrorCodeNativeCaptureRetryReady
			capture.retryable = true
		}
		if _, ok := errors.AsType[*nativeCaptureUnsupportedError](captureErr); ok {
			capture.failureCode = harnessv2.ErrorCodeNativeCaptureUnsupported
		}
	}
	capture.finished = true
	recordSessionOperationLocked(state, request.Metadata, harnessv2.OperationPhaseApplied, "", time.Now().UTC())
	close(capture.done)
	s.mu.Unlock()
	s.writeNativeCapture(w, capture, harnessv2.Classification{Class: harnessv2.RequestClassificationFresh})
}

func canStartNativeCapture(state *sessionState, classification harnessv2.Classification) bool {
	return classification.Class == harnessv2.RequestClassificationFresh && !state.drainCleanupScheduled &&
		(state.descriptor.State == harnessv2.RuntimeSessionStateIdle ||
			state.descriptor.State == harnessv2.RuntimeSessionStateFinalizing && state.publicationFinalization != nil)
}

func matchesTerminalNativeTask(state *sessionState, metadata harnessv2.MutationMetadata) bool {
	return state.supportsNativeSessions && !state.creating && state.runtime != nil && state.profile.ProviderKind == providerKindCodex &&
		state.prompt != nil && state.prompt.settlement != nil &&
		state.prompt.settlement.TerminalEvent == harnessv2.EventCompleted &&
		state.prompt.settlement.Outcome == harnessv2.PromptOutcomeSucceeded &&
		metadata.TaskUID == state.prompt.request.Metadata.TaskUID &&
		metadata.TaskAttempt == state.prompt.request.Metadata.TaskAttempt
}

// A restored conversation and a successful native turn belong to the controller
// until it saves the capture or explicitly deletes the runtime. Drain and
// shutdown may stop the writer, but cannot discard failed continuation evidence.
func retainsNativeCaptureEvidence(state *sessionState) bool {
	if state.descriptor.NativeRestoration != nil || state.nativeCapture != nil || state.nativeInstallUnresolved != nil {
		return true
	}
	if !state.supportsNativeSessions || state.profile.ProviderKind != providerKindCodex || state.prompt == nil || state.prompt.settlement == nil ||
		state.prompt.settlement.TerminalEvent != harnessv2.EventCompleted || state.prompt.settlement.Outcome != harnessv2.PromptOutcomeSucceeded {
		return false
	}
	switch state.descriptor.State {
	case harnessv2.RuntimeSessionStateIdle, harnessv2.RuntimeSessionStatePublicationPrepared:
		return true
	case harnessv2.RuntimeSessionStateFinalizing:
		return state.publicationFinalization != nil
	default:
		return false
	}
}

const nativeInstallUnknownMessage = "native installation outcome is unknown; original plan and private destination retained"
const nativeCreateCleanupUnprovenMessage = "native runtime creation cleanup could not be proven; private home and original journal retained"

type nativeCreateCleanupUnprovenError struct{ Err error }

func (e *nativeCreateCleanupUnprovenError) Error() string { return e.Err.Error() }
func (e *nativeCreateCleanupUnprovenError) Unwrap() error { return e.Err }

// Only an unstarted or provably stopped writer permits this cleanup. A journal
// that predates this create may bind another private destination and is retained.
func cleanupFailedNativeCreate(paths acp.SessionPaths, journalDir string, ownsJournal bool) error {
	if err := acp.ReclaimSessionOwnership(paths.Root); err != nil {
		return err
	}
	if err := os.RemoveAll(paths.Root); err != nil {
		return err
	}
	if ownsJournal {
		return os.RemoveAll(journalDir)
	}
	return nil
}

func isNativeInstallUnknown(err error) bool {
	_, unknown := errors.AsType[*sessionkit.UnknownOutcomeError](err)
	return unknown
}

func (s *Server) retainUnresolvedNativeInstall(state *sessionState, request harnessv2.CreateRuntimeSessionRequest, paths acp.SessionPaths, runtime *acp.RuntimeSession, message string, now time.Time) {
	s.mu.Lock()
	state.paths = paths
	state.runtime = runtime
	state.nativeInstallJournal = filepath.Join(s.cfg.SessionBaseDir, ".native-install", sessionPathID(state.descriptor.RuntimeSessionUID, state.descriptor.Generation))
	state.profile = request.Profile
	state.creating = false
	state.descriptor.State = harnessv2.RuntimeSessionStatePoisoned
	state.descriptor.LastTransitionAt = now
	state.nativeInstallUnresolved = &failedCreateReplay{
		operationID: request.Metadata.OperationID, requestDigest: request.Metadata.RequestDigest,
		statusCode: http.StatusConflict, code: harnessv2.ErrorCodeCleanupUnproven, message: message,
	}
	cleanup := s.poisonPoolLocked("native_install_outcome_unknown")
	s.mu.Unlock()
	s.startDrainCleanup(cleanup)
}

func (s *Server) removeSessionPrivateFiles(state *sessionState) error {
	if err := os.RemoveAll(state.paths.Root); err != nil {
		return err
	}
	if state.descriptor.NativeRestoration != nil {
		// The journal is allocated by this supervisor from the validated fence,
		// never from imported metadata or an agent-controlled path.
		journal := filepath.Join(s.cfg.SessionBaseDir, ".native-install", sessionPathID(state.descriptor.RuntimeSessionUID, state.descriptor.Generation))
		return os.RemoveAll(journal)
	}
	if state.nativeInstallJournal != "" {
		return os.RemoveAll(state.nativeInstallJournal)
	}
	return nil
}

func (s *Server) captureNativeSession(ctx context.Context, state *sessionState) (harnessv2.NativeSessionSnapshot, error) {
	if state.mcpProxy != nil {
		state.mcpProxy.close()
	}
	var proxyErr error
	if state.providerProxy != nil {
		state.providerProxy.close()
		proxyErr = state.providerProxy.wait(ctx)
	}
	cleanup, stopErr := state.runtime.Delete(ctx)
	if proxyErr != nil || stopErr != nil || !cleanup.Proven {
		s.poisonPool("native_capture_descendant_cleanup_unproven")
		return harnessv2.NativeSessionSnapshot{}, fmt.Errorf("native capture could not prove descendant exit")
	}
	if err := reclaimStoppedSessionOwnership(state.paths); err != nil {
		return harnessv2.NativeSessionSnapshot{}, &nativeCaptureRetryableError{message: "native capture could not reclaim stopped session ownership"}
	}
	data, err := codexstate.Capture(ctx, filepath.Join(state.paths.Home, ".codex"), state.descriptor.ProviderSessionID)
	if err != nil {
		if errors.Is(err, codexstate.ErrUnsupported) {
			return harnessv2.NativeSessionSnapshot{}, &nativeCaptureUnsupportedError{}
		}
		return harnessv2.NativeSessionSnapshot{}, &nativeCaptureRetryableError{message: "native Codex session capture failed; private home retained"}
	}
	summary, err := codexstate.Inspect(ctx, data)
	if err != nil || summary.ThreadID != state.descriptor.ProviderSessionID {
		if errors.Is(err, codexstate.ErrUnsupported) {
			return harnessv2.NativeSessionSnapshot{}, &nativeCaptureUnsupportedError{}
		}
		return harnessv2.NativeSessionSnapshot{}, &nativeCaptureRetryableError{message: "native captured bundle validation failed; private home retained"}
	}
	snapshot := harnessv2.NativeSessionSnapshot{
		Data: data, DataDigest: summary.DataDigest, ProviderSessionID: summary.ThreadID,
		ProviderKind: providerKindCodex, ProviderVersion: acp.CodexCLIVersion,
		RuntimeSessionUID: state.descriptor.RuntimeSessionUID, RuntimeProfileDigest: state.descriptor.RuntimeProfileDigest,
		WorkingDirectory: state.paths.Workspace,
	}
	if err := snapshot.Validate(); err != nil {
		return harnessv2.NativeSessionSnapshot{}, fmt.Errorf("native captured snapshot exceeds transport limits or is invalid; private home retained")
	}
	// Persist the exact snapshot before returning it. The stopped, reclaimed
	// tree is supervisor-owned, so the ACP child cannot read or change this file.
	body, err := json.Marshal(snapshot)
	if err != nil {
		return harnessv2.NativeSessionSnapshot{}, fmt.Errorf("native captured snapshot encoding failed; private home retained")
	}
	if err := persistNativeSnapshot(state.paths.Root, body); err != nil {
		return harnessv2.NativeSessionSnapshot{}, &nativeCaptureRetryableError{message: "native captured snapshot persistence failed; private home retained"}
	}
	return snapshot, nil
}

func persistNativeSnapshot(root string, body []byte) error {
	file, err := os.CreateTemp(root, ".native-snapshot-")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), filepath.Join(root, ".native-session-snapshot.json")); err != nil {
		return err
	}
	dir, err := os.Open(root)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

func (s *Server) writeNativeCapture(w http.ResponseWriter, capture *nativeSessionCapture, classification harnessv2.Classification) {
	s.mu.Lock()
	failure, code, snapshot, descriptor, retryable := capture.failure, capture.failureCode, capture.snapshot, capture.descriptor, capture.retryable
	s.mu.Unlock()
	if failure != "" {
		status := http.StatusInternalServerError
		if code == harnessv2.ErrorCodeNativeCaptureUnsupported {
			status = http.StatusUnprocessableEntity
		}
		writeError(w, status, code, failure, nil, retryable)
		return
	}
	writeJSON(w, http.StatusOK, harnessv2.CaptureNativeSessionResponse{
		Protocol: harnessv2.ProtocolVersion, Classification: classification, Session: descriptor, Snapshot: snapshot,
	})
}
