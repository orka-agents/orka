-- Historical SQLite transition: 166-to-167.
-- Source layout: abd2c0406ab660abd37af7c176b4eef610a19ce1 (166 objects).
-- Removes the retired harness v1 tables and their indexes. Nothing in a
-- harness v2 store writes them; the caller refuses the step while either
-- table holds rows, so no record is discarded.
-- Transaction and complete-layout validation are owned by the caller.

DROP TABLE harness_v1_attempts;

DROP TABLE runtime_sessions;
