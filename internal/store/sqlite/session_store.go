package sqlite

import (
	"context"
	"database/sql"
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

// ListSessions returns metadata for all sessions in a namespace.
func (s *Store) ListSessions(ctx context.Context, namespace string) ([]store.SessionMetadata, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, session_type, message_count, input_tokens, output_tokens, created_at, updated_at, active_task
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
		if err := rows.Scan(&m.Name, &m.SessionType, &m.MessageCount, &m.InputTokens, &m.OutputTokens, &m.CreatedAt, &m.UpdatedAt, &m.ActiveTask); err != nil {
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
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE namespace = ? AND name = ?`, namespace, name); err != nil {
		return err
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
func (s *Store) AcquireLock(ctx context.Context, namespace, name, taskName string) error {
	return s.AcquireTaskLock(ctx, namespace, name, taskName, "")
}

// AcquireTaskLock atomically records the immutable Task identity as lock owner.
func (s *Store) AcquireTaskLock(ctx context.Context, namespace, name, taskName, taskUID string) error {
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

	// Chat reservations and task locks are separate domains: tasks created by a
	// chat turn must still be able to run while that turn waits for them.
	result, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET active_task = ?, active_task_uid = ?
		 WHERE namespace = ? AND name = ?
		 AND (active_task = '' OR (active_task = ? AND active_task_uid = ''))`,
		taskName, taskUID, namespace, name, taskName,
	)
	if err != nil {
		return err
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return fmt.Errorf("%w: session %s/%s is already locked", store.ErrConflict, namespace, name)
	}
	return nil
}

// AcquireChatTurn creates the chat session when needed and atomically reserves
// it for one turn. Stale chat reservations may be reclaimed, but task locks are
// never stolen.
func (s *Store) AcquireChatTurn(ctx context.Context, session *store.SessionRecord, turnID string, expiresAt time.Time) error {
	if session == nil {
		return store.ValidationErrorf("session is required")
	}
	if session.Namespace == "" || session.Name == "" || turnID == "" {
		return store.ValidationErrorf("session namespace, name, and turn ID are required")
	}
	now := time.Now()
	if !expiresAt.After(now) {
		return store.ValidationErrorf("chat turn expiration must be in the future")
	}
	createdAt := session.CreatedAt
	if createdAt.IsZero() {
		createdAt = now
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO sessions (namespace, name, session_type, active_task, message_count, input_tokens, output_tokens, cancelled, created_at, updated_at)
		 VALUES (?, ?, ?, '', 0, 0, 0, ?, ?, ?)
		 ON CONFLICT(namespace, name) DO NOTHING`,
		session.Namespace, session.Name, session.SessionType, session.Cancelled, createdAt, now,
	); err != nil {
		return err
	}

	result, err := tx.ExecContext(ctx,
		`UPDATE sessions SET chat_turn_id = ?, chat_turn_expires_at = ?, updated_at = ?
		 WHERE namespace = ? AND name = ? AND active_task = ''
		 AND (chat_turn_id = '' OR chat_turn_expires_at IS NULL OR chat_turn_expires_at <= ?)`,
		turnID, expiresAt, now, session.Namespace, session.Name, now,
	)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("%w: chat session %s/%s already has an active turn", store.ErrConflict, session.Namespace, session.Name)
	}
	return tx.Commit()
}

// ReleaseChatTurn clears the chat reservation if it still belongs to turnID.
func (s *Store) ReleaseChatTurn(ctx context.Context, namespace, name, turnID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET chat_turn_id = '', chat_turn_expires_at = NULL WHERE namespace = ? AND name = ? AND chat_turn_id = ?`,
		namespace, name, turnID,
	)
	return err
}

// ReleaseLock clears the active_task if it matches the given task name.
func (s *Store) ReleaseLock(ctx context.Context, namespace, name, taskName string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET active_task = '', active_task_uid = '' WHERE namespace = ? AND name = ? AND active_task = ?`,
		namespace, name, taskName,
	)
	return err
}

// ReleaseTaskLock releases a task lock only for the same immutable Task UID.
func (s *Store) ReleaseTaskLock(ctx context.Context, namespace, name, taskName, taskUID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET active_task = '', active_task_uid = ''
		 WHERE namespace = ? AND name = ? AND active_task = ?
		 AND (active_task_uid = ? OR active_task_uid = '')`,
		namespace, name, taskName, taskUID,
	)
	return err
}

// IsLocked returns true if the session is locked by a task other than currentTask.
func (s *Store) IsLocked(ctx context.Context, namespace, name, currentTask string) (bool, error) {
	var activeTask string
	err := s.db.QueryRowContext(ctx,
		`SELECT active_task FROM sessions WHERE namespace = ? AND name = ?`,
		namespace, name,
	).Scan(&activeTask)
	if errors.Is(err, sql.ErrNoRows) {
		return false, store.ErrNotFound
	}
	if err != nil {
		return false, err
	}
	return activeTask != "" && activeTask != currentTask, nil
}

// IsTaskLocked returns true unless the lock is unowned, belongs to the exact
// immutable Task identity, or is a same-name legacy lock whose UID can be
// atomically backfilled by AcquireTaskLock.
func (s *Store) IsTaskLocked(ctx context.Context, namespace, name, taskName, taskUID string) (bool, error) {
	var activeTask, activeTaskUID string
	err := s.db.QueryRowContext(ctx,
		`SELECT active_task, active_task_uid FROM sessions WHERE namespace = ? AND name = ?`,
		namespace, name,
	).Scan(&activeTask, &activeTaskUID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, store.ErrNotFound
	}
	if err != nil {
		return false, err
	}
	if activeTask == "" {
		return false, nil
	}
	if activeTask != taskName {
		return true, nil
	}
	if activeTaskUID == "" {
		return false, nil
	}
	return activeTaskUID != taskUID, nil
}

// AppendMessages inserts messages into a session's transcript and updates the session metadata.
func (s *Store) AppendMessages(ctx context.Context, namespace, name string, messages []store.SessionMessage) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	if err := insertSessionMessages(ctx, tx, namespace, name, messages); err != nil {
		return err
	}

	_, err = tx.ExecContext(ctx,
		`UPDATE sessions SET message_count = message_count + ?, updated_at = CURRENT_TIMESTAMP WHERE namespace = ? AND name = ?`,
		len(messages), namespace, name,
	)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// FinalizeTaskSession atomically appends one task's terminal transcript and
// releases the task lock. Replays after a crash are idempotent because an
// already-released or differently-owned session does not append again.
func (s *Store) FinalizeTaskSession(ctx context.Context, namespace, name, taskName, taskUID string, messages []store.SessionMessage) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	var activeTask, activeTaskUID string
	if err := tx.QueryRowContext(ctx,
		`SELECT active_task, active_task_uid FROM sessions WHERE namespace = ? AND name = ?`,
		namespace, name,
	).Scan(&activeTask, &activeTaskUID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if activeTask != taskName || activeTaskUID != taskUID {
		if activeTask != taskName || activeTaskUID != "" || taskUID == "" {
			return nil
		}
		result, err := tx.ExecContext(ctx,
			`UPDATE sessions SET active_task_uid = ?
			 WHERE namespace = ? AND name = ? AND active_task = ? AND active_task_uid = ''`,
			taskUID, namespace, name, taskName,
		)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows != 1 {
			return store.ErrConflict
		}
	}
	if err := insertSessionMessages(ctx, tx, namespace, name, messages); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE sessions
		 SET active_task = '', active_task_uid = '', message_count = message_count + ?, updated_at = CURRENT_TIMESTAMP
		 WHERE namespace = ? AND name = ? AND active_task = ? AND active_task_uid = ?`,
		len(messages), namespace, name, taskName, taskUID,
	)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return store.ErrConflict
	}
	return tx.Commit()
}

// CommitSessionTurn atomically creates the session when absent, appends the
// supplied turn messages, and increments token usage. expectedMessageCount is
// an optimistic guard for the session revision reserved by the chat turn lock.
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
	if session.Namespace == "" || session.Name == "" {
		return store.ValidationErrorf("session namespace and name are required")
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

	now := time.Now()
	createdAt := session.CreatedAt
	if createdAt.IsZero() {
		createdAt = now
	}
	updatedAt := session.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = now
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO sessions (namespace, name, session_type, active_task, message_count, input_tokens, output_tokens, cancelled, created_at, updated_at)
		 VALUES (?, ?, ?, ?, 0, 0, 0, ?, ?, ?)
		 ON CONFLICT(namespace, name) DO NOTHING`,
		session.Namespace, session.Name, session.SessionType, session.ActiveTask,
		session.Cancelled, createdAt, updatedAt,
	); err != nil {
		return err
	}

	var currentMessageCount int
	var activeTurn string
	var activeTurnExpiresAt sql.NullTime
	if err := tx.QueryRowContext(ctx,
		`SELECT message_count, chat_turn_id, chat_turn_expires_at FROM sessions WHERE namespace = ? AND name = ?`,
		session.Namespace, session.Name,
	).Scan(&currentMessageCount, &activeTurn, &activeTurnExpiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrNotFound
		}
		return err
	}
	if activeTurn != turnID {
		return fmt.Errorf("%w: chat turn no longer owns session", store.ErrConflict)
	}
	if turnID != "" && (!activeTurnExpiresAt.Valid || !activeTurnExpiresAt.Time.After(now)) {
		return fmt.Errorf("%w: chat turn reservation expired", store.ErrConflict)
	}
	if currentMessageCount != expectedMessageCount {
		return fmt.Errorf("%w: session message count is %d, expected %d", store.ErrConflict, currentMessageCount, expectedMessageCount)
	}

	if err := insertSessionMessages(ctx, tx, session.Namespace, session.Name, messages); err != nil {
		return err
	}

	result, err := tx.ExecContext(ctx,
		`UPDATE sessions
		 SET message_count = message_count + ?, "input_tokens" = "input_tokens" + ?, "output_tokens" = "output_tokens" + ?, updated_at = ?
		 WHERE namespace = ? AND name = ? AND message_count = ? AND chat_turn_id = ?
		 AND (chat_turn_id = '' OR chat_turn_expires_at > ?)`,
		len(messages), inputTokens, outputTokens, now,
		session.Namespace, session.Name, expectedMessageCount, turnID, now,
	)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("%w: session changed while committing turn", store.ErrConflict)
	}

	return tx.Commit()
}

func insertSessionMessages(ctx context.Context, tx *sql.Tx, namespace, name string, messages []store.SessionMessage) error {
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO session_messages (namespace, session_name, role, content, name, input, tool_calls, tool_call_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
	)
	if err != nil {
		return err
	}
	defer stmt.Close() //nolint:errcheck

	for _, msg := range messages {
		var inputJSON, toolCallsJSON *string
		if msg.Input != nil {
			b, err := json.Marshal(msg.Input)
			if err != nil {
				return fmt.Errorf("failed to marshal input: %w", err)
			}
			s := string(b)
			inputJSON = &s
		}
		if msg.ToolCalls != nil {
			b, err := json.Marshal(msg.ToolCalls)
			if err != nil {
				return fmt.Errorf("failed to marshal tool_calls: %w", err)
			}
			s := string(b)
			toolCallsJSON = &s
		}

		ts := msg.Timestamp
		if ts.IsZero() {
			ts = time.Now()
		}

		if _, err := stmt.ExecContext(ctx, namespace, name, msg.Role, msg.Content, nilIfEmpty(msg.Name), inputJSON, toolCallsJSON, nilIfEmpty(msg.ToolCallID), ts); err != nil {
			return err
		}
	}
	return nil
}

// LoadTranscript retrieves session messages ordered by ID. If maxMessages > 0, limits results.
func (s *Store) LoadTranscript(ctx context.Context, namespace, name string, maxMessages int) ([]store.SessionMessage, error) {
	query := `SELECT role, content, name, input, tool_calls, tool_call_id, created_at
		 FROM session_messages WHERE namespace = ? AND session_name = ? ORDER BY id`
	args := []any{namespace, name}

	if maxMessages > 0 {
		query += ` LIMIT ?`
		args = append(args, maxMessages)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	var messages []store.SessionMessage
	for rows.Next() {
		var msg store.SessionMessage
		var nameStr, inputJSON, toolCallsJSON, toolCallID sql.NullString
		if err := rows.Scan(&msg.Role, &msg.Content, &nameStr, &inputJSON, &toolCallsJSON, &toolCallID, &msg.Timestamp); err != nil {
			return nil, err
		}

		msg.Name = nameStr.String
		msg.ToolCallID = toolCallID.String

		if inputJSON.Valid && inputJSON.String != "" {
			if err := json.Unmarshal([]byte(inputJSON.String), &msg.Input); err != nil {
				return nil, fmt.Errorf("failed to unmarshal input: %w", err)
			}
		}
		if toolCallsJSON.Valid && toolCallsJSON.String != "" {
			if err := json.Unmarshal([]byte(toolCallsJSON.String), &msg.ToolCalls); err != nil {
				return nil, fmt.Errorf("failed to unmarshal tool_calls: %w", err)
			}
		}

		messages = append(messages, msg)
	}
	return messages, rows.Err()
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
