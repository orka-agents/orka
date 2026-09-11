package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/redact"
	"github.com/orka-agents/orka/internal/sessioncontext"
	"github.com/orka-agents/orka/internal/store"
)

var _ store.SessionContextStore = (*Store)(nil)

// AppendContextMessages commits previews and their recoverable source data in
// one transaction. It never creates a Session or adopts an unfenced owner.
func (s *Store) AppendContextMessages(
	ctx context.Context, write store.SessionContextWrite, messages []store.SessionMessage,
) ([]store.SessionMessage, error) {
	if err := write.Validate(); err != nil {
		return nil, err
	}
	// A pinned reader cannot extend canonical history beyond its boundary.
	// The initial checkpoint worker path requires an appendable Session.
	if write.ThroughMessageID != "" {
		return nil, store.ValidationErrorf("session context appends require an unbounded appendable Session")
	}
	prepared := make([]store.SessionMessage, len(messages))
	historyPages := make(map[int]store.SessionHistoryResult)
	for i, message := range messages {
		var err error
		prepared[i], err = sanitizeSessionContextMessage(message)
		if err != nil {
			return nil, err
		}
		if message.Metadata[store.SessionContextOutputRefKey] == message.ID {
			// Only an exact existing preview can use this reserved reference;
			// matchSessionContextMessageTx checks it before accepting the retry.
			prepared[i].Content = message.Content
		}
		if page, ok := sessioncontext.HistoryPage(message.Content); ok {
			// Copies retain receipt semantics in every role. The transaction
			// verifies every byte against saved source data before bypassing text
			// redaction. Encode only decoded fields so duplicate JSON members
			// cannot retain unverified text from the original envelope.
			canonical, err := json.Marshal(page)
			if err != nil {
				return nil, err
			}
			prepared[i].Content = string(canonical)
			historyPages[i] = page
		} else if message.Role == "tool" && strings.TrimSpace(message.Name) == sessioncontext.HistoryToolName &&
			json.Valid([]byte(message.Content)) {
			return nil, store.ValidationErrorf("history tool result contains an invalid page")
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := verifySessionContextWriteTx(ctx, tx, write); err != nil {
		return nil, err
	}
	canonical := make([]store.SessionMessage, len(prepared))
	inserted := 0
	for i, message := range prepared {
		if page, ok := historyPages[i]; ok {
			if err := verifySessionHistoryPageTx(ctx, tx, write, page); err != nil {
				return nil, err
			}
		}
		var added bool
		canonical[i], added, err = appendSessionContextMessageTx(ctx, tx, write, message)
		if err != nil {
			return nil, err
		}
		if added {
			inserted++
		}
	}
	if inserted > 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE sessions SET message_count = message_count + ?, updated_at = CURRENT_TIMESTAMP
			 WHERE namespace = ? AND name = ?`, inserted, write.Namespace, write.SessionName,
		); err != nil {
			return nil, err
		}
	}
	if err := verifySessionWriteLockTx(ctx, tx, write.Namespace, write.SessionName, write.OwnerName, write.OwnerUID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return canonical, nil
}

func appendSessionContextMessageTx(
	ctx context.Context, tx *sql.Tx, write store.SessionContextWrite, message store.SessionMessage,
) (store.SessionMessage, bool, error) {
	existing, err := loadSessionContextMessageTx(ctx, tx, write.Namespace, write.SessionName, message.ID, 0)
	if err == nil {
		canonical, err := matchSessionContextMessageTx(ctx, tx, write, message, existing)
		return canonical, false, err
	}
	if !errors.Is(err, store.ErrNotFound) {
		return store.SessionMessage{}, false, err
	}
	if _, reserved := message.Metadata[store.SessionContextOutputRefKey]; reserved {
		return store.SessionMessage{}, false, store.ValidationErrorf("context output references are assigned by the store")
	}
	order, err := nextSessionMessageOrderTx(ctx, tx, write.Namespace, write.SessionName)
	if err != nil {
		return store.SessionMessage{}, false, err
	}
	if message.Order != 0 && message.Order != order {
		return store.SessionMessage{}, false, store.ValidationErrorf("new context messages must follow existing Session history")
	}
	message.Order = order
	if message.Timestamp.IsZero() {
		message.Timestamp = time.Now().UTC()
	} else {
		message.Timestamp = message.Timestamp.UTC()
	}
	data, err := encodeSessionContextMessage(message)
	if err != nil {
		return store.SessionMessage{}, false, err
	}
	preview := sessionContextPreview(message)
	input, calls, metadata, err := encodeSessionMessagePayload(preview)
	if err != nil {
		return store.SessionMessage{}, false, err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO session_messages
		 (namespace, session_name, message_id, sort_order, role, content, name, input, tool_calls,
		  tool_call_id, source_type, source_ref, metadata_json, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		write.Namespace, write.SessionName, preview.ID, preview.Order, preview.Role, preview.Content,
		nilIfEmpty(preview.Name), input, calls, nilIfEmpty(preview.ToolCallID), preview.SourceType,
		preview.SourceRef, metadata, preview.Timestamp,
	)
	if err != nil {
		return store.SessionMessage{}, false, err
	}
	if preview.Metadata[store.SessionContextOutputRefKey] != "" {
		if err := insertSessionContextOutputTx(ctx, tx, write, message.ID, data); err != nil {
			return store.SessionMessage{}, false, err
		}
	}
	return preview, true, nil
}

func matchSessionContextMessageTx(
	ctx context.Context, tx *sql.Tx, write store.SessionContextWrite, message, existing store.SessionMessage,
) (store.SessionMessage, error) {
	if (message.Order != 0 && message.Order != existing.Order) ||
		(!message.Timestamp.IsZero() && !message.Timestamp.Equal(existing.Timestamp)) {
		return store.SessionMessage{}, store.ErrDuplicateMismatch
	}
	message.Order = existing.Order
	message.Timestamp = existing.Timestamp
	data, err := encodeSessionContextMessage(message)
	if err != nil {
		return store.SessionMessage{}, err
	}
	original, err := sessionContextMessageDataTx(ctx, tx, write.Namespace, write.SessionName, existing)
	if err != nil {
		return store.SessionMessage{}, err
	}
	if !bytes.Equal(original, data) {
		// Returning a canonical preview must not make its next save ambiguous.
		// Only an exact retry of the stored preview can stand in for the original.
		previewData, err := encodeSessionContextMessage(existing)
		if err != nil {
			return store.SessionMessage{}, err
		}
		if message.Metadata[store.SessionContextOutputRefKey] != existing.ID || !bytes.Equal(previewData, data) {
			return store.SessionMessage{}, store.ErrDuplicateMismatch
		}
	}
	if len(existing.Content) <= store.MaxSessionContextPreviewBytes {
		return existing, nil
	}
	// An exact retry of a legacy, full transcript entry can add recoverable
	// output storage without adding another canonical message.
	preview := sessionContextPreview(existing)
	_, _, metadata, err := encodeSessionMessagePayload(preview)
	if err != nil {
		return store.SessionMessage{}, err
	}
	if err := insertSessionContextOutputTx(ctx, tx, write, existing.ID, original); err != nil {
		return store.SessionMessage{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE session_messages SET content = ?, metadata_json = ?
		 WHERE namespace = ? AND session_name = ? AND message_id = ?`,
		preview.Content, metadata, write.Namespace, write.SessionName, existing.ID,
	); err != nil {
		return store.SessionMessage{}, err
	}
	return preview, nil
}

func insertSessionContextOutputTx(ctx context.Context, tx *sql.Tx, write store.SessionContextWrite, id string, data []byte) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO session_context_outputs(namespace, session_name, message_id, message_json) VALUES (?, ?, ?, ?)`,
		write.Namespace, write.SessionName, id, data,
	)
	return err
}

// SaveSessionCheckpoint verifies every reference against committed Session
// history and fences both the insert and retention cleanup to the active owner.
func (s *Store) SaveSessionCheckpoint(ctx context.Context, write store.SessionContextWrite, checkpoint store.SessionCheckpoint) error {
	if err := write.Validate(); err != nil {
		return err
	}
	if (checkpoint.Namespace != "" && checkpoint.Namespace != write.Namespace) ||
		(checkpoint.SessionName != "" && checkpoint.SessionName != write.SessionName) {
		return store.ValidationErrorf("checkpoint identity does not match its Session")
	}
	checkpoint.Namespace, checkpoint.SessionName = write.Namespace, write.SessionName
	if err := checkpoint.Validate(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := verifySessionContextWriteTx(ctx, tx, write); err != nil {
		return err
	}
	throughOrder, err := sessionContextBoundaryTx(ctx, tx, write.Namespace, write.SessionName, write.ThroughMessageID)
	if err != nil {
		return err
	}
	if err := validateSessionCheckpointSourcesTx(ctx, tx, &checkpoint, throughOrder); err != nil {
		return err
	}
	existing, err := scanSessionCheckpoint(tx.QueryRowContext(ctx,
		`SELECT id, namespace, session_name, format_version, last_message_id, last_message_order,
		 note, source_ids_json, created_at FROM session_checkpoints WHERE namespace = ? AND session_name = ? AND id = ?`,
		write.Namespace, write.SessionName, checkpoint.ID,
	))
	if err == nil {
		if !sessionCheckpointsMatch(*existing, checkpoint) {
			return store.ErrDuplicateMismatch
		}
		return verifySessionWriteLockTx(ctx, tx, write.Namespace, write.SessionName, write.OwnerName, write.OwnerUID)
	}
	if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	// Timestamps supplied by a caller may verify a retry, but cannot influence
	// which new checkpoint is retained or loaded.
	checkpoint.CreatedAt = time.Now().UTC()
	sources, err := json.Marshal(checkpoint.SourceMessageIDs)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO session_checkpoints
		 (namespace, session_name, id, format_version, last_message_id, last_message_order, note, source_ids_json, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		write.Namespace, write.SessionName, checkpoint.ID, checkpoint.Version, checkpoint.LastMessageID,
		checkpoint.LastMessageOrder, checkpoint.Note, string(sources), checkpoint.CreatedAt,
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM session_checkpoints WHERE namespace = ? AND session_name = ? AND id NOT IN (
		   SELECT id FROM session_checkpoints WHERE namespace = ? AND session_name = ?
		   ORDER BY last_message_order DESC, created_at DESC, id DESC LIMIT ?
		 )`, write.Namespace, write.SessionName, write.Namespace, write.SessionName, store.MaxSessionCheckpoints,
	); err != nil {
		return err
	}
	if err := verifySessionWriteLockTx(ctx, tx, write.Namespace, write.SessionName, write.OwnerName, write.OwnerUID); err != nil {
		return err
	}
	return tx.Commit()
}

// LoadSessionCheckpoint never creates missing Session data during recovery.
func (s *Store) LoadSessionCheckpoint(
	ctx context.Context, namespace, sessionName, throughMessageID string,
) (*store.SessionCheckpoint, error) {
	if err := validateSessionContextReadIdentity(namespace, sessionName, throughMessageID); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, _, err := sessionContextSessionTx(ctx, tx, namespace, sessionName); err != nil {
		return nil, err
	}
	throughOrder, err := sessionContextBoundaryTx(ctx, tx, namespace, sessionName, throughMessageID)
	if err != nil {
		return nil, err
	}
	checkpoint, err := scanSessionCheckpoint(tx.QueryRowContext(ctx,
		`SELECT id, namespace, session_name, format_version, last_message_id, last_message_order,
		 note, source_ids_json, created_at FROM session_checkpoints
		 WHERE namespace = ? AND session_name = ? AND (? = 0 OR last_message_order <= ?)
		 ORDER BY last_message_order DESC, created_at DESC, id DESC LIMIT 1`,
		namespace, sessionName, throughOrder, throughOrder,
	))
	if err != nil {
		return nil, err
	}
	if err := checkpoint.Validate(); err != nil {
		return nil, err
	}
	if err := validateSessionCheckpointSourcesTx(ctx, tx, checkpoint, throughOrder); err != nil {
		return nil, err
	}
	return checkpoint, nil
}

func validateSessionCheckpointSourcesTx(ctx context.Context, tx *sql.Tx, checkpoint *store.SessionCheckpoint, throughOrder int64) error {
	last, err := loadSessionContextMessageTx(ctx, tx, checkpoint.Namespace, checkpoint.SessionName, checkpoint.LastMessageID, throughOrder)
	if err != nil {
		return err
	}
	if checkpoint.LastMessageOrder != 0 && checkpoint.LastMessageOrder != last.Order {
		return store.ValidationErrorf("checkpoint last message order does not match its saved message")
	}
	checkpoint.LastMessageOrder = last.Order
	if err := verifySessionContextOutputTx(ctx, tx, checkpoint.Namespace, checkpoint.SessionName, last); err != nil {
		return err
	}
	for _, id := range checkpoint.SourceMessageIDs {
		source, err := loadSessionContextMessageTx(ctx, tx, checkpoint.Namespace, checkpoint.SessionName, id, last.Order)
		if err != nil {
			return err
		}
		if err := verifySessionContextOutputTx(ctx, tx, checkpoint.Namespace, checkpoint.SessionName, source); err != nil {
			return err
		}
	}
	return nil
}

// A preview alone is not a recoverable source. Checking existence avoids
// reading up to 2 MiB of output per source merely to save or load a note.
func verifySessionContextOutputTx(ctx context.Context, tx *sql.Tx, namespace, sessionName string, message store.SessionMessage) error {
	ref := message.Metadata[store.SessionContextOutputRefKey]
	if ref == "" {
		return nil
	}
	if ref != message.ID {
		return store.ErrNotFound
	}
	var exists bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM session_context_outputs WHERE namespace = ? AND session_name = ? AND message_id = ?)`,
		namespace, sessionName, message.ID,
	).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return store.ErrNotFound
	}
	return nil
}

func sessionCheckpointsMatch(existing, candidate store.SessionCheckpoint) bool {
	return existing.ID == candidate.ID && existing.Version == candidate.Version &&
		existing.Namespace == candidate.Namespace && existing.SessionName == candidate.SessionName &&
		existing.LastMessageID == candidate.LastMessageID && existing.LastMessageOrder == candidate.LastMessageOrder &&
		existing.Note == candidate.Note && slices.Equal(existing.SourceMessageIDs, candidate.SourceMessageIDs) &&
		(candidate.CreatedAt.IsZero() || candidate.CreatedAt.Equal(existing.CreatedAt))
}

func scanSessionCheckpoint(row gatewayRowScanner) (*store.SessionCheckpoint, error) {
	checkpoint := &store.SessionCheckpoint{}
	var sources string
	err := row.Scan(&checkpoint.ID, &checkpoint.Namespace, &checkpoint.SessionName, &checkpoint.Version,
		&checkpoint.LastMessageID, &checkpoint.LastMessageOrder, &checkpoint.Note, &sources, &checkpoint.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(sources), &checkpoint.SourceMessageIDs); err != nil {
		return nil, fmt.Errorf("decode session checkpoint source IDs: %w", err)
	}
	return checkpoint, nil
}

// ReadSessionHistory reads the caller's byte range inside a single snapshot of
// the Session identity, read boundary, message, and saved output.
func (s *Store) ReadSessionHistory(ctx context.Context, read store.SessionHistoryRead) (*store.SessionHistoryResult, error) {
	if err := read.Validate(); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, _, err := sessionContextSessionTx(ctx, tx, read.Namespace, read.SessionName); err != nil {
		return nil, err
	}
	throughOrder, err := sessionContextBoundaryTx(ctx, tx, read.Namespace, read.SessionName, read.ThroughMessageID)
	if err != nil {
		return nil, err
	}
	message, err := loadSessionContextMessageTx(ctx, tx, read.Namespace, read.SessionName, read.MessageID, throughOrder)
	if err != nil {
		return nil, err
	}
	data, err := sessionHistoryMessageDataTx(ctx, tx, read.Namespace, read.SessionName, message)
	if err != nil {
		return nil, err
	}
	if read.Offset > len(data) || (read.Offset < len(data) && !utf8.RuneStart(data[read.Offset])) {
		return nil, store.ValidationErrorf("history offset must be a UTF-8 boundary within the saved message")
	}
	end := len(data)
	if len(data)-read.Offset > read.Limit {
		end = read.Offset + read.Limit
		for end > read.Offset && !utf8.RuneStart(data[end]) {
			end--
		}
		if end == read.Offset {
			return nil, store.ValidationErrorf("history limit must fit the next UTF-8 character")
		}
	}
	return &store.SessionHistoryResult{
		MessageID: message.ID, Role: message.Role, Data: string(data[read.Offset:end]),
		Offset: read.Offset, NextOffset: end, TotalBytes: len(data),
	}, nil
}

func verifySessionHistoryPageTx(ctx context.Context, tx *sql.Tx, write store.SessionContextWrite, page store.SessionHistoryResult) error {
	source, err := loadSessionContextMessageTx(ctx, tx, write.Namespace, write.SessionName, page.MessageID, 0)
	if err != nil {
		return err
	}
	data, err := sessionHistoryMessageDataTx(ctx, tx, write.Namespace, write.SessionName, source)
	if err != nil {
		return err
	}
	if page.Role != source.Role || page.TotalBytes != len(data) || page.NextOffset > len(data) ||
		string(data[page.Offset:page.NextOffset]) != page.Data {
		return store.ValidationErrorf("history page does not match its saved source")
	}
	return nil
}

// Context sources were sanitized before storage. Legacy messages are sanitized
// before pagination, so neither source JSON nor the page envelope needs a text
// redactor after byte boundaries have been assigned.
func sessionHistoryMessageDataTx(ctx context.Context, tx *sql.Tx, namespace, name string, message store.SessionMessage) ([]byte, error) {
	data, err := sessionContextMessageDataTx(ctx, tx, namespace, name, message)
	if err != nil || message.SourceType == sessioncontext.SourceType {
		return data, err
	}
	var source store.SessionMessage
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&source); err != nil {
		return nil, err
	}
	source, err = sanitizeSessionContextMessage(source)
	if err != nil {
		return nil, err
	}
	return json.Marshal(source)
}

func validateSessionContextReadIdentity(namespace, sessionName, throughMessageID string) error {
	return (store.SessionHistoryRead{
		Namespace: namespace, SessionName: sessionName, ThroughMessageID: throughMessageID,
		MessageID: "identity-check", Limit: 1,
	}).Validate()
}

func sessionContextSessionTx(ctx context.Context, tx *sql.Tx, namespace, name string) (string, string, error) {
	var sessionType, ownerType string
	err := tx.QueryRowContext(ctx, `SELECT session_type, owner_type FROM sessions WHERE namespace = ? AND name = ?`,
		namespace, name).Scan(&sessionType, &ownerType)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", store.ErrNotFound
	}
	if err != nil {
		return "", "", err
	}
	if err := ensureNoSessionCleanupIntentTx(ctx, tx, namespace, name); err != nil {
		return "", "", err
	}
	return sessionType, ownerType, nil
}

func verifySessionContextWriteTx(ctx context.Context, tx *sql.Tx, write store.SessionContextWrite) error {
	sessionType, ownerType, err := sessionContextSessionTx(ctx, tx, write.Namespace, write.SessionName)
	if err != nil {
		return err
	}
	if sessionType == store.SessionTypeGateway || ownerType == gatewaySessionOwnerType {
		return errors.Join(store.ErrConflict, store.ErrGatewayOwnedSession)
	}
	return verifySessionWriteLockTx(ctx, tx, write.Namespace, write.SessionName, write.OwnerName, write.OwnerUID)
}

func sessionContextBoundaryTx(ctx context.Context, tx *sql.Tx, namespace, sessionName, throughMessageID string) (int64, error) {
	if throughMessageID == "" {
		return 0, nil
	}
	var order int64
	err := tx.QueryRowContext(ctx,
		`SELECT sort_order FROM session_messages WHERE namespace = ? AND session_name = ? AND message_id = ?`,
		namespace, sessionName, throughMessageID,
	).Scan(&order)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, store.ErrNotFound
	}
	return order, err
}

func loadSessionContextMessageTx(ctx context.Context, tx *sql.Tx, namespace, sessionName, id string, throughOrder int64) (store.SessionMessage, error) {
	var message store.SessionMessage
	var name, input, calls, callID sql.NullString
	var metadata string
	err := tx.QueryRowContext(ctx,
		`SELECT message_id, sort_order, role, content, name, input, tool_calls, tool_call_id,
		 source_type, source_ref, metadata_json, created_at FROM session_messages
		 WHERE namespace = ? AND session_name = ? AND message_id = ? AND (? = 0 OR sort_order <= ?)`,
		namespace, sessionName, id, throughOrder, throughOrder,
	).Scan(&message.ID, &message.Order, &message.Role, &message.Content, &name, &input, &calls,
		&callID, &message.SourceType, &message.SourceRef, &metadata, &message.Timestamp)
	if errors.Is(err, sql.ErrNoRows) {
		return message, store.ErrNotFound
	}
	if err != nil {
		return message, err
	}
	message.Name, message.ToolCallID = name.String, callID.String
	// Preserve JSON numbers exactly in recoverable source data, including IDs
	// and tool arguments that do not fit in a floating-point integer.
	for _, field := range []struct {
		data string
		out  any
	}{{input.String, &message.Input}, {calls.String, &message.ToolCalls}, {metadata, &message.Metadata}} {
		if field.data != "" {
			decoder := json.NewDecoder(bytes.NewBufferString(field.data))
			decoder.UseNumber()
			if err := decoder.Decode(field.out); err != nil {
				return message, fmt.Errorf("decode saved session context: %w", err)
			}
		}
	}
	return message, nil
}

func sessionContextMessageDataTx(ctx context.Context, tx *sql.Tx, namespace, sessionName string, message store.SessionMessage) ([]byte, error) {
	if ref := message.Metadata[store.SessionContextOutputRefKey]; ref != "" {
		if ref != message.ID {
			return nil, store.ErrNotFound
		}
		var data []byte
		err := tx.QueryRowContext(ctx,
			`SELECT message_json FROM session_context_outputs WHERE namespace = ? AND session_name = ? AND message_id = ?`,
			namespace, sessionName, message.ID,
		).Scan(&data)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return data, err
	}
	return json.Marshal(message)
}

func sanitizeSessionContextMessage(message store.SessionMessage) (store.SessionMessage, error) {
	if err := store.ValidateControlIdentifier("context message ID", message.ID); err != nil {
		return store.SessionMessage{}, err
	}
	if redact.SensitiveText(message.ID) != message.ID {
		return store.SessionMessage{}, store.ValidationErrorf("context message ID contains credential-shaped content")
	}
	switch message.Role {
	case "system", "user", "assistant", "tool":
	default:
		return store.SessionMessage{}, store.ValidationErrorf("context message has an unsupported role")
	}
	if message.Order < 0 {
		return store.SessionMessage{}, store.ValidationErrorf("context message order must not be negative")
	}
	if !utf8.ValidString(message.Content) {
		return store.SessionMessage{}, store.ValidationErrorf("context message content must be valid UTF-8")
	}
	data, err := encodeSessionContextMessage(message)
	if err != nil {
		return store.SessionMessage{}, err
	}
	var copied store.SessionMessage
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&copied); err != nil {
		return store.SessionMessage{}, err
	}
	copied.Content = redact.SensitiveText(copied.Content)
	copied.Name = redact.SensitiveText(copied.Name)
	copied.ToolCallID = redact.SensitiveText(copied.ToolCallID)
	copied.SourceType = redact.SensitiveText(copied.SourceType)
	copied.SourceRef = redact.SensitiveText(copied.SourceRef)
	if copied.Input != nil {
		copied.Input = sanitizeSessionContextJSON(copied.Input).(map[string]any)
	}
	copied.ToolCalls = sanitizeSessionContextJSON(copied.ToolCalls)
	metadata := make(map[string]string, len(copied.Metadata))
	for key, value := range copied.Metadata {
		if events.IsSensitiveExecutionEventKey(key) {
			value = events.ExecutionEventRedactedValue
		} else {
			value = redact.SensitiveText(value)
		}
		metadata[redact.SensitiveText(key)] = value
	}
	copied.Metadata = metadata
	return copied, nil
}

func sanitizeSessionContextJSON(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, child := range typed {
			if events.IsSensitiveExecutionEventKey(key) {
				out[redact.SensitiveText(key)] = events.ExecutionEventRedactedValue
			} else {
				out[redact.SensitiveText(key)] = sanitizeSessionContextJSON(child)
			}
		}
		return out
	case []any:
		for i, child := range typed {
			typed[i] = sanitizeSessionContextJSON(child)
		}
		return typed
	case string:
		return redact.SensitiveText(typed)
	default:
		return typed
	}
}

func encodeSessionContextMessage(message store.SessionMessage) ([]byte, error) {
	data, err := json.Marshal(message)
	if err != nil {
		return nil, fmt.Errorf("encode session context message: %w", err)
	}
	if len(data) > store.MaxSessionContextMessageBytes {
		return nil, store.ValidationErrorf("context message exceeds %d bytes", store.MaxSessionContextMessageBytes)
	}
	return data, nil
}

func sessionContextPreview(message store.SessionMessage) store.SessionMessage {
	if len(message.Content) <= store.MaxSessionContextPreviewBytes {
		return message
	}
	notice := "\n[Full saved message: " + message.ID + ". Use read_session_history for details.]"
	end := store.MaxSessionContextPreviewBytes - len(notice)
	for end > 0 && !utf8.RuneStart(message.Content[end]) {
		end--
	}
	message.Content = message.Content[:end] + notice
	message.Metadata = maps.Clone(message.Metadata)
	if message.Metadata == nil {
		message.Metadata = make(map[string]string)
	}
	message.Metadata[store.SessionContextOutputRefKey] = message.ID
	return message
}
