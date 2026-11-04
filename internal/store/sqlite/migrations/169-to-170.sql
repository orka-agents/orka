-- SQLite transition: 169-to-170 (schema version 2 to 3).
-- Adds the dataset's write watermark and the store time it last advanced. It
-- adds no rows, so a store whose key bootstrap is unfinished can still finish
-- it. Transaction and complete-layout validation are owned by the caller.

ALTER TABLE store_dataset ADD COLUMN write_watermark INTEGER NOT NULL DEFAULT 0 CHECK(write_watermark >= 0);

ALTER TABLE store_dataset ADD COLUMN write_watermark_at_ns INTEGER NOT NULL DEFAULT 0;
