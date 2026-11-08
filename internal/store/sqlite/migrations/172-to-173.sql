-- SQLite transition: 172-to-173 (schema version 5 to 6).
-- Adds eval experiment outcomes (orka.eval.experimentOutcome.v1), written
-- once when an experiment is decided, and eval decisions
-- (orka.eval.decision.v1), which are only appended. Both are kept after the
-- EvalRuns are deleted so rejected experiments stay queryable. It adds no
-- rows. Transaction and complete-layout validation are owned by the caller.

CREATE TABLE IF NOT EXISTS eval_experiment_outcomes (
  namespace         TEXT NOT NULL,
  candidate_run_uid TEXT NOT NULL,
  record_json       TEXT NOT NULL,
  decided_at        TIMESTAMP NOT NULL,
  PRIMARY KEY (namespace, candidate_run_uid)
);

CREATE TABLE IF NOT EXISTS eval_decisions (
  seq               INTEGER PRIMARY KEY,
  namespace         TEXT NOT NULL,
  candidate_run_uid TEXT NOT NULL,
  id                TEXT NOT NULL,
  record_json       TEXT NOT NULL,
  decided_at        TIMESTAMP NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_eval_decisions_id ON eval_decisions(namespace, candidate_run_uid, id);
