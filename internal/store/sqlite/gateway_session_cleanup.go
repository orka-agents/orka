package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/harness"
	"github.com/orka-agents/orka/internal/store"
)

var _ store.GatewaySessionCleanupCandidateStore = (*Store)(nil)

// reclaimGatewayTranscriptTx preserves retention when no ACP coordinator is
// configured. It handles only transcripts without any ACP identity or records.
// All deletion and completion writes share the maintenance transaction; no
// runtime or Kubernetes authority can be retired by this path.
func reclaimGatewayTranscriptTx(ctx context.Context, tx *sql.Tx, namespace, sessionName string, now, terminalCutoff time.Time) (bool, error) {
	var ownerRef string
	var createdAt time.Time
	err := tx.QueryRowContext(ctx, `SELECT session.owner_ref, session.created_at FROM sessions session
		WHERE session.namespace = ? AND session.name = ? AND session.session_type = 'gateway' AND session.owner_type = 'gateway'
		  AND session.active_task = '' AND session.active_task_uid = '' AND session.chat_turn_id = ''
		  AND session.control_session_uid = '' AND session.updated_at < ?
		  AND NOT EXISTS (SELECT 1 FROM session_turns WHERE namespace = session.namespace AND session_name = session.name)
		  AND NOT EXISTS (SELECT 1 FROM session_controls WHERE namespace = session.namespace AND session_name = session.name)
		  AND NOT EXISTS (SELECT 1 FROM runtime_sessions WHERE namespace = session.namespace AND session_name = session.name)
		  AND NOT EXISTS (SELECT 1 FROM session_cleanup_intents WHERE namespace = session.namespace AND session_name = session.name)
		  AND NOT EXISTS (SELECT 1 FROM session_cleanup_completions WHERE namespace = session.namespace AND session_name = session.name)
		  AND NOT EXISTS (SELECT 1 FROM session_messages WHERE namespace = session.namespace AND session_name = session.name)
		  AND NOT EXISTS (SELECT 1 FROM gateway_events WHERE namespace = session.namespace AND session_name = session.name)
		  AND NOT EXISTS (SELECT 1 FROM gateway_deliveries WHERE namespace = session.namespace AND session_name = session.name)`,
		namespace, sessionName, terminalCutoff.UTC()).Scan(&ownerRef, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	parts := strings.Split(ownerRef, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return false, nil
	}
	operationID, operationDigest := store.GatewaySessionCleanupOperation(namespace, sessionName, "", parts[0], parts[1])
	intent := store.SessionCleanupIntent{
		Namespace: namespace, SessionName: sessionName, OperationID: operationID, OperationDigest: operationDigest, PreparedAt: now,
		Gateway: &store.GatewaySessionCleanupProof{
			GatewayUID: parts[0], BindingUID: parts[1], CreatedAt: createdAt.UTC(), TerminalCutoff: terminalCutoff.UTC(),
		},
	}
	if err := normalizeSessionCleanupIntent(&intent); err != nil {
		return false, err
	}
	encoded, err := json.Marshal(intent)
	if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO session_cleanup_intents(namespace, session_name, operation_id, operation_digest, plan, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, namespace, sessionName, operationID, operationDigest, encoded, intent.PreparedAt); err != nil {
		return false, err
	}
	if err := completeSessionCleanupTx(ctx, tx, store.CompleteSessionCleanupRequest{
		Namespace: namespace, SessionName: sessionName, OperationID: operationID, OperationDigest: operationDigest,
	}); err != nil {
		return false, err
	}
	return true, nil
}

// ListGatewaySessionCleanupCandidates also finds Sessions whose event records
// were compacted in an earlier maintenance pass. Runtime retirement must not
// depend on the continued presence of that bounded event history.
func (s *Store) ListGatewaySessionCleanupCandidates(ctx context.Context, namespace string, terminalCutoff time.Time) ([]store.GatewaySessionCleanupCandidate, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT session.namespace, session.name,
		COALESCE(NULLIF(session.control_session_uid, ''),
		  (SELECT MIN(turn.session_uid) FROM session_turns turn
		   WHERE turn.namespace = session.namespace AND turn.session_name = session.name), ''),
		session.owner_ref, session.created_at
		FROM sessions session
		WHERE (? = '' OR session.namespace = ?) AND session.session_type = 'gateway' AND session.owner_type = 'gateway'
		  AND session.active_task = '' AND session.active_task_uid = '' AND session.chat_turn_id = ''
		  AND session.updated_at < ?
		  AND NOT EXISTS (SELECT 1 FROM session_messages message WHERE message.namespace = session.namespace AND message.session_name = session.name)
		  AND NOT EXISTS (SELECT 1 FROM gateway_events event WHERE event.namespace = session.namespace AND event.session_name = session.name)
		  AND NOT EXISTS (SELECT 1 FROM gateway_deliveries delivery WHERE delivery.namespace = session.namespace AND delivery.session_name = session.name)
		ORDER BY session.namespace, session.name`, namespace, namespace, terminalCutoff.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var candidates []store.GatewaySessionCleanupCandidate
	for rows.Next() {
		var candidate store.GatewaySessionCleanupCandidate
		var ownerRef string
		if err := rows.Scan(&candidate.Namespace, &candidate.SessionName, &candidate.SessionUID, &ownerRef, &candidate.Proof.CreatedAt); err != nil {
			return nil, err
		}
		parts := strings.Split(ownerRef, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			continue // No exact Gateway/Binding identity authorizes this row.
		}
		candidate.Proof.GatewayUID, candidate.Proof.BindingUID = parts[0], parts[1]
		candidate.Proof.CreatedAt = candidate.Proof.CreatedAt.UTC()
		candidate.Proof.TerminalCutoff = terminalCutoff.UTC()
		candidates = append(candidates, candidate)
	}
	return candidates, rows.Err()
}

func validateGatewayCleanupProof(intent store.SessionCleanupIntent) error {
	proof := intent.Gateway
	if proof == nil {
		if strings.HasPrefix(intent.OperationID, store.GatewaySessionCleanupOperationPrefix) {
			return store.ValidationErrorf("Gateway cleanup operation requires its ownership proof")
		}
		return nil
	}
	for field, value := range map[string]string{"Gateway UID": proof.GatewayUID, "GatewayBinding UID": proof.BindingUID} {
		if err := store.ValidateControlIdentifier(field, value); err != nil {
			return err
		}
		if strings.Contains(value, "/") {
			return store.ValidationErrorf("%s must not contain a slash", field)
		}
	}
	if proof.CreatedAt.IsZero() || proof.TerminalCutoff.IsZero() || !proof.CreatedAt.Before(proof.TerminalCutoff) ||
		proof.TerminalCutoff.After(intent.PreparedAt) {
		return store.ValidationErrorf("Gateway cleanup requires an elapsed retention boundary and exact creation time")
	}
	operationID, operationDigest := store.GatewaySessionCleanupOperation(intent.Namespace, intent.SessionName, intent.SessionUID, proof.GatewayUID, proof.BindingUID)
	if intent.OperationID != operationID || intent.OperationDigest != operationDigest {
		return store.ConflictErrorf("Gateway cleanup operation does not match its ownership proof")
	}
	return nil
}

func validateGatewayCleanupEligibilityTx(ctx context.Context, tx *sql.Tx, intent store.SessionCleanupIntent) error {
	if intent.Gateway == nil {
		return store.ErrGatewayOwnedSession
	}
	var ownerType, ownerRef, activeTask, activeTaskUID, chatTurnID string
	var createdAt, updatedAt time.Time
	if err := tx.QueryRowContext(ctx, `SELECT owner_type, owner_ref, created_at, updated_at, active_task, active_task_uid, chat_turn_id
		FROM sessions WHERE namespace = ? AND name = ?`, intent.Namespace, intent.SessionName).
		Scan(&ownerType, &ownerRef, &createdAt, &updatedAt, &activeTask, &activeTaskUID, &chatTurnID); err != nil {
		return err
	}
	proof := intent.Gateway
	if ownerType != gatewaySessionOwnerType || ownerRef != proof.GatewayUID+"/"+proof.BindingUID ||
		!createdAt.Equal(proof.CreatedAt) || !updatedAt.Before(proof.TerminalCutoff) ||
		activeTask != "" || activeTaskUID != "" || chatTurnID != "" {
		return store.ConflictErrorf("Gateway session owner, identity, activity, or retention boundary changed")
	}
	var retained int
	if err := tx.QueryRowContext(ctx, `SELECT
		EXISTS(SELECT 1 FROM session_messages WHERE namespace = ? AND session_name = ?) OR
		EXISTS(SELECT 1 FROM gateway_events WHERE namespace = ? AND session_name = ?) OR
		EXISTS(SELECT 1 FROM gateway_deliveries WHERE namespace = ? AND session_name = ?) OR
		EXISTS(SELECT 1 FROM runtime_sessions WHERE namespace = ? AND session_name = ? AND state <> ?) OR
		EXISTS(SELECT 1 FROM session_turns WHERE namespace = ? AND session_name = ?
		  AND (state <> 'Finalized' OR finalized_at IS NULL OR finalized_at >= ? OR updated_at >= ?))`,
		intent.Namespace, intent.SessionName, intent.Namespace, intent.SessionName, intent.Namespace, intent.SessionName,
		intent.Namespace, intent.SessionName, harness.RuntimeSessionStateDeleted,
		intent.Namespace, intent.SessionName, proof.TerminalCutoff.UTC(), proof.TerminalCutoff.UTC()).Scan(&retained); err != nil {
		return err
	}
	if retained != 0 {
		return store.ConflictErrorf("Gateway session still has retained history, events, replies, or turns")
	}
	return nil
}
