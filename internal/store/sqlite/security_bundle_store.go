package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	securitybundle "github.com/orka-agents/orka/internal/security/bundle"
	"github.com/orka-agents/orka/internal/store"
)

func normalizeSecurityBundle(bundle *store.SecurityScanBundle) error {
	if bundle == nil {
		return store.ValidationErrorf("security bundle is required")
	}
	for field, value := range map[string]string{
		"id": bundle.ID, "namespace": bundle.Namespace, "repositoryScan": bundle.RepositoryScan,
		"repositoryScanUID": bundle.RepositoryScanUID, "scanRunID": bundle.ScanRunID, "runUID": bundle.RunUID,
	} {
		if err := validateSecurityID(value, field, true); err != nil {
			return err
		}
	}
	if err := validateSecurityRunUID(bundle.RunUID, true); err != nil {
		return err
	}
	if bundle.Version != securitybundle.SchemaVersion {
		return store.ValidationErrorf("unsupported bundle version %d", bundle.Version)
	}
	if bundle.RepositoryScanGeneration <= 0 {
		return store.ValidationErrorf("repositoryScanGeneration must be positive")
	}
	for name, payload := range map[string][]byte{"manifest": bundle.ManifestJSON, "findings": bundle.FindingsJSON, "coverage": bundle.CoverageJSON} {
		if len(payload) == 0 || len(payload) > maxSecurityPayloadBytes*4 {
			return store.ValidationErrorf("bundle %s is empty or oversized", name)
		}
		if !json.Valid(payload) {
			return store.ValidationErrorf("bundle %s must be valid JSON", name)
		}
	}
	if len(bundle.EvidenceJSON) == 0 || len(bundle.EvidenceJSON) > 96<<20 {
		return store.ValidationErrorf("bundle evidence is empty or oversized")
	}
	if !json.Valid(bundle.EvidenceJSON) {
		return store.ValidationErrorf("bundle evidence must be valid JSON")
	}
	var err error
	bundle.ContentDigest, err = normalizeSecurityDigest(bundle.ContentDigest, true, "contentDigest")
	if err != nil {
		return err
	}
	bundle.RunReceiptDigest, err = normalizeSecurityDigest(bundle.RunReceiptDigest, true, "runReceiptDigest")
	if err != nil {
		return err
	}
	if bundle.SealedAt.IsZero() {
		return store.ValidationErrorf("sealedAt is required")
	}
	bundle.SealedAt = bundle.SealedAt.UTC()
	if err := verifyStoredSecurityBundle(bundle); err != nil {
		return err
	}
	return nil
}

func skipSecurityBundleJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		for decoder.More() {
			if _, err := decoder.Token(); err != nil {
				return err
			}
			if err := skipSecurityBundleJSONValue(decoder); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := skipSecurityBundleJSONValue(decoder); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
	_, err = decoder.Token()
	return err
}

func countSecurityBundleEvidenceItems(payload []byte, maxItems int) (int, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	opening, err := decoder.Token()
	if err != nil {
		return 0, store.ValidationErrorf("bundle evidence must strictly decode: %v", err)
	}
	if delim, ok := opening.(json.Delim); !ok || delim != '[' {
		return 0, store.ValidationErrorf("bundle evidence must use canonical JSON array representation")
	}
	count := 0
	for decoder.More() {
		count++
		if count > maxItems {
			return 0, store.ValidationErrorf("bundle evidence exceeds %d items", maxItems)
		}
		if err := skipSecurityBundleJSONValue(decoder); err != nil {
			return 0, store.ValidationErrorf("bundle evidence must strictly decode: %v", err)
		}
	}
	if _, err := decoder.Token(); err != nil {
		return 0, store.ValidationErrorf("bundle evidence must strictly decode: %v", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err != nil {
			return 0, store.ValidationErrorf("bundle evidence contains invalid trailing data: %v", err)
		}
		return 0, store.ValidationErrorf("bundle evidence contains trailing JSON data")
	}
	return count, nil
}

func decodeCanonicalSecurityBundleEvidence(payload []byte, limits securitybundle.Limits) ([]securitybundle.EvidenceBlob, error) {
	maxItems := limits.MaxEvidenceBlobs
	count, err := countSecurityBundleEvidenceItems(payload, maxItems)
	if err != nil {
		return nil, err
	}
	if count > maxItems {
		return nil, store.ValidationErrorf("bundle evidence exceeds %d items", maxItems)
	}

	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if _, err := decoder.Token(); err != nil {
		return nil, store.ValidationErrorf("bundle evidence must strictly decode: %v", err)
	}
	evidence := make([]securitybundle.EvidenceBlob, 0, count)
	var totalDecodedBytes int64
	for decoder.More() {
		if len(evidence) >= maxItems {
			return nil, store.ValidationErrorf("bundle evidence exceeds %d items", maxItems)
		}
		var blob securitybundle.EvidenceBlob
		if err := decoder.Decode(&blob); err != nil {
			return nil, store.ValidationErrorf("bundle evidence must strictly decode: %v", err)
		}
		if len(blob.Data) > limits.MaxEvidenceBlobBytes {
			return nil, store.ValidationErrorf(
				"bundle evidence blob %q exceeds maximum decoded size of %d bytes",
				blob.Name,
				limits.MaxEvidenceBlobBytes,
			)
		}
		decodedBytes := int64(len(blob.Data))
		if decodedBytes > limits.MaxTotalEvidenceBytes ||
			totalDecodedBytes > limits.MaxTotalEvidenceBytes-decodedBytes {
			return nil, store.ValidationErrorf(
				"bundle evidence exceeds maximum total decoded size of %d bytes",
				limits.MaxTotalEvidenceBytes,
			)
		}
		totalDecodedBytes += decodedBytes
		evidence = append(evidence, blob)
	}
	if _, err := decoder.Token(); err != nil {
		return nil, store.ValidationErrorf("bundle evidence must strictly decode: %v", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, store.ValidationErrorf("bundle evidence contains invalid trailing data: %v", err)
		}
		return nil, store.ValidationErrorf("bundle evidence contains trailing JSON data")
	}
	canonical, err := json.Marshal(evidence)
	if err != nil {
		return nil, store.ValidationErrorf("bundle evidence must encode canonically: %v", err)
	}
	if !bytes.Equal(canonical, payload) {
		return nil, store.ValidationErrorf("bundle evidence JSON is not canonical")
	}
	return evidence, nil
}

func securityBundleRecordDigest(bundle store.SecurityScanBundle) (string, error) {
	bundle.RecordDigest = ""
	return securityRecordDigest(bundle)
}

func verifyStoredSecurityBundle(bundle *store.SecurityScanBundle) error {
	limits := securitybundle.DefaultLimits()
	evidence, err := decodeCanonicalSecurityBundleEvidence(bundle.EvidenceJSON, limits)
	if err != nil {
		return err
	}
	if err := securitybundle.Verify(&securitybundle.Bundle{
		ManifestJSON: bundle.ManifestJSON,
		FindingsJSON: bundle.FindingsJSON,
		CoverageJSON: bundle.CoverageJSON,
		Evidence:     evidence,
		Roots: securitybundle.RootDigests{
			ContentDigest:    bundle.ContentDigest,
			RunReceiptDigest: bundle.RunReceiptDigest,
		},
	}, limits); err != nil {
		return store.ValidationErrorf("bundle verification failed: %v", err)
	}
	var manifest struct {
		Run struct {
			RunUID                   string  `json:"runUid"`
			PublicRunID              *string `json:"publicRunId"`
			Namespace                string  `json:"namespace"`
			RepositoryScanName       string  `json:"repositoryScanName"`
			RepositoryScanUID        string  `json:"repositoryScanUid"`
			RepositoryScanGeneration int64   `json:"repositoryScanGeneration"`
			SealedAt                 string  `json:"sealedAt"`
		} `json:"run"`
	}
	if err := json.Unmarshal(bundle.ManifestJSON, &manifest); err != nil {
		return store.ValidationErrorf("bundle manifest run envelope must decode: %v", err)
	}
	publicRunID := ""
	if manifest.Run.PublicRunID != nil {
		publicRunID = *manifest.Run.PublicRunID
	}
	if manifest.Run.RunUID != bundle.RunUID || publicRunID != bundle.ScanRunID ||
		manifest.Run.Namespace != bundle.Namespace || manifest.Run.RepositoryScanName != bundle.RepositoryScan ||
		manifest.Run.RepositoryScanUID != bundle.RepositoryScanUID ||
		manifest.Run.RepositoryScanGeneration != bundle.RepositoryScanGeneration ||
		manifest.Run.SealedAt != bundle.SealedAt.Format("2006-01-02T15:04:05.999999999Z07:00") {
		return store.ValidationErrorf("bundle outer metadata does not match canonical manifest")
	}
	return nil
}

func (s *Store) SealSecurityScanBundle(ctx context.Context, bundle *store.SecurityScanBundle) (bool, error) {
	if err := normalizeSecurityBundle(bundle); err != nil {
		return false, err
	}
	digest, err := securityBundleRecordDigest(*bundle)
	if err != nil {
		return false, err
	}
	bundle.RecordDigest = digest
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var existingDigest string
	err = tx.QueryRowContext(ctx, `SELECT record_digest FROM security_scan_bundles WHERE namespace = ? AND scan_run_id = ?`,
		bundle.Namespace, bundle.ScanRunID).Scan(&existingDigest)
	if err == nil {
		return immutableReplayResult(existingDigest, digest, "security scan bundle", bundle.ID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO security_scan_bundles
		(id, namespace, repository_scan, repository_scan_uid, repository_scan_generation, scan_run_id, run_uid, version, manifest_json, findings_json,
		 coverage_json, evidence_json, content_digest, run_receipt_digest, sealed_at, record_digest)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		bundle.ID, bundle.Namespace, bundle.RepositoryScan, bundle.RepositoryScanUID, bundle.RepositoryScanGeneration,
		bundle.ScanRunID, bundle.RunUID, bundle.Version,
		bundle.ManifestJSON, bundle.FindingsJSON, bundle.CoverageJSON, bundle.EvidenceJSON, bundle.ContentDigest,
		bundle.RunReceiptDigest, bundle.SealedAt, bundle.RecordDigest)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) GetSecurityScanBundle(ctx context.Context, namespace, scanRunID string) (*store.SecurityScanBundle, error) {
	var bundle store.SecurityScanBundle
	err := s.db.QueryRowContext(ctx, `SELECT id, namespace, repository_scan, repository_scan_uid, repository_scan_generation, scan_run_id, run_uid, version,
		manifest_json, findings_json, coverage_json, evidence_json, content_digest, run_receipt_digest, sealed_at, record_digest
		FROM security_scan_bundles WHERE namespace = ? AND scan_run_id = ?`, namespace, scanRunID).Scan(
		&bundle.ID, &bundle.Namespace, &bundle.RepositoryScan, &bundle.RepositoryScanUID, &bundle.RepositoryScanGeneration,
		&bundle.ScanRunID, &bundle.RunUID, &bundle.Version,
		&bundle.ManifestJSON, &bundle.FindingsJSON, &bundle.CoverageJSON, &bundle.EvidenceJSON, &bundle.ContentDigest,
		&bundle.RunReceiptDigest, &bundle.SealedAt, &bundle.RecordDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get security scan bundle: %w", err)
	}
	if bundle.RepositoryScanUID == "" || bundle.RepositoryScanGeneration <= 0 {
		return nil, fmt.Errorf("%w: legacy security scan bundle lacks immutable RepositoryScan binding", store.ErrNotReady)
	}
	expectedDigest, err := securityBundleRecordDigest(bundle)
	if err != nil {
		return nil, err
	}
	if expectedDigest != bundle.RecordDigest {
		return nil, fmt.Errorf("%w: security scan bundle record digest mismatch", store.ErrDuplicateMismatch)
	}
	if err := verifyStoredSecurityBundle(&bundle); err != nil {
		return nil, err
	}
	return &bundle, nil
}
