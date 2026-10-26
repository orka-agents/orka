package supervisor

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

type preparedSession struct {
	config                acp.RuntimeSessionConfig
	preparation           harnessv2.MutationMetadata
	resumedFromCheckpoint bool
	durableComponent      string
	durableBinding        acp.DurableWorkspaceBinding
	nativeRestoration     *harnessv2.NativeSessionRestoration
}

// launchPreparedSession is the only adapter-start boundary for either create
// mode. A non-nil runtime on failure retains an unproven child stop.
func (s *Server) launchPreparedSession(ctx context.Context, prepared *preparedSession) (*acp.RuntimeSession, error) {
	if prepared.resumedFromCheckpoint {
		// Preparation must leave the untouched checkpoint committed. Only
		// actual adapter start makes its contents potentially uncommitted.
		if err := acp.MarkDurableSessionWorkspaceResumePending(s.cfg.DurableWorkspaceDir, prepared.durableComponent); err != nil {
			return nil, sessionCreationFailed("durable workspace pending mark", err)
		}
	}
	runtime, err := acp.NewRuntimeSession(ctx, prepared.config)
	if err != nil {
		if initialization, failed := errors.AsType[*acp.InitializationError](err); failed && !initialization.Cleanup.Proven {
			return initialization.RuntimeSession(), sessionCreationFailed("provider adapter initialization", &createCleanupUnprovenError{Err: err})
		}
		return nil, sessionCreationFailed("provider adapter initialization", err)
	}
	if s.cfg.DurableWorkspaceDir != "" {
		binding := prepared.durableBinding
		binding.SessionIdentityHighWater = s.cfg.UIDAllocator.Capacity() - s.cfg.UIDAllocator.Remaining()
		if err := acp.CommitDurableSessionWorkspace(s.cfg.DurableWorkspaceDir, prepared.durableComponent, binding); err != nil {
			cleanup, deleteErr := runtime.Delete(ctx)
			if deleteErr != nil || !cleanup.Proven {
				return runtime, sessionCreationFailed("durable workspace commit", &createCleanupUnprovenError{Err: errors.Join(err, deleteErr)})
			}
			return nil, sessionCreationFailed("durable workspace commit", err)
		}
	}
	return runtime, nil
}

func (s *Server) handleLaunchSession(w http.ResponseWriter, r *http.Request) {
	var request harnessv2.LaunchRuntimeSessionRequest
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
	r, span := s.traceOperation(r, request.Metadata, "session.launch")
	defer span.End()
	sessionID := harnessv2.RuntimeSessionID(r.PathValue("sessionID"))
	s.mu.Lock()
	state := s.sessions[sessionID]
	if state == nil {
		s.mu.Unlock()
		writeError(w, http.StatusNotFound, harnessv2.ErrorCodeInvalidRequest, "prepared runtime session not found", nil, false)
		return
	}
	if mismatch := s.sessionFenceMismatch(state, request.Metadata); mismatch != harnessv2.FenceMatch {
		s.mu.Unlock()
		writeClassificationError(w, harnessv2.Classification{Class: harnessv2.RequestClassificationStaleFence, FenceMismatch: mismatch})
		return
	}
	prepared := state.prepared
	if prepared == nil || request.PreparationOperationID != prepared.preparation.OperationID ||
		request.Metadata.TaskUID != prepared.preparation.TaskUID ||
		request.Metadata.TaskAttempt != prepared.preparation.TaskAttempt {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, harnessv2.ErrorCodeInvalidRequest, "launch does not match the original session preparation", nil, false)
		return
	}
	classification, err := harnessv2.ClassifyOperation(
		s.expectedFence(state.descriptor.RuntimeSessionUID, state.descriptor.Generation),
		request.Metadata, sessionOperationPtrLocked(state, request.Metadata.OperationID, now), true, now,
	)
	if err != nil {
		s.mu.Unlock()
		writeError(w, http.StatusBadRequest, harnessv2.ErrorCodeInvalidRequest, err.Error(), nil, false)
		return
	}
	if classification.Class == harnessv2.RequestClassificationDuplicate && !state.creating && state.launchDescriptor != nil {
		response := harnessv2.LaunchRuntimeSessionResponse{
			Protocol: harnessv2.ProtocolVersion, Classification: classification,
			PreparationOperationID: request.PreparationOperationID, Session: *state.launchDescriptor,
		}
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, response)
		return
	}
	if classification.Class != harnessv2.RequestClassificationFresh {
		s.mu.Unlock()
		writeClassificationError(w, classification)
		return
	}
	if state.creating || state.descriptor.State != harnessv2.RuntimeSessionStatePrepared {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, harnessv2.ErrorCodeAlreadyAccepted, "runtime session is not awaiting launch", nil, false)
		return
	}
	if s.drain.Requested || s.lifecycle != harnessv2.SupervisorLifecycleReady {
		s.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, harnessv2.ErrorCodeRateLimited, "runtime pool is not accepting session launch", nil, true)
		return
	}
	if err := ensureSessionOperationCapacityLocked(state, sessionDeletionOperationReserve); err != nil {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, harnessv2.ErrorCodeSessionPoisoned, err.Error(), nil, false)
		return
	}
	state.creating = true
	recordSessionOperationLocked(state, request.Metadata, harnessv2.OperationPhaseRecorded, "", now)
	s.mu.Unlock()

	runtime, launchErr := s.launchPreparedSession(r.Context(), prepared)
	finishedAt := time.Now().UTC()
	s.mu.Lock()
	state.runtime = runtime
	state.creating = false
	state.descriptor.LastTransitionAt = finishedAt
	if launchErr != nil {
		state.descriptor.State = harnessv2.RuntimeSessionStatePoisoned
		var cleanup []drainCleanupCandidate
		if runtime != nil {
			state.createCleanupUnproven = &failedCreateReplay{
				operationID: request.Metadata.OperationID, requestDigest: request.Metadata.RequestDigest,
				statusCode: http.StatusConflict, code: harnessv2.ErrorCodeCleanupUnproven, message: createCleanupUnprovenMessage,
			}
			cleanup = s.poisonPoolLocked("runtime_launch_cleanup_unproven")
		} else if (s.drain.Requested || s.lifecycle != harnessv2.SupervisorLifecycleReady) && !state.drainCleanupScheduled {
			state.drainCleanupScheduled = true
			cleanup = append(cleanup, drainCleanupCandidate{id: sessionID, state: state})
		}
		s.mu.Unlock()
		s.startDrainCleanup(cleanup)
		slog.Error("ACP runtime session launch failed", "stage", sessionCreationStage(launchErr))
		code := harnessv2.ErrorCodeSessionPoisoned
		if runtime != nil {
			code = harnessv2.ErrorCodeCleanupUnproven
		}
		writeError(w, http.StatusConflict, code, safeError(launchErr), nil, false)
		return
	}
	state.promptMutations = runtime
	state.descriptor.State = harnessv2.RuntimeSessionStateIdle
	state.descriptor.ProviderSessionID = runtime.ProviderSessionID()
	state.descriptor.NativeRestoration = prepared.nativeRestoration
	descriptor := state.descriptor
	state.launchDescriptor = &descriptor
	recordSessionOperationLocked(state, request.Metadata, harnessv2.OperationPhaseApplied, "", finishedAt)
	cleanup := (s.drain.Requested || s.lifecycle != harnessv2.SupervisorLifecycleReady) && !state.drainCleanupScheduled && isDrainCleanupState(state)
	if cleanup {
		state.drainCleanupScheduled = true
	}
	s.mu.Unlock()
	if cleanup {
		go s.cleanupDrainedSession(sessionID, state)
	}
	writeJSON(w, http.StatusOK, harnessv2.LaunchRuntimeSessionResponse{
		Protocol: harnessv2.ProtocolVersion, Classification: classification,
		PreparationOperationID: request.PreparationOperationID, Session: descriptor,
	})
}

// Callers reserve deletion under s.mu before invoking this helper. A settled
// preparation has no adapter; an in-flight or unproven launch is never that case.
func deleteSessionRuntime(ctx context.Context, state *sessionState) (acp.CleanupStatus, error) {
	if state.runtime != nil {
		return state.runtime.Delete(ctx)
	}
	if state.prepared != nil && !state.creating {
		return acp.CleanupStatus{Proven: true}, nil
	}
	return acp.CleanupStatus{}, errors.New("runtime stop cannot be proven without a runtime or settled preparation")
}
