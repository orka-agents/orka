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
