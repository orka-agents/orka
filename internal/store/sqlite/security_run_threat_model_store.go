package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/orka-agents/orka/internal/redact"
	"github.com/orka-agents/orka/internal/store"
)

func normalizeRunThreatModel(model *store.SecurityRunThreatModel) error {
	if model == nil {
		return store.ValidationErrorf("run threat model is required")
	}
	for field, value := range map[string]string{
		"runUID": model.RunUID, "namespace": model.Namespace, "repositoryScan": model.RepositoryScan, "scanRunID": model.ScanRunID,
	} {
		if err := validateSecurityID(value, field, true); err != nil {
			return err
		}
	}
	if err := validateSecurityRunUID(model.RunUID, true); err != nil {
		return err
	}
	if model.Version <= 0 {
		return store.ValidationErrorf("run threat model version must be positive")
	}
	submittedContent := model.Content
	if !utf8.ValidString(submittedContent) {
		return store.ValidationErrorf("run threat model content must be valid UTF-8")
	}
	if len(submittedContent) > maxSecurityPayloadBytes {
		return store.ValidationErrorf("run threat model content exceeds %d bytes", maxSecurityPayloadBytes)
	}
	if model.ContentDigest != "" {
		normalized, err := normalizeSecurityDigest(model.ContentDigest, true, "contentDigest")
		if err != nil {
			return err
		}
		if normalized != securityDigestBytes([]byte(submittedContent)) {
			return store.ValidationErrorf("run threat model content digest mismatch")
		}
	}
	model.Content = redact.SensitiveText(strings.TrimSpace(strings.ReplaceAll(model.Content, "\r\n", "\n")))
	if model.Content == "" || len(model.Content) > maxSecurityPayloadBytes {
		return store.ValidationErrorf("run threat model content is empty or oversized")
	}
	model.ContentDigest = securityDigestBytes([]byte(model.Content))
	if err := validateSecurityID(model.SourceReceiptID, "sourceReceiptID", false); err != nil {
		return err
	}
	return nil
}

func runThreatModelRecordDigest(model store.SecurityRunThreatModel) (string, error) {
	model.RecordDigest = ""
	model.CreatedAt = time.Time{}
	return securityRecordDigest(model)
}

func (s *Store) SaveSecurityRunThreatModel(ctx context.Context, model *store.SecurityRunThreatModel) (bool, error) {
	if err := normalizeRunThreatModel(model); err != nil {
		return false, err
	}
	digest, err := runThreatModelRecordDigest(*model)
	if err != nil {
		return false, err
	}
	if model.CreatedAt.IsZero() {
		model.CreatedAt = time.Now().UTC()
	}
	model.RecordDigest = digest
	result, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO security_run_threat_models
		(run_uid, namespace, repository_scan, scan_run_id, version, content, content_digest, source_receipt_id, record_digest, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, model.RunUID, model.Namespace, model.RepositoryScan, model.ScanRunID,
		model.Version, model.Content, model.ContentDigest, model.SourceReceiptID, model.RecordDigest, model.CreatedAt)
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
	var existing string
	if err := s.db.QueryRowContext(ctx, `SELECT record_digest FROM security_run_threat_models WHERE run_uid = ?`, model.RunUID).Scan(&existing); err != nil {
		return false, err
	}
	return immutableReplayResult(existing, digest, "run threat model", model.RunUID)
}

func (s *Store) GetSecurityRunThreatModel(ctx context.Context, namespace, runUID string) (*store.SecurityRunThreatModel, error) {
	var model store.SecurityRunThreatModel
	err := s.db.QueryRowContext(ctx, `SELECT run_uid, namespace, repository_scan, scan_run_id, version, content,
		content_digest, source_receipt_id, record_digest, created_at FROM security_run_threat_models
		WHERE namespace = ? AND run_uid = ?`, namespace, runUID).Scan(&model.RunUID, &model.Namespace, &model.RepositoryScan,
		&model.ScanRunID, &model.Version, &model.Content, &model.ContentDigest, &model.SourceReceiptID, &model.RecordDigest, &model.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return &model, err
}
