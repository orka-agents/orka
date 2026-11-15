-- SQLite transition: 169-to-170 (schema version 2 to 3).
-- Adds the dataset's installation binding: the record that bound it, the
-- storage switch generation, the snapshot key fingerprint, whether the
-- installation has detached it, and when the binding last changed. An
-- installation_id the controller epoch advance recorded stays, with no record
-- and generation 0. It adds no rows, so a store whose key bootstrap is
-- unfinished can still finish it.
-- Transaction and complete-layout validation are owned by the caller.

ALTER TABLE store_dataset ADD COLUMN record_uid TEXT NOT NULL DEFAULT '';

ALTER TABLE store_dataset ADD COLUMN switch_generation INTEGER NOT NULL DEFAULT 0 CHECK(switch_generation >= 0);

ALTER TABLE store_dataset ADD COLUMN key_fingerprint TEXT NOT NULL DEFAULT '';

ALTER TABLE store_dataset ADD COLUMN binding_state TEXT NOT NULL DEFAULT '' CHECK(binding_state IN ('', 'bound', 'detached'));

ALTER TABLE store_dataset ADD COLUMN bound_at_ns INTEGER NOT NULL DEFAULT 0 CHECK(bound_at_ns >= 0);
