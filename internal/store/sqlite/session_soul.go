package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/orka-agents/orka/internal/store"
)

const sessionSoulHistorySQL = `WITH history AS (
 SELECT id, message_id, sort_order, role, source_type, source_ref, metadata_json
 FROM session_messages WHERE namespace = ? AND session_name = ?
), unbound_turns AS (
 SELECT failure.id AS error_id, ingress.id AS user_id
 FROM history AS failure JOIN history AS ingress
  ON ingress.role = 'user' AND ingress.source_type = 'gateway-event'
  AND ingress.source_ref = json_extract(failure.metadata_json, '$.eventId')
  AND ingress.message_id = 'gateway:' || ingress.source_ref || ':user'
  AND ingress.sort_order + 1 = failure.sort_order
 WHERE ? AND failure.role = 'assistant' AND failure.source_type = 'gateway-task'
  AND json_extract(failure.metadata_json, '$."orka.ai/soul-unbound"') = 'true'
  AND json_type(failure.metadata_json, '$."orka.ai/soul-configuration-digest"') IS NULL
  AND json_extract(failure.metadata_json, '$.eventId') <> ''
  AND failure.message_id = 'gateway:' || json_extract(failure.metadata_json, '$.eventId') || ':error'
  AND failure.source_ref <> '' AND failure.source_ref = json_extract(failure.metadata_json, '$.taskName')
), soul_history AS (
 SELECT * FROM history AS message WHERE NOT EXISTS (
  SELECT 1 FROM unbound_turns WHERE message.id = error_id OR message.id = user_id
 )
) `

func (s *Store) ReadSessionSoul(ctx context.Context, namespace, name, taskName, taskUID string) (store.SessionSoulState, error) {
	var soul store.SessionSoulState
	err := s.withTaskDataFence(ctx, func(ctx context.Context) error {
		var err error
		soul, err = s.readSessionSoulUnfenced(ctx, namespace, name, taskName, taskUID)
		return err
	})
	return soul, err
}

func (s *Store) readSessionSoulUnfenced(ctx context.Context, namespace, name, taskName, taskUID string) (store.SessionSoulState, error) {
	var state store.SessionSoulState
	err := s.readSnapshot(ctx, func(q taskDataExecutor) error {
		var err error
		state, err = readSessionSoul(ctx, q, namespace, name, taskName, taskUID)
		return err
	})
	return state, err
}

func readSessionSoul(ctx context.Context, q taskDataExecutor, namespace, name, taskName, taskUID string) (store.SessionSoulState, error) {
	var state store.SessionSoulState
	var ownerName, ownerUID string
	err := q.QueryRowContext(ctx, `SELECT session_type, active_task, active_task_uid, message_count
		FROM sessions WHERE namespace = ? AND name = ?`, namespace, name).
		Scan(&state.SessionType, &ownerName, &ownerUID, &state.MessageCount)
	if errors.Is(err, sql.ErrNoRows) {
		return state, store.ErrNotFound
	}
	if err != nil {
		return state, err
	}
	if taskUID == "" || ownerName != taskName || ownerUID != taskUID {
		return state, store.ConflictErrorf("Task no longer owns the Session prompt context")
	}
	gateway := state.SessionType == store.SessionTypeGateway
	var digest sql.NullString
	err = q.QueryRowContext(ctx, sessionSoulHistorySQL+`, revision AS (
		SELECT json_extract(metadata_json, '$."orka.ai/soul-configuration-digest"') AS digest
		FROM soul_history WHERE (NOT ? OR role = 'assistant' OR source_type = ?)
		ORDER BY sort_order, id LIMIT 1
	) SELECT EXISTS(SELECT 1 FROM revision), (SELECT digest FROM revision),
		COALESCE((SELECT message_id FROM soul_history WHERE source_type <> ? ORDER BY sort_order, id LIMIT 1), '')`,
		namespace, name, gateway, gateway, store.SessionSoulAnchorSource, store.SessionSoulAnchorSource).
		Scan(&state.Established, &digest, &state.FirstMessageID)
	if err != nil {
		return state, fmt.Errorf("read Session prompt revision: %w", err)
	}
	state.Digest = digest.String
	if state.Digest != "" {
		if err := store.ValidateCanonicalDigest("Session soul digest", state.Digest); err != nil {
			return store.SessionSoulState{}, err
		}
	}
	return state, nil
}

func (s *Store) EnsureSessionSoulWithLock(ctx context.Context, owner store.SessionOwnerWrite, digest string) error {
	return writeOperationErr(ctx, s, "EnsureSessionSoulWithLock", func(ctx context.Context) error {
		return s.ensureSessionSoulWithLockWrite(ctx, owner, digest)
	})
}

func (s *Store) ensureSessionSoulWithLockWrite(ctx context.Context, owner store.SessionOwnerWrite, digest string) error {
	namespace, name, taskName, taskUID := owner.Namespace, owner.Name, owner.OwnerName, owner.OwnerUID
	if err := requireRequestTime("session owner write", owner.Now); err != nil {
		return err
	}
	if digest != "" {
		if err := store.ValidateCanonicalDigest("Session soul digest", digest); err != nil {
			return err
		}
	}
	if taskName == "" || taskUID == "" {
		return store.ConflictErrorf("Task identity is required to pin Session soul context")
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	var ownerType, sessionType string
	if err := tx.QueryRowContext(ctx, `SELECT owner_type, session_type FROM sessions WHERE namespace = ? AND name = ?`,
		namespace, name).Scan(&ownerType, &sessionType); errors.Is(err, sql.ErrNoRows) {
		return store.ErrNotFound
	} else if err != nil {
		return err
	}
	if ownerType == gatewaySessionOwnerType || sessionType == store.SessionTypeGateway {
		return store.ConflictErrorf("Gateway owns its canonical Session soul context")
	}
	if err := verifySessionWriteLockTx(ctx, tx.Tx, owner); err != nil {
		return err
	}
	var existing sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT json_extract(metadata_json, '$."orka.ai/soul-configuration-digest"')
		FROM session_messages WHERE namespace = ? AND session_name = ? ORDER BY sort_order, id LIMIT 1`, namespace, name).Scan(&existing)
	if err == nil {
		if existing.String != digest {
			return store.ErrSessionConfigurationMismatch
		}
		return tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	metadata, err := json.Marshal(map[string]string{store.SessionSoulDigestMetadata: digest})
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO session_messages
		(namespace, session_name, message_id, sort_order, role, content, source_type, source_ref, metadata_json, created_at)
		VALUES (?, ?, 'orka:soul-context', 0, 'system', '', ?, '', ?, CURRENT_TIMESTAMP)`,
		namespace, name, store.SessionSoulAnchorSource, string(metadata)); err != nil {
		return err
	}
	return tx.Commit()
}

func retainGatewaySoulAnchorTx(ctx context.Context, tx *sql.Tx, event *store.GatewayEvent) (bool, error) {
	var id int64
	var messageID, sourceType, sourceRef string
	var digest sql.NullString
	err := tx.QueryRowContext(ctx, sessionSoulHistorySQL+`SELECT id, message_id, source_type, source_ref,
  json_extract(metadata_json, '$."orka.ai/soul-configuration-digest"')
  FROM soul_history WHERE role = 'assistant' OR source_type = ?
  ORDER BY sort_order, id LIMIT 1`, event.Namespace, event.SessionName, true, store.SessionSoulAnchorSource).
		Scan(&id, &messageID, &sourceType, &sourceRef, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if sourceType == store.SessionSoulAnchorSource {
		return false, nil
	}
	if messageID != store.GatewayAssistantMessageID(event.ID) && messageID != store.GatewayErrorMessageID(event.ID) &&
		(sourceType != "gateway-event" || sourceRef != event.ID) {
		return false, nil
	}
	if digest.String != "" {
		if err := store.ValidateCanonicalDigest("retained Session soul digest", digest.String); err != nil {
			return false, err
		}
	}
	metadata, err := json.Marshal(map[string]string{store.SessionSoulDigestMetadata: digest.String})
	if err != nil {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE session_messages SET message_id = 'orka:soul-context',
  role = 'system', content = '', name = NULL, input = NULL, tool_calls = NULL,
  tool_call_id = NULL, source_type = ?, source_ref = '', metadata_json = ?
  WHERE id = ?`, store.SessionSoulAnchorSource, string(metadata), id)
	return err == nil, err
}
