-- Additive SQLite transition: 168-to-169.
-- Source layout: 282e6c3d07185a8a90b2b1da5b41bd40d147467e (#863, 159 objects), schema version 0 or 1.
-- Target definitions: Agent Host Protocol tokens, clients, journal, channel states, sessions and backend records (168 objects), schema version 2.
-- Frozen from the target's real schema statements; do not use mutable helpers.
-- Transaction and complete-layout validation are owned by the caller.

CREATE TABLE IF NOT EXISTS ahp_tokens (
			id          TEXT PRIMARY KEY,
			token_hash  TEXT NOT NULL UNIQUE,
			user_key    TEXT NOT NULL,
			issuer      TEXT NOT NULL,
			subject     TEXT NOT NULL,
			username    TEXT NOT NULL,
			namespace   TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			created_at  TIMESTAMP NOT NULL,
			expires_at  TIMESTAMP NOT NULL,
			revoked_at  TIMESTAMP
		);

CREATE INDEX IF NOT EXISTS idx_ahp_tokens_user
			ON ahp_tokens(user_key, created_at);

CREATE TABLE IF NOT EXISTS ahp_clients (
			client_id        TEXT PRIMARY KEY,
			user_key         TEXT NOT NULL,
			protocol_version TEXT NOT NULL,
			dialect          TEXT NOT NULL,
			created_at       TIMESTAMP NOT NULL,
			updated_at       TIMESTAMP NOT NULL
		);

CREATE TABLE IF NOT EXISTS ahp_users (
			user_key   TEXT PRIMARY KEY,
			server_seq INTEGER NOT NULL,
			updated_at TIMESTAMP NOT NULL
		);

CREATE TABLE IF NOT EXISTS ahp_journal (
			user_key TEXT NOT NULL,
			seq      INTEGER NOT NULL,
			channel  TEXT NOT NULL,
			envelope BLOB NOT NULL,
			size     INTEGER NOT NULL,
			PRIMARY KEY (user_key, seq)
		);

CREATE TABLE IF NOT EXISTS ahp_channel_states (
			user_key TEXT NOT NULL,
			channel  TEXT NOT NULL,
			state    BLOB NOT NULL,
			from_seq INTEGER NOT NULL,
			PRIMARY KEY (user_key, channel)
		);

CREATE TABLE IF NOT EXISTS ahp_sessions (
			user_key    TEXT NOT NULL,
			session     TEXT NOT NULL,
			backend     TEXT NOT NULL,
			summary     BLOB NOT NULL,
			created_at  TIMESTAMP NOT NULL,
			modified_at TIMESTAMP NOT NULL,
			PRIMARY KEY (user_key, session)
		);

CREATE INDEX IF NOT EXISTS idx_ahp_sessions_modified
			ON ahp_sessions(user_key, modified_at);

CREATE TABLE IF NOT EXISTS ahp_records (
			user_key   TEXT NOT NULL,
			kind       TEXT NOT NULL,
			key        TEXT NOT NULL,
			value      BLOB NOT NULL,
			updated_at TIMESTAMP NOT NULL,
			PRIMARY KEY (user_key, kind, key)
		);
