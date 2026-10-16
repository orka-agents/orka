-- Additive historical SQLite transition: 150-to-162.
-- Source layout: 1f18614ca9dd2339a9c8caac8e3320e7daf61c2e (150 objects).
-- Target definitions: 57583975d265a1ca23ebc8106d8fe769c84ae4f3 (162 objects).
-- Frozen from the target commit's real schema statements; do not use mutable helpers.
-- Transaction and complete-layout validation are owned by the caller.

CREATE TABLE IF NOT EXISTS connector_completions (
			nonce           TEXT PRIMARY KEY,
			connection_uid  TEXT NOT NULL,
			namespace       TEXT NOT NULL,
			name            TEXT NOT NULL,
			subject_digest  TEXT NOT NULL,
			provider        TEXT NOT NULL,
			mode            TEXT NOT NULL,
			consent_sequence INTEGER NOT NULL DEFAULT 0,
			payload_nonce   BLOB NOT NULL,
			payload         BLOB NOT NULL,
			expires_at      TIMESTAMP NOT NULL,
			created_at      TIMESTAMP NOT NULL
		);

CREATE TABLE IF NOT EXISTS connector_consent_sequences (
			connection_uid  TEXT PRIMARY KEY,
			issued          INTEGER NOT NULL,
			committed       INTEGER NOT NULL DEFAULT 0
		);

CREATE TABLE IF NOT EXISTS connector_consents (
			nonce               TEXT PRIMARY KEY,
			connection_uid      TEXT NOT NULL,
			namespace           TEXT NOT NULL,
			name                TEXT NOT NULL,
			subject_digest      TEXT NOT NULL,
			provider            TEXT NOT NULL,
			mode                TEXT NOT NULL,
			verifier_nonce      BLOB NOT NULL,
			verifier_ciphertext BLOB NOT NULL,
			authority_digest    TEXT NOT NULL DEFAULT '',
			revocation_digest   TEXT NOT NULL DEFAULT '',
			scopes              TEXT NOT NULL DEFAULT '',
			sequence            INTEGER NOT NULL DEFAULT 0,
			expires_at          TIMESTAMP NOT NULL,
			created_at          TIMESTAMP NOT NULL
		);

CREATE TABLE IF NOT EXISTS connector_credential_grants (
			connection_uid  TEXT PRIMARY KEY,
			grant_sequence  INTEGER NOT NULL
		);

CREATE TABLE IF NOT EXISTS connector_credential_tombstones (
			connection_uid  TEXT PRIMARY KEY,
			deleted_at      TIMESTAMP NOT NULL
		);

CREATE TABLE IF NOT EXISTS connector_credential_versions (
			connection_uid  TEXT PRIMARY KEY,
			last_version    INTEGER NOT NULL
		);

CREATE TABLE IF NOT EXISTS connector_credentials (
			connection_uid  TEXT PRIMARY KEY,
			namespace       TEXT NOT NULL,
			name            TEXT NOT NULL,
			subject_digest  TEXT NOT NULL,
			provider        TEXT NOT NULL,
			dek_nonce       BLOB NOT NULL,
			dek_ciphertext  BLOB NOT NULL,
			nonce           BLOB NOT NULL,
			ciphertext      BLOB NOT NULL,
			expires_at      TIMESTAMP,
			version         INTEGER NOT NULL DEFAULT 1,
			grant_sequence  INTEGER NOT NULL DEFAULT 0,
			created_at      TIMESTAMP NOT NULL,
			updated_at      TIMESTAMP NOT NULL
		);

CREATE TABLE IF NOT EXISTS connector_retired_credentials (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			connection_uid TEXT NOT NULL,
			dek_nonce      BLOB NOT NULL,
			dek_ciphertext BLOB NOT NULL,
			nonce          BLOB NOT NULL,
			ciphertext     BLOB NOT NULL,
			revocable_until TIMESTAMP,
			consent_grant  INTEGER NOT NULL DEFAULT 0,
			retired_at     TIMESTAMP NOT NULL
		);

CREATE INDEX IF NOT EXISTS idx_connector_completions_connection
			ON connector_completions(connection_uid);

CREATE INDEX IF NOT EXISTS idx_connector_consents_connection
			ON connector_consents(connection_uid);

CREATE INDEX IF NOT EXISTS idx_connector_credentials_subject
			ON connector_credentials(namespace, subject_digest, provider);

CREATE INDEX IF NOT EXISTS idx_connector_retired_credentials_connection
			ON connector_retired_credentials(connection_uid);
