package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/sozercan/orka/internal/store"
)

// CreateSession inserts a new session record.
func (s *Store) CreateSession(ctx context.Context, session *store.SessionRecord) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (namespace, name, session_type, active_task, message_count, input_tokens, output_tokens, cancelled, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		session.Namespace, session.Name, session.SessionType, session.ActiveTask,
		session.MessageCount, session.InputTokens, session.OutputTokens, session.Cancelled,
		session.CreatedAt, session.UpdatedAt,
	)
	return err
}

// GetSession loads a session with all its messages.
func (s *Store) GetSession(ctx context.Context, namespace, name string) (*store.SessionRecord, error) {
	session := &store.SessionRecord{}
	err := s.db.QueryRowContext(ctx,
		`SELECT namespace, name, session_type, active_task, message_count, input_tokens, output_tokens, cancelled, created_at, updated_at
		 FROM sessions WHERE namespace = ? AND name = ?`,
		namespace, name,
	).Scan(
		&session.Namespace, &session.Name, &session.SessionType, &session.ActiveTask,
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
	var activeTask string
	err = tx.QueryRowContext(ctx,
		`SELECT active_task FROM sessions WHERE namespace = ? AND name = ?`,
		namespace,
		name,
	).Scan(&activeTask)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if activeTask != "" {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO execution_event_deleted_session_tasks(namespace, session_name, task_name)
			 VALUES (?, ?, ?)`,
			namespace,
			name,
			activeTask,
		); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO execution_event_deleted_session_tasks(namespace, session_name, task_name)
		 SELECT DISTINCT namespace, session_name, task_name
		 FROM execution_events
		 WHERE namespace = ? AND session_name = ? AND task_name <> ''`,
		namespace,
		name,
	); err != nil {
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
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE namespace = ? AND name = ?`, namespace, name); err != nil {
		return err
	}
	return tx.Commit()
}

// AcquireLock atomically sets the active_task for a session.
// Returns store.ErrNotFound if the session does not exist, or an error if already locked.
func (s *Store) AcquireLock(ctx context.Context, namespace, name, taskName string) error {
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

	// Try to acquire lock
	result, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET active_task = ? WHERE namespace = ? AND name = ? AND active_task = ''`,
		taskName, namespace, name,
	)
	if err != nil {
		return err
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return fmt.Errorf("session %s/%s is already locked", namespace, name)
	}
	return nil
}

// ReleaseLock clears the active_task if it matches the given task name.
func (s *Store) ReleaseLock(ctx context.Context, namespace, name, taskName string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET active_task = '' WHERE namespace = ? AND name = ? AND active_task = ?`,
		namespace, name, taskName,
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

// AppendMessages inserts messages into a session's transcript and updates the session metadata.
func (s *Store) AppendMessages(ctx context.Context, namespace, name string, messages []store.SessionMessage) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

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

	_, err = tx.ExecContext(ctx,
		`UPDATE sessions SET message_count = message_count + ?, updated_at = CURRENT_TIMESTAMP WHERE namespace = ? AND name = ?`,
		len(messages), namespace, name,
	)
	if err != nil {
		return err
	}

	return tx.Commit()
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
