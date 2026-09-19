package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/orka-agents/orka/internal/store"
)

// sessionSoulHistorySQL ignores only explicitly unbound canonical Gateway errors
// and their exact ingress rows for identity purposes, not transcript delivery.
// Keep the same evidence checks for reads and retention: an unbound error must
// never become a permanent no-soul anchor. The third argument enables the
// exception only for Gateway Sessions; ambiguous/legacy rows remain unchanged.
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

// ReadSessionSoul uses existing transcript metadata rather than introducing an
// independent Session identity store or changing the persisted SQLite schema.
func (s *Store) ReadSessionSoul(ctx context.Context, namespace, name, taskName, taskUID string) (store.SessionSoulState, error) {
	var state store.SessionSoulState
	var ownerName, ownerUID string
	err := s.taskDataExecutor(ctx).QueryRowContext(ctx, `SELECT session_type, active_task, active_task_uid, message_count
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
	// Gateway admission owns user messages before Task creation. Its first
	// assistant result not attested as unbound pins identity. FirstMessageID skips
	// attested failed turns, while still fencing any unrelated earlier history.
	gateway := state.SessionType == store.SessionTypeGateway
	var digest sql.NullString
	err = s.taskDataExecutor(ctx).QueryRowContext(ctx, sessionSoulHistorySQL+`, revision AS (
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

// EnsureSessionSoulWithLock pins a non-Gateway Session before a worker starts.
// The existing hidden anchor survives empty turns and failed transcript writes;
// no transcript content or visible message count is created by this operation.
// An empty digest explicitly pins absence of a soul, not an unused Session.
func (s *Store) EnsureSessionSoulWithLock(ctx context.Context, namespace, name, taskName, taskUID, digest string) error {
	if digest != "" {
		if err := store.ValidateCanonicalDigest("Session soul digest", digest); err != nil {
			return err
		}
	}
	if taskName == "" || taskUID == "" {
		return store.ConflictErrorf("Task identity is required to pin Session soul context")
	}
	tx, err := s.db.BeginTx(ctx, nil)
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
	// This also checks expiry and the cross-store cleanup fence.
	if err := verifySessionWriteLockTx(ctx, tx, namespace, name, taskName, taskUID); err != nil {
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

// retainGatewaySoulAnchorTx removes expired conversation content while keeping
// its first instruction revision as digest-only Session metadata. It stays in
// the existing schema, is invisible to transcript readers, and is deleted with
// the Session rather than with an individual Gateway event.
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
