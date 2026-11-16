-- SQLite transition: 173-to-174 (schema version 6 to 7).
-- Adds memory consolidation runs: one row per run, keyed by its Task UID,
-- with the window of Sessions it reads, the input it read and its report,
-- and an index that lists a namespace's Sessions by when they changed. It
-- adds no rows, so a store whose key bootstrap is unfinished can still
-- finish it.
-- Transaction and complete-layout validation are owned by the caller.

CREATE TABLE IF NOT EXISTS memory_consolidation_runs (
 namespace TEXT NOT NULL,
 task_uid TEXT NOT NULL,
 task_name TEXT NOT NULL,
 mode TEXT NOT NULL DEFAULT '',
 status TEXT NOT NULL,
 window_start TIMESTAMP NOT NULL,
 window_end TIMESTAMP NOT NULL,
 input BLOB,
 report BLOB,
 created_at TIMESTAMP NOT NULL,
 completed_at TIMESTAMP,
 PRIMARY KEY (namespace, task_uid)
);

CREATE INDEX IF NOT EXISTS idx_memory_consolidation_runs_cursor
 ON memory_consolidation_runs(namespace, status, window_end);

CREATE INDEX IF NOT EXISTS idx_sessions_updated
 ON sessions(namespace, updated_at);
