/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package sqlite

// agentExecutionSchemaStatements defines encrypted snapshots and Session lineage.
func agentExecutionSchemaStatements() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS agent_execution_snapshots (
			task_uid        TEXT NOT NULL,
			digest          TEXT NOT NULL,
			schema_version  INTEGER NOT NULL CHECK(schema_version > 0),
			nonce           BLOB NOT NULL,
			ciphertext      BLOB NOT NULL,
			created_at      TIMESTAMP NOT NULL,
			PRIMARY KEY (task_uid, digest)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_agent_execution_snapshots_task
			ON agent_execution_snapshots(task_uid, created_at ASC)`,

		`CREATE TABLE IF NOT EXISTS session_lineages (
			namespace          TEXT NOT NULL,
			session_name       TEXT NOT NULL,
			namespace_uid      TEXT NOT NULL,
			session_uid        TEXT NOT NULL,
			contract_version   TEXT NOT NULL CHECK(contract_version = 'orka.harness.v2'),
			lineage_generation INTEGER NOT NULL CHECK(lineage_generation > 0),
			runtime_identity   TEXT NOT NULL,
			config_digest      TEXT NOT NULL DEFAULT '',
			version            INTEGER NOT NULL CHECK(version > 0),
			created_at         TIMESTAMP NOT NULL,
			updated_at         TIMESTAMP NOT NULL,
			PRIMARY KEY (namespace, session_name),
			UNIQUE (session_uid)
		)`,
	}
}
