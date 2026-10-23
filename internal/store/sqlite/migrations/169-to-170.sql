-- SQLite transition: 169-to-170 (schema version 2 to 3).
-- Binds results and artifacts to the Task UID that wrote them. It rebuilds
-- both tables with the UID in their keys and the order stamp (epoch and
-- sequence) of the write that stored each row, records the Task UID on prompt
-- result receipts and implementation jobs, and adds the deletion tombstones of
-- result and artifact owners. Existing rows keep an empty UID and the stamp
-- (0, 0): they belong to whichever Task holds their name until its first
-- delete. Every row is copied whole, so the step takes time in proportion to
-- the stored result and artifact bytes. It adds no rows, so a store whose key
-- bootstrap is unfinished can still finish it.
-- Transaction and complete-layout validation are owned by the caller.

ALTER TABLE results RENAME TO results_v2;

CREATE TABLE results (
			namespace   TEXT NOT NULL,
			task_name   TEXT NOT NULL,
			data        BLOB NOT NULL,
			created_at  TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at  TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			task_uid    TEXT NOT NULL DEFAULT '',
			write_epoch INTEGER NOT NULL DEFAULT 0,
			write_seq   INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (namespace, task_name, task_uid)
		);

INSERT INTO results (namespace, task_name, data, created_at, updated_at)
 SELECT namespace, task_name, data, created_at, updated_at FROM results_v2;

DROP TABLE results_v2;

CREATE INDEX idx_results_namespace ON results(namespace);

CREATE INDEX idx_results_task_uid ON results(namespace, task_uid);

ALTER TABLE artifacts RENAME TO artifacts_v2;

CREATE TABLE artifacts (
			namespace    TEXT NOT NULL,
			task_name    TEXT NOT NULL,
			filename     TEXT NOT NULL,
			content_type TEXT NOT NULL DEFAULT 'application/octet-stream',
			size         INTEGER NOT NULL,
			data         BLOB NOT NULL,
			created_at   TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			task_uid     TEXT NOT NULL DEFAULT '',
			write_epoch  INTEGER NOT NULL DEFAULT 0,
			write_seq    INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (namespace, task_name, task_uid, filename)
		);

INSERT INTO artifacts (namespace, task_name, filename, content_type, size, data, created_at)
 SELECT namespace, task_name, filename, content_type, size, data, created_at FROM artifacts_v2;

DROP TABLE artifacts_v2;

CREATE INDEX idx_artifacts_task ON artifacts(namespace, task_name);

CREATE INDEX idx_artifacts_task_uid ON artifacts(namespace, task_uid);

ALTER TABLE prompt_result_receipts ADD COLUMN task_uid TEXT NOT NULL DEFAULT '';

ALTER TABLE implementation_jobs ADD COLUMN task_uid TEXT NOT NULL DEFAULT '';

CREATE TABLE result_owner_tombstones (
 namespace TEXT NOT NULL,
 task_uid TEXT NOT NULL,
 task_name TEXT NOT NULL,
 deleted_at_ns INTEGER NOT NULL,
 PRIMARY KEY (namespace, task_uid)
);

CREATE INDEX idx_result_owner_tombstones_deleted_at
 ON result_owner_tombstones(deleted_at_ns);

CREATE TABLE artifact_owner_tombstones (
 namespace TEXT NOT NULL,
 task_uid TEXT NOT NULL,
 task_name TEXT NOT NULL,
 deleted_at_ns INTEGER NOT NULL,
 PRIMARY KEY (namespace, task_uid)
);

CREATE INDEX idx_artifact_owner_tombstones_deleted_at
 ON artifact_owner_tombstones(deleted_at_ns);
