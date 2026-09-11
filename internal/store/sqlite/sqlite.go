package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	_ "modernc.org/sqlite" // SQLite driver registration
)

var (
	dbSizeBytes = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "orka_store_db_size_bytes",
			Help: "Size of the SQLite database file in bytes",
		},
	)
)

func init() {
	metrics.Registry.MustRegister(dbSizeBytes)
}

// NewDB opens a SQLite database with recommended pragmas for WAL mode,
// busy timeout, synchronous mode, and foreign keys.
func NewDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}

	// Single writer — SQLite only supports one concurrent writer.
	// WAL mode allows concurrent reads alongside the single writer.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0) // keep connection alive

	// Set pragmas per-connection. These are not persistent in SQLite and
	// must be set on each connection. With MaxOpenConns(1), we have exactly one.
	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA foreign_keys=ON",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			db.Close() //nolint:errcheck
			return nil, fmt.Errorf("failed to set pragma %q: %w", p, err)
		}
	}

	if err := initializeSchema(db); err != nil {
		db.Close() //nolint:errcheck
		return nil, fmt.Errorf("failed to initialize SQLite schema: %w", err)
	}

	return db, nil
}

// currentSchemaStatements defines the complete supported SQLite layout.
func currentSchemaStatements() []string {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS task_data_cleanup_generations (
			namespace TEXT PRIMARY KEY,
			generation INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS task_data_task_generations (
			namespace TEXT NOT NULL,
			task_name TEXT NOT NULL,
			generation INTEGER NOT NULL,
			PRIMARY KEY (namespace, task_name)
		)`,
		`CREATE TABLE IF NOT EXISTS task_job_revocations (
			namespace TEXT NOT NULL,
			task_uid TEXT NOT NULL,
			job_uid TEXT NOT NULL,
			PRIMARY KEY (namespace, task_uid, job_uid)
		)`,
		`CREATE TABLE IF NOT EXISTS results (
			namespace  TEXT NOT NULL,
			task_name  TEXT NOT NULL,
			data       BLOB NOT NULL,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (namespace, task_name)
		)`,
		`CREATE TABLE IF NOT EXISTS prompt_result_receipts (
			attempt_id       TEXT NOT NULL PRIMARY KEY,
			namespace        TEXT NOT NULL,
			task_name        TEXT NOT NULL,
			operation_id     TEXT NOT NULL,
			operation_digest TEXT NOT NULL,
			data             BLOB NOT NULL,
			created_at       TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_prompt_result_receipts_task
			ON prompt_result_receipts(namespace, task_name)`,
		`CREATE TABLE IF NOT EXISTS sessions (
			namespace     TEXT NOT NULL,
			name          TEXT NOT NULL,
			session_type  TEXT NOT NULL DEFAULT 'task',
			owner_type    TEXT NOT NULL DEFAULT '',
			owner_ref     TEXT NOT NULL DEFAULT '',
			active_task            TEXT NOT NULL DEFAULT '',
			active_task_uid        TEXT NOT NULL DEFAULT '',
			active_task_expires_at TIMESTAMP,
			control_session_uid    TEXT NOT NULL DEFAULT '',
			message_count   INTEGER NOT NULL DEFAULT 0,
			input_tokens  INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			cancelled     BOOLEAN NOT NULL DEFAULT FALSE,
			created_at    TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at    TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (namespace, name)
		)`,
		`CREATE TABLE IF NOT EXISTS session_cleanup_intents (
			namespace        TEXT NOT NULL,
			session_name     TEXT NOT NULL,
			operation_id     TEXT NOT NULL,
			operation_digest TEXT NOT NULL,
			plan             BLOB NOT NULL,
			created_at       TIMESTAMP NOT NULL,
			PRIMARY KEY(namespace, session_name)
		)`,
		`CREATE TABLE IF NOT EXISTS session_cleanup_completions (
			namespace        TEXT NOT NULL,
			session_name     TEXT NOT NULL,
			session_uid      TEXT NOT NULL DEFAULT '',
			operation_id     TEXT NOT NULL,
			operation_digest TEXT NOT NULL,
			completed_at     TIMESTAMP NOT NULL,
			PRIMARY KEY(namespace, session_name)
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_session_cleanup_completions_uid
			ON session_cleanup_completions(session_uid) WHERE session_uid <> ''`,
		`CREATE TABLE IF NOT EXISTS session_turn_cleanup_receipts (
			turn_id           TEXT PRIMARY KEY,
			prompt_attempt_id TEXT NOT NULL UNIQUE,
			namespace         TEXT NOT NULL,
			session_name      TEXT NOT NULL,
			session_uid       TEXT NOT NULL,
			receipt_digest    TEXT NOT NULL,
			receipt           BLOB NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS session_messages (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			namespace    TEXT NOT NULL,
			session_name TEXT NOT NULL,
			message_id   TEXT NOT NULL DEFAULT '',
			sort_order   INTEGER NOT NULL DEFAULT 0,
			role         TEXT NOT NULL,
			content      TEXT NOT NULL DEFAULT '',
			name         TEXT,
			input        TEXT,
			tool_calls   TEXT,
			tool_call_id TEXT,
			source_type  TEXT NOT NULL DEFAULT '',
			source_ref   TEXT NOT NULL DEFAULT '',
			metadata_json TEXT NOT NULL DEFAULT '{}',
			created_at   TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (namespace, session_name) REFERENCES sessions(namespace, name) ON DELETE CASCADE
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_session_messages_message_id
			ON session_messages(namespace, session_name, message_id) WHERE message_id <> ''`,
		`CREATE INDEX IF NOT EXISTS idx_session_messages_namespace_message_id
			ON session_messages(namespace, message_id) WHERE message_id <> ''`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_session_messages_sort_order
			ON session_messages(namespace, session_name, sort_order) WHERE sort_order > 0`,
		`CREATE TABLE IF NOT EXISTS runtime_sessions (
			id              TEXT NOT NULL,
			namespace       TEXT NOT NULL,
			session_name    TEXT NOT NULL,
			active_task     TEXT NOT NULL DEFAULT '',
			agent_name      TEXT NOT NULL DEFAULT '',
			provider        TEXT NOT NULL,
			state           TEXT NOT NULL,
			cleanup_policy  TEXT NOT NULL,
			idle_timeout_ns INTEGER NOT NULL DEFAULT 0,
			max_lifetime_ns INTEGER NOT NULL DEFAULT 0,
			created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (namespace, id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_runtime_sessions_namespace_updated
			ON runtime_sessions(namespace, updated_at DESC, id ASC)`,
		`CREATE INDEX IF NOT EXISTS idx_runtime_sessions_owner
			ON runtime_sessions(namespace, session_name, provider, state, updated_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_runtime_sessions_active_task
			ON runtime_sessions(namespace, active_task, updated_at DESC)
			WHERE active_task <> ''`,
		`CREATE INDEX IF NOT EXISTS idx_runtime_sessions_cleanup
			ON runtime_sessions(namespace, state, cleanup_policy, updated_at ASC)`,
		`CREATE TABLE IF NOT EXISTS plan_states (
			namespace     TEXT NOT NULL,
			task_name     TEXT NOT NULL,
			iteration     INTEGER NOT NULL DEFAULT 0,
			summary       TEXT NOT NULL DEFAULT '',
			progress_pct  INTEGER NOT NULL DEFAULT 0,
			goal_complete BOOLEAN NOT NULL DEFAULT FALSE,
			plan_document TEXT NOT NULL DEFAULT '',
			created_at    TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at    TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (namespace, task_name)
		)`,
		`CREATE TABLE IF NOT EXISTS execution_events (
			id              TEXT PRIMARY KEY,
			namespace       TEXT NOT NULL,
			stream_type     TEXT NOT NULL,
			stream_id       TEXT NOT NULL,
			seq             INTEGER NOT NULL,
			session_seq     INTEGER NOT NULL DEFAULT 0,
			dedupe_key      TEXT NOT NULL DEFAULT '',
			type            TEXT NOT NULL,
			severity        TEXT NOT NULL DEFAULT 'info',
			task_name       TEXT NOT NULL DEFAULT '',
			session_name    TEXT NOT NULL DEFAULT '',
			agent_name      TEXT NOT NULL DEFAULT '',
			tool_name       TEXT NOT NULL DEFAULT '',
			tool_call_id    TEXT NOT NULL DEFAULT '',
			summary         TEXT NOT NULL DEFAULT '',
			content_json    TEXT,
			content_text    TEXT NOT NULL DEFAULT '',
			truncation_json TEXT,
			created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(namespace, stream_type, stream_id, seq)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_session_messages_order ON session_messages(namespace, session_name, id)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_namespace ON sessions(namespace)`,
		`CREATE INDEX IF NOT EXISTS idx_results_namespace ON results(namespace)`,
		`CREATE INDEX IF NOT EXISTS idx_plan_states_namespace ON plan_states(namespace)`,
		`CREATE INDEX IF NOT EXISTS idx_execution_events_stream_seq
			ON execution_events(namespace, stream_type, stream_id, seq)`,
		`CREATE INDEX IF NOT EXISTS idx_execution_events_type
			ON execution_events(namespace, stream_type, stream_id, type, seq)`,
		`CREATE INDEX IF NOT EXISTS idx_execution_events_task
			ON execution_events(namespace, task_name, seq)`,
		`CREATE INDEX IF NOT EXISTS idx_execution_events_session
			ON execution_events(namespace, session_name, seq)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_execution_events_stream_dedupe_key
			ON execution_events(namespace, stream_type, stream_id, dedupe_key) WHERE dedupe_key <> ''`,
		`CREATE INDEX IF NOT EXISTS idx_execution_events_session_seq
			ON execution_events(namespace, session_name, session_seq)`,
		`CREATE TABLE IF NOT EXISTS execution_event_session_sequences (
			namespace    TEXT NOT NULL,
			session_name TEXT NOT NULL,
			latest_seq   INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (namespace, session_name)
		)`,
		`CREATE TABLE IF NOT EXISTS memories (
			id                 TEXT PRIMARY KEY,
			namespace          TEXT NOT NULL,
			session_name       TEXT NOT NULL DEFAULT '',
			agent_name         TEXT NOT NULL DEFAULT '',
			task_name          TEXT NOT NULL DEFAULT '',
			parent_task        TEXT NOT NULL DEFAULT '',
			source             TEXT NOT NULL DEFAULT '',
			source_proposal_id TEXT NOT NULL DEFAULT '',
			content            TEXT NOT NULL,
			tags_json        TEXT NOT NULL DEFAULT '[]',
			disabled         BOOLEAN NOT NULL DEFAULT FALSE,
			deleted          BOOLEAN NOT NULL DEFAULT FALSE,
			created_at       TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at       TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			last_recalled_at TIMESTAMP,
			recalled_count   INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_memories_recall
			ON memories(namespace, deleted, disabled, updated_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_memories_task ON memories(namespace, task_name)`,
		`CREATE INDEX IF NOT EXISTS idx_memories_agent ON memories(namespace, agent_name)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_memories_source_proposal
			ON memories(namespace, source_proposal_id)
			WHERE source_proposal_id <> ''`,
		`CREATE TABLE IF NOT EXISTS memory_proposals (
			id          TEXT PRIMARY KEY,
			namespace   TEXT NOT NULL,
			task_name   TEXT NOT NULL DEFAULT '',
			agent_name  TEXT NOT NULL DEFAULT '',
			type        TEXT NOT NULL,
			skill_name  TEXT NOT NULL DEFAULT '',
			title       TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			content     TEXT NOT NULL DEFAULT '',
			patch       TEXT NOT NULL DEFAULT '',
			status            TEXT NOT NULL DEFAULT 'pending',
			reviewer          TEXT NOT NULL DEFAULT '',
			review_note       TEXT NOT NULL DEFAULT '',
			applied_memory_id TEXT NOT NULL DEFAULT '',
			applied_by        TEXT NOT NULL DEFAULT '',
			created_at        TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at        TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			reviewed_at       TIMESTAMP,
			applied_at        TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_memory_proposals_status
			ON memory_proposals(namespace, status, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_memory_proposals_task
			ON memory_proposals(namespace, task_name, created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS messages (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			namespace   TEXT NOT NULL,
			from_task   TEXT NOT NULL,
			to_task     TEXT NOT NULL,
			parent_task TEXT NOT NULL,
			content     TEXT NOT NULL,
			read        BOOLEAN NOT NULL DEFAULT FALSE,
			created_at  TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_recipient ON messages(namespace, to_task, read)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_parent ON messages(namespace, parent_task)`,
		`CREATE TABLE IF NOT EXISTS artifacts (
			namespace    TEXT NOT NULL,
			task_name    TEXT NOT NULL,
			filename     TEXT NOT NULL,
			content_type TEXT NOT NULL DEFAULT 'application/octet-stream',
			size         INTEGER NOT NULL,
			data         BLOB NOT NULL,
			created_at   TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (namespace, task_name, filename)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_artifacts_task ON artifacts(namespace, task_name)`,
		`CREATE TABLE IF NOT EXISTS security_scan_runs (
			id              TEXT PRIMARY KEY,
			namespace       TEXT NOT NULL,
			repository_scan TEXT NOT NULL,
			task_name       TEXT NOT NULL,
			mode            TEXT NOT NULL,
			phase           TEXT NOT NULL,
			base_commit     TEXT NOT NULL DEFAULT '',
			head_commit     TEXT NOT NULL DEFAULT '',
			commit_count    INTEGER NOT NULL DEFAULT 0,
			slice_count     INTEGER NOT NULL DEFAULT 0,
			reviewed_slice_count INTEGER NOT NULL DEFAULT 0,
			skipped_slice_count INTEGER NOT NULL DEFAULT 0,
			accepted_findings INTEGER NOT NULL DEFAULT 0,
			dropped_findings INTEGER NOT NULL DEFAULT 0,
			summary         TEXT NOT NULL DEFAULT '',
			error_message   TEXT NOT NULL DEFAULT '',
			started_at      TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			completed_at    TIMESTAMP,
			repository_scan_uid TEXT NOT NULL DEFAULT '',
			repository_scan_generation INTEGER NOT NULL DEFAULT 0,
			cancellation_version INTEGER NOT NULL DEFAULT 0,
			cancellation_pending BOOLEAN NOT NULL DEFAULT FALSE,
			scanner_policy_version TEXT NOT NULL DEFAULT '',
			policy_digest TEXT NOT NULL DEFAULT '',
			idempotency_key TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_security_scan_runs_repo
			ON security_scan_runs(namespace, repository_scan, started_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_security_scan_runs_admission
			ON security_scan_runs(namespace, repository_scan)`,
		`CREATE INDEX IF NOT EXISTS idx_security_scan_runs_active
			ON security_scan_runs(namespace, repository_scan) WHERE phase IN ('pending', 'running')`,
		`CREATE INDEX IF NOT EXISTS idx_security_scan_runs_cancellation
			ON security_scan_runs(namespace, repository_scan) WHERE cancellation_pending = TRUE`,
		`CREATE TABLE IF NOT EXISTS security_scan_task_ingestions (
			namespace TEXT NOT NULL,
			repository_scan TEXT NOT NULL,
			scan_run_id TEXT NOT NULL,
			task_name TEXT NOT NULL,
			task_uid TEXT NOT NULL,
			stage TEXT NOT NULL,
			slice_id TEXT NOT NULL,
			finding_ids_json TEXT NOT NULL DEFAULT '[]',
			dropped_findings_json TEXT NOT NULL DEFAULT '',
			completed BOOLEAN NOT NULL DEFAULT FALSE,
			ingested_at TIMESTAMP NOT NULL,
			PRIMARY KEY (namespace, scan_run_id, task_name, task_uid)
		)`,
		`CREATE TABLE IF NOT EXISTS security_threat_models (
			namespace         TEXT NOT NULL,
			repository_scan   TEXT NOT NULL,
			version           INTEGER NOT NULL,
			content           TEXT NOT NULL,
			source            TEXT NOT NULL,
			generated_by_scan TEXT NOT NULL DEFAULT '',
			created_at        TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at        TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (namespace, repository_scan, version)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_security_threat_models_latest
			ON security_threat_models(namespace, repository_scan, version DESC)`,
		`CREATE TABLE IF NOT EXISTS security_findings (
			id                TEXT PRIMARY KEY,
			namespace         TEXT NOT NULL,
			repository_scan   TEXT NOT NULL,
			scan_run_id       TEXT NOT NULL,
			slice_id          TEXT NOT NULL DEFAULT '',
			fingerprint       TEXT NOT NULL,
			target_key        TEXT NOT NULL DEFAULT '',
			title             TEXT NOT NULL,
			category          TEXT NOT NULL DEFAULT '',
			summary           TEXT NOT NULL,
			severity          TEXT NOT NULL,
			confidence        TEXT NOT NULL,
			triage            TEXT NOT NULL DEFAULT '',
			validation_status TEXT NOT NULL,
			state             TEXT NOT NULL,
			decision_at       TIMESTAMP,
			duplicate_of      TEXT NOT NULL DEFAULT '',
			file_path         TEXT NOT NULL DEFAULT '',
			line              INTEGER NOT NULL DEFAULT 0,
			commit_sha        TEXT NOT NULL DEFAULT '',
			root_cause        TEXT NOT NULL DEFAULT '',
			reproduction      TEXT NOT NULL DEFAULT '',
			remediation       TEXT NOT NULL DEFAULT '',
			suggested_action  TEXT NOT NULL DEFAULT '',
			why_tests_do_not_cover TEXT NOT NULL DEFAULT '',
			suggested_regression_test TEXT NOT NULL DEFAULT '',
			minimum_fix_scope TEXT NOT NULL DEFAULT '',
			evidence_json     TEXT NOT NULL DEFAULT '',
			validation_json   TEXT NOT NULL DEFAULT '',
			patch_proposal_id TEXT NOT NULL DEFAULT '',
			pr_number         INTEGER,
			pr_url            TEXT NOT NULL DEFAULT '',
			created_at        TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at        TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(namespace, repository_scan, fingerprint)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_security_findings_repo
			ON security_findings(namespace, repository_scan, severity, validation_status, state, updated_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_security_findings_slice
			ON security_findings(namespace, repository_scan, slice_id, category, updated_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_security_findings_duplicates
			ON security_findings(namespace, repository_scan, duplicate_of, updated_at DESC)`,
		`CREATE TABLE IF NOT EXISTS security_review_slices (
			id                TEXT NOT NULL,
			namespace         TEXT NOT NULL,
			repository_scan   TEXT NOT NULL,
			source            TEXT NOT NULL,
			title             TEXT NOT NULL,
			summary           TEXT NOT NULL DEFAULT '',
			kind              TEXT NOT NULL DEFAULT 'unknown',
			confidence        TEXT NOT NULL DEFAULT 'medium',
			status            TEXT NOT NULL DEFAULT 'pending',
			entrypoints_json  TEXT NOT NULL DEFAULT '[]',
			owned_files_json  TEXT NOT NULL DEFAULT '[]',
			context_files_json TEXT NOT NULL DEFAULT '[]',
			tests_json        TEXT NOT NULL DEFAULT '[]',
			tags_json         TEXT NOT NULL DEFAULT '[]',
			trust_boundaries_json TEXT NOT NULL DEFAULT '[]',
			changed_files_json TEXT NOT NULL DEFAULT '[]',
			changed_line_ranges_json TEXT NOT NULL DEFAULT '[]',
			review_context_json TEXT NOT NULL DEFAULT '',
			review_context_hash TEXT NOT NULL DEFAULT '',
			last_scan_run_id  TEXT NOT NULL DEFAULT '',
			last_reviewed_at  TIMESTAMP,
			created_at        TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at        TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (namespace, repository_scan, id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_security_review_slices_repo
			ON security_review_slices(namespace, repository_scan, status, updated_at DESC)`,
		`CREATE TABLE IF NOT EXISTS security_dropped_findings (
			id                TEXT PRIMARY KEY,
			namespace         TEXT NOT NULL,
			repository_scan   TEXT NOT NULL,
			scan_run_id       TEXT NOT NULL,
			task_name         TEXT NOT NULL,
			slice_id          TEXT NOT NULL DEFAULT '',
			reason            TEXT NOT NULL,
			sample_json       TEXT NOT NULL DEFAULT '',
			created_at        TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			layer TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_security_dropped_findings_run
			ON security_dropped_findings(namespace, repository_scan, scan_run_id, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_security_dropped_findings_layer
			ON security_dropped_findings(namespace, repository_scan, layer, created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS security_patch_proposals (
			id                TEXT PRIMARY KEY,
			namespace         TEXT NOT NULL,
			repository_scan   TEXT NOT NULL,
			finding_id        TEXT NOT NULL,
			task_name         TEXT NOT NULL,
			branch            TEXT NOT NULL,
			diff_artifact     TEXT NOT NULL DEFAULT '',
			summary_artifact  TEXT NOT NULL DEFAULT '',
			status            TEXT NOT NULL,
			reason            TEXT NOT NULL DEFAULT '',
			pr_number         INTEGER,
			pr_url            TEXT NOT NULL DEFAULT '',
			publication_evidence_json TEXT NOT NULL DEFAULT '',
			created_at        TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at        TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_security_patch_proposals_finding
			ON security_patch_proposals(namespace, finding_id, created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS repository_monitors (
			namespace  TEXT NOT NULL,
			name       TEXT NOT NULL,
			uid        TEXT NOT NULL DEFAULT '',
			repo_url   TEXT NOT NULL,
			owner      TEXT NOT NULL DEFAULT '',
			repository TEXT NOT NULL DEFAULT '',
			branch     TEXT NOT NULL DEFAULT '',
			generation INTEGER NOT NULL DEFAULT 0,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (namespace, name)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_repository_monitors_repo
			ON repository_monitors(namespace, owner, repository)`,
		`CREATE TABLE IF NOT EXISTS monitor_runs (
			id                 TEXT PRIMARY KEY,
			monitor_namespace  TEXT NOT NULL,
			monitor_name       TEXT NOT NULL,
			trigger            TEXT NOT NULL,
			target_kind        TEXT NOT NULL DEFAULT '',
			target_number      INTEGER NOT NULL DEFAULT 0,
			target_sha         TEXT NOT NULL DEFAULT '',
			command_event_id   TEXT NOT NULL DEFAULT '',
			phase              TEXT NOT NULL,
			started_at         TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			completed_at       TIMESTAMP,
			selected_count     INTEGER NOT NULL DEFAULT 0,
			created_task_count INTEGER NOT NULL DEFAULT 0,
			skipped_count      INTEGER NOT NULL DEFAULT 0,
			error              TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_monitor_runs_monitor
			ON monitor_runs(monitor_namespace, monitor_name, started_at DESC, id DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_monitor_runs_target
			ON monitor_runs(monitor_namespace, monitor_name, phase, trigger, target_kind, target_number, target_sha)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_monitor_runs_running
			ON monitor_runs(monitor_namespace, monitor_name)
			WHERE phase = 'running'`,
		`CREATE TABLE IF NOT EXISTS monitor_items (
			monitor_namespace     TEXT NOT NULL,
			monitor_name          TEXT NOT NULL,
			kind                  TEXT NOT NULL,
			item_key              TEXT NOT NULL,
			number                INTEGER NOT NULL DEFAULT 0,
			sha                   TEXT NOT NULL DEFAULT '',
			title                 TEXT NOT NULL DEFAULT '',
			body                  TEXT NOT NULL DEFAULT '',
			html_url              TEXT NOT NULL DEFAULT '',
			author                TEXT NOT NULL DEFAULT '',
			state                 TEXT NOT NULL DEFAULT '',
			labels_json           TEXT NOT NULL DEFAULT '[]',
			snapshot_digest       TEXT NOT NULL DEFAULT '',
			github_updated_at     TIMESTAMP NOT NULL DEFAULT '0001-01-01T00:00:00Z',
			workflow_phase        TEXT NOT NULL DEFAULT '',
			linked_pr_number      INTEGER NOT NULL DEFAULT 0,
			last_command_id       TEXT NOT NULL DEFAULT '',
			last_command_intent   TEXT NOT NULL DEFAULT '',
			last_action_id        TEXT NOT NULL DEFAULT '',
			last_action_kind      TEXT NOT NULL DEFAULT '',
			last_action_task_name TEXT NOT NULL DEFAULT '',
			base_branch           TEXT NOT NULL DEFAULT '',
			head_branch           TEXT NOT NULL DEFAULT '',
			head_sha              TEXT NOT NULL DEFAULT '',
			base_sha              TEXT NOT NULL DEFAULT '',
			draft                 BOOLEAN NOT NULL DEFAULT FALSE,
			mergeable_state       TEXT NOT NULL DEFAULT '',
			ci_state              TEXT NOT NULL DEFAULT '',
			skip_reason           TEXT NOT NULL DEFAULT '',
			last_review_id        TEXT NOT NULL DEFAULT '',
			last_reviewed_head_sha TEXT NOT NULL DEFAULT '',
			last_verdict          TEXT NOT NULL DEFAULT '',
			repair_state          TEXT NOT NULL DEFAULT '',
			automerge_state       TEXT NOT NULL DEFAULT '',
			status_comment_id     TEXT NOT NULL DEFAULT '',
			status_comment_url    TEXT NOT NULL DEFAULT '',
			last_publish_id       TEXT NOT NULL DEFAULT '',
			last_publish_phase    TEXT NOT NULL DEFAULT '',
			last_publish_reason   TEXT NOT NULL DEFAULT '',
			last_publish_url      TEXT NOT NULL DEFAULT '',
			updated_at            TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			last_seen_at          TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (monitor_namespace, monitor_name, kind, item_key)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_monitor_items_queue
			ON monitor_items(monitor_namespace, monitor_name, kind, state, last_verdict, repair_state, automerge_state, updated_at DESC)`,

		`CREATE TABLE IF NOT EXISTS work_actions (
			id                     TEXT PRIMARY KEY,
			monitor_namespace      TEXT NOT NULL,
			monitor_name           TEXT NOT NULL,
			run_id                 TEXT NOT NULL DEFAULT '',
			command_event_id       TEXT NOT NULL DEFAULT '',
			monitor_generation     INTEGER NOT NULL DEFAULT 0,
			target_kind            TEXT NOT NULL DEFAULT '',
			target_number          INTEGER NOT NULL DEFAULT 0,
			target_sha             TEXT NOT NULL DEFAULT '',
			target_snapshot_digest TEXT NOT NULL DEFAULT '',
			intent                 TEXT NOT NULL DEFAULT '',
			desired_action         TEXT NOT NULL DEFAULT '',
			depends_on_action_id   TEXT NOT NULL DEFAULT '',
			dedupe_key             TEXT NOT NULL DEFAULT '',
			idempotency_key        TEXT NOT NULL DEFAULT '',
			status                 TEXT NOT NULL DEFAULT '',
			phase                  TEXT NOT NULL DEFAULT '',
			attempt                INTEGER NOT NULL DEFAULT 0,
			lease_owner            TEXT NOT NULL DEFAULT '',
			lease_expires_at       TIMESTAMP,
			task_name              TEXT NOT NULL DEFAULT '',
			blocked_reason         TEXT NOT NULL DEFAULT '',
			error                  TEXT NOT NULL DEFAULT '',
			artifact_ids           TEXT NOT NULL DEFAULT '',
			payload_digest         TEXT NOT NULL DEFAULT '',
			metadata_json          TEXT NOT NULL DEFAULT '{}',
			created_at             TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at             TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			completed_at           TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_work_actions_monitor
			ON work_actions(monitor_namespace, monitor_name, target_kind, target_number, desired_action, status, updated_at DESC)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_work_actions_dedupe
			ON work_actions(monitor_namespace, monitor_name, dedupe_key)
			WHERE dedupe_key <> '' AND status IN ('queued', 'leased', 'running')`,
		`CREATE TABLE IF NOT EXISTS action_records (
			id                 TEXT PRIMARY KEY,
			monitor_namespace  TEXT NOT NULL,
			monitor_name       TEXT NOT NULL,
			kind               TEXT NOT NULL,
			number             INTEGER NOT NULL DEFAULT 0,
			action_kind        TEXT NOT NULL,
			snapshot_digest    TEXT NOT NULL DEFAULT '',
			head_sha           TEXT NOT NULL DEFAULT '',
			task_name          TEXT NOT NULL DEFAULT '',
			command_event_id   TEXT NOT NULL DEFAULT '',
			work_action_id     TEXT NOT NULL DEFAULT '',
			monitor_generation INTEGER NOT NULL DEFAULT 0,
			verdict            TEXT NOT NULL DEFAULT '',
			confidence         TEXT NOT NULL DEFAULT '',
			summary            TEXT NOT NULL DEFAULT '',
			payload_json       TEXT NOT NULL DEFAULT '{}',
			payload_digest     TEXT NOT NULL DEFAULT '',
			created_at         TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_action_records_monitor
			ON action_records(monitor_namespace, monitor_name, kind, number, action_kind, created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS review_records (
			id                 TEXT PRIMARY KEY,
			monitor_namespace  TEXT NOT NULL,
			monitor_name       TEXT NOT NULL,
			kind               TEXT NOT NULL,
			number             INTEGER NOT NULL DEFAULT 0,
			head_sha           TEXT NOT NULL DEFAULT '',
			task_name          TEXT NOT NULL DEFAULT '',
			task_namespace     TEXT NOT NULL DEFAULT '',
			verdict            TEXT NOT NULL DEFAULT '',
			confidence         TEXT NOT NULL DEFAULT '',
			repairable         BOOLEAN NOT NULL DEFAULT FALSE,
			security_status    TEXT NOT NULL DEFAULT '',
			findings_json      TEXT NOT NULL DEFAULT '[]',
			summary            TEXT NOT NULL DEFAULT '',
			suggested_comment  TEXT NOT NULL DEFAULT '',
			validation_task          TEXT NOT NULL DEFAULT '',
			validation_image         TEXT NOT NULL DEFAULT '',
			validation_command_digest TEXT NOT NULL DEFAULT '',
			validation_status        TEXT NOT NULL DEFAULT '',
			validation_evidence      TEXT NOT NULL DEFAULT '',
			rendered_comment   TEXT NOT NULL DEFAULT '',
			marker             TEXT NOT NULL DEFAULT '',
			github_review_id   TEXT NOT NULL DEFAULT '',
			github_comment_id  TEXT NOT NULL DEFAULT '',
			github_comment_url TEXT NOT NULL DEFAULT '',
			created_at         TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_review_records_item
			ON review_records(monitor_namespace, monitor_name, kind, number, head_sha, created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS review_publish_records (
			id                   TEXT PRIMARY KEY,
			monitor_namespace    TEXT NOT NULL,
			monitor_name         TEXT NOT NULL,
			item_kind            TEXT NOT NULL DEFAULT '',
			item_number          INTEGER NOT NULL DEFAULT 0,
			head_sha             TEXT NOT NULL DEFAULT '',
			run_id               TEXT NOT NULL DEFAULT '',
			review_task_name     TEXT NOT NULL DEFAULT '',
			review_record_id     TEXT NOT NULL DEFAULT '',
			phase                TEXT NOT NULL DEFAULT '',
			event                TEXT NOT NULL DEFAULT '',
			github_review_id     TEXT NOT NULL DEFAULT '',
			github_review_url    TEXT NOT NULL DEFAULT '',
			body_digest          TEXT NOT NULL DEFAULT '',
			inline_comment_count INTEGER NOT NULL DEFAULT 0,
			skip_reason          TEXT NOT NULL DEFAULT '',
			error                TEXT NOT NULL DEFAULT '',
			created_at           TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at           TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_review_publish_records_item
			ON review_publish_records(monitor_namespace, monitor_name, item_kind, item_number, head_sha, phase, updated_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_review_publish_records_review
			ON review_publish_records(monitor_namespace, review_record_id, updated_at DESC)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_review_publish_records_succeeded_head
			ON review_publish_records(monitor_namespace, monitor_name, item_kind, item_number, head_sha)
			WHERE phase = 'succeeded'`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_review_publish_records_active_head
			ON review_publish_records(monitor_namespace, monitor_name, item_kind, item_number, head_sha)
			WHERE phase IN ('started', 'succeeded')`,
		`CREATE TABLE IF NOT EXISTS command_events (
			id                    TEXT PRIMARY KEY,
			monitor_namespace     TEXT NOT NULL,
			monitor_name          TEXT NOT NULL,
			repo                  TEXT NOT NULL DEFAULT '',
			kind                  TEXT NOT NULL DEFAULT '',
			number                INTEGER NOT NULL DEFAULT 0,
			source                TEXT NOT NULL DEFAULT '',
			delivery_id           TEXT NOT NULL DEFAULT '',
			label                 TEXT NOT NULL DEFAULT '',
			monitor_generation    INTEGER NOT NULL DEFAULT 0,
			dedupe_key            TEXT NOT NULL DEFAULT '',
			idempotency_key       TEXT NOT NULL DEFAULT '',
			comment_id            TEXT NOT NULL DEFAULT '',
			comment_url           TEXT NOT NULL DEFAULT '',
			author                TEXT NOT NULL DEFAULT '',
			author_association    TEXT NOT NULL DEFAULT '',
			permission            TEXT NOT NULL DEFAULT '',
			command               TEXT NOT NULL DEFAULT '',
			intent                TEXT NOT NULL DEFAULT '',
			head_sha              TEXT NOT NULL DEFAULT '',
			issue_snapshot_digest TEXT NOT NULL DEFAULT '',
			status                TEXT NOT NULL DEFAULT '',
			status_comment_id     TEXT NOT NULL DEFAULT '',
			created_repair_job_id TEXT NOT NULL DEFAULT '',
			created_at            TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			processed_at          TIMESTAMP,
			error                 TEXT NOT NULL DEFAULT '',
			UNIQUE(monitor_namespace, monitor_name, comment_id, command, head_sha)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_command_events_monitor
			ON command_events(monitor_namespace, monitor_name, created_at DESC)`,

		`CREATE UNIQUE INDEX IF NOT EXISTS idx_command_events_dedupe
			ON command_events(monitor_namespace, monitor_name, dedupe_key)
		WHERE dedupe_key <> ''`,
		`CREATE TABLE IF NOT EXISTS implementation_jobs (
			id                 TEXT PRIMARY KEY,
			monitor_namespace  TEXT NOT NULL,
			monitor_name       TEXT NOT NULL,
			repo               TEXT NOT NULL DEFAULT '',
			issue_number       INTEGER NOT NULL DEFAULT 0,
			plan_id            TEXT NOT NULL DEFAULT '',
			snapshot_digest    TEXT NOT NULL DEFAULT '',
			phase              TEXT NOT NULL DEFAULT '',
			attempt            INTEGER NOT NULL DEFAULT 0,
			branch             TEXT NOT NULL DEFAULT '',
			patch_artifact_id  TEXT NOT NULL DEFAULT '',
			pr_number          INTEGER NOT NULL DEFAULT 0,
			validation_state   TEXT NOT NULL DEFAULT '',
			task_name          TEXT NOT NULL DEFAULT '',
			mutation_task_name TEXT NOT NULL DEFAULT '',
			command_event_id   TEXT NOT NULL DEFAULT '',
			work_action_id     TEXT NOT NULL DEFAULT '',
			monitor_generation INTEGER NOT NULL DEFAULT 0,
			error              TEXT NOT NULL DEFAULT '',
			created_at         TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at         TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			completed_at       TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_implementation_jobs_monitor
			ON implementation_jobs(monitor_namespace, monitor_name, issue_number, phase, updated_at DESC)`,
		`CREATE TABLE IF NOT EXISTS github_mutation_records (
			id                 TEXT PRIMARY KEY,
			monitor_namespace  TEXT NOT NULL,
			monitor_name       TEXT NOT NULL,
			run_id             TEXT NOT NULL DEFAULT '',
			command_event_id   TEXT NOT NULL DEFAULT '',
			work_action_id     TEXT NOT NULL DEFAULT '',
			monitor_generation INTEGER NOT NULL DEFAULT 0,
			operation          TEXT NOT NULL,
			target_kind        TEXT NOT NULL DEFAULT '',
			target_number      INTEGER NOT NULL DEFAULT 0,
			target_sha         TEXT NOT NULL DEFAULT '',
			actor              TEXT NOT NULL DEFAULT '',
			reason             TEXT NOT NULL DEFAULT '',
			request_digest     TEXT NOT NULL DEFAULT '',
			github_url         TEXT NOT NULL DEFAULT '',
			github_request_id  TEXT NOT NULL DEFAULT '',
			external_id        TEXT NOT NULL DEFAULT '',
			status             TEXT NOT NULL DEFAULT '',
			error              TEXT NOT NULL DEFAULT '',
			pending_at         TIMESTAMP,
			created_at         TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_github_mutation_records_monitor
			ON github_mutation_records(monitor_namespace, monitor_name, target_kind, target_number, operation, created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS repair_jobs (
			id                  TEXT PRIMARY KEY,
			monitor_namespace   TEXT NOT NULL,
			monitor_name        TEXT NOT NULL,
			repo                TEXT NOT NULL DEFAULT '',
			pr_number           INTEGER NOT NULL DEFAULT 0,
			intent              TEXT NOT NULL DEFAULT '',
			source              TEXT NOT NULL DEFAULT '',
			head_sha            TEXT NOT NULL DEFAULT '',
			base_sha            TEXT NOT NULL DEFAULT '',
			base_branch         TEXT NOT NULL DEFAULT '',
			phase               TEXT NOT NULL DEFAULT '',
			repair_count_pr     INTEGER NOT NULL DEFAULT 0,
			repair_count_head   INTEGER NOT NULL DEFAULT 0,
			validation_attempts INTEGER NOT NULL DEFAULT 0,
			review_fix_attempts INTEGER NOT NULL DEFAULT 0,
			task_name           TEXT NOT NULL DEFAULT '',
			branch              TEXT NOT NULL DEFAULT '',
			pushed_sha          TEXT NOT NULL DEFAULT '',
			last_error          TEXT NOT NULL DEFAULT '',
			created_at          TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at          TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			completed_at        TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_repair_jobs_monitor
			ON repair_jobs(monitor_namespace, monitor_name, pr_number, phase, updated_at DESC)`,
		`CREATE TABLE IF NOT EXISTS monitor_events (
			id                 TEXT PRIMARY KEY,
			monitor_namespace  TEXT NOT NULL,
			monitor_name       TEXT NOT NULL,
			run_id             TEXT NOT NULL DEFAULT '',
			item_kind          TEXT NOT NULL DEFAULT '',
			item_number        INTEGER NOT NULL DEFAULT 0,
			item_sha           TEXT NOT NULL DEFAULT '',
			event_type         TEXT NOT NULL,
			actor              TEXT NOT NULL DEFAULT '',
			summary            TEXT NOT NULL DEFAULT '',
			metadata_json      TEXT NOT NULL DEFAULT '{}',
			created_at         TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_monitor_events_monitor
			ON monitor_events(monitor_namespace, monitor_name, created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS gateway_events (
			id                    TEXT NOT NULL,
			namespace             TEXT NOT NULL,
			namespace_uid         TEXT NOT NULL DEFAULT '',
			gateway_uid           TEXT NOT NULL,
			gateway_generation    INTEGER NOT NULL DEFAULT 0,
			gateway_name          TEXT NOT NULL,
			binding_name          TEXT NOT NULL DEFAULT '',
			binding_uid           TEXT NOT NULL DEFAULT '',
			binding_generation    INTEGER NOT NULL DEFAULT 0,
			agent_name            TEXT NOT NULL DEFAULT '',
			agent_uid             TEXT NOT NULL DEFAULT '',
			external_event_id     TEXT NOT NULL,
			protocol_version      TEXT NOT NULL,
			event_type            TEXT NOT NULL,
			state                 TEXT NOT NULL,
			state_message         TEXT NOT NULL DEFAULT '',
			account_id            TEXT NOT NULL,
			context_id            TEXT NOT NULL,
			thread_id             TEXT NOT NULL DEFAULT '',
			sender_id             TEXT NOT NULL,
			sender_display_name   TEXT NOT NULL DEFAULT '',
			text                  TEXT NOT NULL DEFAULT '',
			reply_target          TEXT NOT NULL DEFAULT '',
			metadata_json         TEXT NOT NULL DEFAULT '{}',
			session_name          TEXT NOT NULL DEFAULT '',
			 task_name             TEXT NOT NULL DEFAULT '',
			 task_uid              TEXT NOT NULL DEFAULT '',
			 task_runtime_allowed_tools_json TEXT,
			 delivery_id           TEXT NOT NULL DEFAULT '',
			 provider_message_id    TEXT NOT NULL DEFAULT '',
			 trace_parent          TEXT NOT NULL DEFAULT '',
			 trace_state           TEXT NOT NULL DEFAULT '',
			 transcript_order      INTEGER NOT NULL DEFAULT 0,
			attempt_count         INTEGER NOT NULL DEFAULT 0,
			claim_owner           TEXT NOT NULL DEFAULT '',
			claim_until           TIMESTAMP,
			next_attempt_at       TIMESTAMP NOT NULL,
			occurred_at           TIMESTAMP,
			received_at           TIMESTAMP NOT NULL,
			expires_at            TIMESTAMP NOT NULL,
			created_at            TIMESTAMP NOT NULL,
			updated_at            TIMESTAMP NOT NULL,
			completed_at          TIMESTAMP,
			PRIMARY KEY (namespace, id),
			UNIQUE(namespace, gateway_uid, external_event_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_gateway_events_dispatch
			ON gateway_events(state, next_attempt_at, received_at, id)`,
		`CREATE INDEX IF NOT EXISTS idx_gateway_events_session
			ON gateway_events(namespace, session_name, state, received_at, id)`,
		`CREATE INDEX IF NOT EXISTS idx_gateway_events_task_owner
			ON gateway_events(namespace, session_name, task_name, created_at DESC, id DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_gateway_events_task_identity
			ON gateway_events(namespace, task_name, task_uid, state, created_at DESC, id DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_gateway_events_gateway
			ON gateway_events(namespace, gateway_name, created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS gateway_event_tombstones (
			namespace          TEXT NOT NULL,
			gateway_uid        TEXT NOT NULL,
			external_event_id  TEXT NOT NULL,
			event_id           TEXT NOT NULL,
			task_name          TEXT NOT NULL DEFAULT '',
			task_uid           TEXT NOT NULL DEFAULT '',
			envelope_digest    TEXT NOT NULL,
			session_name       TEXT NOT NULL DEFAULT '',
			transcript_order   INTEGER NOT NULL DEFAULT 0,
			expires_at         TIMESTAMP NOT NULL,
			created_at         TIMESTAMP NOT NULL,
			PRIMARY KEY (namespace, gateway_uid, external_event_id),
			UNIQUE(namespace, event_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_gateway_event_tombstones_expiry
			ON gateway_event_tombstones(namespace, expires_at)`,
		`CREATE INDEX IF NOT EXISTS idx_gateway_event_tombstones_task
			ON gateway_event_tombstones(namespace, task_name, task_uid)`,
		`CREATE TABLE IF NOT EXISTS gateway_deliveries (
			id                    TEXT NOT NULL,
			idempotency_id        TEXT NOT NULL,
			namespace             TEXT NOT NULL,
			namespace_uid         TEXT NOT NULL DEFAULT '',
			gateway_uid           TEXT NOT NULL,
			gateway_generation    INTEGER NOT NULL DEFAULT 0,
			gateway_name          TEXT NOT NULL,
			binding_name          TEXT NOT NULL DEFAULT '',
			event_id              TEXT NOT NULL,
			task_name             TEXT NOT NULL DEFAULT '',
			session_name          TEXT NOT NULL DEFAULT '',
			kind                  TEXT NOT NULL,
			state                 TEXT NOT NULL,
			account_id            TEXT NOT NULL,
			context_id            TEXT NOT NULL,
			thread_id             TEXT NOT NULL DEFAULT '',
			reply_target          TEXT NOT NULL,
			text                  TEXT NOT NULL,
			metadata_json         TEXT NOT NULL DEFAULT '{}',
			attempt_count         INTEGER NOT NULL DEFAULT 0,
			max_attempts          INTEGER NOT NULL DEFAULT 10,
			manual_retry_count    INTEGER NOT NULL DEFAULT 0,
			next_attempt_at       TIMESTAMP NOT NULL,
			expires_at            TIMESTAMP NOT NULL,
			 provider_message_id   TEXT NOT NULL DEFAULT '',
			 trace_parent          TEXT NOT NULL DEFAULT '',
			 trace_state           TEXT NOT NULL DEFAULT '',
			 last_error            TEXT NOT NULL DEFAULT '',
			claim_owner           TEXT NOT NULL DEFAULT '',
			claim_until           TIMESTAMP,
			created_at            TIMESTAMP NOT NULL,
			updated_at            TIMESTAMP NOT NULL,
			delivered_at          TIMESTAMP,
			PRIMARY KEY (namespace, id),
			UNIQUE(namespace, idempotency_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_gateway_deliveries_send
			ON gateway_deliveries(state, next_attempt_at, created_at, id)`,
		`CREATE INDEX IF NOT EXISTS idx_gateway_deliveries_event
			ON gateway_deliveries(namespace, event_id, created_at, id)`,
		`CREATE INDEX IF NOT EXISTS idx_gateway_deliveries_gateway
			ON gateway_deliveries(namespace, gateway_name, created_at DESC)`,
	}

	statements = append(statements, controlSchemaStatements()...)
	return append(statements, agentExecutionSchemaStatements()...)
}

// Store implements both store.ResultStore and store.SessionStore.
type Store struct {
	db               *sql.DB
	securityTx       *sql.Tx
	dbPath           string
	processLock      io.Closer
	executionEventMu sync.Mutex

	// snapshotCipher encrypts immutable agent execution snapshot bodies at
	// rest. Snapshot persistence fails closed while it is nil.
	snapshotCipher *AgentExecutionSnapshotCipher

	// applyMemoryProposalAfterAcceptedRead is a test hook used to coordinate
	// multi-connection proposal-apply races after an accepted proposal is read.
	applyMemoryProposalAfterAcceptedRead func()

	// archiveMemoryProposalAfterActiveRead is a test hook used to coordinate
	// multi-connection proposal-archive races after an active proposal is read.
	archiveMemoryProposalAfterActiveRead func()
}

// NewStore creates a new Store backed by the given SQLite database.
// The dbPath is the filesystem path to the database file (used for metrics and logging).
func NewStore(db *sql.DB, dbPath string) *Store {
	return &Store{db: db, dbPath: dbPath}
}

// OpenLockedStore acquires the process-lifetime filesystem lock adjacent to
// path before SQLite is opened. Production controller wiring must use this
// constructor so overlapping Pods or releases cannot initialize or write
// the same database even if Kubernetes ownership fencing is misconfigured.
func OpenLockedStore(path string) (*Store, error) {
	lock, err := lockDatabaseFile(path)
	if err != nil {
		return nil, err
	}
	db, err := NewDB(path)
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	return &Store{db: db, dbPath: path, processLock: lock}, nil
}

// Start runs background maintenance and blocks until ctx is cancelled,
// then closes the database without issuing any final writes.
// It satisfies the controller-runtime manager.Runnable interface.
func (s *Store) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("sqlite-store")

	logger.Info("SQLite store is configured — ensure a PersistentVolume is mounted at the store path for data durability", "path", s.dbPath)

	// Update DB size metric periodically
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	// Record initial size
	s.updateDBSizeMetric()

	for {
		select {
		case <-ctx.Done():
			return s.close()
		case <-ticker.C:
			s.updateDBSizeMetric()
		}
	}
}

// NeedLeaderElection keeps SQLite-backed background mutation behind the
// controller-runtime leader gate. The filesystem lock remains held for the
// whole process lifetime, including standby/startup, and is the final defense
// against overlapping writers.
func (s *Store) NeedLeaderElection() bool { return true }

func (s *Store) close() error {
	dbErr := s.db.Close()
	var lockErr error
	if s.processLock != nil {
		lockErr = s.processLock.Close()
		s.processLock = nil
	}
	if dbErr != nil {
		return dbErr
	}
	return lockErr
}

// updateDBSizeMetric reads the database file size and updates the gauge.
func (s *Store) updateDBSizeMetric() {
	if s.dbPath == "" || s.dbPath == ":memory:" {
		return
	}
	info, err := os.Stat(s.dbPath)
	if err != nil {
		return
	}
	dbSizeBytes.Set(float64(info.Size()))
}

// HealthCheck verifies the database is reachable by executing a simple query.
func (s *Store) HealthCheck(ctx context.Context) error {
	var n int
	return s.db.QueryRowContext(ctx, "SELECT 1").Scan(&n)
}
