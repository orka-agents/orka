-- Additive historical SQLite transition: 149-to-150.
-- Source layout: 0e567418ae20b4a11b5684eb42da47c1cdf1c92e (149 objects).
-- Target definitions: 1f18614ca9dd2339a9c8caac8e3320e7daf61c2e (150 objects).
-- Frozen from the target commit's real schema statements; do not use mutable helpers.
-- Transaction and complete-layout validation are owned by the caller.

CREATE INDEX IF NOT EXISTS idx_github_mutation_records_operation_sha
			ON github_mutation_records(operation, target_sha, created_at DESC, id DESC);
