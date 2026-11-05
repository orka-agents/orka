-- SQLite transition: 170-to-171 (schema version 3 to 4).
-- Adds eval trial records (orka.eval.trial.v1): one immutable row per graded
-- trial of an EvalRun incarnation. It adds no rows.
-- Transaction and complete-layout validation are owned by the caller.

CREATE TABLE IF NOT EXISTS eval_trials (
  namespace    TEXT NOT NULL,
  eval_run     TEXT NOT NULL,
  eval_run_uid TEXT NOT NULL,
  case_id      TEXT NOT NULL,
  trial        INTEGER NOT NULL CHECK(trial >= 1),
  outcome      TEXT NOT NULL CHECK(outcome IN ('pass', 'fail', 'infraError')),
  record_json  TEXT NOT NULL,
  created_at   TIMESTAMP NOT NULL,
  PRIMARY KEY (namespace, eval_run, eval_run_uid, case_id, trial)
);
