package sqlite

func nativeSessionSchemaStatements() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS native_session_snapshots (
			namespace             TEXT NOT NULL,
			session_name          TEXT NOT NULL,
			session_uid           TEXT NOT NULL UNIQUE,
			namespace_uid         TEXT NOT NULL,
			source_turn_id        TEXT NOT NULL UNIQUE,
			lease_generation      INTEGER NOT NULL CHECK(lease_generation > 0),
			state                 TEXT NOT NULL CHECK(state IN ('Staged','Finalized','Ready')),
			history_before_digest TEXT NOT NULL,
			metadata              BLOB NOT NULL CHECK(length(metadata) <= 32768),
			data                  BLOB NOT NULL CHECK(length(data) > 0 AND length(data) <= 524288),
			created_at            TIMESTAMP NOT NULL,
			expires_at            TIMESTAMP NOT NULL,
			PRIMARY KEY(namespace, session_name),
			FOREIGN KEY(namespace, session_name) REFERENCES sessions(namespace, name) ON DELETE CASCADE,
			FOREIGN KEY(source_turn_id) REFERENCES session_turns(id) ON DELETE CASCADE,
			CHECK(expires_at > created_at)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_native_session_snapshots_expiry
			ON native_session_snapshots(expires_at)`,
	}
}
