-- SQLite transition: 171-to-172 (schema version 4 to 5).
-- Adds the order stamp of the write that stored each result and artifact:
-- the epoch and sequence of its write envelope. A replacement applies only
-- when its stamp is greater than the stored one. Existing rows get the stamp
-- (0, 0), which every enveloped write exceeds. It adds no rows, so a store
-- whose key bootstrap is unfinished can still finish it.
-- Transaction and complete-layout validation are owned by the caller.

ALTER TABLE results ADD COLUMN write_epoch INTEGER NOT NULL DEFAULT 0;

ALTER TABLE results ADD COLUMN write_seq INTEGER NOT NULL DEFAULT 0;

ALTER TABLE artifacts ADD COLUMN write_epoch INTEGER NOT NULL DEFAULT 0;

ALTER TABLE artifacts ADD COLUMN write_seq INTEGER NOT NULL DEFAULT 0;
