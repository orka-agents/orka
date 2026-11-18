-- SQLite transition: 172-to-173 (schema version 5 to 6).
-- Adds memory generations and consolidation proposal targets. Each memory
-- gains a generation, starting existing memories at 1, and the memory that
-- superseded it, empty for existing memories. Each proposal gains the target
-- memories and source Sessions it names, empty for existing proposals. It
-- adds no rows, so a store whose key bootstrap is unfinished can still
-- finish it.
-- Transaction and complete-layout validation are owned by the caller.

ALTER TABLE memories ADD COLUMN generation INTEGER NOT NULL DEFAULT 1;

ALTER TABLE memories ADD COLUMN superseded_by TEXT NOT NULL DEFAULT '';

ALTER TABLE memory_proposals ADD COLUMN targets_json TEXT NOT NULL DEFAULT '[]';

ALTER TABLE memory_proposals ADD COLUMN sources_json TEXT NOT NULL DEFAULT '[]';
