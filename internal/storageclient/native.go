package storageclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/orka-agents/orka/internal/codexstate"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// A native session record is sealed as its whole JSON encoding. The
// associated data binds the namespace, Session name, owner UID and the
// digest of that encoding; the provider stores the same facts in plain
// fields, and Orka checks them against the plaintext when it opens a record.
// The format matches the records earlier releases stored, so existing
// datasets open unchanged.

func nativeSessionAAD(namespace, name, uid, digest string) []byte {
	return fmt.Appendf(nil, "orka.native-session.v1\x00%s\x00%s\x00%s\x00%s", namespace, name, uid, digest)
}

// SetNativeSessionMaxBytes configures the size limit for new native bundles
// before the client serves requests. Lowering it does not make existing
// authenticated checkpoints unreadable.
func (c *Client) SetNativeSessionMaxBytes(limit int) error {
	normalized, err := harnessv2.NormalizeNativeSessionMaxBytes(limit)
	if err != nil {
		return err
	}
	c.nativeSessionMaxBytes.Store(int64(normalized))
	return nil
}

func (c *Client) nativeSessionLimit() int {
	if limit := c.nativeSessionMaxBytes.Load(); limit > 0 {
		return int(limit)
	}
	return harnessv2.DefaultMaxNativeSessionBytes
}

// validateNativeRecord checks a record's shape. inspect additionally parses the
// bundle, which writes require; authenticated reads already proved the bytes
// are the ones inspected when they were written. Their limit is the absolute
// ceiling, while new captures and imports use the configured write policy.
func validateNativeRecord(ctx context.Context, record store.NativeSessionRecord, staged, inspect bool, maxBytes int) error {
	for field, value := range map[string]string{"session namespace": record.Namespace, "session name": record.SessionName, "native operation ID": record.SourceOperationID} {
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
	if len(record.Snapshot.Data) == 0 || len(record.Snapshot.Data) > maxBytes {
		return store.ValidationErrorf("native session bundle must contain 1 through %d bytes", maxBytes)
	}
	if !inspect {
		return nil
	}
	summary, err := codexstate.Inspect(ctx, record.Snapshot.Data, maxBytes)
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

// sealNativeSession seals the whole record and copies the facts the provider
// enforces into plain fields.
func sealNativeSession(custody *keyCustody, record store.NativeSessionRecord) (storagev1.SealedNativeSession, error) {
	body, err := json.Marshal(record)
	if err != nil {
		return storagev1.SealedNativeSession{}, err
	}
	digest := store.CanonicalBytesDigest(body)
	nonce, ciphertext, err := custody.seal(nativeSessionAAD(record.Namespace, record.SessionName, record.SessionUID, digest), body)
	if err != nil {
		return storagev1.SealedNativeSession{}, fmt.Errorf("seal native session: %w", err)
	}
	return storagev1.SealedNativeSession{
		Namespace:                record.Namespace,
		SessionName:              record.SessionName,
		SessionUID:               record.SessionUID,
		RecordDigest:             digest,
		DataDigest:               record.Snapshot.DataDigest,
		SourceOperationID:        record.SourceOperationID,
		RuntimeSessionGeneration: record.RuntimeSessionGeneration,
		MessageCount:             record.MessageCount,
		ThroughMessageID:         record.ThroughMessageID,
		Nonce:                    nonce,
		Ciphertext:               ciphertext,
	}, nil
}

// openNativeSession authenticates a sealed record, checks every plain field
// the provider used against the plaintext, and validates the record.
func openNativeSession(ctx context.Context, custody *keyCustody, sealed storagev1.SealedNativeSession) (*store.NativeSessionRecord, error) {
	if len(sealed.Nonce) != custody.aead.NonceSize() {
		return nil, errors.New("invalid native session encryption nonce")
	}
	body, err := custody.open(sealed.Nonce, sealed.Ciphertext, nativeSessionAAD(sealed.Namespace, sealed.SessionName, sealed.SessionUID, sealed.RecordDigest))
	if err != nil {
		return nil, errors.New("native session authentication failed")
	}
	if store.CanonicalBytesDigest(body) != sealed.RecordDigest {
		return nil, errors.New("native session integrity failed")
	}
	var record store.NativeSessionRecord
	if err := json.Unmarshal(body, &record); err != nil {
		return nil, fmt.Errorf("decode private native session record: %w", err)
	}
	if record.Namespace != sealed.Namespace || record.SessionName != sealed.SessionName || record.SessionUID != sealed.SessionUID ||
		record.Snapshot.DataDigest != sealed.DataDigest || record.SourceOperationID != sealed.SourceOperationID ||
		record.RuntimeSessionGeneration != sealed.RuntimeSessionGeneration || record.MessageCount != sealed.MessageCount ||
		record.ThroughMessageID != sealed.ThroughMessageID {
		return nil, errors.New("native session metadata integrity failed")
	}
	if err := validateNativeRecord(ctx, record, record.SessionUID == "", false, harnessv2.MaxNativeSessionBytes); err != nil {
		return nil, err
	}
	return &record, nil
}

// nativeCaptureRequestDigest identifies a capture for retries. It excludes
// the record's creation time.
func nativeCaptureRequestDigest(record store.NativeSessionRecord) (string, error) {
	record.CreatedAt = time.Time{}
	body, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	return store.CanonicalBytesDigest(body), nil
}

// GetNativeSession returns the Session's private native state when uid is
// its canonical owner and the state covers the Session's current transcript.
// The provider enforces ownership and the boundary from plain fields; the
// client authenticates the record and checks those fields against it.
func (c *Client) GetNativeSession(ctx context.Context, namespace, name, uid string) (*store.NativeSessionRecord, error) {
	sealed, err := c.GetSealedNativeSession(ctx, namespace, name, uid)
	if err != nil {
		return nil, err
	}
	custody, err := c.activeCustody()
	if err != nil {
		return nil, err
	}
	if sealed == nil || sealed.Namespace != namespace || sealed.SessionName != name {
		return nil, fmt.Errorf("native session provider returned another record for %s/%s", namespace, name)
	}
	record, err := openNativeSession(ctx, custody, *sealed)
	if err != nil {
		return nil, err
	}
	if record.SessionUID != "" && record.SessionUID != uid {
		return nil, store.ErrConflict
	}
	// An imported snapshot has no Orka owner until the real SessionControl binds
	// the transcript. Its provider/runtime metadata is rebound at runtime create.
	record.SessionUID = uid
	return record, nil
}

// SaveNativeSession verifies and seals a native capture. The provider
// preserves monotonic checkpoints and immutable named operation outcomes, so
// an old capture cannot replace newer conversation history.
func (c *Client) SaveNativeSession(ctx context.Context, record store.NativeSessionRecord) error {
	custody, err := c.activeCustody()
	if err != nil {
		return err
	}
	if err := validateNativeRecord(ctx, record, false, true, c.nativeSessionLimit()); err != nil {
		return err
	}
	requestDigest, err := nativeCaptureRequestDigest(record)
	if err != nil {
		return err
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now().UTC()
	}
	sealed, err := sealNativeSession(custody, record)
	if err != nil {
		return err
	}
	return c.SaveSealedNativeSession(ctx, storagev1.SaveSealedNativeSessionRequest{Record: sealed, RequestDigest: requestDigest})
}

// StageNativeSessionImport verifies and seals an imported bundle, then has
// the provider reserve the new Session and stage it. A caller cannot replace
// even an empty Session owned by another call.
func (c *Client) StageNativeSessionImport(ctx context.Context, request store.NativeSessionImport) (*store.NativeSessionImportReceipt, error) {
	custody, err := c.activeCustody()
	if err != nil {
		return nil, err
	}
	record := store.NativeSessionRecord{Namespace: request.Namespace, SessionName: request.SessionName, Snapshot: request.Snapshot, SourceOperationID: request.OperationID}
	if err := validateNativeRecord(ctx, record, true, true, c.nativeSessionLimit()); err != nil {
		return nil, err
	}
	if err := store.ValidateCanonicalDigest("native import request digest", request.RequestDigest); err != nil {
		return nil, err
	}
	if request.RequestDigest != store.NativeSessionImportDigest(request.Namespace, request.SessionName, request.Snapshot.DataDigest) {
		return nil, store.ValidationErrorf("native import request digest mismatch")
	}
	now := time.Now().UTC()
	record.CreatedAt = now
	sealed, err := sealNativeSession(custody, record)
	if err != nil {
		return nil, err
	}
	receipt := store.NativeSessionImportReceipt{
		Namespace: request.Namespace, SessionName: request.SessionName, OperationID: request.OperationID,
		RequestDigest: request.RequestDigest, DataDigest: request.Snapshot.DataDigest,
		ProviderSessionID: request.Snapshot.ProviderSessionID, CreatedAt: now,
	}
	stored, err := c.StageSealedNativeSessionImport(ctx, storagev1.StageSealedNativeSessionImportRequest{Record: sealed, Receipt: receipt})
	if err != nil {
		return nil, err
	}
	if stored == nil || stored.Namespace != receipt.Namespace || stored.SessionName != receipt.SessionName ||
		stored.OperationID != receipt.OperationID || stored.RequestDigest != receipt.RequestDigest {
		return nil, fmt.Errorf("native session provider returned another import receipt for %s/%s", request.Namespace, request.SessionName)
	}
	return stored, nil
}

var _ store.NativeSessionFinalizationSealer = (*Client)(nil)

// SealNativeSessionFinalization implements
// store.NativeSessionFinalizationSealer. A capture is verified like
// SaveNativeSession; a carried checkpoint must match the stored record
// exactly, apart from the transcript boundary it advances.
func (c *Client) SealNativeSessionFinalization(
	ctx context.Context, request store.NativeSessionFinalizationSeal,
) (*storagev1.SealedNativeSessionFinalization, error) {
	custody, err := c.activeCustody()
	if err != nil {
		return nil, err
	}
	if request.Carried {
		return sealCarriedNativeSession(ctx, custody, request)
	}
	record := request.Native
	record.MessageCount = request.MessageCount
	record.ThroughMessageID = request.ThroughMessageID
	if err := validateNativeRecord(ctx, record, false, true, c.nativeSessionLimit()); err != nil {
		return nil, err
	}
	requestDigest, err := nativeCaptureRequestDigest(record)
	if err != nil {
		return nil, err
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now().UTC()
	}
	sealed, err := sealNativeSession(custody, record)
	if err != nil {
		return nil, err
	}
	return &storagev1.SealedNativeSessionFinalization{Record: sealed, RequestDigest: requestDigest}, nil
}

// sealCarriedNativeSession advances only the transcript boundary of the
// stored checkpoint. A failed, cancelled, or lost prompt produced no new
// native state, so the exact stored bytes, source operation, and runtime
// generation must match.
func sealCarriedNativeSession(
	ctx context.Context, custody *keyCustody, request store.NativeSessionFinalizationSeal,
) (*storagev1.SealedNativeSessionFinalization, error) {
	if request.Stored == nil {
		return nil, store.ErrNotFound
	}
	stored := *request.Stored
	if stored.Namespace != request.Native.Namespace || stored.SessionName != request.Native.SessionName {
		return nil, fmt.Errorf("native session provider returned another record for %s/%s", request.Native.Namespace, request.Native.SessionName)
	}
	prior, err := openNativeSession(ctx, custody, stored)
	if err != nil {
		return nil, err
	}
	record := request.Native
	if (prior.SessionUID != "" && prior.SessionUID != record.SessionUID) || prior.Snapshot.DataDigest != record.Snapshot.DataDigest ||
		!bytes.Equal(prior.Snapshot.Data, record.Snapshot.Data) || prior.SourceOperationID != record.SourceOperationID ||
		prior.RuntimeSessionGeneration != record.RuntimeSessionGeneration {
		return nil, store.ConflictErrorf("carried native checkpoint does not match the stored checkpoint")
	}
	if request.MessageCount < prior.MessageCount {
		return nil, store.ErrConflict
	}
	carried := *prior
	carried.MessageCount = request.MessageCount
	carried.ThroughMessageID = request.ThroughMessageID
	sealed, err := sealNativeSession(custody, carried)
	if err != nil {
		return nil, err
	}
	return &storagev1.SealedNativeSessionFinalization{Record: sealed, Carried: true, ExpectedRecordDigest: stored.RecordDigest}, nil
}
