-- Additive SQLite transition: 173-to-174 (schema version 6 to 7).
-- Source layout: the Agent Host Protocol layout (schema version 6).
-- Target definitions: GitHub sign-in sessions, schema version 7.
-- It adds only an empty table and its index, so a store whose key bootstrap
-- is unfinished can still finish it.
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
