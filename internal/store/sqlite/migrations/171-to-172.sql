-- SQLite transition: 171-to-172 (schema version 4 to 5).
-- Adds eval experiment reservations (orka.eval.experiment.v1): one row per
-- candidate EvalRun incarnation that holds an experiment attempt, kept after
-- the EvalRun is deleted so attempt budgets stay spent. It adds no rows.
-- Transaction and complete-layout validation are owned by the caller.

CREATE TABLE IF NOT EXISTS eval_experiments (
  namespace         TEXT NOT NULL,
  candidate_run_uid TEXT NOT NULL,
  baseline_run_uid  TEXT NOT NULL,
  case_set_digest   TEXT NOT NULL,
  record_json       TEXT NOT NULL,
  reserved_at       TIMESTAMP NOT NULL,
  PRIMARY KEY (namespace, candidate_run_uid)
);

CREATE INDEX IF NOT EXISTS idx_eval_experiments_baseline ON eval_experiments(baseline_run_uid);

CREATE INDEX IF NOT EXISTS idx_eval_experiments_case_set ON eval_experiments(case_set_digest);
