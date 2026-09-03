package sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestSessionTurnSchemaMigrationRemovesControlRowForeignKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-turns.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	statements := []string{
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE sessions (
			namespace TEXT NOT NULL, name TEXT NOT NULL, session_type TEXT NOT NULL DEFAULT 'task',
			active_task TEXT NOT NULL DEFAULT '', message_count INTEGER NOT NULL DEFAULT 0,
			input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0,
			cancelled BOOLEAN NOT NULL DEFAULT FALSE, created_at TIMESTAMP, updated_at TIMESTAMP,
			PRIMARY KEY(namespace, name)
		)`,
		`CREATE TABLE session_controls (
			namespace TEXT NOT NULL, session_name TEXT NOT NULL, session_uid TEXT NOT NULL UNIQUE,
			availability TEXT NOT NULL, updated_at TIMESTAMP NOT NULL,
			PRIMARY KEY(namespace, session_name),
			FOREIGN KEY(namespace, session_name) REFERENCES sessions(namespace, name)
		)`,
		`CREATE TABLE session_turns (
			id TEXT PRIMARY KEY, session_uid TEXT NOT NULL, lease_generation INTEGER NOT NULL,
			task_uid TEXT NOT NULL, attempt INTEGER NOT NULL, prompt_id TEXT NOT NULL,
			prompt_attempt_id TEXT NOT NULL, request_digest TEXT NOT NULL, user_prompt TEXT NOT NULL,
			state TEXT NOT NULL, terminal_kind TEXT NOT NULL DEFAULT '', terminal_content TEXT NOT NULL DEFAULT '',
			finalization_digest TEXT NOT NULL DEFAULT '', publication_id TEXT NOT NULL DEFAULT '',
			publication_receipt BLOB, controller_epoch_name TEXT NOT NULL, controller_epoch INTEGER NOT NULL,
			version INTEGER NOT NULL, created_at TIMESTAMP NOT NULL, finalized_at TIMESTAMP, updated_at TIMESTAMP NOT NULL,
			UNIQUE(session_uid, lease_generation, task_uid, attempt, prompt_id),
			FOREIGN KEY(session_uid) REFERENCES session_controls(session_uid)
		)`,
		`INSERT INTO sessions(namespace, name, created_at, updated_at) VALUES ('tenant-a', 'session-a', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		`INSERT INTO session_controls(namespace, session_name, session_uid, availability, updated_at)
		 VALUES ('tenant-a', 'session-a', 'session-uid-a', 'Available', CURRENT_TIMESTAMP)`,
		`INSERT INTO session_turns(
			id, session_uid, lease_generation, task_uid, attempt, prompt_id, prompt_attempt_id,
			request_digest, user_prompt, state, controller_epoch_name, controller_epoch, version, created_at, updated_at
		) VALUES (
			'turn-a', 'session-uid-a', 1, 'task-a', 1, 'prompt-a', 'attempt-a',
			'sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'hello', 'Open',
			'orka-controller', 1, 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP
		)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			t.Fatalf("legacy setup failed for %q: %v", statement, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	migrated, err := NewDB(path)
	if err != nil {
		t.Fatalf("NewDB migration: %v", err)
	}
	t.Cleanup(func() { _ = migrated.Close() })
	var namespace, sessionName string
	if err := migrated.QueryRow(`SELECT namespace, session_name FROM session_turns WHERE id = 'turn-a'`).Scan(&namespace, &sessionName); err != nil {
		t.Fatalf("read migrated turn binding: %v", err)
	}
	if namespace != "tenant-a" || sessionName != "session-a" {
		t.Fatalf("migrated binding = %s/%s", namespace, sessionName)
	}
	rows, err := migrated.Query(`PRAGMA foreign_key_list(session_turns)`)
	if err != nil {
		t.Fatalf("foreign_key_list: %v", err)
	}
	for rows.Next() {
		var id, seq int
		var table, from, to, onUpdate, onDelete, match string
		if err := rows.Scan(&id, &seq, &table, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			t.Fatalf("scan foreign key: %v", err)
		}
		if table == "session_controls" {
			t.Fatal("session_turns still depends on SQLite session_controls")
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("foreign_key_list rows: %v", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close foreign_key_list rows: %v", err)
	}

	var createdAt time.Time
	if err := migrated.QueryRow(`SELECT created_at FROM session_turns WHERE id = 'turn-a'`).Scan(&createdAt); err != nil {
		t.Fatalf("legacy turn timestamp was not preserved: %v", err)
	}
}
