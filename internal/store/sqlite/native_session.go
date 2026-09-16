package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

var _ store.NativeSessionSnapshotStore = (*Store)(nil)

const (
	nativeSessionStaged    = "Staged"
	nativeSessionFinalized = "Finalized"
	nativeSessionReady     = "Ready"
)

type nativeSessionSnapshotRecord struct {
	snapshot      store.NativeSessionSnapshot
	state         string
	historyBefore string
	metadata      []byte
}

// StageNativeSessionSnapshot stores private state under the exact latest open
// turn. Neither a terminal Task nor this write makes the snapshot restorable.
func (s *Store) StageNativeSessionSnapshot(ctx context.Context, snapshot store.NativeSessionSnapshot, historyBeforeDigest string) error {
	if err := store.ValidateCanonicalDigest("native session history digest", historyBeforeDigest); err != nil {
		return err
	}
	if snapshot.HistoryDigest != "" || snapshot.HistoryMessageCount != 0 || snapshot.ThroughMessageID != "" {
		return store.ValidationErrorf("native session finalized history is assigned by the transcript store")
	}
	defaultCreatedAt, defaultExpiresAt := snapshot.CreatedAt.IsZero(), snapshot.ExpiresAt.IsZero()
	now := time.Now().UTC()
	snapshot, err := normalizeNativeSessionSnapshot(snapshot, now)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireNativeSessionSnapshotStageTx(ctx, tx, snapshot, historyBeforeDigest); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM native_session_snapshots WHERE expires_at <= ?`, now); err != nil {
		return err
	}
	existing, err := getNativeSessionSnapshotRecord(ctx, tx, snapshot.Namespace, snapshot.SessionName)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if err == nil {
		if existing.snapshot.ID == snapshot.ID {
			if defaultCreatedAt {
				snapshot.CreatedAt = existing.snapshot.CreatedAt
			}
			if defaultExpiresAt {
				snapshot.ExpiresAt = existing.snapshot.ExpiresAt
			}
			metadata, err := json.Marshal(snapshot)
			if err != nil {
				return err
			}
			if existing.state != nativeSessionStaged || existing.historyBefore != historyBeforeDigest ||
				!bytes.Equal(existing.metadata, metadata) || !bytes.Equal(existing.snapshot.Data, snapshot.Data) {
				return store.ConflictErrorf("native session snapshot source turn was reused with different state")
			}
			return tx.Commit()
		}
		if existing.snapshot.SessionUID != snapshot.SessionUID || existing.snapshot.NamespaceUID != snapshot.NamespaceUID ||
			existing.snapshot.Key.LeaseGeneration >= snapshot.Key.LeaseGeneration {
			return store.ConflictErrorf("native session snapshot replacement does not advance the same Session")
		}
	}
	metadata, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO native_session_snapshots (
			namespace, session_name, session_uid, namespace_uid, source_turn_id, lease_generation,
			state, history_before_digest, metadata, data, created_at, expires_at
		) VALUES (?, ?, ?, ?, ?, ?, 'Staged', ?, ?, ?, ?, ?)
		ON CONFLICT(namespace, session_name) DO UPDATE SET
			session_uid = excluded.session_uid, namespace_uid = excluded.namespace_uid,
			source_turn_id = excluded.source_turn_id, lease_generation = excluded.lease_generation,
			state = excluded.state, history_before_digest = excluded.history_before_digest,
			metadata = excluded.metadata, data = excluded.data,
			created_at = excluded.created_at, expires_at = excluded.expires_at`,
		snapshot.Namespace, snapshot.SessionName, snapshot.SessionUID, snapshot.NamespaceUID, snapshot.ID,
		snapshot.Key.LeaseGeneration, historyBeforeDigest, metadata, snapshot.Data, snapshot.CreatedAt, snapshot.ExpiresAt,
	)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// GetNativeSessionSnapshot returns only activated, unexpired state for the
// exact Session UID. The caller may already own the next turn and must check
// that the source generation immediately precedes its current mutation lease.
func (s *Store) GetNativeSessionSnapshot(ctx context.Context, namespace, sessionName, sessionUID string) (*store.NativeSessionSnapshot, error) {
	for field, value := range map[string]string{
		sessionControlFieldNamespace: namespace, sessionControlFieldName: sessionName, "session UID": sessionUID,
	} {
		if err := store.ValidateControlIdentifier(field, value); err != nil {
			return nil, err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM native_session_snapshots
		WHERE namespace = ? AND session_name = ? AND expires_at <= ?`, namespace, sessionName, time.Now().UTC()); err != nil {
		return nil, err
	}
	snapshot, err := readyNativeSessionSnapshotTx(ctx, tx, namespace, sessionName, sessionUID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if snapshot == nil {
		return nil, store.ErrNotFound
	}
	return snapshot, nil
}

func readyNativeSessionSnapshotTx(ctx context.Context, tx *sql.Tx, namespace, sessionName, sessionUID string) (*store.NativeSessionSnapshot, error) {
	record, err := getNativeSessionSnapshotRecord(ctx, tx, namespace, sessionName)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if record.state != nativeSessionReady || record.snapshot.SessionUID != sessionUID {
		return nil, nil
	}
	if err := requireNativeSessionSnapshotBinding(ctx, tx, record.snapshot); err != nil {
		if errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound) {
			return nil, deleteNativeSessionSnapshotTx(ctx, tx, record.snapshot.ID)
		}
		return nil, err
	}
	return &record.snapshot, nil
}

func normalizeNativeSessionSnapshot(snapshot store.NativeSessionSnapshot, now time.Time) (store.NativeSessionSnapshot, error) {
	turnID, err := snapshot.Key.CanonicalID()
	if err != nil {
		return store.NativeSessionSnapshot{}, err
	}
	if snapshot.ID == "" {
		snapshot.ID = turnID
	}
	if snapshot.ID != turnID || snapshot.SessionUID != snapshot.Key.SessionUID {
		return store.NativeSessionSnapshot{}, store.ValidationErrorf("native session snapshot identity must match its source turn")
	}
	for field, value := range map[string]string{
		sessionControlFieldNamespace: snapshot.Namespace, sessionControlFieldName: snapshot.SessionName,
		"namespace UID": snapshot.NamespaceUID, "provider session ID": snapshot.ProviderSessionID,
		"provider version": snapshot.ProviderVersion, "working directory": snapshot.WorkingDirectory,
	} {
		if err := store.ValidateControlIdentifier(field, value); err != nil {
			return store.NativeSessionSnapshot{}, err
		}
	}
	if !path.IsAbs(snapshot.WorkingDirectory) || path.Clean(snapshot.WorkingDirectory) != snapshot.WorkingDirectory {
		return store.NativeSessionSnapshot{}, store.ValidationErrorf("native session working directory must be a canonical absolute path")
	}
	for field, value := range map[string]string{
		"profile digest": snapshot.ProfileDigest, "configuration digest": snapshot.ConfigurationDigest,
		"workspace binding digest": snapshot.WorkspaceBindingDigest, "workspace digest": snapshot.WorkspaceDigest,
		"workspace state digest": snapshot.WorkspaceStateDigest,
	} {
		if err := store.ValidateCanonicalDigest(field, value); err != nil {
			return store.NativeSessionSnapshot{}, err
		}
	}
	if len(snapshot.Data) == 0 || len(snapshot.Data) > store.NativeSessionSnapshotMaxBytes {
		return store.NativeSessionSnapshot{}, store.ValidationErrorf("native session snapshot must contain between 1 and %d bytes", store.NativeSessionSnapshotMaxBytes)
	}
	digest := store.CanonicalBytesDigest(snapshot.Data)
	if snapshot.DataDigest != "" && snapshot.DataDigest != digest {
		return store.NativeSessionSnapshot{}, store.ValidationErrorf("native session snapshot data digest does not match")
	}
	snapshot.DataDigest = digest
	if snapshot.CreatedAt.IsZero() {
		snapshot.CreatedAt = now
	}
	snapshot.CreatedAt = snapshot.CreatedAt.UTC()
	if snapshot.ExpiresAt.IsZero() {
		snapshot.ExpiresAt = snapshot.CreatedAt.Add(store.NativeSessionSnapshotMaxTTL)
	}
	snapshot.ExpiresAt = snapshot.ExpiresAt.UTC()
	if snapshot.CreatedAt.After(now) || !snapshot.ExpiresAt.After(now) ||
		!snapshot.ExpiresAt.After(snapshot.CreatedAt) || snapshot.ExpiresAt.Sub(snapshot.CreatedAt) > store.NativeSessionSnapshotMaxTTL {
		return store.NativeSessionSnapshot{}, store.ValidationErrorf("native session snapshot lifetime must be current and no longer than %s", store.NativeSessionSnapshotMaxTTL)
	}
	return snapshot, nil
}

func requireNativeSessionSnapshotStageTx(ctx context.Context, tx *sql.Tx, snapshot store.NativeSessionSnapshot, historyBeforeDigest string) error {
	if err := requireNativeSessionSnapshotBinding(ctx, tx, snapshot); err != nil {
		return err
	}
	turn, err := getSessionTurn(ctx, tx, snapshot.ID)
	if err != nil {
		return err
	}
	if turn.State != store.SessionTurnOpen || turn.Key != snapshot.Key {
		return store.ConflictErrorf("native session snapshot requires its exact open SessionTurn")
	}
	if err := requireSessionTurnBinding(ctx, tx, turn.ID, snapshot.Namespace, snapshot.SessionName); err != nil {
		return err
	}
	if err := requireLatestNativeSessionTurn(ctx, tx, snapshot); err != nil {
		return err
	}
	history, err := nativeSessionHistoryTx(ctx, tx, snapshot.Namespace, snapshot.SessionName)
	if err != nil {
		return err
	}
	digest, err := store.NativeSessionHistoryDigest(history)
	if err != nil {
		return err
	}
	if digest != historyBeforeDigest {
		return store.ConflictErrorf("native session snapshot transcript changed before staging")
	}
	return nil
}

func requireNativeSessionSnapshotBinding(ctx context.Context, tx *sql.Tx, snapshot store.NativeSessionSnapshot) error {
	var sessionUID string
	err := tx.QueryRowContext(ctx, `SELECT control_session_uid FROM sessions WHERE namespace = ? AND name = ?`,
		snapshot.Namespace, snapshot.SessionName).Scan(&sessionUID)
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrNotFound
	}
	if err != nil {
		return err
	}
	if sessionUID != snapshot.SessionUID {
		return store.ConflictErrorf("native session snapshot transcript belongs to a different Session UID")
	}
	var fences int
	if err := tx.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM session_cleanup_intents WHERE namespace = ? AND session_name = ?) +
		(SELECT COUNT(*) FROM session_cleanup_completions WHERE (namespace = ? AND session_name = ?) OR session_uid = ?)`,
		snapshot.Namespace, snapshot.SessionName, snapshot.Namespace, snapshot.SessionName, snapshot.SessionUID).Scan(&fences); err != nil {
		return err
	}
	if fences != 0 {
		return store.ConflictErrorf("native session snapshot Session is being deleted")
	}
	var namespaceUID string
	err = tx.QueryRowContext(ctx, `SELECT session_uid, namespace_uid FROM session_lineages WHERE namespace = ? AND session_name = ?`,
		snapshot.Namespace, snapshot.SessionName).Scan(&sessionUID, &namespaceUID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if sessionUID != snapshot.SessionUID || namespaceUID != snapshot.NamespaceUID {
		return store.ConflictErrorf("native session snapshot belongs to a different Session lineage")
	}
	return nil
}

func requireLatestNativeSessionTurn(ctx context.Context, tx *sql.Tx, snapshot store.NativeSessionSnapshot) error {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_turns
		WHERE session_uid = ? AND lease_generation >= ? AND id <> ?`,
		snapshot.SessionUID, snapshot.Key.LeaseGeneration, snapshot.ID).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return store.ConflictErrorf("native session snapshot source turn is no longer the latest Session turn")
	}
	return nil
}

func getNativeSessionSnapshotRecord(ctx context.Context, tx *sql.Tx, namespace, sessionName string) (nativeSessionSnapshotRecord, error) {
	var record nativeSessionSnapshotRecord
	var sessionUID, namespaceUID, sourceTurnID string
	var leaseGeneration int64
	var data []byte
	var createdAt, expiresAt time.Time
	err := tx.QueryRowContext(ctx, `SELECT session_uid, namespace_uid, source_turn_id, lease_generation,
		state, history_before_digest, metadata, data, created_at, expires_at FROM native_session_snapshots
		WHERE namespace = ? AND session_name = ?`, namespace, sessionName).Scan(
		&sessionUID, &namespaceUID, &sourceTurnID, &leaseGeneration, &record.state, &record.historyBefore,
		&record.metadata, &data, &createdAt, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return record, store.ErrNotFound
	}
	if err != nil {
		return record, err
	}
	if err := json.Unmarshal(record.metadata, &record.snapshot); err != nil {
		return record, store.ConflictErrorf("native session snapshot metadata is invalid")
	}
	snapshot := &record.snapshot
	canonicalTurnID, keyErr := snapshot.Key.CanonicalID()
	if snapshot.Namespace != namespace || snapshot.SessionName != sessionName || snapshot.SessionUID != sessionUID ||
		snapshot.NamespaceUID != namespaceUID || snapshot.ID != sourceTurnID || snapshot.Key.LeaseGeneration != leaseGeneration ||
		keyErr != nil || canonicalTurnID != sourceTurnID || snapshot.Key.SessionUID != sessionUID ||
		!snapshot.CreatedAt.Equal(createdAt) || !snapshot.ExpiresAt.Equal(expiresAt) || snapshot.DataDigest != store.CanonicalBytesDigest(data) {
		return record, store.ConflictErrorf("native session snapshot metadata does not match its private record")
	}
	snapshot.Data = data
	return record, nil
}

func nativeSessionHistoryTx(ctx context.Context, tx *sql.Tx, namespace, sessionName string) ([]store.SessionMessage, error) {
	rows, err := tx.QueryContext(ctx, `SELECT message_id, sort_order, role, content, name, input, tool_calls, tool_call_id,
		source_type, source_ref, metadata_json, created_at FROM session_messages
		WHERE namespace = ? AND session_name = ? ORDER BY sort_order, id`, namespace, sessionName)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
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

func deleteNativeSessionSnapshotTx(ctx context.Context, tx *sql.Tx, turnID string) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM native_session_snapshots WHERE source_turn_id = ?`, turnID)
	return err
}
