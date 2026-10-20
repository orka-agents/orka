package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/json"
	"errors"
	"strings"

	"github.com/orka-agents/orka/internal/store"
)

// resolveGatewaySessionNameTx follows completed incarnations without reusing
// their physical names. The original completions remain available to old Tasks
// and stale cleanup retries, while fresh events share the same successor.
func resolveGatewaySessionNameTx(ctx context.Context, tx *sql.Tx, event *store.GatewayEvent) (string, error) {
	name := event.SessionName
	for {
		var pending bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM session_cleanup_intents WHERE namespace = ? AND session_name = ?
		)`, event.Namespace, name).Scan(&pending); err != nil {
			return "", err
		}
		if pending {
			return "", store.ErrGatewaySessionCleanupPending
		}

		completion, err := getSessionCleanupCompletionTx(ctx, tx, event.Namespace, name)
		if errors.Is(err, store.ErrNotFound) {
			return name, nil
		}
		if err != nil {
			return "", err
		}
		operationID, operationDigest := store.GatewaySessionCleanupOperation(
			event.Namespace, name, completion.SessionUID, event.GatewayUID, event.BindingUID,
		)
		if completion.OperationID != operationID || completion.OperationDigest != operationDigest {
			return "", store.ConflictErrorf("session %s/%s was reclaimed by another owner or cleanup operation", event.Namespace, name)
		}
		var sessionExists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM sessions WHERE namespace = ? AND name = ?
		)`, event.Namespace, name).Scan(&sessionExists); err != nil {
			return "", err
		}
		if sessionExists {
			return "", store.ConflictErrorf("reclaimed Gateway session %s/%s still exists", event.Namespace, name)
		}
		name = gatewaySessionSuccessorName(event.Namespace, event.SessionName, completion.OperationDigest)
	}
}

func gatewaySessionSuccessorName(namespace, logicalName, operationDigest string) string {
	encoded, _ := json.Marshal([]string{"orka.gateway.session-continuation.v1", namespace, logicalName, operationDigest})
	digest := sha256.Sum256(encoded)
	// Full SHA-256 in unpadded base32 fits in one DNS label with this prefix.
	return "gateway-" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(digest[:]))
}
