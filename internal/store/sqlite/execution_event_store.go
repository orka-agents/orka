package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/store"
)

var _ store.ExecutionEventStore = (*Store)(nil)

// AppendExecutionEvent appends an execution event and allocates the next sequence
// number transactionally for its (namespace, stream_type, stream_id) stream.
func (s *Store) AppendExecutionEvent(ctx context.Context, event *store.ExecutionEvent) (*store.ExecutionEvent, error) {
	if event == nil {
		return nil, store.ValidationErrorf("execution event is required")
	}
	copy := cloneSQLiteExecutionEvent(*event)
	if err := normalizeSQLiteExecutionEvent(&copy); err != nil {
		return nil, err
	}
	if copy.CreatedAt.IsZero() {
		copy.CreatedAt = time.Now().UTC()
	} else {
		copy.CreatedAt = copy.CreatedAt.UTC()
	}

	contentJSON := nullableStringFromRaw(copy.Content)
	truncationJSON, err := marshalExecutionEventTruncation(copy.Truncation)
	if err != nil {
		return nil, err
	}

	s.executionEventMu.Lock()
	defer s.executionEventMu.Unlock()

	const maxAttempts = 8
	retryBackoffs := [...]time.Duration{
		10 * time.Millisecond,
		25 * time.Millisecond,
		50 * time.Millisecond,
		100 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
		800 * time.Millisecond,
	}
	var lastErr error
	for attempt := range maxAttempts {
		appended, err := s.appendExecutionEventOnce(ctx, copy, contentJSON, truncationJSON)
		if err == nil {
			return appended, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !isSQLiteRetryableError(err) && !isSQLiteConstraintError(err) {
			return nil, err
		}
		lastErr = err
		if attempt == maxAttempts-1 {
			break
		}
		timer := time.NewTimer(retryBackoffs[attempt])
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, lastErr
}

func (s *Store) appendExecutionEventOnce(ctx context.Context, event store.ExecutionEvent, contentJSON, truncationJSON any) (*store.ExecutionEvent, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close() //nolint:errcheck

	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()

	var latestSeq int64
	if err := conn.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), 0)
		 FROM execution_events
		 WHERE namespace = ? AND stream_type = ? AND stream_id = ?`,
		event.Namespace, event.StreamType, event.StreamID,
	).Scan(&latestSeq); err != nil {
		return nil, err
	}
	event.Seq = latestSeq + 1
	event.ID = executionEventID(event.Namespace, event.StreamType, event.StreamID, event.Seq)

	if _, err := conn.ExecContext(ctx,
		`INSERT INTO execution_events
		 (id, namespace, stream_type, stream_id, seq, type, severity, task_name, session_name,
		  agent_name, tool_name, tool_call_id, summary, content_json, content_text, truncation_json, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.ID, event.Namespace, event.StreamType, event.StreamID, event.Seq, event.Type, event.Severity,
		event.TaskName, event.SessionName, event.AgentName, event.ToolName, event.ToolCallID,
		event.Summary, contentJSON, event.ContentText, truncationJSON, event.CreatedAt,
	); err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return nil, err
	}
	committed = true
	return &event, nil
}

// ListExecutionEvents returns execution events matching filter in stream sequence order.
func (s *Store) ListExecutionEvents(ctx context.Context, filter store.ExecutionEventFilter) ([]store.ExecutionEvent, error) {
	filter = filter.Normalized()
	if err := filter.Validate(); err != nil {
		return nil, err
	}

	where := []string{"seq > ?"}
	args := []any{filter.AfterSeq}
	if filter.Namespace != "" {
		where = append(where, "namespace = ?")
		args = append(args, filter.Namespace)
	}
	if filter.StreamType != "" {
		where = append(where, "stream_type = ?")
		args = append(args, filter.StreamType)
	}
	if filter.StreamID != "" {
		where = append(where, "stream_id = ?")
		args = append(args, filter.StreamID)
	}
	if filter.TaskName != "" {
		where = append(where, "task_name = ?")
		args = append(args, filter.TaskName)
	}
	if filter.SessionName != "" {
		where = append(where, "session_name = ?")
		args = append(args, filter.SessionName)
	}
	if len(filter.EventTypes) > 0 {
		placeholders := make([]string, len(filter.EventTypes))
		for i, typ := range filter.EventTypes {
			placeholders[i] = "?"
			args = append(args, typ)
		}
		where = append(where, "type IN ("+strings.Join(placeholders, ",")+")")
	}
	args = append(args, filter.Limit)

	query := `SELECT id, namespace, stream_type, stream_id, seq, type, severity, task_name, session_name,
		agent_name, tool_name, tool_call_id, summary, content_json, content_text, truncation_json, created_at
		FROM execution_events
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY namespace ASC, stream_type ASC, stream_id ASC, seq ASC
		LIMIT ?`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	var out []store.ExecutionEvent
	for rows.Next() {
		event, err := scanExecutionEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, event)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// GetLatestExecutionEventSeq returns the latest sequence for a stream or zero when empty.
func (s *Store) GetLatestExecutionEventSeq(ctx context.Context, namespace, streamType, streamID string) (int64, error) {
	filter := store.ExecutionEventFilter{Namespace: namespace, StreamType: streamType, StreamID: streamID}.Normalized()
	if err := filter.Validate(); err != nil {
		return 0, err
	}
	var seq int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), 0)
		 FROM execution_events
		 WHERE namespace = ? AND stream_type = ? AND stream_id = ?`,
		filter.Namespace, filter.StreamType, filter.StreamID,
	).Scan(&seq)
	return seq, err
}

// DeleteExecutionEvents removes all execution events for one stream.
func (s *Store) DeleteExecutionEvents(ctx context.Context, namespace, streamType, streamID string) error {
	filter := store.ExecutionEventFilter{Namespace: namespace, StreamType: streamType, StreamID: streamID}.Normalized()
	if err := filter.Validate(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM execution_events WHERE namespace = ? AND stream_type = ? AND stream_id = ?`,
		filter.Namespace, filter.StreamType, filter.StreamID,
	)
	return err
}

type executionEventScanner interface {
	Scan(dest ...any) error
}

func scanExecutionEvent(scanner executionEventScanner) (store.ExecutionEvent, error) {
	var event store.ExecutionEvent
	var contentJSON sql.NullString
	var truncationJSON sql.NullString
	if err := scanner.Scan(
		&event.ID,
		&event.Namespace,
		&event.StreamType,
		&event.StreamID,
		&event.Seq,
		&event.Type,
		&event.Severity,
		&event.TaskName,
		&event.SessionName,
		&event.AgentName,
		&event.ToolName,
		&event.ToolCallID,
		&event.Summary,
		&contentJSON,
		&event.ContentText,
		&truncationJSON,
		&event.CreatedAt,
	); err != nil {
		return store.ExecutionEvent{}, err
	}
	if contentJSON.Valid && strings.TrimSpace(contentJSON.String) != "" {
		event.Content = json.RawMessage(contentJSON.String)
	}
	if truncationJSON.Valid && strings.TrimSpace(truncationJSON.String) != "" {
		var truncation events.ExecutionEventTruncation
		if err := json.Unmarshal([]byte(truncationJSON.String), &truncation); err != nil {
			return store.ExecutionEvent{}, fmt.Errorf("unmarshal execution event truncation metadata: %w", err)
		}
		event.Truncation = &truncation
	}
	return event, nil
}

func normalizeSQLiteExecutionEvent(event *store.ExecutionEvent) error {
	event.Namespace = strings.TrimSpace(event.Namespace)
	event.StreamType = strings.TrimSpace(event.StreamType)
	if event.StreamType == "" {
		event.StreamType = store.ExecutionEventStreamTypeTask
	}
	event.StreamID = strings.TrimSpace(event.StreamID)
	event.Type = strings.TrimSpace(event.Type)
	event.Severity = events.NormalizeExecutionEventSeverity(event.Severity)
	event.TaskName = strings.TrimSpace(event.TaskName)
	event.SessionName = strings.TrimSpace(event.SessionName)
	event.AgentName = strings.TrimSpace(event.AgentName)
	event.ToolName = strings.TrimSpace(event.ToolName)
	event.ToolCallID = strings.TrimSpace(event.ToolCallID)

	if event.Namespace == "" {
		return store.ValidationErrorf("execution event namespace is required")
	}
	if !events.IsValidExecutionEventStreamType(event.StreamType) {
		return store.ValidationErrorf("unsupported execution event stream type %q", event.StreamType)
	}
	if event.StreamID == "" {
		return store.ValidationErrorf("execution event stream id is required")
	}
	if !events.IsValidExecutionEventType(event.Type) {
		return store.ValidationErrorf("unsupported execution event type %q", event.Type)
	}
	if err := store.SanitizeExecutionEventPayloadFields(event); err != nil {
		return store.ValidationErrorf("invalid execution event payload: %v", err)
	}
	return nil
}

func cloneSQLiteExecutionEvent(event store.ExecutionEvent) store.ExecutionEvent {
	if event.Content != nil {
		event.Content = append(json.RawMessage(nil), event.Content...)
	}
	if event.Truncation != nil {
		truncation := *event.Truncation
		event.Truncation = &truncation
	}
	return event
}

func nullableStringFromRaw(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}

func marshalExecutionEventTruncation(truncation *events.ExecutionEventTruncation) (any, error) {
	if truncation == nil || truncation.Empty() {
		return nil, nil
	}
	data, err := json.Marshal(truncation)
	if err != nil {
		return nil, fmt.Errorf("marshal execution event truncation metadata: %w", err)
	}
	return string(data), nil
}

func executionEventID(namespace, streamType, streamID string, seq int64) string {
	return fmt.Sprintf("%s/%s/%s/%d", namespace, streamType, streamID, seq)
}
