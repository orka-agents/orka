-- Additive historical SQLite transition: 162-to-166.
-- Source layout: 57583975d265a1ca23ebc8106d8fe769c84ae4f3 (162 objects).
-- Target definitions: abd2c0406ab660abd37af7c176b4eef610a19ce1 (166 objects).
-- Frozen from the target commit's real schema statements; do not use mutable helpers.
-- Transaction and complete-layout validation are owned by the caller.

CREATE TABLE IF NOT EXISTS native_session_deleted_names (
			namespace TEXT NOT NULL,
			session_name TEXT NOT NULL,
			PRIMARY KEY(namespace, session_name)
		);

CREATE TABLE IF NOT EXISTS native_session_finalizations (
			turn_id TEXT NOT NULL PRIMARY KEY,
			namespace TEXT NOT NULL,
			session_name TEXT NOT NULL,
			capture_digest TEXT NOT NULL,
			FOREIGN KEY(namespace, session_name) REFERENCES sessions(namespace, name) ON DELETE CASCADE
		);

CREATE TABLE IF NOT EXISTS native_session_operations (
			namespace TEXT NOT NULL,
			session_name TEXT NOT NULL,
			operation_id TEXT NOT NULL,
			request_digest TEXT NOT NULL,
			kind TEXT NOT NULL,
			receipt BLOB NOT NULL,
			PRIMARY KEY(namespace, session_name, kind, operation_id),
			FOREIGN KEY(namespace, session_name) REFERENCES sessions(namespace, name) ON DELETE CASCADE
		);

CREATE TABLE IF NOT EXISTS native_session_snapshots (
			namespace TEXT NOT NULL,
			session_name TEXT NOT NULL,
			session_uid TEXT NOT NULL,
			record_digest TEXT NOT NULL,
			data_digest TEXT NOT NULL,
			source_operation_id TEXT NOT NULL,
			runtime_generation INTEGER NOT NULL,
			message_count INTEGER NOT NULL,
			through_message_id TEXT NOT NULL,
			nonce BLOB NOT NULL,
			ciphertext BLOB NOT NULL,
			PRIMARY KEY(namespace, session_name),
			FOREIGN KEY(namespace, session_name) REFERENCES sessions(namespace, name) ON DELETE CASCADE
		);
