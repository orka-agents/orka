package sqlite

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/orka-agents/orka/internal/redact"
	"github.com/orka-agents/orka/internal/store"
	omsprotocol "github.com/orka-agents/orka/pkg/oms/protocol"
)

var _ store.GovernedMemoryStore = (*Store)(nil)

const (
	maxMemoryOperationPayloadBytes  = store.MaxMemoryOperationPayloadBytes
	maxIdempotencySnapshotBytes     = store.MaxMemoryIdempotencySnapshotBytes
	defaultGovernedMemoryLimit      = 100
	maxGovernedMemoryLimit          = 200
	defaultOperationRecoveryWindow  = 7 * 24 * time.Hour
	minimumIdempotencyRetention     = 30 * 24 * time.Hour
	activationRecoveryReceiptMaxAge = 15 * time.Minute
	restorePreviewTTL               = 10 * time.Minute
	maxMemoryAuditTextBytes         = 1024
	maxMemoryReceiptIdentityBytes   = 256
	estimatedMemoryReceiptBytes     = 1536
	legacyMemoryFenceMessage        = "legacy memory namespace fenced"
	durablePayloadCompressionPrefix = "orka.zlib.v1\x00"
	maxFeatureHeartbeatTTL          = 2 * time.Minute
	maxFeatureHeartbeatClockSkew    = 30 * time.Second

	legacyAppliedProposalBackfillMigration = "legacy-applied-proposal-operation-id-v1"
	legacyAppliedProposalBackfillPrefix    = legacyProposalApplyOperationPrefix + "migrated-"
)

func backfillLegacyAppliedProposalOperationIDs(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migration failed: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	result, err := tx.Exec(`INSERT INTO memory_governance_migrations(name) VALUES (?)
		ON CONFLICT(name) DO NOTHING`, legacyAppliedProposalBackfillMigration)
	if err != nil {
		return fmt.Errorf("migration failed: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("migration failed: %w", err)
	}
	if inserted == 0 {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migration failed: %w", err)
		}
		return nil
	}

	if _, err := tx.Exec(`UPDATE memory_proposals AS p
		SET apply_operation_id = ? || p.id
		WHERE p.apply_operation_id = ''
			AND lower(trim(p.type)) = 'memory'
			AND lower(trim(p.status)) = 'applied'
			AND trim(p.applied_memory_id) <> ''
			AND p.reviewed_at IS NOT NULL AND p.applied_at IS NOT NULL
			AND p.applied_at >= p.reviewed_at
			AND EXISTS (
				SELECT 1 FROM memories AS m
				WHERE m.namespace = p.namespace AND m.id = p.applied_memory_id
					AND m.source = 'memory_proposal' AND m.source_proposal_id = p.id
					AND m.content = p.content
			)`, legacyAppliedProposalBackfillPrefix); err != nil {
		return fmt.Errorf("migration failed: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migration failed: %w", err)
	}
	return nil
}

type memoryGovernanceQuotaConfig struct {
	NamespaceCatalogRows          int64
	GlobalCatalogRows             int64
	NamespaceTombstoneRows        int64
	GlobalTombstoneRows           int64
	NamespaceOperationRows        int64
	GlobalOperationRows           int64
	NamespaceIdempotencyRows      int64
	GlobalIdempotencyRows         int64
	NamespaceIdempotencyBytes     int64
	GlobalIdempotencyBytes        int64
	NamespaceReceiptBytes         int64
	GlobalReceiptBytes            int64
	NamespaceAuditRows            int64
	GlobalAuditRows               int64
	NamespaceAuditBytes           int64
	GlobalAuditBytes              int64
	NamespaceUnresolvedRows       int64
	NamespaceUnresolvedBytes      int64
	GlobalUnresolvedBytes         int64
	NamespaceRetainedPayloadBytes int64
	GlobalRetainedPayloadBytes    int64
	NamespaceSearchCursorRows     int64
	GlobalSearchCursorRows        int64
	NamespaceSearchCursorBytes    int64
	GlobalSearchCursorBytes       int64
	NamespaceSearchReplayRows     int64
	GlobalSearchReplayRows        int64
	NamespaceSearchReplayBytes    int64
	GlobalSearchReplayBytes       int64
	SafetyReserveRows             int64
}

var governedMemoryQuotas = memoryGovernanceQuotaConfig{
	NamespaceCatalogRows:          100_000,
	GlobalCatalogRows:             1_000_000,
	NamespaceTombstoneRows:        50_000,
	GlobalTombstoneRows:           500_000,
	NamespaceOperationRows:        100_000,
	GlobalOperationRows:           1_000_000,
	NamespaceIdempotencyRows:      100_000,
	GlobalIdempotencyRows:         1_000_000,
	NamespaceIdempotencyBytes:     256 << 20,
	GlobalIdempotencyBytes:        2 << 30,
	NamespaceReceiptBytes:         128 << 20,
	GlobalReceiptBytes:            1 << 30,
	NamespaceAuditRows:            250_000,
	GlobalAuditRows:               2_000_000,
	NamespaceAuditBytes:           256 << 20,
	GlobalAuditBytes:              2 << 30,
	NamespaceUnresolvedRows:       1_000,
	NamespaceUnresolvedBytes:      16 << 20,
	GlobalUnresolvedBytes:         256 << 20,
	NamespaceRetainedPayloadBytes: 64 << 20,
	GlobalRetainedPayloadBytes:    256 << 20,
	NamespaceSearchCursorRows:     128,
	GlobalSearchCursorRows:        4_096,
	NamespaceSearchCursorBytes:    2 << 20,
	GlobalSearchCursorBytes:       64 << 20,
	NamespaceSearchReplayRows:     1_024,
	GlobalSearchReplayRows:        32_768,
	NamespaceSearchReplayBytes:    32 << 20,
	GlobalSearchReplayBytes:       512 << 20,
	SafetyReserveRows:             1_000,
}

func migrateMemoryGovernance(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS memory_governance_migrations (
		name       TEXT PRIMARY KEY,
		applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		return fmt.Errorf("migration failed: %w", err)
	}
	if err := ensureSQLiteColumns(db, "memory_proposals", []sqliteColumnMigration{
		{Name: "apply_operation_id", Definition: "apply_operation_id TEXT NOT NULL DEFAULT ''"},
		{Name: "application_abandoned_by", Definition: "application_abandoned_by TEXT NOT NULL DEFAULT ''"},
		{Name: "application_abandoned_reason", Definition: "application_abandoned_reason TEXT NOT NULL DEFAULT ''"},
		{Name: "application_abandoned_at", Definition: "application_abandoned_at TIMESTAMP"},
	}); err != nil {
		return err
	}
	if err := backfillLegacyAppliedProposalOperationIDs(db); err != nil {
		return err
	}

	statements := []string{
		`CREATE TABLE IF NOT EXISTS legacy_memory_archive (
			namespace_uid       TEXT NOT NULL,
			namespace_name      TEXT NOT NULL,
			id                  TEXT NOT NULL,
			session_name        TEXT NOT NULL DEFAULT '',
			agent_name          TEXT NOT NULL DEFAULT '',
			task_name           TEXT NOT NULL DEFAULT '',
			parent_task         TEXT NOT NULL DEFAULT '',
			source              TEXT NOT NULL DEFAULT '',
			source_proposal_id  TEXT NOT NULL DEFAULT '',
			content             TEXT NOT NULL,
			tags_json           TEXT NOT NULL DEFAULT '[]',
			disabled            BOOLEAN NOT NULL DEFAULT FALSE,
			deleted             BOOLEAN NOT NULL DEFAULT FALSE,
			created_at          TIMESTAMP NOT NULL,
			updated_at          TIMESTAMP NOT NULL,
			last_recalled_at    TIMESTAMP,
			recalled_count      INTEGER NOT NULL DEFAULT 0,
			backend_uid         TEXT NOT NULL,
			authority_epoch     INTEGER NOT NULL,
			routing_epoch       INTEGER NOT NULL,
			archived_at         TIMESTAMP NOT NULL,
			PRIMARY KEY (namespace_uid, id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_legacy_memory_archive_namespace
			ON legacy_memory_archive(namespace_name, namespace_uid, archived_at, id)`,
		`CREATE TABLE IF NOT EXISTS memory_legacy_fences (
			namespace_name  TEXT PRIMARY KEY,
			namespace_uid   TEXT NOT NULL UNIQUE,
			backend_uid     TEXT NOT NULL,
			authority_epoch INTEGER NOT NULL CHECK (authority_epoch > 0),
			routing_epoch   INTEGER NOT NULL CHECK (routing_epoch > 0),
			created_at      TIMESTAMP NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS controller_feature_heartbeats (
			instance_id       TEXT PRIMARY KEY,
			role              TEXT NOT NULL,
			feature_epoch     INTEGER NOT NULL CHECK (feature_epoch >= 0),
			last_heartbeat_at TIMESTAMP NOT NULL,
			expires_at        TIMESTAMP NOT NULL CHECK (expires_at > last_heartbeat_at)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_controller_feature_heartbeats_live
			ON controller_feature_heartbeats(expires_at, role, feature_epoch)`,
		`CREATE TABLE IF NOT EXISTS controller_feature_epoch_history (
			role          TEXT NOT NULL,
			feature_epoch INTEGER NOT NULL CHECK (feature_epoch >= 0),
			first_seen_at TIMESTAMP NOT NULL,
			last_seen_at  TIMESTAMP NOT NULL CHECK (last_seen_at >= first_seen_at),
			PRIMARY KEY (role, feature_epoch)
		)`,
		`CREATE TABLE IF NOT EXISTS memory_backend_bindings (
			namespace_name          TEXT NOT NULL UNIQUE,
			namespace_uid           TEXT PRIMARY KEY,
			cluster_id              TEXT NOT NULL,
			mode                    TEXT NOT NULL CHECK (mode IN ('legacy','remote')),
			backend_uid             TEXT NOT NULL,
			backend_generation      INTEGER NOT NULL CHECK (backend_generation > 0),
			authority_epoch         INTEGER NOT NULL CHECK (authority_epoch > 0),
			routing_epoch           INTEGER NOT NULL CHECK (routing_epoch > 0),
			spec_digest             TEXT NOT NULL,
			endpoint_digest          TEXT NOT NULL,
			resolved_address_digest  TEXT NOT NULL,
			server_certificate_digest TEXT NOT NULL,
			secret_name              TEXT NOT NULL,
			secret_key              TEXT NOT NULL,
			secret_uid              TEXT NOT NULL,
			secret_resource_version TEXT NOT NULL,
			tenant_id               TEXT NOT NULL,
			store_name              TEXT NOT NULL,
			store_uuid              TEXT NOT NULL,
			ownership_claim         TEXT NOT NULL,
			capability_revision     TEXT NOT NULL,
			protocol                TEXT NOT NULL,
			state                   TEXT NOT NULL CHECK (state IN ('legacy','validating','accepting','draining','recovering','decommissioned','removed')),
			activation_epoch        INTEGER NOT NULL CHECK (activation_epoch > 0),
			minimum_feature_epoch   INTEGER NOT NULL CHECK (minimum_feature_epoch >= 0),
			validation_expires_at   TIMESTAMP NOT NULL,
			activated_at            TIMESTAMP NOT NULL,
			updated_at              TIMESTAMP NOT NULL,
			decommissioned_at       TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_memory_backend_bindings_backend
			ON memory_backend_bindings(backend_uid, state, namespace_uid)`,
		`CREATE TABLE IF NOT EXISTS remote_memory_catalog (
			namespace_uid          TEXT NOT NULL,
			id                     TEXT NOT NULL,
			namespace_name         TEXT NOT NULL,
			cluster_id             TEXT NOT NULL,
			backend_uid            TEXT NOT NULL,
			authority_epoch        INTEGER NOT NULL CHECK (authority_epoch > 0),
			routing_epoch          INTEGER NOT NULL CHECK (routing_epoch > 0),
			tenant_id              TEXT NOT NULL,
			store_uuid             TEXT NOT NULL,
			backend_memory_id      TEXT NOT NULL DEFAULT '',
			backend_version        TEXT NOT NULL DEFAULT '',
			generation             INTEGER NOT NULL DEFAULT 0 CHECK (generation >= 0),
			desired_generation     INTEGER NOT NULL CHECK (desired_generation >= generation),
			governance_revision    INTEGER NOT NULL DEFAULT 1 CHECK (governance_revision > 0),
			materialization_state  TEXT NOT NULL CHECK (materialization_state IN ('pending','active','deleted','diverged','lost','orphaned')),
			disabled               BOOLEAN NOT NULL DEFAULT FALSE,
			deleted                BOOLEAN NOT NULL DEFAULT FALSE,
			trust                  TEXT NOT NULL CHECK (trust IN ('untrusted','reviewed','trusted')),
			session_name           TEXT NOT NULL DEFAULT '',
			agent_name             TEXT NOT NULL DEFAULT '',
			task_name              TEXT NOT NULL DEFAULT '',
			parent_task            TEXT NOT NULL DEFAULT '',
			source                 TEXT NOT NULL DEFAULT '',
			source_proposal_id     TEXT NOT NULL DEFAULT '',
			tags_json              TEXT NOT NULL DEFAULT '[]',
			pending_session_name   TEXT NOT NULL DEFAULT '',
			pending_agent_name     TEXT NOT NULL DEFAULT '',
			pending_task_name      TEXT NOT NULL DEFAULT '',
			pending_parent_task    TEXT NOT NULL DEFAULT '',
			pending_source         TEXT NOT NULL DEFAULT '',
			pending_tags_json      TEXT NOT NULL DEFAULT '[]',
			content_digest         TEXT NOT NULL DEFAULT '',
			content_available      BOOLEAN NOT NULL DEFAULT FALSE,
			pending_operation_id   TEXT NOT NULL DEFAULT '',
			created_at             TIMESTAMP NOT NULL,
			updated_at             TIMESTAMP NOT NULL,
			last_recalled_at       TIMESTAMP,
			recalled_count         INTEGER NOT NULL DEFAULT 0 CHECK (recalled_count >= 0),
			PRIMARY KEY (namespace_uid, id),
			FOREIGN KEY (namespace_uid) REFERENCES memory_backend_bindings(namespace_uid) ON DELETE RESTRICT
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_remote_memory_catalog_backend_id
			ON remote_memory_catalog(namespace_uid, backend_memory_id)
			WHERE backend_memory_id <> ''`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_remote_memory_catalog_source_proposal
			ON remote_memory_catalog(namespace_uid, source_proposal_id)
			WHERE source_proposal_id <> ''`,
		`CREATE INDEX IF NOT EXISTS idx_remote_memory_catalog_list
			ON remote_memory_catalog(namespace_uid, deleted, disabled, updated_at DESC, id DESC)`,
		`CREATE TABLE IF NOT EXISTS remote_memory_generation_watermarks (
			namespace_uid   TEXT NOT NULL,
			id              TEXT NOT NULL,
			backend_uid     TEXT NOT NULL,
			authority_epoch INTEGER NOT NULL CHECK (authority_epoch > 0),
			routing_epoch   INTEGER NOT NULL CHECK (routing_epoch > 0),
			store_uuid      TEXT NOT NULL,
			generation      INTEGER NOT NULL CHECK (generation > 0),
			updated_at      TIMESTAMP NOT NULL,
			PRIMARY KEY (namespace_uid, id)
		)`,
		`CREATE TABLE IF NOT EXISTS memory_operations (
			sequence                         INTEGER PRIMARY KEY AUTOINCREMENT,
			id                               TEXT NOT NULL UNIQUE,
			namespace_name                   TEXT NOT NULL,
			namespace_uid                    TEXT NOT NULL,
			cluster_id                       TEXT NOT NULL,
			backend_uid                      TEXT NOT NULL,
			authority_epoch                  INTEGER NOT NULL CHECK (authority_epoch > 0),
			routing_epoch                    INTEGER NOT NULL CHECK (routing_epoch > 0),
			memory_id                        TEXT NOT NULL,
			proposal_id                      TEXT NOT NULL DEFAULT '',
			kind                             TEXT NOT NULL CHECK (kind IN ('create','replace','delete')),
			desired_generation               INTEGER NOT NULL CHECK (desired_generation > 0),
			expected_materialized_generation INTEGER NOT NULL CHECK (expected_materialized_generation >= 0),
			expected_backend_version         TEXT NOT NULL DEFAULT '',
			operation_idempotency_key        TEXT NOT NULL,
			mutation_idempotency_key         TEXT NOT NULL,
			request_digest                   TEXT NOT NULL,
			mutation_digest                  TEXT NOT NULL,
			content_digest                   TEXT NOT NULL DEFAULT '',
			payload                          BLOB NOT NULL CHECK (length(payload) <= 524288),
			payload_bytes                    INTEGER NOT NULL CHECK (payload_bytes >= 0),
			state                            TEXT NOT NULL CHECK (state IN ('queued','leased','dispatching','ambiguous','dead_lettered','succeeded','abandoned','superseded','orphaned')),
			lease_owner                      TEXT NOT NULL DEFAULT '',
			lease_epoch                      INTEGER NOT NULL DEFAULT 0 CHECK (lease_epoch >= 0),
			lease_origin_state               TEXT NOT NULL DEFAULT '',
			lease_expires_at                 TIMESTAMP,
			send_started_at                  TIMESTAMP,
			request_deadline                 TIMESTAMP,
			dispatches                       INTEGER NOT NULL DEFAULT 0 CHECK (dispatches >= 0),
			attempts                         INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
			next_retry_at                    TIMESTAMP NOT NULL,
			max_age_at                       TIMESTAMP NOT NULL,
			payload_retain_until             TIMESTAMP NOT NULL,
			error_code                       TEXT NOT NULL DEFAULT '',
			error_message                    TEXT NOT NULL DEFAULT '',
			receipt_binding_digest           TEXT NOT NULL DEFAULT '',
			receipt_applied_generation       INTEGER NOT NULL DEFAULT 0,
			receipt_backend_version          TEXT NOT NULL DEFAULT '',
			receipt_backend_memory_id        TEXT NOT NULL DEFAULT '',
			receipt_content_digest           TEXT NOT NULL DEFAULT '',
			receipt_mutation_digest          TEXT NOT NULL DEFAULT '',
			receipt_completed_at             TIMESTAMP,
			actor                            TEXT NOT NULL,
			reason                           TEXT NOT NULL DEFAULT '',
			created_at                       TIMESTAMP NOT NULL,
			updated_at                       TIMESTAMP NOT NULL,
			completed_at                     TIMESTAMP,
			FOREIGN KEY (namespace_uid, memory_id) REFERENCES remote_memory_catalog(namespace_uid, id) ON DELETE RESTRICT
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_memory_operations_unresolved_memory
			ON memory_operations(namespace_uid, memory_id)
			WHERE state IN ('queued','leased','dispatching','ambiguous','dead_lettered')`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_memory_operations_operation_idempotency
			ON memory_operations(namespace_uid, operation_idempotency_key)`,
		`CREATE INDEX IF NOT EXISTS idx_memory_operations_claim
			ON memory_operations(namespace_uid, backend_uid, authority_epoch, routing_epoch, state, next_retry_at, sequence)`,
		`CREATE INDEX IF NOT EXISTS idx_memory_operations_proposal
			ON memory_operations(namespace_uid, proposal_id, sequence DESC)
			WHERE proposal_id <> ''`,
		`CREATE TABLE IF NOT EXISTS memory_idempotency (
			namespace_uid           TEXT NOT NULL,
			principal               TEXT NOT NULL,
			route                   TEXT NOT NULL,
			caller_key              TEXT NOT NULL,
			request_digest          TEXT NOT NULL,
			authority_epoch         INTEGER NOT NULL,
			routing_epoch           INTEGER NOT NULL,
			original_status         INTEGER NOT NULL,
			response_type           TEXT NOT NULL CHECK (response_type IN ('memory','operation','empty')),
			memory_id               TEXT NOT NULL DEFAULT '',
			operation_id            TEXT NOT NULL DEFAULT '',
			location                TEXT NOT NULL DEFAULT '',
			retry_after_seconds     INTEGER NOT NULL DEFAULT 0 CHECK (retry_after_seconds >= 0),
			response_digest         TEXT NOT NULL DEFAULT '',
			response_snapshot       BLOB NOT NULL DEFAULT X'' CHECK (length(response_snapshot) <= 524288),
			expires_at              TIMESTAMP NOT NULL,
			terminal_binding_digest TEXT NOT NULL DEFAULT '',
			created_at              TIMESTAMP NOT NULL,
			updated_at              TIMESTAMP NOT NULL,
			PRIMARY KEY (namespace_uid, principal, route, caller_key)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_memory_idempotency_expiry
			ON memory_idempotency(expires_at, namespace_uid)`,
		`CREATE TABLE IF NOT EXISTS memory_search_cursors (
			id             TEXT PRIMARY KEY,
			namespace_uid  TEXT NOT NULL,
			binding_digest TEXT NOT NULL,
			query_digest   TEXT NOT NULL,
			state_json     BLOB NOT NULL,
			expires_at     TIMESTAMP NOT NULL,
			created_at     TIMESTAMP NOT NULL,
			retired_at     TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_memory_search_cursors_expiry
			ON memory_search_cursors(expires_at, namespace_uid)`,
		`CREATE TABLE IF NOT EXISTS memory_audit (
			id                     TEXT PRIMARY KEY,
			namespace_name         TEXT NOT NULL,
			namespace_uid          TEXT NOT NULL,
			actor                  TEXT NOT NULL,
			action                 TEXT NOT NULL,
			reason                 TEXT NOT NULL DEFAULT '',
			previous_state         TEXT NOT NULL DEFAULT '',
			new_state              TEXT NOT NULL DEFAULT '',
			authority_epoch        INTEGER NOT NULL DEFAULT 0,
			previous_routing_epoch INTEGER NOT NULL DEFAULT 0,
			routing_epoch          INTEGER NOT NULL DEFAULT 0,
			memory_id              TEXT NOT NULL DEFAULT '',
			operation_id           TEXT NOT NULL DEFAULT '',
			proposal_id            TEXT NOT NULL DEFAULT '',
			request_digest         TEXT NOT NULL DEFAULT '',
			mutation_digest        TEXT NOT NULL DEFAULT '',
			content_digest         TEXT NOT NULL DEFAULT '',
			request_id             TEXT NOT NULL DEFAULT '',
			created_at             TIMESTAMP NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_memory_audit_namespace
			ON memory_audit(namespace_uid, created_at DESC, id DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_memory_audit_memory
			ON memory_audit(namespace_uid, memory_id, created_at DESC)
			WHERE memory_id <> ''`,
		`CREATE TABLE IF NOT EXISTS memory_force_orphan_fences (
			namespace_uid    TEXT PRIMARY KEY,
			namespace_name   TEXT NOT NULL,
			backend_uid      TEXT NOT NULL,
			authority_epoch  INTEGER NOT NULL,
			routing_epoch    INTEGER NOT NULL,
			tenant_id        TEXT NOT NULL,
			store_uuid       TEXT NOT NULL,
			actor            TEXT NOT NULL,
			reason           TEXT NOT NULL,
			request_id       TEXT NOT NULL DEFAULT '',
			created_at       TIMESTAMP NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS memory_restore_previews (
			namespace_uid    TEXT PRIMARY KEY,
			backend_uid      TEXT NOT NULL,
			authority_epoch  INTEGER NOT NULL,
			routing_epoch    INTEGER NOT NULL,
			tenant_id        TEXT NOT NULL,
			store_name       TEXT NOT NULL,
			store_uuid       TEXT NOT NULL,
			preview_digest   TEXT NOT NULL,
			created_at       TIMESTAMP NOT NULL,
				expires_at       TIMESTAMP NOT NULL
			)`,
		`CREATE TABLE IF NOT EXISTS memory_activation_recovery_receipts (
				namespace_uid  TEXT PRIMARY KEY,
				receipt_id     TEXT NOT NULL UNIQUE,
				namespace_name TEXT NOT NULL,
				backend_uid    TEXT NOT NULL,
				route_digest   TEXT NOT NULL,
				store_uuid     TEXT NOT NULL,
				manifest_digest TEXT NOT NULL,
				actor          TEXT NOT NULL,
				reason         TEXT NOT NULL,
				request_id     TEXT NOT NULL DEFAULT '',
				verified_at    TIMESTAMP NOT NULL
			)`,
		`CREATE TABLE IF NOT EXISTS memory_verified_checkpoints (
			namespace_uid             TEXT PRIMARY KEY,
			checkpoint_id             TEXT NOT NULL UNIQUE,
			namespace_name            TEXT NOT NULL,
			backend_uid               TEXT NOT NULL,
			authority_epoch           INTEGER NOT NULL,
			routing_epoch             INTEGER NOT NULL,
			tenant_id                 TEXT NOT NULL,
			store_uuid                TEXT NOT NULL,
			maximum_operation_sequence INTEGER NOT NULL CHECK (maximum_operation_sequence >= 0),
			checkpoint_digest         TEXT NOT NULL,
			actor                     TEXT NOT NULL,
			reason                    TEXT NOT NULL,
			request_id                TEXT NOT NULL DEFAULT '',
			verified_at               TIMESTAMP NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS memory_audit_purge_guard (
			singleton INTEGER PRIMARY KEY CHECK (singleton = 1)
		)`,
		`CREATE TABLE IF NOT EXISTS memory_governance_purge_watermarks (
			namespace_uid                TEXT PRIMARY KEY,
			purge_count                  INTEGER NOT NULL CHECK (purge_count > 0),
			maximum_operation_sequence   INTEGER NOT NULL CHECK (maximum_operation_sequence >= 0),
			before_at                    TIMESTAMP NOT NULL,
			cumulative_payloads_purged   INTEGER NOT NULL,
			cumulative_receipts_purged   INTEGER NOT NULL,
			cumulative_idempotency_purged INTEGER NOT NULL,
			cumulative_tombstones_purged INTEGER NOT NULL,
			cumulative_audit_rows_purged INTEGER NOT NULL,
			purge_digest                 TEXT NOT NULL,
			actor                       TEXT NOT NULL,
			reason                      TEXT NOT NULL,
			request_id                  TEXT NOT NULL DEFAULT '',
			updated_at                  TIMESTAMP NOT NULL
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_memory_proposals_apply_operation
			ON memory_proposals(namespace, apply_operation_id)
			WHERE apply_operation_id <> ''`,
		`CREATE TRIGGER IF NOT EXISTS trg_memories_fence_insert
			BEFORE INSERT ON memories
			WHEN EXISTS (SELECT 1 FROM memory_legacy_fences WHERE namespace_name = NEW.namespace)
			BEGIN SELECT RAISE(ABORT, 'legacy memory namespace fenced'); END`,
		`CREATE TRIGGER IF NOT EXISTS trg_memories_fence_update
			BEFORE UPDATE ON memories
			WHEN EXISTS (SELECT 1 FROM memory_legacy_fences WHERE namespace_name IN (OLD.namespace, NEW.namespace))
			BEGIN SELECT RAISE(ABORT, 'legacy memory namespace fenced'); END`,
		`CREATE TRIGGER IF NOT EXISTS trg_memories_fence_delete
			BEFORE DELETE ON memories
			WHEN EXISTS (SELECT 1 FROM memory_legacy_fences WHERE namespace_name = OLD.namespace)
			BEGIN SELECT RAISE(ABORT, 'legacy memory namespace fenced'); END`,
		`CREATE TRIGGER IF NOT EXISTS trg_memory_audit_immutable_update
			BEFORE UPDATE ON memory_audit
			BEGIN SELECT RAISE(ABORT, 'memory audit is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_memory_audit_immutable_delete`,
		`CREATE TRIGGER IF NOT EXISTS trg_memory_audit_immutable_delete
			BEFORE DELETE ON memory_audit
			WHEN NOT EXISTS (SELECT 1 FROM memory_audit_purge_guard WHERE singleton = 1)
			BEGIN SELECT RAISE(ABORT, 'memory audit is immutable'); END`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			return fmt.Errorf("memory governance migration failed: %w", err)
		}
	}
	if err := ensureSQLiteColumns(db, "memory_backend_bindings", []sqliteColumnMigration{
		{Name: "secret_name", Definition: "secret_name TEXT NOT NULL DEFAULT ''"},
		{Name: "secret_key", Definition: "secret_key TEXT NOT NULL DEFAULT ''"},
		{Name: "resolved_address_digest", Definition: "resolved_address_digest TEXT NOT NULL DEFAULT ''"},
		{Name: "server_certificate_digest", Definition: "server_certificate_digest TEXT NOT NULL DEFAULT ''"},
	}); err != nil {
		return err
	}
	if _, err := db.Exec(`UPDATE memory_backend_bindings
		SET state = 'recovering', validation_expires_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
		WHERE mode = 'remote' AND state IN ('accepting','draining')
			AND (resolved_address_digest = '' OR server_certificate_digest = '')`); err != nil {
		return fmt.Errorf("fence bindings missing validated route identity: %w", err)
	}
	if err := ensureSQLiteColumns(db, "remote_memory_catalog", []sqliteColumnMigration{
		{Name: "pending_session_name", Definition: "pending_session_name TEXT NOT NULL DEFAULT ''"},
		{Name: "pending_agent_name", Definition: "pending_agent_name TEXT NOT NULL DEFAULT ''"},
		{Name: "pending_task_name", Definition: "pending_task_name TEXT NOT NULL DEFAULT ''"},
		{Name: "pending_parent_task", Definition: "pending_parent_task TEXT NOT NULL DEFAULT ''"},
		{Name: "pending_source", Definition: "pending_source TEXT NOT NULL DEFAULT ''"},
		{Name: "pending_tags_json", Definition: "pending_tags_json TEXT NOT NULL DEFAULT '[]'"},
	}); err != nil {
		return err
	}
	if err := ensureSQLiteColumns(db, "memory_operations", []sqliteColumnMigration{
		{Name: "lease_origin_state", Definition: "lease_origin_state TEXT NOT NULL DEFAULT ''"},
		{Name: "dispatches", Definition: "dispatches INTEGER NOT NULL DEFAULT 0 CHECK (dispatches >= 0)"},
		{Name: "payload_bytes", Definition: "payload_bytes INTEGER NOT NULL DEFAULT 0 CHECK (payload_bytes >= 0)"},
	}); err != nil {
		return err
	}
	if _, err := db.Exec(`UPDATE memory_operations SET payload_bytes = length(payload)
		WHERE payload_bytes = 0 AND length(payload) > 0`); err != nil {
		return fmt.Errorf("backfill memory operation payload bytes: %w", err)
	}
	if err := ensureSQLiteColumns(db, "memory_idempotency", []sqliteColumnMigration{
		{Name: "response_snapshot", Definition: "response_snapshot BLOB NOT NULL DEFAULT X'' CHECK (length(response_snapshot) <= 524288)"},
	}); err != nil {
		return err
	}
	if err := ensureSQLiteColumns(db, "memory_search_cursors", []sqliteColumnMigration{
		{Name: "retired_at", Definition: "retired_at TIMESTAMP"},
	}); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_memory_search_cursors_active
		ON memory_search_cursors(namespace_uid, retired_at, expires_at)`); err != nil {
		return fmt.Errorf("create memory search cursor active index: %w", err)
	}
	if err := ensureMemoryOperationPayloadCapacity(db); err != nil {
		return err
	}
	return nil
}

func ensureMemoryOperationPayloadCapacity(db *sql.DB) error {
	var schema string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'memory_operations'`).Scan(&schema); err != nil {
		return fmt.Errorf("read memory_operations schema: %w", err)
	}
	normalized := strings.ToLower(strings.Join(strings.Fields(schema), ""))
	wanted := fmt.Sprintf("check(length(payload)<=%d)", maxMemoryOperationPayloadBytes)
	if strings.Contains(normalized, wanted) {
		return ensureMemoryOperationPayloadTriggers(db)
	}
	legacyLimit := ""
	for _, candidate := range []string{"2097152", "1048576"} {
		if strings.Contains(normalized, "check(length(payload)<="+candidate+")") {
			legacyLimit = candidate
			break
		}
	}
	if legacyLimit == "" {
		return fmt.Errorf("memory_operations payload constraint is unsupported")
	}

	// Existing oversized redo payloads are grandfathered for upgrade safety.
	// The application and triggers below reject every new or replacement payload
	// above the immutable 512 KiB cap. Once the retained rows are checkpointed
	// and purged, a later migration can rebuild the table with the tighter CHECK.
	var oversized int
	if err := db.QueryRow(`SELECT COUNT(*) FROM memory_operations WHERE length(payload) > ?`, maxMemoryOperationPayloadBytes).Scan(&oversized); err != nil {
		return fmt.Errorf("inspect memory operation payloads: %w", err)
	}
	if oversized > 0 {
		return ensureMemoryOperationPayloadTriggers(db)
	}

	open := strings.Index(schema, "(")
	if open < 0 {
		return fmt.Errorf("memory_operations schema is malformed")
	}
	rebuildSchema := "CREATE TABLE memory_operations_rebuild " + schema[open:]
	rebuildSchema = strings.Replace(rebuildSchema, legacyLimit, fmt.Sprint(maxMemoryOperationPayloadBytes), 1)
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin memory_operations payload migration: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	for _, statement := range []string{
		`DROP TABLE IF EXISTS memory_operations_rebuild`,
		rebuildSchema,
		`INSERT INTO memory_operations_rebuild SELECT * FROM memory_operations`,
		`DROP TABLE memory_operations`,
		`ALTER TABLE memory_operations_rebuild RENAME TO memory_operations`,
		`CREATE UNIQUE INDEX idx_memory_operations_unresolved_memory
			ON memory_operations(namespace_uid, memory_id)
			WHERE state IN ('queued','leased','dispatching','ambiguous','dead_lettered')`,
		`CREATE UNIQUE INDEX idx_memory_operations_operation_idempotency
			ON memory_operations(namespace_uid, operation_idempotency_key)`,
		`CREATE INDEX idx_memory_operations_claim
			ON memory_operations(namespace_uid, backend_uid, authority_epoch, routing_epoch, state, next_retry_at, sequence)`,
		`CREATE INDEX idx_memory_operations_proposal
			ON memory_operations(namespace_uid, proposal_id, sequence DESC)
			WHERE proposal_id <> ''`,
	} {
		if _, err := tx.Exec(statement); err != nil {
			return fmt.Errorf("rebuild memory_operations payload constraint: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit memory_operations payload migration: %w", err)
	}
	return ensureMemoryOperationPayloadTriggers(db)
}

func ensureMemoryOperationPayloadTriggers(db *sql.DB) error {
	for _, statement := range []string{
		`DROP TRIGGER IF EXISTS trg_memory_operations_payload_insert`,
		`DROP TRIGGER IF EXISTS trg_memory_operations_payload_update`,
		fmt.Sprintf(`CREATE TRIGGER trg_memory_operations_payload_insert
			BEFORE INSERT ON memory_operations WHEN length(NEW.payload) > %d
			BEGIN SELECT RAISE(ABORT, 'memory operation payload exceeds durable cap'); END`, maxMemoryOperationPayloadBytes),
		fmt.Sprintf(`CREATE TRIGGER trg_memory_operations_payload_update
			BEFORE UPDATE OF payload ON memory_operations WHEN length(NEW.payload) > %d
			BEGIN SELECT RAISE(ABORT, 'memory operation payload exceeds durable cap'); END`, maxMemoryOperationPayloadBytes),
	} {
		if _, err := db.Exec(statement); err != nil {
			return fmt.Errorf("ensure memory operation payload trigger: %w", err)
		}
	}
	return nil
}

func normalizeMemoryNow(now time.Time) time.Time {
	if now.IsZero() {
		return time.Now().UTC()
	}
	return now.UTC()
}

func boundedMemoryAuditText(value string) string {
	value = redact.SensitiveText(strings.TrimSpace(value))
	if len(value) <= maxMemoryAuditTextBytes {
		return value
	}
	end := maxMemoryAuditTextBytes
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}
	return value[:end]
}

func isLegacyMemoryFenceError(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), legacyMemoryFenceMessage)
}

func mapLegacyMemoryFenceError(err error) error {
	if err == nil {
		return nil
	}
	if isLegacyMemoryFenceError(err) {
		return fmt.Errorf("%w: legacy memory writes are fenced for remote authority", store.ErrConflict)
	}
	return err
}

func validMemoryTrust(trust store.MemoryTrust) bool {
	switch trust {
	case store.MemoryTrustUntrusted, store.MemoryTrustReviewed, store.MemoryTrustTrusted:
		return true
	default:
		return false
	}
}

func validMemoryMaterializationState(state store.MemoryMaterializationState) bool {
	switch state {
	case store.MemoryMaterializationPending, store.MemoryMaterializationActive, store.MemoryMaterializationDeleted,
		store.MemoryMaterializationDiverged, store.MemoryMaterializationLost, store.MemoryMaterializationOrphaned:
		return true
	default:
		return false
	}
}

func validMemoryBackendMode(mode store.MemoryBackendMode) bool {
	return mode == store.MemoryBackendModeLegacy || mode == store.MemoryBackendModeRemote
}

func validMemoryBindingState(state store.MemoryBackendBindingState) bool {
	switch state {
	case store.MemoryBackendBindingLegacy, store.MemoryBackendBindingValidating, store.MemoryBackendBindingAccepting,
		store.MemoryBackendBindingDraining, store.MemoryBackendBindingRecovering,
		store.MemoryBackendBindingDecommissioned, store.MemoryBackendBindingRemoved:
		return true
	default:
		return false
	}
}

func validMemoryOperationKind(kind store.MemoryOperationKind) bool {
	switch kind {
	case store.MemoryOperationCreate, store.MemoryOperationReplace, store.MemoryOperationDelete:
		return true
	default:
		return false
	}
}

func validMemoryOperationState(state store.MemoryOperationState) bool {
	switch state {
	case store.MemoryOperationQueued, store.MemoryOperationLeased, store.MemoryOperationDispatching,
		store.MemoryOperationAmbiguous, store.MemoryOperationDeadLettered, store.MemoryOperationSucceeded,
		store.MemoryOperationAbandoned, store.MemoryOperationSuperseded, store.MemoryOperationOrphaned:
		return true
	default:
		return false
	}
}

func ensureMemoryRowsAffectedConflict(result sql.Result, message string) error {
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return fmt.Errorf("%w: %s", store.ErrConflict, message)
	}
	return nil
}

func memoryPlaceholders(count int) string {
	if count <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

func newMemoryAuditID() string {
	id, err := uuid.NewV7()
	if err != nil {
		return "maudit-" + uuid.NewString()
	}
	return "maudit-" + id.String()
}

func applyLegacyMemoryGovernanceDefaults(memory *store.Memory) {
	if memory == nil {
		return
	}
	if memory.Generation == 0 {
		memory.Generation = 1
	}
	if memory.DesiredGeneration == 0 {
		memory.DesiredGeneration = memory.Generation
	}
	if memory.GovernanceRevision == 0 {
		memory.GovernanceRevision = 1
	}
	if memory.Deleted {
		memory.MaterializationState = store.MemoryMaterializationDeleted
		memory.ContentAvailable = false
	} else {
		memory.MaterializationState = store.MemoryMaterializationActive
		memory.ContentAvailable = true
	}
	if !validMemoryTrust(memory.Trust) {
		memory.Trust = store.MemoryTrustUntrusted
	}
	if memory.ContentAvailable && memory.ContentDigest == "" {
		digest := sha256.Sum256([]byte(memory.Content))
		memory.ContentDigest = hex.EncodeToString(digest[:])
	}
}

// UpsertControllerFeatureHeartbeat records a bounded liveness advertisement.
func (s *Store) UpsertControllerFeatureHeartbeat(ctx context.Context, heartbeat store.ControllerFeatureHeartbeat) error {
	heartbeat.InstanceID = strings.TrimSpace(heartbeat.InstanceID)
	heartbeat.Role = strings.TrimSpace(heartbeat.Role)
	if heartbeat.InstanceID == "" || heartbeat.Role == "" {
		return store.ValidationErrorf("instance id and role are required")
	}
	if heartbeat.FeatureEpoch < 0 {
		return store.ValidationErrorf("feature epoch cannot be negative")
	}
	heartbeat.LastHeartbeatAt = normalizeMemoryNow(heartbeat.LastHeartbeatAt)
	heartbeat.ExpiresAt = heartbeat.ExpiresAt.UTC()
	if heartbeat.ExpiresAt.IsZero() || !heartbeat.ExpiresAt.After(heartbeat.LastHeartbeatAt) ||
		heartbeat.ExpiresAt.Sub(heartbeat.LastHeartbeatAt) > maxFeatureHeartbeatTTL {
		return store.ValidationErrorf("heartbeat expiry must be after its heartbeat time and within the maximum TTL")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO controller_feature_heartbeats
		(instance_id, role, feature_epoch, last_heartbeat_at, expires_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(instance_id) DO UPDATE SET
			role = excluded.role,
			feature_epoch = excluded.feature_epoch,
			last_heartbeat_at = excluded.last_heartbeat_at,
			expires_at = excluded.expires_at`,
		heartbeat.InstanceID, heartbeat.Role, heartbeat.FeatureEpoch, heartbeat.LastHeartbeatAt, heartbeat.ExpiresAt.UTC()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO controller_feature_epoch_history
		(role, feature_epoch, first_seen_at, last_seen_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(role, feature_epoch) DO UPDATE SET
			first_seen_at = MIN(first_seen_at, excluded.first_seen_at),
			last_seen_at = MAX(last_seen_at, excluded.last_seen_at)`,
		heartbeat.Role, heartbeat.FeatureEpoch, heartbeat.LastHeartbeatAt, heartbeat.LastHeartbeatAt); err != nil {
		return err
	}
	return tx.Commit()
}

// ListLiveControllerFeatureHeartbeats lists non-expired memory feature advertisements.
func (s *Store) ListLiveControllerFeatureHeartbeats(ctx context.Context, now time.Time) ([]store.ControllerFeatureHeartbeat, error) {
	now = normalizeMemoryNow(now)
	rows, err := s.db.QueryContext(ctx, `SELECT instance_id, role, feature_epoch, last_heartbeat_at, expires_at
		FROM controller_feature_heartbeats WHERE expires_at > ? AND last_heartbeat_at >= ? AND last_heartbeat_at <= ?
		ORDER BY instance_id`, now, now.Add(-maxFeatureHeartbeatTTL), now.Add(maxFeatureHeartbeatClockSkew))
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var heartbeats []store.ControllerFeatureHeartbeat
	for rows.Next() {
		var heartbeat store.ControllerFeatureHeartbeat
		if err := rows.Scan(&heartbeat.InstanceID, &heartbeat.Role, &heartbeat.FeatureEpoch, &heartbeat.LastHeartbeatAt, &heartbeat.ExpiresAt); err != nil {
			return nil, err
		}
		heartbeats = append(heartbeats, heartbeat)
	}
	return heartbeats, rows.Err()
}

// ActivateMemoryBackend atomically archives legacy memory, installs a database fence,
// and commits the remote binding identity.
//
//nolint:gocyclo // Activation intentionally validates and commits the full fail-closed cutover in one transaction.
func (s *Store) ActivateMemoryBackend(ctx context.Context, activation store.MemoryBackendActivation) (*store.MemoryBackendActivationResult, error) {
	activation.Now = normalizeMemoryNow(activation.Now)
	binding := activation.Binding
	binding.Namespace = strings.TrimSpace(binding.Namespace)
	binding.NamespaceUID = strings.TrimSpace(binding.NamespaceUID)
	binding.ClusterID = strings.TrimSpace(binding.ClusterID)
	binding.BackendUID = strings.TrimSpace(binding.BackendUID)
	binding.SecretName = strings.TrimSpace(binding.SecretName)
	binding.SecretKey = strings.TrimSpace(binding.SecretKey)
	binding.TenantID = strings.TrimSpace(binding.TenantID)
	binding.StoreUUID = strings.TrimSpace(binding.StoreUUID)
	binding.Protocol = strings.TrimSpace(binding.Protocol)
	if binding.Mode == "" {
		binding.Mode = store.MemoryBackendModeRemote
	}
	if binding.State == "" {
		binding.State = store.MemoryBackendBindingAccepting
	}
	if binding.ActivationEpoch == 0 {
		binding.ActivationEpoch = binding.AuthorityEpoch
	}
	if binding.MinimumFeatureEpoch == 0 {
		binding.MinimumFeatureEpoch = activation.RequiredFeatureEpoch
	}
	binding.ValidationExpiresAt = binding.ValidationExpiresAt.UTC()
	if binding.Namespace == "" || binding.NamespaceUID == "" || binding.ClusterID == "" || binding.BackendUID == "" ||
		binding.SpecDigest == "" || binding.EndpointDigest == "" || binding.ResolvedAddressDigest == "" ||
		binding.ServerCertificateDigest == "" || binding.SecretName == "" || binding.SecretKey == "" ||
		binding.SecretUID == "" || binding.SecretResourceVersion == "" ||
		binding.TenantID == "" || binding.StoreName == "" || binding.StoreUUID == "" || binding.OwnershipClaim == "" ||
		binding.CapabilityRevision == "" || binding.Protocol == "" {
		return nil, store.ValidationErrorf("complete namespace and backend binding identity is required")
	}
	if binding.Mode != store.MemoryBackendModeRemote || binding.BackendGeneration <= 0 || binding.AuthorityEpoch <= 0 ||
		binding.RoutingEpoch <= 0 || binding.ActivationEpoch <= 0 {
		return nil, store.ValidationErrorf("remote mode and positive backend, authority, routing, and activation epochs are required")
	}
	if binding.State != store.MemoryBackendBindingAccepting {
		return nil, store.ValidationErrorf("activation binding state must be accepting")
	}
	if activation.RequiredFeatureEpoch < 0 || binding.MinimumFeatureEpoch != activation.RequiredFeatureEpoch {
		return nil, store.ValidationErrorf("binding feature epoch must exactly match the activation requirement")
	}
	if strings.TrimSpace(binding.ResolvedAddressDigest) == "" || strings.TrimSpace(binding.ServerCertificateDigest) == "" {
		return nil, store.ValidationErrorf("resolved address and server certificate digests are required")
	}
	if binding.ValidationExpiresAt.IsZero() || !binding.ValidationExpiresAt.After(activation.Now) {
		return nil, store.ValidationErrorf("binding validation expiry must be in the future")
	}
	if strings.TrimSpace(activation.Actor) == "" || strings.TrimSpace(activation.Reason) == "" {
		return nil, store.ValidationErrorf("activation actor and reason are required")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	if binding.MinimumFeatureEpoch > 0 {
		if binding.MinimumFeatureEpoch > 1 {
			var prerequisiteEpochs int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM controller_feature_epoch_history
				WHERE feature_epoch > 0 AND feature_epoch < ?
					AND first_seen_at <= ?
					AND role IN ('serving','dispatching','serving_dispatching')`,
				binding.MinimumFeatureEpoch, activation.Now.Add(maxFeatureHeartbeatClockSkew)).Scan(&prerequisiteEpochs); err != nil {
				return nil, err
			}
			if prerequisiteEpochs == 0 {
				return nil, fmt.Errorf("%w: a prior foundation feature epoch has not been observed", store.ErrNotReady)
			}
		}
		var live, incompatible int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(CASE WHEN feature_epoch < ? THEN 1 ELSE 0 END), 0)
			FROM controller_feature_heartbeats WHERE expires_at > ?
				AND last_heartbeat_at >= ? AND last_heartbeat_at <= ?
				AND role IN ('serving','dispatching','serving_dispatching')`, binding.MinimumFeatureEpoch, activation.Now,
			activation.Now.Add(-maxFeatureHeartbeatTTL), activation.Now.Add(maxFeatureHeartbeatClockSkew)).
			Scan(&live, &incompatible); err != nil {
			return nil, err
		}
		if live == 0 || incompatible != 0 {
			return nil, fmt.Errorf("%w: controller feature epoch activation barrier is not satisfied", store.ErrNotReady)
		}
	}

	if err := requireFreshMemoryActivationRecoveryReceipt(ctx, tx, binding, activation.Now); err != nil {
		return nil, err
	}

	existing, err := getMemoryBackendBindingQuery(ctx, tx, binding.NamespaceUID)
	reactivating := false
	if err == nil {
		if sameMemoryBindingIdentity(existing, &binding) && existing.State == store.MemoryBackendBindingAccepting {
			if err := tx.Commit(); err != nil {
				return nil, err
			}
			return &store.MemoryBackendActivationResult{Binding: *existing, AlreadyActive: true}, nil
		}
		if existing.Namespace == binding.Namespace && existing.Mode == store.MemoryBackendModeLegacy &&
			existing.State == store.MemoryBackendBindingLegacy && binding.AuthorityEpoch > existing.AuthorityEpoch &&
			binding.RoutingEpoch > existing.RoutingEpoch {
			reactivating = true
		} else {
			return nil, fmt.Errorf("%w: namespace uid already has a different memory backend binding", store.ErrConflict)
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if !reactivating {
		conflicting, conflictErr := getMemoryBackendBindingByNamespaceQuery(ctx, tx, binding.Namespace)
		if conflictErr == nil {
			if err := rotateTerminalMemoryNamespaceIdentity(ctx, tx, conflicting, binding.NamespaceUID, activation); err != nil {
				return nil, err
			}
		} else if !errors.Is(conflictErr, sql.ErrNoRows) {
			return nil, conflictErr
		}
	}

	result, err := tx.ExecContext(ctx, `INSERT INTO legacy_memory_archive
		(namespace_uid, namespace_name, id, session_name, agent_name, task_name, parent_task, source, source_proposal_id,
		 content, tags_json, disabled, deleted, created_at, updated_at, last_recalled_at, recalled_count,
		 backend_uid, authority_epoch, routing_epoch, archived_at)
		SELECT ?, namespace, id, session_name, agent_name, task_name, parent_task, source, source_proposal_id,
		 content, tags_json, disabled, deleted, created_at, updated_at, last_recalled_at, recalled_count,
		 ?, ?, ?, ? FROM memories WHERE namespace = ?`,
		binding.NamespaceUID, binding.BackendUID, binding.AuthorityEpoch, binding.RoutingEpoch, activation.Now, binding.Namespace)
	if err != nil {
		return nil, err
	}
	archived, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memories WHERE namespace = ?`, binding.Namespace); err != nil {
		return nil, err
	}
	binding.ActivatedAt = activation.Now
	binding.UpdatedAt = activation.Now
	if reactivating {
		result, err := tx.ExecContext(ctx, `UPDATE memory_backend_bindings SET
			cluster_id = ?, mode = ?, backend_uid = ?, backend_generation = ?, authority_epoch = ?, routing_epoch = ?,
			spec_digest = ?, endpoint_digest = ?, resolved_address_digest = ?, server_certificate_digest = ?,
			secret_name = ?, secret_key = ?, secret_uid = ?, secret_resource_version = ?, tenant_id = ?,
			store_name = ?, store_uuid = ?, ownership_claim = ?, capability_revision = ?, protocol = ?, state = ?,
			activation_epoch = ?, minimum_feature_epoch = ?, validation_expires_at = ?, activated_at = ?, updated_at = ?, decommissioned_at = NULL
			WHERE namespace_uid = ? AND mode = ? AND state = ? AND authority_epoch = ? AND routing_epoch = ?`,
			binding.ClusterID, binding.Mode, binding.BackendUID, binding.BackendGeneration, binding.AuthorityEpoch, binding.RoutingEpoch,
			binding.SpecDigest, binding.EndpointDigest, binding.ResolvedAddressDigest, binding.ServerCertificateDigest,
			binding.SecretName, binding.SecretKey, binding.SecretUID, binding.SecretResourceVersion, binding.TenantID,
			binding.StoreName, binding.StoreUUID, binding.OwnershipClaim, binding.CapabilityRevision, binding.Protocol, binding.State,
			binding.ActivationEpoch, binding.MinimumFeatureEpoch, binding.ValidationExpiresAt, binding.ActivatedAt, binding.UpdatedAt,
			binding.NamespaceUID, store.MemoryBackendModeLegacy, store.MemoryBackendBindingLegacy,
			existing.AuthorityEpoch, existing.RoutingEpoch)
		if err != nil {
			return nil, err
		}
		if err := ensureMemoryRowsAffectedConflict(result, "legacy authority changed before remote reactivation"); err != nil {
			return nil, err
		}
	} else if _, err := tx.ExecContext(ctx, `INSERT INTO memory_backend_bindings
		(namespace_name, namespace_uid, cluster_id, mode, backend_uid, backend_generation, authority_epoch, routing_epoch,
		 spec_digest, endpoint_digest, resolved_address_digest, server_certificate_digest,
		 secret_name, secret_key, secret_uid, secret_resource_version, tenant_id, store_name, store_uuid,
		 ownership_claim, capability_revision, protocol, state, activation_epoch, minimum_feature_epoch,
		 validation_expires_at, activated_at, updated_at, decommissioned_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)`,
		binding.Namespace, binding.NamespaceUID, binding.ClusterID, binding.Mode, binding.BackendUID, binding.BackendGeneration,
		binding.AuthorityEpoch, binding.RoutingEpoch, binding.SpecDigest, binding.EndpointDigest,
		binding.ResolvedAddressDigest, binding.ServerCertificateDigest, binding.SecretName, binding.SecretKey,
		binding.SecretUID, binding.SecretResourceVersion, binding.TenantID, binding.StoreName, binding.StoreUUID, binding.OwnershipClaim,
		binding.CapabilityRevision, binding.Protocol, binding.State, binding.ActivationEpoch, binding.MinimumFeatureEpoch,
		binding.ValidationExpiresAt, binding.ActivatedAt, binding.UpdatedAt); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO memory_legacy_fences
		(namespace_name, namespace_uid, backend_uid, authority_epoch, routing_epoch, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, binding.Namespace, binding.NamespaceUID, binding.BackendUID,
		binding.AuthorityEpoch, binding.RoutingEpoch, activation.Now); err != nil {
		return nil, err
	}
	if err := insertMemoryAudit(ctx, tx, store.MemoryAuditRecord{
		Namespace: binding.Namespace, NamespaceUID: binding.NamespaceUID, Actor: activation.Actor,
		Action: "backend.activate", Reason: activation.Reason, PreviousState: string(store.MemoryBackendBindingLegacy),
		NewState: string(binding.State), AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		RequestID: activation.RequestID, CreatedAt: activation.Now,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &store.MemoryBackendActivationResult{Binding: binding, ArchivedMemories: int(archived)}, nil
}

// GetMemoryBackendBinding returns the durable binding for a namespace incarnation.
func (s *Store) GetMemoryBackendBinding(ctx context.Context, namespaceUID string) (*store.MemoryBackendBinding, error) {
	binding, err := getMemoryBackendBindingQuery(ctx, s.db, strings.TrimSpace(namespaceUID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return binding, err
}

// GetMemoryBackendBindingByNamespace returns the exact durable binding for a namespace name.
func (s *Store) GetMemoryBackendBindingByNamespace(ctx context.Context, namespace string) (*store.MemoryBackendBinding, error) {
	namespace = strings.TrimSpace(namespace)
	if namespace == "" {
		return nil, store.ValidationErrorf("namespace is required")
	}
	binding, err := getMemoryBackendBindingByNamespaceQuery(ctx, s.db, namespace)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return binding, err
}

// ListMemoryBackendBindings discovers safe durable binding metadata for dispatch and recovery.
func (s *Store) ListMemoryBackendBindings(ctx context.Context, filter store.MemoryBackendBindingFilter) ([]store.MemoryBackendBinding, error) {
	filter.BeforeNamespaceUID = strings.TrimSpace(filter.BeforeNamespaceUID)
	query := selectMemoryBackendBindingSQL() + ` WHERE 1 = 1`
	var args []any
	if len(filter.Modes) > 0 {
		query += ` AND mode IN (` + memoryPlaceholders(len(filter.Modes)) + `)`
		for _, mode := range filter.Modes {
			if !validMemoryBackendMode(mode) {
				return nil, store.ValidationErrorf("invalid memory backend mode %q", mode)
			}
			args = append(args, mode)
		}
	}
	if len(filter.States) > 0 {
		query += ` AND state IN (` + memoryPlaceholders(len(filter.States)) + `)`
		for _, state := range filter.States {
			if !validMemoryBindingState(state) {
				return nil, store.ValidationErrorf("invalid memory backend binding state %q", state)
			}
			args = append(args, state)
		}
	}
	if filter.BeforeNamespaceUID != "" {
		query += ` AND namespace_uid > ?`
		args = append(args, filter.BeforeNamespaceUID)
	}
	query += ` ORDER BY namespace_uid LIMIT ?`
	args = append(args, boundedLimit(filter.Limit, defaultGovernedMemoryLimit, maxGovernedMemoryLimit))
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var bindings []store.MemoryBackendBinding
	for rows.Next() {
		binding, err := scanMemoryBackendBinding(rows)
		if err != nil {
			return nil, err
		}
		bindings = append(bindings, *binding)
	}
	return bindings, rows.Err()
}

// ForEachMemoryBackendBinding visits every matching binding in namespace-UID order.
// It owns pagination so callers cannot accidentally stop at the per-query hard cap.
func (s *Store) ForEachMemoryBackendBinding(ctx context.Context, filter store.MemoryBackendBindingFilter, visit store.MemoryBackendBindingVisitor) error {
	if visit == nil {
		return store.ValidationErrorf("memory backend binding visitor is required")
	}
	cursor := strings.TrimSpace(filter.BeforeNamespaceUID)
	for {
		filter.BeforeNamespaceUID = cursor
		filter.Limit = maxGovernedMemoryLimit
		bindings, err := s.ListMemoryBackendBindings(ctx, filter)
		if err != nil {
			return err
		}
		for _, binding := range bindings {
			if err := visit(binding); err != nil {
				return err
			}
		}
		if len(bindings) < maxGovernedMemoryLimit {
			return nil
		}
		next := bindings[len(bindings)-1].NamespaceUID
		if next == "" || next <= cursor {
			return fmt.Errorf("memory backend binding pagination did not advance")
		}
		cursor = next
	}
}

// MaxRequiredMemoryFeatureEpoch returns the highest epoch required by any remote binding.
func (s *Store) MaxRequiredMemoryFeatureEpoch(ctx context.Context) (int64, error) {
	var maximum int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(minimum_feature_epoch), 0)
		FROM memory_backend_bindings WHERE mode = ?`, store.MemoryBackendModeRemote).Scan(&maximum); err != nil {
		return 0, err
	}
	return maximum, nil
}

// TransitionMemoryBackendBinding commits a lifecycle/routing transition with a CAS.
func (s *Store) TransitionMemoryBackendBinding(ctx context.Context, transition store.MemoryBackendTransition) (*store.MemoryBackendBinding, error) {
	transition.Now = normalizeMemoryNow(transition.Now)
	transition.NamespaceUID = strings.TrimSpace(transition.NamespaceUID)
	transition.BackendUID = strings.TrimSpace(transition.BackendUID)
	if transition.NamespaceUID == "" || transition.BackendUID == "" || transition.ExpectedState == "" || !validMemoryBindingState(transition.State) {
		return nil, store.ValidationErrorf("namespace uid, backend uid, expected state, and valid target state are required")
	}
	stateOnlyDrain := transition.ExpectedState == store.MemoryBackendBindingAccepting &&
		transition.State == store.MemoryBackendBindingDraining &&
		transition.RoutingEpoch == transition.ExpectedRoutingEpoch
	if transition.ExpectedRoutingEpoch <= 0 || transition.RoutingEpoch < transition.ExpectedRoutingEpoch ||
		(transition.RoutingEpoch == transition.ExpectedRoutingEpoch && !stateOnlyDrain) {
		return nil, store.ValidationErrorf("routing epoch must advance unless entering the state-only draining barrier")
	}
	if strings.TrimSpace(transition.Actor) == "" || strings.TrimSpace(transition.Reason) == "" {
		return nil, store.ValidationErrorf("transition actor and reason are required")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	binding, err := getMemoryBackendBindingQuery(ctx, tx, transition.NamespaceUID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if binding.BackendUID != transition.BackendUID || binding.State != transition.ExpectedState || binding.RoutingEpoch != transition.ExpectedRoutingEpoch {
		return nil, fmt.Errorf("%w: memory backend binding changed", store.ErrConflict)
	}
	if !allowedMemoryBindingTransition(binding.State, transition.State) {
		return nil, store.ValidationErrorf("memory backend binding transition %q -> %q is not allowed", binding.State, transition.State)
	}
	requireCompleteRebase := transition.State == store.MemoryBackendBindingAccepting ||
		transition.State == store.MemoryBackendBindingDecommissioned
	if transition.RoutingEpoch > transition.ExpectedRoutingEpoch {
		if err := rebaseRemoteMemoryCatalogRoutingEpoch(ctx, tx, binding, transition.RoutingEpoch, transition.Now, requireCompleteRebase); err != nil {
			return nil, err
		}
	}
	var decommissionedAt any
	if transition.State == store.MemoryBackendBindingDecommissioned {
		decommissionedAt = transition.Now
	}
	result, err := tx.ExecContext(ctx, `UPDATE memory_backend_bindings
		SET state = ?, routing_epoch = ?, updated_at = ?,
			decommissioned_at = CASE WHEN ? IS NULL THEN decommissioned_at ELSE ? END
		WHERE namespace_uid = ? AND backend_uid = ? AND state = ? AND routing_epoch = ?`,
		transition.State, transition.RoutingEpoch, transition.Now, decommissionedAt, decommissionedAt,
		transition.NamespaceUID, transition.BackendUID, transition.ExpectedState, transition.ExpectedRoutingEpoch)
	if err != nil {
		return nil, err
	}
	if err := ensureMemoryRowsAffectedConflict(result, "memory backend binding changed"); err != nil {
		return nil, err
	}
	if transition.RoutingEpoch > transition.ExpectedRoutingEpoch {
		if _, err := tx.ExecContext(ctx, `UPDATE memory_legacy_fences SET routing_epoch = ? WHERE namespace_uid = ?`, transition.RoutingEpoch, transition.NamespaceUID); err != nil {
			return nil, err
		}
	}
	if err := insertMemoryAudit(ctx, tx, store.MemoryAuditRecord{
		Namespace: binding.Namespace, NamespaceUID: binding.NamespaceUID, Actor: transition.Actor,
		Action: "backend.transition", Reason: transition.Reason, PreviousState: string(binding.State), NewState: string(transition.State),
		AuthorityEpoch: binding.AuthorityEpoch, PreviousRoutingEpoch: binding.RoutingEpoch, RoutingEpoch: transition.RoutingEpoch,
		RequestID: transition.RequestID, CreatedAt: transition.Now,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetMemoryBackendBinding(ctx, transition.NamespaceUID)
}

func rebaseRemoteMemoryCatalogRoutingEpoch(
	ctx context.Context,
	tx *sql.Tx,
	binding *store.MemoryBackendBinding,
	targetRoutingEpoch int64,
	now time.Time,
	requireNoUnresolved bool,
) error {
	var unresolved int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_operations
		WHERE namespace_uid = ? AND backend_uid = ? AND authority_epoch = ? AND routing_epoch < ?
			AND state IN ('queued','leased','dispatching','ambiguous','dead_lettered')`,
		binding.NamespaceUID, binding.BackendUID, binding.AuthorityEpoch, targetRoutingEpoch).Scan(&unresolved); err != nil {
		return err
	}
	if unresolved > 0 {
		if requireNoUnresolved {
			return fmt.Errorf("%w: unresolved old-route memory operations block routing transition", store.ErrConflict)
		}
		return nil
	}

	var pendingCatalog int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM remote_memory_catalog
		WHERE namespace_uid = ? AND backend_uid = ? AND authority_epoch = ? AND routing_epoch < ?
			AND pending_operation_id <> ''`, binding.NamespaceUID, binding.BackendUID,
		binding.AuthorityEpoch, targetRoutingEpoch).Scan(&pendingCatalog); err != nil {
		return err
	}
	if pendingCatalog > 0 {
		return fmt.Errorf("%w: pending old-route catalog rows block routing transition", store.ErrConflict)
	}

	var mismatchedCatalog int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM remote_memory_catalog
		WHERE namespace_uid = ? AND backend_uid = ? AND authority_epoch = ? AND routing_epoch < ?
			AND (tenant_id <> ? OR store_uuid <> ?)`, binding.NamespaceUID, binding.BackendUID,
		binding.AuthorityEpoch, targetRoutingEpoch, binding.TenantID, binding.StoreUUID).Scan(&mismatchedCatalog); err != nil {
		return err
	}
	if mismatchedCatalog > 0 {
		return fmt.Errorf("%w: catalog authority identity mismatch blocks routing transition", store.ErrConflict)
	}

	_, err := tx.ExecContext(ctx, `UPDATE remote_memory_catalog
		SET routing_epoch = ?, updated_at = ?
		WHERE namespace_uid = ? AND backend_uid = ? AND authority_epoch = ? AND routing_epoch < ?
			AND tenant_id = ? AND store_uuid = ? AND pending_operation_id = ''`,
		targetRoutingEpoch, now, binding.NamespaceUID, binding.BackendUID,
		binding.AuthorityEpoch, targetRoutingEpoch, binding.TenantID, binding.StoreUUID)
	return err
}

// RefreshMemoryBackendBinding updates fresh validation metadata without changing authority.
// A routing-epoch increase fences previously validated credentials/endpoints; an equal
// routing epoch is permitted only for freshness renewal of otherwise identical identity.
// A Draining or Recovering binding may atomically resume Accepting only with an
// acknowledged routing advance, no unresolved old-route work, and the complete
// validated route snapshot.
//
//nolint:gocyclo // Refresh validates and rebases the complete durable authority and route identity atomically.
func (s *Store) RefreshMemoryBackendBinding(ctx context.Context, refresh store.MemoryBackendBindingRefresh) (*store.MemoryBackendBinding, error) {
	refresh.Now = normalizeMemoryNow(refresh.Now)
	binding := refresh.Binding
	binding.Namespace = strings.TrimSpace(binding.Namespace)
	binding.NamespaceUID = strings.TrimSpace(binding.NamespaceUID)
	binding.ClusterID = strings.TrimSpace(binding.ClusterID)
	binding.BackendUID = strings.TrimSpace(binding.BackendUID)
	binding.SecretName = strings.TrimSpace(binding.SecretName)
	binding.SecretKey = strings.TrimSpace(binding.SecretKey)
	binding.TenantID = strings.TrimSpace(binding.TenantID)
	binding.StoreName = strings.TrimSpace(binding.StoreName)
	binding.StoreUUID = strings.TrimSpace(binding.StoreUUID)
	binding.Protocol = strings.TrimSpace(binding.Protocol)
	if binding.Namespace == "" || binding.NamespaceUID == "" || binding.ClusterID == "" || binding.BackendUID == "" ||
		binding.BackendGeneration <= 0 || binding.AuthorityEpoch <= 0 || binding.RoutingEpoch <= 0 ||
		refresh.ExpectedRoutingEpoch <= 0 || binding.RoutingEpoch < refresh.ExpectedRoutingEpoch ||
		(binding.State != store.MemoryBackendBindingAccepting && binding.State != store.MemoryBackendBindingDraining) ||
		binding.SpecDigest == "" || binding.EndpointDigest == "" || binding.ResolvedAddressDigest == "" ||
		binding.ServerCertificateDigest == "" || binding.SecretName == "" || binding.SecretKey == "" ||
		binding.SecretUID == "" ||
		binding.SecretResourceVersion == "" || binding.TenantID == "" || binding.StoreName == "" ||
		binding.StoreUUID == "" || binding.OwnershipClaim == "" || binding.CapabilityRevision == "" || binding.Protocol == "" ||
		strings.TrimSpace(refresh.Actor) == "" || strings.TrimSpace(refresh.Reason) == "" {
		return nil, store.ValidationErrorf("complete binding refresh identity, epochs, actor, and reason are required")
	}
	if binding.ValidationExpiresAt.IsZero() || !binding.ValidationExpiresAt.After(refresh.Now) {
		return nil, store.ValidationErrorf("binding validation expiry must be in the future")
	}
	binding.ValidationExpiresAt = binding.ValidationExpiresAt.UTC()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	current, err := getMemoryBackendBindingQuery(ctx, tx, binding.NamespaceUID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if current.RoutingEpoch != refresh.ExpectedRoutingEpoch || current.Mode != store.MemoryBackendModeRemote ||
		(current.State != store.MemoryBackendBindingAccepting && current.State != store.MemoryBackendBindingDraining &&
			current.State != store.MemoryBackendBindingRecovering) {
		return nil, fmt.Errorf("%w: memory backend binding changed before validation refresh", store.ErrConflict)
	}
	if binding.BackendGeneration < current.BackendGeneration {
		return nil, fmt.Errorf("%w: backend generation cannot move backward during validation refresh", store.ErrConflict)
	}
	if current.TenantID != binding.TenantID || current.StoreName != binding.StoreName || current.StoreUUID != binding.StoreUUID {
		return nil, fmt.Errorf("%w: tenant or store identity change requires a new memory authority", store.ErrConflict)
	}
	if !sameMemoryRefreshAuthorityIdentity(current, &binding) {
		return nil, fmt.Errorf("%w: memory authority identity changed before validation refresh", store.ErrConflict)
	}
	stateChanged := current.State != binding.State
	if stateChanged && ((current.State != store.MemoryBackendBindingDraining && current.State != store.MemoryBackendBindingRecovering) ||
		binding.State != store.MemoryBackendBindingAccepting || binding.RoutingEpoch <= current.RoutingEpoch) {
		return nil, fmt.Errorf("%w: accepting resume requires a draining or recovering binding and acknowledged routing advance", store.ErrConflict)
	}
	routeChanged := !sameMemoryRefreshRouteIdentity(current, &binding)
	if binding.RoutingEpoch == current.RoutingEpoch && routeChanged {
		return nil, fmt.Errorf("%w: route-sensitive validation changes require a routing epoch increment", store.ErrConflict)
	}

	if binding.RoutingEpoch > current.RoutingEpoch {
		if err := rebaseRemoteMemoryCatalogRoutingEpoch(ctx, tx, current, binding.RoutingEpoch, refresh.Now, true); err != nil {
			return nil, err
		}
	}

	result, err := tx.ExecContext(ctx, `UPDATE memory_backend_bindings SET
		backend_generation = ?, routing_epoch = ?, spec_digest = ?, endpoint_digest = ?, resolved_address_digest = ?,
		server_certificate_digest = ?, secret_name = ?, secret_key = ?, secret_uid = ?, secret_resource_version = ?,
		ownership_claim = ?, capability_revision = ?, protocol = ?,
		validation_expires_at = ?, state = ?, updated_at = ?
		WHERE namespace_uid = ? AND backend_uid = ? AND backend_generation = ? AND authority_epoch = ? AND routing_epoch = ?
			AND tenant_id = ? AND store_name = ? AND store_uuid = ? AND mode = ? AND state = ?`,
		binding.BackendGeneration, binding.RoutingEpoch, binding.SpecDigest, binding.EndpointDigest,
		binding.ResolvedAddressDigest, binding.ServerCertificateDigest, binding.SecretName, binding.SecretKey,
		binding.SecretUID, binding.SecretResourceVersion, binding.OwnershipClaim, binding.CapabilityRevision, binding.Protocol,
		binding.ValidationExpiresAt, binding.State, refresh.Now, binding.NamespaceUID, binding.BackendUID,
		current.BackendGeneration, binding.AuthorityEpoch, refresh.ExpectedRoutingEpoch,
		binding.TenantID, binding.StoreName, binding.StoreUUID,
		store.MemoryBackendModeRemote, current.State)
	if err != nil {
		return nil, err
	}
	if err := ensureMemoryRowsAffectedConflict(result, "memory backend binding changed before validation refresh"); err != nil {
		return nil, err
	}
	if binding.RoutingEpoch != current.RoutingEpoch {
		result, err := tx.ExecContext(ctx, `UPDATE memory_legacy_fences SET routing_epoch = ?
			WHERE namespace_uid = ? AND backend_uid = ? AND authority_epoch = ? AND routing_epoch = ?`,
			binding.RoutingEpoch, binding.NamespaceUID, binding.BackendUID, binding.AuthorityEpoch, current.RoutingEpoch)
		if err != nil {
			return nil, err
		}
		if err := ensureMemoryRowsAffectedConflict(result, "legacy memory fence changed before routing refresh"); err != nil {
			return nil, err
		}
	}
	if err := insertMemoryAudit(ctx, tx, store.MemoryAuditRecord{
		Namespace: current.Namespace, NamespaceUID: current.NamespaceUID, Actor: refresh.Actor,
		Action: "backend.validation.refresh", Reason: refresh.Reason, PreviousState: string(current.State), NewState: string(binding.State),
		AuthorityEpoch: current.AuthorityEpoch, PreviousRoutingEpoch: current.RoutingEpoch, RoutingEpoch: binding.RoutingEpoch,
		RequestID: refresh.RequestID, CreatedAt: refresh.Now,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetMemoryBackendBinding(ctx, binding.NamespaceUID)
}

// PreviewLegacyMemoryRestore reports whether clean restore can proceed and issues
// a short-lived, identity-bound preview token. The token is retained server-side
// as well so existing callers that immediately preview then apply remain safe.
func (s *Store) PreviewLegacyMemoryRestore(ctx context.Context, namespaceUID, backendUID string) (*store.LegacyMemoryRestorePreview, error) {
	namespaceUID = strings.TrimSpace(namespaceUID)
	backendUID = strings.TrimSpace(backendUID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	preview, err := previewLegacyMemoryRestoreQuery(ctx, tx, namespaceUID, backendUID)
	if err != nil {
		return nil, err
	}
	if !preview.Restorable {
		if _, err := tx.ExecContext(ctx, `DELETE FROM memory_restore_previews WHERE namespace_uid = ?`, namespaceUID); err != nil {
			return nil, err
		}
	} else {
		now := normalizeMemoryNow(time.Time{})
		expiresAt := now.Add(restorePreviewTTL)
		preview.PreviewDigest = legacyRestorePreviewDigest(preview)
		preview.PreviewExpiresAt = &expiresAt
		_, err := tx.ExecContext(ctx, `INSERT INTO memory_restore_previews
			(namespace_uid, backend_uid, authority_epoch, routing_epoch, tenant_id, store_name, store_uuid,
			 preview_digest, created_at, expires_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(namespace_uid) DO UPDATE SET
			 backend_uid = excluded.backend_uid, authority_epoch = excluded.authority_epoch,
			 routing_epoch = excluded.routing_epoch, tenant_id = excluded.tenant_id,
			 store_name = excluded.store_name, store_uuid = excluded.store_uuid,
			 preview_digest = excluded.preview_digest, created_at = excluded.created_at, expires_at = excluded.expires_at`,
			preview.NamespaceUID, preview.BackendUID, preview.AuthorityEpoch, preview.RoutingEpoch,
			preview.TenantID, preview.StoreName, preview.StoreUUID, preview.PreviewDigest, now, expiresAt)
		if err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return preview, nil
}

// RestoreLegacyMemories atomically removes the fence, restores archived rows, and returns routing to legacy.
//
//nolint:gocyclo // Restore is one atomic identity, fence, archive, and audit transaction.
func (s *Store) RestoreLegacyMemories(ctx context.Context, restore store.LegacyMemoryRestore) (*store.LegacyMemoryRestoreResult, error) {
	restore.Now = normalizeMemoryNow(restore.Now)
	restore.NamespaceUID = strings.TrimSpace(restore.NamespaceUID)
	restore.BackendUID = strings.TrimSpace(restore.BackendUID)
	restore.ExpectedTenantID = strings.TrimSpace(restore.ExpectedTenantID)
	restore.ExpectedStoreName = strings.TrimSpace(restore.ExpectedStoreName)
	restore.ExpectedStoreUUID = strings.TrimSpace(restore.ExpectedStoreUUID)
	restore.PreviewDigest = strings.TrimSpace(restore.PreviewDigest)
	if restore.NamespaceUID == "" || restore.BackendUID == "" || strings.TrimSpace(restore.Actor) == "" || strings.TrimSpace(restore.Reason) == "" {
		return nil, store.ValidationErrorf("namespace uid, backend uid, actor, and reason are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	preview, err := previewLegacyMemoryRestoreQuery(ctx, tx, restore.NamespaceUID, restore.BackendUID)
	if err != nil {
		return nil, err
	}
	if !preview.Restorable {
		return nil, fmt.Errorf("%w: legacy restore blocked: %s", store.ErrConflict, preview.Reason)
	}
	binding, err := getMemoryBackendBindingQuery(ctx, tx, restore.NamespaceUID)
	if err != nil {
		return nil, err
	}
	var issued struct {
		backendUID, tenantID, storeName, storeUUID, digest string
		authorityEpoch, routingEpoch                       int64
		expiresAt                                          time.Time
	}
	if err := tx.QueryRowContext(ctx, `SELECT backend_uid, authority_epoch, routing_epoch, tenant_id,
		store_name, store_uuid, preview_digest, expires_at FROM memory_restore_previews WHERE namespace_uid = ?`,
		restore.NamespaceUID).Scan(&issued.backendUID, &issued.authorityEpoch, &issued.routingEpoch, &issued.tenantID,
		&issued.storeName, &issued.storeUUID, &issued.digest, &issued.expiresAt); errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: a fresh legacy restore preview is required", store.ErrConflict)
	} else if err != nil {
		return nil, err
	}
	currentDigest := legacyRestorePreviewDigest(preview)
	if !issued.expiresAt.After(time.Now().UTC()) || issued.backendUID != binding.BackendUID ||
		issued.authorityEpoch != binding.AuthorityEpoch || issued.routingEpoch != binding.RoutingEpoch ||
		issued.tenantID != binding.TenantID || issued.storeName != binding.StoreName || issued.storeUUID != binding.StoreUUID ||
		issued.digest != currentDigest || (restore.PreviewDigest != "" && restore.PreviewDigest != issued.digest) ||
		(restore.ExpectedAuthorityEpoch > 0 && restore.ExpectedAuthorityEpoch != binding.AuthorityEpoch) ||
		(restore.ExpectedRoutingEpoch > 0 && restore.ExpectedRoutingEpoch != binding.RoutingEpoch) ||
		(restore.ExpectedTenantID != "" && restore.ExpectedTenantID != binding.TenantID) ||
		(restore.ExpectedStoreName != "" && restore.ExpectedStoreName != binding.StoreName) ||
		(restore.ExpectedStoreUUID != "" && restore.ExpectedStoreUUID != binding.StoreUUID) {
		return nil, fmt.Errorf("%w: legacy restore preview identity is stale", store.ErrConflict)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_legacy_fences WHERE namespace_uid = ? AND backend_uid = ?`, restore.NamespaceUID, restore.BackendUID); err != nil {
		return nil, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO memories
		(id, namespace, session_name, agent_name, task_name, parent_task, source, source_proposal_id, content, tags_json,
		 disabled, deleted, created_at, updated_at, last_recalled_at, recalled_count)
		SELECT id, namespace_name, session_name, agent_name, task_name, parent_task, source, source_proposal_id, content, tags_json,
		 disabled, deleted, created_at, updated_at, last_recalled_at, recalled_count
		FROM legacy_memory_archive WHERE namespace_uid = ?`, restore.NamespaceUID)
	if err != nil {
		return nil, err
	}
	restored, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM legacy_memory_archive WHERE namespace_uid = ?`, restore.NamespaceUID); err != nil {
		return nil, err
	}
	newRoutingEpoch := binding.RoutingEpoch + 1
	result, err = tx.ExecContext(ctx, `UPDATE memory_backend_bindings
		SET mode = ?, state = ?, routing_epoch = ?, updated_at = ?
		WHERE namespace_uid = ? AND backend_uid = ? AND state = ? AND routing_epoch = ?`,
		store.MemoryBackendModeLegacy, store.MemoryBackendBindingLegacy, newRoutingEpoch, restore.Now, restore.NamespaceUID, restore.BackendUID,
		store.MemoryBackendBindingDecommissioned, binding.RoutingEpoch)
	if err != nil {
		return nil, err
	}
	if err := ensureMemoryRowsAffectedConflict(result, "memory backend binding changed during restore"); err != nil {
		return nil, err
	}
	if err := insertMemoryAudit(ctx, tx, store.MemoryAuditRecord{
		Namespace: binding.Namespace, NamespaceUID: binding.NamespaceUID, Actor: restore.Actor,
		Action: "backend.restore_legacy", Reason: restore.Reason, PreviousState: string(binding.State),
		NewState: string(store.MemoryBackendBindingLegacy), AuthorityEpoch: binding.AuthorityEpoch,
		PreviousRoutingEpoch: binding.RoutingEpoch, RoutingEpoch: newRoutingEpoch,
		RequestID: restore.RequestID, CreatedAt: restore.Now,
	}); err != nil {
		return nil, err
	}
	updated, err := getMemoryBackendBindingQuery(ctx, tx, restore.NamespaceUID)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_restore_previews WHERE namespace_uid = ? AND preview_digest = ?`,
		restore.NamespaceUID, issued.digest); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &store.LegacyMemoryRestoreResult{Binding: *updated, RestoredMemories: int(restored)}, nil
}

func selectMemoryBackendBindingSQL() string {
	return `SELECT namespace_name, namespace_uid, cluster_id, mode, backend_uid, backend_generation,
		authority_epoch, routing_epoch, spec_digest, endpoint_digest, resolved_address_digest, server_certificate_digest,
		secret_name, secret_key, secret_uid, secret_resource_version, tenant_id,
		store_name, store_uuid, ownership_claim, capability_revision, protocol, state, activation_epoch,
		minimum_feature_epoch, validation_expires_at, activated_at, updated_at, decommissioned_at
		FROM memory_backend_bindings`
}

func getMemoryBackendBindingQuery(ctx context.Context, q rowQueryer, namespaceUID string) (*store.MemoryBackendBinding, error) {
	return scanMemoryBackendBinding(q.QueryRowContext(ctx, selectMemoryBackendBindingSQL()+` WHERE namespace_uid = ?`, namespaceUID))
}

func getMemoryBackendBindingByNamespaceQuery(ctx context.Context, q rowQueryer, namespace string) (*store.MemoryBackendBinding, error) {
	return scanMemoryBackendBinding(q.QueryRowContext(ctx, selectMemoryBackendBindingSQL()+` WHERE namespace_name = ?`, namespace))
}

func rotateTerminalMemoryNamespaceIdentity(
	ctx context.Context,
	tx *sql.Tx,
	previous *store.MemoryBackendBinding,
	newNamespaceUID string,
	activation store.MemoryBackendActivation,
) error {
	if previous == nil || previous.NamespaceUID == newNamespaceUID {
		return fmt.Errorf("%w: namespace name is already bound to the requested namespace uid", store.ErrConflict)
	}
	if previous.Mode != store.MemoryBackendModeRemote || previous.State != store.MemoryBackendBindingRemoved {
		return fmt.Errorf("%w: namespace name reuse requires the prior namespace uid %q to be removed", store.ErrConflict, previous.NamespaceUID)
	}
	historicalNamespace := historicalMemoryNamespaceIdentity(previous.Namespace, previous.NamespaceUID)
	result, err := tx.ExecContext(ctx, `UPDATE memory_backend_bindings SET namespace_name = ?, updated_at = ?
		WHERE namespace_uid = ? AND namespace_name = ? AND backend_uid = ? AND authority_epoch = ? AND routing_epoch = ?
			AND mode = ? AND state = ?`, historicalNamespace, activation.Now,
		previous.NamespaceUID, previous.Namespace, previous.BackendUID, previous.AuthorityEpoch, previous.RoutingEpoch,
		store.MemoryBackendModeRemote, store.MemoryBackendBindingRemoved)
	if err != nil {
		return err
	}
	if err := ensureMemoryRowsAffectedConflict(result, "terminal namespace binding changed before namespace name reuse"); err != nil {
		return err
	}
	result, err = tx.ExecContext(ctx, `UPDATE memory_legacy_fences SET namespace_name = ?
		WHERE namespace_uid = ? AND namespace_name = ? AND backend_uid = ? AND authority_epoch = ? AND routing_epoch = ?`,
		historicalNamespace, previous.NamespaceUID, previous.Namespace, previous.BackendUID,
		previous.AuthorityEpoch, previous.RoutingEpoch)
	if err != nil {
		return err
	}
	if err := ensureMemoryRowsAffectedConflict(result, "terminal namespace fence changed before namespace name reuse"); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memory_proposals SET namespace = ?, updated_at = ?
		WHERE namespace = ? AND created_at <= ?`, historicalNamespace, activation.Now, previous.Namespace, previous.UpdatedAt); err != nil {
		return err
	}
	return insertMemoryAudit(ctx, tx, store.MemoryAuditRecord{
		Namespace: previous.Namespace, NamespaceUID: previous.NamespaceUID, Actor: activation.Actor,
		Action: "backend.namespace_identity.rotate", Reason: activation.Reason,
		PreviousState: string(previous.State), NewState: string(previous.State),
		AuthorityEpoch: previous.AuthorityEpoch, PreviousRoutingEpoch: previous.RoutingEpoch,
		RoutingEpoch: previous.RoutingEpoch, RequestID: activation.RequestID, CreatedAt: activation.Now,
	})
}

func historicalMemoryNamespaceIdentity(namespace, namespaceUID string) string {
	digest := sha256.Sum256([]byte(namespaceUID))
	return "retired:" + namespace + ":" + hex.EncodeToString(digest[:])
}

func scanMemoryBackendBinding(scanner rowScanner) (*store.MemoryBackendBinding, error) {
	var binding store.MemoryBackendBinding
	var decommissionedAt sql.NullTime
	err := scanner.Scan(
		&binding.Namespace, &binding.NamespaceUID, &binding.ClusterID, &binding.Mode, &binding.BackendUID,
		&binding.BackendGeneration, &binding.AuthorityEpoch, &binding.RoutingEpoch, &binding.SpecDigest,
		&binding.EndpointDigest, &binding.ResolvedAddressDigest, &binding.ServerCertificateDigest,
		&binding.SecretName, &binding.SecretKey, &binding.SecretUID, &binding.SecretResourceVersion, &binding.TenantID,
		&binding.StoreName, &binding.StoreUUID, &binding.OwnershipClaim, &binding.CapabilityRevision,
		&binding.Protocol, &binding.State, &binding.ActivationEpoch, &binding.MinimumFeatureEpoch,
		&binding.ValidationExpiresAt, &binding.ActivatedAt, &binding.UpdatedAt, &decommissionedAt,
	)
	if err != nil {
		return nil, err
	}
	if decommissionedAt.Valid {
		binding.DecommissionedAt = &decommissionedAt.Time
	}
	return &binding, nil
}

type rowQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func sameMemoryBindingIdentity(left, right *store.MemoryBackendBinding) bool {
	return left != nil && right != nil &&
		left.Namespace == right.Namespace && left.NamespaceUID == right.NamespaceUID && left.ClusterID == right.ClusterID &&
		left.Mode == right.Mode && left.BackendUID == right.BackendUID && left.BackendGeneration == right.BackendGeneration &&
		left.AuthorityEpoch == right.AuthorityEpoch && left.RoutingEpoch == right.RoutingEpoch && left.SpecDigest == right.SpecDigest &&
		left.EndpointDigest == right.EndpointDigest && left.ResolvedAddressDigest == right.ResolvedAddressDigest &&
		left.ServerCertificateDigest == right.ServerCertificateDigest && left.SecretName == right.SecretName && left.SecretKey == right.SecretKey &&
		left.SecretUID == right.SecretUID && left.SecretResourceVersion == right.SecretResourceVersion && left.TenantID == right.TenantID &&
		left.StoreName == right.StoreName && left.StoreUUID == right.StoreUUID && left.OwnershipClaim == right.OwnershipClaim &&
		left.CapabilityRevision == right.CapabilityRevision && left.Protocol == right.Protocol &&
		left.ActivationEpoch == right.ActivationEpoch && left.MinimumFeatureEpoch == right.MinimumFeatureEpoch &&
		left.ValidationExpiresAt.Equal(right.ValidationExpiresAt)
}

func sameMemoryRefreshAuthorityIdentity(current, desired *store.MemoryBackendBinding) bool {
	return current != nil && desired != nil && current.Namespace == desired.Namespace &&
		current.NamespaceUID == desired.NamespaceUID && current.ClusterID == desired.ClusterID &&
		current.Mode == desired.Mode && current.BackendUID == desired.BackendUID &&
		current.AuthorityEpoch == desired.AuthorityEpoch && current.TenantID == desired.TenantID &&
		current.StoreName == desired.StoreName && current.StoreUUID == desired.StoreUUID &&
		current.ActivationEpoch == desired.ActivationEpoch &&
		current.MinimumFeatureEpoch == desired.MinimumFeatureEpoch && current.ActivatedAt.Equal(desired.ActivatedAt)
}

func sameMemoryRefreshRouteIdentity(current, desired *store.MemoryBackendBinding) bool {
	// BackendGeneration records which CR generation authorized the currently
	// validated route. Advancing only that generation while retaining every
	// route-sensitive field is drain-compatible and does not require a routing
	// fence.
	return current != nil && desired != nil &&
		current.SpecDigest == desired.SpecDigest && current.EndpointDigest == desired.EndpointDigest &&
		current.ResolvedAddressDigest == desired.ResolvedAddressDigest &&
		current.ServerCertificateDigest == desired.ServerCertificateDigest &&
		current.SecretName == desired.SecretName && current.SecretKey == desired.SecretKey &&
		current.SecretUID == desired.SecretUID && current.SecretResourceVersion == desired.SecretResourceVersion &&
		current.OwnershipClaim == desired.OwnershipClaim && current.CapabilityRevision == desired.CapabilityRevision &&
		current.Protocol == desired.Protocol
}

func allowedMemoryBindingTransition(from, to store.MemoryBackendBindingState) bool {
	switch from {
	case store.MemoryBackendBindingAccepting:
		return to == store.MemoryBackendBindingDraining || to == store.MemoryBackendBindingRecovering || to == store.MemoryBackendBindingDecommissioned
	case store.MemoryBackendBindingDraining:
		return to == store.MemoryBackendBindingRecovering || to == store.MemoryBackendBindingDecommissioned
	case store.MemoryBackendBindingRecovering:
		return to == store.MemoryBackendBindingAccepting || to == store.MemoryBackendBindingDecommissioned || to == store.MemoryBackendBindingRemoved
	case store.MemoryBackendBindingDecommissioned:
		return to == store.MemoryBackendBindingRemoved
	case store.MemoryBackendBindingValidating:
		return to == store.MemoryBackendBindingAccepting || to == store.MemoryBackendBindingRemoved
	default:
		return false
	}
}

func previewLegacyMemoryRestoreQuery(ctx context.Context, q rowQueryer, namespaceUID, backendUID string) (*store.LegacyMemoryRestorePreview, error) {
	binding, err := getMemoryBackendBindingQuery(ctx, q, namespaceUID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	preview := &store.LegacyMemoryRestorePreview{
		Namespace: binding.Namespace, NamespaceUID: namespaceUID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		TenantID: binding.TenantID, StoreName: binding.StoreName, StoreUUID: binding.StoreUUID,
	}
	if binding.BackendUID != backendUID {
		preview.Reason = "backend identity does not match"
		return preview, nil
	}
	if binding.State != store.MemoryBackendBindingDecommissioned {
		preview.Reason = "binding is not decommissioned"
	}
	if preview.Reason == "" {
		var forceOrphaned int
		if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_force_orphan_fences WHERE namespace_uid = ?`, namespaceUID).
			Scan(&forceOrphaned); err != nil {
			return nil, err
		}
		if forceOrphaned > 0 {
			preview.Reason = "force-orphan anti-resurrection fence is present"
		}
	}
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM legacy_memory_archive WHERE namespace_uid = ?`, namespaceUID).Scan(&preview.ArchivedMemories); err != nil {
		return nil, err
	}
	var oldest time.Time
	if err := q.QueryRowContext(ctx, `SELECT created_at FROM legacy_memory_archive WHERE namespace_uid = ? ORDER BY created_at ASC LIMIT 1`, namespaceUID).Scan(&oldest); err == nil {
		value := oldest.UTC()
		preview.OldestCreatedAt = &value
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	var newest time.Time
	if err := q.QueryRowContext(ctx, `SELECT created_at FROM legacy_memory_archive WHERE namespace_uid = ? ORDER BY created_at DESC LIMIT 1`, namespaceUID).Scan(&newest); err == nil {
		value := newest.UTC()
		preview.NewestCreatedAt = &value
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM legacy_memory_archive a
		JOIN memories m ON m.namespace = a.namespace_name AND m.id = a.id WHERE a.namespace_uid = ?`, namespaceUID).
		Scan(&preview.ConflictingMemories); err != nil {
		return nil, err
	}
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_operations
		WHERE namespace_uid = ? AND state IN ('queued','leased','dispatching','ambiguous','dead_lettered')`, namespaceUID).
		Scan(&preview.UnresolvedOperations); err != nil {
		return nil, err
	}
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM remote_memory_catalog
		WHERE namespace_uid = ? AND materialization_state IN ('pending','active','diverged','lost')`, namespaceUID).
		Scan(&preview.BlockingCatalogRows); err != nil {
		return nil, err
	}
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM legacy_memory_archive a
		JOIN remote_memory_catalog c ON c.namespace_uid = a.namespace_uid AND c.id = a.id
		WHERE a.namespace_uid = ? AND a.deleted = FALSE
			AND (c.deleted = TRUE OR c.materialization_state = 'deleted')`, namespaceUID).
		Scan(&preview.RemoteDeletedMemories); err != nil {
		return nil, err
	}
	if preview.Reason == "" && preview.ConflictingMemories > 0 {
		preview.Reason = "legacy memory ids conflict with current rows"
	}
	if preview.Reason == "" && preview.UnresolvedOperations > 0 {
		preview.Reason = "unresolved remote memory operations remain"
	}
	if preview.Reason == "" && preview.BlockingCatalogRows > 0 {
		preview.Reason = "remote catalog still contains authoritative materializations"
	}
	if preview.Reason == "" && preview.RemoteDeletedMemories > 0 {
		preview.Reason = "remote deletions would resurrect archived memory ids"
	}
	if preview.Reason == "" {
		var fences int
		if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_legacy_fences
			WHERE namespace_uid = ? AND backend_uid = ?`, namespaceUID, backendUID).Scan(&fences); err != nil {
			return nil, err
		}
		if fences != 1 {
			preview.Reason = "legacy write fence is missing or mismatched"
		}
	}
	preview.Restorable = preview.Reason == ""
	return preview, nil
}

func legacyRestorePreviewDigest(preview *store.LegacyMemoryRestorePreview) string {
	if preview == nil {
		return ""
	}
	preimage := struct {
		Namespace, NamespaceUID, BackendUID, TenantID, StoreName, StoreUUID string
		AuthorityEpoch, RoutingEpoch                                        int64
		ArchivedMemories, ConflictingMemories, RemoteDeletedMemories        int
		UnresolvedOperations, BlockingCatalogRows                           int
		OldestCreatedAt, NewestCreatedAt                                    *time.Time
	}{
		Namespace: preview.Namespace, NamespaceUID: preview.NamespaceUID, BackendUID: preview.BackendUID,
		TenantID: preview.TenantID, StoreName: preview.StoreName, StoreUUID: preview.StoreUUID,
		AuthorityEpoch: preview.AuthorityEpoch, RoutingEpoch: preview.RoutingEpoch,
		ArchivedMemories: preview.ArchivedMemories, ConflictingMemories: preview.ConflictingMemories,
		RemoteDeletedMemories: preview.RemoteDeletedMemories, UnresolvedOperations: preview.UnresolvedOperations,
		BlockingCatalogRows: preview.BlockingCatalogRows, OldestCreatedAt: preview.OldestCreatedAt,
		NewestCreatedAt: preview.NewestCreatedAt,
	}
	encoded, _ := json.Marshal(preimage)
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func insertMemoryAudit(ctx context.Context, tx *sql.Tx, audit store.MemoryAuditRecord) error {
	if audit.ID == "" {
		audit.ID = newMemoryAuditID()
	}
	audit.CreatedAt = normalizeMemoryNow(audit.CreatedAt)
	audit.Actor = boundedMemoryAuditText(audit.Actor)
	audit.Action = boundedMemoryAuditText(audit.Action)
	audit.Reason = boundedMemoryAuditText(audit.Reason)
	audit.PreviousState = boundedMemoryAuditText(audit.PreviousState)
	audit.NewState = boundedMemoryAuditText(audit.NewState)
	audit.RequestID = boundedMemoryAuditText(audit.RequestID)
	allowReserved := audit.Action == "memory.checkpoint.verify" || audit.Action == "memory.governance.purge"
	if err := enforceMemoryAuditHardQuota(ctx, tx, audit.NamespaceUID, memoryAuditRecordBytes(audit), allowReserved); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO memory_audit
		(id, namespace_name, namespace_uid, actor, action, reason, previous_state, new_state, authority_epoch,
		 previous_routing_epoch, routing_epoch, memory_id, operation_id, proposal_id, request_digest,
		 mutation_digest, content_digest, request_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		audit.ID, audit.Namespace, audit.NamespaceUID, audit.Actor, audit.Action, audit.Reason,
		audit.PreviousState, audit.NewState, audit.AuthorityEpoch, audit.PreviousRoutingEpoch, audit.RoutingEpoch,
		audit.MemoryID, audit.OperationID, audit.ProposalID, audit.RequestDigest, audit.MutationDigest,
		audit.ContentDigest, audit.RequestID, audit.CreatedAt)
	return err
}

func memoryAuditRecordBytes(audit store.MemoryAuditRecord) int64 {
	return int64(len(audit.ID) + len(audit.Namespace) + len(audit.NamespaceUID) + len(audit.Actor) + len(audit.Action) +
		len(audit.Reason) + len(audit.PreviousState) + len(audit.NewState) + len(audit.MemoryID) + len(audit.OperationID) +
		len(audit.ProposalID) + len(audit.RequestDigest) + len(audit.MutationDigest) + len(audit.ContentDigest) + len(audit.RequestID) + 128)
}

func enforceMemoryAuditHardQuota(
	ctx context.Context,
	q rowQueryer,
	namespaceUID string,
	addingBytes int64,
	allowReserved bool,
) error {
	limits := governedMemoryQuotas
	if allowReserved {
		limits.NamespaceAuditRows += 16
		limits.GlobalAuditRows += 1024
		limits.NamespaceAuditBytes += 64 << 10
		limits.GlobalAuditBytes += 4 << 20
	}
	var rows, size int64
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(length(id) + length(namespace_name) + length(namespace_uid) +
		length(actor) + length(action) + length(reason) + length(previous_state) + length(new_state) +
		length(memory_id) + length(operation_id) + length(proposal_id) + length(request_digest) +
		length(mutation_digest) + length(content_digest) + length(request_id) + 128), 0)
		FROM memory_audit WHERE namespace_uid = ?`, namespaceUID).Scan(&rows, &size); err != nil {
		return err
	}
	if (limits.NamespaceAuditRows > 0 && rows >= limits.NamespaceAuditRows) ||
		(limits.NamespaceAuditBytes > 0 && size+addingBytes > limits.NamespaceAuditBytes) {
		return fmt.Errorf("%w: namespace memory audit hard quota reached", store.ErrCapacity)
	}
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(length(id) + length(namespace_name) + length(namespace_uid) +
		length(actor) + length(action) + length(reason) + length(previous_state) + length(new_state) +
		length(memory_id) + length(operation_id) + length(proposal_id) + length(request_digest) +
		length(mutation_digest) + length(content_digest) + length(request_id) + 128), 0)
		FROM memory_audit`).Scan(&rows, &size); err != nil {
		return err
	}
	if (limits.GlobalAuditRows > 0 && rows >= limits.GlobalAuditRows) ||
		(limits.GlobalAuditBytes > 0 && size+addingBytes > limits.GlobalAuditBytes) {
		return fmt.Errorf("%w: global memory audit hard quota reached", store.ErrCapacity)
	}
	return nil
}

// AdmitRemoteMemoryCreate atomically admits a generation-one remote memory creation.
func (s *Store) AdmitRemoteMemoryCreate(ctx context.Context, admission store.RemoteMemoryCreateAdmission) (*store.MemoryMutationAdmissionResult, error) {
	admission.Mutation.ProposalID = ""
	admission.Memory.SourceProposalID = ""
	return s.admitRemoteMemoryMutation(ctx, store.MemoryOperationCreate, admission.Mutation, admission.Memory, 0, "", "")
}

// AdmitRemoteMemoryReplace atomically admits a full-content replacement.
func (s *Store) AdmitRemoteMemoryReplace(ctx context.Context, admission store.RemoteMemoryReplaceAdmission) (*store.MemoryMutationAdmissionResult, error) {
	admission.Mutation.ProposalID = ""
	admission.Memory.SourceProposalID = ""
	return s.admitRemoteMemoryMutation(ctx, store.MemoryOperationReplace, admission.Mutation, admission.Memory,
		admission.ExpectedGeneration, admission.ExpectedBackendVersion, "")
}

// AdmitRemoteMemoryDelete atomically installs a higher-generation tombstone and supersedes unresolved content work.
func (s *Store) AdmitRemoteMemoryDelete(ctx context.Context, admission store.RemoteMemoryDeleteAdmission) (*store.MemoryMutationAdmissionResult, error) {
	admission.Mutation.ProposalID = ""
	return s.admitRemoteMemoryMutation(ctx, store.MemoryOperationDelete, admission.Mutation, store.RemoteMemoryCatalogEntry{},
		admission.ExpectedGeneration, admission.ExpectedBackendVersion, "")
}

// AdmitRemoteMemoryProposalApply atomically transitions accepted -> applying and admits the linked create.
func (s *Store) AdmitRemoteMemoryProposalApply(ctx context.Context, admission store.RemoteMemoryProposalApplyAdmission) (*store.MemoryMutationAdmissionResult, error) {
	admission.Mutation.ProposalID = strings.TrimSpace(admission.Mutation.ProposalID)
	if admission.Mutation.ProposalID == "" {
		admission.Mutation.ProposalID = strings.TrimSpace(admission.Memory.SourceProposalID)
	}
	if admission.Mutation.ProposalID == "" {
		return nil, store.ValidationErrorf("proposal id is required")
	}
	admission.Memory.SourceProposalID = admission.Mutation.ProposalID
	appliedBy := strings.TrimSpace(admission.AppliedBy)
	if appliedBy == "" {
		appliedBy = strings.TrimSpace(admission.Mutation.Actor)
	}
	admission.Memory.Source = memorySourceProposal
	admission.Memory.Trust = store.MemoryTrustReviewed
	return s.admitRemoteMemoryMutation(ctx, store.MemoryOperationCreate, admission.Mutation, admission.Memory, 0, "", appliedBy)
}

func (s *Store) admitRemoteMemoryMutation(
	ctx context.Context,
	kind store.MemoryOperationKind,
	mutation store.MemoryMutationAdmission,
	desired store.RemoteMemoryCatalogEntry,
	expectedGeneration int64,
	expectedBackendVersion string,
	proposalAppliedBy string,
) (*store.MemoryMutationAdmissionResult, error) {
	proposalAppliedBy = strings.TrimSpace(proposalAppliedBy)
	if proposalAppliedBy != "" && kind != store.MemoryOperationCreate {
		return nil, store.ValidationErrorf("proposal application is only valid for create operations")
	}
	if proposalAppliedBy == "" {
		mutation.ProposalID = ""
		desired.SourceProposalID = ""
	}
	if err := normalizeAndValidateMemoryMutation(&mutation, kind); err != nil {
		return nil, err
	}
	if len(mutation.Payload) == 0 {
		return nil, store.ValidationErrorf("canonical operation payload is required")
	}

	const maxAttempts = 6
	retryBackoffs := [...]time.Duration{
		5 * time.Millisecond,
		10 * time.Millisecond,
		25 * time.Millisecond,
		50 * time.Millisecond,
		100 * time.Millisecond,
	}
	var lastErr error
	for attempt := range maxAttempts {
		result, err := s.admitRemoteMemoryMutationOnce(
			ctx,
			kind,
			mutation,
			desired,
			expectedGeneration,
			expectedBackendVersion,
			proposalAppliedBy,
		)
		if err == nil {
			return result, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !isSQLiteRetryableError(err) {
			return nil, err
		}
		lastErr = err
		if attempt == maxAttempts-1 {
			break
		}
		timer := time.NewTimer(retryBackoffs[attempt])
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, lastErr
}

func (s *Store) admitRemoteMemoryMutationOnce(
	ctx context.Context,
	kind store.MemoryOperationKind,
	mutation store.MemoryMutationAdmission,
	desired store.RemoteMemoryCatalogEntry,
	expectedGeneration int64,
	expectedBackendVersion string,
	proposalAppliedBy string,
) (*store.MemoryMutationAdmissionResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	binding, err := requireMemoryBindingForMutation(ctx, tx, mutation)
	if err != nil {
		return nil, err
	}

	idempotency := memoryIdempotencyFromMutation(mutation)
	idempotency.MemoryID = mutation.MemoryID
	idempotency.OperationID = mutation.OperationID
	reserved, replayed, err := reserveMemoryIdempotency(ctx, tx, idempotency)
	if err != nil {
		return nil, err
	}
	if !reserved {
		if replayed.RequestDigest != mutation.RequestDigest {
			return nil, fmt.Errorf("%w: memory idempotency key was reused with a different request", store.ErrDuplicateMismatch)
		}
		result, err := memoryAdmissionReplayResult(ctx, tx, replayed)
		if err != nil {
			return nil, err
		}
		result.Replayed = true
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return result, nil
	}

	if kind != store.MemoryOperationDelete {
		if err := validateRemoteMemoryMetadata(desired); err != nil {
			return nil, err
		}
	}
	var catalog *store.RemoteMemoryCatalogEntry
	var operation *store.MemoryOperation
	switch kind {
	case store.MemoryOperationCreate:
		catalog, operation, err = admitRemoteMemoryCreateTx(ctx, tx, mutation, desired, binding, proposalAppliedBy)
	case store.MemoryOperationReplace:
		catalog, operation, err = admitRemoteMemoryReplaceTx(ctx, tx, mutation, desired, binding, expectedGeneration, expectedBackendVersion)
	case store.MemoryOperationDelete:
		catalog, operation, err = admitRemoteMemoryDeleteTx(ctx, tx, mutation, binding, expectedGeneration, expectedBackendVersion)
	default:
		err = store.ValidationErrorf("unsupported memory operation kind %q", kind)
	}
	if err != nil {
		return nil, err
	}
	if catalog.ID != idempotency.MemoryID || operation.ID != idempotency.OperationID {
		return nil, fmt.Errorf("%w: admitted memory operation identity changed", store.ErrConflict)
	}
	responseSnapshot, err := encodeMemoryIdempotencySnapshot(catalog, operation, mutation.Payload)
	if err != nil {
		return nil, err
	}
	if err := saveMemoryIdempotencySnapshot(ctx, tx, &idempotency, responseSnapshot, mutation.Now); err != nil {
		return nil, err
	}
	if err := enforceMemoryGovernanceAdmissionCapacity(ctx, tx, mutation.NamespaceUID, kind); err != nil {
		return nil, err
	}
	if err := insertMemoryAudit(ctx, tx, store.MemoryAuditRecord{
		Namespace: mutation.Namespace, NamespaceUID: mutation.NamespaceUID, Actor: mutation.Actor,
		Action: "memory." + string(kind) + ".admit", Reason: mutation.Reason,
		NewState: string(catalog.MaterializationState), AuthorityEpoch: mutation.AuthorityEpoch,
		RoutingEpoch: mutation.RoutingEpoch, MemoryID: catalog.ID, OperationID: operation.ID,
		ProposalID: mutation.ProposalID, RequestDigest: mutation.RequestDigest, MutationDigest: mutation.MutationDigest,
		ContentDigest: mutation.ContentDigest, RequestID: mutation.RequestID, CreatedAt: mutation.Now,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &store.MemoryMutationAdmissionResult{Memory: *catalog, Operation: *operation, Idempotency: idempotency}, nil
}

func normalizeAndValidateMemoryMutation(mutation *store.MemoryMutationAdmission, kind store.MemoryOperationKind) error {
	if mutation == nil || !validMemoryOperationKind(kind) {
		return store.ValidationErrorf("valid memory mutation is required")
	}
	mutation.Namespace = strings.TrimSpace(mutation.Namespace)
	mutation.NamespaceUID = strings.TrimSpace(mutation.NamespaceUID)
	mutation.ClusterID = strings.TrimSpace(mutation.ClusterID)
	mutation.BackendUID = strings.TrimSpace(mutation.BackendUID)
	mutation.MemoryID = strings.TrimSpace(mutation.MemoryID)
	mutation.OperationID = strings.TrimSpace(mutation.OperationID)
	mutation.ProposalID = strings.TrimSpace(mutation.ProposalID)
	mutation.Principal = strings.TrimSpace(mutation.Principal)
	mutation.Route = strings.TrimSpace(mutation.Route)
	mutation.IdempotencyKey = strings.TrimSpace(mutation.IdempotencyKey)
	mutation.RequestDigest = strings.TrimSpace(mutation.RequestDigest)
	mutation.OperationIdempotencyKey = strings.TrimSpace(mutation.OperationIdempotencyKey)
	mutation.MutationDigest = strings.TrimSpace(mutation.MutationDigest)
	mutation.ContentDigest = strings.TrimSpace(mutation.ContentDigest)
	mutation.Actor = strings.TrimSpace(mutation.Actor)
	if mutation.Namespace == "" || mutation.NamespaceUID == "" || mutation.ClusterID == "" || mutation.BackendUID == "" {
		return store.ValidationErrorf("complete namespace and backend identity is required")
	}
	if mutation.AuthorityEpoch <= 0 || mutation.RoutingEpoch <= 0 {
		return store.ValidationErrorf("authority and routing epochs must be positive")
	}
	if mutation.MemoryID == "" || mutation.OperationID == "" {
		return store.ValidationErrorf("preallocated memory and operation ids are required")
	}
	if mutation.OperationIdempotencyKey == "" {
		mutation.OperationIdempotencyKey = mutation.OperationID
	}
	if mutation.Principal == "" || mutation.Route == "" || mutation.IdempotencyKey == "" || mutation.RequestDigest == "" {
		return store.ValidationErrorf("principal, route, idempotency key, and request digest are required")
	}
	if mutation.MutationDigest == "" || mutation.Actor == "" {
		return store.ValidationErrorf("mutation digest and actor are required")
	}
	mutation.Now = normalizeMemoryNow(mutation.Now)
	if mutation.MaxAgeAt.IsZero() {
		mutation.MaxAgeAt = mutation.Now.Add(defaultOperationRecoveryWindow)
	} else {
		mutation.MaxAgeAt = mutation.MaxAgeAt.UTC()
	}
	if !mutation.MaxAgeAt.After(mutation.Now) {
		return store.ValidationErrorf("operation maximum age must be in the future")
	}
	if mutation.MaxAgeAt.Sub(mutation.Now) > 30*24*time.Hour {
		return store.ValidationErrorf("operation maximum age exceeds 30 day hard cap")
	}
	if mutation.IdempotencyExpiresAt.IsZero() {
		mutation.IdempotencyExpiresAt = mutation.Now.Add(30 * 24 * time.Hour)
	} else {
		mutation.IdempotencyExpiresAt = mutation.IdempotencyExpiresAt.UTC()
	}
	if !mutation.IdempotencyExpiresAt.After(mutation.Now) {
		return store.ValidationErrorf("idempotency expiry must be in the future")
	}
	if mutation.IdempotencyExpiresAt.Before(mutation.MaxAgeAt) {
		mutation.IdempotencyExpiresAt = mutation.MaxAgeAt
	}
	if mutation.OriginalStatus == 0 {
		mutation.OriginalStatus = 202
	}
	if mutation.ResponseType == "" {
		mutation.ResponseType = store.MemoryIdempotencyOperation
	}
	switch mutation.ResponseType {
	case store.MemoryIdempotencyMemory, store.MemoryIdempotencyOperation, store.MemoryIdempotencyEmpty:
	default:
		return store.ValidationErrorf("invalid idempotency response type %q", mutation.ResponseType)
	}
	mutation.Payload = append([]byte(nil), mutation.Payload...)
	return nil
}

func requireMemoryBindingForMutation(ctx context.Context, q rowQueryer, mutation store.MemoryMutationAdmission) (*store.MemoryBackendBinding, error) {
	binding, err := getMemoryBackendBindingQuery(ctx, q, mutation.NamespaceUID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if binding.Mode != store.MemoryBackendModeRemote || binding.State != store.MemoryBackendBindingAccepting ||
		binding.ResolvedAddressDigest == "" || binding.ServerCertificateDigest == "" ||
		!binding.ValidationExpiresAt.After(mutation.Now) || binding.Namespace != mutation.Namespace ||
		binding.ClusterID != mutation.ClusterID || binding.BackendUID != mutation.BackendUID ||
		binding.AuthorityEpoch != mutation.AuthorityEpoch || binding.RoutingEpoch != mutation.RoutingEpoch {
		return nil, fmt.Errorf("%w: memory backend binding is stale or not accepting this mutation", store.ErrConflict)
	}
	return binding, nil
}

func admitRemoteMemoryCreateTx(
	ctx context.Context,
	tx *sql.Tx,
	mutation store.MemoryMutationAdmission,
	desired store.RemoteMemoryCatalogEntry,
	binding *store.MemoryBackendBinding,
	proposalAppliedBy string,
) (*store.RemoteMemoryCatalogEntry, *store.MemoryOperation, error) {
	proposalApply := proposalAppliedBy != ""
	if _, err := getRemoteMemoryQuery(ctx, tx, mutation.NamespaceUID, mutation.MemoryID); err == nil {
		return nil, nil, fmt.Errorf("%w: memory id already exists", store.ErrConflict)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, nil, err
	}
	expectedGeneration, err := getRemoteMemoryGenerationWatermarkQuery(
		ctx, tx, mutation.NamespaceUID, mutation.MemoryID, binding.BackendUID, binding.AuthorityEpoch, binding.StoreUUID,
	)
	if err != nil {
		return nil, nil, err
	}
	if expectedGeneration == math.MaxInt64 {
		return nil, nil, fmt.Errorf("%w: memory generation watermark is exhausted", store.ErrConflict)
	}
	desiredGeneration := expectedGeneration + 1
	if err := validateAndCanonicalizeMemoryMutation(
		&mutation, store.MemoryOperationCreate, desiredGeneration, expectedGeneration, "", binding,
	); err != nil {
		return nil, nil, err
	}
	canonicalTags, err := canonicalMutationTags(mutation.Payload)
	if err != nil {
		return nil, nil, err
	}
	desired.Tags = canonicalTags
	if err := enforceMemoryOperationAdmissionCapacity(ctx, tx, mutation.NamespaceUID, mutation.Payload, store.MemoryOperationCreate); err != nil {
		return nil, nil, err
	}
	if proposalApply {
		proposal, err := scanMemoryProposal(tx.QueryRowContext(ctx, selectMemoryProposalSQL()+` WHERE namespace = ? AND id = ?`, mutation.Namespace, mutation.ProposalID))
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, store.ErrNotFound
		}
		if err != nil {
			return nil, nil, err
		}
		if normalizeProposalStatus(proposal.Status) != proposalStatusAccepted || proposal.AppliedMemoryID != "" || proposal.ApplyOperationID != "" {
			return nil, nil, fmt.Errorf("%w: proposal is no longer accepted for application", store.ErrConflict)
		}
		if strings.ToLower(strings.TrimSpace(proposal.Type)) != proposalTypeMemory {
			return nil, nil, store.ValidationErrorf("proposal type %q cannot be applied as memory", proposal.Type)
		}
		if proposal.ReviewedAt == nil {
			return nil, nil, store.ValidationErrorf("proposal is missing an authoritative review")
		}
		if err := validateRemoteProposalApplyMutation(
			proposal, mutation, &desired, binding, desiredGeneration, expectedGeneration,
		); err != nil {
			return nil, nil, err
		}
		result, err := tx.ExecContext(ctx, `UPDATE memory_proposals
			SET status = ?, apply_operation_id = ?, applied_by = ?, updated_at = ?
			WHERE namespace = ? AND id = ? AND status = ? AND applied_memory_id = '' AND apply_operation_id = ''`,
			proposalStatusApplying, mutation.OperationID, boundedMemoryAuditText(proposalAppliedBy), mutation.Now,
			mutation.Namespace, mutation.ProposalID, proposalStatusAccepted)
		if err != nil {
			return nil, nil, err
		}
		if err := ensureMemoryRowsAffectedConflict(result, "proposal changed during remote apply admission"); err != nil {
			return nil, nil, err
		}
	}
	catalog := normalizeNewRemoteCatalog(
		desired, mutation, binding, proposalApply, expectedGeneration, desiredGeneration,
	)
	if err := insertRemoteMemoryCatalog(ctx, tx, &catalog); err != nil {
		if isSQLiteConstraintError(err) {
			return nil, nil, fmt.Errorf("%w: remote memory catalog identity already exists", store.ErrConflict)
		}
		return nil, nil, err
	}
	operation := newMemoryOperation(mutation, store.MemoryOperationCreate, desiredGeneration, expectedGeneration, "")
	if err := insertMemoryOperationWithPayload(ctx, tx, &operation, mutation.Payload); err != nil {
		return nil, nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM remote_memory_generation_watermarks
		WHERE namespace_uid = ? AND id = ?`, mutation.NamespaceUID, mutation.MemoryID); err != nil {
		return nil, nil, err
	}
	return &catalog, &operation, nil
}

func validateRemoteProposalApplyMutation(
	proposal *store.MemoryProposal,
	mutation store.MemoryMutationAdmission,
	desired *store.RemoteMemoryCatalogEntry,
	binding *store.MemoryBackendBinding,
	desiredGeneration, expectedGeneration int64,
) error {
	envelope, err := omsprotocol.DecodeMutationEnvelope(mutation.Payload)
	if err != nil {
		return store.ValidationErrorf("canonical proposal mutation payload is invalid: %v", err)
	}
	if envelope.OperationID != mutation.OperationID || envelope.MemoryID != mutation.MemoryID ||
		envelope.Kind != omsprotocol.MutationKindCreate || envelope.Generation != uint64(desiredGeneration) ||
		envelope.ExpectedGeneration != uint64(expectedGeneration) || envelope.ExpectedBackendVersion != "" ||
		envelope.MutationDigest != mutation.MutationDigest || envelope.ContentDigest != mutation.ContentDigest {
		return store.ValidationErrorf("canonical proposal mutation payload does not match the admitted mutation")
	}
	expectedBinding := omsprotocol.Binding{
		ClusterID: mutation.ClusterID, NamespaceUID: mutation.NamespaceUID, BackendUID: mutation.BackendUID,
		AuthorityEpoch: uint64(mutation.AuthorityEpoch), RoutingEpoch: uint64(mutation.RoutingEpoch),
		TenantID: binding.TenantID, StoreUUID: binding.StoreUUID,
	}
	if envelope.Binding != expectedBinding {
		return store.ValidationErrorf("canonical proposal mutation payload does not match the durable binding")
	}
	if envelope.State == nil {
		return store.ValidationErrorf("canonical proposal mutation payload is missing content state")
	}

	expectedContent := redact.SensitiveText(proposal.Content)
	expectedContentDigest := omsprotocol.ContentDigest(expectedContent)
	if envelope.State.Content != expectedContent || envelope.ContentDigest != expectedContentDigest ||
		mutation.ContentDigest != expectedContentDigest || desired.ContentDigest != expectedContentDigest {
		return store.ValidationErrorf("proposal mutation content does not match the accepted reviewed proposal")
	}
	expectedTags, err := canonicalRemoteProposalTags(proposal.Description)
	if err != nil {
		return store.ValidationErrorf("accepted proposal tags are invalid: %v", err)
	}
	expectedMetadata, err := canonicalRemoteProposalMetadata(proposal)
	if err != nil {
		return store.ValidationErrorf("accepted proposal metadata is invalid: %v", err)
	}
	if !slices.Equal(envelope.State.Tags, expectedTags) || !maps.Equal(envelope.State.Metadata, expectedMetadata) {
		return store.ValidationErrorf("proposal mutation metadata does not match the accepted reviewed proposal")
	}

	desiredAgentName := strings.TrimSpace(redact.SensitiveText(desired.AgentName))
	desiredTaskName := strings.TrimSpace(redact.SensitiveText(desired.TaskName))
	if strings.TrimSpace(desired.SessionName) != "" || strings.TrimSpace(desired.ParentTask) != "" ||
		desiredAgentName != expectedMetadata["agentname"] || desiredTaskName != expectedMetadata["taskname"] ||
		strings.TrimSpace(desired.Source) != memorySourceProposal || desired.SourceProposalID != proposal.ID {
		return store.ValidationErrorf("proposal catalog metadata does not match the accepted reviewed proposal")
	}
	desiredTags := desired.Tags
	if desiredTags == nil {
		desiredTags = []string{}
	}
	normalizedDesiredTags, err := omsprotocol.NormalizeTags(desiredTags)
	if err != nil || !slices.Equal(normalizedDesiredTags, expectedTags) {
		return store.ValidationErrorf("proposal catalog tags do not match the accepted reviewed proposal")
	}
	desired.SessionName = ""
	desired.ParentTask = ""
	desired.AgentName = expectedMetadata["agentname"]
	desired.TaskName = expectedMetadata["taskname"]
	desired.Source = memorySourceProposal
	desired.SourceProposalID = proposal.ID
	desired.Tags = append([]string(nil), expectedTags...)
	desired.ContentDigest = expectedContentDigest
	return nil
}

func canonicalRemoteProposalTags(description string) ([]string, error) {
	tags := tagsFromProposalDescription(description)
	if tags == nil {
		tags = []string{}
	}
	return omsprotocol.NormalizeTags(tags)
}

func canonicalRemoteProposalMetadata(proposal *store.MemoryProposal) (map[string]string, error) {
	metadata := map[string]string{
		"source":           memorySourceProposal,
		"sourceProposalId": proposal.ID,
	}
	if agentName := strings.TrimSpace(redact.SensitiveText(proposal.AgentName)); agentName != "" {
		metadata["agentName"] = agentName
	}
	if taskName := strings.TrimSpace(redact.SensitiveText(proposal.TaskName)); taskName != "" {
		metadata["taskName"] = taskName
	}
	return omsprotocol.NormalizeMetadata(metadata)
}

func admitRemoteMemoryReplaceTx(
	ctx context.Context,
	tx *sql.Tx,
	mutation store.MemoryMutationAdmission,
	desired store.RemoteMemoryCatalogEntry,
	binding *store.MemoryBackendBinding,
	expectedGeneration int64,
	expectedBackendVersion string,
) (*store.RemoteMemoryCatalogEntry, *store.MemoryOperation, error) {
	current, err := getRemoteMemoryQuery(ctx, tx, mutation.NamespaceUID, mutation.MemoryID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, store.ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	if current.BackendUID != binding.BackendUID || current.AuthorityEpoch != binding.AuthorityEpoch || current.RoutingEpoch != binding.RoutingEpoch {
		return nil, nil, fmt.Errorf("%w: catalog binding identity is stale", store.ErrConflict)
	}
	if current.Generation != expectedGeneration || current.BackendVersion != expectedBackendVersion {
		return nil, nil, fmt.Errorf("%w: materialized generation or backend version changed", store.ErrConflict)
	}
	if current.PendingOperationID != "" || current.Deleted || current.MaterializationState != store.MemoryMaterializationActive {
		return nil, nil, fmt.Errorf("%w: memory is not available for replacement", store.ErrConflict)
	}
	desiredGeneration := current.DesiredGeneration + 1
	if desiredGeneration <= current.Generation {
		desiredGeneration = current.Generation + 1
	}
	if err := validateAndCanonicalizeMemoryMutation(&mutation, store.MemoryOperationReplace, desiredGeneration,
		current.Generation, expectedBackendVersion, binding); err != nil {
		return nil, nil, err
	}
	canonicalTags, err := canonicalMutationTags(mutation.Payload)
	if err != nil {
		return nil, nil, err
	}
	desired.Tags = canonicalTags
	if err := enforceMemoryOperationAdmissionCapacity(ctx, tx, mutation.NamespaceUID, mutation.Payload, store.MemoryOperationReplace); err != nil {
		return nil, nil, err
	}
	tagsJSON, err := marshalTags(desired.Tags)
	if err != nil {
		return nil, nil, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE remote_memory_catalog
		SET desired_generation = ?, routing_epoch = ?, pending_session_name = ?, pending_agent_name = ?,
			pending_task_name = ?, pending_parent_task = ?, pending_source = ?, pending_tags_json = ?,
			pending_operation_id = ?, updated_at = ?
		WHERE namespace_uid = ? AND id = ? AND generation = ? AND desired_generation = ? AND backend_version = ?
			AND pending_operation_id = '' AND deleted = FALSE AND materialization_state = ?`,
		desiredGeneration, mutation.RoutingEpoch, desired.SessionName, desired.AgentName, desired.TaskName, desired.ParentTask,
		desired.Source, tagsJSON, mutation.OperationID, mutation.Now, mutation.NamespaceUID, mutation.MemoryID,
		current.Generation, current.DesiredGeneration, expectedBackendVersion, store.MemoryMaterializationActive)
	if err != nil {
		return nil, nil, err
	}
	if err := ensureMemoryRowsAffectedConflict(result, "memory changed during replacement admission"); err != nil {
		return nil, nil, err
	}
	operation := newMemoryOperation(mutation, store.MemoryOperationReplace, desiredGeneration, current.Generation, expectedBackendVersion)
	if err := insertMemoryOperationWithPayload(ctx, tx, &operation, mutation.Payload); err != nil {
		return nil, nil, err
	}
	updated, err := getRemoteMemoryQuery(ctx, tx, mutation.NamespaceUID, mutation.MemoryID)
	return updated, &operation, err
}

func admitRemoteMemoryDeleteTx(
	ctx context.Context,
	tx *sql.Tx,
	mutation store.MemoryMutationAdmission,
	binding *store.MemoryBackendBinding,
	expectedGeneration int64,
	expectedBackendVersion string,
) (*store.RemoteMemoryCatalogEntry, *store.MemoryOperation, error) {
	current, err := getRemoteMemoryQuery(ctx, tx, mutation.NamespaceUID, mutation.MemoryID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, store.ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	if current.BackendUID != binding.BackendUID || current.AuthorityEpoch != binding.AuthorityEpoch {
		return nil, nil, fmt.Errorf("%w: catalog binding identity is stale", store.ErrConflict)
	}
	if current.Generation != expectedGeneration || current.BackendVersion != expectedBackendVersion {
		return nil, nil, fmt.Errorf("%w: materialized generation or backend version changed", store.ErrConflict)
	}
	if current.Deleted {
		return nil, nil, fmt.Errorf("%w: memory is already deleted", store.ErrConflict)
	}
	desiredGeneration := current.DesiredGeneration + 1
	if desiredGeneration <= current.Generation {
		desiredGeneration = current.Generation + 1
	}
	if err := validateAndCanonicalizeMemoryMutation(&mutation, store.MemoryOperationDelete, desiredGeneration,
		current.Generation, expectedBackendVersion, binding); err != nil {
		return nil, nil, err
	}
	if err := supersedeUnresolvedMemoryOperations(ctx, tx, current, mutation); err != nil {
		return nil, nil, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE remote_memory_catalog
		SET desired_generation = ?, routing_epoch = ?, deleted = TRUE,
			pending_session_name = '', pending_agent_name = '', pending_task_name = '',
			pending_parent_task = '', pending_source = '', pending_tags_json = '[]', pending_operation_id = ?, updated_at = ?
		WHERE namespace_uid = ? AND id = ? AND generation = ? AND backend_version = ? AND deleted = FALSE`,
		desiredGeneration, mutation.RoutingEpoch, mutation.OperationID, mutation.Now,
		mutation.NamespaceUID, mutation.MemoryID, current.Generation, expectedBackendVersion)
	if err != nil {
		return nil, nil, err
	}
	if err := ensureMemoryRowsAffectedConflict(result, "memory changed during delete admission"); err != nil {
		return nil, nil, err
	}
	operation := newMemoryOperation(mutation, store.MemoryOperationDelete, desiredGeneration, current.Generation, expectedBackendVersion)
	if err := insertMemoryOperationWithPayload(ctx, tx, &operation, mutation.Payload); err != nil {
		return nil, nil, err
	}
	updated, err := getRemoteMemoryQuery(ctx, tx, mutation.NamespaceUID, mutation.MemoryID)
	return updated, &operation, err
}

func validateRemoteMemoryMetadata(memory store.RemoteMemoryCatalogEntry) error {
	if len(memory.Tags) > 64 {
		return store.ValidationErrorf("memory tags exceed 64 tag hard cap")
	}
	for _, tag := range memory.Tags {
		if len(strings.TrimSpace(tag)) > 128 {
			return store.ValidationErrorf("memory tag exceeds 128 byte hard cap")
		}
	}
	return nil
}

func validateAndCanonicalizeMemoryMutation(
	mutation *store.MemoryMutationAdmission,
	kind store.MemoryOperationKind,
	desiredGeneration, expectedGeneration int64,
	expectedBackendVersion string,
	binding *store.MemoryBackendBinding,
) error {
	if mutation == nil || binding == nil {
		return store.ValidationErrorf("canonical mutation and durable binding are required")
	}
	envelope, err := omsprotocol.DecodeMutationEnvelope(mutation.Payload)
	if err != nil {
		return store.ValidationErrorf("canonical mutation payload is invalid: %v", err)
	}
	expectedKind := ""
	switch kind {
	case store.MemoryOperationCreate:
		expectedKind = omsprotocol.MutationKindCreate
	case store.MemoryOperationReplace:
		expectedKind = omsprotocol.MutationKindReplace
	case store.MemoryOperationDelete:
		expectedKind = omsprotocol.MutationKindDelete
	default:
		return store.ValidationErrorf("unsupported memory operation kind %q", kind)
	}
	expectedBinding := omsprotocol.Binding{
		ClusterID: mutation.ClusterID, NamespaceUID: mutation.NamespaceUID, BackendUID: mutation.BackendUID,
		AuthorityEpoch: uint64(mutation.AuthorityEpoch), RoutingEpoch: uint64(mutation.RoutingEpoch),
		TenantID: binding.TenantID, StoreUUID: binding.StoreUUID,
	}
	if envelope.OperationID != mutation.OperationID || envelope.MemoryID != mutation.MemoryID ||
		envelope.Binding != expectedBinding || envelope.Kind != expectedKind ||
		envelope.Generation != uint64(desiredGeneration) || envelope.ExpectedGeneration != uint64(expectedGeneration) ||
		envelope.ExpectedBackendVersion != expectedBackendVersion || envelope.MutationDigest != mutation.MutationDigest ||
		envelope.ContentDigest != mutation.ContentDigest {
		return store.ValidationErrorf("canonical mutation payload does not match the admitted operation and binding")
	}
	canonical, err := omsprotocol.EncodeJSON(envelope)
	if err != nil {
		return store.ValidationErrorf("canonical mutation payload could not be encoded: %v", err)
	}
	if len(canonical) > omsprotocol.MaxHTTPBodyBytes {
		return store.ValidationErrorf("canonical operation payload exceeds %d bytes", omsprotocol.MaxHTTPBodyBytes)
	}
	mutation.Payload = canonical
	return nil
}

func canonicalMutationTags(payload []byte) ([]string, error) {
	envelope, err := omsprotocol.DecodeMutationEnvelope(payload)
	if err != nil || envelope.State == nil {
		return nil, store.ValidationErrorf("live mutation payload is invalid")
	}
	return append([]string(nil), envelope.State.Tags...), nil
}

func normalizeNewRemoteCatalog(
	desired store.RemoteMemoryCatalogEntry,
	mutation store.MemoryMutationAdmission,
	binding *store.MemoryBackendBinding,
	proposalApply bool,
	expectedGeneration, desiredGeneration int64,
) store.RemoteMemoryCatalogEntry {
	trust := store.MemoryTrustUntrusted
	sourceProposalID := ""
	if proposalApply {
		trust = store.MemoryTrustReviewed
		sourceProposalID = mutation.ProposalID
	}
	source := strings.TrimSpace(desired.Source)
	if source == "" {
		source = memorySourceManual
	}
	return store.RemoteMemoryCatalogEntry{
		ID: mutation.MemoryID, Namespace: mutation.Namespace, NamespaceUID: mutation.NamespaceUID,
		ClusterID: mutation.ClusterID, BackendUID: mutation.BackendUID, AuthorityEpoch: mutation.AuthorityEpoch,
		RoutingEpoch: mutation.RoutingEpoch, TenantID: binding.TenantID, StoreUUID: binding.StoreUUID,
		Generation: expectedGeneration, DesiredGeneration: desiredGeneration, GovernanceRevision: 1,
		MaterializationState: store.MemoryMaterializationPending, Trust: trust,
		SessionName: desired.SessionName, AgentName: desired.AgentName, TaskName: desired.TaskName,
		ParentTask: desired.ParentTask, Source: source, SourceProposalID: sourceProposalID,
		Tags: normalizeTags(desired.Tags), ContentAvailable: false, PendingOperationID: mutation.OperationID,
		CreatedAt: mutation.Now, UpdatedAt: mutation.Now,
	}
}

func newMemoryOperation(mutation store.MemoryMutationAdmission, kind store.MemoryOperationKind, desiredGeneration, expectedGeneration int64, expectedBackendVersion string) store.MemoryOperation {
	retainUntil := mutation.Now.Add(defaultOperationRecoveryWindow)
	if mutation.MaxAgeAt.After(retainUntil) {
		retainUntil = mutation.MaxAgeAt
	}
	return store.MemoryOperation{
		ID: mutation.OperationID, Namespace: mutation.Namespace, NamespaceUID: mutation.NamespaceUID,
		ClusterID: mutation.ClusterID, BackendUID: mutation.BackendUID, AuthorityEpoch: mutation.AuthorityEpoch,
		RoutingEpoch: mutation.RoutingEpoch, MemoryID: mutation.MemoryID, ProposalID: mutation.ProposalID,
		Kind: kind, DesiredGeneration: desiredGeneration, ExpectedMaterializedGeneration: expectedGeneration,
		ExpectedBackendVersion: expectedBackendVersion, OperationIdempotencyKey: mutation.OperationIdempotencyKey,
		MutationIdempotencyKey: mutation.IdempotencyKey, RequestDigest: mutation.RequestDigest,
		MutationDigest: mutation.MutationDigest, ContentDigest: mutation.ContentDigest, PayloadBytes: len(mutation.Payload),
		State: store.MemoryOperationQueued, NextRetryAt: mutation.Now, MaxAgeAt: mutation.MaxAgeAt,
		PayloadRetainUntil: retainUntil, Actor: boundedMemoryAuditText(mutation.Actor), Reason: boundedMemoryAuditText(mutation.Reason),
		CreatedAt: mutation.Now, UpdatedAt: mutation.Now,
	}
}

func insertRemoteMemoryCatalog(ctx context.Context, tx *sql.Tx, catalog *store.RemoteMemoryCatalogEntry) error {
	tagsJSON, err := marshalTags(catalog.Tags)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO remote_memory_catalog
		(namespace_uid, id, namespace_name, cluster_id, backend_uid, authority_epoch, routing_epoch, tenant_id, store_uuid,
		 backend_memory_id, backend_version, generation, desired_generation, governance_revision, materialization_state,
		 disabled, deleted, trust, session_name, agent_name, task_name, parent_task, source, source_proposal_id,
		 tags_json, content_digest, content_available, pending_operation_id, created_at, updated_at, last_recalled_at, recalled_count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		catalog.NamespaceUID, catalog.ID, catalog.Namespace, catalog.ClusterID, catalog.BackendUID,
		catalog.AuthorityEpoch, catalog.RoutingEpoch, catalog.TenantID, catalog.StoreUUID,
		catalog.BackendMemoryID, catalog.BackendVersion, catalog.Generation, catalog.DesiredGeneration,
		catalog.GovernanceRevision, catalog.MaterializationState, catalog.Disabled, catalog.Deleted, catalog.Trust,
		catalog.SessionName, catalog.AgentName, catalog.TaskName, catalog.ParentTask, catalog.Source, catalog.SourceProposalID,
		tagsJSON, catalog.ContentDigest, catalog.ContentAvailable, catalog.PendingOperationID, catalog.CreatedAt,
		catalog.UpdatedAt, catalog.LastRecalledAt, catalog.RecalledCount)
	return err
}

func encodeDurableMemoryOperationPayload(payload []byte) ([]byte, error) {
	if len(payload) == 0 || len(payload) > omsprotocol.MaxHTTPBodyBytes {
		return nil, store.ValidationErrorf("canonical operation payload size is invalid")
	}
	if len(payload) <= maxMemoryOperationPayloadBytes {
		return append([]byte(nil), payload...), nil
	}
	var compressed bytes.Buffer
	compressed.WriteString(durablePayloadCompressionPrefix)
	writer := zlib.NewWriter(&compressed)
	if _, err := writer.Write(payload); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	if compressed.Len() > maxMemoryOperationPayloadBytes {
		return nil, store.ValidationErrorf("compressed operation payload exceeds %d bytes", maxMemoryOperationPayloadBytes)
	}
	return compressed.Bytes(), nil
}

func decodeDurableMemoryOperationPayload(payload []byte) ([]byte, error) {
	if !bytes.HasPrefix(payload, []byte(durablePayloadCompressionPrefix)) {
		if len(payload) > omsprotocol.MaxHTTPBodyBytes {
			return nil, store.ValidationErrorf("stored operation payload exceeds the wire limit")
		}
		return append([]byte(nil), payload...), nil
	}
	reader, err := zlib.NewReader(bytes.NewReader(payload[len(durablePayloadCompressionPrefix):]))
	if err != nil {
		return nil, fmt.Errorf("open compressed memory operation payload: %w", err)
	}
	decoded, readErr := io.ReadAll(io.LimitReader(reader, omsprotocol.MaxHTTPBodyBytes+1))
	closeErr := reader.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read compressed memory operation payload: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close compressed memory operation payload: %w", closeErr)
	}
	if len(decoded) == 0 || len(decoded) > omsprotocol.MaxHTTPBodyBytes {
		return nil, store.ValidationErrorf("decompressed operation payload size is invalid")
	}
	return decoded, nil
}

func insertMemoryOperationWithPayload(ctx context.Context, tx *sql.Tx, operation *store.MemoryOperation, payload []byte) error {
	storedPayload, err := encodeDurableMemoryOperationPayload(payload)
	if err != nil {
		return err
	}
	operation.PayloadBytes = len(payload)
	result, err := tx.ExecContext(ctx, `INSERT INTO memory_operations
		(id, namespace_name, namespace_uid, cluster_id, backend_uid, authority_epoch, routing_epoch, memory_id, proposal_id,
		 kind, desired_generation, expected_materialized_generation, expected_backend_version, operation_idempotency_key,
		 mutation_idempotency_key, request_digest, mutation_digest, content_digest, payload, payload_bytes, state, lease_owner, lease_epoch,
		 lease_origin_state, lease_expires_at, send_started_at, request_deadline, attempts, next_retry_at, max_age_at,
		 payload_retain_until, error_code, error_message, receipt_binding_digest, receipt_applied_generation,
		 receipt_backend_version, receipt_backend_memory_id, receipt_content_digest, receipt_mutation_digest,
		 receipt_completed_at, actor, reason, created_at, updated_at, completed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', 0, '', NULL, NULL, NULL, 0, ?, ?, ?, '', '', '', 0, '', '', '', '', NULL, ?, ?, ?, ?, NULL)`,
		operation.ID, operation.Namespace, operation.NamespaceUID, operation.ClusterID, operation.BackendUID,
		operation.AuthorityEpoch, operation.RoutingEpoch, operation.MemoryID, operation.ProposalID, operation.Kind,
		operation.DesiredGeneration, operation.ExpectedMaterializedGeneration, operation.ExpectedBackendVersion,
		operation.OperationIdempotencyKey, operation.MutationIdempotencyKey, operation.RequestDigest,
		operation.MutationDigest, operation.ContentDigest, storedPayload, len(payload), operation.State, operation.NextRetryAt,
		operation.MaxAgeAt, operation.PayloadRetainUntil, operation.Actor, operation.Reason, operation.CreatedAt, operation.UpdatedAt)
	if err != nil {
		if isSQLiteConstraintError(err) {
			return fmt.Errorf("%w: unresolved memory operation or idempotency identity already exists", store.ErrConflict)
		}
		return err
	}
	sequence, err := result.LastInsertId()
	if err != nil {
		return err
	}
	operation.Sequence = sequence
	operation.PayloadBytes = len(payload)
	return nil
}

func memoryIdempotencyFromMutation(mutation store.MemoryMutationAdmission) store.MemoryIdempotencyRecord {
	return store.MemoryIdempotencyRecord{
		NamespaceUID: mutation.NamespaceUID, Principal: mutation.Principal, Route: mutation.Route,
		CallerKey: mutation.IdempotencyKey, RequestDigest: mutation.RequestDigest,
		AuthorityEpoch: mutation.AuthorityEpoch, RoutingEpoch: mutation.RoutingEpoch,
		OriginalStatus: mutation.OriginalStatus, ResponseType: mutation.ResponseType,
		Location: mutation.Location, RetryAfterSeconds: mutation.RetryAfterSeconds,
		ResponseSnapshot: []byte{},
		ExpiresAt:        mutation.IdempotencyExpiresAt, CreatedAt: mutation.Now, UpdatedAt: mutation.Now,
	}
}

func reserveMemoryIdempotency(
	ctx context.Context,
	tx *sql.Tx,
	record store.MemoryIdempotencyRecord,
) (bool, *store.MemoryIdempotencyRecord, error) {
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_idempotency
		WHERE namespace_uid = ? AND principal = ? AND route = ? AND caller_key = ? AND expires_at <= ?
			AND (operation_id = '' OR EXISTS (SELECT 1 FROM memory_operations o
				WHERE o.namespace_uid = memory_idempotency.namespace_uid AND o.id = memory_idempotency.operation_id
					AND o.state IN ('succeeded','abandoned','superseded','orphaned')))`,
		record.NamespaceUID, record.Principal, record.Route, record.CallerKey, record.CreatedAt); err != nil {
		return false, nil, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO memory_idempotency
		(namespace_uid, principal, route, caller_key, request_digest, authority_epoch, routing_epoch,
		 original_status, response_type, memory_id, operation_id, location, retry_after_seconds, response_digest,
		 response_snapshot, expires_at, terminal_binding_digest, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(namespace_uid, principal, route, caller_key) DO NOTHING`,
		record.NamespaceUID, record.Principal, record.Route, record.CallerKey, record.RequestDigest,
		record.AuthorityEpoch, record.RoutingEpoch, record.OriginalStatus, record.ResponseType,
		record.MemoryID, record.OperationID, record.Location, record.RetryAfterSeconds, record.ResponseDigest,
		record.ResponseSnapshot, record.ExpiresAt, record.TerminalBindingDigest, record.CreatedAt, record.UpdatedAt)
	if err != nil {
		return false, nil, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, nil, err
	}
	if rows == 1 {
		return true, &record, nil
	}
	existing, err := getMemoryIdempotencyQuery(
		ctx,
		tx,
		record.NamespaceUID,
		record.Principal,
		record.Route,
		record.CallerKey,
	)
	if err != nil {
		return false, nil, err
	}
	if existing.AuthorityEpoch != record.AuthorityEpoch || existing.RoutingEpoch != record.RoutingEpoch {
		return false, nil, fmt.Errorf(
			"%w: memory idempotency key belongs to a prior authority or routing binding",
			store.ErrDuplicateMismatch,
		)
	}
	return false, existing, nil
}

func getMemoryIdempotencyQuery(ctx context.Context, q rowQueryer, namespaceUID, principal, route, callerKey string) (*store.MemoryIdempotencyRecord, error) {
	var record store.MemoryIdempotencyRecord
	err := q.QueryRowContext(ctx, `SELECT namespace_uid, principal, route, caller_key, request_digest, authority_epoch,
		routing_epoch, original_status, response_type, memory_id, operation_id, location, retry_after_seconds,
		response_digest, response_snapshot, expires_at, terminal_binding_digest, created_at, updated_at
		FROM memory_idempotency WHERE namespace_uid = ? AND principal = ? AND route = ? AND caller_key = ?`,
		namespaceUID, principal, route, callerKey).Scan(
		&record.NamespaceUID, &record.Principal, &record.Route, &record.CallerKey, &record.RequestDigest,
		&record.AuthorityEpoch, &record.RoutingEpoch, &record.OriginalStatus, &record.ResponseType,
		&record.MemoryID, &record.OperationID, &record.Location, &record.RetryAfterSeconds,
		&record.ResponseDigest, &record.ResponseSnapshot, &record.ExpiresAt, &record.TerminalBindingDigest, &record.CreatedAt, &record.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &record, nil
}

func memoryAdmissionReplayResult(ctx context.Context, q rowQueryer, idempotency *store.MemoryIdempotencyRecord) (*store.MemoryMutationAdmissionResult, error) {
	if len(idempotency.ResponseSnapshot) > 0 {
		var snapshot memoryIdempotencySnapshot
		if err := json.Unmarshal(idempotency.ResponseSnapshot, &snapshot); err != nil {
			return nil, fmt.Errorf("decode immutable memory idempotency snapshot: %w", err)
		}
		if snapshot.Memory.ID != idempotency.MemoryID || snapshot.Operation.ID != idempotency.OperationID {
			return nil, fmt.Errorf("%w: immutable memory idempotency snapshot identity changed", store.ErrConflict)
		}
		return &store.MemoryMutationAdmissionResult{
			Memory: snapshot.Memory, Operation: snapshot.Operation, Idempotency: *idempotency,
		}, nil
	}
	catalog, err := getRemoteMemoryQuery(ctx, q, idempotency.NamespaceUID, idempotency.MemoryID)
	if err != nil {
		return nil, err
	}
	operation, err := getMemoryOperationQuery(ctx, q, idempotency.NamespaceUID, idempotency.OperationID)
	if err != nil {
		return nil, err
	}
	return &store.MemoryMutationAdmissionResult{Memory: *catalog, Operation: *operation, Idempotency: *idempotency}, nil
}

type memoryIdempotencySnapshot struct {
	Memory    store.RemoteMemoryCatalogEntry `json:"memory"`
	Operation store.MemoryOperation          `json:"operation"`
	Content   []byte                         `json:"content,omitempty"`
}

func encodeMemoryIdempotencySnapshot(
	catalog *store.RemoteMemoryCatalogEntry,
	operation *store.MemoryOperation,
	payload []byte,
) ([]byte, error) {
	if catalog == nil || operation == nil {
		return nil, store.ValidationErrorf("memory idempotency snapshot requires memory and operation")
	}
	var content []byte
	if len(payload) > 0 {
		envelope, err := omsprotocol.DecodeMutationEnvelope(payload)
		if err != nil {
			return nil, fmt.Errorf("decode memory idempotency payload: %w", err)
		}
		if envelope.OperationID != operation.ID || envelope.MemoryID != catalog.ID {
			return nil, fmt.Errorf("%w: memory idempotency payload identity changed", store.ErrConflict)
		}
		if envelope.State != nil {
			content = []byte(envelope.State.Content)
		}
	}
	encoded, err := json.Marshal(memoryIdempotencySnapshot{Memory: *catalog, Operation: *operation, Content: content})
	if err != nil {
		return nil, fmt.Errorf("encode memory idempotency snapshot: %w", err)
	}
	if len(encoded) > maxIdempotencySnapshotBytes {
		return nil, fmt.Errorf("%w: memory idempotency snapshot exceeds %d bytes", store.ErrCapacity, maxIdempotencySnapshotBytes)
	}
	return encoded, nil
}

func saveMemoryIdempotencySnapshot(
	ctx context.Context,
	tx *sql.Tx,
	record *store.MemoryIdempotencyRecord,
	snapshot []byte,
	now time.Time,
) error {
	if record == nil || len(snapshot) == 0 || len(snapshot) > maxIdempotencySnapshotBytes {
		return store.ValidationErrorf("bounded memory idempotency snapshot is required")
	}
	result, err := tx.ExecContext(ctx, `UPDATE memory_idempotency SET response_snapshot = ?, updated_at = ?
		WHERE namespace_uid = ? AND principal = ? AND route = ? AND caller_key = ? AND request_digest = ?`,
		snapshot, now, record.NamespaceUID, record.Principal, record.Route, record.CallerKey, record.RequestDigest)
	if err != nil {
		return err
	}
	if err := ensureMemoryRowsAffectedConflict(result, "memory idempotency record changed before snapshot persistence"); err != nil {
		return err
	}
	record.ResponseSnapshot = append([]byte(nil), snapshot...)
	record.UpdatedAt = now
	return nil
}

func enforceMemoryOperationAdmissionCapacity(ctx context.Context, q rowQueryer, namespaceUID string, payload []byte, kind store.MemoryOperationKind) error {
	limits := governedMemoryQuotas
	if kind == store.MemoryOperationDelete {
		return nil
	}
	storedPayload, err := encodeDurableMemoryOperationPayload(payload)
	if err != nil {
		return err
	}
	payloadBytes := len(storedPayload)
	var count, namespaceBytes int64
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(length(payload)), 0) FROM memory_operations
		WHERE namespace_uid = ? AND state IN ('queued','leased','dispatching','ambiguous','dead_lettered')`, namespaceUID).
		Scan(&count, &namespaceBytes); err != nil {
		return err
	}
	if count >= limits.NamespaceUnresolvedRows || namespaceBytes+int64(payloadBytes) > limits.NamespaceUnresolvedBytes {
		return fmt.Errorf("%w: namespace unresolved memory operation quota reached", store.ErrCapacity)
	}
	var globalBytes int64
	if err := q.QueryRowContext(ctx, `SELECT COALESCE(SUM(length(payload)), 0) FROM memory_operations
		WHERE state IN ('queued','leased','dispatching','ambiguous','dead_lettered')`).Scan(&globalBytes); err != nil {
		return err
	}
	if globalBytes+int64(payloadBytes) > limits.GlobalUnresolvedBytes {
		return fmt.Errorf("%w: global unresolved memory operation payload quota reached", store.ErrCapacity)
	}
	var namespaceRetained, globalRetained int64
	if err := q.QueryRowContext(ctx, `SELECT COALESCE(SUM(length(payload)), 0) FROM memory_operations
		WHERE namespace_uid = ? AND length(payload) > 0`, namespaceUID).Scan(&namespaceRetained); err != nil {
		return err
	}
	if namespaceRetained+int64(payloadBytes) > limits.NamespaceRetainedPayloadBytes {
		return fmt.Errorf("%w: namespace retained operation payload quota reached", store.ErrCapacity)
	}
	if err := q.QueryRowContext(ctx, `SELECT COALESCE(SUM(length(payload)), 0) FROM memory_operations
		WHERE length(payload) > 0`).Scan(&globalRetained); err != nil {
		return err
	}
	if globalRetained+int64(payloadBytes) > limits.GlobalRetainedPayloadBytes {
		return fmt.Errorf("%w: global retained operation payload quota reached", store.ErrCapacity)
	}
	return nil
}

//nolint:gocyclo // Capacity gates enumerate independent namespace/global safety reserves.
func enforceMemoryGovernanceAdmissionCapacity(ctx context.Context, q rowQueryer, namespaceUID string, kind store.MemoryOperationKind) error {
	limits := governedMemoryQuotas
	safetyOperation := kind == store.MemoryOperationDelete
	threshold := func(limit int64) int64 {
		if safetyOperation || limit <= 0 {
			return limit
		}
		reserve := limits.SafetyReserveRows
		if reserve >= limit {
			reserve = limit / 10
		}
		return limit - reserve
	}
	checkRows := func(query string, namespaceLimit, globalLimit int64, name string) error {
		if namespaceLimit > 0 {
			var count int64
			if err := q.QueryRowContext(ctx, query+` WHERE namespace_uid = ?`, namespaceUID).Scan(&count); err != nil {
				return err
			}
			if count > threshold(namespaceLimit) {
				return fmt.Errorf("%w: namespace %s quota reached", store.ErrCapacity, name)
			}
		}
		if globalLimit > 0 {
			var count int64
			if err := q.QueryRowContext(ctx, query).Scan(&count); err != nil {
				return err
			}
			if count > threshold(globalLimit) {
				return fmt.Errorf("%w: global %s quota reached", store.ErrCapacity, name)
			}
		}
		return nil
	}
	if kind == store.MemoryOperationCreate {
		if err := checkRows(`SELECT COUNT(*) FROM remote_memory_catalog`, limits.NamespaceCatalogRows, limits.GlobalCatalogRows, "memory catalog row"); err != nil {
			return err
		}
	}
	if limits.NamespaceTombstoneRows > 0 {
		var tombstones int64
		if err := q.QueryRowContext(ctx, `SELECT
			(SELECT COUNT(*) FROM remote_memory_catalog
				WHERE namespace_uid = ? AND (deleted = TRUE OR materialization_state IN ('deleted','orphaned'))) +
			(SELECT COUNT(*) FROM remote_memory_generation_watermarks WHERE namespace_uid = ?)`,
			namespaceUID, namespaceUID).Scan(&tombstones); err != nil {
			return err
		}
		if !safetyOperation && tombstones >= threshold(limits.NamespaceTombstoneRows) {
			return fmt.Errorf("%w: namespace tombstone row watermark reached", store.ErrCapacity)
		}
	}
	if limits.GlobalTombstoneRows > 0 {
		var tombstones int64
		if err := q.QueryRowContext(ctx, `SELECT
			(SELECT COUNT(*) FROM remote_memory_catalog
				WHERE deleted = TRUE OR materialization_state IN ('deleted','orphaned')) +
			(SELECT COUNT(*) FROM remote_memory_generation_watermarks)`).Scan(&tombstones); err != nil {
			return err
		}
		if !safetyOperation && tombstones >= threshold(limits.GlobalTombstoneRows) {
			return fmt.Errorf("%w: global tombstone row watermark reached", store.ErrCapacity)
		}
	}
	if err := checkRows(`SELECT COUNT(*) FROM memory_operations`, limits.NamespaceOperationRows, limits.GlobalOperationRows, "memory operation row"); err != nil {
		return err
	}
	if err := checkRows(`SELECT COUNT(*) FROM memory_idempotency`, limits.NamespaceIdempotencyRows, limits.GlobalIdempotencyRows, "memory idempotency row"); err != nil {
		return err
	}
	if err := enforceMemoryByteQuota(ctx, q, namespaceUID,
		`SELECT COALESCE(SUM(length(response_snapshot) + length(response_digest) + length(location)), 0) FROM memory_idempotency`,
		limits.NamespaceIdempotencyBytes, limits.GlobalIdempotencyBytes, threshold, "memory idempotency bytes"); err != nil {
		return err
	}
	receiptQuery := `SELECT COALESCE(SUM(length(receipt_binding_digest) + length(receipt_backend_version) +
		length(receipt_backend_memory_id) + length(receipt_content_digest) + length(receipt_mutation_digest)), 0) +
		(COUNT(CASE WHEN state IN ('queued','leased','dispatching','ambiguous','dead_lettered') THEN 1 END) * ?)
		FROM memory_operations`
	if err := enforceMemoryByteQuotaWithArg(ctx, q, namespaceUID, receiptQuery, estimatedMemoryReceiptBytes,
		limits.NamespaceReceiptBytes, limits.GlobalReceiptBytes, threshold, "memory receipt bytes"); err != nil {
		return err
	}
	var auditRows, auditBytes int64
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(length(id) + length(actor) + length(action) +
		length(reason) + length(previous_state) + length(new_state) + length(memory_id) + length(operation_id) +
		length(proposal_id) + length(request_digest) + length(mutation_digest) + length(content_digest) + length(request_id)), 0)
		FROM memory_audit WHERE namespace_uid = ?`, namespaceUID).Scan(&auditRows, &auditBytes); err != nil {
		return err
	}
	if limits.NamespaceAuditRows > 0 && auditRows+1 > threshold(limits.NamespaceAuditRows) {
		return fmt.Errorf("%w: namespace memory audit row watermark reached", store.ErrCapacity)
	}
	if limits.NamespaceAuditBytes > 0 && auditBytes+maxMemoryAuditTextBytes > threshold(limits.NamespaceAuditBytes) {
		return fmt.Errorf("%w: namespace memory audit byte watermark reached", store.ErrCapacity)
	}
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(length(id) + length(actor) + length(action) +
		length(reason) + length(previous_state) + length(new_state) + length(memory_id) + length(operation_id) +
		length(proposal_id) + length(request_digest) + length(mutation_digest) + length(content_digest) + length(request_id)), 0)
		FROM memory_audit`).Scan(&auditRows, &auditBytes); err != nil {
		return err
	}
	if limits.GlobalAuditRows > 0 && auditRows+1 > threshold(limits.GlobalAuditRows) {
		return fmt.Errorf("%w: global memory audit row watermark reached", store.ErrCapacity)
	}
	if limits.GlobalAuditBytes > 0 && auditBytes+maxMemoryAuditTextBytes > threshold(limits.GlobalAuditBytes) {
		return fmt.Errorf("%w: global memory audit byte watermark reached", store.ErrCapacity)
	}
	return nil
}

func enforceMemoryByteQuota(
	ctx context.Context,
	q rowQueryer,
	namespaceUID, query string,
	namespaceLimit, globalLimit int64,
	threshold func(int64) int64,
	name string,
) error {
	return enforceMemoryByteQuotaWithArg(ctx, q, namespaceUID, query, nil, namespaceLimit, globalLimit, threshold, name)
}

func enforceMemoryByteQuotaWithArg(
	ctx context.Context,
	q rowQueryer,
	namespaceUID, query string,
	queryArg any,
	namespaceLimit, globalLimit int64,
	threshold func(int64) int64,
	name string,
) error {
	args := []any{}
	if queryArg != nil {
		args = append(args, queryArg)
	}
	if namespaceLimit > 0 {
		var total int64
		namespaceQuery := query + ` WHERE namespace_uid = ?`
		namespaceArgs := append(append([]any(nil), args...), namespaceUID)
		if err := q.QueryRowContext(ctx, namespaceQuery, namespaceArgs...).Scan(&total); err != nil {
			return err
		}
		if total > threshold(namespaceLimit) {
			return fmt.Errorf("%w: namespace %s quota reached", store.ErrCapacity, name)
		}
	}
	if globalLimit > 0 {
		var total int64
		if err := q.QueryRowContext(ctx, query, args...).Scan(&total); err != nil {
			return err
		}
		if total > threshold(globalLimit) {
			return fmt.Errorf("%w: global %s quota reached", store.ErrCapacity, name)
		}
	}
	return nil
}

func extendMemoryIdempotencyTerminalRetention(
	ctx context.Context,
	tx *sql.Tx,
	namespaceUID, operationID string,
	now time.Time,
) error {
	minimumExpiry := now.Add(minimumIdempotencyRetention)
	_, err := tx.ExecContext(ctx, `UPDATE memory_idempotency
		SET expires_at = CASE WHEN expires_at > ? THEN expires_at ELSE ? END, updated_at = ?
		WHERE namespace_uid = ? AND operation_id = ?`,
		minimumExpiry, minimumExpiry, now, namespaceUID, operationID)
	return err
}

func supersedeUnresolvedMemoryOperations(ctx context.Context, tx *sql.Tx, catalog *store.RemoteMemoryCatalogEntry, mutation store.MemoryMutationAdmission) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, proposal_id, state, lease_origin_state, send_started_at,
		dispatches, mutation_digest, content_digest FROM memory_operations
		WHERE namespace_uid = ? AND memory_id = ? AND state IN ('queued','leased','dispatching','ambiguous','dead_lettered')`,
		catalog.NamespaceUID, catalog.ID)
	if err != nil {
		return err
	}
	type unresolved struct {
		id, proposalID, state, leaseOrigin, mutationDigest, contentDigest string
		sendStartedAt                                                     sql.NullTime
		dispatches                                                        int64
	}
	var operations []unresolved
	for rows.Next() {
		var operation unresolved
		if err := rows.Scan(&operation.id, &operation.proposalID, &operation.state, &operation.leaseOrigin,
			&operation.sendStartedAt, &operation.dispatches, &operation.mutationDigest, &operation.contentDigest); err != nil {
			rows.Close() //nolint:errcheck
			return err
		}
		operations = append(operations, operation)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, operation := range operations {
		provablyUnsent := operation.dispatches == 0 && !operation.sendStartedAt.Valid &&
			(operation.state == string(store.MemoryOperationQueued) ||
				(operation.state == string(store.MemoryOperationLeased) && operation.leaseOrigin == string(store.MemoryOperationQueued)) ||
				operation.state == string(store.MemoryOperationDeadLettered))
		if !provablyUnsent {
			return fmt.Errorf("%w: unresolved memory operation %q crossed the provider send boundary", store.ErrConflict, operation.id)
		}
	}
	for _, operation := range operations {
		result, err := tx.ExecContext(ctx, `UPDATE memory_operations
			SET state = ?, lease_owner = '', lease_origin_state = '', lease_expires_at = NULL,
				request_deadline = NULL, updated_at = ?, completed_at = ?, error_code = 'MEMORY_OPERATION_SUPERSEDED',
				error_message = 'superseded by a higher-generation delete'
			WHERE namespace_uid = ? AND id = ? AND state = ?`,
			store.MemoryOperationSuperseded, mutation.Now, mutation.Now, catalog.NamespaceUID, operation.id, operation.state)
		if err != nil {
			return err
		}
		if err := ensureMemoryRowsAffectedConflict(result, "memory operation changed before delete supersession"); err != nil {
			return err
		}
		if err := extendMemoryIdempotencyTerminalRetention(ctx, tx, catalog.NamespaceUID, operation.id, mutation.Now); err != nil {
			return err
		}
		if operation.proposalID != "" {
			if err := abandonProposalApplication(ctx, tx, catalog.Namespace, operation.proposalID, operation.id,
				mutation.Actor, mutation.Reason, mutation.Now); err != nil {
				return err
			}
		}
		if err := insertMemoryAudit(ctx, tx, store.MemoryAuditRecord{
			Namespace: catalog.Namespace, NamespaceUID: catalog.NamespaceUID, Actor: mutation.Actor,
			Action: "memory.operation.supersede", Reason: mutation.Reason, PreviousState: operation.state,
			NewState: string(store.MemoryOperationSuperseded), AuthorityEpoch: mutation.AuthorityEpoch,
			RoutingEpoch: mutation.RoutingEpoch, MemoryID: catalog.ID, OperationID: operation.id,
			ProposalID: operation.proposalID, MutationDigest: operation.mutationDigest,
			ContentDigest: operation.contentDigest, RequestID: mutation.RequestID, CreatedAt: mutation.Now,
		}); err != nil {
			return err
		}
	}
	return nil
}

func abandonProposalApplication(
	ctx context.Context,
	tx *sql.Tx,
	namespace, proposalID, operationID, actor, reason string,
	now time.Time,
) error {
	if proposalID == "" {
		return nil
	}
	result, err := tx.ExecContext(ctx, `UPDATE memory_proposals
		SET status = ?, application_abandoned_by = ?, application_abandoned_reason = ?,
			application_abandoned_at = ?, updated_at = ?
		WHERE namespace = ? AND id = ? AND status = ? AND apply_operation_id = ?`,
		proposalStatusApplicationAbandoned, boundedMemoryAuditText(actor), boundedMemoryAuditText(reason),
		now, now, namespace, proposalID, proposalStatusApplying, operationID)
	if err != nil {
		return err
	}
	return ensureMemoryRowsAffectedConflict(result, "proposal changed before application abandonment")
}

func orphanProposalApplication(
	ctx context.Context,
	tx *sql.Tx,
	namespace, proposalID, operationID, actor, reason string,
	now time.Time,
) error {
	if proposalID == "" {
		return nil
	}
	result, err := tx.ExecContext(ctx, `UPDATE memory_proposals
		SET status = ?, application_abandoned_by = ?, application_abandoned_reason = ?,
			application_abandoned_at = ?, updated_at = ?
		WHERE namespace = ? AND id = ? AND status = ? AND apply_operation_id = ?`,
		proposalStatusApplicationOrphaned, boundedMemoryAuditText(actor), boundedMemoryAuditText(reason),
		now, now, namespace, proposalID, proposalStatusApplying, operationID)
	if err != nil {
		return err
	}
	return ensureMemoryRowsAffectedConflict(result, "proposal changed before application orphaning")
}

// GetRemoteMemory returns retention-safe catalog metadata for one canonical memory.
func (s *Store) GetRemoteMemory(ctx context.Context, namespaceUID, id string) (*store.RemoteMemoryCatalogEntry, error) {
	catalog, err := getRemoteMemoryQuery(ctx, s.db, strings.TrimSpace(namespaceUID), strings.TrimSpace(id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return catalog, err
}

// GetRemoteMemoryGenerationWatermark returns the compact provider generation
// retained after a catalog tombstone has been purged for the same authority.
func (s *Store) GetRemoteMemoryGenerationWatermark(
	ctx context.Context,
	namespaceUID, id, backendUID string,
	authorityEpoch int64,
	storeUUID string,
) (int64, error) {
	namespaceUID = strings.TrimSpace(namespaceUID)
	id = strings.TrimSpace(id)
	backendUID = strings.TrimSpace(backendUID)
	storeUUID = strings.TrimSpace(storeUUID)
	if namespaceUID == "" || id == "" || backendUID == "" || authorityEpoch <= 0 || storeUUID == "" {
		return 0, store.ValidationErrorf("generation watermark requires exact memory authority identity")
	}
	return getRemoteMemoryGenerationWatermarkQuery(
		ctx, s.db, namespaceUID, id, backendUID, authorityEpoch, storeUUID,
	)
}

// ListRemoteMemories lists catalog metadata deterministically without loading operation payloads.
func (s *Store) ListRemoteMemories(ctx context.Context, filter store.RemoteMemoryCatalogFilter) ([]store.RemoteMemoryCatalogEntry, error) {
	filter.NamespaceUID = strings.TrimSpace(filter.NamespaceUID)
	if filter.NamespaceUID == "" {
		return nil, store.ValidationErrorf("namespace uid is required")
	}
	if (filter.BeforeUpdatedAt == nil) != (strings.TrimSpace(filter.BeforeID) == "") {
		return nil, store.ValidationErrorf("catalog pagination requires both before updated-at and before id")
	}
	query := selectRemoteMemorySQL() + ` WHERE namespace_uid = ?`
	args := []any{filter.NamespaceUID}
	if !filter.IncludeDisabled {
		query += ` AND disabled = FALSE`
	}
	if !filter.IncludeDeleted {
		query += ` AND deleted = FALSE`
	}
	if len(filter.IDs) > 0 {
		ids := compactStrings(filter.IDs)
		query += ` AND id IN (` + memoryPlaceholders(len(ids)) + `)`
		for _, id := range ids {
			args = append(args, id)
		}
	}
	if len(filter.States) > 0 {
		query += ` AND materialization_state IN (` + memoryPlaceholders(len(filter.States)) + `)`
		for _, state := range filter.States {
			if !validMemoryMaterializationState(state) {
				return nil, store.ValidationErrorf("invalid materialization state %q", state)
			}
			args = append(args, state)
		}
	}
	if len(filter.Trust) > 0 {
		query += ` AND trust IN (` + memoryPlaceholders(len(filter.Trust)) + `)`
		for _, trust := range filter.Trust {
			if !validMemoryTrust(trust) {
				return nil, store.ValidationErrorf("invalid memory trust %q", trust)
			}
			args = append(args, trust)
		}
	}
	if filter.BeforeUpdatedAt != nil {
		query += ` AND (updated_at < ? OR (updated_at = ? AND id < ?))`
		args = append(args, filter.BeforeUpdatedAt.UTC(), filter.BeforeUpdatedAt.UTC(), filter.BeforeID)
	}
	query += ` ORDER BY updated_at DESC, id DESC LIMIT ?`
	args = append(args, boundedLimit(filter.Limit, defaultGovernedMemoryLimit, maxGovernedMemoryLimit))
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var catalog []store.RemoteMemoryCatalogEntry
	for rows.Next() {
		entry, err := scanRemoteMemory(rows)
		if err != nil {
			return nil, err
		}
		catalog = append(catalog, *entry)
	}
	return catalog, rows.Err()
}

// MarkRemoteMemoryMaterializationIssue fail-closes a catalog row after verified hydration mismatch or absence.
//
//nolint:gocyclo // The CAS and suppression checks are kept together to preserve one audited transition.
func (s *Store) MarkRemoteMemoryMaterializationIssue(ctx context.Context, issue store.RemoteMemoryMaterializationIssue) (*store.RemoteMemoryCatalogEntry, error) {
	issue.Now = normalizeMemoryNow(issue.Now)
	issue.NamespaceUID = strings.TrimSpace(issue.NamespaceUID)
	issue.ID = strings.TrimSpace(issue.ID)
	issue.BackendUID = strings.TrimSpace(issue.BackendUID)
	issue.ExpectedBackendVersion = strings.TrimSpace(issue.ExpectedBackendVersion)
	if issue.NamespaceUID == "" || issue.ID == "" || issue.BackendUID == "" || issue.AuthorityEpoch <= 0 || issue.RoutingEpoch <= 0 ||
		issue.ExpectedGeneration < 0 || strings.TrimSpace(issue.Actor) == "" || strings.TrimSpace(issue.Reason) == "" {
		return nil, store.ValidationErrorf("materialization issue requires exact catalog identity, actor, and reason")
	}
	if issue.State != store.MemoryMaterializationDiverged && issue.State != store.MemoryMaterializationLost {
		return nil, store.ValidationErrorf("materialization issue state must be diverged or lost")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	catalog, err := getRemoteMemoryQuery(ctx, tx, issue.NamespaceUID, issue.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	binding, err := getMemoryBackendBindingQuery(ctx, tx, issue.NamespaceUID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if binding.Mode != store.MemoryBackendModeRemote ||
		(binding.State != store.MemoryBackendBindingAccepting && binding.State != store.MemoryBackendBindingDraining) ||
		binding.BackendUID != issue.BackendUID || binding.AuthorityEpoch != issue.AuthorityEpoch ||
		binding.RoutingEpoch != issue.RoutingEpoch {
		return nil, fmt.Errorf("%w: durable binding changed before materialization issue", store.ErrConflict)
	}
	if catalog.BackendUID != issue.BackendUID || catalog.AuthorityEpoch != issue.AuthorityEpoch ||
		catalog.RoutingEpoch != issue.RoutingEpoch || catalog.Generation != issue.ExpectedGeneration ||
		catalog.BackendVersion != issue.ExpectedBackendVersion {
		return nil, fmt.Errorf("%w: catalog generation or binding changed before materialization issue", store.ErrConflict)
	}
	if catalog.Deleted || catalog.MaterializationState != store.MemoryMaterializationActive || catalog.PendingOperationID != "" {
		return nil, fmt.Errorf("%w: only active materialized memory without a pending operation can be marked %q", store.ErrConflict, issue.State)
	}
	result, err := tx.ExecContext(ctx, `UPDATE remote_memory_catalog
		SET materialization_state = ?, disabled = TRUE, content_available = FALSE,
			governance_revision = governance_revision + 1, updated_at = ?
		WHERE namespace_uid = ? AND id = ? AND backend_uid = ? AND authority_epoch = ? AND routing_epoch = ?
			AND generation = ? AND backend_version = ? AND deleted = FALSE
			AND materialization_state = ? AND pending_operation_id = ''`,
		issue.State, issue.Now, issue.NamespaceUID, issue.ID, issue.BackendUID, issue.AuthorityEpoch,
		issue.RoutingEpoch, issue.ExpectedGeneration, issue.ExpectedBackendVersion, store.MemoryMaterializationActive)
	if err != nil {
		return nil, err
	}
	if err := ensureMemoryRowsAffectedConflict(result, "catalog changed before materialization issue"); err != nil {
		return nil, err
	}
	if err := insertMemoryAudit(ctx, tx, store.MemoryAuditRecord{
		Namespace: catalog.Namespace, NamespaceUID: catalog.NamespaceUID, Actor: issue.Actor,
		Action: "memory.materialization.issue", Reason: issue.Reason,
		PreviousState: string(catalog.MaterializationState), NewState: string(issue.State),
		AuthorityEpoch: catalog.AuthorityEpoch, RoutingEpoch: catalog.RoutingEpoch, MemoryID: catalog.ID,
		OperationID: catalog.PendingOperationID, ContentDigest: catalog.ContentDigest,
		RequestID: issue.RequestID, CreatedAt: issue.Now,
	}); err != nil {
		return nil, err
	}
	updated, err := getRemoteMemoryQuery(ctx, tx, issue.NamespaceUID, issue.ID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return updated, nil
}

// MarkRemoteMemoriesRecalled updates recall telemetry only for eligible, materialized catalog rows.
func (s *Store) MarkRemoteMemoriesRecalled(ctx context.Context, namespaceUID string, ids []string, now time.Time) error {
	namespaceUID = strings.TrimSpace(namespaceUID)
	ids = compactStrings(ids)
	if namespaceUID == "" {
		return store.ValidationErrorf("namespace uid is required")
	}
	if len(ids) == 0 {
		return nil
	}
	now = normalizeMemoryNow(now)
	args := []any{now, namespaceUID}
	for _, id := range ids {
		args = append(args, id)
	}
	_, err := s.db.ExecContext(ctx, `UPDATE remote_memory_catalog
		SET last_recalled_at = ?, recalled_count = recalled_count + 1
		WHERE namespace_uid = ? AND id IN (`+memoryPlaceholders(len(ids))+`)
			AND materialization_state = 'active' AND disabled = FALSE AND deleted = FALSE
			AND content_available = TRUE AND trust IN ('reviewed','trusted')`, args...)
	return err
}

// SetRemoteMemoryDisabled applies the synchronous local suppression overlay.
func (s *Store) SetRemoteMemoryDisabled(ctx context.Context, change store.RemoteMemoryDisabledChange) (*store.RemoteMemoryCatalogEntry, error) {
	change.Now = normalizeMemoryNow(change.Now)
	change.NamespaceUID = strings.TrimSpace(change.NamespaceUID)
	change.ID = strings.TrimSpace(change.ID)
	if change.NamespaceUID == "" || change.ID == "" || change.ExpectedGovernanceRevision <= 0 || strings.TrimSpace(change.Actor) == "" {
		return nil, store.ValidationErrorf("namespace uid, id, expected governance revision, and actor are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	catalog, err := getRemoteMemoryQuery(ctx, tx, change.NamespaceUID, change.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if catalog.GovernanceRevision != change.ExpectedGovernanceRevision {
		return nil, fmt.Errorf("%w: memory governance revision changed", store.ErrConflict)
	}
	if !change.Disabled && (catalog.Deleted || catalog.MaterializationState == store.MemoryMaterializationDiverged ||
		catalog.MaterializationState == store.MemoryMaterializationLost || catalog.MaterializationState == store.MemoryMaterializationOrphaned) {
		return nil, store.ValidationErrorf("memory state %q cannot be enabled", catalog.MaterializationState)
	}
	if catalog.Disabled == change.Disabled {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return catalog, nil
	}
	result, err := tx.ExecContext(ctx, `UPDATE remote_memory_catalog
		SET disabled = ?, governance_revision = governance_revision + 1, updated_at = ?
		WHERE namespace_uid = ? AND id = ? AND governance_revision = ?`,
		change.Disabled, change.Now, change.NamespaceUID, change.ID, change.ExpectedGovernanceRevision)
	if err != nil {
		return nil, err
	}
	if err := ensureMemoryRowsAffectedConflict(result, "memory governance revision changed"); err != nil {
		return nil, err
	}
	newState := "enabled"
	if change.Disabled {
		newState = "disabled"
	}
	if err := insertMemoryAudit(ctx, tx, store.MemoryAuditRecord{
		Namespace: catalog.Namespace, NamespaceUID: catalog.NamespaceUID, Actor: change.Actor,
		Action: "memory.disable", Reason: change.Reason, PreviousState: fmt.Sprintf("disabled=%t", catalog.Disabled),
		NewState: newState, AuthorityEpoch: catalog.AuthorityEpoch, RoutingEpoch: catalog.RoutingEpoch,
		MemoryID: catalog.ID, RequestID: change.RequestID, CreatedAt: change.Now,
	}); err != nil {
		return nil, err
	}
	updated, err := getRemoteMemoryQuery(ctx, tx, change.NamespaceUID, change.ID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return updated, nil
}

// SetRemoteMemoryTrust changes the server-owned trust classification without changing content generation.
func (s *Store) SetRemoteMemoryTrust(ctx context.Context, change store.RemoteMemoryTrustChange) (*store.RemoteMemoryCatalogEntry, error) {
	change.Now = normalizeMemoryNow(change.Now)
	change.NamespaceUID = strings.TrimSpace(change.NamespaceUID)
	change.ID = strings.TrimSpace(change.ID)
	if change.NamespaceUID == "" || change.ID == "" || change.ExpectedGovernanceRevision <= 0 || !validMemoryTrust(change.Trust) || strings.TrimSpace(change.Actor) == "" {
		return nil, store.ValidationErrorf("namespace uid, id, valid trust, expected governance revision, and actor are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	catalog, err := getRemoteMemoryQuery(ctx, tx, change.NamespaceUID, change.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if catalog.GovernanceRevision != change.ExpectedGovernanceRevision {
		return nil, fmt.Errorf("%w: memory governance revision changed", store.ErrConflict)
	}
	if catalog.PendingOperationID != "" || catalog.DesiredGeneration != catalog.Generation {
		return nil, fmt.Errorf("%w: memory content mutation is pending", store.ErrConflict)
	}
	if change.Trust == store.MemoryTrustReviewed {
		if catalog.Trust != store.MemoryTrustReviewed || catalog.Source != memorySourceProposal || catalog.SourceProposalID == "" {
			return nil, store.ValidationErrorf("reviewed trust is reserved for accepted proposal application")
		}
	} else if catalog.Trust == store.MemoryTrustReviewed {
		return nil, store.ValidationErrorf("reviewed proposal trust cannot be changed through the administrative trust endpoint")
	}
	if change.Trust != store.MemoryTrustUntrusted && (catalog.Deleted || catalog.MaterializationState != store.MemoryMaterializationActive) {
		return nil, store.ValidationErrorf("memory state %q cannot be promoted", catalog.MaterializationState)
	}
	if catalog.Trust == change.Trust {
		result, err := tx.ExecContext(ctx, `UPDATE remote_memory_catalog SET trust = trust
			WHERE namespace_uid = ? AND id = ? AND governance_revision = ? AND generation = ?
				AND desired_generation = generation AND pending_operation_id = ''`,
			change.NamespaceUID, change.ID, change.ExpectedGovernanceRevision, catalog.Generation)
		if err != nil {
			return nil, err
		}
		if err := ensureMemoryRowsAffectedConflict(result, "memory content or governance changed"); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return catalog, nil
	}
	result, err := tx.ExecContext(ctx, `UPDATE remote_memory_catalog
		SET trust = ?, governance_revision = governance_revision + 1, updated_at = ?
		WHERE namespace_uid = ? AND id = ? AND governance_revision = ? AND generation = ?
			AND desired_generation = generation AND pending_operation_id = ''`,
		change.Trust, change.Now, change.NamespaceUID, change.ID, change.ExpectedGovernanceRevision, catalog.Generation)
	if err != nil {
		return nil, err
	}
	if err := ensureMemoryRowsAffectedConflict(result, "memory governance revision changed"); err != nil {
		return nil, err
	}
	if err := insertMemoryAudit(ctx, tx, store.MemoryAuditRecord{
		Namespace: catalog.Namespace, NamespaceUID: catalog.NamespaceUID, Actor: change.Actor,
		Action: "memory.trust", Reason: change.Reason, PreviousState: string(catalog.Trust), NewState: string(change.Trust),
		AuthorityEpoch: catalog.AuthorityEpoch, RoutingEpoch: catalog.RoutingEpoch,
		MemoryID: catalog.ID, RequestID: change.RequestID, CreatedAt: change.Now,
	}); err != nil {
		return nil, err
	}
	updated, err := getRemoteMemoryQuery(ctx, tx, change.NamespaceUID, change.ID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return updated, nil
}

// GetMemoryIdempotency returns a compact replay record.
func (s *Store) GetMemoryIdempotency(ctx context.Context, namespaceUID, principal, route, callerKey string) (*store.MemoryIdempotencyRecord, error) {
	record, err := getMemoryIdempotencyQuery(ctx, s.db, strings.TrimSpace(namespaceUID), strings.TrimSpace(principal), strings.TrimSpace(route), strings.TrimSpace(callerKey))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err == nil && !record.ExpiresAt.After(time.Now().UTC()) {
		if record.OperationID == "" {
			return nil, store.ErrNotFound
		}
		operation, operationErr := getMemoryOperationQuery(ctx, s.db, record.NamespaceUID, record.OperationID)
		if errors.Is(operationErr, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		if operationErr != nil {
			return nil, operationErr
		}
		switch operation.State {
		case store.MemoryOperationQueued, store.MemoryOperationLeased, store.MemoryOperationDispatching,
			store.MemoryOperationAmbiguous, store.MemoryOperationDeadLettered:
			return record, nil
		default:
			return nil, store.ErrNotFound
		}
	}
	return record, err
}

// SaveMemorySearchCursor persists bounded opaque provider pagination state.
func (s *Store) SaveMemorySearchCursor(ctx context.Context, cursor store.MemorySearchCursorState) error {
	cursor.ID = strings.TrimSpace(cursor.ID)
	cursor.NamespaceUID = strings.TrimSpace(cursor.NamespaceUID)
	cursor.BindingDigest = strings.TrimSpace(cursor.BindingDigest)
	cursor.QueryDigest = strings.TrimSpace(cursor.QueryDigest)
	cursor.CreatedAt = normalizeMemoryNow(cursor.CreatedAt)
	cursor.ExpiresAt = cursor.ExpiresAt.UTC()
	if cursor.ID == "" || cursor.NamespaceUID == "" || cursor.BindingDigest == "" || cursor.QueryDigest == "" ||
		len(cursor.State) == 0 || len(cursor.State) > store.MaxMemorySearchCursorStateBytes ||
		!cursor.ExpiresAt.After(cursor.CreatedAt) || cursor.ExpiresAt.Sub(cursor.CreatedAt) > 10*time.Minute {
		return store.ValidationErrorf("memory search cursor state is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_search_cursors WHERE expires_at <= ?`, cursor.CreatedAt); err != nil {
		return err
	}
	var existingNamespace, existingBinding, existingQuery string
	var existingState []byte
	err = tx.QueryRowContext(ctx, `SELECT namespace_uid, binding_digest, query_digest, state_json
		FROM memory_search_cursors WHERE id = ?`, cursor.ID).
		Scan(&existingNamespace, &existingBinding, &existingQuery, &existingState)
	if err == nil {
		if existingNamespace == cursor.NamespaceUID && existingBinding == cursor.BindingDigest &&
			existingQuery == cursor.QueryDigest && bytes.Equal(existingState, cursor.State) {
			return tx.Commit()
		}
		return store.ErrConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	limits := governedMemoryQuotas
	var namespaceCount, namespaceBytes, globalCount, globalBytes int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(length(state_json)), 0)
		FROM memory_search_cursors WHERE namespace_uid = ? AND retired_at IS NULL`, cursor.NamespaceUID).
		Scan(&namespaceCount, &namespaceBytes); err != nil {
		return err
	}
	if namespaceCount >= limits.NamespaceSearchCursorRows || namespaceBytes+int64(len(cursor.State)) > limits.NamespaceSearchCursorBytes {
		return fmt.Errorf("%w: namespace memory search cursor capacity reached", store.ErrCapacity)
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(length(state_json)), 0)
		FROM memory_search_cursors WHERE retired_at IS NULL`).Scan(&globalCount, &globalBytes); err != nil {
		return err
	}
	if globalCount >= limits.GlobalSearchCursorRows || globalBytes+int64(len(cursor.State)) > limits.GlobalSearchCursorBytes {
		return fmt.Errorf("%w: global memory search cursor capacity reached", store.ErrCapacity)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO memory_search_cursors
		(id, namespace_uid, binding_digest, query_digest, state_json, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, cursor.ID, cursor.NamespaceUID, cursor.BindingDigest,
		cursor.QueryDigest, append([]byte(nil), cursor.State...), cursor.ExpiresAt, cursor.CreatedAt); err != nil {
		return err
	}
	return tx.Commit()
}

// GetMemorySearchCursor loads one unexpired opaque pagination state.
func (s *Store) GetMemorySearchCursor(
	ctx context.Context,
	namespaceUID, id string,
	now time.Time,
) (*store.MemorySearchCursorState, error) {
	now = normalizeMemoryNow(now)
	var cursor store.MemorySearchCursorState
	err := s.db.QueryRowContext(ctx, `SELECT id, namespace_uid, binding_digest, query_digest, state_json, expires_at, created_at
		FROM memory_search_cursors WHERE namespace_uid = ? AND id = ? AND expires_at > ?`,
		strings.TrimSpace(namespaceUID), strings.TrimSpace(id), now).Scan(
		&cursor.ID, &cursor.NamespaceUID, &cursor.BindingDigest, &cursor.QueryDigest,
		&cursor.State, &cursor.ExpiresAt, &cursor.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	cursor.State = append([]byte(nil), cursor.State...)
	return &cursor, nil
}

// RetireMemorySearchCursor removes one consumed cursor from active admission
// accounting while retaining it until expiry for deterministic replay.
func (s *Store) RetireMemorySearchCursor(
	ctx context.Context,
	namespaceUID, id string,
	now time.Time,
) error {
	namespaceUID = strings.TrimSpace(namespaceUID)
	id = strings.TrimSpace(id)
	now = normalizeMemoryNow(now)
	if namespaceUID == "" || id == "" {
		return store.ValidationErrorf("memory search cursor identity is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_search_cursors WHERE expires_at <= ?`, now); err != nil {
		return err
	}
	var retiredAt sql.NullTime
	var stateBytes int64
	err = tx.QueryRowContext(ctx, `SELECT retired_at, length(state_json)
		FROM memory_search_cursors WHERE namespace_uid = ? AND id = ? AND expires_at > ?`, namespaceUID, id, now).
		Scan(&retiredAt, &stateBytes)
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrNotFound
	}
	if err != nil {
		return err
	}
	if retiredAt.Valid {
		return tx.Commit()
	}
	limits := governedMemoryQuotas
	var namespaceCount, namespaceBytes, globalCount, globalBytes int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(length(state_json)), 0)
		FROM memory_search_cursors WHERE namespace_uid = ? AND retired_at IS NOT NULL`, namespaceUID).
		Scan(&namespaceCount, &namespaceBytes); err != nil {
		return err
	}
	if namespaceCount >= limits.NamespaceSearchReplayRows ||
		namespaceBytes+stateBytes > limits.NamespaceSearchReplayBytes {
		return fmt.Errorf("%w: namespace memory search cursor replay capacity reached", store.ErrCapacity)
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(length(state_json)), 0)
		FROM memory_search_cursors WHERE retired_at IS NOT NULL`).Scan(&globalCount, &globalBytes); err != nil {
		return err
	}
	if globalCount >= limits.GlobalSearchReplayRows || globalBytes+stateBytes > limits.GlobalSearchReplayBytes {
		return fmt.Errorf("%w: global memory search cursor replay capacity reached", store.ErrCapacity)
	}
	result, err := tx.ExecContext(ctx, `UPDATE memory_search_cursors SET retired_at = ?
		WHERE namespace_uid = ? AND id = ? AND retired_at IS NULL AND expires_at > ?`, now, namespaceUID, id, now)
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated != 1 {
		return store.ErrConflict
	}
	return tx.Commit()
}

// AppendMemoryAudit commits an insert-only administrative intent or completion record.
func (s *Store) AppendMemoryAudit(ctx context.Context, audit store.MemoryAuditRecord) error {
	audit.Namespace = strings.TrimSpace(audit.Namespace)
	audit.NamespaceUID = strings.TrimSpace(audit.NamespaceUID)
	audit.Actor = strings.TrimSpace(audit.Actor)
	audit.Action = strings.TrimSpace(audit.Action)
	if audit.Namespace == "" || audit.NamespaceUID == "" || audit.Actor == "" || audit.Action == "" {
		return store.ValidationErrorf("memory audit namespace, namespace uid, actor, and action are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := insertMemoryAudit(ctx, tx, audit); err != nil {
		if isSQLiteConstraintError(err) {
			return fmt.Errorf("%w: memory audit id already exists", store.ErrConflict)
		}
		return err
	}
	return tx.Commit()
}

// ListMemoryAudit lists immutable safe audit records.
func (s *Store) ListMemoryAudit(ctx context.Context, filter store.MemoryAuditFilter) ([]store.MemoryAuditRecord, error) {
	filter.NamespaceUID = strings.TrimSpace(filter.NamespaceUID)
	if filter.NamespaceUID == "" {
		return nil, store.ValidationErrorf("namespace uid is required")
	}
	if (filter.BeforeCreatedAt == nil) != (strings.TrimSpace(filter.BeforeID) == "") {
		return nil, store.ValidationErrorf("audit pagination requires both before created-at and before id")
	}
	query := selectMemoryAuditSQL() + ` WHERE namespace_uid = ?`
	args := []any{filter.NamespaceUID}
	if filter.MemoryID != "" {
		query += ` AND memory_id = ?`
		args = append(args, filter.MemoryID)
	}
	if filter.OperationID != "" {
		query += ` AND operation_id = ?`
		args = append(args, filter.OperationID)
	}
	if filter.ProposalID != "" {
		query += ` AND proposal_id = ?`
		args = append(args, filter.ProposalID)
	}
	if filter.BeforeCreatedAt != nil {
		query += ` AND (created_at < ? OR (created_at = ? AND id < ?))`
		args = append(args, filter.BeforeCreatedAt.UTC(), filter.BeforeCreatedAt.UTC(), filter.BeforeID)
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, boundedLimit(filter.Limit, defaultGovernedMemoryLimit, maxGovernedMemoryLimit))
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var records []store.MemoryAuditRecord
	for rows.Next() {
		record, err := scanMemoryAudit(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, *record)
	}
	return records, rows.Err()
}

func selectRemoteMemorySQL() string {
	return `SELECT namespace_uid, id, namespace_name, cluster_id, backend_uid, authority_epoch, routing_epoch,
		tenant_id, store_uuid, backend_memory_id, backend_version, generation, desired_generation,
		governance_revision, materialization_state, disabled, deleted, trust, session_name, agent_name,
		task_name, parent_task, source, source_proposal_id, tags_json, content_digest, content_available,
		pending_operation_id, created_at, updated_at, last_recalled_at, recalled_count FROM remote_memory_catalog`
}

func getRemoteMemoryQuery(ctx context.Context, q rowQueryer, namespaceUID, id string) (*store.RemoteMemoryCatalogEntry, error) {
	return scanRemoteMemory(q.QueryRowContext(ctx, selectRemoteMemorySQL()+` WHERE namespace_uid = ? AND id = ?`, namespaceUID, id))
}

func getRemoteMemoryGenerationWatermarkQuery(
	ctx context.Context,
	q rowQueryer,
	namespaceUID, id, backendUID string,
	authorityEpoch int64,
	storeUUID string,
) (int64, error) {
	var generation int64
	err := q.QueryRowContext(ctx, `SELECT generation FROM remote_memory_generation_watermarks
		WHERE namespace_uid = ? AND id = ? AND backend_uid = ? AND authority_epoch = ? AND store_uuid = ?`,
		namespaceUID, id, backendUID, authorityEpoch, storeUUID).Scan(&generation)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if generation <= 0 {
		return 0, store.ValidationErrorf("stored memory generation watermark is invalid")
	}
	return generation, nil
}

func scanRemoteMemory(scanner rowScanner) (*store.RemoteMemoryCatalogEntry, error) {
	var catalog store.RemoteMemoryCatalogEntry
	var tagsJSON string
	var lastRecalledAt sql.NullTime
	err := scanner.Scan(
		&catalog.NamespaceUID, &catalog.ID, &catalog.Namespace, &catalog.ClusterID, &catalog.BackendUID,
		&catalog.AuthorityEpoch, &catalog.RoutingEpoch, &catalog.TenantID, &catalog.StoreUUID,
		&catalog.BackendMemoryID, &catalog.BackendVersion, &catalog.Generation, &catalog.DesiredGeneration,
		&catalog.GovernanceRevision, &catalog.MaterializationState, &catalog.Disabled, &catalog.Deleted, &catalog.Trust,
		&catalog.SessionName, &catalog.AgentName, &catalog.TaskName, &catalog.ParentTask, &catalog.Source,
		&catalog.SourceProposalID, &tagsJSON, &catalog.ContentDigest, &catalog.ContentAvailable,
		&catalog.PendingOperationID, &catalog.CreatedAt, &catalog.UpdatedAt, &lastRecalledAt, &catalog.RecalledCount,
	)
	if err != nil {
		return nil, err
	}
	if lastRecalledAt.Valid {
		catalog.LastRecalledAt = &lastRecalledAt.Time
	}
	if tagsJSON != "" {
		if err := jsonUnmarshalTags(tagsJSON, &catalog.Tags); err != nil {
			return nil, err
		}
	}
	return &catalog, nil
}

func jsonUnmarshalTags(tagsJSON string, tags *[]string) error {
	if err := json.Unmarshal([]byte(tagsJSON), tags); err != nil {
		return fmt.Errorf("failed to unmarshal memory tags: %w", err)
	}
	return nil
}

func selectMemoryAuditSQL() string {
	return `SELECT id, namespace_name, namespace_uid, actor, action, reason, previous_state, new_state,
		authority_epoch, previous_routing_epoch, routing_epoch, memory_id, operation_id, proposal_id,
		request_digest, mutation_digest, content_digest, request_id, created_at FROM memory_audit`
}

func scanMemoryAudit(scanner rowScanner) (*store.MemoryAuditRecord, error) {
	var audit store.MemoryAuditRecord
	if err := scanner.Scan(&audit.ID, &audit.Namespace, &audit.NamespaceUID, &audit.Actor, &audit.Action,
		&audit.Reason, &audit.PreviousState, &audit.NewState, &audit.AuthorityEpoch, &audit.PreviousRoutingEpoch,
		&audit.RoutingEpoch, &audit.MemoryID, &audit.OperationID, &audit.ProposalID, &audit.RequestDigest,
		&audit.MutationDigest, &audit.ContentDigest, &audit.RequestID, &audit.CreatedAt); err != nil {
		return nil, err
	}
	return &audit, nil
}

// GetMemoryOperation returns a retention-safe operation summary.
func (s *Store) GetMemoryOperation(ctx context.Context, namespaceUID, id string) (*store.MemoryOperation, error) {
	operation, err := getMemoryOperationQuery(ctx, s.db, strings.TrimSpace(namespaceUID), strings.TrimSpace(id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	return operation, err
}

// ListMemoryOperations lists operation summaries without loading canonical payloads.
func (s *Store) ListMemoryOperations(ctx context.Context, filter store.MemoryOperationFilter) ([]store.MemoryOperation, error) {
	filter.NamespaceUID = strings.TrimSpace(filter.NamespaceUID)
	if filter.NamespaceUID == "" {
		return nil, store.ValidationErrorf("namespace uid is required")
	}
	if (filter.BeforeCreatedAt == nil) != (filter.BeforeSequence == 0) {
		return nil, store.ValidationErrorf("operation pagination requires both before created-at and before sequence")
	}
	query := selectMemoryOperationSQL() + ` WHERE namespace_uid = ?`
	args := []any{filter.NamespaceUID}
	if filter.MemoryID != "" {
		query += ` AND memory_id = ?`
		args = append(args, filter.MemoryID)
	}
	if filter.ProposalID != "" {
		query += ` AND proposal_id = ?`
		args = append(args, filter.ProposalID)
	}
	if len(filter.Kinds) > 0 {
		query += ` AND kind IN (` + memoryPlaceholders(len(filter.Kinds)) + `)`
		for _, kind := range filter.Kinds {
			if !validMemoryOperationKind(kind) {
				return nil, store.ValidationErrorf("invalid memory operation kind %q", kind)
			}
			args = append(args, kind)
		}
	}
	if len(filter.States) > 0 {
		query += ` AND state IN (` + memoryPlaceholders(len(filter.States)) + `)`
		for _, state := range filter.States {
			if !validMemoryOperationState(state) {
				return nil, store.ValidationErrorf("invalid memory operation state %q", state)
			}
			args = append(args, state)
		}
	}
	if filter.BeforeCreatedAt != nil {
		query += ` AND (created_at < ? OR (created_at = ? AND sequence < ?))`
		args = append(args, filter.BeforeCreatedAt.UTC(), filter.BeforeCreatedAt.UTC(), filter.BeforeSequence)
	}
	query += ` ORDER BY created_at DESC, sequence DESC LIMIT ?`
	args = append(args, boundedLimit(filter.Limit, defaultGovernedMemoryLimit, maxGovernedMemoryLimit))
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var operations []store.MemoryOperation
	for rows.Next() {
		operation, err := scanMemoryOperation(rows)
		if err != nil {
			return nil, err
		}
		operations = append(operations, *operation)
	}
	return operations, rows.Err()
}

// ClaimNextMemoryOperation recovers expired leases and leases one due operation.
func (s *Store) ClaimNextMemoryOperation(ctx context.Context, claim store.MemoryOperationClaim) (*store.MemoryOperationDispatch, error) {
	return s.claimMemoryOperation(ctx, "", claim)
}

// ClaimMemoryOperation recovers maintenance state and claims one exact due operation ID.
func (s *Store) ClaimMemoryOperation(ctx context.Context, id string, claim store.MemoryOperationClaim) (*store.MemoryOperationDispatch, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, store.ValidationErrorf("memory operation id is required")
	}
	return s.claimMemoryOperation(ctx, id, claim)
}

//nolint:gocyclo // Claiming combines lease recovery, expiry maintenance, exact/fair selection, and payload checks.
func (s *Store) claimMemoryOperation(ctx context.Context, exactID string, claim store.MemoryOperationClaim) (*store.MemoryOperationDispatch, error) {
	claim.Now = normalizeMemoryNow(claim.Now)
	claim.NamespaceUID = strings.TrimSpace(claim.NamespaceUID)
	claim.BackendUID = strings.TrimSpace(claim.BackendUID)
	claim.LeaseOwner = strings.TrimSpace(claim.LeaseOwner)
	if claim.NamespaceUID == "" || claim.BackendUID == "" || claim.AuthorityEpoch <= 0 || claim.RoutingEpoch <= 0 || claim.LeaseOwner == "" || claim.LeaseDuration <= 0 {
		return nil, store.ValidationErrorf("complete binding identity, lease owner, and positive lease duration are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	binding, err := getMemoryBackendBindingQuery(ctx, tx, claim.NamespaceUID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	stateAllowed := binding.State == store.MemoryBackendBindingAccepting || binding.State == store.MemoryBackendBindingDraining
	validationFresh := binding.ValidationExpiresAt.After(claim.Now)
	if !stateAllowed || binding.BackendUID != claim.BackendUID || binding.AuthorityEpoch != claim.AuthorityEpoch || binding.RoutingEpoch != claim.RoutingEpoch ||
		(!validationFresh && !claim.AllowExpiredValidationMaintenance) {
		return nil, fmt.Errorf("%w: dispatcher binding identity is stale or egress is disabled", store.ErrConflict)
	}
	if err := recoverExpiredMemoryOperationLeases(ctx, tx, claim, binding.Namespace); err != nil {
		return nil, err
	}
	if err := deadLetterExpiredMemoryOperations(ctx, tx, claim, binding.Namespace); err != nil {
		return nil, err
	}
	if !validationFresh {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return nil, store.ErrNotReady
	}

	var operationID string
	claimQuery := `SELECT id FROM memory_operations
		WHERE namespace_uid = ? AND backend_uid = ? AND authority_epoch = ? AND routing_epoch = ?
			AND state IN ('queued','ambiguous') AND next_retry_at <= ? AND max_age_at > ?`
	claimArgs := []any{claim.NamespaceUID, claim.BackendUID, claim.AuthorityEpoch, claim.RoutingEpoch, claim.Now, claim.Now}
	if exactID != "" {
		claimQuery += ` AND id = ?`
		claimArgs = append(claimArgs, exactID)
	}
	claimQuery += ` ORDER BY next_retry_at, sequence LIMIT 1`
	err = tx.QueryRowContext(ctx, claimQuery, claimArgs...).Scan(&operationID)
	if errors.Is(err, sql.ErrNoRows) {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	current, err := getMemoryOperationQuery(ctx, tx, claim.NamespaceUID, operationID)
	if err != nil {
		return nil, err
	}
	leaseExpiresAt := claim.Now.Add(claim.LeaseDuration)
	if operationMaxAge := current.MaxAgeAt.UTC(); leaseExpiresAt.After(operationMaxAge) {
		leaseExpiresAt = operationMaxAge
	}
	if validationExpiresAt := binding.ValidationExpiresAt.UTC(); leaseExpiresAt.After(validationExpiresAt) {
		leaseExpiresAt = validationExpiresAt
	}
	result, err := tx.ExecContext(ctx, `UPDATE memory_operations
		SET state = ?, lease_owner = ?, lease_epoch = lease_epoch + 1, lease_origin_state = ?, lease_expires_at = ?, updated_at = ?
		WHERE namespace_uid = ? AND id = ? AND state = ? AND next_retry_at <= ? AND max_age_at > ?`,
		store.MemoryOperationLeased, claim.LeaseOwner, current.State, leaseExpiresAt, claim.Now,
		claim.NamespaceUID, operationID, current.State, claim.Now, claim.Now)
	if err != nil {
		return nil, err
	}
	if err := ensureMemoryRowsAffectedConflict(result, "memory operation was claimed concurrently"); err != nil {
		return nil, err
	}
	dispatch, err := getMemoryOperationDispatchQuery(ctx, tx, claim.NamespaceUID, operationID)
	if err != nil {
		return nil, err
	}
	if err := insertMemoryAudit(ctx, tx, store.MemoryAuditRecord{
		Namespace: dispatch.Operation.Namespace, NamespaceUID: claim.NamespaceUID, Actor: claim.LeaseOwner,
		Action: "memory.operation.claim", PreviousState: string(current.State), NewState: string(store.MemoryOperationLeased),
		AuthorityEpoch: claim.AuthorityEpoch, RoutingEpoch: claim.RoutingEpoch, MemoryID: dispatch.Operation.MemoryID,
		OperationID: dispatch.Operation.ID, ProposalID: dispatch.Operation.ProposalID,
		MutationDigest: dispatch.Operation.MutationDigest, ContentDigest: dispatch.Operation.ContentDigest, CreatedAt: claim.Now,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return dispatch, nil
}

func requireCurrentMemoryBindingForDispatch(ctx context.Context, q rowQueryer, namespaceUID, backendUID string, authorityEpoch, routingEpoch int64, now time.Time) (*store.MemoryBackendBinding, error) {
	binding, err := getMemoryBackendBindingQuery(ctx, q, namespaceUID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	stateAllowed := binding.State == store.MemoryBackendBindingAccepting || binding.State == store.MemoryBackendBindingDraining
	if binding.Mode != store.MemoryBackendModeRemote || !stateAllowed ||
		binding.ResolvedAddressDigest == "" || binding.ServerCertificateDigest == "" ||
		binding.BackendUID != backendUID || binding.AuthorityEpoch != authorityEpoch || binding.RoutingEpoch != routingEpoch ||
		!binding.ValidationExpiresAt.After(now) {
		return nil, fmt.Errorf("%w: dispatcher binding is stale, expired, or not accepting", store.ErrConflict)
	}
	return binding, nil
}

// MarkMemoryOperationSendStarted commits the durable network send boundary.
//
//nolint:gocyclo // Send-boundary validation intentionally checks every lease and binding fence.
func (s *Store) MarkMemoryOperationSendStarted(ctx context.Context, send store.MemoryOperationSend) (*store.MemoryOperation, error) {
	send.Now = normalizeMemoryNow(send.Now)
	send.NamespaceUID = strings.TrimSpace(send.NamespaceUID)
	send.ID = strings.TrimSpace(send.ID)
	send.BackendUID = strings.TrimSpace(send.BackendUID)
	send.LeaseOwner = strings.TrimSpace(send.LeaseOwner)
	if send.NamespaceUID == "" || send.ID == "" || send.BackendUID == "" || send.AuthorityEpoch <= 0 || send.RoutingEpoch <= 0 || send.LeaseOwner == "" || send.LeaseEpoch <= 0 {
		return nil, store.ValidationErrorf("complete operation lease and binding identity is required")
	}
	if send.RequestDeadline.IsZero() || !send.RequestDeadline.After(send.Now) {
		return nil, store.ValidationErrorf("request deadline must be after send time")
	}
	send.RequestDeadline = send.RequestDeadline.UTC()
	if send.RequestDeadline.Sub(send.Now) > 60*time.Second {
		send.RequestDeadline = send.Now.Add(60 * time.Second)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	operation, err := getMemoryOperationQuery(ctx, tx, send.NamespaceUID, send.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	binding, err := requireCurrentMemoryBindingForDispatch(ctx, tx, send.NamespaceUID, send.BackendUID, send.AuthorityEpoch, send.RoutingEpoch, send.Now)
	if err != nil {
		return nil, err
	}
	if operation.State != store.MemoryOperationLeased || operation.BackendUID != send.BackendUID ||
		operation.AuthorityEpoch != send.AuthorityEpoch || operation.RoutingEpoch != send.RoutingEpoch ||
		operation.LeaseOwner != send.LeaseOwner || operation.LeaseEpoch != send.LeaseEpoch ||
		operation.LeaseExpiresAt == nil || !operation.LeaseExpiresAt.After(send.Now) {
		return nil, fmt.Errorf("%w: memory operation lease changed or expired before send", store.ErrConflict)
	}
	if !send.Now.Before(operation.MaxAgeAt) {
		return nil, fmt.Errorf("%w: memory operation reached its maximum age before send", store.ErrConflict)
	}
	if send.RequestDeadline.After(operation.MaxAgeAt) {
		send.RequestDeadline = operation.MaxAgeAt.UTC()
	}
	if send.RequestDeadline.After(binding.ValidationExpiresAt) {
		send.RequestDeadline = binding.ValidationExpiresAt.UTC()
	}
	if operation.LeaseExpiresAt != nil && !send.RequestDeadline.Before(operation.LeaseExpiresAt.UTC()) {
		send.RequestDeadline = operation.LeaseExpiresAt.UTC().Add(-time.Nanosecond)
	}
	if !send.RequestDeadline.After(send.Now) || operation.LeaseExpiresAt == nil || !send.RequestDeadline.Before(operation.LeaseExpiresAt.UTC()) {
		return nil, fmt.Errorf("%w: request deadline must remain before the existing lease expiry", store.ErrConflict)
	}
	result, err := tx.ExecContext(ctx, `UPDATE memory_operations
		SET state = ?, send_started_at = COALESCE(send_started_at, ?), request_deadline = ?, dispatches = dispatches + 1, updated_at = ?
		WHERE namespace_uid = ? AND id = ? AND state = ? AND backend_uid = ? AND authority_epoch = ? AND routing_epoch = ?
			AND lease_owner = ? AND lease_epoch = ? AND lease_expires_at > ?`,
		store.MemoryOperationDispatching, send.Now, send.RequestDeadline, send.Now,
		send.NamespaceUID, send.ID, store.MemoryOperationLeased, send.BackendUID, send.AuthorityEpoch,
		send.RoutingEpoch, send.LeaseOwner, send.LeaseEpoch, send.RequestDeadline)
	if err != nil {
		return nil, err
	}
	if err := ensureMemoryRowsAffectedConflict(result, "memory operation lease changed before send"); err != nil {
		return nil, err
	}
	if err := insertMemoryAudit(ctx, tx, store.MemoryAuditRecord{
		Namespace: operation.Namespace, NamespaceUID: operation.NamespaceUID, Actor: send.LeaseOwner,
		Action: "memory.operation.send_started", PreviousState: string(operation.State), NewState: string(store.MemoryOperationDispatching),
		AuthorityEpoch: operation.AuthorityEpoch, RoutingEpoch: operation.RoutingEpoch, MemoryID: operation.MemoryID,
		OperationID: operation.ID, ProposalID: operation.ProposalID, MutationDigest: operation.MutationDigest,
		ContentDigest: operation.ContentDigest, CreatedAt: send.Now,
	}); err != nil {
		return nil, err
	}
	updated, err := getMemoryOperationQuery(ctx, tx, send.NamespaceUID, send.ID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return updated, nil
}

func memoryBackendBindingDigest(binding *store.MemoryBackendBinding) string {
	if binding == nil {
		return ""
	}
	return omsprotocol.BindingDigest(omsprotocol.Binding{
		ClusterID: binding.ClusterID, NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		AuthorityEpoch: uint64(binding.AuthorityEpoch), RoutingEpoch: uint64(binding.RoutingEpoch),
		TenantID: binding.TenantID, StoreUUID: binding.StoreUUID,
	})
}

// CompleteMemoryOperation commits a strict receipt and advances materialized state using operation/lease/generation CASes.
//
//nolint:gocyclo // Completion validates every durable fence before atomically advancing catalog, operation, proposal, and replay state.
func (s *Store) CompleteMemoryOperation(ctx context.Context, completion store.MemoryOperationCompletion) (*store.MemoryOperation, error) {
	completion.NamespaceUID = strings.TrimSpace(completion.NamespaceUID)
	completion.ID = strings.TrimSpace(completion.ID)
	completion.BackendUID = strings.TrimSpace(completion.BackendUID)
	completion.LeaseOwner = strings.TrimSpace(completion.LeaseOwner)
	completion.Receipt.BackendVersion = strings.TrimSpace(completion.Receipt.BackendVersion)
	completion.Receipt.BackendMemoryID = strings.TrimSpace(completion.Receipt.BackendMemoryID)
	completion.Now = normalizeMemoryNow(completion.Now)
	if completion.Receipt.CompletedAt.IsZero() {
		return nil, store.ValidationErrorf("receipt completion time is required")
	}
	completion.Receipt.CompletedAt = completion.Receipt.CompletedAt.UTC()
	if completion.NamespaceUID == "" || completion.ID == "" || completion.BackendUID == "" || completion.LeaseOwner == "" ||
		completion.AuthorityEpoch <= 0 || completion.RoutingEpoch <= 0 || completion.LeaseEpoch <= 0 {
		return nil, store.ValidationErrorf("complete operation lease and binding identity is required")
	}
	if completion.Receipt.AppliedGeneration <= 0 || strings.TrimSpace(completion.Receipt.BindingIdentityDigest) == "" || strings.TrimSpace(completion.Receipt.MutationDigest) == "" ||
		completion.Receipt.BackendVersion == "" || len(completion.Receipt.BackendVersion) > maxMemoryReceiptIdentityBytes ||
		completion.Receipt.BackendMemoryID == "" || len(completion.Receipt.BackendMemoryID) > maxMemoryReceiptIdentityBytes {
		return nil, store.ValidationErrorf("strict completion receipt with bounded backend identity is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	operation, err := getMemoryOperationQuery(ctx, tx, completion.NamespaceUID, completion.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	binding, err := requireCurrentMemoryBindingForDispatch(ctx, tx, completion.NamespaceUID, completion.BackendUID,
		completion.AuthorityEpoch, completion.RoutingEpoch, completion.Now)
	if err != nil {
		return nil, err
	}
	if operation.State != store.MemoryOperationDispatching || operation.BackendUID != completion.BackendUID ||
		operation.AuthorityEpoch != completion.AuthorityEpoch || operation.RoutingEpoch != completion.RoutingEpoch ||
		operation.LeaseOwner != completion.LeaseOwner || operation.LeaseEpoch != completion.LeaseEpoch {
		return nil, fmt.Errorf("%w: stale memory operation completion", store.ErrConflict)
	}
	if operation.LeaseExpiresAt == nil || !operation.LeaseExpiresAt.After(completion.Now) ||
		operation.RequestDeadline == nil || !operation.RequestDeadline.After(completion.Now) {
		return nil, fmt.Errorf("%w: memory operation dispatch lease or request deadline expired before completion", store.ErrConflict)
	}
	if completion.Receipt.AppliedGeneration != operation.DesiredGeneration ||
		completion.Receipt.MutationDigest != operation.MutationDigest ||
		completion.Receipt.BindingIdentityDigest != memoryBackendBindingDigest(binding) {
		return nil, fmt.Errorf("%w: completion receipt does not match the admitted operation or binding", store.ErrConflict)
	}
	if completion.Receipt.ContentDigest != operation.ContentDigest {
		return nil, fmt.Errorf("%w: completion content digest does not match the admitted operation", store.ErrConflict)
	}
	catalog, err := getRemoteMemoryQuery(ctx, tx, completion.NamespaceUID, operation.MemoryID)
	if err != nil {
		return nil, err
	}
	if catalog.PendingOperationID != operation.ID || catalog.Generation != operation.ExpectedMaterializedGeneration ||
		catalog.BackendUID != operation.BackendUID || catalog.AuthorityEpoch != operation.AuthorityEpoch || catalog.RoutingEpoch != operation.RoutingEpoch {
		return nil, fmt.Errorf("%w: catalog generation or binding changed before completion", store.ErrConflict)
	}
	if operation.Kind == store.MemoryOperationDelete {
		if !catalog.Deleted {
			return nil, fmt.Errorf("%w: catalog delete suppression is missing before completion", store.ErrConflict)
		}
	} else {
		expectedState := store.MemoryMaterializationActive
		if operation.Kind == store.MemoryOperationCreate {
			expectedState = store.MemoryMaterializationPending
		}
		if catalog.MaterializationState != expectedState {
			return nil, fmt.Errorf("%w: catalog materialization state %q blocks completion", store.ErrConflict, catalog.MaterializationState)
		}
	}
	if err := completeRemoteCatalogOperation(ctx, tx, catalog, operation, completion); err != nil {
		return nil, err
	}
	retainUntil := completion.Now.Add(defaultOperationRecoveryWindow)
	result, err := tx.ExecContext(ctx, `UPDATE memory_operations
		SET state = ?, lease_owner = '', lease_origin_state = '', lease_expires_at = NULL, request_deadline = NULL,
			receipt_binding_digest = ?, receipt_applied_generation = ?, receipt_backend_version = ?,
			receipt_backend_memory_id = ?, receipt_content_digest = ?, receipt_mutation_digest = ?,
			receipt_completed_at = ?, payload_retain_until = CASE WHEN payload_retain_until > ? THEN payload_retain_until ELSE ? END,
			updated_at = ?, completed_at = ?, error_code = '', error_message = ''
		WHERE namespace_uid = ? AND id = ? AND state = ? AND lease_owner = ? AND lease_epoch = ?
			AND backend_uid = ? AND authority_epoch = ? AND routing_epoch = ?
			AND lease_expires_at IS NOT NULL AND lease_expires_at > ?
			AND request_deadline IS NOT NULL AND request_deadline > ?`,
		store.MemoryOperationSucceeded, completion.Receipt.BindingIdentityDigest, completion.Receipt.AppliedGeneration,
		completion.Receipt.BackendVersion, completion.Receipt.BackendMemoryID, completion.Receipt.ContentDigest,
		completion.Receipt.MutationDigest, completion.Receipt.CompletedAt, retainUntil, retainUntil,
		completion.Now, completion.Now, completion.NamespaceUID, completion.ID,
		store.MemoryOperationDispatching, completion.LeaseOwner, completion.LeaseEpoch, completion.BackendUID,
		completion.AuthorityEpoch, completion.RoutingEpoch, completion.Now, completion.Now)
	if err != nil {
		return nil, err
	}
	if err := ensureMemoryRowsAffectedConflict(result, "stale memory operation completion"); err != nil {
		return nil, err
	}
	if operation.ProposalID != "" {
		result, err := tx.ExecContext(ctx, `UPDATE memory_proposals
			SET status = ?, applied_memory_id = ?, applied_at = ?, updated_at = ?
			WHERE namespace = ? AND id = ? AND status = ? AND apply_operation_id = ? AND applied_memory_id = ''`,
			proposalStatusApplied, operation.MemoryID, completion.Now, completion.Now,
			operation.Namespace, operation.ProposalID, proposalStatusApplying, operation.ID)
		if err != nil {
			return nil, err
		}
		if err := ensureMemoryRowsAffectedConflict(result, "proposal changed before operation completion"); err != nil {
			return nil, err
		}
	}
	if err := completeMemoryIdempotencyOutcome(ctx, tx, operation, completion); err != nil {
		return nil, err
	}
	if err := insertMemoryAudit(ctx, tx, store.MemoryAuditRecord{
		Namespace: operation.Namespace, NamespaceUID: operation.NamespaceUID, Actor: completion.Actor,
		Action: "memory.operation.complete", Reason: completion.Reason, PreviousState: string(operation.State),
		NewState: string(store.MemoryOperationSucceeded), AuthorityEpoch: operation.AuthorityEpoch,
		RoutingEpoch: operation.RoutingEpoch, MemoryID: operation.MemoryID, OperationID: operation.ID,
		ProposalID: operation.ProposalID, MutationDigest: operation.MutationDigest,
		ContentDigest: completion.Receipt.ContentDigest, RequestID: completion.RequestID, CreatedAt: completion.Now,
	}); err != nil {
		return nil, err
	}
	updated, err := getMemoryOperationQuery(ctx, tx, completion.NamespaceUID, completion.ID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return updated, nil
}

func completeMemoryIdempotencyOutcome(ctx context.Context, tx *sql.Tx, operation *store.MemoryOperation, completion store.MemoryOperationCompletion) error {
	minimumExpiry := completion.Now.Add(minimumIdempotencyRetention)
	if !completion.FinalizeIdempotencyOutcome {
		result, err := tx.ExecContext(ctx, `UPDATE memory_idempotency SET terminal_binding_digest = ?,
			expires_at = CASE WHEN expires_at > ? THEN expires_at ELSE ? END, updated_at = ?
			WHERE namespace_uid = ? AND operation_id = ? AND authority_epoch = ? AND routing_epoch = ?`,
			completion.Receipt.BindingIdentityDigest, minimumExpiry, minimumExpiry, completion.Now, completion.NamespaceUID,
			completion.ID, completion.AuthorityEpoch, completion.RoutingEpoch)
		if err != nil {
			return err
		}
		return ensureMemoryRowsAffectedConflict(result, "memory idempotency record is missing or stale")
	}
	outcome := completion.IdempotencyOutcome
	expectedStatus := 200
	expectedType := store.MemoryIdempotencyMemory
	switch operation.Kind {
	case store.MemoryOperationCreate:
		if operation.ProposalID == "" {
			expectedStatus = 201
		}
	case store.MemoryOperationReplace:
		expectedStatus = 200
	case store.MemoryOperationDelete:
		expectedStatus = 204
		expectedType = store.MemoryIdempotencyEmpty
	default:
		return store.ValidationErrorf("unsupported operation kind %q", operation.Kind)
	}
	if outcome.Status != expectedStatus || outcome.ResponseType != expectedType || outcome.RetryAfterSeconds != 0 {
		return store.ValidationErrorf("immediate idempotency outcome must be status %d, response type %q, and no retry-after", expectedStatus, expectedType)
	}
	if strings.ContainsAny(outcome.Location, "\r\n") || len(outcome.Location) > maxMemoryAuditTextBytes || len(outcome.ResponseDigest) > 256 {
		return store.ValidationErrorf("idempotency outcome metadata is invalid or too large")
	}
	if expectedType == store.MemoryIdempotencyMemory && strings.TrimSpace(outcome.ResponseDigest) == "" {
		return store.ValidationErrorf("immediate memory response digest is required")
	}
	updatedOperation, err := getMemoryOperationQuery(ctx, tx, operation.NamespaceUID, operation.ID)
	if err != nil {
		return err
	}
	updatedCatalog, err := getRemoteMemoryQuery(ctx, tx, operation.NamespaceUID, operation.MemoryID)
	if err != nil {
		return err
	}
	var payload []byte
	if err := tx.QueryRowContext(ctx, `SELECT payload FROM memory_operations
		WHERE namespace_uid = ? AND id = ?`, operation.NamespaceUID, operation.ID).Scan(&payload); err != nil {
		return err
	}
	payload, err = decodeDurableMemoryOperationPayload(payload)
	if err != nil {
		return err
	}
	responseSnapshot, err := encodeMemoryIdempotencySnapshot(updatedCatalog, updatedOperation, payload)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE memory_idempotency
		SET original_status = ?, response_type = ?, location = ?, retry_after_seconds = ?, response_digest = ?, response_snapshot = ?,
			terminal_binding_digest = ?, expires_at = CASE WHEN expires_at > ? THEN expires_at ELSE ? END, updated_at = ?
		WHERE namespace_uid = ? AND operation_id = ? AND authority_epoch = ? AND routing_epoch = ?`,
		outcome.Status, outcome.ResponseType, outcome.Location, outcome.RetryAfterSeconds, outcome.ResponseDigest, responseSnapshot,
		completion.Receipt.BindingIdentityDigest, minimumExpiry, minimumExpiry, completion.Now, completion.NamespaceUID,
		completion.ID, completion.AuthorityEpoch, completion.RoutingEpoch)
	if err != nil {
		return err
	}
	return ensureMemoryRowsAffectedConflict(result, "memory idempotency record is missing or stale")
}

func completeRemoteCatalogOperation(ctx context.Context, tx *sql.Tx, catalog *store.RemoteMemoryCatalogEntry, operation *store.MemoryOperation, completion store.MemoryOperationCompletion) error {
	var result sql.Result
	var err error
	switch operation.Kind {
	case store.MemoryOperationDelete:
		result, err = tx.ExecContext(ctx, `UPDATE remote_memory_catalog
			SET generation = ?, desired_generation = ?, backend_memory_id = ?, backend_version = ?,
				materialization_state = ?, disabled = TRUE, deleted = TRUE, content_available = FALSE,
				pending_session_name = '', pending_agent_name = '', pending_task_name = '', pending_parent_task = '',
				pending_source = '', pending_tags_json = '[]', pending_operation_id = '', updated_at = ?
			WHERE namespace_uid = ? AND id = ? AND generation = ? AND desired_generation = ? AND pending_operation_id = ?
				AND backend_uid = ? AND authority_epoch = ? AND routing_epoch = ?`,
			operation.DesiredGeneration, operation.DesiredGeneration, completion.Receipt.BackendMemoryID,
			completion.Receipt.BackendVersion, store.MemoryMaterializationDeleted, completion.Now,
			catalog.NamespaceUID, catalog.ID, operation.ExpectedMaterializedGeneration, operation.DesiredGeneration,
			operation.ID, operation.BackendUID, operation.AuthorityEpoch, operation.RoutingEpoch)
	case store.MemoryOperationReplace:
		provenanceChanged := operation.ContentDigest != catalog.ContentDigest
		if !provenanceChanged {
			provenanceChanged, err = remoteReplacementProvenanceChanged(ctx, tx, catalog, operation)
			if err != nil {
				return err
			}
		}
		if provenanceChanged {
			result, err = tx.ExecContext(ctx, `UPDATE remote_memory_catalog
				SET generation = ?, desired_generation = ?, backend_memory_id = ?, backend_version = ?,
					materialization_state = ?, content_digest = ?, content_available = TRUE,
					session_name = pending_session_name, agent_name = pending_agent_name,
					task_name = pending_task_name, parent_task = pending_parent_task,
					source = CASE WHEN pending_source = ? THEN ? ELSE pending_source END,
					source_proposal_id = '', tags_json = pending_tags_json, trust = ?,
					governance_revision = governance_revision + 1,
					pending_session_name = '', pending_agent_name = '', pending_task_name = '', pending_parent_task = '',
					pending_source = '', pending_tags_json = '[]', pending_operation_id = '', updated_at = ?
				WHERE namespace_uid = ? AND id = ? AND generation = ? AND desired_generation = ? AND pending_operation_id = ?
					AND backend_uid = ? AND authority_epoch = ? AND routing_epoch = ?`,
				operation.DesiredGeneration, operation.DesiredGeneration, completion.Receipt.BackendMemoryID,
				completion.Receipt.BackendVersion, store.MemoryMaterializationActive, completion.Receipt.ContentDigest,
				memorySourceProposal, memorySourceManual, store.MemoryTrustUntrusted, completion.Now,
				catalog.NamespaceUID, catalog.ID, operation.ExpectedMaterializedGeneration,
				operation.DesiredGeneration, operation.ID, operation.BackendUID, operation.AuthorityEpoch, operation.RoutingEpoch)
		} else {
			result, err = tx.ExecContext(ctx, `UPDATE remote_memory_catalog
				SET generation = ?, desired_generation = ?, backend_memory_id = ?, backend_version = ?,
					materialization_state = ?, content_digest = ?, content_available = TRUE,
					pending_session_name = '', pending_agent_name = '', pending_task_name = '', pending_parent_task = '',
					pending_source = '', pending_tags_json = '[]', pending_operation_id = '', updated_at = ?
				WHERE namespace_uid = ? AND id = ? AND generation = ? AND desired_generation = ? AND pending_operation_id = ?
					AND backend_uid = ? AND authority_epoch = ? AND routing_epoch = ?`,
				operation.DesiredGeneration, operation.DesiredGeneration, completion.Receipt.BackendMemoryID,
				completion.Receipt.BackendVersion, store.MemoryMaterializationActive, completion.Receipt.ContentDigest,
				completion.Now, catalog.NamespaceUID, catalog.ID, operation.ExpectedMaterializedGeneration,
				operation.DesiredGeneration, operation.ID, operation.BackendUID, operation.AuthorityEpoch, operation.RoutingEpoch)
		}
	case store.MemoryOperationCreate:
		result, err = tx.ExecContext(ctx, `UPDATE remote_memory_catalog
			SET generation = ?, desired_generation = ?, backend_memory_id = ?, backend_version = ?,
				materialization_state = ?, content_digest = ?, content_available = TRUE,
				pending_operation_id = '', updated_at = ?
			WHERE namespace_uid = ? AND id = ? AND generation = ? AND desired_generation = ? AND pending_operation_id = ?
				AND backend_uid = ? AND authority_epoch = ? AND routing_epoch = ?`,
			operation.DesiredGeneration, operation.DesiredGeneration, completion.Receipt.BackendMemoryID,
			completion.Receipt.BackendVersion, store.MemoryMaterializationActive, completion.Receipt.ContentDigest,
			completion.Now, catalog.NamespaceUID, catalog.ID, operation.ExpectedMaterializedGeneration,
			operation.DesiredGeneration, operation.ID, operation.BackendUID, operation.AuthorityEpoch, operation.RoutingEpoch)
	default:
		return store.ValidationErrorf("unsupported operation kind %q", operation.Kind)
	}
	if err != nil {
		return err
	}
	return ensureMemoryRowsAffectedConflict(result, "catalog changed before operation completion")
}

func remoteReplacementProvenanceChanged(
	ctx context.Context,
	q rowQueryer,
	catalog *store.RemoteMemoryCatalogEntry,
	operation *store.MemoryOperation,
) (bool, error) {
	var changed bool
	err := q.QueryRowContext(ctx, `SELECT NOT (
			session_name = pending_session_name AND agent_name = pending_agent_name AND
			task_name = pending_task_name AND parent_task = pending_parent_task AND
			source = pending_source AND tags_json = pending_tags_json
		) FROM remote_memory_catalog
		WHERE namespace_uid = ? AND id = ? AND generation = ? AND desired_generation = ? AND pending_operation_id = ?
			AND backend_uid = ? AND authority_epoch = ? AND routing_epoch = ?`,
		catalog.NamespaceUID, catalog.ID, operation.ExpectedMaterializedGeneration, operation.DesiredGeneration,
		operation.ID, operation.BackendUID, operation.AuthorityEpoch, operation.RoutingEpoch,
	).Scan(&changed)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("%w: catalog changed before operation completion", store.ErrConflict)
	}
	if err != nil {
		return false, err
	}
	return changed, nil
}

// RetryMemoryOperation reschedules or dead-letters the same durable operation.
//
//nolint:gocyclo // Retry covers automatic, ambiguous, expiry, and audited manual transitions in one CAS path.
func (s *Store) RetryMemoryOperation(ctx context.Context, retry store.MemoryOperationRetry) (*store.MemoryOperation, error) {
	retry.Now = normalizeMemoryNow(retry.Now)
	retry.NamespaceUID = strings.TrimSpace(retry.NamespaceUID)
	retry.ID = strings.TrimSpace(retry.ID)
	retry.BackendUID = strings.TrimSpace(retry.BackendUID)
	retry.LeaseOwner = strings.TrimSpace(retry.LeaseOwner)
	if retry.NamespaceUID == "" || retry.ID == "" || retry.BackendUID == "" || retry.AuthorityEpoch <= 0 || retry.RoutingEpoch <= 0 {
		return nil, store.ValidationErrorf("complete operation and binding identity is required")
	}
	if retry.NextRetryAt.IsZero() {
		retry.NextRetryAt = retry.Now
	} else {
		retry.NextRetryAt = retry.NextRetryAt.UTC()
	}
	if retry.DependencyFailure && (retry.Manual || retry.UnsentRelease) {
		return nil, store.ValidationErrorf("dependency-wide failure classification is only valid after a send boundary")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	operation, err := getMemoryOperationQuery(ctx, tx, retry.NamespaceUID, retry.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if operation.BackendUID != retry.BackendUID || operation.AuthorityEpoch != retry.AuthorityEpoch || operation.RoutingEpoch != retry.RoutingEpoch {
		return nil, fmt.Errorf("%w: operation binding identity changed", store.ErrConflict)
	}
	if retry.Manual {
		if retry.UnsentRelease {
			return nil, store.ValidationErrorf("manual retry cannot release an unsent claim")
		}
		if operation.State != store.MemoryOperationDeadLettered || strings.TrimSpace(retry.Actor) == "" || strings.TrimSpace(retry.Reason) == "" {
			return nil, store.ValidationErrorf("manual retry requires a dead letter, actor, and reason")
		}
		if operation.PayloadBytes <= 0 {
			return nil, fmt.Errorf("%w: canonical operation payload is no longer retained", store.ErrConflict)
		}
		if _, err := requireCurrentMemoryBindingForDispatch(ctx, tx, retry.NamespaceUID, retry.BackendUID,
			retry.AuthorityEpoch, retry.RoutingEpoch, retry.Now); err != nil {
			return nil, err
		}
	} else {
		if retry.UnsentRelease {
			if operation.State != store.MemoryOperationLeased ||
				(operation.LeaseOriginState != store.MemoryOperationQueued && operation.LeaseOriginState != store.MemoryOperationAmbiguous) {
				return nil, fmt.Errorf("%w: operation is not an unsent queued or ambiguous claim", store.ErrConflict)
			}
		} else if operation.State != store.MemoryOperationLeased && operation.State != store.MemoryOperationDispatching {
			return nil, fmt.Errorf("%w: operation state %q cannot be retried", store.ErrConflict, operation.State)
		}
		if retry.LeaseOwner == "" || retry.LeaseEpoch <= 0 ||
			operation.LeaseOwner != retry.LeaseOwner || operation.LeaseEpoch != retry.LeaseEpoch {
			return nil, fmt.Errorf("%w: stale memory operation retry", store.ErrConflict)
		}
	}
	newState := store.MemoryOperationQueued
	if retry.Manual {
		if retry.MaxAgeAt.IsZero() {
			retry.MaxAgeAt = retry.Now.Add(defaultOperationRecoveryWindow)
		} else {
			retry.MaxAgeAt = retry.MaxAgeAt.UTC()
		}
		if !retry.MaxAgeAt.After(retry.Now) || retry.MaxAgeAt.Sub(retry.Now) > 30*24*time.Hour {
			return nil, store.ValidationErrorf("manual retry maximum age must be within 30 days")
		}
	} else if retry.UnsentRelease {
		newState = operation.LeaseOriginState
		if retry.DeadLetter || !retry.Now.Before(operation.MaxAgeAt) {
			newState = store.MemoryOperationDeadLettered
		}
	} else {
		if retry.Ambiguous {
			newState = store.MemoryOperationAmbiguous
		}
		semanticAttempts := operation.Attempts
		if !retry.DependencyFailure {
			semanticAttempts++
		}
		if retry.DeadLetter || semanticAttempts >= 10 || !retry.Now.Before(operation.MaxAgeAt) {
			newState = store.MemoryOperationDeadLettered
		}
	}
	errorCode := boundedMemoryAuditText(retry.ErrorCode)
	errorMessage := boundedMemoryAuditText(retry.ErrorMessage)
	query := `UPDATE memory_operations SET state = ?, lease_owner = '', lease_origin_state = '', lease_expires_at = NULL,
		request_deadline = NULL, next_retry_at = ?, error_code = ?, error_message = ?,
		attempts = attempts + ?,
		max_age_at = CASE WHEN ? IS NULL THEN max_age_at ELSE ? END,
		payload_retain_until = CASE
			WHEN ? IS NULL THEN payload_retain_until
			WHEN payload_retain_until > ? THEN payload_retain_until ELSE ? END, updated_at = ?
		WHERE namespace_uid = ? AND id = ? AND state = ? AND backend_uid = ? AND authority_epoch = ? AND routing_epoch = ?`
	var retryMaxAge, retryPayloadRetainUntil any
	if retry.Manual {
		retryMaxAge = retry.MaxAgeAt
		retainUntil := retry.Now.Add(defaultOperationRecoveryWindow)
		if retry.MaxAgeAt.After(retainUntil) {
			retainUntil = retry.MaxAgeAt
		}
		retryPayloadRetainUntil = retainUntil
	}
	semanticAttemptIncrement := 0
	if !retry.Manual && !retry.UnsentRelease && !retry.DependencyFailure {
		semanticAttemptIncrement = 1
	}
	args := []any{newState, retry.NextRetryAt, errorCode, errorMessage, semanticAttemptIncrement, retryMaxAge, retryMaxAge,
		retryPayloadRetainUntil, retryPayloadRetainUntil, retryPayloadRetainUntil, retry.Now,
		retry.NamespaceUID, retry.ID, operation.State, retry.BackendUID, retry.AuthorityEpoch, retry.RoutingEpoch}
	if retry.Manual {
		query += ` AND length(payload) > 0`
	} else {
		query += ` AND lease_owner = ? AND lease_epoch = ?`
		args = append(args, retry.LeaseOwner, retry.LeaseEpoch)
	}
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	if err := ensureMemoryRowsAffectedConflict(result, "stale memory operation retry or payload unavailable"); err != nil {
		return nil, err
	}
	if retry.Manual {
		if _, err := tx.ExecContext(ctx, `UPDATE memory_idempotency
			SET expires_at = CASE WHEN expires_at > ? THEN expires_at ELSE ? END, updated_at = ?
			WHERE namespace_uid = ? AND operation_id = ?`,
			retry.MaxAgeAt, retry.MaxAgeAt, retry.Now, retry.NamespaceUID, retry.ID); err != nil {
			return nil, err
		}
	}
	actor := retry.Actor
	if actor == "" {
		actor = retry.LeaseOwner
	}
	if err := insertMemoryAudit(ctx, tx, store.MemoryAuditRecord{
		Namespace: operation.Namespace, NamespaceUID: operation.NamespaceUID, Actor: actor,
		Action: "memory.operation.retry", Reason: retry.Reason, PreviousState: string(operation.State), NewState: string(newState),
		AuthorityEpoch: operation.AuthorityEpoch, RoutingEpoch: operation.RoutingEpoch, MemoryID: operation.MemoryID,
		OperationID: operation.ID, ProposalID: operation.ProposalID, MutationDigest: operation.MutationDigest,
		ContentDigest: operation.ContentDigest, RequestID: retry.RequestID, CreatedAt: retry.Now,
	}); err != nil {
		return nil, err
	}
	updated, err := getMemoryOperationQuery(ctx, tx, retry.NamespaceUID, retry.ID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return updated, nil
}

// RecordMemoryActivationRecoveryReceipt persists the operator-verified matched
// recovery manifest required before the first write-authority cutover. The
// receipt is bound to the complete non-secret validated route and expires for
// activation purposes after activationRecoveryReceiptMaxAge.
func (s *Store) RecordMemoryActivationRecoveryReceipt(
	ctx context.Context,
	receipt store.MemoryActivationRecoveryReceipt,
) (*store.MemoryActivationRecoveryReceipt, error) {
	receipt.ID = strings.TrimSpace(receipt.ID)
	if receipt.ID == "" {
		receipt.ID = "mrecovery-" + uuid.NewString()
	}
	receipt.Namespace = strings.TrimSpace(receipt.Namespace)
	receipt.NamespaceUID = strings.TrimSpace(receipt.NamespaceUID)
	receipt.BackendUID = strings.TrimSpace(receipt.BackendUID)
	receipt.RouteDigest = strings.TrimSpace(receipt.RouteDigest)
	receipt.StoreUUID = strings.TrimSpace(receipt.StoreUUID)
	receipt.ManifestDigest = strings.TrimSpace(receipt.ManifestDigest)
	receipt.Actor = strings.TrimSpace(receipt.Actor)
	receipt.Reason = strings.TrimSpace(receipt.Reason)
	receipt.RequestID = strings.TrimSpace(receipt.RequestID)
	receipt.VerifiedAt = normalizeMemoryNow(receipt.VerifiedAt)
	if receipt.Namespace == "" || receipt.NamespaceUID == "" || receipt.BackendUID == "" ||
		!validMemorySHA256Digest(receipt.RouteDigest) || receipt.StoreUUID == "" ||
		!validMemorySHA256Digest(receipt.ManifestDigest) || receipt.Actor == "" || receipt.Reason == "" {
		return nil, store.ValidationErrorf("activation recovery receipt requires exact route, store, manifest, actor, and reason")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if binding, bindingErr := getMemoryBackendBindingQuery(ctx, tx, receipt.NamespaceUID); bindingErr == nil {
		if binding.Mode == store.MemoryBackendModeRemote {
			return nil, fmt.Errorf("%w: remote authority is already active; record a runtime checkpoint instead", store.ErrConflict)
		}
	} else if !errors.Is(bindingErr, sql.ErrNoRows) {
		return nil, bindingErr
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO memory_activation_recovery_receipts
		(namespace_uid, receipt_id, namespace_name, backend_uid, route_digest, store_uuid,
		 manifest_digest, actor, reason, request_id, verified_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(namespace_uid) DO UPDATE SET receipt_id = excluded.receipt_id,
		 namespace_name = excluded.namespace_name, backend_uid = excluded.backend_uid,
		 route_digest = excluded.route_digest, store_uuid = excluded.store_uuid,
		 manifest_digest = excluded.manifest_digest, actor = excluded.actor,
		 reason = excluded.reason, request_id = excluded.request_id, verified_at = excluded.verified_at`,
		receipt.NamespaceUID, receipt.ID, receipt.Namespace, receipt.BackendUID, receipt.RouteDigest,
		receipt.StoreUUID, receipt.ManifestDigest, boundedMemoryAuditText(receipt.Actor),
		boundedMemoryAuditText(receipt.Reason), boundedMemoryAuditText(receipt.RequestID), receipt.VerifiedAt)
	if err != nil {
		if isSQLiteConstraintError(err) {
			return nil, fmt.Errorf("%w: recovery receipt id is already bound to another namespace", store.ErrConflict)
		}
		return nil, err
	}
	if err := insertMemoryAudit(ctx, tx, store.MemoryAuditRecord{
		Namespace: receipt.Namespace, NamespaceUID: receipt.NamespaceUID, Actor: receipt.Actor,
		Action: "memory.recovery.activation_receipt", Reason: receipt.Reason,
		NewState: receipt.ID, RequestDigest: receipt.ManifestDigest,
		ContentDigest: receipt.RouteDigest, RequestID: receipt.RequestID, CreatedAt: receipt.VerifiedAt,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &receipt, nil
}

func requireFreshMemoryActivationRecoveryReceipt(
	ctx context.Context,
	tx *sql.Tx,
	binding store.MemoryBackendBinding,
	now time.Time,
) error {
	var receipt store.MemoryActivationRecoveryReceipt
	if err := tx.QueryRowContext(ctx, `SELECT receipt_id, namespace_name, namespace_uid, backend_uid,
		route_digest, store_uuid, manifest_digest, actor, reason, request_id, verified_at
		FROM memory_activation_recovery_receipts WHERE namespace_uid = ?`, binding.NamespaceUID).Scan(
		&receipt.ID, &receipt.Namespace, &receipt.NamespaceUID, &receipt.BackendUID,
		&receipt.RouteDigest, &receipt.StoreUUID, &receipt.ManifestDigest, &receipt.Actor,
		&receipt.Reason, &receipt.RequestID, &receipt.VerifiedAt,
	); errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: a fresh matched activation recovery receipt is required", store.ErrNotReady)
	} else if err != nil {
		return err
	}
	expectedRouteDigest := memoryRecoveryRouteIdentityFromBinding(binding).Digest()
	if receipt.Namespace != binding.Namespace || receipt.BackendUID != binding.BackendUID ||
		receipt.StoreUUID != binding.StoreUUID || receipt.RouteDigest != expectedRouteDigest ||
		!validMemorySHA256Digest(receipt.ManifestDigest) || receipt.VerifiedAt.After(now.Add(maxFeatureHeartbeatClockSkew)) ||
		now.Sub(receipt.VerifiedAt) > activationRecoveryReceiptMaxAge {
		return fmt.Errorf("%w: activation recovery receipt does not match the fresh backend route", store.ErrNotReady)
	}
	return nil
}

func memoryRecoveryRouteIdentityFromBinding(binding store.MemoryBackendBinding) store.MemoryRecoveryRouteIdentity {
	return binding.RecoveryRouteIdentity()
}

func validMemorySHA256Digest(value string) bool {
	if len(value) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

// RecordMemoryVerifiedCheckpoint durably advances the identity-bound payload
// purge watermark only after the caller has verified the matched remote
// checkpoint.
//
//nolint:gocyclo // Checkpoint identity and monotonic watermark checks are intentionally explicit.
func (s *Store) RecordMemoryVerifiedCheckpoint(ctx context.Context, checkpoint store.MemoryVerifiedCheckpoint) (*store.MemoryVerifiedCheckpoint, error) {
	checkpoint.ID = strings.TrimSpace(checkpoint.ID)
	if checkpoint.ID == "" {
		checkpoint.ID = "mcheckpoint-" + uuid.NewString()
	}
	checkpoint.NamespaceUID = strings.TrimSpace(checkpoint.NamespaceUID)
	checkpoint.BackendUID = strings.TrimSpace(checkpoint.BackendUID)
	checkpoint.StoreUUID = strings.TrimSpace(checkpoint.StoreUUID)
	checkpoint.CheckpointDigest = strings.TrimSpace(checkpoint.CheckpointDigest)
	checkpoint.Actor = strings.TrimSpace(checkpoint.Actor)
	checkpoint.Reason = strings.TrimSpace(checkpoint.Reason)
	checkpoint.VerifiedAt = normalizeMemoryNow(checkpoint.VerifiedAt)
	if checkpoint.NamespaceUID == "" || checkpoint.BackendUID == "" || checkpoint.StoreUUID == "" ||
		checkpoint.AuthorityEpoch <= 0 || checkpoint.RoutingEpoch <= 0 || checkpoint.MaximumOperationSequence < 0 ||
		checkpoint.CheckpointDigest == "" || checkpoint.Actor == "" || checkpoint.Reason == "" {
		return nil, store.ValidationErrorf("verified checkpoint requires exact binding, sequence, digest, actor, and reason")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	binding, err := getMemoryBackendBindingQuery(ctx, tx, checkpoint.NamespaceUID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if binding.BackendUID != checkpoint.BackendUID || binding.AuthorityEpoch != checkpoint.AuthorityEpoch ||
		checkpoint.RoutingEpoch <= 0 || checkpoint.RoutingEpoch > binding.RoutingEpoch ||
		binding.StoreUUID != checkpoint.StoreUUID {
		return nil, fmt.Errorf("%w: checkpoint binding identity changed", store.ErrConflict)
	}
	var maximumCommitted int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence), 0) FROM memory_operations
		WHERE namespace_uid = ? AND backend_uid = ? AND authority_epoch = ? AND routing_epoch <= ?`,
		checkpoint.NamespaceUID, checkpoint.BackendUID, checkpoint.AuthorityEpoch, checkpoint.RoutingEpoch).
		Scan(&maximumCommitted); err != nil {
		return nil, err
	}
	if checkpoint.MaximumOperationSequence > maximumCommitted {
		return nil, store.ValidationErrorf("checkpoint operation sequence exceeds the committed ledger watermark")
	}
	var unresolved int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_operations
		WHERE namespace_uid = ? AND backend_uid = ? AND authority_epoch = ? AND routing_epoch <= ? AND sequence <= ?
			AND state IN ('queued','leased','dispatching','ambiguous','dead_lettered')`,
		checkpoint.NamespaceUID, checkpoint.BackendUID, checkpoint.AuthorityEpoch, checkpoint.RoutingEpoch,
		checkpoint.MaximumOperationSequence).Scan(&unresolved); err != nil {
		return nil, err
	}
	if unresolved > 0 {
		return nil, fmt.Errorf("%w: checkpoint watermark includes unresolved memory operations", store.ErrConflict)
	}
	var existingID, existingBackend, existingStore string
	var existingAuthority, existingRouting, existingSequence int64
	err = tx.QueryRowContext(ctx, `SELECT checkpoint_id, backend_uid, authority_epoch, routing_epoch, store_uuid,
		maximum_operation_sequence FROM memory_verified_checkpoints WHERE namespace_uid = ?`, checkpoint.NamespaceUID).
		Scan(&existingID, &existingBackend, &existingAuthority, &existingRouting, &existingStore, &existingSequence)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil && existingBackend == checkpoint.BackendUID && existingAuthority == checkpoint.AuthorityEpoch &&
		existingStore == checkpoint.StoreUUID &&
		(checkpoint.RoutingEpoch < existingRouting || checkpoint.MaximumOperationSequence < existingSequence) {
		return nil, fmt.Errorf("%w: checkpoint route or sequence cannot move backward", store.ErrConflict)
	}
	checkpoint.Namespace = binding.Namespace
	checkpoint.TenantID = binding.TenantID
	_, err = tx.ExecContext(ctx, `INSERT INTO memory_verified_checkpoints
		(namespace_uid, checkpoint_id, namespace_name, backend_uid, authority_epoch, routing_epoch, tenant_id,
		 store_uuid, maximum_operation_sequence, checkpoint_digest, actor, reason, request_id, verified_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(namespace_uid) DO UPDATE SET checkpoint_id = excluded.checkpoint_id,
		 namespace_name = excluded.namespace_name, backend_uid = excluded.backend_uid,
		 authority_epoch = excluded.authority_epoch, routing_epoch = excluded.routing_epoch,
		 tenant_id = excluded.tenant_id, store_uuid = excluded.store_uuid,
		 maximum_operation_sequence = excluded.maximum_operation_sequence,
		 checkpoint_digest = excluded.checkpoint_digest, actor = excluded.actor, reason = excluded.reason,
		 request_id = excluded.request_id, verified_at = excluded.verified_at`,
		checkpoint.NamespaceUID, checkpoint.ID, checkpoint.Namespace, checkpoint.BackendUID,
		checkpoint.AuthorityEpoch, checkpoint.RoutingEpoch, checkpoint.TenantID, checkpoint.StoreUUID,
		checkpoint.MaximumOperationSequence, checkpoint.CheckpointDigest, boundedMemoryAuditText(checkpoint.Actor),
		boundedMemoryAuditText(checkpoint.Reason), boundedMemoryAuditText(checkpoint.RequestID), checkpoint.VerifiedAt)
	if err != nil {
		if isSQLiteConstraintError(err) {
			return nil, fmt.Errorf("%w: checkpoint id is already bound to another namespace", store.ErrConflict)
		}
		return nil, err
	}
	if err := insertMemoryAudit(ctx, tx, store.MemoryAuditRecord{
		Namespace: binding.Namespace, NamespaceUID: binding.NamespaceUID, Actor: checkpoint.Actor,
		Action: "memory.checkpoint.verify", Reason: checkpoint.Reason, AuthorityEpoch: binding.AuthorityEpoch,
		RoutingEpoch: checkpoint.RoutingEpoch, RequestDigest: checkpoint.CheckpointDigest,
		RequestID: checkpoint.RequestID, CreatedAt: checkpoint.VerifiedAt,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &checkpoint, nil
}

// PurgeMemoryGovernance executes one audited retention purge. Provider redo
// payloads, receipts, and tombstones are removable only through the exact
// verified checkpoint watermark; expired idempotency and audit retention are
// likewise deleted only through this method.
//
//nolint:gocyclo // Audited retention performs several independently fenced purge classes atomically.
func (s *Store) PurgeMemoryGovernance(ctx context.Context, purge store.MemoryGovernancePurge) (*store.MemoryGovernancePurgeResult, error) {
	purge.Now = normalizeMemoryNow(purge.Now)
	purge.Before = purge.Before.UTC()
	purge.NamespaceUID = strings.TrimSpace(purge.NamespaceUID)
	purge.BackendUID = strings.TrimSpace(purge.BackendUID)
	purge.StoreUUID = strings.TrimSpace(purge.StoreUUID)
	purge.CheckpointID = strings.TrimSpace(purge.CheckpointID)
	purge.Actor = strings.TrimSpace(purge.Actor)
	purge.Reason = strings.TrimSpace(purge.Reason)
	if purge.NamespaceUID == "" || purge.BackendUID == "" || purge.AuthorityEpoch <= 0 || purge.RoutingEpoch <= 0 ||
		purge.StoreUUID == "" || purge.CheckpointID == "" || purge.Before.IsZero() || purge.Before.After(purge.Now) ||
		purge.Actor == "" || purge.Reason == "" ||
		(!purge.PurgePayloads && !purge.PurgeReceipts && !purge.PurgeExpiredIdempotency && !purge.PurgeTombstones && !purge.PurgeAudit) {
		return nil, store.ValidationErrorf("governance purge requires exact checkpoint identity, cutoff, actor, reason, and at least one target")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	binding, err := getMemoryBackendBindingQuery(ctx, tx, purge.NamespaceUID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if binding.BackendUID != purge.BackendUID || binding.AuthorityEpoch != purge.AuthorityEpoch ||
		purge.RoutingEpoch <= 0 || purge.RoutingEpoch > binding.RoutingEpoch || binding.StoreUUID != purge.StoreUUID {
		return nil, fmt.Errorf("%w: purge binding identity changed", store.ErrConflict)
	}
	var checkpoint store.MemoryVerifiedCheckpoint
	if err := tx.QueryRowContext(ctx, `SELECT checkpoint_id, namespace_name, namespace_uid, backend_uid, authority_epoch,
		routing_epoch, tenant_id, store_uuid, maximum_operation_sequence, checkpoint_digest, actor, reason, request_id, verified_at
		FROM memory_verified_checkpoints WHERE namespace_uid = ?`, purge.NamespaceUID).Scan(
		&checkpoint.ID, &checkpoint.Namespace, &checkpoint.NamespaceUID, &checkpoint.BackendUID,
		&checkpoint.AuthorityEpoch, &checkpoint.RoutingEpoch, &checkpoint.TenantID, &checkpoint.StoreUUID,
		&checkpoint.MaximumOperationSequence, &checkpoint.CheckpointDigest, &checkpoint.Actor,
		&checkpoint.Reason, &checkpoint.RequestID, &checkpoint.VerifiedAt); errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: verified checkpoint is missing", store.ErrConflict)
	} else if err != nil {
		return nil, err
	}
	if checkpoint.ID != purge.CheckpointID || checkpoint.BackendUID != purge.BackendUID ||
		checkpoint.AuthorityEpoch != purge.AuthorityEpoch || checkpoint.RoutingEpoch != purge.RoutingEpoch ||
		checkpoint.StoreUUID != purge.StoreUUID {
		return nil, fmt.Errorf("%w: verified checkpoint identity changed", store.ErrConflict)
	}
	if purge.MaximumOperationSequence == 0 {
		purge.MaximumOperationSequence = checkpoint.MaximumOperationSequence
	}
	if purge.MaximumOperationSequence < 0 || purge.MaximumOperationSequence > checkpoint.MaximumOperationSequence {
		return nil, store.ValidationErrorf("purge operation sequence exceeds the verified checkpoint")
	}

	result := &store.MemoryGovernancePurgeResult{}
	if purge.PurgePayloads {
		updated, err := tx.ExecContext(ctx, `UPDATE memory_operations SET payload = X'', payload_bytes = 0
			WHERE namespace_uid = ? AND backend_uid = ? AND authority_epoch = ? AND routing_epoch <= ?
				AND sequence <= ? AND state IN ('succeeded','abandoned','superseded','orphaned')
				AND completed_at IS NOT NULL AND completed_at < ?
				AND payload_retain_until <= ? AND length(payload) > 0`, purge.NamespaceUID, purge.BackendUID,
			purge.AuthorityEpoch, purge.RoutingEpoch, purge.MaximumOperationSequence, purge.Before, purge.Now)
		if err != nil {
			return nil, err
		}
		result.PayloadsPurged, err = updated.RowsAffected()
		if err != nil {
			return nil, err
		}
	}
	if purge.PurgeReceipts {
		updated, err := tx.ExecContext(ctx, `UPDATE memory_operations SET receipt_binding_digest = '',
			receipt_applied_generation = 0, receipt_backend_version = '', receipt_backend_memory_id = '',
			receipt_content_digest = '', receipt_mutation_digest = '', receipt_completed_at = NULL
			WHERE namespace_uid = ? AND backend_uid = ? AND authority_epoch = ? AND routing_epoch <= ?
				AND sequence <= ? AND state = 'succeeded' AND completed_at IS NOT NULL AND completed_at < ?
				AND (receipt_binding_digest <> '' OR receipt_applied_generation <> 0 OR receipt_backend_version <> ''
					OR receipt_backend_memory_id <> '' OR receipt_content_digest <> '' OR receipt_mutation_digest <> ''
					OR receipt_completed_at IS NOT NULL)`, purge.NamespaceUID,
			purge.BackendUID, purge.AuthorityEpoch, purge.RoutingEpoch, purge.MaximumOperationSequence, purge.Before)
		if err != nil {
			return nil, err
		}
		result.ReceiptsPurged, err = updated.RowsAffected()
		if err != nil {
			return nil, err
		}
	}
	if purge.PurgeExpiredIdempotency {
		deleted, err := tx.ExecContext(ctx, `DELETE FROM memory_idempotency
			WHERE namespace_uid = ? AND expires_at <= ? AND updated_at < ? AND
				(operation_id = '' OR EXISTS (SELECT 1 FROM memory_operations o WHERE o.namespace_uid = memory_idempotency.namespace_uid
					AND o.id = memory_idempotency.operation_id AND o.sequence <= ?
					AND o.state IN ('succeeded','abandoned','superseded','orphaned')))`,
			purge.NamespaceUID, purge.Now, purge.Before, purge.MaximumOperationSequence)
		if err != nil {
			return nil, err
		}
		result.IdempotencyPurged, err = deleted.RowsAffected()
		if err != nil {
			return nil, err
		}
	}
	if purge.PurgeTombstones {
		const tombstonePurgeBatchSize = 200
		for {
			rows, err := tx.QueryContext(ctx, `SELECT c.id FROM remote_memory_catalog c
				WHERE c.namespace_uid = ? AND c.backend_uid = ? AND c.authority_epoch = ?
					AND c.routing_epoch <= ? AND c.store_uuid = ? AND c.updated_at < ?
					AND (c.deleted = TRUE OR c.materialization_state IN ('deleted','orphaned'))
					AND NOT EXISTS (SELECT 1 FROM memory_idempotency i
						WHERE i.namespace_uid = c.namespace_uid AND i.memory_id = c.id)
					AND NOT EXISTS (SELECT 1 FROM memory_operations u
						WHERE u.namespace_uid = c.namespace_uid AND u.memory_id = c.id
							AND (u.state NOT IN ('succeeded','abandoned','superseded','orphaned')
								OR u.payload_retain_until > ? OR u.updated_at >= ? OR length(u.payload) > 0
								OR u.receipt_binding_digest <> '' OR u.receipt_applied_generation <> 0
								OR u.receipt_backend_version <> '' OR u.receipt_backend_memory_id <> ''
								OR u.receipt_content_digest <> '' OR u.receipt_mutation_digest <> ''
								OR u.receipt_completed_at IS NOT NULL))
					AND NOT EXISTS (SELECT 1 FROM memory_operations o
						WHERE o.namespace_uid = c.namespace_uid AND o.memory_id = c.id
							AND (o.backend_uid <> ? OR o.authority_epoch <> ? OR o.routing_epoch > ?))
					AND COALESCE((SELECT MAX(o.sequence) FROM memory_operations o
						WHERE o.namespace_uid = c.namespace_uid AND o.memory_id = c.id), 0) <= ?
				ORDER BY c.id LIMIT ?`, purge.NamespaceUID, purge.BackendUID, purge.AuthorityEpoch,
				purge.RoutingEpoch, purge.StoreUUID, purge.Before, purge.Now, purge.Before,
				purge.BackendUID, purge.AuthorityEpoch, purge.RoutingEpoch,
				purge.MaximumOperationSequence, tombstonePurgeBatchSize)
			if err != nil {
				return nil, err
			}
			ids := make([]string, 0, tombstonePurgeBatchSize)
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					rows.Close() //nolint:errcheck
					return nil, err
				}
				ids = append(ids, id)
			}
			if err := rows.Close(); err != nil {
				return nil, err
			}
			if len(ids) == 0 {
				break
			}
			placeholders := memoryPlaceholders(len(ids))
			args := make([]any, 0, len(ids)+1)
			args = append(args, purge.NamespaceUID)
			for _, id := range ids {
				args = append(args, id)
			}
			watermarkArgs := make([]any, 0, len(args)+1)
			watermarkArgs = append(watermarkArgs, purge.Now)
			watermarkArgs = append(watermarkArgs, args...)
			if _, err := tx.ExecContext(ctx, `INSERT INTO remote_memory_generation_watermarks
				(namespace_uid, id, backend_uid, authority_epoch, routing_epoch, store_uuid, generation, updated_at)
				SELECT c.namespace_uid, c.id, c.backend_uid, c.authority_epoch, c.routing_epoch, c.store_uuid,
					MAX(c.generation, c.desired_generation), ?
				FROM remote_memory_catalog c WHERE c.namespace_uid = ? AND c.id IN (`+placeholders+`)
				ON CONFLICT(namespace_uid, id) DO UPDATE SET
					backend_uid = excluded.backend_uid, authority_epoch = excluded.authority_epoch,
					routing_epoch = CASE
						WHEN remote_memory_generation_watermarks.backend_uid = excluded.backend_uid
							AND remote_memory_generation_watermarks.authority_epoch = excluded.authority_epoch
							AND remote_memory_generation_watermarks.store_uuid = excluded.store_uuid
						THEN MAX(remote_memory_generation_watermarks.routing_epoch, excluded.routing_epoch)
						ELSE excluded.routing_epoch END,
					store_uuid = excluded.store_uuid, generation = CASE
						WHEN remote_memory_generation_watermarks.backend_uid = excluded.backend_uid
							AND remote_memory_generation_watermarks.authority_epoch = excluded.authority_epoch
							AND remote_memory_generation_watermarks.store_uuid = excluded.store_uuid
						THEN MAX(remote_memory_generation_watermarks.generation, excluded.generation)
						ELSE excluded.generation END,
					updated_at = excluded.updated_at`, watermarkArgs...); err != nil {
				return nil, err
			}
			archiveArgs := make([]any, 0, len(args)+1)
			archiveArgs = append(archiveArgs, args...)
			archiveArgs = append(archiveArgs, store.MemoryMaterializationDeleted)
			if _, err := tx.ExecContext(ctx, `DELETE FROM legacy_memory_archive
				WHERE namespace_uid = ? AND id IN (`+placeholders+`)
					AND EXISTS (SELECT 1 FROM remote_memory_catalog c
						WHERE c.namespace_uid = legacy_memory_archive.namespace_uid
							AND c.id = legacy_memory_archive.id AND c.deleted = TRUE
							AND c.materialization_state = ?)`, archiveArgs...); err != nil {
				return nil, err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM memory_operations
				WHERE namespace_uid = ? AND memory_id IN (`+placeholders+`)
					AND state IN ('succeeded','abandoned','superseded','orphaned')
					AND length(payload) = 0 AND receipt_binding_digest = '' AND receipt_applied_generation = 0
					AND receipt_backend_version = '' AND receipt_backend_memory_id = ''
					AND receipt_content_digest = '' AND receipt_mutation_digest = ''
					AND receipt_completed_at IS NULL`, args...); err != nil {
				return nil, err
			}
			deleted, err := tx.ExecContext(ctx, `DELETE FROM remote_memory_catalog
				WHERE namespace_uid = ? AND id IN (`+placeholders+`)`, args...)
			if err != nil {
				return nil, err
			}
			count, err := deleted.RowsAffected()
			if err != nil {
				return nil, err
			}
			result.TombstonesPurged += count
		}
	}
	if purge.PurgeAudit {
		if _, err := tx.ExecContext(ctx, `INSERT INTO memory_audit_purge_guard(singleton) VALUES (1)
			ON CONFLICT(singleton) DO NOTHING`); err != nil {
			return nil, err
		}
		// Backend lifecycle/validation rows remain authoritative for controller
		// reconciliation, while legacy memory trust and governance are materialized
		// from their audit history. Purging either class can silently change live
		// behavior, so only non-authoritative audit history is eligible here.
		deleted, err := tx.ExecContext(ctx, `DELETE FROM memory_audit
			WHERE namespace_uid = ? AND created_at < ?
				AND action NOT LIKE 'backend.%'
				AND action NOT IN ('memory.disable','memory.trust')`, purge.NamespaceUID, purge.Before)
		if err != nil {
			return nil, err
		}
		result.AuditRowsPurged, err = deleted.RowsAffected()
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM memory_audit_purge_guard WHERE singleton = 1`); err != nil {
			return nil, err
		}
	}
	var previousDigest string
	var purgeCount int64
	var cumulativePayloads, cumulativeReceipts, cumulativeIdempotency, cumulativeTombstones, cumulativeAudit int64
	err = tx.QueryRowContext(ctx, `SELECT purge_count, cumulative_payloads_purged, cumulative_receipts_purged,
		cumulative_idempotency_purged, cumulative_tombstones_purged, cumulative_audit_rows_purged, purge_digest
		FROM memory_governance_purge_watermarks WHERE namespace_uid = ?`, purge.NamespaceUID).Scan(
		&purgeCount, &cumulativePayloads, &cumulativeReceipts, &cumulativeIdempotency,
		&cumulativeTombstones, &cumulativeAudit, &previousDigest)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	purgeCount++
	result.PurgeDigest = memoryGovernancePurgeDigest(previousDigest, purge, *result)
	_, err = tx.ExecContext(ctx, `INSERT INTO memory_governance_purge_watermarks
		(namespace_uid, purge_count, maximum_operation_sequence, before_at, cumulative_payloads_purged,
		 cumulative_receipts_purged, cumulative_idempotency_purged, cumulative_tombstones_purged,
		 cumulative_audit_rows_purged, purge_digest, actor, reason, request_id, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(namespace_uid) DO UPDATE SET purge_count = excluded.purge_count,
		 maximum_operation_sequence = excluded.maximum_operation_sequence, before_at = excluded.before_at,
		 cumulative_payloads_purged = excluded.cumulative_payloads_purged,
		 cumulative_receipts_purged = excluded.cumulative_receipts_purged,
		 cumulative_idempotency_purged = excluded.cumulative_idempotency_purged,
		 cumulative_tombstones_purged = excluded.cumulative_tombstones_purged,
		 cumulative_audit_rows_purged = excluded.cumulative_audit_rows_purged,
		 purge_digest = excluded.purge_digest, actor = excluded.actor, reason = excluded.reason,
		 request_id = excluded.request_id, updated_at = excluded.updated_at`,
		purge.NamespaceUID, purgeCount, purge.MaximumOperationSequence, purge.Before,
		cumulativePayloads+result.PayloadsPurged, cumulativeReceipts+result.ReceiptsPurged,
		cumulativeIdempotency+result.IdempotencyPurged, cumulativeTombstones+result.TombstonesPurged,
		cumulativeAudit+result.AuditRowsPurged, result.PurgeDigest, boundedMemoryAuditText(purge.Actor),
		boundedMemoryAuditText(purge.Reason), boundedMemoryAuditText(purge.RequestID), purge.Now)
	if err != nil {
		return nil, err
	}
	if err := insertMemoryAudit(ctx, tx, store.MemoryAuditRecord{
		Namespace: binding.Namespace, NamespaceUID: binding.NamespaceUID, Actor: purge.Actor,
		Action: "memory.governance.purge", Reason: purge.Reason, AuthorityEpoch: binding.AuthorityEpoch,
		RoutingEpoch: purge.RoutingEpoch, RequestDigest: result.PurgeDigest,
		RequestID: purge.RequestID, CreatedAt: purge.Now,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

func memoryGovernancePurgeDigest(previous string, purge store.MemoryGovernancePurge, result store.MemoryGovernancePurgeResult) string {
	preimage := struct {
		Previous string
		Purge    store.MemoryGovernancePurge
		Result   store.MemoryGovernancePurgeResult
	}{Previous: previous, Purge: purge, Result: result}
	encoded, _ := json.Marshal(preimage)
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:])
}

// AbandonMemoryOperation terminally resolves a dead letter after a durable fence/proof.
func (s *Store) AbandonMemoryOperation(ctx context.Context, abandonment store.MemoryOperationAbandonment) (*store.MemoryOperation, error) {
	abandonment.Now = normalizeMemoryNow(abandonment.Now)
	abandonment.NamespaceUID = strings.TrimSpace(abandonment.NamespaceUID)
	abandonment.ID = strings.TrimSpace(abandonment.ID)
	abandonment.BackendUID = strings.TrimSpace(abandonment.BackendUID)
	if abandonment.NamespaceUID == "" || abandonment.ID == "" || abandonment.BackendUID == "" ||
		abandonment.AuthorityEpoch <= 0 || abandonment.RoutingEpoch <= 0 || strings.TrimSpace(abandonment.Actor) == "" ||
		strings.TrimSpace(abandonment.Reason) == "" || !abandonment.Fenced {
		return nil, store.ValidationErrorf("abandonment requires exact binding identity, actor, reason, and durable fence")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	operation, err := getMemoryOperationQuery(ctx, tx, abandonment.NamespaceUID, abandonment.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if operation.State != store.MemoryOperationDeadLettered || operation.BackendUID != abandonment.BackendUID ||
		operation.AuthorityEpoch != abandonment.AuthorityEpoch || operation.RoutingEpoch != abandonment.RoutingEpoch {
		return nil, fmt.Errorf("%w: operation is not the expected dead letter", store.ErrConflict)
	}
	providerApplicationUncertain := operation.SendStartedAt != nil
	if (providerApplicationUncertain || operation.ProposalID != "") && !abandonment.ProviderNeverApplied {
		return nil, store.ValidationErrorf("sent or proposal mutation abandonment requires provider proof that the mutation was never applied")
	}
	result, err := tx.ExecContext(ctx, `UPDATE memory_operations SET state = ?, lease_owner = '', lease_origin_state = '',
		lease_expires_at = NULL, request_deadline = NULL, updated_at = ?, completed_at = ?
		WHERE namespace_uid = ? AND id = ? AND state = ? AND backend_uid = ? AND authority_epoch = ? AND routing_epoch = ?`,
		store.MemoryOperationAbandoned, abandonment.Now, abandonment.Now, abandonment.NamespaceUID, abandonment.ID,
		store.MemoryOperationDeadLettered, abandonment.BackendUID, abandonment.AuthorityEpoch, abandonment.RoutingEpoch)
	if err != nil {
		return nil, err
	}
	if err := ensureMemoryRowsAffectedConflict(result, "memory operation changed before abandonment"); err != nil {
		return nil, err
	}
	if err := extendMemoryIdempotencyTerminalRetention(ctx, tx, abandonment.NamespaceUID, abandonment.ID, abandonment.Now); err != nil {
		return nil, err
	}
	if err := applyMemoryOperationAbandonmentToCatalog(ctx, tx, operation, abandonment.Now); err != nil {
		return nil, err
	}
	if operation.ProposalID != "" {
		result, err := tx.ExecContext(ctx, `UPDATE memory_proposals
			SET status = ?, application_abandoned_by = ?, application_abandoned_reason = ?, application_abandoned_at = ?, updated_at = ?
			WHERE namespace = ? AND id = ? AND status = ? AND apply_operation_id = ?`,
			proposalStatusApplicationAbandoned, boundedMemoryAuditText(abandonment.Actor), boundedMemoryAuditText(abandonment.Reason),
			abandonment.Now, abandonment.Now, operation.Namespace, operation.ProposalID, proposalStatusApplying, operation.ID)
		if err != nil {
			return nil, err
		}
		if err := ensureMemoryRowsAffectedConflict(result, "proposal changed before application abandonment"); err != nil {
			return nil, err
		}
	}
	if err := insertMemoryAudit(ctx, tx, store.MemoryAuditRecord{
		Namespace: operation.Namespace, NamespaceUID: operation.NamespaceUID, Actor: abandonment.Actor,
		Action: "memory.operation.abandon", Reason: abandonment.Reason, PreviousState: string(operation.State),
		NewState: string(store.MemoryOperationAbandoned), AuthorityEpoch: operation.AuthorityEpoch,
		RoutingEpoch: operation.RoutingEpoch, MemoryID: operation.MemoryID, OperationID: operation.ID,
		ProposalID: operation.ProposalID, MutationDigest: operation.MutationDigest,
		ContentDigest: operation.ContentDigest, RequestID: abandonment.RequestID, CreatedAt: abandonment.Now,
	}); err != nil {
		return nil, err
	}
	updated, err := getMemoryOperationQuery(ctx, tx, abandonment.NamespaceUID, abandonment.ID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return updated, nil
}

func applyMemoryOperationAbandonmentToCatalog(ctx context.Context, tx *sql.Tx, operation *store.MemoryOperation, now time.Time) error {
	var result sql.Result
	var err error
	switch operation.Kind {
	case store.MemoryOperationCreate:
		result, err = tx.ExecContext(ctx, `UPDATE remote_memory_catalog
			SET materialization_state = ?, disabled = TRUE, content_available = FALSE, pending_operation_id = '', updated_at = ?
			WHERE namespace_uid = ? AND id = ? AND pending_operation_id = ? AND generation = 0`,
			store.MemoryMaterializationOrphaned, now, operation.NamespaceUID, operation.MemoryID, operation.ID)
	case store.MemoryOperationReplace:
		result, err = tx.ExecContext(ctx, `UPDATE remote_memory_catalog
			SET desired_generation = generation, pending_session_name = '', pending_agent_name = '',
				pending_task_name = '', pending_parent_task = '', pending_source = '', pending_tags_json = '[]',
				pending_operation_id = '', updated_at = ?
			WHERE namespace_uid = ? AND id = ? AND pending_operation_id = ?`,
			now, operation.NamespaceUID, operation.MemoryID, operation.ID)
	case store.MemoryOperationDelete:
		result, err = tx.ExecContext(ctx, `UPDATE remote_memory_catalog
			SET desired_generation = generation,
				materialization_state = CASE
					WHEN materialization_state = ? THEN ?
					WHEN materialization_state IN (?, ?, ?) THEN materialization_state
					ELSE ?
				END,
				disabled = CASE WHEN materialization_state = ? THEN disabled ELSE TRUE END,
				deleted = FALSE,
				content_available = CASE WHEN materialization_state = ? THEN content_available ELSE FALSE END,
				pending_operation_id = '', updated_at = ?
			WHERE namespace_uid = ? AND id = ? AND pending_operation_id = ? AND deleted = TRUE
				AND generation = ? AND desired_generation = ? AND backend_version = ?`,
			store.MemoryMaterializationActive, store.MemoryMaterializationActive,
			store.MemoryMaterializationDiverged, store.MemoryMaterializationLost, store.MemoryMaterializationOrphaned,
			store.MemoryMaterializationOrphaned, store.MemoryMaterializationActive, store.MemoryMaterializationActive,
			now, operation.NamespaceUID, operation.MemoryID, operation.ID,
			operation.ExpectedMaterializedGeneration, operation.DesiredGeneration, operation.ExpectedBackendVersion)
	default:
		return store.ValidationErrorf("unsupported operation kind %q", operation.Kind)
	}
	if err != nil {
		return err
	}
	return ensureMemoryRowsAffectedConflict(result, "catalog changed before abandonment")
}

// OrphanMemoryOperations terminally fences all unresolved work for a binding identity.
//
// ResolveMemoryOperationsForDecommission terminally resolves work only after
// the coordinator has advanced and acknowledged the remote routing fence. It
// does not install the force-orphan anti-restore fence: provably unsent work is
// abandoned, while work that may have crossed the send boundary is recorded as
// orphaned without claiming a provider outcome.
//
//nolint:gocyclo // Orphaning atomically fences operations, catalog, proposals, and audit history.
func (s *Store) ResolveMemoryOperationsForDecommission(
	ctx context.Context,
	resolution store.MemoryDecommissionResolution,
) (int, error) {
	resolution.Now = normalizeMemoryNow(resolution.Now)
	resolution.NamespaceUID = strings.TrimSpace(resolution.NamespaceUID)
	resolution.BackendUID = strings.TrimSpace(resolution.BackendUID)
	resolution.Actor = strings.TrimSpace(resolution.Actor)
	resolution.Reason = strings.TrimSpace(resolution.Reason)
	if resolution.NamespaceUID == "" || resolution.BackendUID == "" || resolution.AuthorityEpoch <= 0 ||
		resolution.RoutingEpoch <= 0 || resolution.Actor == "" || resolution.Reason == "" {
		return 0, store.ValidationErrorf("decommission resolution requires exact binding identity, actor, and reason")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	binding, err := getMemoryBackendBindingQuery(ctx, tx, resolution.NamespaceUID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, store.ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	if binding.Mode != store.MemoryBackendModeRemote || binding.State != store.MemoryBackendBindingRecovering ||
		binding.BackendUID != resolution.BackendUID || binding.AuthorityEpoch != resolution.AuthorityEpoch ||
		binding.RoutingEpoch != resolution.RoutingEpoch {
		return 0, fmt.Errorf("%w: exact recovering decommission barrier is required", store.ErrConflict)
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, memory_id, proposal_id, state, lease_origin_state,
		send_started_at, routing_epoch, mutation_digest, content_digest
		FROM memory_operations WHERE namespace_uid = ? AND backend_uid = ? AND authority_epoch = ?
			AND state IN ('queued','leased','dispatching','ambiguous','dead_lettered')
		ORDER BY sequence`, resolution.NamespaceUID, resolution.BackendUID, resolution.AuthorityEpoch)
	if err != nil {
		return 0, err
	}
	type unresolvedOperation struct {
		id, memoryID, proposalID, state, leaseOrigin, mutationDigest, contentDigest string
		sendStartedAt                                                               sql.NullTime
		routingEpoch                                                                int64
	}
	var operations []unresolvedOperation
	for rows.Next() {
		var operation unresolvedOperation
		if err := rows.Scan(&operation.id, &operation.memoryID, &operation.proposalID, &operation.state,
			&operation.leaseOrigin, &operation.sendStartedAt, &operation.routingEpoch,
			&operation.mutationDigest, &operation.contentDigest); err != nil {
			rows.Close() //nolint:errcheck
			return 0, err
		}
		operations = append(operations, operation)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	for _, operation := range operations {
		provablyUnsent := operation.state == string(store.MemoryOperationQueued) && !operation.sendStartedAt.Valid
		provablyUnsent = provablyUnsent || (operation.state == string(store.MemoryOperationLeased) &&
			operation.leaseOrigin == string(store.MemoryOperationQueued) && !operation.sendStartedAt.Valid)
		targetState := store.MemoryOperationOrphaned
		errorCode := "MEMORY_OPERATION_ORPHANED"
		errorMessage := "operation orphaned after acknowledged decommission fence"
		if provablyUnsent {
			targetState = store.MemoryOperationAbandoned
			errorCode = "MEMORY_OPERATION_ABANDONED"
			errorMessage = "provably unsent operation abandoned during decommission"
		}
		result, err := tx.ExecContext(ctx, `UPDATE memory_operations SET state = ?, lease_owner = '', lease_origin_state = '',
			lease_expires_at = NULL, request_deadline = NULL, updated_at = ?, completed_at = ?,
			error_code = ?, error_message = ?
			WHERE namespace_uid = ? AND id = ? AND state = ? AND backend_uid = ? AND authority_epoch = ?`,
			targetState, resolution.Now, resolution.Now, errorCode, errorMessage,
			resolution.NamespaceUID, operation.id, operation.state, resolution.BackendUID, resolution.AuthorityEpoch)
		if err != nil {
			return 0, err
		}
		if err := ensureMemoryRowsAffectedConflict(result, "memory operation changed during decommission resolution"); err != nil {
			return 0, err
		}
		minimumExpiry := resolution.Now.Add(minimumIdempotencyRetention)
		if _, err := tx.ExecContext(ctx, `UPDATE memory_idempotency
			SET expires_at = CASE WHEN expires_at > ? THEN expires_at ELSE ? END, updated_at = ?
			WHERE namespace_uid = ? AND operation_id = ?`, minimumExpiry, minimumExpiry,
			resolution.Now, resolution.NamespaceUID, operation.id); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE remote_memory_catalog
			SET materialization_state = ?, disabled = TRUE, content_available = FALSE,
				pending_session_name = '', pending_agent_name = '', pending_task_name = '', pending_parent_task = '',
				pending_source = '', pending_tags_json = '[]', pending_operation_id = '', updated_at = ?
			WHERE namespace_uid = ? AND pending_operation_id = ?`, store.MemoryMaterializationOrphaned,
			resolution.Now, resolution.NamespaceUID, operation.id); err != nil {
			return 0, err
		}
		if operation.proposalID != "" {
			var proposalErr error
			if provablyUnsent {
				proposalErr = abandonProposalApplication(ctx, tx, binding.Namespace, operation.proposalID, operation.id,
					resolution.Actor, resolution.Reason, resolution.Now)
			} else {
				proposalErr = orphanProposalApplication(ctx, tx, binding.Namespace, operation.proposalID, operation.id,
					resolution.Actor, resolution.Reason, resolution.Now)
			}
			if proposalErr != nil {
				return 0, proposalErr
			}
		}
		if err := insertMemoryAudit(ctx, tx, store.MemoryAuditRecord{
			Namespace: binding.Namespace, NamespaceUID: binding.NamespaceUID, Actor: resolution.Actor,
			Action: "memory.operation.decommission_resolve", Reason: resolution.Reason,
			PreviousState: operation.state, NewState: string(targetState), AuthorityEpoch: binding.AuthorityEpoch,
			RoutingEpoch: operation.routingEpoch, MemoryID: operation.memoryID, OperationID: operation.id,
			ProposalID: operation.proposalID, MutationDigest: operation.mutationDigest,
			ContentDigest: operation.contentDigest, RequestID: resolution.RequestID, CreatedAt: resolution.Now,
		}); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(operations), nil
}

//nolint:gocyclo // Force-orphaning validates and persists every anti-resurrection and audit boundary in one transaction.
func (s *Store) OrphanMemoryOperations(ctx context.Context, orphaning store.MemoryOperationOrphaning) (int, error) {
	orphaning.Now = normalizeMemoryNow(orphaning.Now)
	orphaning.NamespaceUID = strings.TrimSpace(orphaning.NamespaceUID)
	orphaning.BackendUID = strings.TrimSpace(orphaning.BackendUID)
	if orphaning.NamespaceUID == "" || orphaning.BackendUID == "" || orphaning.AuthorityEpoch <= 0 || orphaning.RoutingEpoch <= 0 ||
		strings.TrimSpace(orphaning.Actor) == "" || strings.TrimSpace(orphaning.Reason) == "" {
		return 0, store.ValidationErrorf("orphaning requires exact binding identity, actor, and reason")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	binding, err := getMemoryBackendBindingQuery(ctx, tx, orphaning.NamespaceUID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, store.ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	if binding.BackendUID != orphaning.BackendUID || binding.AuthorityEpoch != orphaning.AuthorityEpoch || binding.RoutingEpoch != orphaning.RoutingEpoch {
		return 0, fmt.Errorf("%w: binding identity changed before orphaning", store.ErrConflict)
	}
	if binding.Mode != store.MemoryBackendModeRemote || (binding.State != store.MemoryBackendBindingDraining && binding.State != store.MemoryBackendBindingRecovering && binding.State != store.MemoryBackendBindingDecommissioned &&
		binding.State != store.MemoryBackendBindingRemoved) {
		return 0, fmt.Errorf("%w: orphaning requires a draining, recovering, decommissioned, or removed egress barrier", store.ErrConflict)
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO memory_force_orphan_fences
		(namespace_uid, namespace_name, backend_uid, authority_epoch, routing_epoch, tenant_id, store_uuid,
		 actor, reason, request_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(namespace_uid) DO NOTHING`,
		binding.NamespaceUID, binding.Namespace, binding.BackendUID, binding.AuthorityEpoch, binding.RoutingEpoch,
		binding.TenantID, binding.StoreUUID, boundedMemoryAuditText(orphaning.Actor), boundedMemoryAuditText(orphaning.Reason),
		boundedMemoryAuditText(orphaning.RequestID), orphaning.Now)
	if err != nil {
		return 0, err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if inserted == 0 {
		var backendUID, tenantID, storeUUID string
		var authorityEpoch int64
		if err := tx.QueryRowContext(ctx, `SELECT backend_uid, authority_epoch, tenant_id, store_uuid
			FROM memory_force_orphan_fences WHERE namespace_uid = ?`, binding.NamespaceUID).
			Scan(&backendUID, &authorityEpoch, &tenantID, &storeUUID); err != nil {
			return 0, err
		}
		if backendUID != binding.BackendUID || authorityEpoch != binding.AuthorityEpoch ||
			tenantID != binding.TenantID || storeUUID != binding.StoreUUID {
			return 0, fmt.Errorf("%w: namespace was already force-orphaned under a different authority", store.ErrConflict)
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, memory_id, proposal_id, state, lease_origin_state,
		send_started_at, routing_epoch, mutation_digest, content_digest
		FROM memory_operations WHERE namespace_uid = ? AND backend_uid = ? AND authority_epoch = ?
			AND state IN ('queued','leased','dispatching','ambiguous','dead_lettered')`,
		orphaning.NamespaceUID, orphaning.BackendUID, orphaning.AuthorityEpoch)
	if err != nil {
		return 0, err
	}
	type orphaned struct {
		id, memoryID, proposalID, state, leaseOrigin, mutationDigest, contentDigest string
		sendStartedAt                                                               sql.NullTime
		routingEpoch                                                                int64
	}
	var operations []orphaned
	for rows.Next() {
		var operation orphaned
		if err := rows.Scan(&operation.id, &operation.memoryID, &operation.proposalID, &operation.state,
			&operation.leaseOrigin, &operation.sendStartedAt, &operation.routingEpoch,
			&operation.mutationDigest, &operation.contentDigest); err != nil {
			rows.Close() //nolint:errcheck
			return 0, err
		}
		operations = append(operations, operation)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	result, err = tx.ExecContext(ctx, `UPDATE memory_operations SET state = ?, lease_owner = '', lease_origin_state = '',
		lease_expires_at = NULL, request_deadline = NULL, updated_at = ?, completed_at = ?,
		error_code = 'MEMORY_OPERATION_ORPHANED', error_message = 'operation orphaned after egress barrier'
		WHERE namespace_uid = ? AND backend_uid = ? AND authority_epoch = ?
			AND state IN ('queued','leased','dispatching','ambiguous','dead_lettered')`,
		store.MemoryOperationOrphaned, orphaning.Now, orphaning.Now, orphaning.NamespaceUID,
		orphaning.BackendUID, orphaning.AuthorityEpoch)
	if err != nil {
		return 0, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	minimumExpiry := orphaning.Now.Add(minimumIdempotencyRetention)
	if _, err := tx.ExecContext(ctx, `UPDATE memory_idempotency
		SET expires_at = CASE WHEN expires_at > ? THEN expires_at ELSE ? END, updated_at = ?
		WHERE namespace_uid = ? AND operation_id IN (
			SELECT id FROM memory_operations WHERE namespace_uid = ? AND backend_uid = ?
				AND authority_epoch = ? AND state = ?
		)`, minimumExpiry, minimumExpiry, orphaning.Now, orphaning.NamespaceUID,
		orphaning.NamespaceUID, orphaning.BackendUID, orphaning.AuthorityEpoch, store.MemoryOperationOrphaned); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE remote_memory_catalog
		SET materialization_state = ?, disabled = TRUE, content_available = FALSE,
			pending_session_name = '', pending_agent_name = '', pending_task_name = '', pending_parent_task = '',
			pending_source = '', pending_tags_json = '[]', pending_operation_id = '', updated_at = ?
		WHERE namespace_uid = ? AND pending_operation_id IN (
			SELECT id FROM memory_operations WHERE namespace_uid = ? AND state = ?
		)`, store.MemoryMaterializationOrphaned, orphaning.Now, orphaning.NamespaceUID,
		orphaning.NamespaceUID, store.MemoryOperationOrphaned); err != nil {
		return 0, err
	}
	for _, operation := range operations {
		provablyUnsent := operation.state == string(store.MemoryOperationQueued) && !operation.sendStartedAt.Valid
		provablyUnsent = provablyUnsent || (operation.state == string(store.MemoryOperationLeased) &&
			operation.leaseOrigin == string(store.MemoryOperationQueued) && !operation.sendStartedAt.Valid)
		if operation.proposalID != "" {
			var proposalErr error
			if provablyUnsent {
				proposalErr = abandonProposalApplication(ctx, tx, binding.Namespace, operation.proposalID, operation.id,
					orphaning.Actor, orphaning.Reason, orphaning.Now)
			} else {
				proposalErr = orphanProposalApplication(ctx, tx, binding.Namespace, operation.proposalID, operation.id,
					orphaning.Actor, orphaning.Reason, orphaning.Now)
			}
			if proposalErr != nil {
				return 0, proposalErr
			}
		}
		if err := insertMemoryAudit(ctx, tx, store.MemoryAuditRecord{
			Namespace: binding.Namespace, NamespaceUID: binding.NamespaceUID, Actor: orphaning.Actor,
			Action: "memory.operation.orphan", Reason: orphaning.Reason, PreviousState: operation.state,
			NewState: string(store.MemoryOperationOrphaned), AuthorityEpoch: binding.AuthorityEpoch,
			RoutingEpoch: operation.routingEpoch, MemoryID: operation.memoryID, OperationID: operation.id,
			ProposalID: operation.proposalID, MutationDigest: operation.mutationDigest,
			ContentDigest: operation.contentDigest, RequestID: orphaning.RequestID, CreatedAt: orphaning.Now,
		}); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(count), nil
}

func recoverExpiredMemoryOperationLeases(ctx context.Context, tx *sql.Tx, claim store.MemoryOperationClaim, namespace string) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, memory_id, proposal_id, state, lease_origin_state, mutation_digest, content_digest
		FROM memory_operations WHERE namespace_uid = ? AND backend_uid = ? AND authority_epoch = ? AND routing_epoch = ?
			AND ((state = 'leased' AND lease_expires_at IS NOT NULL AND lease_expires_at <= ?)
				OR (state = 'dispatching' AND request_deadline IS NOT NULL AND request_deadline <= ?))`,
		claim.NamespaceUID, claim.BackendUID, claim.AuthorityEpoch, claim.RoutingEpoch, claim.Now, claim.Now)
	if err != nil {
		return err
	}
	type expired struct{ id, memoryID, proposalID, state, origin, mutationDigest, contentDigest string }
	var operations []expired
	for rows.Next() {
		var operation expired
		if err := rows.Scan(&operation.id, &operation.memoryID, &operation.proposalID, &operation.state, &operation.origin,
			&operation.mutationDigest, &operation.contentDigest); err != nil {
			rows.Close() //nolint:errcheck
			return err
		}
		operations = append(operations, operation)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, operation := range operations {
		newState := store.MemoryOperationQueued
		if operation.state == string(store.MemoryOperationDispatching) || operation.origin == string(store.MemoryOperationAmbiguous) {
			newState = store.MemoryOperationAmbiguous
		}
		if _, err := tx.ExecContext(ctx, `UPDATE memory_operations
			SET state = ?, lease_owner = '', lease_origin_state = '', lease_expires_at = NULL, request_deadline = NULL,
				next_retry_at = ?, error_code = 'MEMORY_OPERATION_LEASE_EXPIRED',
				error_message = 'operation lease expired', updated_at = ?
			WHERE namespace_uid = ? AND id = ? AND state = ?`, newState, claim.Now, claim.Now,
			claim.NamespaceUID, operation.id, operation.state); err != nil {
			return err
		}
		if err := insertMemoryAudit(ctx, tx, store.MemoryAuditRecord{
			Namespace: namespace, NamespaceUID: claim.NamespaceUID, Actor: "system",
			Action: "memory.operation.lease_expired", PreviousState: operation.state, NewState: string(newState),
			AuthorityEpoch: claim.AuthorityEpoch, RoutingEpoch: claim.RoutingEpoch, MemoryID: operation.memoryID,
			OperationID: operation.id, ProposalID: operation.proposalID, MutationDigest: operation.mutationDigest,
			ContentDigest: operation.contentDigest, CreatedAt: claim.Now,
		}); err != nil {
			return err
		}
	}
	return nil
}

func deadLetterExpiredMemoryOperations(ctx context.Context, tx *sql.Tx, claim store.MemoryOperationClaim, namespace string) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, memory_id, proposal_id, state, mutation_digest, content_digest
		FROM memory_operations WHERE namespace_uid = ? AND backend_uid = ? AND authority_epoch = ? AND routing_epoch = ?
			AND state IN ('queued','ambiguous') AND max_age_at <= ?`,
		claim.NamespaceUID, claim.BackendUID, claim.AuthorityEpoch, claim.RoutingEpoch, claim.Now)
	if err != nil {
		return err
	}
	type expired struct{ id, memoryID, proposalID, state, mutationDigest, contentDigest string }
	var operations []expired
	for rows.Next() {
		var operation expired
		if err := rows.Scan(&operation.id, &operation.memoryID, &operation.proposalID, &operation.state,
			&operation.mutationDigest, &operation.contentDigest); err != nil {
			rows.Close() //nolint:errcheck
			return err
		}
		operations = append(operations, operation)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, operation := range operations {
		if _, err := tx.ExecContext(ctx, `UPDATE memory_operations
			SET state = ?, lease_owner = '', lease_origin_state = '', lease_expires_at = NULL, request_deadline = NULL,
				error_code = 'MEMORY_OPERATION_EXPIRED', error_message = 'operation maximum age expired', updated_at = ?
			WHERE namespace_uid = ? AND id = ? AND state = ?`, store.MemoryOperationDeadLettered,
			claim.Now, claim.NamespaceUID, operation.id, operation.state); err != nil {
			return err
		}
		if err := insertMemoryAudit(ctx, tx, store.MemoryAuditRecord{
			Namespace: namespace, NamespaceUID: claim.NamespaceUID, Actor: "system", Action: "memory.operation.expire",
			Reason: "MEMORY_OPERATION_EXPIRED", PreviousState: operation.state, NewState: string(store.MemoryOperationDeadLettered),
			AuthorityEpoch: claim.AuthorityEpoch, RoutingEpoch: claim.RoutingEpoch, MemoryID: operation.memoryID,
			OperationID: operation.id, ProposalID: operation.proposalID, MutationDigest: operation.mutationDigest,
			ContentDigest: operation.contentDigest, CreatedAt: claim.Now,
		}); err != nil {
			return err
		}
	}
	return nil
}

func selectMemoryOperationSQL() string {
	return `SELECT sequence, id, namespace_name, namespace_uid, cluster_id, backend_uid, authority_epoch, routing_epoch,
		memory_id, proposal_id, kind, desired_generation, expected_materialized_generation, expected_backend_version,
		operation_idempotency_key, mutation_idempotency_key, request_digest, mutation_digest, content_digest,
		payload_bytes, state, lease_owner, lease_epoch, lease_origin_state, lease_expires_at, send_started_at, request_deadline,
		dispatches, attempts, next_retry_at, max_age_at, payload_retain_until, error_code, error_message, receipt_binding_digest,
		receipt_applied_generation, receipt_backend_version, receipt_backend_memory_id, receipt_content_digest,
		receipt_mutation_digest, receipt_completed_at, actor, reason, created_at, updated_at, completed_at FROM memory_operations`
}

func getMemoryOperationQuery(ctx context.Context, q rowQueryer, namespaceUID, id string) (*store.MemoryOperation, error) {
	return scanMemoryOperation(q.QueryRowContext(ctx, selectMemoryOperationSQL()+` WHERE namespace_uid = ? AND id = ?`, namespaceUID, id))
}

func getMemoryOperationDispatchQuery(ctx context.Context, q rowQueryer, namespaceUID, id string) (*store.MemoryOperationDispatch, error) {
	row := q.QueryRowContext(ctx, `SELECT sequence, id, namespace_name, namespace_uid, cluster_id, backend_uid, authority_epoch, routing_epoch,
		memory_id, proposal_id, kind, desired_generation, expected_materialized_generation, expected_backend_version,
		operation_idempotency_key, mutation_idempotency_key, request_digest, mutation_digest, content_digest,
		payload_bytes, state, lease_owner, lease_epoch, lease_origin_state, lease_expires_at, send_started_at, request_deadline,
		dispatches, attempts, next_retry_at, max_age_at, payload_retain_until, error_code, error_message, receipt_binding_digest,
		receipt_applied_generation, receipt_backend_version, receipt_backend_memory_id, receipt_content_digest,
		receipt_mutation_digest, receipt_completed_at, actor, reason, created_at, updated_at, completed_at, payload
		FROM memory_operations WHERE namespace_uid = ? AND id = ?`, namespaceUID, id)
	operation, payload, err := scanMemoryOperationWithPayload(row)
	if err != nil {
		return nil, err
	}
	payload, err = decodeDurableMemoryOperationPayload(payload)
	if err != nil {
		return nil, err
	}
	return &store.MemoryOperationDispatch{Operation: *operation, Payload: payload}, nil
}

func scanMemoryOperation(scanner rowScanner) (*store.MemoryOperation, error) {
	operation, _, err := scanMemoryOperationInternal(scanner, false)
	return operation, err
}

func scanMemoryOperationWithPayload(scanner rowScanner) (*store.MemoryOperation, []byte, error) {
	return scanMemoryOperationInternal(scanner, true)
}

func scanMemoryOperationInternal(scanner rowScanner, withPayload bool) (*store.MemoryOperation, []byte, error) {
	var operation store.MemoryOperation
	var leaseExpiresAt, sendStartedAt, requestDeadline, receiptCompletedAt, completedAt sql.NullTime
	var payload []byte
	dest := []any{
		&operation.Sequence, &operation.ID, &operation.Namespace, &operation.NamespaceUID, &operation.ClusterID,
		&operation.BackendUID, &operation.AuthorityEpoch, &operation.RoutingEpoch, &operation.MemoryID,
		&operation.ProposalID, &operation.Kind, &operation.DesiredGeneration, &operation.ExpectedMaterializedGeneration,
		&operation.ExpectedBackendVersion, &operation.OperationIdempotencyKey, &operation.MutationIdempotencyKey,
		&operation.RequestDigest, &operation.MutationDigest, &operation.ContentDigest, &operation.PayloadBytes,
		&operation.State, &operation.LeaseOwner, &operation.LeaseEpoch, &operation.LeaseOriginState, &leaseExpiresAt, &sendStartedAt,
		&requestDeadline, &operation.Dispatches, &operation.Attempts, &operation.NextRetryAt, &operation.MaxAgeAt, &operation.PayloadRetainUntil,
		&operation.ErrorCode, &operation.ErrorMessage, &operation.ReceiptBindingDigest,
		&operation.ReceiptAppliedGeneration, &operation.ReceiptBackendVersion, &operation.ReceiptBackendMemoryID,
		&operation.ReceiptContentDigest, &operation.ReceiptMutationDigest, &receiptCompletedAt,
		&operation.Actor, &operation.Reason, &operation.CreatedAt, &operation.UpdatedAt, &completedAt,
	}
	if withPayload {
		dest = append(dest, &payload)
	}
	if err := scanner.Scan(dest...); err != nil {
		return nil, nil, err
	}
	if leaseExpiresAt.Valid {
		operation.LeaseExpiresAt = &leaseExpiresAt.Time
	}
	if sendStartedAt.Valid {
		operation.SendStartedAt = &sendStartedAt.Time
	}
	if requestDeadline.Valid {
		operation.RequestDeadline = &requestDeadline.Time
	}
	if receiptCompletedAt.Valid {
		operation.ReceiptCompletedAt = &receiptCompletedAt.Time
	}
	if completedAt.Valid {
		operation.CompletedAt = &completedAt.Time
	}
	return &operation, append([]byte(nil), payload...), nil
}
