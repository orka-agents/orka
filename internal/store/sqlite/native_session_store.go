package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/orka-agents/orka/internal/store"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// Native session records arrive sealed by the storage client. SQLite stores
// the nonce and ciphertext as opaque bytes and decides ownership, transcript
// boundaries and monotonic history from the plain fields beside them.

var _ storagev1.SealedNativeSessionStore = (*Store)(nil)

// SetNativeSessionFinalizationSealer installs the storage client's sealer for
// the duplicate SQL control implementation, which tests use in place of the
// Kubernetes control store. That path finalizes turns inside its own
// transaction, so it asks the sealer to seal native state there. Production
// finalization reaches SQLite already sealed and never uses it.
func (s *Store) SetNativeSessionFinalizationSealer(sealer store.NativeSessionFinalizationSealer) {
	s.nativeSessionSealer = sealer
}

func writeSealedNativeSessionTx(ctx context.Context, tx *sql.Tx, record storagev1.SealedNativeSession) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO native_session_snapshots(namespace,session_name,session_uid,record_digest,data_digest,source_operation_id,runtime_generation,message_count,through_message_id,nonce,ciphertext)
 VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(namespace,session_name) DO UPDATE SET
 session_uid=excluded.session_uid, record_digest=excluded.record_digest, data_digest=excluded.data_digest,
 source_operation_id=excluded.source_operation_id,runtime_generation=excluded.runtime_generation,message_count=excluded.message_count,
 through_message_id=excluded.through_message_id,nonce=excluded.nonce,ciphertext=excluded.ciphertext`,
		record.Namespace, record.SessionName, record.SessionUID, record.RecordDigest, record.DataDigest, record.SourceOperationID,
		record.RuntimeSessionGeneration, record.MessageCount, record.ThroughMessageID, record.Nonce, record.Ciphertext)
	return err
}

func readSealedNativeSessionTx(ctx context.Context, tx *sql.Tx, namespace, name string) (*storagev1.SealedNativeSession, error) {
	record := storagev1.SealedNativeSession{Namespace: namespace, SessionName: name}
	err := tx.QueryRowContext(ctx, `SELECT session_uid,record_digest,data_digest,source_operation_id,runtime_generation,message_count,through_message_id,nonce,ciphertext FROM native_session_snapshots WHERE namespace=? AND session_name=?`, namespace, name).
		Scan(&record.SessionUID, &record.RecordDigest, &record.DataDigest, &record.SourceOperationID, &record.RuntimeSessionGeneration,
			&record.MessageCount, &record.ThroughMessageID, &record.Nonce, &record.Ciphertext)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &record, nil
}

func nativeSessionOwnerTx(ctx context.Context, tx *sql.Tx, namespace, name string) (uid string, count int, lastID string, err error) {
	var sessionType string
	err = tx.QueryRowContext(ctx, `SELECT control_session_uid,message_count,session_type FROM sessions WHERE namespace=? AND name=?`, namespace, name).Scan(&uid, &count, &sessionType)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, "", store.ErrNotFound
	}
	if err != nil {
		return
	}
	if sessionType == store.SessionTypeGateway {
		return "", 0, "", store.ErrGatewayOwnedSession
	}
	if err = ensureNoSessionCleanupIntentTx(ctx, tx, namespace, name); err != nil {
		return
	}
	var deleted int
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM native_session_deleted_names WHERE namespace=? AND session_name=?`, namespace, name).Scan(&deleted)
	if err != nil {
		return
	}
	if deleted > 0 {
		return "", 0, "", store.ErrConflict
	}
	lastID, err = lastSessionMessageIDTx(ctx, tx, namespace, name, count)
	return
}

// lastSessionMessageIDTx returns the ID of the Session's last transcript
// message, or empty when count is zero.
func lastSessionMessageIDTx(ctx context.Context, tx *sql.Tx, namespace, name string, count int) (string, error) {
	if count <= 0 {
		return "", nil
	}
	var lastID string
	err := tx.QueryRowContext(ctx, `SELECT message_id FROM session_messages WHERE namespace=? AND session_name=? ORDER BY sort_order DESC,id DESC LIMIT 1`, namespace, name).Scan(&lastID)
	return lastID, err
}

// GetSealedNativeSession implements storagev1.SealedNativeSessionStore. It
// returns the record only to its canonical owner and only while it covers the
// current transcript.
func (s *Store) GetSealedNativeSession(ctx context.Context, namespace, name, uid string) (*storagev1.SealedNativeSession, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM native_session_snapshots WHERE namespace=? AND session_name=?`, namespace, name).Scan(&exists); err != nil {
		return nil, err
	}
	if exists == 0 {
		return nil, store.ErrNotFound
	}
	owner, count, lastID, err := nativeSessionOwnerTx(ctx, tx, namespace, name)
	if err != nil {
		return nil, err
	}
	if owner != uid {
		return nil, store.ErrConflict
	}
	record, err := readSealedNativeSessionTx(ctx, tx, namespace, name)
	if err != nil {
		return nil, err
	}
	if record.SessionUID != "" && record.SessionUID != uid {
		return nil, store.ErrConflict
	}
	if record.MessageCount != count || record.ThroughMessageID != lastID {
		return nil, store.ConflictErrorf("native session snapshot does not cover current transcript history")
	}
	return record, nil
}

// SaveSealedNativeSession implements storagev1.SealedNativeSessionStore. It
// preserves monotonic native checkpoints and immutable named operation
// outcomes. Old captures cannot replace newer conversation history.
func (s *Store) SaveSealedNativeSession(ctx context.Context, request storagev1.SaveSealedNativeSessionRequest) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := saveSealedNativeSessionTx(ctx, tx, request); err != nil {
		return err
	}
	return tx.Commit()
}

func saveSealedNativeSessionTx(ctx context.Context, tx *sql.Tx, request storagev1.SaveSealedNativeSessionRequest) error {
	record := request.Record
	if err := record.Validate(); err != nil {
		return err
	}
	if record.SessionUID == "" {
		return store.ValidationErrorf("native capture requires the canonical Session UID")
	}
	if err := store.ValidateCanonicalDigest("native capture request digest", request.RequestDigest); err != nil {
		return err
	}
	owner, count, lastID, err := nativeSessionOwnerTx(ctx, tx, record.Namespace, record.SessionName)
	if err != nil {
		return err
	}
	if owner != record.SessionUID {
		return store.ErrConflict
	}
	var priorDigest, kind string
	err = tx.QueryRowContext(ctx, `SELECT request_digest,kind FROM native_session_operations WHERE namespace=? AND session_name=? AND kind='capture' AND operation_id=?`, record.Namespace, record.SessionName, record.SourceOperationID).Scan(&priorDigest, &kind)
	if err == nil {
		if priorDigest == request.RequestDigest && kind == "capture" {
			return nil
		}
		return store.ErrDuplicateMismatch
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if count != record.MessageCount || lastID != record.ThroughMessageID {
		return store.ErrConflict
	}
	prior, err := readSealedNativeSessionTx(ctx, tx, record.Namespace, record.SessionName)
	if err == nil {
		if (prior.SessionUID != "" && prior.SessionUID != record.SessionUID) || record.RuntimeSessionGeneration < prior.RuntimeSessionGeneration || record.MessageCount < prior.MessageCount {
			return store.ErrConflict
		}
		if record.RuntimeSessionGeneration == prior.RuntimeSessionGeneration && record.MessageCount == prior.MessageCount {
			return store.ErrConflict
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if err = writeSealedNativeSessionTx(ctx, tx, record); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO native_session_operations(namespace,session_name,operation_id,request_digest,kind,receipt) VALUES(?,?,?,?,?,?)`, record.Namespace, record.SessionName, record.SourceOperationID, request.RequestDigest, "capture", []byte("{}"))
	return err
}

// commitSealedNativeFinalizationTx binds sealed native state to the canonical
// messages a turn finalization just appended in this transaction. The record
// must cover exactly that transcript boundary.
func commitSealedNativeFinalizationTx(
	ctx context.Context, tx *sql.Tx, turnID, namespace, name, uid string,
	native *storagev1.SealedNativeSessionFinalization, captureDigest string,
) error {
	if native == nil {
		return nil
	}
	record := native.Record
	if record.Namespace != namespace || record.SessionName != name {
		return store.ConflictErrorf("native capture does not match finalized Session ownership")
	}
	owner, count, lastID, err := nativeSessionOwnerTx(ctx, tx, namespace, name)
	if err != nil {
		return err
	}
	if owner != uid {
		return store.ErrConflict
	}
	if record.MessageCount != count || record.ThroughMessageID != lastID {
		return store.ConflictErrorf("native session state does not cover the finalized transcript")
	}
	if native.Carried {
		err = carrySealedNativeSessionTx(ctx, tx, uid, native)
	} else {
		if record.SessionUID != uid {
			return store.ConflictErrorf("native capture does not match finalized Session ownership")
		}
		err = saveSealedNativeSessionTx(ctx, tx, storagev1.SaveSealedNativeSessionRequest{Record: record, RequestDigest: native.RequestDigest})
	}
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO native_session_finalizations(turn_id,namespace,session_name,capture_digest) VALUES(?,?,?,?)`, turnID, namespace, name, captureDigest)
	return err
}

// carrySealedNativeSessionTx replaces the stored checkpoint with the same
// checkpoint at an advanced transcript boundary. The stored record must be the
// one Orka opened, and the plain facts must carry over unchanged.
func carrySealedNativeSessionTx(ctx context.Context, tx *sql.Tx, uid string, native *storagev1.SealedNativeSessionFinalization) error {
	record := native.Record
	if err := record.Validate(); err != nil {
		return err
	}
	prior, err := readSealedNativeSessionTx(ctx, tx, record.Namespace, record.SessionName)
	if err != nil {
		return err
	}
	if prior.RecordDigest != native.ExpectedRecordDigest || (prior.SessionUID != "" && prior.SessionUID != uid) ||
		prior.SessionUID != record.SessionUID || prior.DataDigest != record.DataDigest ||
		prior.SourceOperationID != record.SourceOperationID || prior.RuntimeSessionGeneration != record.RuntimeSessionGeneration {
		return store.ConflictErrorf("carried native checkpoint does not match the stored checkpoint")
	}
	if record.MessageCount < prior.MessageCount {
		return store.ErrConflict
	}
	return writeSealedNativeSessionTx(ctx, tx, record)
}

// StageSealedNativeSessionImport implements storagev1.SealedNativeSessionStore.
// It atomically reserves a new Session and stages the import. A caller cannot
// replace even an empty Session owned by another call.
func (s *Store) StageSealedNativeSessionImport(ctx context.Context, request storagev1.StageSealedNativeSessionImportRequest) (*store.NativeSessionImportReceipt, error) {
	record, receipt := request.Record, request.Receipt
	if err := record.Validate(); err != nil {
		return nil, err
	}
	if record.SessionUID != "" || record.MessageCount != 0 {
		return nil, store.ValidationErrorf("staged native import must not carry runtime ownership")
	}
	if receipt.Namespace != record.Namespace || receipt.SessionName != record.SessionName ||
		receipt.OperationID != record.SourceOperationID || receipt.DataDigest != record.DataDigest {
		return nil, store.ValidationErrorf("native import receipt does not match the staged record")
	}
	if err := store.ValidateCanonicalDigest("native import request digest", receipt.RequestDigest); err != nil {
		return nil, err
	}
	if receipt.CreatedAt.IsZero() {
		return nil, store.ValidationErrorf("native import creation time is required")
	}
	receipt.CreatedAt = receipt.CreatedAt.UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var denied int
	err = tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM session_cleanup_intents WHERE namespace=? AND session_name=?) + (SELECT COUNT(*) FROM session_cleanup_completions WHERE namespace=? AND session_name=?) + (SELECT COUNT(*) FROM native_session_deleted_names WHERE namespace=? AND session_name=?)`, record.Namespace, record.SessionName, record.Namespace, record.SessionName, record.Namespace, record.SessionName).Scan(&denied)
	if err != nil {
		return nil, err
	}
	if denied > 0 {
		return nil, store.ErrConflict
	}
	var sessionType string
	err = tx.QueryRowContext(ctx, `SELECT session_type FROM sessions WHERE namespace=? AND name=?`, record.Namespace, record.SessionName).Scan(&sessionType)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if sessionType == store.SessionTypeGateway {
		return nil, store.ErrGatewayOwnedSession
	}
	var digest, kind string
	var encoded []byte
	err = tx.QueryRowContext(ctx, `SELECT request_digest,kind,receipt FROM native_session_operations WHERE namespace=? AND session_name=? AND kind='import' AND operation_id=?`, record.Namespace, record.SessionName, record.SourceOperationID).Scan(&digest, &kind, &encoded)
	if err == nil {
		if digest != receipt.RequestDigest || kind != "import" {
			return nil, store.ErrDuplicateMismatch
		}
		var stored store.NativeSessionImportReceipt
		if err = json.Unmarshal(encoded, &stored); err != nil {
			return nil, err
		}
		return &stored, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO sessions(namespace,name,session_type,created_at,updated_at) VALUES(?,?,'task',?,?)`, record.Namespace, record.SessionName, receipt.CreatedAt, receipt.CreatedAt)
	if isSQLiteConstraintError(err) {
		return nil, store.ErrConflict
	}
	if err != nil {
		return nil, err
	}
	if err = writeSealedNativeSessionTx(ctx, tx, record); err != nil {
		return nil, err
	}
	encoded, err = json.Marshal(receipt)
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO native_session_operations(namespace,session_name,operation_id,request_digest,kind,receipt) VALUES(?,?,?,?,?,?)`, record.Namespace, record.SessionName, record.SourceOperationID, receipt.RequestDigest, "import", encoded)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &receipt, nil
}

// ListSealedNativeSessions implements storagev1.SealedNativeSessionStore.
func (s *Store) ListSealedNativeSessions(ctx context.Context, cursor string, limit int) (*storagev1.SealedNativeSessionPage, error) {
	if err := validateSealedPageLimit(limit); err != nil {
		return nil, err
	}
	afterNamespace, afterName, err := decodeKeysetCursor(cursor)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT namespace,session_name,session_uid,record_digest,data_digest,source_operation_id,runtime_generation,message_count,through_message_id,nonce,ciphertext
		FROM native_session_snapshots
		WHERE ? = '' OR namespace > ? OR (namespace = ? AND session_name > ?)
		ORDER BY namespace ASC, session_name ASC
		LIMIT ?`, cursor, afterNamespace, afterNamespace, afterName, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	page := &storagev1.SealedNativeSessionPage{Sessions: make([]storagev1.SealedNativeSession, 0)}
	for rows.Next() {
		var record storagev1.SealedNativeSession
		if err := rows.Scan(&record.Namespace, &record.SessionName, &record.SessionUID, &record.RecordDigest, &record.DataDigest,
			&record.SourceOperationID, &record.RuntimeSessionGeneration, &record.MessageCount, &record.ThroughMessageID,
			&record.Nonce, &record.Ciphertext); err != nil {
			return nil, err
		}
		page.Sessions = append(page.Sessions, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(page.Sessions) == limit {
		// Probe for a following record by key only, so a full page never
		// reads one more record's sealed bytes.
		last := page.Sessions[limit-1]
		var more bool
		if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM native_session_snapshots
			WHERE namespace > ? OR (namespace = ? AND session_name > ?))`, last.Namespace, last.Namespace, last.SessionName).Scan(&more); err != nil {
			return nil, err
		}
		if more {
			page.NextCursor = encodeKeysetCursor(last.Namespace, last.SessionName)
		}
	}
	return page, nil
}

// validateNativeFinalizationShape admits plaintext native state on exactly two
// terminal shapes: a fresh capture committed with a canonical assistant
// result, or the Session's existing checkpoint carried across a non-success
// outcome marker.
func validateNativeFinalizationShape(native *store.NativeSessionRecord, carried bool, kind store.SessionTurnTerminalKind, skipTranscriptAppend bool) error {
	if native == nil {
		if carried {
			return store.ValidationErrorf("carried native checkpoint requires the existing record")
		}
		return nil
	}
	if skipTranscriptAppend {
		return store.ValidationErrorf("native state cannot bind to a finalization that leaves the transcript unchanged")
	}
	if carried && kind != store.SessionTurnOutcomeMarker {
		return store.ValidationErrorf("carried native checkpoint requires an outcome-marker finalization")
	}
	if !carried && kind != store.SessionTurnAssistantResult {
		return store.ValidationErrorf("native capture requires canonical assistant-result finalization")
	}
	return nil
}

// newFinalizationMessageIDs assigns the IDs of the transcript messages a
// duplicate SQL control finalization appends.
func newFinalizationMessageIDs(skipTranscript, skipUserPrompt bool) (string, string, error) {
	if skipTranscript {
		return "", "", nil
	}
	userMessageID := ""
	if !skipUserPrompt {
		id, err := newSessionMessageID()
		if err != nil {
			return "", "", err
		}
		userMessageID = id
	}
	terminalMessageID, err := newSessionMessageID()
	if err != nil {
		return "", "", err
	}
	return userMessageID, terminalMessageID, nil
}

// sealDuplicateControlNativeSessionTx has the installed storage client sealer
// seal the duplicate SQL control path's native state against the transcript
// boundary its finalization produces. It fails closed without a sealer.
func (s *Store) sealDuplicateControlNativeSessionTx(
	ctx context.Context, tx *sql.Tx, control store.SessionControl, request store.FinalizeSessionTurnRequest,
	userMessageID, terminalMessageID string,
) (*storagev1.SealedNativeSessionFinalization, error) {
	if request.NativeSession == nil {
		return nil, nil
	}
	native := *request.NativeSession
	if native.Namespace != control.Namespace || native.SessionName != control.SessionName || native.SessionUID != request.Key.SessionUID {
		return nil, store.ConflictErrorf("native capture does not match finalized Session ownership")
	}
	if s.nativeSessionSealer == nil {
		return nil, errors.New("native session state requires the storage client's sealer; native finalization fails closed")
	}
	turnID, err := request.Key.CanonicalID()
	if err != nil {
		return nil, err
	}
	basis, err := readSessionTurnFinalizationBasisTx(ctx, tx, turnID, control.Namespace, control.SessionName)
	if err != nil {
		return nil, err
	}
	appended := 1
	if userMessageID != "" {
		appended++
	}
	sealed, err := s.nativeSessionSealer.SealNativeSessionFinalization(ctx, store.NativeSessionFinalizationSeal{
		Native: native, Carried: request.NativeSessionCarried, Stored: basis.NativeSession,
		MessageCount: basis.MessageCount + appended, ThroughMessageID: terminalMessageID,
	})
	if err != nil {
		return nil, err
	}
	sealed.ExpectedMessageCount = basis.MessageCount
	sealed.ExpectedLastMessageID = basis.LastMessageID
	return sealed, nil
}

func deleteNativeSessionTx(ctx context.Context, tx *sql.Tx, namespace, name string) error {
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO native_session_deleted_names(namespace,session_name) SELECT namespace,session_name FROM native_session_snapshots WHERE namespace=? AND session_name=?`, namespace, name)
	if err != nil {
		return err
	}
	for _, table := range []string{"native_session_snapshots", "native_session_operations", "native_session_finalizations"} {
		if _, err = tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE namespace=? AND session_name=?`, namespace, name); err != nil {
			return err
		}
	}
	return nil
}

func ensureNativeSessionNameAvailableTx(ctx context.Context, tx *sql.Tx, namespace, name string) error {
	var deleted int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM native_session_deleted_names WHERE namespace=? AND session_name=?`, namespace, name).Scan(&deleted); err != nil {
		return err
	}
	if deleted > 0 {
		return store.ConflictErrorf("deleted native Session name is reserved")
	}
	return nil
}
