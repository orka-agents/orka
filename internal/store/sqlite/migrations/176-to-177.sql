-- SQLite transition: 176-to-177 (schema version 9 to 10).
-- Records what a security review corroborated it examined: the coverage
-- disposition and the trusted-context files the review read or cited. Only
-- those files can support resolving a finding the review no longer observed.
-- Existing slices get no coverage, which supports no such resolution until
-- they are reviewed again. It adds no rows, so a store whose key bootstrap
-- is unfinished can still finish it.
-- Transaction and complete-layout validation are owned by the caller.

ALTER TABLE security_review_slices ADD COLUMN coverage TEXT NOT NULL DEFAULT '';

ALTER TABLE security_review_slices ADD COLUMN reviewed_files_json TEXT NOT NULL DEFAULT '[]';
