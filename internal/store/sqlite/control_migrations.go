package sqlite

import (
	"database/sql"
	"fmt"
)

// migrateControlStore installs the durable ACP-core control-store foundation.
// It is intentionally called from migrate after the legacy session/transcript
// tables exist because session_controls has a preserving foreign key to them.
func migrateControlStore(db *sql.DB) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS controller_epochs (
			name           TEXT PRIMARY KEY,
			epoch          INTEGER NOT NULL CHECK(epoch > 0),
			holder_id      TEXT NOT NULL,
			request_digest TEXT NOT NULL,
			version        INTEGER NOT NULL CHECK(version > 0),
			acquired_at    TIMESTAMP NOT NULL,
			updated_at     TIMESTAMP NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS prompt_attempts (
			id                       TEXT PRIMARY KEY,
			namespace                TEXT NOT NULL,
			task_uid                 TEXT NOT NULL,
			attempt                  INTEGER NOT NULL CHECK(attempt > 0),
			prompt_id                TEXT NOT NULL,
			session_uid              TEXT NOT NULL DEFAULT '',
			session_lease_generation INTEGER NOT NULL DEFAULT 0 CHECK(session_lease_generation >= 0),
			runtime_instance_id      TEXT NOT NULL DEFAULT '',
			request_digest           TEXT NOT NULL,
			binding_digest           TEXT NOT NULL DEFAULT '',
			snapshot_digest          TEXT NOT NULL DEFAULT '',
			execution_state          TEXT NOT NULL CHECK(execution_state IN (
				'Queued','Reserved','SessionStarting','Planned','Submitting','SubmittedUnknown',
				'Accepted','Running','Settling','Succeeded','Failed','Cancelled','OutcomeUnknown'
			)),
			delivery_state           TEXT NOT NULL CHECK(delivery_state IN (
				'NotRequested','Validating','Preparing','Prepared','Publishing','Verifying',
				'VerifiedExact','DeliveredSuperseded','ReadValidated','NoChange','CancelledBeforePublish',
				'ReadOnlyWorkspaceModified','DeliveryConflict','CredentialBlocked','PublicationOutcomeUnknown'
			)),
			terminal_reason          TEXT NOT NULL DEFAULT '',
			outcome_marker           TEXT NOT NULL DEFAULT '',
			controller_epoch_name    TEXT NOT NULL,
			controller_epoch         INTEGER NOT NULL CHECK(controller_epoch > 0),
			last_operation_id        TEXT NOT NULL DEFAULT '',
			last_operation_digest    TEXT NOT NULL DEFAULT '',
			version                  INTEGER NOT NULL CHECK(version > 0),
			created_at               TIMESTAMP NOT NULL,
			updated_at               TIMESTAMP NOT NULL,
			UNIQUE(namespace, task_uid, attempt, prompt_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_prompt_attempts_task
			ON prompt_attempts(namespace, task_uid, attempt DESC, prompt_id)`,
		`CREATE INDEX IF NOT EXISTS idx_prompt_attempts_state
			ON prompt_attempts(namespace, execution_state, delivery_state, updated_at)`,
		`CREATE TABLE IF NOT EXISTS prompt_attempt_reclamations (
			namespace                             TEXT NOT NULL,
			task_name                             TEXT NOT NULL,
			task_uid                              TEXT NOT NULL,
			mode                                  TEXT NOT NULL CHECK(mode IN ('Projected','Unbound','NoAttempt')),
			continuity_session                    BOOLEAN NOT NULL,
			final_continuity_session              BOOLEAN NOT NULL,
			final_prompt_attempt_id               TEXT NOT NULL DEFAULT '',
			terminal_projection_id                TEXT NOT NULL DEFAULT '',
			final_session_turn_id                 TEXT NOT NULL DEFAULT '',
			terminal_projection_aggregate_kind    TEXT NOT NULL DEFAULT '',
			terminal_projection_aggregate_id      TEXT NOT NULL DEFAULT '',
			candidate_ids                         BLOB NOT NULL,
			requested_external_effect_aggregate_ids BLOB NOT NULL,
			related_external_effect_aggregate_ids BLOB NOT NULL,
			controller_epoch_name                 TEXT NOT NULL,
			controller_epoch                      INTEGER NOT NULL CHECK(controller_epoch > 0),
			created_at                            TIMESTAMP NOT NULL,
			PRIMARY KEY(namespace, task_uid)
		)`,
		`CREATE TABLE IF NOT EXISTS session_controls (
			namespace                 TEXT NOT NULL,
			session_name              TEXT NOT NULL,
			session_uid               TEXT NOT NULL UNIQUE,
			request_digest            TEXT NOT NULL,
			availability              TEXT NOT NULL CHECK(availability IN ('Available','ReconciliationBlocked')),
			lease_generation          INTEGER NOT NULL DEFAULT 0 CHECK(lease_generation >= 0),
			lease_task_uid            TEXT NOT NULL DEFAULT '',
			lease_attempt             INTEGER NOT NULL DEFAULT 0 CHECK(lease_attempt >= 0),
			lease_prompt_id           TEXT NOT NULL DEFAULT '',
			lease_request_digest      TEXT NOT NULL DEFAULT '',
			lease_acquired_at         TIMESTAMP,
			lease_expires_at          TIMESTAMP,
			blocked_reason            TEXT NOT NULL DEFAULT '',
			related_prompt_attempt_id TEXT NOT NULL DEFAULT '',
			related_publication_id    TEXT NOT NULL DEFAULT '',
			verified_repository_id    TEXT NOT NULL DEFAULT '',
			verified_ref              TEXT NOT NULL DEFAULT '',
			verified_sha              TEXT NOT NULL DEFAULT '',
			controller_epoch_name     TEXT NOT NULL,
			controller_epoch          INTEGER NOT NULL CHECK(controller_epoch > 0),
			last_operation_id         TEXT NOT NULL DEFAULT '',
			last_operation_digest     TEXT NOT NULL DEFAULT '',
			version                   INTEGER NOT NULL CHECK(version > 0),
			created_at                TIMESTAMP NOT NULL,
			updated_at                TIMESTAMP NOT NULL,
			PRIMARY KEY(namespace, session_name),
			FOREIGN KEY(namespace, session_name) REFERENCES sessions(namespace, name) ON DELETE RESTRICT,
			CHECK((lease_task_uid = '' AND lease_attempt = 0 AND lease_prompt_id = '' AND lease_request_digest = '' AND lease_acquired_at IS NULL AND lease_expires_at IS NULL)
			   OR (lease_task_uid <> '' AND lease_attempt > 0 AND lease_prompt_id <> '' AND lease_request_digest <> '' AND lease_acquired_at IS NOT NULL)),
			CHECK((availability = 'Available' AND blocked_reason = '') OR availability = 'ReconciliationBlocked'),
			CHECK((verified_repository_id = '' AND verified_ref = '' AND verified_sha = '')
			   OR (verified_repository_id <> '' AND verified_ref <> '' AND verified_sha <> ''))
		)`,
		`CREATE INDEX IF NOT EXISTS idx_session_controls_availability
			ON session_controls(namespace, availability, updated_at)`,
		`CREATE TABLE IF NOT EXISTS session_turns (
			id                    TEXT PRIMARY KEY,
			namespace             TEXT NOT NULL,
			session_name          TEXT NOT NULL,
			session_uid           TEXT NOT NULL,
			lease_generation      INTEGER NOT NULL CHECK(lease_generation > 0),
			task_uid              TEXT NOT NULL,
			attempt               INTEGER NOT NULL CHECK(attempt > 0),
			prompt_id             TEXT NOT NULL,
			prompt_attempt_id     TEXT NOT NULL,
			request_digest        TEXT NOT NULL,
			user_prompt           TEXT NOT NULL,
			state                 TEXT NOT NULL CHECK(state IN ('Open','Finalized')),
			terminal_kind         TEXT NOT NULL DEFAULT '' CHECK(terminal_kind IN ('','AssistantResult','OutcomeMarker')),
			terminal_content      TEXT NOT NULL DEFAULT '',
			finalization_digest   TEXT NOT NULL DEFAULT '',
			publication_id        TEXT NOT NULL DEFAULT '',
			publication_receipt   BLOB,
			projection_id         TEXT NOT NULL DEFAULT '',
			projection_kind       TEXT NOT NULL DEFAULT '',
			projection_digest     TEXT NOT NULL DEFAULT '',
			projection_available_at TIMESTAMP,
			controller_epoch_name TEXT NOT NULL,
			controller_epoch      INTEGER NOT NULL CHECK(controller_epoch > 0),
			version               INTEGER NOT NULL CHECK(version > 0),
			created_at            TIMESTAMP NOT NULL,
			finalized_at          TIMESTAMP,
			updated_at            TIMESTAMP NOT NULL,
			UNIQUE(session_uid, lease_generation, task_uid, attempt, prompt_id),
			FOREIGN KEY(namespace, session_name) REFERENCES sessions(namespace, name) ON DELETE RESTRICT,
			CHECK((state = 'Open' AND terminal_kind = '' AND finalization_digest = '' AND finalized_at IS NULL)
			   OR (state = 'Finalized' AND terminal_kind <> '' AND finalization_digest <> '' AND finalized_at IS NOT NULL))
		)`,
		`CREATE INDEX IF NOT EXISTS idx_session_turns_session
			ON session_turns(session_uid, lease_generation DESC)`,
		`CREATE TABLE IF NOT EXISTS branch_claims (
			id                     TEXT PRIMARY KEY,
			repository_id          TEXT NOT NULL,
			ref                    TEXT NOT NULL,
			owner_kind             TEXT NOT NULL CHECK(owner_kind IN ('Task','Session')),
			owner_uid              TEXT NOT NULL,
			generation             INTEGER NOT NULL CHECK(generation > 0),
			last_verified_absent   BOOLEAN NOT NULL,
			last_verified_sha      TEXT NOT NULL DEFAULT '',
			availability           TEXT NOT NULL CHECK(availability IN ('Available','ReconciliationBlocked')),
			blocked_reason         TEXT NOT NULL DEFAULT '',
			related_publication_id TEXT NOT NULL DEFAULT '',
			request_digest         TEXT NOT NULL,
			controller_epoch_name  TEXT NOT NULL,
			controller_epoch       INTEGER NOT NULL CHECK(controller_epoch > 0),
			last_operation_id      TEXT NOT NULL DEFAULT '',
			last_operation_digest  TEXT NOT NULL DEFAULT '',
			version                INTEGER NOT NULL CHECK(version > 0),
			created_at             TIMESTAMP NOT NULL,
			updated_at             TIMESTAMP NOT NULL,
			UNIQUE(repository_id, ref),
			CHECK((last_verified_absent = TRUE AND last_verified_sha = '')
			   OR (last_verified_absent = FALSE AND last_verified_sha <> '')),
			CHECK((availability = 'Available' AND blocked_reason = '') OR availability = 'ReconciliationBlocked')
		)`,
		`CREATE INDEX IF NOT EXISTS idx_branch_claims_owner
			ON branch_claims(owner_kind, owner_uid, availability)`,
		`CREATE TABLE IF NOT EXISTS publications (
			id                         TEXT PRIMARY KEY,
			namespace                  TEXT NOT NULL,
			generation                 INTEGER NOT NULL CHECK(generation > 0),
			task_uid                   TEXT NOT NULL,
			attempt                    INTEGER NOT NULL CHECK(attempt > 0),
			prompt_id                  TEXT NOT NULL,
			session_uid                TEXT NOT NULL DEFAULT '',
			branch_claim_id            TEXT NOT NULL,
			branch_claim_generation    INTEGER NOT NULL CHECK(branch_claim_generation > 0),
			source_repository_id       TEXT NOT NULL,
			source_ref                 TEXT NOT NULL,
			source_baseline_sha        TEXT NOT NULL,
			target_repository_id       TEXT NOT NULL,
			target_ref                 TEXT NOT NULL,
			baseline_absent            BOOLEAN NOT NULL,
			baseline_sha               TEXT NOT NULL DEFAULT '',
			artifact_id                TEXT NOT NULL,
			artifact_digest            TEXT NOT NULL,
			artifact_size_bytes        INTEGER NOT NULL CHECK(artifact_size_bytes > 0),
			artifact_media_type        TEXT NOT NULL,
			publication_credential_ref TEXT NOT NULL,
			commit_identity            TEXT NOT NULL,
			commit_message             TEXT NOT NULL,
			commit_timestamp           TIMESTAMP NOT NULL,
			pr_intent                  BLOB,
			request_digest             TEXT NOT NULL,
			state                      TEXT NOT NULL CHECK(state IN (
				'Preparing','Prepared','Publishing','Verifying','VerifiedExact','DeliveredSuperseded',
				'CancelledBeforePublish','DeliveryConflict','CredentialBlocked','PreparationFailed','PublicationOutcomeUnknown'
			)),
			prepared_receipt           BLOB,
			publish_receipt            BLOB,
			verification_receipt       BLOB,
			pr_receipt                 BLOB,
			terminal_reason            TEXT NOT NULL DEFAULT '',
			controller_epoch_name      TEXT NOT NULL,
			controller_epoch           INTEGER NOT NULL CHECK(controller_epoch > 0),
			last_operation_id          TEXT NOT NULL DEFAULT '',
			last_operation_digest      TEXT NOT NULL DEFAULT '',
			version                    INTEGER NOT NULL CHECK(version > 0),
			created_at                 TIMESTAMP NOT NULL,
			updated_at                 TIMESTAMP NOT NULL,
			FOREIGN KEY(branch_claim_id) REFERENCES branch_claims(id) ON DELETE RESTRICT,
			CHECK((baseline_absent = TRUE AND baseline_sha = '')
			   OR (baseline_absent = FALSE AND baseline_sha <> ''))
		)`,
		`CREATE INDEX IF NOT EXISTS idx_publications_task
			ON publications(namespace, task_uid, attempt, prompt_id, generation)`,
		`CREATE INDEX IF NOT EXISTS idx_publications_reconcile
			ON publications(state, updated_at)`,
		`CREATE TABLE IF NOT EXISTS external_effects (
			id                    TEXT PRIMARY KEY,
			kind                  TEXT NOT NULL,
			namespace             TEXT NOT NULL,
			aggregate_id          TEXT NOT NULL,
			operation_id          TEXT NOT NULL,
			request_digest        TEXT NOT NULL,
			state                 TEXT NOT NULL CHECK(state IN ('Pending','InFlight','Succeeded','Failed','OutcomeUnknown')),
			response_digest       TEXT NOT NULL DEFAULT '',
			response              BLOB,
			lease_owner           TEXT NOT NULL DEFAULT '',
			lease_expires_at      TIMESTAMP,
			attempts              INTEGER NOT NULL DEFAULT 0 CHECK(attempts >= 0),
			controller_epoch_name TEXT NOT NULL,
			controller_epoch      INTEGER NOT NULL CHECK(controller_epoch > 0),
			version               INTEGER NOT NULL CHECK(version > 0),
			created_at            TIMESTAMP NOT NULL,
			updated_at            TIMESTAMP NOT NULL,
			UNIQUE(kind, namespace, aggregate_id, operation_id),
			CHECK((lease_owner = '' AND lease_expires_at IS NULL) OR (lease_owner <> '' AND lease_expires_at IS NOT NULL))
		)`,
		`CREATE INDEX IF NOT EXISTS idx_external_effects_reconcile
			ON external_effects(state, lease_expires_at, updated_at)`,
		`CREATE TABLE IF NOT EXISTS outbox_projections (
			id                    TEXT PRIMARY KEY,
			aggregate_kind        TEXT NOT NULL,
			aggregate_id          TEXT NOT NULL,
			projection_kind       TEXT NOT NULL,
			payload_digest        TEXT NOT NULL,
			payload               BLOB NOT NULL,
			state                 TEXT NOT NULL CHECK(state IN ('Pending','Delivering','Delivered','DeadLetter')),
			attempts              INTEGER NOT NULL DEFAULT 0 CHECK(attempts >= 0),
			initial_available_at  TIMESTAMP NOT NULL,
			available_at          TIMESTAMP NOT NULL,
			lease_owner           TEXT NOT NULL DEFAULT '',
			lease_expires_at      TIMESTAMP,
			delivery_digest       TEXT NOT NULL DEFAULT '',
			last_error            TEXT NOT NULL DEFAULT '',
			last_operation_id     TEXT NOT NULL DEFAULT '',
			last_operation_digest TEXT NOT NULL DEFAULT '',
			controller_epoch_name TEXT NOT NULL,
			controller_epoch      INTEGER NOT NULL CHECK(controller_epoch > 0),
			version               INTEGER NOT NULL CHECK(version > 0),
			created_at            TIMESTAMP NOT NULL,
			updated_at            TIMESTAMP NOT NULL,
			delivered_at          TIMESTAMP,
			CHECK((state = 'Delivering' AND lease_owner <> '' AND lease_expires_at IS NOT NULL)
			   OR (state <> 'Delivering' AND lease_owner = '' AND lease_expires_at IS NULL)),
			CHECK((state = 'Delivered' AND delivered_at IS NOT NULL)
			   OR (state <> 'Delivered' AND delivered_at IS NULL))
		)`,
		`CREATE INDEX IF NOT EXISTS idx_outbox_projections_due
			ON outbox_projections(state, available_at, lease_expires_at, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_outbox_projections_aggregate
			ON outbox_projections(aggregate_kind, aggregate_id, projection_kind)`,
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("control-store migration failed: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			return fmt.Errorf("control-store migration failed: %w", err)
		}
	}
	if err := migrateSessionTurnsToKubernetesAuthority(tx); err != nil {
		return fmt.Errorf("control-store migration failed: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("control-store migration failed: %w", err)
	}
	if err := ensureSQLiteColumns(db, "prompt_attempts", []sqliteColumnMigration{
		{Name: "binding_digest", Definition: "binding_digest TEXT NOT NULL DEFAULT ''"},
		{Name: "snapshot_digest", Definition: "snapshot_digest TEXT NOT NULL DEFAULT ''"},
	}); err != nil {
		return fmt.Errorf("control-store migration failed: %w", err)
	}
	return nil
}

func migrateSessionTurnsToKubernetesAuthority(tx *sql.Tx) error {
	hasNamespace := false
	hasProjectionBinding := false
	columns, err := tx.Query(`PRAGMA table_info(session_turns)`)
	if err != nil {
		return fmt.Errorf("inspect session_turns columns: %w", err)
	}
	for columns.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := columns.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			_ = columns.Close()
			return fmt.Errorf("scan session_turns column: %w", err)
		}
		if name == "namespace" {
			hasNamespace = true
		}
		if name == "projection_id" {
			hasProjectionBinding = true
		}
	}
	if err := columns.Err(); err != nil {
		_ = columns.Close()
		return fmt.Errorf("inspect session_turns columns: %w", err)
	}
	if err := columns.Close(); err != nil {
		return fmt.Errorf("close session_turns columns: %w", err)
	}

	referencesControlRows := false
	foreignKeys, err := tx.Query(`PRAGMA foreign_key_list(session_turns)`)
	if err != nil {
		return fmt.Errorf("inspect session_turns foreign keys: %w", err)
	}
	for foreignKeys.Next() {
		var id, seq int
		var table, from, to, onUpdate, onDelete, match string
		if err := foreignKeys.Scan(&id, &seq, &table, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			_ = foreignKeys.Close()
			return fmt.Errorf("scan session_turns foreign key: %w", err)
		}
		if table == "session_controls" {
			referencesControlRows = true
		}
	}
	if err := foreignKeys.Err(); err != nil {
		_ = foreignKeys.Close()
		return fmt.Errorf("inspect session_turns foreign keys: %w", err)
	}
	if err := foreignKeys.Close(); err != nil {
		return fmt.Errorf("close session_turns foreign keys: %w", err)
	}
	if hasNamespace && hasProjectionBinding && !referencesControlRows {
		return nil
	}

	if _, err := tx.Exec(`DROP TABLE IF EXISTS session_turns_kubernetes_authoritative`); err != nil {
		return fmt.Errorf("drop stale session_turns migration table: %w", err)
	}
	if _, err := tx.Exec(`CREATE TABLE session_turns_kubernetes_authoritative (
		id                    TEXT PRIMARY KEY,
		namespace             TEXT NOT NULL,
		session_name          TEXT NOT NULL,
		session_uid           TEXT NOT NULL,
		lease_generation      INTEGER NOT NULL CHECK(lease_generation > 0),
		task_uid              TEXT NOT NULL,
		attempt               INTEGER NOT NULL CHECK(attempt > 0),
		prompt_id             TEXT NOT NULL,
		prompt_attempt_id     TEXT NOT NULL,
		request_digest        TEXT NOT NULL,
		user_prompt           TEXT NOT NULL,
		state                 TEXT NOT NULL CHECK(state IN ('Open','Finalized')),
		terminal_kind         TEXT NOT NULL DEFAULT '' CHECK(terminal_kind IN ('','AssistantResult','OutcomeMarker')),
		terminal_content      TEXT NOT NULL DEFAULT '',
		finalization_digest   TEXT NOT NULL DEFAULT '',
		publication_id        TEXT NOT NULL DEFAULT '',
		publication_receipt   BLOB,
		projection_id         TEXT NOT NULL DEFAULT '',
		projection_kind       TEXT NOT NULL DEFAULT '',
		projection_digest     TEXT NOT NULL DEFAULT '',
		projection_available_at TIMESTAMP,
		controller_epoch_name TEXT NOT NULL,
		controller_epoch      INTEGER NOT NULL CHECK(controller_epoch > 0),
		version               INTEGER NOT NULL CHECK(version > 0),
		created_at            TIMESTAMP NOT NULL,
		finalized_at          TIMESTAMP,
		updated_at            TIMESTAMP NOT NULL,
		UNIQUE(session_uid, lease_generation, task_uid, attempt, prompt_id),
		FOREIGN KEY(namespace, session_name) REFERENCES sessions(namespace, name) ON DELETE RESTRICT,
		CHECK((state = 'Open' AND terminal_kind = '' AND finalization_digest = '' AND finalized_at IS NULL)
		   OR (state = 'Finalized' AND terminal_kind <> '' AND finalization_digest <> '' AND finalized_at IS NOT NULL))
	)`); err != nil {
		return fmt.Errorf("create Kubernetes-authoritative session_turns table: %w", err)
	}

	var sourceCount int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM session_turns`).Scan(&sourceCount); err != nil {
		return fmt.Errorf("count existing session turns: %w", err)
	}
	copyStatement := `INSERT INTO session_turns_kubernetes_authoritative(
		id, namespace, session_name, session_uid, lease_generation, task_uid, attempt, prompt_id,
		prompt_attempt_id, request_digest, user_prompt, state, terminal_kind, terminal_content,
		finalization_digest, publication_id, publication_receipt, projection_id, projection_kind,
		projection_digest, projection_available_at, controller_epoch_name,
		controller_epoch, version, created_at, finalized_at, updated_at
	)`
	if hasNamespace && hasProjectionBinding {
		copyStatement += ` SELECT id, namespace, session_name, session_uid, lease_generation, task_uid, attempt, prompt_id,
			prompt_attempt_id, request_digest, user_prompt, state, terminal_kind, terminal_content,
			finalization_digest, publication_id, publication_receipt, projection_id, projection_kind,
			projection_digest, projection_available_at, controller_epoch_name,
			controller_epoch, version, created_at, finalized_at, updated_at FROM session_turns`
	} else if hasNamespace {
		copyStatement += ` SELECT id, namespace, session_name, session_uid, lease_generation, task_uid, attempt, prompt_id,
			prompt_attempt_id, request_digest, user_prompt, state, terminal_kind, terminal_content,
			finalization_digest, publication_id, publication_receipt, '', '', '', NULL, controller_epoch_name,
			controller_epoch, version, created_at, finalized_at, updated_at FROM session_turns`
	} else {
		copyStatement += ` SELECT turns.id, controls.namespace, controls.session_name, turns.session_uid,
			turns.lease_generation, turns.task_uid, turns.attempt, turns.prompt_id, turns.prompt_attempt_id,
			turns.request_digest, turns.user_prompt, turns.state, turns.terminal_kind, turns.terminal_content,
			turns.finalization_digest, turns.publication_id, turns.publication_receipt, '', '', '', NULL,
			turns.controller_epoch_name, turns.controller_epoch, turns.version, turns.created_at,
			turns.finalized_at, turns.updated_at
			FROM session_turns AS turns JOIN session_controls AS controls ON controls.session_uid = turns.session_uid`
	}
	if _, err := tx.Exec(copyStatement); err != nil {
		return fmt.Errorf("copy existing session turns: %w", err)
	}
	var copiedCount int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM session_turns_kubernetes_authoritative`).Scan(&copiedCount); err != nil {
		return fmt.Errorf("count copied session turns: %w", err)
	}
	if copiedCount != sourceCount {
		return fmt.Errorf("copied %d of %d existing session turns", copiedCount, sourceCount)
	}
	if _, err := tx.Exec(`DROP TABLE session_turns`); err != nil {
		return fmt.Errorf("drop legacy session_turns table: %w", err)
	}
	if _, err := tx.Exec(`ALTER TABLE session_turns_kubernetes_authoritative RENAME TO session_turns`); err != nil {
		return fmt.Errorf("rename Kubernetes-authoritative session_turns table: %w", err)
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_session_turns_session ON session_turns(session_uid, lease_generation DESC)`); err != nil {
		return fmt.Errorf("recreate session_turns index: %w", err)
	}
	return nil
}
