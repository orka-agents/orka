package supervisor

import (
	"net/http"
	"time"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/workspacedelta"
)

// preparedDeltaChanges keeps the exact change set of one prepared write delta
// so an ADR 0059 rebase can fold it into the trusted baseline without reading
// the live tree. It holds no file content.
type preparedDeltaChanges struct {
	manifestDigest string
	changes        []workspacedelta.Change
	deletions      []workspacedelta.Deletion
}

func newPreparedDeltaChanges(result workspacedelta.Result) preparedDeltaChanges {
	return preparedDeltaChanges{
		manifestDigest: result.ManifestDigest,
		changes:        append([]workspacedelta.Change(nil), result.Changes...),
		deletions:      append([]workspacedelta.Deletion(nil), result.Deletions...),
	}
}

// sessionQuiescentLocked reports an idle session that may cross a turn
// boundary in the same process (ADR 0059): no prompt is running or unsettled,
// no permission is pending, no operator operation is registered, the
// workspace is not frozen, and the provider process is alive. The caller must
// hold s.mu.
func sessionQuiescentLocked(state *sessionState, liveProcesses uint32) bool {
	if state == nil || state.creating || state.runtime == nil || state.descriptor.State != harnessv2.RuntimeSessionStateIdle {
		return false
	}
	if state.prompt != nil && state.prompt.settlement == nil {
		return false
	}
	if len(state.permissions) != 0 || len(state.operators) != 0 || state.runtime.Frozen() {
		return false
	}
	return liveProcesses > 0
}

func (s *Server) handleRebaseSession(w http.ResponseWriter, r *http.Request) {
	var request harnessv2.RebaseRuntimeSessionRequest
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
	r, span := s.traceOperation(r, request.Metadata, "session.rebase")
	defer span.End()
	sessionID := harnessv2.RuntimeSessionID(r.PathValue("sessionID"))
	s.mu.Lock()
	state := s.sessions[sessionID]
	if state == nil || state.creating {
		s.mu.Unlock()
		writeError(w, http.StatusNotFound, harnessv2.ErrorCodeInvalidRequest, "runtime session not found", nil, false)
		return
	}
	if mismatch := s.sessionFenceMismatch(state, request.Metadata); mismatch != harnessv2.FenceMatch {
		s.mu.Unlock()
		writeClassificationError(w, harnessv2.Classification{Class: harnessv2.RequestClassificationStaleFence, FenceMismatch: mismatch})
		return
	}
	classification, err := harnessv2.ClassifyOperation(
		s.expectedFence(state.descriptor.RuntimeSessionUID, state.descriptor.Generation), request.Metadata,
		sessionOperationPtrLocked(state, request.Metadata.OperationID, now), true, now,
	)
	if err != nil {
		s.mu.Unlock()
		writeError(w, http.StatusBadRequest, harnessv2.ErrorCodeInvalidRequest, err.Error(), nil, false)
		return
	}
	if classification.Class != harnessv2.RequestClassificationFresh {
		descriptor, receipt := state.descriptor, state.rebase
		s.mu.Unlock()
		if classification.Class == harnessv2.RequestClassificationDuplicate && receipt != nil &&
			rebaseReceiptMatches(*receipt, request) && descriptor.WorkspaceBaseline == receipt.NextBaseline {
			writeJSON(w, http.StatusOK, harnessv2.RebaseRuntimeSessionResponse{
				Protocol: harnessv2.ProtocolVersion, Classification: classification, Session: descriptor, Rebase: *receipt,
			})
		} else {
			writeClassificationError(w, classification)
		}
		return
	}
	prepared, failure := rebasePreconditionFailureLocked(state, request)
	if failure != nil {
		s.mu.Unlock()
		writeError(w, failure.status, failure.code, failure.message, nil, failure.retryable)
		return
	}
	recordSessionOperationLocked(state, request.Metadata, harnessv2.OperationPhaseRecorded, "", now)
	baseline, runtimeSession := state.baseline, state.runtime
	previousTreeDigest := state.descriptor.WorkspaceBaseline.TreeDigest
	receipt := *state.publicationFinalization
	s.mu.Unlock()

	// The derived baseline never reads the live tree (ADR 0027): it is the
	// trusted snapshot with the exactly published change set applied.
	rebased, err := baseline.Rebase(r.Context(), prepared.changes, prepared.deletions)
	if err != nil {
		// The session stays finalizing and deletable: the controller retires it.
		writeError(w, http.StatusConflict, harnessv2.ErrorCodeDigestConflict, "published workspace delta does not apply to the trusted baseline", nil, false)
		return
	}
	s.mu.Lock()
	if s.sessions[sessionID] != state || state.descriptor.State != harnessv2.RuntimeSessionStateFinalizing ||
		state.publicationFinalization == nil || *state.publicationFinalization != receipt || state.baseline != baseline {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, harnessv2.ErrorCodeSessionPoisoned, "runtime session changed during rebase", nil, false)
		return
	}
	s.mu.Unlock()
	thaw := s.thawRuntime
	if thaw == nil {
		thaw = func(runtime *acp.RuntimeSession) error { return runtime.Thaw() }
	}
	if err := thaw(runtimeSession); err != nil {
		s.poisonSession(state, "workspace thaw failed during rebase")
		writeError(w, http.StatusInternalServerError, harnessv2.ErrorCodeSessionPoisoned, "workspace thaw failed", nil, false)
		return
	}

	appliedAt := time.Now().UTC()
	rebaseReceipt := harnessv2.RuntimeSessionRebaseReceipt{
		WorkspaceDeltaID: request.WorkspaceDeltaID, PublicationID: request.PublicationID,
		TerminalReceiptDigest: request.TerminalReceiptDigest, PublishedManifestDigest: request.PublishedManifestDigest,
		PreviousTreeDigest: previousTreeDigest, NextBaseline: request.NextBaseline, AppliedAt: appliedAt,
	}
	s.mu.Lock()
	if s.sessions[sessionID] != state || state.descriptor.State != harnessv2.RuntimeSessionStateFinalizing {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, harnessv2.ErrorCodeSessionPoisoned, "runtime session changed during rebase", nil, false)
		return
	}
	state.baseline = rebased
	state.descriptor.WorkspaceBaseline = request.NextBaseline
	state.publicationFinalization = nil
	state.rebase = &rebaseReceipt
	delete(state.preparedDeltas, request.WorkspaceDeltaID)
	setSessionState(state, harnessv2.RuntimeSessionStateIdle)
	state.descriptor.LastTransitionAt = appliedAt
	recordSessionOperationLocked(state, request.Metadata, harnessv2.OperationPhaseApplied, "", appliedAt)
	descriptor := state.descriptor
	mcpProxy := state.mcpProxy
	drainCleanup := s.drain.Requested && !state.drainCleanupScheduled && isDrainCleanupState(state)
	if drainCleanup {
		state.drainCleanupScheduled = true
	}
	s.mu.Unlock()
	if mcpProxy != nil {
		mcpProxy.revoke(harnessv2.RuntimeSessionStateIdle)
	}
	writeJSON(w, http.StatusOK, harnessv2.RebaseRuntimeSessionResponse{
		Protocol: harnessv2.ProtocolVersion, Classification: harnessv2.Classification{Class: harnessv2.RequestClassificationFresh},
		Session: descriptor, Rebase: rebaseReceipt,
	})
	if drainCleanup {
		go s.cleanupDrainedSession(sessionID, state)
	}
}

// rebasePreconditionFailureLocked admits a rebase only for a write session
// that is finalizing with the exact verified-exact publication receipt of the
// prompt that produced the published delta, with nothing else running. The
// caller must hold s.mu.
func rebasePreconditionFailureLocked(state *sessionState, request harnessv2.RebaseRuntimeSessionRequest) (preparedDeltaChanges, *operationFailure) {
	notReady := func(message string) (preparedDeltaChanges, *operationFailure) {
		return preparedDeltaChanges{}, &operationFailure{status: http.StatusConflict, code: harnessv2.ErrorCodeSessionPoisoned, message: message}
	}
	conflict := func(message string) (preparedDeltaChanges, *operationFailure) {
		return preparedDeltaChanges{}, &operationFailure{status: http.StatusConflict, code: harnessv2.ErrorCodeDigestConflict, message: message}
	}
	if state.workspaceIntent != harnessv2.WorkspaceIntentWrite || state.descriptor.State != harnessv2.RuntimeSessionStateFinalizing ||
		state.publicationFinalization == nil || state.baseline == nil || state.runtime == nil {
		return notReady("runtime session is not finalizing a write publication")
	}
	if state.prompt == nil || state.prompt.settlement == nil || !promptMetadataMatches(request.Metadata, state.prompt.request.Metadata) {
		return notReady("rebase must come from the prompt that produced the published delta")
	}
	if len(state.permissions) != 0 || len(state.operators) != 0 {
		return notReady("runtime session has a pending permission or operator operation")
	}
	receipt := *state.publicationFinalization
	if receipt.TerminalState != harnessv2.PublicationTerminalVerifiedExact || receipt.WorkspaceDeltaID != request.WorkspaceDeltaID ||
		receipt.PublicationID != request.PublicationID || receipt.TerminalReceiptDigest != request.TerminalReceiptDigest {
		return conflict("rebase does not match the accepted verified-exact publication receipt")
	}
	delta, ok := state.deltas[request.WorkspaceDeltaID]
	prepared, preparedOK := state.preparedDeltas[request.WorkspaceDeltaID]
	if !ok || !preparedOK || delta.Delta.State != harnessv2.WorkspaceDeltaPrepared ||
		delta.Delta.ManifestDigest != request.PublishedManifestDigest || prepared.manifestDigest != request.PublishedManifestDigest {
		return conflict("rebase does not match the prepared workspace delta")
	}
	expectedTree, err := harnessv2.RebasedWorkspaceTreeDigest(
		state.descriptor.WorkspaceBaseline.TreeDigest, request.PublishedManifestDigest, request.NextBaseline.Revision,
	)
	if err != nil || request.NextBaseline.TreeDigest != expectedTree {
		return conflict("rebase next baseline does not match the derived tree identity")
	}
	if err := ensureSessionOperationCapacityLocked(state, sessionDeletionOperationReserve); err != nil {
		return notReady(err.Error())
	}
	return prepared, nil
}

func rebaseReceiptMatches(receipt harnessv2.RuntimeSessionRebaseReceipt, request harnessv2.RebaseRuntimeSessionRequest) bool {
	return receipt.WorkspaceDeltaID == request.WorkspaceDeltaID && receipt.PublicationID == request.PublicationID &&
		receipt.TerminalReceiptDigest == request.TerminalReceiptDigest &&
		receipt.PublishedManifestDigest == request.PublishedManifestDigest && receipt.NextBaseline == request.NextBaseline
}
