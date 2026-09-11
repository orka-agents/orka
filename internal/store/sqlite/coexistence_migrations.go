/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package sqlite

import (
	"database/sql"
	"fmt"
)

// migrateAgentExecution creates the immutable encrypted execution snapshots,
// Session protocol/runtime lineage, and durable harness v1 attempt aggregates.
// All statements are idempotent and run inside one transaction.
func migrateAgentExecution(db *sql.DB) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS agent_execution_snapshots (
			task_uid        TEXT NOT NULL,
			digest          TEXT NOT NULL,
			schema_version  INTEGER NOT NULL CHECK(schema_version > 0),
			nonce           BLOB NOT NULL,
			ciphertext      BLOB NOT NULL,
			created_at      TIMESTAMP NOT NULL,
			PRIMARY KEY (task_uid, digest)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_agent_execution_snapshots_task
			ON agent_execution_snapshots(task_uid, created_at ASC)`,

		`CREATE TABLE IF NOT EXISTS session_lineages (
			namespace          TEXT NOT NULL,
			session_name       TEXT NOT NULL,
			namespace_uid      TEXT NOT NULL,
			session_uid        TEXT NOT NULL,
			contract_version   TEXT NOT NULL CHECK(contract_version IN ('orka.harness.v1','orka.harness.v2')),
			lineage_generation INTEGER NOT NULL CHECK(lineage_generation > 0),
			runtime_identity   TEXT NOT NULL,
			config_digest      TEXT NOT NULL DEFAULT '',
			version            INTEGER NOT NULL CHECK(version > 0),
			created_at         TIMESTAMP NOT NULL,
			updated_at         TIMESTAMP NOT NULL,
			PRIMARY KEY (namespace, session_name),
			UNIQUE (session_uid)
		)`,
		`CREATE TABLE IF NOT EXISTS harness_v1_attempts (
			id                           TEXT PRIMARY KEY,
			namespace                    TEXT NOT NULL,
			task_name                    TEXT NOT NULL DEFAULT '',
			task_uid                     TEXT NOT NULL,
			attempt                      INTEGER NOT NULL CHECK(attempt > 0),
			binding_digest               TEXT NOT NULL,
			snapshot_digest              TEXT NOT NULL,
			request_digest               TEXT NOT NULL,
			turn_id                      TEXT NOT NULL,
			runtime_session_id           TEXT NOT NULL DEFAULT '',
			correlation_id               TEXT NOT NULL DEFAULT '',
			backend                      TEXT NOT NULL DEFAULT '',
			backend_endpoint             TEXT NOT NULL DEFAULT '',
			auth_secret_namespace        TEXT NOT NULL DEFAULT '',
			auth_secret_name             TEXT NOT NULL DEFAULT '',
			auth_secret_key              TEXT NOT NULL DEFAULT '',
			auth_secret_uid              TEXT NOT NULL DEFAULT '',
			auth_secret_resource_version TEXT NOT NULL DEFAULT '',
			state                        TEXT NOT NULL CHECK(state IN (
				'Prepared','Submitting','Rejected','SubmittedUnknown','Accepted','Running',
				'CancelRequested','Settling','Succeeded','Failed','Cancelled','OutcomeUnknown')),
			last_event_seq               INTEGER NOT NULL DEFAULT 0,
			cancel_requested_at          TIMESTAMP,
			terminal_receipt_digest      TEXT NOT NULL DEFAULT '',
			terminal_reason              TEXT NOT NULL DEFAULT '',
			duplicate_safe               BOOLEAN NOT NULL DEFAULT FALSE,
			retry_class                  TEXT NOT NULL CHECK(retry_class IN ('none','duplicate-safe')),
			controller_epoch_name        TEXT NOT NULL DEFAULT '',
			controller_epoch             INTEGER NOT NULL DEFAULT 0,
			last_operation_id            TEXT NOT NULL DEFAULT '',
			last_operation_digest        TEXT NOT NULL DEFAULT '',
			version                      INTEGER NOT NULL CHECK(version > 0),
			created_at                   TIMESTAMP NOT NULL,
			updated_at                   TIMESTAMP NOT NULL,
			UNIQUE (namespace, task_uid, attempt)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_harness_v1_attempts_task
			ON harness_v1_attempts(namespace, task_uid, attempt ASC)`,
		`CREATE INDEX IF NOT EXISTS idx_harness_v1_attempts_active
			ON harness_v1_attempts(state, updated_at ASC)
			WHERE state NOT IN ('Rejected','Succeeded','Failed','Cancelled','OutcomeUnknown')`,
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin coexistence migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			return fmt.Errorf("agent execution migration failed: %w", err)
		}
	}
	if err := migrateSessionLineagesWithoutProvenance(tx); err != nil {
		return fmt.Errorf("agent execution migration failed: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit agent execution migration: %w", err)
	}
	return nil
}

func migrateSessionLineagesWithoutProvenance(tx *sql.Tx) error {
	columns, err := tx.Query(`PRAGMA table_info(session_lineages)`)
	if err != nil {
		return fmt.Errorf("inspect session lineage schema: %w", err)
	}
	hasProvenance := false
	for columns.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := columns.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			_ = columns.Close()
			return fmt.Errorf("scan session lineage schema: %w", err)
		}
		hasProvenance = hasProvenance || name == "provenance"
	}
	if err := columns.Err(); err != nil {
		_ = columns.Close()
		return fmt.Errorf("inspect session lineage schema: %w", err)
	}
	if err := columns.Close(); err != nil {
		return fmt.Errorf("close session lineage schema: %w", err)
	}
	if !hasProvenance {
		return nil
	}

	if _, err := tx.Exec(`DROP TABLE IF EXISTS session_lineages_static_mode`); err != nil {
		return fmt.Errorf("drop stale static session lineage migration table: %w", err)
	}
	if _, err := tx.Exec(`CREATE TABLE session_lineages_static_mode (
		namespace          TEXT NOT NULL,
		session_name       TEXT NOT NULL,
		namespace_uid      TEXT NOT NULL,
		session_uid        TEXT NOT NULL,
		contract_version   TEXT NOT NULL CHECK(contract_version IN ('orka.harness.v1','orka.harness.v2')),
		lineage_generation INTEGER NOT NULL CHECK(lineage_generation > 0),
		runtime_identity   TEXT NOT NULL,
		config_digest      TEXT NOT NULL DEFAULT '',
		version            INTEGER NOT NULL CHECK(version > 0),
		created_at         TIMESTAMP NOT NULL,
		updated_at         TIMESTAMP NOT NULL,
		PRIMARY KEY (namespace, session_name),
		UNIQUE (session_uid)
	)`); err != nil {
		return fmt.Errorf("create static session lineage migration table: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO session_lineages_static_mode (
		namespace, session_name, namespace_uid, session_uid, contract_version,
		lineage_generation, runtime_identity, config_digest, version, created_at, updated_at
	) SELECT namespace, session_name, namespace_uid, session_uid, contract_version,
		lineage_generation, runtime_identity, config_digest, version, created_at, updated_at
		FROM session_lineages`); err != nil {
		return fmt.Errorf("copy static session lineages: %w", err)
	}
	if _, err := tx.Exec(`DROP TABLE session_lineages`); err != nil {
		return fmt.Errorf("drop legacy session lineage table: %w", err)
	}
	if _, err := tx.Exec(`ALTER TABLE session_lineages_static_mode RENAME TO session_lineages`); err != nil {
		return fmt.Errorf("rename static session lineage table: %w", err)
	}
	return nil
}
