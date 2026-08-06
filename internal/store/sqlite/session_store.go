package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

var (
	_ store.SessionStore         = (*Store)(nil)
	_ store.SessionTurnCommitter = (*Store)(nil)
)

// CreateSession inserts a new session record.
func (s *Store) CreateSession(ctx context.Context, session *store.SessionRecord) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (namespace, name, session_type, active_task, active_task_uid, message_count, input_tokens, output_tokens, cancelled, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		session.Namespace, session.Name, session.SessionType, session.ActiveTask, session.ActiveTaskUID,
		session.MessageCount, session.InputTokens, session.OutputTokens, session.Cancelled,
		session.CreatedAt, session.UpdatedAt,
	)
	return err
}

// GetSession loads a session with all its messages.
func (s *Store) GetSession(ctx context.Context, namespace, name string) (*store.SessionRecord, error) {
	session := &store.SessionRecord{}
	err := s.db.QueryRowContext(ctx,
		`SELECT namespace, name, session_type, active_task, active_task_uid, message_count, input_tokens, output_tokens, cancelled, created_at, updated_at
		 FROM sessions WHERE namespace = ? AND name = ?`,
		namespace, name,
	).Scan(
		&session.Namespace, &session.Name, &session.SessionType, &session.ActiveTask, &session.ActiveTaskUID,
		&session.MessageCount, &session.InputTokens, &session.OutputTokens, &session.Cancelled,
		&session.CreatedAt, &session.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	messages, err := s.LoadTranscript(ctx, namespace, name, 0)
	if err != nil {
		return nil, err
	}
	session.Messages = messages

	return session, nil
}

// GetSessionType returns the stored Session type without loading transcript messages.
func (s *Store) GetSessionType(ctx context.Context, namespace, name string) (string, error) {
	var sessionType string
	err := s.db.QueryRowContext(ctx,
		`SELECT session_type FROM sessions WHERE namespace = ? AND name = ?`,
		namespace, name,
	).Scan(&sessionType)
	if errors.Is(err, sql.ErrNoRows) {
		return "", store.ErrNotFound
	}
	return sessionType, err
}

// ListSessions returns metadata for all sessions in a namespace.
func (s *Store) ListSessions(ctx context.Context, namespace string) ([]store.SessionMetadata, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, session_type, message_count, input_tokens, output_tokens, created_at, updated_at, active_task, active_task_uid
		 FROM sessions WHERE namespace = ?`,
		namespace,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	var sessions []store.SessionMetadata
	for rows.Next() {
		var m store.SessionMetadata
		if err := rows.Scan(&m.Name, &m.SessionType, &m.MessageCount, &m.InputTokens, &m.OutputTokens, &m.CreatedAt, &m.UpdatedAt, &m.ActiveTask, &m.ActiveTaskUID); err != nil {
			return nil, err
		}
		sessions = append(sessions, m)
	}
	return sessions, rows.Err()
}

// DeleteSession removes a session, its messages (via CASCADE), and its session-scoped execution event read model.
func (s *Store) DeleteSession(ctx context.Context, namespace, name string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var sessionType, chatTurnID string
	if err := tx.QueryRowContext(ctx,
		`SELECT session_type, chat_turn_id FROM sessions WHERE namespace = ? AND name = ?`,
		namespace, name,
	).Scan(&sessionType, &chatTurnID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if chatTurnID != "" {
		return store.ErrConflict
	}
	var pendingGatewayEvents int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM gateway_events
		WHERE namespace = ? AND session_name = ? AND state IN (?, ?, ?, ?)`,
		namespace, name, store.GatewayEventAccepted, store.GatewayEventQueued,
		store.GatewayEventDispatching, store.GatewayEventTaskCreated,
	).Scan(&pendingGatewayEvents); err != nil {
		return err
	}
	if pendingGatewayEvents > 0 {
		if sessionType == store.SessionTypeGateway {
			return errors.Join(store.ErrConflict, store.ErrGatewayOwnedSession)
		}
		return store.ErrConflict
	}
	if sessionType == store.SessionTypeGateway {
		return store.ErrGatewayOwnedSession
	}
	deleteResult, err := tx.ExecContext(ctx,
		`DELETE FROM sessions WHERE namespace = ? AND name = ? AND session_type <> ?`,
		namespace, name, store.SessionTypeGateway,
	)
	if err != nil {
		return err
	}
	deleted, err := deleteResult.RowsAffected()
	if err != nil {
		return err
	}
	if sessionType != "" && deleted == 0 {
		return store.ErrConflict
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE execution_events SET session_name = '', session_seq = 0 WHERE namespace = ? AND session_name = ?`,
		namespace,
		name,
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM execution_event_session_sequences WHERE namespace = ? AND session_name = ?`, namespace, name); err != nil {
		return err
	}
	return tx.Commit()
}

// AcquireLock atomically sets the active_task for a session.
// Returns store.ErrNotFound if the session does not exist, or an error if already locked.
func (s *Store) AcquireLock(ctx context.Context, namespace, name, taskName, taskUID string) error {
	// Check if session exists
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sessions WHERE namespace = ? AND name = ?`,
		namespace, name,
	).Scan(&count)
	if err != nil {
		return err
	}
	if count == 0 {
		return store.ErrNotFound
	}
	var ownerType string
	if err := s.db.QueryRowContext(ctx, `SELECT owner_type FROM sessions WHERE namespace = ? AND name = ?`,
		namespace, name).Scan(&ownerType); err != nil {
		return err
	}
	if ownerType == gatewaySessionOwnerType {
		if taskUID == "" {
			return store.ValidationErrorf("gateway-owned session %s/%s requires an admitted Task identity", namespace, name)
		}
		var eventState store.GatewayEventState
		var storedTaskUID string
		err := s.db.QueryRowContext(ctx, `SELECT state, task_uid FROM gateway_events
			WHERE namespace = ? AND session_name = ? AND task_name = ?
			ORDER BY created_at DESC, id DESC LIMIT 1`,
			namespace, name, taskName,
		).Scan(&eventState, &storedTaskUID)
		if errors.Is(err, sql.ErrNoRows) {
			return store.ValidationErrorf(
				"task %s/%s is not the admitted owner of gateway session %s/%s",
				namespace, taskName, namespace, name,
			)
		}
		if err != nil {
			return err
		}
		if eventState == store.GatewayEventDispatching && storedTaskUID == "" {
			return fmt.Errorf("%w: gateway Task ownership linkage is pending", store.ErrNotReady)
		}
		if eventState != store.GatewayEventTaskCreated || storedTaskUID != taskUID {
			return store.ValidationErrorf(
				"task %s/%s is not the admitted owner of gateway session %s/%s",
				namespace, taskName, namespace, name,
			)
		}
	}

	// Try to acquire the exact Task incarnation. Empty stored UIDs are adopted only for
	// compatibility with locks created before the fencing column existed. A live chat
	// turn owns the same transcript boundary and therefore excludes Task locks; an
	// expired chat lease is cleared as part of the successful lock acquisition.
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET active_task = ?, active_task_uid = ?,
		   chat_turn_id = CASE
		     WHEN chat_turn_id <> '' AND (chat_turn_expires_at IS NULL OR chat_turn_expires_at <= ?) THEN ''
		     ELSE chat_turn_id
		   END,
		   chat_turn_expires_at = CASE
		     WHEN chat_turn_id <> '' AND (chat_turn_expires_at IS NULL OR chat_turn_expires_at <= ?) THEN NULL
		     ELSE chat_turn_expires_at
		   END
		 WHERE namespace = ? AND name = ? AND (active_task = '' OR
		   (active_task = ? AND (active_task_uid = '' OR active_task_uid = ?)))
		 AND (chat_turn_id = '' OR chat_turn_expires_at IS NULL OR chat_turn_expires_at <= ?)`,
		taskName, taskUID, now, now, namespace, name, taskName, taskUID, now,
	)
	if err != nil {
		return err
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return fmt.Errorf("session %s/%s is already locked or has an active chat turn", namespace, name)
	}
	return nil
}

// AcquireChatTurn creates a chat Session when needed and reserves its current
// transcript revision for one turn. Expired reservations may be reclaimed, but
// Task locks and Gateway-owned Sessions are never taken over by chat.
func (s *Store) AcquireChatTurn(
	ctx context.Context,
	session *store.SessionRecord,
	turnID string,
	expiresAt time.Time,
) (bool, error) {
	if session == nil {
		return false, store.ValidationErrorf("session is required")
	}
	if session.Namespace == "" || session.Name == "" || session.SessionType == "" || turnID == "" {
		return false, store.ValidationErrorf("session namespace, name, type, and turn ID are required")
	}
	now := time.Now().UTC()
	expiresAt = expiresAt.UTC()
	if !expiresAt.After(now) {
		return false, store.ValidationErrorf("chat turn expiration must be in the future")
	}
	createdAt := session.CreatedAt.UTC()
	if createdAt.IsZero() {
		createdAt = now
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback() //nolint:errcheck

	insertResult, err := tx.ExecContext(ctx,
		`INSERT INTO sessions
		 (namespace, name, session_type, active_task, active_task_uid, message_count, input_tokens, output_tokens, cancelled, created_at, updated_at)
		 VALUES (?, ?, ?, '', '', 0, 0, 0, ?, ?, ?)
		 ON CONFLICT(namespace, name) DO NOTHING`,
		session.Namespace, session.Name, session.SessionType, session.Cancelled, createdAt, now,
	)
	if err != nil {
		return false, err
	}
	createdRows, err := insertResult.RowsAffected()
	if err != nil {
		return false, err
	}
	created := createdRows == 1

	var sessionType, ownerType, activeTask, activeTurn string
	var activeTurnExpiresAt sql.NullTime
	if err := tx.QueryRowContext(ctx,
		`SELECT session_type, owner_type, active_task, chat_turn_id, chat_turn_expires_at
		 FROM sessions WHERE namespace = ? AND name = ?`,
		session.Namespace, session.Name,
	).Scan(&sessionType, &ownerType, &activeTask, &activeTurn, &activeTurnExpiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, store.ErrNotFound
		}
		return false, err
	}
	if sessionType == store.SessionTypeGateway || ownerType == gatewaySessionOwnerType {
		return false, store.ErrGatewayOwnedSession
	}
	if activeTask != "" {
		return false, fmt.Errorf("%w: chat session %s/%s has an active Task", store.ErrConflict, session.Namespace, session.Name)
	}
	if activeTurn != "" && activeTurnExpiresAt.Valid && activeTurnExpiresAt.Time.After(now) {
		return false, fmt.Errorf("%w: chat session %s/%s already has an active turn", store.ErrConflict, session.Namespace, session.Name)
	}

	result, err := tx.ExecContext(ctx,
		`UPDATE sessions SET chat_turn_id = ?, chat_turn_expires_at = ?, updated_at = ?
		 WHERE namespace = ? AND name = ? AND active_task = ''
		 AND session_type <> ? AND owner_type <> ?
		 AND (chat_turn_id = '' OR chat_turn_expires_at IS NULL OR chat_turn_expires_at <= ?)`,
		turnID, expiresAt, now, session.Namespace, session.Name,
		store.SessionTypeGateway, gatewaySessionOwnerType, now,
	)
	if err != nil {
		return false, err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if updated != 1 {
		return false, fmt.Errorf("%w: chat session %s/%s already has an active turn", store.ErrConflict, session.Namespace, session.Name)
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return created, nil
}

// ReleaseChatTurn clears a reservation only when it is still owned by turnID.
// If this reservation created the Session, failed turns remove that row only
// while it is still empty and otherwise untouched.
func (s *Store) ReleaseChatTurn(
	ctx context.Context,
	namespace, name, turnID string,
	deleteEmptyCreatedSession bool,
) error {
	if turnID == "" {
		return nil
	}
	if deleteEmptyCreatedSession {
		result, err := s.db.ExecContext(ctx,
			`DELETE FROM sessions
			 WHERE namespace = ? AND name = ? AND chat_turn_id = ?
			   AND session_type = ? AND owner_type <> ? AND active_task = ''
			   AND message_count = 0 AND input_tokens = 0 AND output_tokens = 0
			   AND NOT EXISTS (
			     SELECT 1 FROM session_messages
			     WHERE namespace = ? AND session_name = ?
			   )`,
			namespace, name, turnID, store.SessionTypeChat, gatewaySessionOwnerType, namespace, name,
		)
		if err != nil {
			return err
		}
		deleted, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if deleted == 1 {
			return nil
		}
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET chat_turn_id = '', chat_turn_expires_at = NULL
		 WHERE namespace = ? AND name = ? AND chat_turn_id = ?`,
		namespace, name, turnID,
	)
	return err
}

// CommitSessionTurn atomically appends one chat turn's messages and token
// usage. The reservation owner, lease, and observed message count fence the
// commit against concurrent or recovered turns.
func (s *Store) CommitSessionTurn(
	ctx context.Context,
	session *store.SessionRecord,
	turnID string,
	expectedMessageCount int,
	messages []store.SessionMessage,
	inputTokens, outputTokens int,
) error {
	if session == nil {
		return store.ValidationErrorf("session is required")
	}
	if session.Namespace == "" || session.Name == "" || turnID == "" {
		return store.ValidationErrorf("session namespace, name, and turn ID are required")
	}
	if expectedMessageCount < 0 {
		return store.ValidationErrorf("expected message count must not be negative")
	}
	if inputTokens < 0 || outputTokens < 0 {
		return store.ValidationErrorf("token increments must not be negative")
	}
	if len(messages) == 0 && (inputTokens != 0 || outputTokens != 0) {
		return store.ValidationErrorf("token increments require transcript messages")
	}

	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	var sessionType, ownerType, activeTask, activeTurn string
	var activeTurnExpiresAt sql.NullTime
	var currentMessageCount int
	if err := tx.QueryRowContext(ctx,
		`SELECT session_type, owner_type, active_task, message_count, chat_turn_id, chat_turn_expires_at
		 FROM sessions WHERE namespace = ? AND name = ?`,
		session.Namespace, session.Name,
	).Scan(&sessionType, &ownerType, &activeTask, &currentMessageCount, &activeTurn, &activeTurnExpiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrNotFound
		}
		return err
	}
	if sessionType == store.SessionTypeGateway || ownerType == gatewaySessionOwnerType {
		return store.ErrGatewayOwnedSession
	}
	if activeTask != "" {
		return fmt.Errorf("%w: chat session has an active Task", store.ErrConflict)
	}
	if activeTurn != turnID {
		return fmt.Errorf("%w: chat turn no longer owns session", store.ErrConflict)
	}
	if !activeTurnExpiresAt.Valid || !activeTurnExpiresAt.Time.After(now) {
		return fmt.Errorf("%w: chat turn reservation expired", store.ErrConflict)
	}
	if currentMessageCount != expectedMessageCount {
		return fmt.Errorf(
			"%w: session message count is %d, expected %d",
			store.ErrConflict, currentMessageCount, expectedMessageCount,
		)
	}

	inserted, err := insertSessionMessagesTx(ctx, tx, session.Namespace, session.Name, messages)
	if err != nil {
		return err
	}
	if inserted != len(messages) {
		return fmt.Errorf("%w: chat turn messages were already present", store.ErrConflict)
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE sessions
		 SET message_count = message_count + ?, input_tokens = input_tokens + ?, output_tokens = output_tokens + ?, updated_at = ?
		 WHERE namespace = ? AND name = ? AND active_task = ''
		   AND message_count = ? AND chat_turn_id = ? AND chat_turn_expires_at > ?`,
		inserted, inputTokens, outputTokens, now,
		session.Namespace, session.Name, expectedMessageCount, turnID, now,
	)
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated != 1 {
		return fmt.Errorf("%w: session changed while committing turn", store.ErrConflict)
	}
	return tx.Commit()
}

// ReleaseLock clears the lock only for the exact Task incarnation.
func (s *Store) ReleaseLock(ctx context.Context, namespace, name, taskName, taskUID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET active_task = '', active_task_uid = ''
		 WHERE namespace = ? AND name = ? AND active_task = ?
		   AND active_task_uid = ?`,
		namespace, name, taskName, taskUID,
	)
	return err
}

// IsLocked returns true if the session is locked by another Task incarnation.
func (s *Store) IsLocked(ctx context.Context, namespace, name, currentTask, currentTaskUID string) (bool, error) {
	var activeTask, activeTaskUID, activeTurn string
	var activeTurnExpiresAt sql.NullTime
	err := s.db.QueryRowContext(ctx,
		`SELECT active_task, active_task_uid, chat_turn_id, chat_turn_expires_at
		 FROM sessions WHERE namespace = ? AND name = ?`,
		namespace, name,
	).Scan(&activeTask, &activeTaskUID, &activeTurn, &activeTurnExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, store.ErrNotFound
	}
	if err != nil {
		return false, err
	}
	if activeTurn != "" && activeTurnExpiresAt.Valid && activeTurnExpiresAt.Time.After(time.Now().UTC()) {
		return true, nil
	}
	if activeTask == "" {
		return false, nil
	}
	return activeTask != currentTask || (activeTaskUID != "" && activeTaskUID != currentTaskUID), nil
}

// AppendMessages inserts messages into a session's logical transcript and updates session metadata.
// Stable message IDs make retries idempotent; even-numbered logical orders leave a gap for
// gateway terminal projections that arrive after later user messages were durably admitted.
func (s *Store) AppendMessages(ctx context.Context, namespace, name string, messages []store.SessionMessage) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	var ownerType string
	if err := tx.QueryRowContext(ctx, `SELECT owner_type FROM sessions WHERE namespace = ? AND name = ?`,
		namespace, name).Scan(&ownerType); errors.Is(err, sql.ErrNoRows) {
		return store.ErrNotFound
	} else if err != nil {
		return err
	}
	if ownerType == gatewaySessionOwnerType {
		return store.ErrConflict
	}

	inserted, err := insertSessionMessagesTx(ctx, tx, namespace, name, messages)
	if err != nil {
		return err
	}
	if inserted > 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE sessions SET message_count = message_count + ?, updated_at = CURRENT_TIMESTAMP WHERE namespace = ? AND name = ?`,
			inserted, namespace, name,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func insertSessionMessagesTx(
	ctx context.Context,
	tx *sql.Tx,
	namespace, name string,
	messages []store.SessionMessage,
) (int, error) {
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO session_messages
		 (namespace, session_name, message_id, sort_order, role, content, name, input, tool_calls, tool_call_id,
		  source_type, source_ref, metadata_json, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(namespace, session_name, message_id) WHERE message_id <> '' DO NOTHING`,
	)
	if err != nil {
		return 0, err
	}
	defer stmt.Close() //nolint:errcheck

	inserted := 0
	for _, msg := range messages {
		inputJSON, toolCallsJSON, metadataJSON, err := encodeSessionMessagePayload(msg)
		if err != nil {
			return 0, err
		}
		ts := msg.Timestamp
		if ts.IsZero() {
			ts = time.Now().UTC()
		}
		messageID := msg.ID
		if messageID == "" {
			messageID, err = newSessionMessageID()
			if err != nil {
				return 0, err
			}
		}
		logicalOrder := msg.Order
		if logicalOrder <= 0 {
			logicalOrder, err = nextSessionMessageOrderTx(ctx, tx, namespace, name)
			if err != nil {
				return 0, err
			}
		}
		result, err := stmt.ExecContext(ctx,
			namespace, name, messageID, logicalOrder, msg.Role, msg.Content, nilIfEmpty(msg.Name), inputJSON,
			toolCallsJSON, nilIfEmpty(msg.ToolCallID), msg.SourceType, msg.SourceRef, metadataJSON, ts,
		)
		if err != nil {
			return 0, err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}
		if rows == 0 {
			matches, matchErr := sessionMessageMatchesTx(
				ctx, tx, namespace, name, messageID, logicalOrder, msg.Order > 0, msg, inputJSON, toolCallsJSON, metadataJSON,
			)
			if matchErr != nil {
				return 0, matchErr
			}
			if !matches {
				return 0, store.ErrDuplicateMismatch
			}
		}
		inserted += int(rows)
	}
	return inserted, nil
}

// LoadTranscript retrieves messages in logical conversation order.
func (s *Store) LoadTranscript(ctx context.Context, namespace, name string, maxMessages int) ([]store.SessionMessage, error) {
	return s.loadTranscript(ctx, namespace, name, 0, maxMessages)
}

// LoadTranscriptThrough retrieves logical history through one stable message ID.
func (s *Store) LoadTranscriptThrough(
	ctx context.Context,
	namespace, name, throughMessageID string,
	maxMessages int,
) ([]store.SessionMessage, error) {
	var throughOrder int64
	if err := s.db.QueryRowContext(ctx, `SELECT sort_order FROM session_messages
		WHERE namespace = ? AND session_name = ? AND message_id = ?`,
		namespace, name, throughMessageID,
	).Scan(&throughOrder); errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	} else if err != nil {
		return nil, err
	}
	return s.loadTranscript(ctx, namespace, name, throughOrder, maxMessages)
}

func (s *Store) loadTranscript(
	ctx context.Context,
	namespace, name string,
	throughOrder int64,
	maxMessages int,
) ([]store.SessionMessage, error) {
	baseColumns := `message_id, sort_order, role, content, name, input, tool_calls, tool_call_id,
		 source_type, source_ref, metadata_json, created_at`
	where := `namespace = ? AND session_name = ?`
	args := []any{namespace, name}
	if throughOrder > 0 {
		where += ` AND sort_order <= ?`
		args = append(args, throughOrder)
	}
	var query string
	if maxMessages > 0 {
		query = `SELECT ` + baseColumns + ` FROM (
			SELECT id, ` + baseColumns + ` FROM session_messages WHERE ` + where + `
			ORDER BY sort_order DESC, id DESC LIMIT ?
		) ORDER BY sort_order, id`
		args = append(args, maxMessages)
	} else {
		query = `SELECT ` + baseColumns + ` FROM session_messages WHERE ` + where + ` ORDER BY sort_order, id`
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	var messages []store.SessionMessage
	for rows.Next() {
		message, err := scanSessionMessage(rows)
		if err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	return messages, rows.Err()
}

func scanSessionMessage(row gatewayRowScanner) (store.SessionMessage, error) {
	var msg store.SessionMessage
	var nameStr, inputJSON, toolCallsJSON, toolCallID sql.NullString
	var metadataJSON string
	if err := row.Scan(
		&msg.ID, &msg.Order, &msg.Role, &msg.Content, &nameStr, &inputJSON, &toolCallsJSON, &toolCallID,
		&msg.SourceType, &msg.SourceRef, &metadataJSON, &msg.Timestamp,
	); err != nil {
		return msg, err
	}
	msg.Name = nameStr.String
	msg.ToolCallID = toolCallID.String
	if inputJSON.Valid && inputJSON.String != "" {
		if err := json.Unmarshal([]byte(inputJSON.String), &msg.Input); err != nil {
			return msg, fmt.Errorf("failed to unmarshal input: %w", err)
		}
	}
	if toolCallsJSON.Valid && toolCallsJSON.String != "" {
		if err := json.Unmarshal([]byte(toolCallsJSON.String), &msg.ToolCalls); err != nil {
			return msg, fmt.Errorf("failed to unmarshal tool_calls: %w", err)
		}
	}
	if metadataJSON != "" && metadataJSON != "{}" {
		if err := json.Unmarshal([]byte(metadataJSON), &msg.Metadata); err != nil {
			return msg, fmt.Errorf("failed to unmarshal message metadata: %w", err)
		}
	}
	return msg, nil
}

func newSessionMessageID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate session message ID: %w", err)
	}
	return "local:" + hex.EncodeToString(value[:]), nil
}

func sessionMessageMatchesTx(
	ctx context.Context, tx *sql.Tx, namespace, name, messageID string, logicalOrder int64, compareOrder bool,
	msg store.SessionMessage, inputJSON, toolCallsJSON *string, metadataJSON string,
) (bool, error) {
	var storedOrder int64
	var storedTimestamp time.Time
	var storedRole, storedContent, storedSourceType, storedSourceRef, storedMetadata string
	var storedName, storedInput, storedToolCalls, storedToolCallID sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT sort_order, role, content, name, input, tool_calls, tool_call_id,
		source_type, source_ref, metadata_json, created_at FROM session_messages
		WHERE namespace = ? AND session_name = ? AND message_id = ?`, namespace, name, messageID).Scan(
		&storedOrder, &storedRole, &storedContent, &storedName, &storedInput, &storedToolCalls, &storedToolCallID,
		&storedSourceType, &storedSourceRef, &storedMetadata, &storedTimestamp,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return false, store.ErrConflict
	}
	if err != nil {
		return false, err
	}
	return (!compareOrder || storedOrder == logicalOrder) && storedRole == msg.Role && storedContent == msg.Content &&
		nullableStringMatches(storedName, nilIfEmpty(msg.Name)) && nullableStringMatches(storedInput, inputJSON) &&
		nullableStringMatches(storedToolCalls, toolCallsJSON) && nullableStringMatches(storedToolCallID, nilIfEmpty(msg.ToolCallID)) &&
		storedSourceType == msg.SourceType && storedSourceRef == msg.SourceRef && storedMetadata == metadataJSON &&
		(msg.Timestamp.IsZero() || storedTimestamp.Equal(msg.Timestamp)), nil
}

func nullableStringMatches(stored sql.NullString, expected *string) bool {
	if expected == nil {
		return !stored.Valid
	}
	return stored.Valid && stored.String == *expected
}

func encodeSessionMessagePayload(msg store.SessionMessage) (*string, *string, string, error) {
	var inputJSON, toolCallsJSON *string
	if msg.Input != nil {
		data, err := json.Marshal(msg.Input)
		if err != nil {
			return nil, nil, "", fmt.Errorf("failed to marshal input: %w", err)
		}
		encoded := string(data)
		inputJSON = &encoded
	}
	if msg.ToolCalls != nil {
		data, err := json.Marshal(msg.ToolCalls)
		if err != nil {
			return nil, nil, "", fmt.Errorf("failed to marshal tool_calls: %w", err)
		}
		encoded := string(data)
		toolCallsJSON = &encoded
	}
	metadataJSON := "{}"
	if len(msg.Metadata) > 0 {
		data, err := json.Marshal(msg.Metadata)
		if err != nil {
			return nil, nil, "", fmt.Errorf("failed to marshal message metadata: %w", err)
		}
		metadataJSON = string(data)
	}
	return inputJSON, toolCallsJSON, metadataJSON, nil
}

func nextSessionMessageOrderTx(ctx context.Context, tx *sql.Tx, namespace, name string) (int64, error) {
	var current int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sort_order), 0) FROM session_messages
		WHERE namespace = ? AND session_name = ?`, namespace, name).Scan(&current); err != nil {
		return 0, err
	}
	return current + 2, nil
}

// UpdateTokenCounts atomically increments the token counters for a session.
func (s *Store) UpdateTokenCounts(ctx context.Context, namespace, name string, inputTokens, outputTokens int) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET input_tokens = input_tokens + ?, output_tokens = output_tokens + ?, updated_at = ? WHERE namespace = ? AND name = ?`,
		inputTokens, outputTokens, time.Now(), namespace, name)
	return err
}

// nilIfEmpty returns nil if s is empty, otherwise returns a pointer to s.
func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
