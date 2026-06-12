package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/metrics"
	"github.com/sozercan/orka/internal/store"
)

const (
	sqliteUnknownMetricLabel = "unknown"
	sqliteInvalidMetricLabel = "invalid"
)

var _ store.ExecutionEventStore = (*Store)(nil)

// AppendExecutionEvent appends an execution event and allocates the next sequence
// number transactionally for its (namespace, stream_type, stream_id) stream.
func (s *Store) AppendExecutionEvent(ctx context.Context, event *store.ExecutionEvent) (*store.ExecutionEvent, error) {
	started := time.Now()
	metricStreamType, metricEventType := sqliteMetricLabelsForExecutionEvent(event)
	success := false
	defer func() {
		metrics.RecordExecutionEventAppend(metricStreamType, metricEventType, success, time.Since(started).Seconds())
	}()
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
			success = true
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

	if err := ensureNoApprovalDecision(ctx, conn, event); err != nil {
		return nil, err
	}

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
	if event.SessionName != "" {
		var latestSessionSeq int64
		seqErr := conn.QueryRowContext(ctx,
			`SELECT latest_seq
			 FROM execution_event_session_sequences
			 WHERE namespace = ? AND session_name = ?`,
			event.Namespace, event.SessionName,
		).Scan(&latestSessionSeq)
		if seqErr != nil && seqErr != sql.ErrNoRows {
			return nil, seqErr
		}
		event.SessionSeq = latestSessionSeq + 1
		if seqErr == sql.ErrNoRows {
			if _, err := conn.ExecContext(ctx,
				`INSERT INTO execution_event_session_sequences(namespace, session_name, latest_seq) VALUES (?, ?, ?)`,
				event.Namespace, event.SessionName, event.SessionSeq,
			); err != nil {
				return nil, err
			}
		} else if _, err := conn.ExecContext(ctx,
			`UPDATE execution_event_session_sequences
			 SET latest_seq = ?
			 WHERE namespace = ? AND session_name = ?`,
			event.SessionSeq, event.Namespace, event.SessionName,
		); err != nil {
			return nil, err
		}
	}
	event.ID = executionEventID(event.Namespace, event.StreamType, event.StreamID, event.Seq)

	if _, err := conn.ExecContext(ctx,
		`INSERT INTO execution_events
		 (id, namespace, stream_type, stream_id, seq, session_seq, type, severity, task_name, session_name,
		  agent_name, tool_name, tool_call_id, summary, content_json, content_text, truncation_json, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.ID, event.Namespace, event.StreamType, event.StreamID, event.Seq, event.SessionSeq, event.Type, event.Severity,
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

type executionEventQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func ensureNoApprovalDecision(ctx context.Context, q executionEventQueryer, event store.ExecutionEvent) error {
	approvalID, ok := store.ApprovalDecisionID(event)
	if !ok {
		return nil
	}
	rows, err := q.QueryContext(ctx, `SELECT type, tool_call_id, content_json
		FROM execution_events
		WHERE namespace = ? AND stream_type = ? AND stream_id = ?
		  AND type IN (?, ?, ?, ?)`,
		event.Namespace,
		event.StreamType,
		event.StreamID,
		events.ExecutionEventTypeApprovalApproved,
		events.ExecutionEventTypeApprovalDeclined,
		events.ExecutionEventTypeApprovalExpired,
		events.ExecutionEventTypeApprovalCancelled,
	)
	if err != nil {
		return err
	}
	defer rows.Close() //nolint:errcheck
	for rows.Next() {
		var existing store.ExecutionEvent
		var contentJSON sql.NullString
		if err := rows.Scan(&existing.Type, &existing.ToolCallID, &contentJSON); err != nil {
			return err
		}
		if contentJSON.Valid && strings.TrimSpace(contentJSON.String) != "" {
			existing.Content = json.RawMessage(contentJSON.String)
		}
		existingApprovalID, ok := store.ApprovalDecisionID(existing)
		if ok && existingApprovalID == approvalID {
			return fmt.Errorf("%w: approval %s already has a terminal decision", store.ErrConflict, approvalID)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return nil
}

// ListExecutionEvents returns execution events matching filter in stream sequence order.
func (s *Store) ListExecutionEvents(ctx context.Context, filter store.ExecutionEventFilter) ([]store.ExecutionEvent, error) {
	started := time.Now()
	success := false
	defer func() {
		metrics.RecordExecutionEventList("task_store", success, time.Since(started).Seconds())
	}()
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
	success = true
	return out, nil
}

// ListSessionExecutionEvents returns task-derived events for one session with a deterministic session cursor.
func (s *Store) ListSessionExecutionEvents(ctx context.Context, filter store.SessionExecutionEventFilter) ([]store.SessionExecutionEvent, int64, error) {
	started := time.Now()
	success := false
	defer func() {
		metrics.RecordExecutionEventList("session_store", success, time.Since(started).Seconds())
	}()
	filter = filter.Normalized()
	if err := filter.Validate(); err != nil {
		return nil, 0, err
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = tx.Rollback() }()

	var latestSeq int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(latest_seq, 0)
		 FROM execution_event_session_sequences
		 WHERE namespace = ? AND session_name = ?`,
		filter.Namespace,
		filter.SessionName,
	).Scan(&latestSeq); err != nil {
		if err != sql.ErrNoRows {
			return nil, 0, err
		}
	}

	where := []string{"namespace = ?", "session_name = ?", "session_seq > ?"}
	args := []any{filter.Namespace, filter.SessionName, filter.AfterSeq}
	if len(filter.EventTypes) > 0 {
		placeholders := make([]string, len(filter.EventTypes))
		for i, typ := range filter.EventTypes {
			placeholders[i] = "?"
			args = append(args, typ)
		}
		where = append(where, "type IN ("+strings.Join(placeholders, ",")+")")
	}
	args = append(args, filter.Limit)

	query := `SELECT session_seq, id, namespace, stream_type, stream_id, seq, type, severity, task_name, session_name,
		agent_name, tool_name, tool_call_id, summary, content_json, content_text, truncation_json, created_at
	FROM execution_events
	WHERE ` + strings.Join(where, " AND ") + `
	ORDER BY session_seq ASC
	LIMIT ?`
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close() //nolint:errcheck

	out := []store.SessionExecutionEvent{}
	for rows.Next() {
		event, err := scanSessionExecutionEvent(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, event)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	if len(out) > 0 && latestSeq < out[len(out)-1].SessionSeq {
		latestSeq = out[len(out)-1].SessionSeq
	}
	success = true
	return out, latestSeq, nil
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

func scanSessionExecutionEvent(scanner executionEventScanner) (store.SessionExecutionEvent, error) {
	var sessionSeq int64
	var event store.ExecutionEvent
	var contentJSON sql.NullString
	var truncationJSON sql.NullString
	if err := scanner.Scan(
		&sessionSeq,
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
		return store.SessionExecutionEvent{}, err
	}
	if contentJSON.Valid && strings.TrimSpace(contentJSON.String) != "" {
		event.Content = json.RawMessage(contentJSON.String)
	}
	if truncationJSON.Valid && strings.TrimSpace(truncationJSON.String) != "" {
		var truncation events.ExecutionEventTruncation
		if err := json.Unmarshal([]byte(truncationJSON.String), &truncation); err != nil {
			return store.SessionExecutionEvent{}, fmt.Errorf("unmarshal execution event truncation metadata: %w", err)
		}
		event.Truncation = &truncation
	}
	return store.SessionExecutionEvent{
		ExecutionEvent: event,
		SessionSeq:     sessionSeq,
		TaskSeq:        event.Seq,
	}, nil
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

func sqliteMetricLabelsForExecutionEvent(event *store.ExecutionEvent) (string, string) {
	if event == nil {
		return sqliteUnknownMetricLabel, sqliteUnknownMetricLabel
	}
	streamType := strings.TrimSpace(event.StreamType)
	if streamType == "" {
		streamType = store.ExecutionEventStreamTypeTask
	}
	if !events.IsValidExecutionEventStreamType(streamType) {
		streamType = sqliteInvalidMetricLabel
	}
	eventType := strings.TrimSpace(event.Type)
	if eventType == "" {
		eventType = sqliteUnknownMetricLabel
	} else if !events.IsValidExecutionEventType(eventType) {
		eventType = sqliteInvalidMetricLabel
	}
	return streamType, eventType
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
