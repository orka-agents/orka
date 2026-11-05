package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/store"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

var _ storagev1.SessionTurnRecordStore = (*Store)(nil)

var deferredSessionTurnProjectionTime = time.Date(9999, time.December, 31, 23, 59, 59, 0, time.UTC)

// CreateSessionTurnRecord persists only the SQLite-owned open turn after the
// Kubernetes store has validated SessionControl and PromptAttempt fences.
func (s *Store) CreateSessionTurnRecord(ctx context.Context, request store.CreateSessionTurnRecordRequest) (*store.SessionTurn, error) {
	request.Namespace = strings.TrimSpace(request.Namespace)
	request.SessionName = strings.TrimSpace(request.SessionName)
	if err := store.ValidateControlIdentifier("session namespace", request.Namespace); err != nil {
		return nil, err
	}
	if err := store.ValidateControlIdentifier("session name", request.SessionName); err != nil {
		return nil, err
	}
	normalized, _, err := store.NormalizeSessionTurnForCreate(request.Turn, request.Fence)
	if err != nil {
		return nil, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if existing, getErr := getSessionTurn(ctx, tx, normalized.ID); getErr == nil {
		if !store.SameSessionTurnCreation(existing, normalized) {
			return nil, store.ConflictErrorf("session turn %q was reused with different prompt input or request digest", normalized.ID)
		}
		if err := requireSessionTurnBinding(ctx, tx, normalized.ID, request.Namespace, request.SessionName); err != nil {
			return nil, err
		}
		return &existing, nil
	} else if !errors.Is(getErr, store.ErrNotFound) {
		return nil, getErr
	}
	if err := requireTranscriptSession(ctx, tx, request.Namespace, request.SessionName); err != nil {
		return nil, err
	}
	if err := requireSessionUIDBinding(ctx, tx, normalized.Key.SessionUID, request.Namespace, request.SessionName); err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO session_turns(
			id, namespace, session_name, session_uid, lease_generation, task_uid, attempt, prompt_id,
			prompt_attempt_id, request_digest, user_prompt, state, terminal_kind, terminal_content,
			finalization_digest, publication_id, publication_receipt, controller_epoch_name,
			controller_epoch, version, created_at, finalized_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'Open', '', '', '', '', NULL, ?, ?, 1, ?, NULL, ?)`,
		normalized.ID, request.Namespace, request.SessionName, normalized.Key.SessionUID,
		normalized.Key.LeaseGeneration, normalized.Key.TaskUID, normalized.Key.Attempt,
		normalized.Key.PromptID, normalized.PromptAttemptID, normalized.RequestDigest,
		normalized.UserPrompt, normalized.ControllerEpochName, normalized.ControllerEpoch,
		normalized.CreatedAt, normalized.UpdatedAt,
	)
	if err != nil {
		if isSQLiteConstraintError(err) {
			return nil, store.ConflictErrorf("session turn %q was created concurrently", normalized.ID)
		}
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &normalized, nil
}

// ReadSessionTurnFinalizationBasis implements storagev1.SessionTurnRecordStore.
// The turn, the transcript boundary and the stored native record are read
// from one transaction snapshot.
func (s *Store) ReadSessionTurnFinalizationBasis(ctx context.Context, turnID, namespace, sessionName string) (*storagev1.SessionTurnFinalizationBasis, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	return readSessionTurnFinalizationBasisTx(ctx, tx, strings.TrimSpace(turnID), strings.TrimSpace(namespace), strings.TrimSpace(sessionName))
}

func readSessionTurnFinalizationBasisTx(ctx context.Context, tx *sql.Tx, turnID, namespace, sessionName string) (*storagev1.SessionTurnFinalizationBasis, error) {
	turn, err := getSessionTurn(ctx, tx, turnID)
	if err != nil {
		return nil, err
	}
	basis := &storagev1.SessionTurnFinalizationBasis{Turn: turn}
	if basis.MessageCount, basis.LastMessageID, err = sessionTranscriptBoundaryTx(ctx, tx, namespace, sessionName); err != nil {
		return nil, err
	}
	basis.NativeSession, err = readSealedNativeSessionTx(ctx, tx, namespace, sessionName)
	if errors.Is(err, store.ErrNotFound) {
		err = nil
	}
	if err != nil {
		return nil, err
	}
	return basis, nil
}

// sessionTranscriptBoundaryTx returns the Session's transcript message count
// and the ID of its last message.
func sessionTranscriptBoundaryTx(ctx context.Context, tx *sql.Tx, namespace, sessionName string) (int, string, error) {
	var count int
	err := tx.QueryRowContext(ctx, `SELECT message_count FROM sessions WHERE namespace = ? AND name = ?`, namespace, sessionName).Scan(&count)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", store.ErrNotFound
	}
	if err != nil {
		return 0, "", err
	}
	lastID, err := lastSessionMessageIDTx(ctx, tx, namespace, sessionName, count)
	if err != nil {
		return 0, "", err
	}
	return count, lastID, nil
}

// CommitSealedSessionTurnFinalization implements
// storagev1.SessionTurnRecordStore. It atomically persists optional transcript
// entries with the IDs Orka assigned, optional sealed native state, the
// finalized turn receipt, and a deliberately deferred terminal projection. It
// never reads or mutates SQLite control-plane mirror tables.
func (s *Store) CommitSealedSessionTurnFinalization(ctx context.Context, finalization storagev1.SealedSessionTurnFinalization) (*store.SessionTurn, error) {
	normalized, projection, err := normalizeSealedSessionTurnFinalization(finalization)
	if err != nil {
		return nil, err
	}
	deferredProjection := projection
	deferredProjection.AvailableAt = deferredSessionTurnProjectionTime

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	turnID, err := normalized.Key.CanonicalID()
	if err != nil {
		return nil, err
	}
	turn, err := getSessionTurn(ctx, tx, turnID)
	if err != nil {
		return nil, err
	}
	if err := requireSessionTurnBinding(ctx, tx, turn.ID, normalized.Namespace, normalized.SessionName); err != nil {
		return nil, err
	}
	if turn.State == store.SessionTurnFinalized {
		if sessionTurnPersistenceFinalizationMatches(turn, normalized) {
			return &turn, nil
		}
		return nil, store.ConflictErrorf("session turn %q was finalized with different terminal data, receipt, or digest", turn.ID)
	}
	if normalized.NativeSessionDigest != "" && normalized.NativeSession == nil {
		return nil, store.ValidationErrorf("session turn %q native state is required to finalize it", turn.ID)
	}
	if turn.Key != normalized.Key {
		return nil, store.ConflictErrorf("session turn %q key does not match finalization request", turn.ID)
	}
	if turn.Version != normalized.ExpectedTurnVersion {
		return nil, store.ConflictErrorf("session turn %q is version %d, expected %d", turn.ID, turn.Version, normalized.ExpectedTurnVersion)
	}
	if err := requireTranscriptSession(ctx, tx, normalized.Namespace, normalized.SessionName); err != nil {
		return nil, err
	}
	if native := normalized.NativeSession; native != nil {
		count, lastID, err := sessionTranscriptBoundaryTx(ctx, tx, normalized.Namespace, normalized.SessionName)
		if err != nil {
			return nil, err
		}
		if count != native.ExpectedMessageCount || lastID != native.ExpectedLastMessageID {
			return nil, store.ConflictErrorf("session %s/%s transcript changed after its native state was sealed", normalized.Namespace, normalized.SessionName)
		}
	}
	receiptJSON, err := marshalOptionalControlJSON(normalized.PublicationReceipt)
	if err != nil {
		return nil, err
	}
	messageCountDelta := 0
	if !normalized.SkipTranscriptAppend {
		terminalRole := sessionTurnRoleAssistant
		if normalized.TerminalKind == store.SessionTurnOutcomeMarker {
			terminalRole = sessionTurnRoleOutcomeMarker
		}
		messageCountDelta, err = appendSessionTurnTranscript(
			ctx, tx, normalized.Namespace, normalized.SessionName, turn.UserPrompt,
			terminalRole, normalized.TerminalContent, normalized.FinalizedAt,
			normalized.UserMessageID, normalized.TerminalMessageID,
		)
		if err != nil {
			return nil, err
		}
	}
	if err := finalizeSessionTranscriptTx(ctx, tx, normalized.Namespace, normalized.SessionName,
		normalized.Key, messageCountDelta, normalized.InputTokens, normalized.OutputTokens, normalized.FinalizedAt); err != nil {
		return nil, err
	}
	if err := commitSealedNativeFinalizationTx(ctx, tx, turn.ID, normalized.Namespace, normalized.SessionName,
		normalized.Key.SessionUID, normalized.NativeSession, normalized.NativeSessionDigest); err != nil {
		return nil, err
	}
	turnResult, err := tx.ExecContext(ctx,
		`UPDATE session_turns
		 SET state = 'Finalized', terminal_kind = ?, terminal_content = ?, finalization_digest = ?,
		     publication_id = ?, publication_receipt = ?, projection_id = ?, projection_kind = ?,
		     projection_digest = ?, projection_available_at = ?, controller_epoch_name = ?, controller_epoch = ?,
		     version = version + 1, finalized_at = ?, updated_at = ?
		 WHERE id = ? AND version = ? AND state = 'Open'`,
		string(normalized.TerminalKind), normalized.TerminalContent, normalized.FinalizationDigest,
		normalized.PublicationID, receiptJSON, projection.ID, projection.ProjectionKind,
		projection.PayloadDigest, projection.InitialAvailableAt, normalized.Fence.Name, normalized.Fence.Epoch,
		normalized.FinalizedAt, normalized.FinalizedAt, turn.ID, normalized.ExpectedTurnVersion,
	)
	if err != nil {
		return nil, err
	}
	if err := rowsAffectedExactlyOne(turnResult, "session turn finalization"); err != nil {
		return nil, err
	}
	if _, err := enqueueOutboxProjectionTx(ctx, tx, deferredProjection, true); err != nil {
		return nil, err
	}

	turn.State = store.SessionTurnFinalized
	turn.TerminalKind = normalized.TerminalKind
	turn.TerminalContent = normalized.TerminalContent
	turn.FinalizationDigest = normalized.FinalizationDigest
	turn.NativeSessionDigest = normalized.NativeSessionDigest
	turn.PublicationID = normalized.PublicationID
	turn.PublicationReceipt = clonePublicationReceipt(normalized.PublicationReceipt)
	turn.ProjectionID = projection.ID
	turn.ProjectionKind = projection.ProjectionKind
	turn.ProjectionDigest = projection.PayloadDigest
	turn.ProjectionAvailableAt = projection.InitialAvailableAt
	turn.ControllerEpochName = normalized.Fence.Name
	turn.ControllerEpoch = normalized.Fence.Epoch
	turn.Version++
	turn.FinalizedAt = &normalized.FinalizedAt
	turn.UpdatedAt = normalized.FinalizedAt
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &turn, nil
}

func finalizeSessionTranscriptTx(
	ctx context.Context, tx *sql.Tx, namespace, name string, key store.SessionTurnKey,
	messageCountDelta int, inputTokens, outputTokens int64, finalizedAt time.Time,
) error {
	taskUID := key.TaskUID
	// A turn is finalized exactly once, so the usage Orka attributed to its
	// prompt attempt is added to the Session's cumulative counters once; an
	// earlier attempt of the same Task was counted by its own turn.
	result, err := tx.ExecContext(ctx,
		`UPDATE sessions SET message_count = message_count + ?, input_tokens = input_tokens + ?,
		     output_tokens = output_tokens + ?, updated_at = ?
		 WHERE namespace = ? AND name = ?`,
		messageCountDelta, inputTokens, outputTokens, finalizedAt, namespace, name,
	)
	if err != nil {
		return err
	}
	if err := rowsAffectedExactlyOne(result, "session transcript finalization"); err != nil {
		return err
	}
	// Gateway projection owns the canonical transcript and releases its Task
	// lock atomically with the terminal message. Ordinary finalization releases
	// only this Task UID, including the legacy UID-in-active_task representation.
	_, err = tx.ExecContext(ctx,
		`UPDATE sessions SET active_task = '', active_task_uid = '', active_task_expires_at = NULL
		 WHERE namespace = ? AND name = ? AND session_type <> ? AND owner_type <> ?
		   AND (active_task_uid = ? OR (active_task_uid = '' AND active_task = ?))`,
		namespace, name, store.SessionTypeGateway, gatewaySessionOwnerType, taskUID, taskUID,
	)
	return err
}

// ActivateSessionTurnProjection makes the deferred terminal projection
// claimable only after Kubernetes control finalization has committed.
func (s *Store) ActivateSessionTurnProjection(ctx context.Context, request store.ActivateSessionTurnProjectionRequest) (*store.OutboxProjection, error) {
	normalized, err := normalizeActivateSessionTurnProjectionRequest(request)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	turn, err := getSessionTurn(ctx, tx, normalized.TurnID)
	if err != nil {
		return nil, err
	}
	if turn.State != store.SessionTurnFinalized || turn.FinalizationDigest != normalized.FinalizationDigest {
		return nil, store.ConflictErrorf("session turn %q is not finalized with the expected digest", normalized.TurnID)
	}
	projection, err := getOutboxProjection(ctx, tx, normalized.ProjectionID)
	if err != nil {
		return nil, err
	}
	if projection.AggregateKind != normalized.ExpectedAggregateKind || projection.AggregateID != normalized.TurnID || projection.ProjectionKind != normalized.ExpectedProjectionKind || projection.PayloadDigest != normalized.ExpectedPayloadDigest {
		return nil, store.ConflictErrorf("outbox projection %q does not match finalized SessionTurn", projection.ID)
	}
	operationID := "activate-session-turn:" + normalized.TurnID
	if projection.LastOperationID == operationID {
		if projection.LastOperationDigest != normalized.FinalizationDigest {
			return nil, store.ConflictErrorf("outbox activation operation %q was reused with a different digest", operationID)
		}
		return &projection, nil
	}
	if projection.InitialAvailableAt.Equal(normalized.AvailableAt) && !projection.AvailableAt.Equal(deferredSessionTurnProjectionTime) {
		// Once the deferred timestamp is released, later outbox claim/retry/
		// delivery transitions own AvailableAt and LastOperationID. Finalization
		// retries must not rewind or reject that delivery state.
		return &projection, nil
	}
	if projection.State != store.OutboxProjectionPending || projection.Attempts != 0 || projection.LeaseOwner != "" || projection.LeaseExpiresAt != nil {
		return nil, store.ConflictErrorf("outbox projection %q was claimed before SessionTurn activation", projection.ID)
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE outbox_projections
		 SET available_at = ?, last_operation_id = ?, last_operation_digest = ?,
		     controller_epoch_name = ?, controller_epoch = ?, version = version + 1, updated_at = ?
		 WHERE id = ? AND version = ? AND state = 'Pending' AND attempts = 0 AND lease_owner = ''`,
		normalized.AvailableAt, operationID, normalized.FinalizationDigest,
		normalized.Fence.Name, normalized.Fence.Epoch, normalized.UpdatedAt,
		projection.ID, projection.Version,
	)
	if err != nil {
		return nil, err
	}
	if err := rowsAffectedExactlyOne(result, "session turn outbox activation"); err != nil {
		return nil, err
	}
	projection.AvailableAt = normalized.AvailableAt
	projection.LastOperationID = operationID
	projection.LastOperationDigest = normalized.FinalizationDigest
	projection.ControllerEpochName = normalized.Fence.Name
	projection.ControllerEpoch = normalized.Fence.Epoch
	projection.Version++
	projection.UpdatedAt = normalized.UpdatedAt
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &projection, nil
}

func normalizeSealedSessionTurnFinalization(request storagev1.SealedSessionTurnFinalization) (storagev1.SealedSessionTurnFinalization, store.OutboxProjection, error) {
	if err := request.Key.Validate(); err != nil {
		return storagev1.SealedSessionTurnFinalization{}, store.OutboxProjection{}, err
	}
	request.Namespace = strings.TrimSpace(request.Namespace)
	request.SessionName = strings.TrimSpace(request.SessionName)
	if err := store.ValidateControlIdentifier("session namespace", request.Namespace); err != nil {
		return storagev1.SealedSessionTurnFinalization{}, store.OutboxProjection{}, err
	}
	if err := store.ValidateControlIdentifier("session name", request.SessionName); err != nil {
		return storagev1.SealedSessionTurnFinalization{}, store.OutboxProjection{}, err
	}
	fence, err := store.NormalizeEpochFence(request.Fence)
	if err != nil {
		return storagev1.SealedSessionTurnFinalization{}, store.OutboxProjection{}, err
	}
	request.Fence = fence
	if request.ExpectedTurnVersion < 1 {
		return storagev1.SealedSessionTurnFinalization{}, store.OutboxProjection{}, store.ValidationErrorf("expected turn version must be at least 1")
	}
	if err := store.ValidateCanonicalDigest("session turn finalization digest", request.FinalizationDigest); err != nil {
		return storagev1.SealedSessionTurnFinalization{}, store.OutboxProjection{}, err
	}
	if request.TerminalKind != store.SessionTurnAssistantResult && request.TerminalKind != store.SessionTurnOutcomeMarker {
		return storagev1.SealedSessionTurnFinalization{}, store.OutboxProjection{}, store.ValidationErrorf("unsupported session turn terminal kind %q", request.TerminalKind)
	}
	if request.TerminalKind == store.SessionTurnOutcomeMarker && strings.TrimSpace(request.TerminalContent) == "" {
		return storagev1.SealedSessionTurnFinalization{}, store.OutboxProjection{}, store.ValidationErrorf("outcome-marker finalization requires explicit marker content")
	}
	if request.SkipTranscriptAppend && request.SkipUserPromptAppend {
		return storagev1.SealedSessionTurnFinalization{}, store.OutboxProjection{}, store.ValidationErrorf("session turn cannot combine full transcript suppression with user-prompt-only suppression")
	}
	if err := validateSealedNativeFinalization(request); err != nil {
		return storagev1.SealedSessionTurnFinalization{}, store.OutboxProjection{}, err
	}
	if err := validateFinalizationMessageIDs(request.SkipTranscriptAppend, request.SkipUserPromptAppend, request.UserMessageID, request.TerminalMessageID); err != nil {
		return storagev1.SealedSessionTurnFinalization{}, store.OutboxProjection{}, err
	}
	if request.InputTokens < 0 || request.OutputTokens < 0 ||
		request.InputTokens > store.MaxUsageTokenCount || request.OutputTokens > store.MaxUsageTokenCount {
		return storagev1.SealedSessionTurnFinalization{}, store.OutboxProjection{}, store.ValidationErrorf("session turn token usage is out of range")
	}
	if err := store.ValidateControlText("session turn terminal content", request.TerminalContent); err != nil {
		return storagev1.SealedSessionTurnFinalization{}, store.OutboxProjection{}, err
	}
	request.PublicationID = strings.TrimSpace(request.PublicationID)
	if request.PublicationID != "" {
		if err := store.ValidateControlIdentifier("publication ID", request.PublicationID); err != nil {
			return storagev1.SealedSessionTurnFinalization{}, store.OutboxProjection{}, err
		}
		if request.PublicationReceipt == nil || request.PublicationReceipt.PublicationID != request.PublicationID {
			return storagev1.SealedSessionTurnFinalization{}, store.OutboxProjection{}, store.ValidationErrorf("publication finalization requires the matching receipt snapshot")
		}
	} else if request.PublicationReceipt != nil {
		return storagev1.SealedSessionTurnFinalization{}, store.OutboxProjection{}, store.ValidationErrorf("publication receipt requires a publication ID")
	}
	request.FinalizedAt = store.NormalizeControlTime(request.FinalizedAt)
	projection, _, err := normalizeOutboxProjectionForCreate(request.Projection, request.Fence, request.FinalizedAt)
	if err != nil {
		return storagev1.SealedSessionTurnFinalization{}, store.OutboxProjection{}, err
	}
	turnID, err := request.Key.CanonicalID()
	if err != nil {
		return storagev1.SealedSessionTurnFinalization{}, store.OutboxProjection{}, err
	}
	if projection.AggregateKind != sessionTurnAggregateKind || projection.AggregateID != turnID {
		return storagev1.SealedSessionTurnFinalization{}, store.OutboxProjection{}, store.ValidationErrorf("finalization projection must target SessionTurn aggregate %q", turnID)
	}
	return request, projection, nil
}

// appendSessionTurnTranscript appends the user prompt, unless userMessageID
// is empty, and the terminal message with the given IDs.
func appendSessionTurnTranscript(
	ctx context.Context,
	tx *sql.Tx,
	namespace, sessionName, userPrompt, terminalRole, terminalContent string,
	createdAt time.Time,
	userMessageID, terminalMessageID string,
) (int, error) {
	appendMessage := func(messageID, role, content string) error {
		sortOrder, err := nextSessionMessageOrderTx(ctx, tx, namespace, sessionName)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO session_messages(
				namespace, session_name, message_id, sort_order, role, content, metadata_json, created_at
			) VALUES (?, ?, ?, ?, ?, ?, '{}', ?)`,
			namespace, sessionName, messageID, sortOrder, role, content, createdAt,
		)
		if isSQLiteConstraintError(err) {
			return store.ConflictErrorf("session %s/%s already has transcript message %q", namespace, sessionName, messageID)
		}
		return err
	}
	messageCount := 0
	if userMessageID != "" {
		if err := appendMessage(userMessageID, "user", userPrompt); err != nil {
			return 0, err
		}
		messageCount++
	}
	if err := appendMessage(terminalMessageID, terminalRole, terminalContent); err != nil {
		return 0, err
	}
	return messageCount + 1, nil
}

// validateFinalizationMessageIDs requires an ID for each message a
// finalization appends and none for a message it leaves out.
func validateFinalizationMessageIDs(skipTranscript, skipUserPrompt bool, userMessageID, terminalMessageID string) error {
	if skipTranscript {
		if userMessageID != "" || terminalMessageID != "" {
			return store.ValidationErrorf("a finalization that leaves the transcript unchanged must not carry message IDs")
		}
		return nil
	}
	if err := store.ValidateControlIdentifier("terminal message ID", terminalMessageID); err != nil {
		return err
	}
	if skipUserPrompt {
		if userMessageID != "" {
			return store.ValidationErrorf("a finalization that skips the user prompt must not carry its message ID")
		}
		return nil
	}
	if err := store.ValidateControlIdentifier("user message ID", userMessageID); err != nil {
		return err
	}
	if userMessageID == terminalMessageID {
		return store.ValidationErrorf("finalization message IDs must differ")
	}
	return nil
}

// validateSealedNativeFinalization admits sealed native state on exactly two
// terminal shapes: a fresh capture committed with a canonical assistant
// result, or the Session's existing checkpoint carried across a non-success
// outcome marker.
func validateSealedNativeFinalization(request storagev1.SealedSessionTurnFinalization) error {
	native := request.NativeSession
	if native == nil {
		return nil
	}
	if err := store.ValidateCanonicalDigest("native capture digest", request.NativeSessionDigest); err != nil {
		return err
	}
	if request.SkipTranscriptAppend {
		return store.ValidationErrorf("native state cannot bind to a finalization that leaves the transcript unchanged")
	}
	if native.Carried && request.TerminalKind != store.SessionTurnOutcomeMarker {
		return store.ValidationErrorf("carried native checkpoint requires an outcome-marker finalization")
	}
	if !native.Carried && request.TerminalKind != store.SessionTurnAssistantResult {
		return store.ValidationErrorf("native capture requires canonical assistant-result finalization")
	}
	if native.Carried {
		if err := store.ValidateCanonicalDigest("carried native record digest", native.ExpectedRecordDigest); err != nil {
			return err
		}
	}
	// A message stored before transcript messages carried IDs has none, so
	// a nonempty transcript may end without a last message ID.
	if native.ExpectedMessageCount < 0 || (native.ExpectedMessageCount == 0 && native.ExpectedLastMessageID != "") {
		return store.ValidationErrorf("expected transcript boundary is incomplete")
	}
	return native.Record.Validate()
}

func normalizeActivateSessionTurnProjectionRequest(request store.ActivateSessionTurnProjectionRequest) (store.ActivateSessionTurnProjectionRequest, error) {
	request.TurnID = strings.TrimSpace(request.TurnID)
	request.ProjectionID = strings.TrimSpace(request.ProjectionID)
	request.ExpectedAggregateKind = strings.TrimSpace(request.ExpectedAggregateKind)
	request.ExpectedProjectionKind = strings.TrimSpace(request.ExpectedProjectionKind)
	for field, value := range map[string]string{
		"session turn ID":        request.TurnID,
		"outbox projection ID":   request.ProjectionID,
		"outbox aggregate kind":  request.ExpectedAggregateKind,
		"outbox projection kind": request.ExpectedProjectionKind,
	} {
		if err := store.ValidateControlIdentifier(field, value); err != nil {
			return store.ActivateSessionTurnProjectionRequest{}, err
		}
	}
	if err := store.ValidateCanonicalDigest("outbox payload digest", request.ExpectedPayloadDigest); err != nil {
		return store.ActivateSessionTurnProjectionRequest{}, err
	}
	fence, err := store.NormalizeEpochFence(request.Fence)
	if err != nil {
		return store.ActivateSessionTurnProjectionRequest{}, err
	}
	request.Fence = fence
	if err := store.ValidateCanonicalDigest("session turn finalization digest", request.FinalizationDigest); err != nil {
		return store.ActivateSessionTurnProjectionRequest{}, err
	}
	request.UpdatedAt = store.NormalizeControlTime(request.UpdatedAt)
	if request.AvailableAt.IsZero() {
		request.AvailableAt = request.UpdatedAt
	} else {
		request.AvailableAt = request.AvailableAt.UTC()
	}
	return request, nil
}

func sessionTurnPersistenceFinalizationMatches(turn store.SessionTurn, request storagev1.SealedSessionTurnFinalization) bool {
	return turn.FinalizationDigest == request.FinalizationDigest && turn.TerminalKind == request.TerminalKind &&
		turn.NativeSessionDigest == request.NativeSessionDigest &&
		turn.TerminalContent == request.TerminalContent && turn.PublicationID == request.PublicationID &&
		reflect.DeepEqual(turn.PublicationReceipt, request.PublicationReceipt) && turn.ProjectionID == request.Projection.ID &&
		turn.ProjectionKind == request.Projection.ProjectionKind && turn.ProjectionDigest == request.Projection.PayloadDigest &&
		turn.ProjectionAvailableAt.Equal(projectionAvailableAt(request.Projection, request.FinalizedAt))
}

func projectionAvailableAt(projection store.OutboxProjection, fallback time.Time) time.Time {
	if projection.AvailableAt.IsZero() {
		return fallback.UTC()
	}
	return projection.AvailableAt.UTC()
}

func requireTranscriptSession(ctx context.Context, q controlQueryRower, namespace, sessionName string) error {
	var count int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE namespace = ? AND name = ?`, namespace, sessionName).Scan(&count); err != nil {
		return err
	}
	if count != 1 {
		return store.ErrNotFound
	}
	return nil
}

func requireSessionUIDBinding(ctx context.Context, q controlQueryRower, sessionUID, namespace, sessionName string) error {
	var existingNamespace, existingSessionName string
	err := q.QueryRowContext(ctx, `SELECT namespace, session_name FROM session_turns WHERE session_uid = ? LIMIT 1`, sessionUID).Scan(&existingNamespace, &existingSessionName)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if existingNamespace != namespace || existingSessionName != sessionName {
		return store.ConflictErrorf("session UID %q is already bound to %s/%s", sessionUID, existingNamespace, existingSessionName)
	}
	return nil
}

func requireSessionTurnBinding(ctx context.Context, q controlQueryRower, turnID, namespace, sessionName string) error {
	var existingNamespace, existingSessionName string
	err := q.QueryRowContext(ctx, `SELECT namespace, session_name FROM session_turns WHERE id = ?`, turnID).Scan(&existingNamespace, &existingSessionName)
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrNotFound
	}
	if err != nil {
		return err
	}
	if existingNamespace != namespace || existingSessionName != sessionName {
		return store.ConflictErrorf("session turn %q is bound to %s/%s, not %s/%s", turnID, existingNamespace, existingSessionName, namespace, sessionName)
	}
	return nil
}

func clonePublicationReceipt(receipt *store.PublicationReceipt) *store.PublicationReceipt {
	if receipt == nil {
		return nil
	}
	copyValue := *receipt
	return &copyValue
}
