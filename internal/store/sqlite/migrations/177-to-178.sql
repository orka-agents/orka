-- Additive SQLite transition: 177-to-178 (schema version 10 to 11).
-- Source layout: the dataset write watermark (schema version 10).
-- Target definitions: oversight decisions, schema version 11.
-- It adds only empty tables and their indexes, so a store whose key bootstrap
-- is unfinished can still finish it.
-- Frozen from the target's real schema statements; do not use mutable helpers.
-- Transaction and complete-layout validation are owned by the caller.

CREATE TABLE IF NOT EXISTS oversight_decisions (
			seq               INTEGER PRIMARY KEY AUTOINCREMENT,
			namespace         TEXT NOT NULL,
			id                TEXT NOT NULL,
			task_name         TEXT NOT NULL,
			agent_name        TEXT NOT NULL,
			run_id            TEXT NOT NULL,
			tool_name         TEXT NOT NULL,
			disposition       TEXT NOT NULL,
			would_have_paused INTEGER NOT NULL,
			max_score         REAL NOT NULL,
			signature_hash    TEXT NOT NULL,
			observed_at       TIMESTAMP NOT NULL,
			recorded_at       TIMESTAMP NOT NULL,
			workload_identity TEXT NOT NULL,
			action_narrative  TEXT NOT NULL,
			search_text       TEXT NOT NULL,
			digest            TEXT NOT NULL,
			body              BLOB NOT NULL,
			UNIQUE(namespace, id)
		);

CREATE INDEX IF NOT EXISTS idx_oversight_decisions_task
			ON oversight_decisions(namespace, task_name, seq);

CREATE INDEX IF NOT EXISTS idx_oversight_decisions_agent
			ON oversight_decisions(namespace, agent_name, seq);

CREATE INDEX IF NOT EXISTS idx_oversight_decisions_disposition
			ON oversight_decisions(namespace, disposition, seq);

CREATE INDEX IF NOT EXISTS idx_oversight_decisions_signature
			ON oversight_decisions(namespace, signature_hash, seq);

CREATE INDEX IF NOT EXISTS idx_oversight_decisions_observed
			ON oversight_decisions(namespace, observed_at);

CREATE TABLE IF NOT EXISTS oversight_decision_facets (
			namespace TEXT NOT NULL,
			kind      TEXT NOT NULL,
			value     TEXT NOT NULL,
			seq       INTEGER NOT NULL,
			PRIMARY KEY(namespace, kind, value, seq)
		);

CREATE INDEX IF NOT EXISTS idx_oversight_decision_facets_seq
			ON oversight_decision_facets(seq);

CREATE TABLE IF NOT EXISTS oversight_feedback (
			seq         INTEGER PRIMARY KEY AUTOINCREMENT,
			namespace   TEXT NOT NULL,
			decision_id TEXT NOT NULL,
			label       TEXT NOT NULL,
			note        TEXT NOT NULL,
			reviewer    TEXT NOT NULL,
			recorded_at TIMESTAMP NOT NULL
		);

CREATE INDEX IF NOT EXISTS idx_oversight_feedback_decision
			ON oversight_feedback(namespace, decision_id, seq);
