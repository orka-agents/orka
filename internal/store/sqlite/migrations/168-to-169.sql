-- SQLite transition: 168-to-169 (schema version 1 to 2).
-- Adds operation receipts, the epoch each controller epoch replaced and when
-- it was advanced, and the installation a dataset is bound to. It adds no
-- rows, so a store whose key bootstrap is unfinished can still finish it.
-- Transaction and complete-layout validation are owned by the caller.

CREATE TABLE IF NOT EXISTS operation_receipts (
 operation_id TEXT PRIMARY KEY,
 method TEXT NOT NULL,
 request_digest TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('committed', 'aborted')),
 fence_name TEXT NOT NULL,
 fence_epoch INTEGER NOT NULL,
 fence_holder TEXT NOT NULL,
 issued_at_ns INTEGER NOT NULL,
 recorded_at_ns INTEGER NOT NULL,
 result BLOB,
 custody_scope TEXT NOT NULL DEFAULT '',
 result_cleared INTEGER NOT NULL DEFAULT 0 CHECK(result_cleared IN (0, 1))
);

CREATE INDEX IF NOT EXISTS idx_operation_receipts_recorded_at
 ON operation_receipts(recorded_at_ns);

CREATE INDEX IF NOT EXISTS idx_operation_receipts_custody_scope
 ON operation_receipts(custody_scope) WHERE custody_scope <> '';

ALTER TABLE controller_epochs ADD COLUMN previous_epoch INTEGER;

ALTER TABLE controller_epochs ADD COLUMN previous_holder_id TEXT;

ALTER TABLE controller_epochs ADD COLUMN advanced_at TIMESTAMP;

ALTER TABLE store_dataset ADD COLUMN installation_id TEXT NOT NULL DEFAULT '';
