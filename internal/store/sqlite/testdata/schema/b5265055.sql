-- Independent #700 delta from b526505518fa139848cc610f6055b33e919ce594 (150 to 161 objects).
-- Add to the base and frozen 149-to-150 step; never use the #701 connector definitions here.
CREATE TABLE connector_completions ( nonce TEXT PRIMARY KEY, connection_uid TEXT NOT NULL, namespace TEXT NOT NULL, name TEXT NOT NULL, subject_digest TEXT NOT NULL, provider TEXT NOT NULL, mode TEXT NOT NULL, consent_sequence INTEGER NOT NULL DEFAULT 0, payload_nonce BLOB NOT NULL, payload BLOB NOT NULL, expires_at TIMESTAMP NOT NULL, created_at TIMESTAMP NOT NULL );
CREATE TABLE connector_consent_sequences ( connection_uid TEXT PRIMARY KEY, issued INTEGER NOT NULL, committed INTEGER NOT NULL DEFAULT 0 );
CREATE TABLE connector_consents ( nonce TEXT PRIMARY KEY, connection_uid TEXT NOT NULL, namespace TEXT NOT NULL, name TEXT NOT NULL, subject_digest TEXT NOT NULL, provider TEXT NOT NULL, mode TEXT NOT NULL, verifier_nonce BLOB NOT NULL, verifier_ciphertext BLOB NOT NULL, authority_digest TEXT NOT NULL DEFAULT '', revocation_digest TEXT NOT NULL DEFAULT '', scopes TEXT NOT NULL DEFAULT '', sequence INTEGER NOT NULL DEFAULT 0, expires_at TIMESTAMP NOT NULL, created_at TIMESTAMP NOT NULL );
CREATE TABLE connector_credential_grants ( connection_uid TEXT PRIMARY KEY, grant_sequence INTEGER NOT NULL );
CREATE TABLE connector_credential_tombstones ( connection_uid TEXT PRIMARY KEY, deleted_at TIMESTAMP NOT NULL );
CREATE TABLE connector_credentials ( connection_uid TEXT PRIMARY KEY, namespace TEXT NOT NULL, name TEXT NOT NULL, subject_digest TEXT NOT NULL, provider TEXT NOT NULL, dek_nonce BLOB NOT NULL, dek_ciphertext BLOB NOT NULL, nonce BLOB NOT NULL, ciphertext BLOB NOT NULL, expires_at TIMESTAMP, grant_sequence INTEGER NOT NULL DEFAULT 0, created_at TIMESTAMP NOT NULL, updated_at TIMESTAMP NOT NULL );
CREATE TABLE connector_retired_credentials ( id INTEGER PRIMARY KEY AUTOINCREMENT, connection_uid TEXT NOT NULL, dek_nonce BLOB NOT NULL, dek_ciphertext BLOB NOT NULL, nonce BLOB NOT NULL, ciphertext BLOB NOT NULL, consent_grant INTEGER NOT NULL DEFAULT 0, retired_at TIMESTAMP NOT NULL );
CREATE INDEX idx_connector_completions_connection ON connector_completions(connection_uid);
CREATE INDEX idx_connector_consents_connection ON connector_consents(connection_uid);
CREATE INDEX idx_connector_credentials_subject ON connector_credentials(namespace, subject_digest, provider);
CREATE INDEX idx_connector_retired_credentials_connection ON connector_retired_credentials(connection_uid);
