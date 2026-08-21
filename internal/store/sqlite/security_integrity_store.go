package sqlite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/orka-agents/orka/internal/redact"
	securityidentity "github.com/orka-agents/orka/internal/security"
	"github.com/orka-agents/orka/internal/store"
)

const (
	maxSecurityStableIDBytes      = 256
	maxSecurityCoordinateBytes    = 512
	maxSecurityNameBytes          = 512
	maxSecurityReasonCodeBytes    = 128
	maxSecurityReasonBytes        = 4 * 1024
	maxSecuritySummaryBytes       = 64 * 1024
	maxSecurityPayloadBytes       = 1 << 20
	maxSecurityEvidenceReceiptIDs = 256
	maxOccurrenceObservationLinks = 4096
	semanticIdentityAlgorithmV1   = "semantic-v1"
	redactedSecurityJSONValue     = "[REDACTED]"
)

var securityStableIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]*$`)

func ensureSecurityIntegritySchema(db *sql.DB) error {
	if err := ensureSQLiteColumns(db, "security_scan_runs", []sqliteColumnMigration{
		{Name: "run_uid", Definition: "run_uid TEXT NOT NULL DEFAULT ''"},
		{Name: "repository_scan_uid", Definition: "repository_scan_uid TEXT NOT NULL DEFAULT ''"},
		{Name: "repository_scan_generation", Definition: "repository_scan_generation INTEGER NOT NULL DEFAULT 0"},
		{Name: "request_idempotency_key", Definition: "request_idempotency_key TEXT NOT NULL DEFAULT ''"},
		{Name: "resolved_target_key", Definition: "resolved_target_key TEXT NOT NULL DEFAULT ''"},
		{Name: "target_receipt_id", Definition: "target_receipt_id TEXT NOT NULL DEFAULT ''"},
		{Name: "quality_schema_version", Definition: "quality_schema_version INTEGER NOT NULL DEFAULT 1"},
		{Name: "inventory_coverage_status", Definition: "inventory_coverage_status TEXT NOT NULL DEFAULT 'unknown'"},
		{Name: "candidate_coverage_status", Definition: "candidate_coverage_status TEXT NOT NULL DEFAULT 'unknown'"},
		{Name: "coverage_status", Definition: "coverage_status TEXT NOT NULL DEFAULT 'unknown'"},
		{Name: "validation_scope", Definition: "validation_scope TEXT NOT NULL DEFAULT 'unknown'"},
		{Name: "validation_execution", Definition: "validation_execution TEXT NOT NULL DEFAULT 'unknown'"},
		{Name: "attack_path_execution", Definition: "attack_path_execution TEXT NOT NULL DEFAULT 'unknown'"},
		{Name: "analysis_attestation_level", Definition: "analysis_attestation_level TEXT NOT NULL DEFAULT 'unverified'"},
		{Name: "target_verification", Definition: "target_verification TEXT NOT NULL DEFAULT 'unverified'"},
		{Name: "bundle_status", Definition: "bundle_status TEXT NOT NULL DEFAULT 'not_started'"},
		{Name: "authorization_status", Definition: "authorization_status TEXT NOT NULL DEFAULT 'legacy-unverified'"},
		{Name: "isolation_status", Definition: "isolation_status TEXT NOT NULL DEFAULT 'legacy'"},
		{Name: "quality_reason_codes_json", Definition: "quality_reason_codes_json TEXT NOT NULL DEFAULT '[]'"},
	}); err != nil {
		return err
	}
	if err := ensureSQLiteColumns(db, "security_findings", []sqliteColumnMigration{
		{Name: "identity_quality", Definition: "identity_quality TEXT NOT NULL DEFAULT 'legacy'"},
		{Name: "identity_algorithm_version", Definition: "identity_algorithm_version TEXT NOT NULL DEFAULT 'legacy-v2'"},
		{Name: "semantic_fingerprint", Definition: "semantic_fingerprint TEXT NOT NULL DEFAULT ''"},
		{Name: "legacy_fingerprint", Definition: "legacy_fingerprint TEXT NOT NULL DEFAULT ''"},
		{Name: "history_status", Definition: "history_status TEXT NOT NULL DEFAULT 'legacy-unrebuildable'"},
		{Name: "current_occurrence_id", Definition: "current_occurrence_id TEXT NOT NULL DEFAULT ''"},
		{Name: "decision_version", Definition: "decision_version INTEGER NOT NULL DEFAULT 0"},
	}); err != nil {
		return err
	}

	if err := ensureSecurityActiveRepositoryIndex(db); err != nil {
		return err
	}

	statements := []string{
		`CREATE TABLE IF NOT EXISTS security_target_receipts (
			id TEXT PRIMARY KEY,
			namespace TEXT NOT NULL,
			repository_scan TEXT NOT NULL,
			scan_run_id TEXT NOT NULL,
			run_uid TEXT NOT NULL,
			target_id TEXT NOT NULL,
			head_sha TEXT NOT NULL,
			object_format TEXT NOT NULL,
			snapshot_digest TEXT NOT NULL,
			tree_digest TEXT NOT NULL,
			receipt_json BLOB NOT NULL,
			inventory_json BLOB NOT NULL DEFAULT X'',
			inventory_digest TEXT NOT NULL DEFAULT '',
			payload_digest TEXT NOT NULL,
			record_digest TEXT NOT NULL,
			created_at TIMESTAMP NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_security_target_receipts_run
		 ON security_target_receipts(namespace, scan_run_id)`,
		`UPDATE security_findings
		 SET identity_quality = CASE WHEN identity_quality = '' THEN 'legacy' ELSE identity_quality END,
		     identity_algorithm_version = CASE WHEN identity_algorithm_version = '' THEN 'legacy-v2' ELSE identity_algorithm_version END,
		     legacy_fingerprint = CASE WHEN legacy_fingerprint = '' THEN fingerprint ELSE legacy_fingerprint END,
		     history_status = CASE WHEN history_status = '' THEN 'legacy-unrebuildable' ELSE history_status END`,
		`CREATE TABLE IF NOT EXISTS security_run_threat_models (
			run_uid TEXT PRIMARY KEY,
			namespace TEXT NOT NULL,
			repository_scan TEXT NOT NULL,
			scan_run_id TEXT NOT NULL,
			version INTEGER NOT NULL,
			content TEXT NOT NULL,
			content_digest TEXT NOT NULL,
			source_receipt_id TEXT NOT NULL DEFAULT '',
			record_digest TEXT NOT NULL,
			created_at TIMESTAMP NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_security_run_threat_models_scan ON security_run_threat_models(namespace, scan_run_id)`,
		`CREATE TABLE IF NOT EXISTS security_run_task_inputs (
			run_uid TEXT NOT NULL,
			stage TEXT NOT NULL,
			namespace TEXT NOT NULL,
			repository_scan TEXT NOT NULL,
			scan_run_id TEXT NOT NULL,
			source_version INTEGER NOT NULL DEFAULT 0,
			content TEXT NOT NULL DEFAULT '',
			content_digest TEXT NOT NULL,
			record_digest TEXT NOT NULL,
			created_at TIMESTAMP NOT NULL,
			PRIMARY KEY(run_uid, stage)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_security_run_task_inputs_scan
		 ON security_run_task_inputs(namespace, scan_run_id, stage)`,
		`CREATE TABLE IF NOT EXISTS security_scan_bundles (
			id TEXT PRIMARY KEY,
			namespace TEXT NOT NULL,
			repository_scan TEXT NOT NULL,
			repository_scan_uid TEXT NOT NULL DEFAULT '',
			repository_scan_generation INTEGER NOT NULL DEFAULT 0,
			scan_run_id TEXT NOT NULL,
			run_uid TEXT NOT NULL,
			version INTEGER NOT NULL,
			manifest_json BLOB NOT NULL,
			findings_json BLOB NOT NULL,
			coverage_json BLOB NOT NULL,
			evidence_json BLOB NOT NULL DEFAULT X'5b5d',
			content_digest TEXT NOT NULL,
			run_receipt_digest TEXT NOT NULL,
			sealed_at TIMESTAMP NOT NULL,
			record_digest TEXT NOT NULL,
			UNIQUE(namespace, scan_run_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_security_scan_bundles_repo ON security_scan_bundles(namespace, repository_scan, sealed_at DESC)`,
		`CREATE TABLE IF NOT EXISTS security_stage_receipts (
			id TEXT PRIMARY KEY,
			namespace TEXT NOT NULL,
			repository_scan TEXT NOT NULL,
			scan_run_id TEXT NOT NULL,
			run_uid TEXT NOT NULL,
			stage TEXT NOT NULL,
			scope_kind TEXT NOT NULL DEFAULT '',
			scope_id TEXT NOT NULL DEFAULT '',
			execution_kind TEXT NOT NULL,
			execution_provenance_json TEXT NOT NULL,
			expected_target_sha TEXT NOT NULL DEFAULT '',
			observed_target_sha TEXT NOT NULL DEFAULT '',
			target_receipt_id TEXT NOT NULL DEFAULT '',
			agent_name TEXT NOT NULL DEFAULT '',
			agent_uid TEXT NOT NULL DEFAULT '',
			agent_generation INTEGER NOT NULL DEFAULT 0,
			producer_image TEXT NOT NULL DEFAULT '',
			producer_version TEXT NOT NULL DEFAULT '',
			attestation_level TEXT NOT NULL,
			scanner_policy_digest TEXT NOT NULL DEFAULT '',
			normalized_input_digest TEXT NOT NULL DEFAULT '',
			source_artifact_name TEXT NOT NULL DEFAULT '',
			source_artifact_media_type TEXT NOT NULL DEFAULT '',
			source_artifact_size INTEGER NOT NULL DEFAULT 0,
			source_artifact_generation INTEGER NOT NULL DEFAULT 0,
			source_artifact_digest TEXT NOT NULL DEFAULT '',
			controller_ingestion_version TEXT NOT NULL,
			normalized_output_digest TEXT NOT NULL DEFAULT '',
			evidence_digest TEXT NOT NULL DEFAULT '',
			disposition TEXT NOT NULL,
			reason_code TEXT NOT NULL DEFAULT '',
			reason TEXT NOT NULL DEFAULT '',
			started_at TIMESTAMP NOT NULL,
			ingested_at TIMESTAMP NOT NULL,
			completed_at TIMESTAMP,
			record_digest TEXT NOT NULL,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS security_finding_observations (
			id TEXT PRIMARY KEY,
			namespace TEXT NOT NULL,
			repository_scan TEXT NOT NULL,
			scan_run_id TEXT NOT NULL,
			run_uid TEXT NOT NULL,
			stage_receipt_id TEXT NOT NULL,
			target_receipt_id TEXT NOT NULL DEFAULT '',
			slice_id TEXT NOT NULL DEFAULT '',
			pass_id TEXT NOT NULL DEFAULT '',
			candidate_key TEXT NOT NULL DEFAULT '',
			producer_finding_id TEXT NOT NULL DEFAULT '',
			source_artifact_name TEXT NOT NULL DEFAULT '',
			source_artifact_generation INTEGER NOT NULL DEFAULT 0,
			source_artifact_digest TEXT NOT NULL DEFAULT '',
			policy_digest TEXT NOT NULL DEFAULT '',
			ordinal INTEGER NOT NULL,
			disposition TEXT NOT NULL,
			reason_code TEXT NOT NULL DEFAULT '',
			reason TEXT NOT NULL DEFAULT '',
			rule_id TEXT NOT NULL DEFAULT '',
			identity_anchor TEXT NOT NULL DEFAULT '',
			identity_instance TEXT NOT NULL DEFAULT '',
			identity_quality TEXT NOT NULL DEFAULT '',
			identity_algorithm_version TEXT NOT NULL DEFAULT '',
			semantic_fingerprint TEXT NOT NULL DEFAULT '',
			legacy_fingerprint TEXT NOT NULL DEFAULT '',
			normalized_payload BLOB NOT NULL DEFAULT X'',
			payload_digest TEXT NOT NULL DEFAULT '',
			record_digest TEXT NOT NULL,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS security_finding_occurrences (
			id TEXT PRIMARY KEY,
			namespace TEXT NOT NULL,
			repository_scan TEXT NOT NULL,
			scan_run_id TEXT NOT NULL,
			run_uid TEXT NOT NULL,
			public_finding_id TEXT NOT NULL,
			semantic_finding_id TEXT NOT NULL,
			semantic_fingerprint TEXT NOT NULL,
			identity_quality TEXT NOT NULL,
			identity_algorithm_version TEXT NOT NULL,
			legacy_fingerprint TEXT NOT NULL DEFAULT '',
			rule_id TEXT NOT NULL DEFAULT '',
			identity_anchor TEXT NOT NULL DEFAULT '',
			identity_instance TEXT NOT NULL DEFAULT '',
			target_receipt_id TEXT NOT NULL DEFAULT '',
			target_sha TEXT NOT NULL DEFAULT '',
			discovery_payload BLOB NOT NULL DEFAULT X'',
			payload_digest TEXT NOT NULL DEFAULT '',
			record_digest TEXT NOT NULL,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS security_finding_occurrence_observations (
			namespace TEXT NOT NULL,
			occurrence_id TEXT NOT NULL,
			observation_id TEXT NOT NULL,
			relationship TEXT NOT NULL,
			ordinal INTEGER NOT NULL,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY(namespace, occurrence_id, observation_id)
		)`,
		`CREATE TABLE IF NOT EXISTS security_finding_aliases (
			namespace TEXT NOT NULL,
			repository_scan TEXT NOT NULL,
			semantic_fingerprint TEXT NOT NULL,
			semantic_finding_id TEXT NOT NULL,
			public_finding_id TEXT NOT NULL,
			identity_quality TEXT NOT NULL,
			identity_algorithm_version TEXT NOT NULL,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY(namespace, repository_scan, semantic_fingerprint)
		)`,
		`CREATE TABLE IF NOT EXISTS security_finding_assessments (
			id TEXT PRIMARY KEY,
			namespace TEXT NOT NULL,
			repository_scan TEXT NOT NULL,
			scan_run_id TEXT NOT NULL,
			run_uid TEXT NOT NULL,
			occurrence_id TEXT NOT NULL,
			public_finding_id TEXT NOT NULL,
			kind TEXT NOT NULL,
			stage_receipt_id TEXT NOT NULL,
			target_receipt_id TEXT NOT NULL DEFAULT '',
			target_sha TEXT NOT NULL DEFAULT '',
			method TEXT NOT NULL DEFAULT '',
			outcome TEXT NOT NULL,
			failure_class TEXT NOT NULL DEFAULT '',
			summary TEXT NOT NULL DEFAULT '',
			proof_gap TEXT NOT NULL DEFAULT '',
			evidence_receipt_ids_json TEXT NOT NULL DEFAULT '[]',
			normalized_payload BLOB NOT NULL DEFAULT X'',
			payload_digest TEXT NOT NULL DEFAULT '',
			projection_validation_status TEXT NOT NULL DEFAULT '',
			projection_evidence_json TEXT NOT NULL DEFAULT '[]',
			record_digest TEXT NOT NULL,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS security_finding_decisions (
			id TEXT NOT NULL,
			namespace TEXT NOT NULL,
			repository_scan TEXT NOT NULL,
			public_finding_id TEXT NOT NULL,
			scope TEXT NOT NULL,
			occurrence_id TEXT NOT NULL DEFAULT '',
			action TEXT NOT NULL,
			reason_code TEXT NOT NULL DEFAULT '',
			reason TEXT NOT NULL DEFAULT '',
			evidence_receipt_ids_json TEXT NOT NULL DEFAULT '[]',
			supersedes_decision_id TEXT NOT NULL DEFAULT '',
			expected_decision_version INTEGER NOT NULL,
			decision_version INTEGER NOT NULL,
			applicability_json TEXT NOT NULL DEFAULT '',
			actor_subject TEXT NOT NULL,
			actor_issuer TEXT NOT NULL DEFAULT '',
			authentication_source TEXT NOT NULL,
			source TEXT NOT NULL DEFAULT '',
			feedback_eligible BOOLEAN NOT NULL DEFAULT FALSE,
			record_digest TEXT NOT NULL,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY(namespace, id),
			UNIQUE(namespace, public_finding_id, decision_version)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_security_stage_receipts_run
		 ON security_stage_receipts(namespace, repository_scan, scan_run_id, stage, created_at, id)`,
		`CREATE INDEX IF NOT EXISTS idx_security_stage_receipts_scope
		 ON security_stage_receipts(namespace, scan_run_id, scope_id, disposition, created_at, id)`,
		`CREATE INDEX IF NOT EXISTS idx_security_finding_observations_run
		 ON security_finding_observations(namespace, repository_scan, scan_run_id, disposition, created_at, id)`,
		`CREATE INDEX IF NOT EXISTS idx_security_finding_observations_semantic
		 ON security_finding_observations(namespace, repository_scan, semantic_fingerprint, created_at, id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_security_finding_observations_source
		 ON security_finding_observations(namespace, stage_receipt_id, source_artifact_name,
		 source_artifact_generation, ordinal)`,
		`CREATE INDEX IF NOT EXISTS idx_security_finding_occurrences_finding
		 ON security_finding_occurrences(namespace, public_finding_id, created_at, id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_security_finding_occurrences_canonical_semantic
		 ON security_finding_occurrences(namespace, repository_scan, scan_run_id, semantic_fingerprint)
		 WHERE identity_quality = 'canonical'`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_security_finding_occurrence_observations_observation
		 ON security_finding_occurrence_observations(namespace, observation_id)`,
		`CREATE INDEX IF NOT EXISTS idx_security_finding_aliases_public
		 ON security_finding_aliases(namespace, public_finding_id, created_at)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_security_finding_aliases_repository_public
		 ON security_finding_aliases(namespace, repository_scan, public_finding_id)`,
		`CREATE INDEX IF NOT EXISTS idx_security_finding_assessments_occurrence
		 ON security_finding_assessments(namespace, occurrence_id, kind, created_at, id)`,
		`CREATE INDEX IF NOT EXISTS idx_security_finding_decisions_finding
		 ON security_finding_decisions(namespace, public_finding_id, decision_version, id)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			return fmt.Errorf("migration failed: %w", err)
		}
	}

	if err := ensureSQLiteColumns(db, "security_scan_bundles", []sqliteColumnMigration{
		{Name: "evidence_json", Definition: "evidence_json BLOB NOT NULL DEFAULT X'5b5d'"},
		{Name: "repository_scan_uid", Definition: "repository_scan_uid TEXT NOT NULL DEFAULT ''"},
		{Name: "repository_scan_generation", Definition: "repository_scan_generation INTEGER NOT NULL DEFAULT 0"},
	}); err != nil {
		return err
	}

	if err := ensureSQLiteColumns(db, "security_target_receipts", []sqliteColumnMigration{
		{Name: "inventory_json", Definition: "inventory_json BLOB NOT NULL DEFAULT X''"},
		{Name: "inventory_digest", Definition: "inventory_digest TEXT NOT NULL DEFAULT ''"},
	}); err != nil {
		return err
	}
	if err := ensureSQLiteColumns(db, "security_finding_assessments", []sqliteColumnMigration{
		{Name: "projection_evidence_json", Definition: "projection_evidence_json TEXT NOT NULL DEFAULT '[]'"},
	}); err != nil {
		return err
	}

	for _, table := range []string{
		"security_target_receipts",
		"security_run_threat_models",
		"security_run_task_inputs",
		"security_scan_bundles",
		"security_stage_receipts",
		"security_finding_observations",
		"security_finding_occurrences",
		"security_finding_occurrence_observations",
		"security_finding_aliases",
		"security_finding_assessments",
		"security_finding_decisions",
	} {
		if err := createSecurityImmutableTriggers(db, table); err != nil {
			return err
		}
	}
	return nil
}

const duplicateSealingMigrationMessage = "migration failed closed: duplicate active repository reservation included a sealing run"

func ensureSecurityActiveRepositoryIndex(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migration failed: begin active-repository index migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, indexName := range []string{
		"idx_security_scan_runs_active_request",
		"idx_security_scan_runs_active_repository",
	} {
		if _, err := tx.Exec(`DROP INDEX IF EXISTS ` + indexName); err != nil {
			return fmt.Errorf("migration failed: drop %s index: %w", indexName, err)
		}
	}
	if _, err := tx.Exec(`UPDATE security_scan_runs
		SET request_idempotency_key = idempotency_key
		WHERE request_idempotency_key = '' AND idempotency_key <> ''`); err != nil {
		return fmt.Errorf("migration failed: backfill active-repository request keys: %w", err)
	}

	// The legacy index scoped reservations by request key and did not retain
	// sealing runs. If a pending/running run was admitted while another request
	// for the repository was active or sealing, the repository-wide index cannot
	// be created. Preserve every row and its immutable evidence, but fail every
	// ambiguous sealing participant closed. Any active phase on such a row is
	// terminalized as well so it cannot remain in the new index predicate.
	if _, err := tx.Exec(`WITH duplicate_groups AS (
		SELECT namespace, repository_scan
		FROM security_scan_runs
		WHERE run_uid <> '' AND request_idempotency_key <> ''
		  AND (phase IN ('pending', 'running') OR bundle_status = 'sealing')
		GROUP BY namespace, repository_scan
		HAVING COUNT(*) > 1
		   AND SUM(CASE WHEN bundle_status = 'sealing' THEN 1 ELSE 0 END) > 0
	)
	UPDATE security_scan_runs
	SET bundle_status = 'failed',
	    phase = CASE WHEN phase IN ('pending', 'running') THEN 'failed' ELSE phase END,
	    completed_at = COALESCE(completed_at, started_at),
	    error_message = CASE
	      WHEN INSTR(error_message, ?) > 0 THEN error_message
	      WHEN TRIM(error_message) = '' THEN ?
	      ELSE error_message || '; ' || ?
	    END
	WHERE bundle_status = 'sealing'
	  AND EXISTS (
	    SELECT 1 FROM duplicate_groups
	    WHERE duplicate_groups.namespace = security_scan_runs.namespace
	      AND duplicate_groups.repository_scan = security_scan_runs.repository_scan
	  )`, duplicateSealingMigrationMessage, duplicateSealingMigrationMessage, duplicateSealingMigrationMessage); err != nil {
		return fmt.Errorf("migration failed: reconcile duplicate sealing repository reservations: %w", err)
	}

	var duplicateGroups int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM (
		SELECT 1
		FROM security_scan_runs
		WHERE run_uid <> '' AND request_idempotency_key <> ''
		  AND (phase IN ('pending', 'running') OR bundle_status = 'sealing')
		GROUP BY namespace, repository_scan
		HAVING COUNT(*) > 1
	)`).Scan(&duplicateGroups); err != nil {
		return fmt.Errorf("migration failed: preflight active-repository index: %w", err)
	}
	if duplicateGroups != 0 {
		return fmt.Errorf("migration failed: %d duplicate active repository groups remain after sealing reconciliation", duplicateGroups)
	}

	if _, err := tx.Exec(`CREATE UNIQUE INDEX idx_security_scan_runs_active_repository
		ON security_scan_runs(namespace, repository_scan)
		WHERE run_uid <> '' AND request_idempotency_key <> ''
		  AND (phase IN ('pending', 'running') OR bundle_status = 'sealing')`); err != nil {
		return fmt.Errorf("migration failed: create active-repository index: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migration failed: commit active-repository index migration: %w", err)
	}
	return nil
}

func createSecurityImmutableTriggers(db *sql.DB, table string) error {
	for operation, timing := range map[string]string{"update": "UPDATE", "delete": "DELETE"} {
		statement := fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS trg_%s_immutable_%s
			BEFORE %s ON %s
			BEGIN
				SELECT RAISE(ABORT, '%s is immutable');
			END`, table, operation, timing, table, table)
		if _, err := db.Exec(statement); err != nil {
			return fmt.Errorf("migration failed: %w", err)
		}
	}
	return nil
}

func securityDigestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func securityRecordDigest(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return securityDigestBytes(data), nil
}

func normalizeSecurityDigest(value string, required bool, field string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		if required {
			return "", store.ValidationErrorf("%s is required", field)
		}
		return "", nil
	}
	value = strings.TrimPrefix(value, "sha256:")
	if len(value) != sha256.Size*2 {
		return "", store.ValidationErrorf("%s must be a full SHA-256 digest", field)
	}
	if _, err := hex.DecodeString(value); err != nil {
		return "", store.ValidationErrorf("%s must be a full SHA-256 digest", field)
	}
	return "sha256:" + value, nil
}

func normalizeSecurityPayload(payload json.RawMessage, suppliedDigest string, required bool, field string) (json.RawMessage, string, error) {
	payload = append(json.RawMessage(nil), payload...)
	if len(payload) > maxSecurityPayloadBytes {
		return nil, "", store.ValidationErrorf("%s exceeds %d bytes", field, maxSecurityPayloadBytes)
	}
	if len(payload) == 0 {
		if required {
			return nil, "", store.ValidationErrorf("%s is required", field)
		}
		digest, err := normalizeSecurityDigest(suppliedDigest, false, field+"Digest")
		return nil, digest, err
	}
	if !utf8.Valid(payload) {
		return nil, "", store.ValidationErrorf("%s must be valid UTF-8", field)
	}
	if !json.Valid(payload) {
		return nil, "", store.ValidationErrorf("%s must be valid JSON", field)
	}
	if strings.TrimSpace(suppliedDigest) != "" {
		normalized, err := normalizeSecurityDigest(suppliedDigest, true, field+"Digest")
		if err != nil {
			return nil, "", err
		}
		if normalized != securityDigestBytes(payload) {
			return nil, "", store.ValidationErrorf("%s digest does not match payload", field)
		}
	}

	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, "", store.ValidationErrorf("%s must be valid JSON", field)
	}
	normalizedValue, _, err := redactSecurityJSONValue(value, "")
	if err != nil {
		return nil, "", err
	}
	canonical, err := json.Marshal(normalizedValue)
	if err != nil {
		return nil, "", store.ValidationErrorf("%s could not be canonically encoded", field)
	}
	if len(canonical) > maxSecurityPayloadBytes {
		return nil, "", store.ValidationErrorf("%s exceeds %d bytes after canonical encoding", field, maxSecurityPayloadBytes)
	}
	payload = append(json.RawMessage(nil), canonical...)
	return payload, securityDigestBytes(payload), nil
}

func redactSecurityJSONValue(value any, field string) (any, bool, error) {
	if targetJSONFieldIsSensitive(field) {
		return redactedSecurityJSONValue, true, nil
	}
	switch typed := value.(type) {
	case string:
		redacted := redact.SensitiveText(typed)
		if field != "" {
			contextual := field + ": " + redacted
			if redact.SensitiveText(contextual) != contextual {
				redacted = redactedSecurityJSONValue
			}
		}
		return redacted, redacted != typed, nil
	case []any:
		normalized := make([]any, len(typed))
		changed := false
		for i := range typed {
			item, itemChanged, err := redactSecurityJSONValue(typed[i], field)
			if err != nil {
				return nil, false, err
			}
			normalized[i] = item
			changed = changed || itemChanged
		}
		return normalized, changed, nil
	case map[string]any:
		normalized := make(map[string]any, len(typed))
		changed := false
		for key, item := range typed {
			redactedKey := redact.SensitiveText(key)
			childField := redactedKey
			if field != "" {
				childField = field + "." + redactedKey
			}
			normalizedItem, itemChanged, err := redactSecurityJSONValue(item, childField)
			if err != nil {
				return nil, false, err
			}
			if _, exists := normalized[redactedKey]; exists {
				return nil, false, store.ValidationErrorf("JSON object keys collide after credential redaction")
			}
			normalized[redactedKey] = normalizedItem
			changed = changed || redactedKey != key || itemChanged
		}
		return normalized, changed, nil
	default:
		return value, false, nil
	}
}

func securityPayloadBytes(payload json.RawMessage) []byte {
	if len(payload) == 0 {
		return []byte{}
	}
	return []byte(payload)
}

func validateSecurityText(value, field string, maxBytes int, required bool) error {
	if required && strings.TrimSpace(value) == "" {
		return store.ValidationErrorf("%s is required", field)
	}
	if !utf8.ValidString(value) {
		return store.ValidationErrorf("%s must be valid UTF-8", field)
	}
	if len(value) > maxBytes {
		return store.ValidationErrorf("%s exceeds %d bytes", field, maxBytes)
	}
	if redact.SensitiveText(value) != value {
		return store.ValidationErrorf("%s contains credential-like content", field)
	}
	return nil
}

func validateSecurityID(value, field string, required bool) error {
	if value != strings.TrimSpace(value) {
		return store.ValidationErrorf("%s must not contain surrounding whitespace", field)
	}
	if err := validateSecurityText(value, field, maxSecurityStableIDBytes, required); err != nil {
		return err
	}
	if value != "" && !securityStableIDPattern.MatchString(value) {
		return store.ValidationErrorf("%s contains unsupported characters", field)
	}
	return nil
}

func validateSecurityRunUID(value string, required bool) error {
	if value != strings.TrimSpace(value) {
		return store.ValidationErrorf("runUID must not contain surrounding whitespace")
	}
	if value == "" {
		if required {
			return store.ValidationErrorf("runUID is required")
		}
		return nil
	}
	if !strings.HasPrefix(value, "run_") || len(value) != len("run_")+sha256.Size*2 {
		return store.ValidationErrorf("runUID must contain a full-width 256-bit identity")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(value, "run_")); err != nil {
		return store.ValidationErrorf("runUID must contain a full-width 256-bit identity")
	}
	return nil
}

func normalizeSecurityStringList(values []string, field string, maxCount int) ([]string, error) {
	if len(values) > maxCount {
		return nil, store.ValidationErrorf("%s exceeds %d items", field, maxCount)
	}
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if err := validateSecurityID(value, field, true); err != nil {
			return nil, err
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out, nil
}

func validateSecurityReason(code, reason string, required bool) error {
	if err := validateSecurityText(code, "reasonCode", maxSecurityReasonCodeBytes, false); err != nil {
		return err
	}
	return validateSecurityText(reason, "reason", maxSecurityReasonBytes, required)
}

func normalizeSecurityTime(value time.Time) time.Time {
	if value.IsZero() {
		return time.Time{}
	}
	return value.UTC()
}

func normalizeSecurityTimePtr(value *time.Time) *time.Time {
	if value == nil || value.IsZero() {
		return nil
	}
	normalized := value.UTC()
	return &normalized
}

func immutableReplayResult(existingDigest, candidateDigest, kind, id string) (bool, error) {
	if existingDigest == candidateDigest {
		return false, nil
	}
	return false, fmt.Errorf("%w: %s %q was replayed with different canonical content", store.ErrDuplicateMismatch, kind, id)
}

func normalizeExecutionProvenance(provenance *store.ExecutionProvenance) error {
	if provenance == nil {
		return store.ValidationErrorf("provenance is required")
	}
	members := 0
	if provenance.Kubernetes != nil {
		members++
	}
	if provenance.Harness != nil {
		members++
	}
	if provenance.ExternalAdapter != nil {
		members++
	}
	if members != 1 {
		return store.ValidationErrorf("provenance must set exactly one execution identity")
	}
	switch provenance.Kind {
	case store.ExecutionProvenanceKubernetes:
		if provenance.Kubernetes == nil || members != 1 {
			return store.ValidationErrorf("kubernetes provenance does not match kind")
		}
		p := provenance.Kubernetes
		if err := validateSecurityID(p.TaskName, "provenance.taskName", true); err != nil {
			return err
		}
		if err := validateSecurityID(p.TaskUID, "provenance.taskUID", true); err != nil {
			return err
		}
		if p.Attempt < 0 {
			return store.ValidationErrorf("provenance.attempt must be non-negative")
		}
		for field, value := range map[string]string{
			"provenance.jobName": p.JobName,
			"provenance.jobUID":  p.JobUID,
			"provenance.podName": p.PodName,
			"provenance.podUID":  p.PodUID,
		} {
			if err := validateSecurityID(value, field, false); err != nil {
				return err
			}
		}
	case store.ExecutionProvenanceHarness:
		if provenance.Harness == nil || members != 1 {
			return store.ValidationErrorf("harness provenance does not match kind")
		}
		p := provenance.Harness
		for field, value := range map[string]string{
			"provenance.runtimeSessionID": p.RuntimeSessionID,
			"provenance.turnID":           p.TurnID,
			"provenance.correlationID":    p.CorrelationID,
		} {
			if err := validateSecurityID(value, field, true); err != nil {
				return err
			}
		}
		if err := validateSecurityText(p.RuntimeName, "provenance.runtimeName", maxSecurityNameBytes, false); err != nil {
			return err
		}
		if err := validateSecurityText(p.RuntimeVersion, "provenance.runtimeVersion", maxSecurityNameBytes, false); err != nil {
			return err
		}
	case store.ExecutionProvenanceExternalAdapter:
		if provenance.ExternalAdapter == nil || members != 1 {
			return store.ValidationErrorf("external adapter provenance does not match kind")
		}
		p := provenance.ExternalAdapter
		p.Attestation = redact.SensitiveText(p.Attestation)
		if err := validateSecurityText(p.RuntimeName, "provenance.runtimeName", maxSecurityNameBytes, true); err != nil {
			return err
		}
		if err := validateSecurityID(p.InvocationID, "provenance.invocationID", true); err != nil {
			return err
		}
		if err := validateSecurityID(p.RuntimeUID, "provenance.runtimeUID", false); err != nil {
			return err
		}
		if p.RuntimeGeneration < 0 {
			return store.ValidationErrorf("provenance.runtimeGeneration must be non-negative")
		}
		if err := validateSecurityText(p.AdapterVersion, "provenance.adapterVersion", maxSecurityNameBytes, false); err != nil {
			return err
		}
		if err := validateSecurityText(p.Attestation, "provenance.attestation", maxSecurityReasonBytes, false); err != nil {
			return err
		}
	default:
		return store.ValidationErrorf("unsupported provenance kind %q", provenance.Kind)
	}
	return nil
}

func normalizeGitObjectID(value, field string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "", nil
	}
	if len(value) != 40 && len(value) != 64 {
		return "", store.ValidationErrorf("%s must be a full Git object ID", field)
	}
	if _, err := hex.DecodeString(value); err != nil {
		return "", store.ValidationErrorf("%s must be hexadecimal", field)
	}
	return value, nil
}

func normalizeStageReceipt(receipt *store.StageReceipt) error {
	if receipt == nil {
		return store.ValidationErrorf("stage receipt is required")
	}
	for field, value := range map[string]string{
		"id":             receipt.ID,
		"namespace":      receipt.Namespace,
		"repositoryScan": receipt.RepositoryScan,
		"scanRunID":      receipt.ScanRunID,
		"runUID":         receipt.RunUID,
		"stage":          receipt.Stage,
	} {
		if err := validateSecurityID(value, field, true); err != nil {
			return err
		}
	}
	if err := validateSecurityRunUID(receipt.RunUID, true); err != nil {
		return err
	}
	for field, value := range map[string]string{
		"scopeKind":       receipt.ScopeKind,
		"scopeID":         receipt.ScopeID,
		"targetReceiptID": receipt.TargetReceiptID,
		"agentUID":        receipt.AgentUID,
	} {
		if err := validateSecurityID(value, field, false); err != nil {
			return err
		}
	}
	for field, value := range map[string]string{
		"expectedTargetSHA":          receipt.ExpectedTargetSHA,
		"observedTargetSHA":          receipt.ObservedTargetSHA,
		"agentName":                  receipt.AgentName,
		"producerImage":              receipt.ProducerImage,
		"producerVersion":            receipt.ProducerVersion,
		"sourceArtifactName":         receipt.SourceArtifactName,
		"sourceArtifactMediaType":    receipt.SourceArtifactMediaType,
		"controllerIngestionVersion": receipt.ControllerIngestionVersion,
	} {
		if err := validateSecurityText(value, field, maxSecurityNameBytes, field == "controllerIngestionVersion"); err != nil {
			return err
		}
	}
	var oidErr error
	receipt.ExpectedTargetSHA, oidErr = normalizeGitObjectID(receipt.ExpectedTargetSHA, "expectedTargetSHA")
	if oidErr != nil {
		return oidErr
	}
	receipt.ObservedTargetSHA, oidErr = normalizeGitObjectID(receipt.ObservedTargetSHA, "observedTargetSHA")
	if oidErr != nil {
		return oidErr
	}
	if err := normalizeExecutionProvenance(&receipt.Provenance); err != nil {
		return err
	}
	if receipt.AgentGeneration < 0 || receipt.SourceArtifactSize < 0 || receipt.SourceArtifactGeneration < 0 {
		return store.ValidationErrorf("receipt generations and artifact size must be non-negative")
	}
	if !slices.Contains([]store.AnalysisAttestationLevel{
		store.AnalysisAttestationDelivered,
		store.AnalysisAttestationToolObserved,
		store.AnalysisAttestationBrokered,
		store.AnalysisAttestationUnverified,
	}, receipt.AttestationLevel) {
		return store.ValidationErrorf("unsupported attestationLevel %q", receipt.AttestationLevel)
	}
	if !slices.Contains([]store.StageReceiptDisposition{
		store.StageReceiptAccepted,
		store.StageReceiptRejected,
		store.StageReceiptDeferred,
		store.StageReceiptSuperseded,
	}, receipt.Disposition) {
		return store.ValidationErrorf("unsupported stage receipt disposition %q", receipt.Disposition)
	}
	receipt.Reason = redact.SensitiveText(receipt.Reason)
	if err := validateSecurityReason(receipt.ReasonCode, receipt.Reason, receipt.Disposition != store.StageReceiptAccepted); err != nil {
		return err
	}
	var err error
	for field, target := range map[string]*string{
		"scannerPolicyDigest":    &receipt.ScannerPolicyDigest,
		"normalizedInputDigest":  &receipt.NormalizedInputDigest,
		"sourceArtifactDigest":   &receipt.SourceArtifactDigest,
		"normalizedOutputDigest": &receipt.NormalizedOutputDigest,
		"evidenceDigest":         &receipt.EvidenceDigest,
	} {
		*target, err = normalizeSecurityDigest(*target, false, field)
		if err != nil {
			return err
		}
	}
	receipt.StartedAt = normalizeSecurityTime(receipt.StartedAt)
	receipt.IngestedAt = normalizeSecurityTime(receipt.IngestedAt)
	receipt.CompletedAt = normalizeSecurityTimePtr(receipt.CompletedAt)
	if receipt.StartedAt.IsZero() || receipt.IngestedAt.IsZero() {
		return store.ValidationErrorf("startedAt and ingestedAt are required")
	}
	if receipt.CompletedAt != nil && receipt.CompletedAt.Before(receipt.StartedAt) {
		return store.ValidationErrorf("completedAt cannot precede startedAt")
	}
	if receipt.IngestedAt.Before(receipt.StartedAt) {
		return store.ValidationErrorf("ingestedAt cannot precede startedAt")
	}
	return nil
}

func stageReceiptDigest(receipt store.StageReceipt) (string, error) {
	receipt.RecordDigest = ""
	receipt.CreatedAt = time.Time{}
	return securityRecordDigest(receipt)
}

// AppendStageReceipt inserts one immutable receipt. Exact replay is a no-op.
func (s *Store) AppendStageReceipt(ctx context.Context, receipt *store.StageReceipt) (bool, error) {
	if err := normalizeStageReceipt(receipt); err != nil {
		return false, err
	}
	digest, err := stageReceiptDigest(*receipt)
	if err != nil {
		return false, err
	}
	provenanceJSON, err := json.Marshal(receipt.Provenance)
	if err != nil {
		return false, err
	}
	if receipt.CreatedAt.IsZero() {
		receipt.CreatedAt = time.Now().UTC()
	} else {
		receipt.CreatedAt = receipt.CreatedAt.UTC()
	}
	receipt.RecordDigest = digest

	var existingDigest string
	err = s.db.QueryRowContext(ctx, `SELECT record_digest FROM security_stage_receipts WHERE id = ?`, receipt.ID).Scan(&existingDigest)
	if err == nil {
		return immutableReplayResult(existingDigest, digest, "stage receipt", receipt.ID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO security_stage_receipts
		(id, namespace, repository_scan, scan_run_id, run_uid, stage, scope_kind, scope_id,
		 execution_kind, execution_provenance_json, expected_target_sha, observed_target_sha, target_receipt_id,
		 agent_name, agent_uid, agent_generation, producer_image, producer_version, attestation_level,
		 scanner_policy_digest, normalized_input_digest, source_artifact_name, source_artifact_media_type,
		 source_artifact_size, source_artifact_generation, source_artifact_digest, controller_ingestion_version,
		 normalized_output_digest, evidence_digest, disposition, reason_code, reason, started_at, ingested_at,
		 completed_at, record_digest, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		receipt.ID, receipt.Namespace, receipt.RepositoryScan, receipt.ScanRunID, receipt.RunUID, receipt.Stage,
		receipt.ScopeKind, receipt.ScopeID, receipt.Provenance.Kind, string(provenanceJSON), receipt.ExpectedTargetSHA,
		receipt.ObservedTargetSHA, receipt.TargetReceiptID, receipt.AgentName, receipt.AgentUID, receipt.AgentGeneration,
		receipt.ProducerImage, receipt.ProducerVersion, receipt.AttestationLevel, receipt.ScannerPolicyDigest,
		receipt.NormalizedInputDigest, receipt.SourceArtifactName, receipt.SourceArtifactMediaType, receipt.SourceArtifactSize,
		receipt.SourceArtifactGeneration, receipt.SourceArtifactDigest, receipt.ControllerIngestionVersion,
		receipt.NormalizedOutputDigest, receipt.EvidenceDigest, receipt.Disposition, receipt.ReasonCode, receipt.Reason,
		receipt.StartedAt, receipt.IngestedAt, receipt.CompletedAt, receipt.RecordDigest, receipt.CreatedAt,
	)
	if err != nil {
		if replay, replayErr := s.GetStageReceipt(ctx, receipt.Namespace, receipt.ID); replayErr == nil {
			return immutableReplayResult(replay.RecordDigest, digest, "stage receipt", receipt.ID)
		}
		return false, err
	}
	return true, nil
}

func scanStageReceipt(scanner interface{ Scan(...any) error }) (*store.StageReceipt, error) {
	var receipt store.StageReceipt
	var provenanceJSON string
	err := scanner.Scan(
		&receipt.ID, &receipt.Namespace, &receipt.RepositoryScan, &receipt.ScanRunID, &receipt.RunUID,
		&receipt.Stage, &receipt.ScopeKind, &receipt.ScopeID, &receipt.Provenance.Kind, &provenanceJSON,
		&receipt.ExpectedTargetSHA, &receipt.ObservedTargetSHA, &receipt.TargetReceiptID, &receipt.AgentName,
		&receipt.AgentUID, &receipt.AgentGeneration, &receipt.ProducerImage, &receipt.ProducerVersion,
		&receipt.AttestationLevel, &receipt.ScannerPolicyDigest, &receipt.NormalizedInputDigest,
		&receipt.SourceArtifactName, &receipt.SourceArtifactMediaType, &receipt.SourceArtifactSize,
		&receipt.SourceArtifactGeneration, &receipt.SourceArtifactDigest, &receipt.ControllerIngestionVersion,
		&receipt.NormalizedOutputDigest, &receipt.EvidenceDigest, &receipt.Disposition, &receipt.ReasonCode,
		&receipt.Reason, &receipt.StartedAt, &receipt.IngestedAt, &receipt.CompletedAt, &receipt.RecordDigest,
		&receipt.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(provenanceJSON), &receipt.Provenance); err != nil {
		return nil, err
	}
	return &receipt, nil
}

const stageReceiptSelectColumns = `id, namespace, repository_scan, scan_run_id, run_uid, stage, scope_kind, scope_id,
	execution_kind, execution_provenance_json, expected_target_sha, observed_target_sha, target_receipt_id,
	agent_name, agent_uid, agent_generation, producer_image, producer_version, attestation_level,
	scanner_policy_digest, normalized_input_digest, source_artifact_name, source_artifact_media_type,
	source_artifact_size, source_artifact_generation, source_artifact_digest, controller_ingestion_version,
	normalized_output_digest, evidence_digest, disposition, reason_code, reason, started_at, ingested_at,
	completed_at, record_digest, created_at`

// GetStageReceipt returns one immutable stage receipt.
func (s *Store) GetStageReceipt(ctx context.Context, namespace, id string) (*store.StageReceipt, error) {
	receipt, err := scanStageReceipt(s.db.QueryRowContext(ctx,
		`SELECT `+stageReceiptSelectColumns+` FROM security_stage_receipts WHERE namespace = ? AND id = ?`, namespace, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return receipt, err
}

// ListStageReceipts lists immutable stage receipts oldest first.
func (s *Store) ListStageReceipts(ctx context.Context, filter store.StageReceiptFilter) ([]store.StageReceipt, string, error) {
	offset, err := parseOffsetCursor(filter.Cursor)
	if err != nil {
		return nil, "", err
	}
	filter.Limit = boundedFindingHistoryLimit(filter.Limit)
	query := strings.Builder{}
	query.WriteString(`SELECT ` + stageReceiptSelectColumns + ` FROM security_stage_receipts WHERE namespace = ?`)
	args := []any{filter.Namespace}
	appendSecurityStringFilter(&query, &args, "repository_scan", filter.RepositoryScan)
	appendSecurityStringFilter(&query, &args, "scan_run_id", filter.ScanRunID)
	appendSecurityStringFilter(&query, &args, "stage", filter.Stage)
	appendSecurityStringFilter(&query, &args, "scope_id", filter.ScopeID)
	appendSecurityStringFilter(&query, &args, "disposition", string(filter.Disposition))
	query.WriteString(` ORDER BY created_at ASC, id ASC LIMIT ? OFFSET ?`)
	args = append(args, filter.Limit, offset)
	rows, err := s.db.QueryContext(ctx, query.String(), args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close() //nolint:errcheck
	items := make([]store.StageReceipt, 0)
	for rows.Next() {
		item, err := scanStageReceipt(rows)
		if err != nil {
			return nil, "", err
		}
		items = append(items, *item)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	return items, nextOffsetCursor(offset, len(items), filter.Limit), nil
}

func appendSecurityStringFilter(query *strings.Builder, args *[]any, column, value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	query.WriteString(` AND ` + column + ` = ?`)
	*args = append(*args, value)
}

func normalizeFindingObservation(observation *store.FindingObservation) error {
	if observation == nil {
		return store.ValidationErrorf("finding observation is required")
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"id", observation.ID},
		{"namespace", observation.Namespace},
		{"repositoryScan", observation.RepositoryScan},
		{"scanRunID", observation.ScanRunID},
		{"runUID", observation.RunUID},
		{"stageReceiptID", observation.StageReceiptID},
	} {
		if err := validateSecurityID(field.value, field.name, true); err != nil {
			return err
		}
	}
	if err := validateSecurityRunUID(observation.RunUID, true); err != nil {
		return err
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"targetReceiptID", observation.TargetReceiptID},
		{"sliceID", observation.SliceID},
		{"passID", observation.PassID},
		{"candidateKey", observation.CandidateKey},
		{"producerFindingID", observation.ProducerFindingID},
	} {
		if err := validateSecurityID(field.value, field.name, false); err != nil {
			return err
		}
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"sourceArtifactName", observation.SourceArtifactName},
		{"ruleID", observation.RuleID},
		{"identityAnchor", observation.IdentityAnchor},
		{"identityInstance", observation.IdentityInstance},
		{"identityQuality", observation.IdentityQuality},
		{"identityAlgorithmVersion", observation.IdentityAlgorithmVersion},
		{"legacyFingerprint", observation.LegacyFingerprint},
	} {
		if err := validateSecurityText(field.value, field.name, maxSecurityNameBytes, false); err != nil {
			return err
		}
	}
	if observation.SourceArtifactGeneration < 0 || observation.Ordinal < 0 {
		return store.ValidationErrorf("observation generation and ordinal must be non-negative")
	}
	if !slices.Contains([]store.FindingObservationDisposition{
		store.FindingObservationAccepted,
		store.FindingObservationRejected,
		store.FindingObservationDeferred,
	}, observation.Disposition) {
		return store.ValidationErrorf("unsupported observation disposition %q", observation.Disposition)
	}
	if observation.Disposition == store.FindingObservationAccepted {
		if !slices.Contains([]string{
			store.IdentityQualityCanonical,
			store.IdentityQualityProducerProposed,
			store.IdentityQualityLegacy,
		}, observation.IdentityQuality) {
			return store.ValidationErrorf("accepted observation has unsupported identityQuality %q", observation.IdentityQuality)
		}
		if strings.TrimSpace(observation.IdentityAlgorithmVersion) == "" {
			return store.ValidationErrorf("accepted observation identityAlgorithmVersion is required")
		}
		if observation.IdentityQuality == store.IdentityQualityCanonical {
			if observation.IdentityAlgorithmVersion != semanticIdentityAlgorithmV1 || strings.TrimSpace(observation.RuleID) == "" ||
				strings.TrimSpace(observation.IdentityAnchor) == "" || strings.TrimSpace(observation.IdentityInstance) == "" {
				return store.ValidationErrorf("canonical observation requires semantic-v1 rule, anchor, and instance coordinates")
			}
		}
	}
	observation.Reason = redact.SensitiveText(observation.Reason)
	if err := validateSecurityReason(observation.ReasonCode, observation.Reason, observation.Disposition != store.FindingObservationAccepted); err != nil {
		return err
	}
	var err error
	observation.SourceArtifactDigest, err = normalizeSecurityDigest(observation.SourceArtifactDigest, false, "sourceArtifactDigest")
	if err != nil {
		return err
	}
	observation.PolicyDigest, err = normalizeSecurityDigest(observation.PolicyDigest, false, "policyDigest")
	if err != nil {
		return err
	}
	observation.SemanticFingerprint, err = normalizeSecurityDigest(observation.SemanticFingerprint, observation.Disposition == store.FindingObservationAccepted, "semanticFingerprint")
	if err != nil {
		return err
	}
	observation.NormalizedPayload, observation.PayloadDigest, err = normalizeSecurityPayload(
		observation.NormalizedPayload,
		observation.PayloadDigest,
		observation.Disposition == store.FindingObservationAccepted,
		"normalizedPayload",
	)
	if err != nil {
		return err
	}
	return nil
}

func findingObservationDigest(observation store.FindingObservation) (string, error) {
	observation.RecordDigest = ""
	observation.CreatedAt = time.Time{}
	return securityRecordDigest(observation)
}

func (s *Store) bindCanonicalObservationIdentity(ctx context.Context, observation *store.FindingObservation) error {
	if observation.Disposition != store.FindingObservationAccepted || observation.IdentityQuality != store.IdentityQualityCanonical {
		return nil
	}
	if observation.TargetReceiptID == "" {
		return store.ValidationErrorf("canonical observation requires a target receipt")
	}
	var namespace, repositoryScan, scanRunID, runUID, repositoryIdentity string
	err := s.db.QueryRowContext(ctx, `SELECT namespace, repository_scan, scan_run_id, run_uid, target_id
		FROM security_target_receipts WHERE id = ?`, observation.TargetReceiptID).
		Scan(&namespace, &repositoryScan, &scanRunID, &runUID, &repositoryIdentity)
	if errors.Is(err, sql.ErrNoRows) {
		return store.ValidationErrorf("targetReceiptID %q does not exist", observation.TargetReceiptID)
	}
	if err != nil {
		return err
	}
	if namespace != observation.Namespace || repositoryScan != observation.RepositoryScan ||
		scanRunID != observation.ScanRunID || runUID != observation.RunUID {
		return store.ValidationErrorf("canonical observation binding does not match target receipt")
	}
	identity, ok := securityidentity.DeriveCanonicalSemanticIdentity(
		repositoryIdentity, observation.RuleID, observation.IdentityAnchor, observation.IdentityInstance,
	)
	if !ok {
		return store.ValidationErrorf("canonical observation repository and semantic coordinates are required")
	}
	observation.RuleID = identity.RuleID
	observation.IdentityAnchor = identity.Anchor
	observation.IdentityInstance = identity.Instance
	observation.IdentityAlgorithmVersion = identity.AlgorithmVersion
	observation.SemanticFingerprint = identity.SemanticFingerprint
	return nil
}

func (s *Store) normalizeAndBindFindingObservation(ctx context.Context, observation *store.FindingObservation) error {
	if err := normalizeFindingObservation(observation); err != nil {
		return err
	}
	return s.bindCanonicalObservationIdentity(ctx, observation)
}

// AcceptFindingObservation appends one immutable normalized producer observation.
func (s *Store) AcceptFindingObservation(ctx context.Context, observation *store.FindingObservation) (bool, error) {
	if err := s.normalizeAndBindFindingObservation(ctx, observation); err != nil {
		return false, err
	}
	digest, err := findingObservationDigest(*observation)
	if err != nil {
		return false, err
	}
	if observation.CreatedAt.IsZero() {
		observation.CreatedAt = time.Now().UTC()
	} else {
		observation.CreatedAt = observation.CreatedAt.UTC()
	}
	observation.RecordDigest = digest

	var existingDigest string
	err = s.db.QueryRowContext(ctx, `SELECT record_digest FROM security_finding_observations WHERE id = ?`, observation.ID).Scan(&existingDigest)
	if err == nil {
		return immutableReplayResult(existingDigest, digest, "finding observation", observation.ID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	var receiptNamespace, receiptRepositoryScan, receiptScanRunID, receiptRunUID string
	var receiptTargetID, receiptArtifactName, receiptArtifactDigest, receiptPolicyDigest, receiptDisposition string
	var receiptScopeKind, receiptScopeID string
	var receiptArtifactGeneration int64
	err = s.db.QueryRowContext(ctx, `SELECT namespace, repository_scan, scan_run_id, run_uid, target_receipt_id,
		source_artifact_name, source_artifact_generation, source_artifact_digest, scanner_policy_digest, disposition,
		scope_kind, scope_id
		FROM security_stage_receipts WHERE id = ?`, observation.StageReceiptID).
		Scan(&receiptNamespace, &receiptRepositoryScan, &receiptScanRunID, &receiptRunUID, &receiptTargetID,
			&receiptArtifactName, &receiptArtifactGeneration, &receiptArtifactDigest, &receiptPolicyDigest, &receiptDisposition,
			&receiptScopeKind, &receiptScopeID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, store.ValidationErrorf("stageReceiptID %q does not exist", observation.StageReceiptID)
	}
	if err != nil {
		return false, err
	}
	if receiptNamespace != observation.Namespace || receiptRepositoryScan != observation.RepositoryScan ||
		receiptScanRunID != observation.ScanRunID || receiptRunUID != observation.RunUID {
		return false, store.ValidationErrorf("observation binding does not match stage receipt")
	}
	if receiptDisposition != string(store.StageReceiptAccepted) || receiptTargetID != observation.TargetReceiptID ||
		receiptArtifactName != observation.SourceArtifactName ||
		receiptArtifactGeneration != observation.SourceArtifactGeneration ||
		receiptArtifactDigest != observation.SourceArtifactDigest || receiptPolicyDigest != observation.PolicyDigest {
		return false, store.ValidationErrorf("observation provenance does not match stage receipt")
	}
	switch receiptScopeKind {
	case "slice":
		if receiptScopeID == "" || receiptScopeID != observation.SliceID {
			return false, store.ValidationErrorf("observation slice does not match stage receipt scope")
		}
	case "pass":
		if receiptScopeID == "" || receiptScopeID != observation.PassID {
			return false, store.ValidationErrorf("observation pass does not match stage receipt scope")
		}
	case "candidate":
		if receiptScopeID == "" || receiptScopeID != observation.CandidateKey {
			return false, store.ValidationErrorf("observation candidate does not match stage receipt scope")
		}
	case "run", "":
	default:
		return false, store.ValidationErrorf("unsupported observation stage receipt scope %q", receiptScopeKind)
	}
	if err := s.findingObservationSourceConflict(ctx, observation); err != nil {
		return false, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO security_finding_observations
		(id, namespace, repository_scan, scan_run_id, run_uid, stage_receipt_id, target_receipt_id,
		 slice_id, pass_id, candidate_key, producer_finding_id, source_artifact_name,
		 source_artifact_generation, source_artifact_digest, policy_digest, ordinal, disposition,
		 reason_code, reason, rule_id, identity_anchor, identity_instance, identity_quality,
		 identity_algorithm_version, semantic_fingerprint, legacy_fingerprint, normalized_payload,
		 payload_digest, record_digest, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		observation.ID, observation.Namespace, observation.RepositoryScan, observation.ScanRunID, observation.RunUID,
		observation.StageReceiptID, observation.TargetReceiptID, observation.SliceID, observation.PassID,
		observation.CandidateKey, observation.ProducerFindingID, observation.SourceArtifactName,
		observation.SourceArtifactGeneration, observation.SourceArtifactDigest, observation.PolicyDigest,
		observation.Ordinal, observation.Disposition, observation.ReasonCode, observation.Reason,
		observation.RuleID, observation.IdentityAnchor, observation.IdentityInstance, observation.IdentityQuality,
		observation.IdentityAlgorithmVersion, observation.SemanticFingerprint, observation.LegacyFingerprint,
		securityPayloadBytes(observation.NormalizedPayload), observation.PayloadDigest, observation.RecordDigest, observation.CreatedAt,
	)
	if err != nil {
		return s.resolveFindingObservationInsertError(ctx, observation, digest, err)
	}
	return true, nil
}

func (s *Store) resolveFindingObservationInsertError(
	ctx context.Context,
	observation *store.FindingObservation,
	digest string,
	insertErr error,
) (bool, error) {
	if replay, replayErr := s.GetFindingObservation(ctx, observation.Namespace, observation.ID); replayErr == nil {
		return immutableReplayResult(replay.RecordDigest, digest, "finding observation", observation.ID)
	}
	if sourceErr := s.findingObservationSourceConflict(ctx, observation); sourceErr != nil {
		return false, sourceErr
	}
	return false, insertErr
}

const findingObservationSelectColumns = `id, namespace, repository_scan, scan_run_id, run_uid, stage_receipt_id,
	target_receipt_id, slice_id, pass_id, candidate_key, producer_finding_id, source_artifact_name,
	source_artifact_generation, source_artifact_digest, policy_digest, ordinal, disposition, reason_code,
	reason, rule_id, identity_anchor, identity_instance, identity_quality, identity_algorithm_version,
	semantic_fingerprint, legacy_fingerprint, normalized_payload, payload_digest, record_digest, created_at`

func scanFindingObservation(scanner interface{ Scan(...any) error }) (*store.FindingObservation, error) {
	var observation store.FindingObservation
	var payload []byte
	err := scanner.Scan(
		&observation.ID, &observation.Namespace, &observation.RepositoryScan, &observation.ScanRunID,
		&observation.RunUID, &observation.StageReceiptID, &observation.TargetReceiptID, &observation.SliceID,
		&observation.PassID, &observation.CandidateKey, &observation.ProducerFindingID,
		&observation.SourceArtifactName, &observation.SourceArtifactGeneration, &observation.SourceArtifactDigest,
		&observation.PolicyDigest, &observation.Ordinal, &observation.Disposition, &observation.ReasonCode,
		&observation.Reason, &observation.RuleID, &observation.IdentityAnchor, &observation.IdentityInstance,
		&observation.IdentityQuality, &observation.IdentityAlgorithmVersion, &observation.SemanticFingerprint,
		&observation.LegacyFingerprint, &payload, &observation.PayloadDigest, &observation.RecordDigest,
		&observation.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	observation.NormalizedPayload = append(json.RawMessage(nil), payload...)
	return &observation, nil
}

// GetFindingObservation returns one immutable observation.
func (s *Store) GetFindingObservation(ctx context.Context, namespace, id string) (*store.FindingObservation, error) {
	observation, err := scanFindingObservation(s.db.QueryRowContext(ctx,
		`SELECT `+findingObservationSelectColumns+` FROM security_finding_observations WHERE namespace = ? AND id = ?`,
		namespace, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return observation, err
}

func (s *Store) getFindingObservationBySourceCoordinate(
	ctx context.Context,
	observation *store.FindingObservation,
) (*store.FindingObservation, error) {
	return scanFindingObservation(s.db.QueryRowContext(ctx,
		`SELECT `+findingObservationSelectColumns+` FROM security_finding_observations
		 WHERE namespace = ? AND stage_receipt_id = ? AND source_artifact_name = ?
		 AND source_artifact_generation = ? AND ordinal = ?`,
		observation.Namespace, observation.StageReceiptID, observation.SourceArtifactName,
		observation.SourceArtifactGeneration, observation.Ordinal))
}

func (s *Store) findingObservationSourceConflict(ctx context.Context, observation *store.FindingObservation) error {
	existing, err := s.getFindingObservationBySourceCoordinate(ctx, observation)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: finding observation source coordinate is already bound to %q",
		store.ErrDuplicateMismatch, existing.ID)
}

// ListFindingObservations lists immutable observations oldest first.
func (s *Store) ListFindingObservations(ctx context.Context, filter store.FindingObservationFilter) ([]store.FindingObservation, string, error) {
	offset, err := parseOffsetCursor(filter.Cursor)
	if err != nil {
		return nil, "", err
	}
	filter.Limit = boundedFindingHistoryLimit(filter.Limit)
	query := strings.Builder{}
	query.WriteString(`SELECT ` + findingObservationSelectColumns + ` FROM security_finding_observations WHERE namespace = ?`)
	args := []any{filter.Namespace}
	appendSecurityStringFilter(&query, &args, "repository_scan", filter.RepositoryScan)
	appendSecurityStringFilter(&query, &args, "scan_run_id", filter.ScanRunID)
	appendSecurityStringFilter(&query, &args, "stage_receipt_id", filter.StageReceiptID)
	appendSecurityStringFilter(&query, &args, "semantic_fingerprint", filter.SemanticFingerprint)
	appendSecurityStringFilter(&query, &args, "disposition", string(filter.Disposition))
	query.WriteString(` ORDER BY created_at ASC, id ASC LIMIT ? OFFSET ?`)
	args = append(args, filter.Limit, offset)
	rows, err := s.db.QueryContext(ctx, query.String(), args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close() //nolint:errcheck
	items := make([]store.FindingObservation, 0)
	for rows.Next() {
		item, err := scanFindingObservation(rows)
		if err != nil {
			return nil, "", err
		}
		items = append(items, *item)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	return items, nextOffsetCursor(offset, len(items), filter.Limit), nil
}

func normalizeScanQuality(quality *store.ScanQuality) error {
	if quality == nil {
		return nil
	}
	legacy := store.LegacyScanQuality()
	if quality.SchemaVersion == 0 {
		quality.SchemaVersion = legacy.SchemaVersion
	}
	if quality.InventoryCoverageStatus == "" {
		quality.InventoryCoverageStatus = legacy.InventoryCoverageStatus
	}
	if quality.CandidateCoverageStatus == "" {
		quality.CandidateCoverageStatus = legacy.CandidateCoverageStatus
	}
	if quality.CoverageStatus == "" {
		quality.CoverageStatus = legacy.CoverageStatus
	}
	if quality.ValidationScope == "" {
		quality.ValidationScope = legacy.ValidationScope
	}
	if quality.ValidationExecution == "" {
		quality.ValidationExecution = legacy.ValidationExecution
	}
	if quality.AttackPathExecution == "" {
		quality.AttackPathExecution = legacy.AttackPathExecution
	}
	if quality.AnalysisAttestationLevel == "" {
		quality.AnalysisAttestationLevel = legacy.AnalysisAttestationLevel
	}
	if quality.TargetVerification == "" {
		quality.TargetVerification = legacy.TargetVerification
	}
	if quality.BundleStatus == "" {
		quality.BundleStatus = legacy.BundleStatus
	}
	if quality.AuthorizationStatus == "" {
		quality.AuthorizationStatus = legacy.AuthorizationStatus
	}
	if quality.IsolationStatus == "" {
		quality.IsolationStatus = legacy.IsolationStatus
	}
	if quality.SchemaVersion != store.SecurityQualitySchemaVersion {
		return store.ValidationErrorf("unsupported qualitySchemaVersion %d", quality.SchemaVersion)
	}
	for field, value := range map[string]store.CoverageStatus{
		"inventoryCoverageStatus": quality.InventoryCoverageStatus,
		"candidateCoverageStatus": quality.CandidateCoverageStatus,
		"coverageStatus":          quality.CoverageStatus,
	} {
		if !slices.Contains([]store.CoverageStatus{
			store.CoverageStatusNotStarted,
			store.CoverageStatusPending,
			store.CoverageStatusComplete,
			store.CoverageStatusPartial,
			store.CoverageStatusFailed,
			store.CoverageStatusUnknown,
		}, value) {
			return store.ValidationErrorf("unsupported %s %q", field, value)
		}
	}
	if !slices.Contains([]store.ValidationScope{
		store.ValidationScopeOff,
		store.ValidationScopeSampled,
		store.ValidationScopeAll,
		store.ValidationScopeUnknown,
	}, quality.ValidationScope) {
		return store.ValidationErrorf("unsupported validationScope %q", quality.ValidationScope)
	}
	if !slices.Contains([]store.QualityExecutionStatus{
		store.QualityExecutionNotStarted,
		store.QualityExecutionPending,
		store.QualityExecutionComplete,
		store.QualityExecutionPartial,
		store.QualityExecutionFailed,
		store.QualityExecutionUnknown,
	}, quality.ValidationExecution) {
		return store.ValidationErrorf("unsupported validationExecution %q", quality.ValidationExecution)
	}
	if !slices.Contains([]store.QualityExecutionStatus{
		store.QualityExecutionNotStarted,
		store.QualityExecutionPending,
		store.QualityExecutionComplete,
		store.QualityExecutionPartial,
		store.QualityExecutionFailed,
		store.QualityExecutionDeferred,
		store.QualityExecutionUnknown,
	}, quality.AttackPathExecution) {
		return store.ValidationErrorf("unsupported attackPathExecution %q", quality.AttackPathExecution)
	}
	if !slices.Contains([]store.AnalysisAttestationLevel{
		store.AnalysisAttestationDelivered,
		store.AnalysisAttestationToolObserved,
		store.AnalysisAttestationBrokered,
		store.AnalysisAttestationUnverified,
	}, quality.AnalysisAttestationLevel) {
		return store.ValidationErrorf("unsupported analysisAttestationLevel %q", quality.AnalysisAttestationLevel)
	}
	if !slices.Contains([]store.TargetVerificationStatus{
		store.TargetVerificationPending,
		store.TargetVerificationVerified,
		store.TargetVerificationUnverified,
		store.TargetVerificationMismatch,
	}, quality.TargetVerification) {
		return store.ValidationErrorf("unsupported targetVerification %q", quality.TargetVerification)
	}
	if !slices.Contains([]store.BundleStatus{
		store.BundleStatusNotStarted,
		store.BundleStatusDraft,
		store.BundleStatusSealing,
		store.BundleStatusSealed,
		store.BundleStatusRetryableFailed,
		store.BundleStatusFailed,
	}, quality.BundleStatus) {
		return store.ValidationErrorf("unsupported bundleStatus %q", quality.BundleStatus)
	}
	if !slices.Contains([]store.AuthorizationStatus{
		store.AuthorizationStatusVerified,
		store.AuthorizationStatusAdmitted,
		store.AuthorizationStatusDelegated,
		store.AuthorizationStatusLegacyUnverified,
		store.AuthorizationStatusRevoked,
		store.AuthorizationStatusExpired,
	}, quality.AuthorizationStatus) {
		return store.ValidationErrorf("unsupported authorizationStatus %q", quality.AuthorizationStatus)
	}
	if !slices.Contains([]store.IsolationStatus{
		store.IsolationStatusLegacy,
		store.IsolationStatusHardened,
		store.IsolationStatusFallback,
		store.IsolationStatusUnverified,
		store.IsolationStatusFailed,
	}, quality.IsolationStatus) {
		return store.ValidationErrorf("unsupported isolationStatus %q", quality.IsolationStatus)
	}
	reasons, err := normalizeSecurityStringList(quality.ReasonCodes, "quality.reasonCodes", 16)
	if err != nil {
		return err
	}
	quality.ReasonCodes = reasons
	return nil
}

func normalizeScanRunIntegrityFields(run *store.ScanRun) error {
	if run == nil {
		return store.ValidationErrorf("scan run is required")
	}
	if run.RequestIdempotencyKey != "" && run.IdempotencyKey != "" && run.RequestIdempotencyKey != run.IdempotencyKey {
		return store.ValidationErrorf("scan run idempotencyKey and requestIdempotencyKey must match")
	}
	if run.RequestIdempotencyKey == "" {
		run.RequestIdempotencyKey = run.IdempotencyKey
	}
	if run.IdempotencyKey == "" {
		run.IdempotencyKey = run.RequestIdempotencyKey
	}
	for _, field := range []struct {
		name     string
		value    string
		required bool
	}{
		{"repositoryScanUID", run.RepositoryScanUID, false},
		{"targetReceiptID", run.TargetReceiptID, false},
	} {
		if err := validateSecurityID(field.value, field.name, field.required); err != nil {
			return err
		}
	}
	if err := validateSecurityRunUID(run.RunUID, false); err != nil {
		return err
	}
	if run.RepositoryScanGeneration < 0 {
		return store.ValidationErrorf("repositoryScanGeneration must be non-negative")
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"requestIdempotencyKey", run.RequestIdempotencyKey},
		{"resolvedTargetKey", run.ResolvedTargetKey},
	} {
		if err := validateSecurityText(field.value, field.name, maxSecurityCoordinateBytes, false); err != nil {
			return err
		}
	}
	return normalizeScanQuality(&run.Quality)
}

func normalizeFindingIntegrityFields(finding *store.Finding) error {
	if finding == nil {
		return store.ValidationErrorf("finding is required")
	}
	if finding.IdentityQuality == "" {
		finding.IdentityQuality = store.IdentityQualityLegacy
	}
	if finding.IdentityAlgorithmVersion == "" && finding.IdentityQuality == store.IdentityQualityLegacy {
		finding.IdentityAlgorithmVersion = store.IdentityAlgorithmLegacyV2
	}
	if finding.LegacyFingerprint == "" {
		finding.LegacyFingerprint = finding.Fingerprint
	}
	if finding.HistoryStatus == "" {
		finding.HistoryStatus = store.FindingHistoryLegacyUnrebuildable
	}
	if !slices.Contains([]string{
		store.IdentityQualityCanonical,
		store.IdentityQualityProducerProposed,
		store.IdentityQualityLegacy,
	}, finding.IdentityQuality) {
		return store.ValidationErrorf("unsupported identityQuality %q", finding.IdentityQuality)
	}
	if !slices.Contains([]string{
		store.FindingHistoryCanonical,
		store.FindingHistoryLegacyUnrebuildable,
	}, finding.HistoryStatus) {
		return store.ValidationErrorf("unsupported historyStatus %q", finding.HistoryStatus)
	}
	if err := validateSecurityText(finding.IdentityAlgorithmVersion, "identityAlgorithmVersion", maxSecurityNameBytes, true); err != nil {
		return err
	}
	if err := validateSecurityText(finding.LegacyFingerprint, "legacyFingerprint", maxSecurityCoordinateBytes, false); err != nil {
		return err
	}
	if finding.IdentityQuality == store.IdentityQualityCanonical && finding.SemanticFingerprint == "" {
		return store.ValidationErrorf("canonical finding requires semanticFingerprint")
	}
	if finding.SemanticFingerprint != "" {
		normalized, err := normalizeSecurityDigest(finding.SemanticFingerprint, true, "semanticFingerprint")
		if err != nil {
			return err
		}
		finding.SemanticFingerprint = normalized
	}
	if err := validateSecurityID(finding.CurrentOccurrenceID, "currentOccurrenceID", false); err != nil {
		return err
	}
	if finding.DecisionVersion < 0 {
		return store.ValidationErrorf("decisionVersion must be non-negative")
	}
	for i := range finding.Evidence {
		if finding.Evidence[i].ContentSize < 0 {
			return store.ValidationErrorf("evidence contentSize must be non-negative")
		}
		if finding.Evidence[i].ContentSHA256 != "" {
			value := finding.Evidence[i].ContentSHA256
			if strings.TrimSpace(value) != value || len(value) != sha256.Size*2 {
				return store.ValidationErrorf("evidence.contentSHA256 must be an unprefixed full SHA-256 digest")
			}
			if _, err := hex.DecodeString(value); err != nil {
				return store.ValidationErrorf("evidence.contentSHA256 must be an unprefixed full SHA-256 digest")
			}
		}
	}
	return nil
}
