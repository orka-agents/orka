/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package sqlite

import (
	"database/sql"
	"fmt"
)

// migrateAgentExecutionCoexistence creates the harness v1/v2 coexistence
// payload tables: immutable encrypted execution snapshots, Session
// protocol/runtime lineage, and durable harness v1 attempt aggregates. All
// statements are idempotent and run inside one transaction. Kubernetes control
// CRDs and coordination Leases remain authoritative for lifecycle and fences;
// these tables are payload persistence.
func migrateAgentExecutionCoexistence(db *sql.DB) error {
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
			provenance         TEXT NOT NULL CHECK(provenance IN ('first-use','legacy-adopted','transcript-bootstrap')),
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
			return fmt.Errorf("coexistence migration failed: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit coexistence migration: %w", err)
	}
	return nil
}
