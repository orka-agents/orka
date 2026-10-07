package sqlite

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/orka-agents/orka/internal/codexstate"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
)

var _ store.NativeSessionImportStore = (*Store)(nil)

// maxNativeSessionBytes is the single transport cap shared with the API and runtime.
const maxNativeSessionBytes = harnessv2.MaxNativeSessionBytes

func nativeSessionAAD(namespace, name, uid, digest string) []byte {
	return fmt.Appendf(nil, "orka.native-session.v1\x00%s\x00%s\x00%s\x00%s", namespace, name, uid, digest)
}

// validateNativeRecord checks a record's shape. inspect additionally parses the
// bundle, which writes require; authenticated reads already proved the bytes
// are the ones inspected when they were written.
func validateNativeRecord(ctx context.Context, record store.NativeSessionRecord, staged, inspect bool) error {
	for field, value := range map[string]string{sessionControlFieldNamespace: record.Namespace, sessionControlFieldName: record.SessionName, "native operation ID": record.SourceOperationID} {
		if err := store.ValidateControlIdentifier(field, value); err != nil {
			return err
		}
	}
	if !staged {
		if err := store.ValidateControlIdentifier("session UID", record.SessionUID); err != nil {
			return err
		}
		if record.RuntimeSessionGeneration < 1 {
			return store.ValidationErrorf("native runtime generation must be positive")
		}
		if err := record.Snapshot.Validate(); err != nil {
			return store.ValidationErrorf("invalid captured native snapshot")
		}
	} else if record.SessionUID != "" || record.RuntimeSessionGeneration != 0 {
		// A staged import never owns a runtime. Its transcript boundary may
		// advance when a non-success prompt carries it across a terminal marker.
		return store.ValidationErrorf("staged native import must not carry runtime ownership")
	}
	if staged && (record.Snapshot.RuntimeSessionUID != "" || record.Snapshot.RuntimeProfileDigest != "" || record.Snapshot.WorkingDirectory != "") {
		return store.ValidationErrorf("staged import must not carry runtime configuration")
	}
	if record.MessageCount < 0 || (record.MessageCount == 0) != (record.ThroughMessageID == "") {
		return store.ValidationErrorf("native snapshot transcript boundary is incomplete")
	}
	if len(record.Snapshot.Data) == 0 || len(record.Snapshot.Data) > maxNativeSessionBytes {
		return store.ValidationErrorf("native session bundle must contain 1 through %d bytes", maxNativeSessionBytes)
	}
	if !inspect {
		return nil
	}
	summary, err := codexstate.Inspect(ctx, record.Snapshot.Data)
	if err != nil {
		return store.ValidationErrorf("invalid native session bundle")
	}
	if summary.DataDigest != record.Snapshot.DataDigest || summary.ThreadID != record.Snapshot.ProviderSessionID {
		return store.ValidationErrorf("native session bundle identity or digest mismatch")
	}
	if record.Snapshot.ProviderKind != "codex" || record.Snapshot.ProviderVersion != summary.Manifest.SourceCLIVersion {
		return store.ValidationErrorf("native provider does not match the verified bundle")
	}
	return nil
}

func (s *Store) writeNativeRecordTx(ctx context.Context, tx *sql.Tx, record store.NativeSessionRecord) error {
	body, err := json.Marshal(record)
	if err != nil {
		return err
	}
	digest := store.CanonicalBytesDigest(body)
	nonce := make([]byte, s.snapshotCipher.aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return err
	}
	ciphertext := s.snapshotCipher.aead.Seal(nil, nonce, body, nativeSessionAAD(record.Namespace, record.SessionName, record.SessionUID, digest))
	_, err = tx.ExecContext(ctx, `INSERT INTO native_session_snapshots(namespace,session_name,session_uid,record_digest,data_digest,source_operation_id,runtime_generation,message_count,through_message_id,nonce,ciphertext)
 VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(namespace,session_name) DO UPDATE SET
 session_uid=excluded.session_uid, record_digest=excluded.record_digest, data_digest=excluded.data_digest,
 source_operation_id=excluded.source_operation_id,runtime_generation=excluded.runtime_generation,message_count=excluded.message_count,
 through_message_id=excluded.through_message_id,nonce=excluded.nonce,ciphertext=excluded.ciphertext`,
		record.Namespace, record.SessionName, record.SessionUID, digest, record.Snapshot.DataDigest, record.SourceOperationID, record.RuntimeSessionGeneration, record.MessageCount, record.ThroughMessageID, nonce, ciphertext)
	return err
}

func (s *Store) readNativeRecordTx(ctx context.Context, tx *sql.Tx, namespace, name string) (*store.NativeSessionRecord, error) {
	var uid, digest, dataDigest, operationID, throughID string
	var generation int64
	var count int
	var nonce, ciphertext []byte
	err := tx.QueryRowContext(ctx, `SELECT session_uid,record_digest,data_digest,source_operation_id,runtime_generation,message_count,through_message_id,nonce,ciphertext FROM native_session_snapshots WHERE namespace=? AND session_name=?`, namespace, name).
		Scan(&uid, &digest, &dataDigest, &operationID, &generation, &count, &throughID, &nonce, &ciphertext)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if len(nonce) != s.snapshotCipher.aead.NonceSize() {
		return nil, fmt.Errorf("invalid native session encryption nonce")
	}
	body, err := s.snapshotCipher.aead.Open(nil, nonce, ciphertext, nativeSessionAAD(namespace, name, uid, digest))
	if err != nil {
		return nil, fmt.Errorf("native session authentication failed")
	}
	if store.CanonicalBytesDigest(body) != digest {
		return nil, fmt.Errorf("native session integrity failed")
	}
	var record store.NativeSessionRecord
	if err = json.Unmarshal(body, &record); err != nil {
		return nil, fmt.Errorf("decode private native session record: %w", err)
	}
	if record.Namespace != namespace || record.SessionName != name || record.SessionUID != uid || record.Snapshot.DataDigest != dataDigest || record.SourceOperationID != operationID || record.RuntimeSessionGeneration != generation || record.MessageCount != count || record.ThroughMessageID != throughID {
		return nil, fmt.Errorf("native session metadata integrity failed")
	}
	if err = validateNativeRecord(ctx, record, uid == "", false); err != nil {
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
	if count > 0 {
		err = tx.QueryRowContext(ctx, `SELECT message_id FROM session_messages WHERE namespace=? AND session_name=? ORDER BY sort_order DESC,id DESC LIMIT 1`, namespace, name).Scan(&lastID)
	}
	return
}

// GetNativeSession verifies private state against the existing canonical owner
// and exact transcript revision before exposing it for a replacement runtime.
func (s *Store) GetNativeSession(ctx context.Context, namespace, name, uid string) (*store.NativeSessionRecord, error) {
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
	if s.snapshotCipher == nil {
		return nil, errSnapshotCipherRequired
	}
	owner, count, lastID, err := nativeSessionOwnerTx(ctx, tx, namespace, name)
	if err != nil {
		return nil, err
	}
	if owner != uid {
		return nil, store.ErrConflict
	}
	record, err := s.readNativeRecordTx(ctx, tx, namespace, name)
	if err != nil {
		return nil, err
	}
	if record.SessionUID != "" && record.SessionUID != uid {
		return nil, store.ErrConflict
	}
	if record.MessageCount != count || record.ThroughMessageID != lastID {
		return nil, store.ConflictErrorf("native session snapshot does not cover current transcript history")
	}
	// An imported snapshot has no Orka owner until the real SessionControl binds
	// the transcript. Its provider/runtime metadata is rebound at runtime create.
	record.SessionUID = uid
	return record, nil
}

// SaveNativeSession preserves monotonic native checkpoints and immutable named
// operation outcomes. Old captures cannot replace newer conversation history.
func (s *Store) SaveNativeSession(ctx context.Context, record store.NativeSessionRecord) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.saveNativeSessionTx(ctx, tx, record); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) saveNativeSessionTx(ctx context.Context, tx *sql.Tx, record store.NativeSessionRecord) error {
	if s.snapshotCipher == nil {
		return errSnapshotCipherRequired
	}
	if err := validateNativeRecord(ctx, record, false, true); err != nil {
		return err
	}
	canonical := record
	canonical.CreatedAt = time.Time{}
	body, err := json.Marshal(canonical)
	if err != nil {
		return err
	}
	requestDigest := store.CanonicalBytesDigest(body)
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
		if priorDigest == requestDigest && kind == "capture" {
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
	prior, err := s.readNativeRecordTx(ctx, tx, record.Namespace, record.SessionName)
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
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now().UTC()
	}
	if err = s.writeNativeRecordTx(ctx, tx, record); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO native_session_operations(namespace,session_name,operation_id,request_digest,kind,receipt) VALUES(?,?,?,?,?,?)`, record.Namespace, record.SessionName, record.SourceOperationID, requestDigest, "capture", []byte("{}")); err != nil {
		return err
	}
	return nil
}

// validateNativeFinalization admits native state on exactly two terminal
// shapes: a fresh capture committed with a canonical assistant result, or the
// Session's existing checkpoint carried across a non-success outcome marker.
func validateNativeFinalization(native *store.NativeSessionRecord, carried bool, kind store.SessionTurnTerminalKind, skipTranscriptAppend bool) error {
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

// commitNativeSessionFinalizationTx binds the verified capture to the exact
// canonical messages just appended in this transaction. Caller boundaries are
// ignored because message IDs are generated by canonical finalization.
func (s *Store) commitNativeSessionFinalizationTx(ctx context.Context, tx *sql.Tx, turnID, namespace, name, uid string, native *store.NativeSessionRecord, carried bool) error {
	if native == nil {
		return nil
	}
	record := *native
	if record.Namespace != namespace || record.SessionName != name || record.SessionUID != uid {
		return store.ConflictErrorf("native capture does not match finalized Session ownership")
	}
	owner, count, lastID, err := nativeSessionOwnerTx(ctx, tx, namespace, name)
	if err != nil {
		return err
	}
	if owner != uid {
		return store.ErrConflict
	}
	record.MessageCount = count
	record.ThroughMessageID = lastID
	if carried {
		err = s.carryNativeSessionTx(ctx, tx, record)
	} else {
		err = s.saveNativeSessionTx(ctx, tx, record)
	}
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO native_session_finalizations(turn_id,namespace,session_name,capture_digest) VALUES(?,?,?,?)`, turnID, namespace, name, store.NativeSessionCaptureDigest(native))
	return err
}

// carryNativeSessionTx advances only the transcript boundary of the stored
// checkpoint. A failed, cancelled, or lost prompt produced no new native state,
// so the exact stored bytes, source operation, and runtime generation must
// match and no new capture operation is recorded.
func (s *Store) carryNativeSessionTx(ctx context.Context, tx *sql.Tx, record store.NativeSessionRecord) error {
	if s.snapshotCipher == nil {
		return errSnapshotCipherRequired
	}
	prior, err := s.readNativeRecordTx(ctx, tx, record.Namespace, record.SessionName)
	if err != nil {
		return err
	}
	if (prior.SessionUID != "" && prior.SessionUID != record.SessionUID) || prior.Snapshot.DataDigest != record.Snapshot.DataDigest ||
		!bytes.Equal(prior.Snapshot.Data, record.Snapshot.Data) || prior.SourceOperationID != record.SourceOperationID ||
		prior.RuntimeSessionGeneration != record.RuntimeSessionGeneration {
		return store.ConflictErrorf("carried native checkpoint does not match the stored checkpoint")
	}
	if record.MessageCount < prior.MessageCount {
		return store.ErrConflict
	}
	carried := *prior
	carried.MessageCount = record.MessageCount
	carried.ThroughMessageID = record.ThroughMessageID
	return s.writeNativeRecordTx(ctx, tx, carried)
}

// StageNativeSessionImport atomically reserves a new Session and stages the
// import. A caller cannot replace even an empty Session owned by another call.
func (s *Store) StageNativeSessionImport(ctx context.Context, request store.NativeSessionImport) (*store.NativeSessionImportReceipt, error) {
	if s.snapshotCipher == nil {
		return nil, errSnapshotCipherRequired
	}
	record := store.NativeSessionRecord{Namespace: request.Namespace, SessionName: request.SessionName, Snapshot: request.Snapshot, SourceOperationID: request.OperationID}
	if err := validateNativeRecord(ctx, record, true, true); err != nil {
		return nil, err
	}
	if err := store.ValidateCanonicalDigest("native import request digest", request.RequestDigest); err != nil {
		return nil, err
	}
	if request.RequestDigest != store.NativeSessionImportDigest(request.Namespace, request.SessionName, request.Snapshot.DataDigest) {
		return nil, store.ValidationErrorf("native import request digest mismatch")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var denied int
	err = tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM session_cleanup_intents WHERE namespace=? AND session_name=?) + (SELECT COUNT(*) FROM session_cleanup_completions WHERE namespace=? AND session_name=?) + (SELECT COUNT(*) FROM native_session_deleted_names WHERE namespace=? AND session_name=?)`, request.Namespace, request.SessionName, request.Namespace, request.SessionName, request.Namespace, request.SessionName).Scan(&denied)
	if err != nil {
		return nil, err
	}
	if denied > 0 {
		return nil, store.ErrConflict
	}
	var sessionType string
	err = tx.QueryRowContext(ctx, `SELECT session_type FROM sessions WHERE namespace=? AND name=?`, request.Namespace, request.SessionName).Scan(&sessionType)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if sessionType == store.SessionTypeGateway {
		return nil, store.ErrGatewayOwnedSession
	}
	var digest, kind string
	var encoded []byte
	err = tx.QueryRowContext(ctx, `SELECT request_digest,kind,receipt FROM native_session_operations WHERE namespace=? AND session_name=? AND kind='import' AND operation_id=?`, request.Namespace, request.SessionName, request.OperationID).Scan(&digest, &kind, &encoded)
	if err == nil {
		if digest != request.RequestDigest || kind != "import" {
			return nil, store.ErrDuplicateMismatch
		}
		var receipt store.NativeSessionImportReceipt
		if err = json.Unmarshal(encoded, &receipt); err != nil {
			return nil, err
		}
		return &receipt, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	now := time.Now().UTC()
	record.CreatedAt = now
	_, err = tx.ExecContext(ctx, `INSERT INTO sessions(namespace,name,session_type,created_at,updated_at) VALUES(?,?,'task',?,?)`, request.Namespace, request.SessionName, now, now)
	if isSQLiteConstraintError(err) {
		return nil, store.ErrConflict
	}
	if err != nil {
		return nil, err
	}
	if err = s.writeNativeRecordTx(ctx, tx, record); err != nil {
		return nil, err
	}
	receipt := &store.NativeSessionImportReceipt{Namespace: request.Namespace, SessionName: request.SessionName, OperationID: request.OperationID, RequestDigest: request.RequestDigest, DataDigest: request.Snapshot.DataDigest, ProviderSessionID: request.Snapshot.ProviderSessionID, CreatedAt: now}
	encoded, err = json.Marshal(receipt)
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO native_session_operations(namespace,session_name,operation_id,request_digest,kind,receipt) VALUES(?,?,?,?,?,?)`, request.Namespace, request.SessionName, request.OperationID, request.RequestDigest, "import", encoded)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return receipt, nil
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

func (s *Store) verifyNativeSessionCipher(candidate *AgentExecutionSnapshotCipher) error {
	rows, err := s.db.Query(`SELECT namespace,session_name,session_uid,record_digest,nonce,ciphertext FROM native_session_snapshots`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var namespace, name, uid, digest string
		var nonce, ciphertext []byte
		if err = rows.Scan(&namespace, &name, &uid, &digest, &nonce, &ciphertext); err != nil {
			return err
		}
		if len(nonce) != candidate.aead.NonceSize() {
			return fmt.Errorf("retained native session has invalid nonce")
		}
		body, openErr := candidate.aead.Open(nil, nonce, ciphertext, nativeSessionAAD(namespace, name, uid, digest))
		if openErr != nil || store.CanonicalBytesDigest(body) != digest {
			return fmt.Errorf("candidate snapshot key cannot authenticate retained native session; restore the previous key")
		}
	}
	return rows.Err()
}
