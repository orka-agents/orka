-- SQLite transition: 172-to-175 (schema version 5 to 8).
-- Adds the RepositoryMonitor feedback attempt ledger: one immutable row per
-- attempt to address one piece of reviewer feedback, numbered without gaps
-- per feedback ID, and one mutable outcome per feedback repair command. It
-- adds no rows, so a store whose key bootstrap is unfinished can still
-- finish it.
-- Transaction and complete-layout validation are owned by the caller.

CREATE TABLE IF NOT EXISTS monitor_feedback_attempts (
			id                TEXT PRIMARY KEY,
			monitor_namespace TEXT NOT NULL,
			monitor_name      TEXT NOT NULL,
			repo              TEXT NOT NULL,
			pr_number         INTEGER NOT NULL,
			feedback_id       TEXT NOT NULL,
			attempt           INTEGER NOT NULL CHECK(attempt >= 1),
			head_sha          TEXT NOT NULL,
			snapshot_digest   TEXT NOT NULL,
			command_event_id  TEXT NOT NULL,
			repair_job_id     TEXT NOT NULL,
			created_at        TIMESTAMP NOT NULL,
			UNIQUE(monitor_namespace, monitor_name, repo, pr_number, feedback_id, attempt)
		);

CREATE INDEX IF NOT EXISTS idx_monitor_feedback_attempts_command
			ON monitor_feedback_attempts(monitor_namespace, command_event_id);

CREATE TABLE IF NOT EXISTS monitor_feedback_outcomes (
			monitor_namespace TEXT NOT NULL,
			command_event_id  TEXT NOT NULL,
			monitor_name      TEXT NOT NULL,
			repo              TEXT NOT NULL,
			pr_number         INTEGER NOT NULL,
			head_sha          TEXT NOT NULL,
			state             TEXT NOT NULL CHECK(state IN ('reserved', 'started', 'withdrawn', 'no_change', 'failed', 'succeeded')),
			admitted_ids      TEXT NOT NULL,
			targets           TEXT NOT NULL DEFAULT '[]',
			task_name         TEXT NOT NULL DEFAULT '',
			reason            TEXT NOT NULL DEFAULT '',
			created_at        TIMESTAMP NOT NULL,
			updated_at        TIMESTAMP NOT NULL,
			started_at        TIMESTAMP,
			completed_at      TIMESTAMP,
			PRIMARY KEY(monitor_namespace, command_event_id)
		);

CREATE INDEX IF NOT EXISTS idx_monitor_feedback_outcomes_pull_request
			ON monitor_feedback_outcomes(monitor_namespace, monitor_name, repo, pr_number);
