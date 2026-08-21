package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/orka-agents/orka/internal/redact"
	"github.com/orka-agents/orka/internal/store"
)

func normalizeTargetReceipt(receipt *store.SecurityTargetReceipt) error {
	if receipt == nil {
		return store.ValidationErrorf("target receipt is required")
	}
	for field, value := range map[string]string{
		"id": receipt.ID, "namespace": receipt.Namespace, "repositoryScan": receipt.RepositoryScan,
		"scanRunID": receipt.ScanRunID, "runUID": receipt.RunUID, "targetID": receipt.TargetID,
	} {
		if err := validateSecurityID(value, field, true); err != nil {
			return err
		}
	}
	if err := validateSecurityRunUID(receipt.RunUID, true); err != nil {
		return err
	}
	receipt.ObjectFormat = strings.ToLower(strings.TrimSpace(receipt.ObjectFormat))
	expectedLength := 0
	switch receipt.ObjectFormat {
	case "sha1":
		expectedLength = 40
	case "sha256":
		expectedLength = 64
	default:
		return store.ValidationErrorf("target receipt objectFormat must be sha1 or sha256")
	}
	receipt.HeadSHA = strings.ToLower(strings.TrimSpace(receipt.HeadSHA))
	if len(receipt.HeadSHA) != expectedLength {
		return store.ValidationErrorf("target receipt headSHA must be a full %s object ID", receipt.ObjectFormat)
	}
	if _, err := hex.DecodeString(receipt.HeadSHA); err != nil {
		return store.ValidationErrorf("target receipt headSHA must be hexadecimal")
	}
	var err error
	receipt.SnapshotDigest, err = normalizeSecurityDigest(receipt.SnapshotDigest, true, "snapshotDigest")
	if err != nil {
		return err
	}
	receipt.TreeDigest, err = normalizeSecurityDigest(receipt.TreeDigest, true, "treeDigest")
	if err != nil {
		return err
	}
	receipt.ReceiptJSON, receipt.PayloadDigest, err = normalizeTargetPayload(receipt.ReceiptJSON, receipt.PayloadDigest, true, "receiptJSON")
	if err != nil {
		return err
	}
	if err := validateTargetReceiptPayloadBindings(receipt); err != nil {
		return err
	}
	receipt.InventoryJSON, receipt.InventoryDigest, err = normalizeTargetPayload(receipt.InventoryJSON, receipt.InventoryDigest, false, "inventoryJSON")
	return err
}

func normalizeTargetPayload(payload json.RawMessage, suppliedDigest string, required bool, field string) (json.RawMessage, string, error) {
	const maxTargetPayloadBytes = 16 << 20
	payload = append(json.RawMessage(nil), payload...)
	if len(payload) == 0 {
		if required {
			return nil, "", store.ValidationErrorf("%s is required", field)
		}
		digest, err := normalizeSecurityDigest(suppliedDigest, false, field+"Digest")
		return nil, digest, err
	}
	if len(payload) > maxTargetPayloadBytes {
		return nil, "", store.ValidationErrorf("%s exceeds %d bytes", field, maxTargetPayloadBytes)
	}
	if !utf8.Valid(payload) {
		return nil, "", store.ValidationErrorf("%s must be valid UTF-8", field)
	}
	if !json.Valid(payload) {
		return nil, "", store.ValidationErrorf("%s must be valid JSON", field)
	}
	containsCredential, err := targetPayloadContainsCredential(payload)
	if err != nil {
		return nil, "", store.ValidationErrorf("%s could not be inspected for credential-like content", field)
	}
	if containsCredential {
		// Target receipt IDs may already be derived from the supplied payload.
		// Reject instead of rewriting so no credential-derived identity or digest
		// can become an authoritative immutable record.
		return nil, "", store.ValidationErrorf("%s contains credential-like content", field)
	}
	actual := securityDigestBytes(payload)
	if suppliedDigest != "" {
		normalized, err := normalizeSecurityDigest(suppliedDigest, true, field+"Digest")
		if err != nil {
			return nil, "", err
		}
		if normalized != actual {
			return nil, "", store.ValidationErrorf("%s digest does not match payload", field)
		}
	}
	return payload, actual, nil
}

func validateTargetReceiptPayloadBindings(receipt *store.SecurityTargetReceipt) error {
	canonicalNames := map[string]string{
		"headoid":        "headOID",
		"objectformat":   "objectFormat",
		"snapshotdigest": "snapshotDigest",
		"treedigest":     "treeDigest",
	}
	fields := make(map[string]json.RawMessage, len(canonicalNames))
	decoder := json.NewDecoder(bytes.NewReader(receipt.ReceiptJSON))
	token, err := decoder.Token()
	if err != nil {
		return store.ValidationErrorf("receiptJSON must be a JSON object")
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return store.ValidationErrorf("receiptJSON must be a JSON object")
	}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return store.ValidationErrorf("receiptJSON could not be decoded")
		}
		key, ok := keyToken.(string)
		if !ok {
			return store.ValidationErrorf("receiptJSON object key must be a string")
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return store.ValidationErrorf("receiptJSON field %q could not be decoded", key)
		}
		canonical, tracked := canonicalNames[strings.ToLower(key)]
		if !tracked {
			continue
		}
		if key != canonical {
			return store.ValidationErrorf("receiptJSON field %q must use canonical name %q", key, canonical)
		}
		if _, duplicate := fields[canonical]; duplicate {
			return store.ValidationErrorf("receiptJSON field %q must not be duplicated", canonical)
		}
		fields[canonical] = append(json.RawMessage(nil), raw...)
	}
	if _, err := decoder.Token(); err != nil {
		return store.ValidationErrorf("receiptJSON object is incomplete")
	}

	headOID, err := targetReceiptStringField(fields, "headOID", true)
	if err != nil {
		return err
	}
	if headOID != receipt.HeadSHA {
		return store.ValidationErrorf("receiptJSON headOID does not match target receipt headSHA")
	}
	for field, indexed := range map[string]string{
		"objectFormat":   receipt.ObjectFormat,
		"snapshotDigest": receipt.SnapshotDigest,
		"treeDigest":     receipt.TreeDigest,
	} {
		value, err := targetReceiptStringField(fields, field, false)
		if err != nil {
			return err
		}
		if value != "" && value != indexed {
			return store.ValidationErrorf("receiptJSON %s does not match indexed target receipt value", field)
		}
	}
	return nil
}

func targetReceiptStringField(fields map[string]json.RawMessage, field string, required bool) (string, error) {
	raw, ok := fields[field]
	if !ok {
		if required {
			return "", store.ValidationErrorf("receiptJSON %s is required", field)
		}
		return "", nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", store.ValidationErrorf("receiptJSON %s must be a string", field)
	}
	if value == "" {
		if required {
			return "", store.ValidationErrorf("receiptJSON %s is required", field)
		}
		return "", store.ValidationErrorf("receiptJSON %s must not be empty when present", field)
	}
	return value, nil
}

func targetPayloadContainsCredential(payload []byte) (bool, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	return targetJSONValueContainsCredential(decoder, "")
}

func targetJSONValueContainsCredential(decoder *json.Decoder, field string) (bool, error) {
	if targetJSONFieldIsSensitive(field) {
		return true, nil
	}
	token, err := decoder.Token()
	if err != nil {
		return false, err
	}
	delim, isDelim := token.(json.Delim)
	if !isDelim {
		value, isString := token.(string)
		if !isString {
			return false, nil
		}
		if redact.SensitiveText(value) != value {
			return true, nil
		}
		if field != "" {
			fieldValue := field + ": " + value
			if redact.SensitiveText(fieldValue) != fieldValue {
				return true, nil
			}
		}
		return false, nil
	}

	switch delim {
	case '{':
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return false, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return false, fmt.Errorf("JSON object key is not a string")
			}
			if targetJSONFieldIsSensitive(key) {
				return true, nil
			}
			found, err := targetJSONValueContainsCredential(decoder, key)
			if err != nil || found {
				return found, err
			}
		}
	case '[':
		for decoder.More() {
			found, err := targetJSONValueContainsCredential(decoder, field)
			if err != nil || found {
				return found, err
			}
		}
	default:
		return false, fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
	_, err = decoder.Token()
	return false, err
}

func targetJSONFieldIsSensitive(field string) bool {
	field = strings.TrimSpace(field)
	if field == "" {
		return false
	}
	for _, probe := range []string{field + ": placeholder", field + ": Bearer placeholder"} {
		if redact.SensitiveText(probe) != probe {
			return true
		}
	}
	return false
}

func targetReceiptRecordDigest(receipt store.SecurityTargetReceipt) (string, error) {
	receipt.RecordDigest = ""
	receipt.CreatedAt = time.Time{}
	return securityRecordDigest(receipt)
}

func (s *Store) SaveSecurityTargetReceipt(ctx context.Context, receipt *store.SecurityTargetReceipt) (bool, error) {
	if err := normalizeTargetReceipt(receipt); err != nil {
		return false, err
	}
	digest, err := targetReceiptRecordDigest(*receipt)
	if err != nil {
		return false, err
	}
	if receipt.CreatedAt.IsZero() {
		receipt.CreatedAt = time.Now().UTC()
	}
	receipt.RecordDigest = digest
	result, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO security_target_receipts
		(id, namespace, repository_scan, scan_run_id, run_uid, target_id, head_sha, object_format,
		 snapshot_digest, tree_digest, receipt_json, inventory_json, inventory_digest, payload_digest, record_digest, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		receipt.ID, receipt.Namespace, receipt.RepositoryScan, receipt.ScanRunID, receipt.RunUID,
		receipt.TargetID, receipt.HeadSHA, receipt.ObjectFormat, receipt.SnapshotDigest, receipt.TreeDigest,
		securityPayloadBytes(receipt.ReceiptJSON), securityPayloadBytes(receipt.InventoryJSON), receipt.InventoryDigest, receipt.PayloadDigest, receipt.RecordDigest, receipt.CreatedAt)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected > 0 {
		return true, nil
	}
	var existingDigest string
	if err := s.db.QueryRowContext(ctx, `SELECT record_digest FROM security_target_receipts WHERE id = ?`, receipt.ID).Scan(&existingDigest); err != nil {
		return false, err
	}
	return immutableReplayResult(existingDigest, digest, "security target receipt", receipt.ID)
}

func (s *Store) GetSecurityTargetReceipt(ctx context.Context, namespace, id string) (*store.SecurityTargetReceipt, error) {
	var receipt store.SecurityTargetReceipt
	var receiptJSON, inventoryJSON []byte
	err := s.db.QueryRowContext(ctx, `SELECT id, namespace, repository_scan, scan_run_id, run_uid, target_id,
		head_sha, object_format, snapshot_digest, tree_digest, receipt_json, inventory_json, inventory_digest, payload_digest, record_digest, created_at
		FROM security_target_receipts WHERE namespace = ? AND id = ?`, namespace, id).Scan(
		&receipt.ID, &receipt.Namespace, &receipt.RepositoryScan, &receipt.ScanRunID, &receipt.RunUID,
		&receipt.TargetID, &receipt.HeadSHA, &receipt.ObjectFormat, &receipt.SnapshotDigest, &receipt.TreeDigest,
		&receiptJSON, &inventoryJSON, &receipt.InventoryDigest, &receipt.PayloadDigest, &receipt.RecordDigest, &receipt.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if !json.Valid(receiptJSON) {
		return nil, fmt.Errorf("stored target receipt is invalid")
	}
	if len(inventoryJSON) > 0 && !json.Valid(inventoryJSON) {
		return nil, fmt.Errorf("stored target inventory is invalid")
	}
	receipt.ReceiptJSON = append(json.RawMessage(nil), receiptJSON...)
	receipt.InventoryJSON = append(json.RawMessage(nil), inventoryJSON...)
	return &receipt, nil
}
