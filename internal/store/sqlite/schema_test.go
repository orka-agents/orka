package sqlite

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewDBRejectsIncompatibleSchemaWithoutChangingData(t *testing.T) {
	for _, test := range []struct {
		name      string
		statement string
		object    string
		old, new  string
	}{
		{name: "missing table", statement: `DROP TABLE prompt_result_receipts`},
		{name: "missing column", statement: `ALTER TABLE security_scan_runs DROP COLUMN repository_scan_uid`},
		{name: "missing index", statement: `DROP INDEX idx_session_messages_message_id`},
		{name: "extra table", statement: `CREATE TABLE retired_state (value TEXT)`},
		{
			name: "review slice identity", object: "security_review_slices",
			old: "PRIMARY KEY (namespace, repository_scan, id)", new: "PRIMARY KEY (id)",
		},
		{
			name: "session turn authority", object: "session_turns",
			old: "REFERENCES sessions(namespace, name)", new: "REFERENCES session_controls(namespace, session_name)",
		},
		{
			name: "required value", object: "prompt_attempts",
			old: "CHECK(attempt > 0)", new: "",
		},
		{
			name: "default literal whitespace", object: "sessions",
			old: "DEFAULT 'task'", new: "DEFAULT 'task '",
		},
		{
			name: "column constraint", object: "results",
			old: "BLOB NOT NULL", new: "BLOBNOTNULL",
		},
		{
			name: "quoted constraint keywords", object: "results",
			old: "BLOB NOT NULL", new: `BLOB "NOT" "NULL"`,
		},
		{
			name: "deduplication namespace", object: "idx_command_events_dedupe",
			old: "(monitor_namespace, monitor_name, dedupe_key)", new: "(monitor_name, dedupe_key)",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "store.db")
			db, err := NewDB(dbPath)
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			ctx := context.Background()
			if test.object != "" {
				replaceSchemaObject(t, db, test.object, test.old, test.new)
			} else {
				_, err = db.Exec(test.statement)
				require.NoError(t, err)
			}
			require.NoError(t, NewStore(db, dbPath).SaveResult(ctx, "ns", "task", []byte("saved result")))
			before := storedSchemaSQL(t, db)
			require.NoError(t, db.Close())
			info, err := os.Stat(dbPath)
			require.NoError(t, err)

			for range 2 {
				rejected, err := NewDB(dbPath)
				if rejected != nil {
					_ = rejected.Close()
				}
				require.ErrorContains(t, err, "unsupported SQLite schema")
				require.Nil(t, rejected)

				// Inspect the rejected file without invoking setup again.
				raw, err := sql.Open("sqlite", dbPath)
				require.NoError(t, err)
				t.Cleanup(func() { _ = raw.Close() })
				require.Equal(t, before, storedSchemaSQL(t, raw))
				data, err := NewStore(raw, dbPath).GetResult(ctx, "ns", "task")
				require.NoError(t, err)
				require.Equal(t, "saved result", string(data))
				require.NoError(t, raw.Close())
				after, err := os.Stat(dbPath)
				require.NoError(t, err)
				require.True(t, os.SameFile(info, after), "setup replaced the database file")
			}
		})
	}
}

func TestNewDBAcceptsCurrentSchemaFormatting(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "store.db")
	db, err := NewDB(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	replaceSchemaObject(t, db, "security_scan_runs", "CREATE TABLE security_scan_runs (", "create\n table \"security_scan_runs\"(")
	before := storedSchemaSQL(t, db)
	require.NoError(t, db.Close())

	reopened, err := NewDB(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reopened.Close() })
	require.Equal(t, before, storedSchemaSQL(t, reopened))
}

func TestInitializeCurrentSchemaDoesNotWrite(t *testing.T) {
	s := setupDiskStore(t)
	require.NoError(t, s.SaveResult(context.Background(), "ns", "task", []byte("saved result")))
	_, err := s.db.Exec(`PRAGMA query_only = ON`)
	require.NoError(t, err)
	require.NoError(t, initializeSchema(s.db))
	data, err := s.GetResult(context.Background(), "ns", "task")
	require.NoError(t, err)
	require.Equal(t, "saved result", string(data))
}

func storedSchemaSQL(t *testing.T, db *sql.DB) string {
	t.Helper()
	var schema string
	require.NoError(t, db.QueryRow(`SELECT group_concat(sql, char(10)) FROM (
		SELECT sql FROM sqlite_schema WHERE sql IS NOT NULL ORDER BY name
	)`).Scan(&schema))
	return schema
}

func replaceSchemaObject(t *testing.T, db *sql.DB, name, old, replacement string) {
	t.Helper()
	var kind, definition string
	require.NoError(t, db.QueryRow(`SELECT type, sql FROM sqlite_schema WHERE name = ?`, name).Scan(&kind, &definition))
	require.Contains(t, definition, old)
	// Preserve the table's indexes so each case changes only the named rule.
	rows, err := db.Query(`SELECT sql FROM sqlite_schema WHERE type = 'index' AND tbl_name = ? AND sql IS NOT NULL`, name)
	require.NoError(t, err)
	var indexes []string
	for rows.Next() {
		var index string
		require.NoError(t, rows.Scan(&index))
		indexes = append(indexes, index)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	_, err = db.Exec("DROP " + kind + " " + name)
	require.NoError(t, err)
	_, err = db.Exec(strings.Replace(definition, old, replacement, 1))
	require.NoError(t, err)
	for _, index := range indexes {
		_, err = db.Exec(index)
		require.NoError(t, err)
	}
}
