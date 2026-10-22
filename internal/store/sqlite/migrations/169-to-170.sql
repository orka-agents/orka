-- Additive SQLite transition: 169-to-170.
-- Source layout: ce216cafb30565348e53173dfe36520c7d5e9807 (#901, 168 objects), schema version 0, 1 or 2.
-- Target definitions: GitHub sign-in sessions (170 objects), schema version 3.
-- Frozen from the target's real schema statements; do not use mutable helpers.
-- Transaction and complete-layout validation are owned by the caller.

CREATE TABLE IF NOT EXISTS auth_sessions (
			id         TEXT PRIMARY KEY,
			token_hash TEXT NOT NULL UNIQUE,
			issuer     TEXT NOT NULL,
			subject    TEXT NOT NULL,
			username   TEXT NOT NULL,
			namespace  TEXT NOT NULL,
			created_at TIMESTAMP NOT NULL,
			expires_at TIMESTAMP NOT NULL,
			revoked_at TIMESTAMP
		);

CREATE INDEX IF NOT EXISTS idx_auth_sessions_subject
			ON auth_sessions(issuer, subject, created_at);
