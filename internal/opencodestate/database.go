package opencodestate

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const maxDatabaseBytes = 128 << 20

// These are the native columns at 4da7bb44c84e013fa53e9c5d02ac753d1435c81a,
// packages/core/src/database/schema.gen.ts. SQL identifiers below are fixed in
// this package, never taken from the database or the private snapshot.
var nativeColumns = map[string]string{
	"project": "id:text worktree:text vcs:text name:text icon_url:text icon_url_override:text icon_color:text " +
		"time_created:integer time_updated:integer time_initialized:integer sandboxes:text commands:text",
	"session": "id:text project_id:text workspace_id:text parent_id:text slug:text directory:text path:text title:text " +
		"version:text share_url:text summary_additions:integer summary_deletions:integer summary_files:integer " +
		"summary_diffs:text metadata:text cost:real tokens_input:integer tokens_output:integer tokens_reasoning:integer " +
		"tokens_cache_read:integer tokens_cache_write:integer revert:text permission:text agent:text model:text " +
		"time_created:integer time_updated:integer time_compacting:integer time_archived:integer",
	"message": "id:text session_id:text time_created:integer time_updated:integer data:text",
	"part":    "id:text message_id:text session_id:text time_created:integer time_updated:integer data:text",
}

var nativeTables = strings.Fields("workspace data_migration account_state account control_account credential " +
	"event_sequence event permission project_directory project message part session_context_epoch session_input " +
	"session_message session todo session_share migration __drizzle_migrations")

func openDatabase(ctx context.Context, path string, readOnly bool) (*sql.DB, *sql.Conn, error) {
	if err := validateDatabasePath(path); err != nil {
		return nil, nil, err
	}
	mode := "rw"
	if readOnly {
		mode = "ro"
	}
	dsn := url.URL{Scheme: "file", Path: path, RawQuery: url.Values{"mode": {mode}}.Encode()}
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, nil, databaseError(ctx, "open native database")
	}
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		_ = db.Close()
		return nil, nil, databaseError(ctx, "open native database connection")
	}
	if _, err := sqlite.Limit(conn, sqlite3.SQLITE_LIMIT_LENGTH, MaxRowBytes); err != nil {
		closeDatabase(db, conn)
		return nil, nil, databaseError(ctx, "bound native database rows")
	}
	pragmas := []string{"PRAGMA trusted_schema=OFF", "PRAGMA foreign_keys=ON", "PRAGMA busy_timeout=250"}
	if readOnly {
		pragmas = append(pragmas, "PRAGMA query_only=ON")
	}
	for _, pragma := range pragmas {
		if _, err := conn.ExecContext(ctx, pragma); err != nil {
			closeDatabase(db, conn)
			return nil, nil, databaseError(ctx, "configure native database connection")
		}
	}
	return db, conn, nil
}

func closeDatabase(db *sql.DB, conn *sql.Conn) {
	_ = conn.Close()
	_ = db.Close()
}

func validateDatabasePath(path string) error {
	if !canonicalAbsolutePath(path) {
		return incompatible("native database path must be canonical and absolute")
	}
	for current := filepath.Dir(path); ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return incompatible("native database ancestors must be real directories")
		}
		if current == filepath.Dir(current) {
			break
		}
	}
	var total int64
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		info, err := os.Lstat(path + suffix)
		if os.IsNotExist(err) && suffix != "" {
			continue
		}
		if err != nil || !info.Mode().IsRegular() {
			return incompatible("native database and sidecars must be regular files")
		}
		total += info.Size()
		if total > maxDatabaseBytes {
			return ErrSizeLimit
		}
	}
	return nil
}

func validateSchema(ctx context.Context, tx *sql.Tx) error {
	known := make(map[string]bool, len(nativeTables))
	for _, table := range nativeTables {
		known[table] = true
	}
	rows, err := tx.QueryContext(ctx, "SELECT name, type FROM sqlite_schema WHERE name NOT GLOB 'sqlite_*'")
	if err != nil {
		return databaseError(ctx, "inspect native schema")
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name, kind string
		if err := rows.Scan(&name, &kind); err != nil {
			return databaseError(ctx, "inspect native schema row")
		}
		if kind == "trigger" || kind == "view" || kind == "table" && !known[name] {
			return incompatible("native database has an unsupported schema")
		}
	}
	if err := rows.Err(); err != nil {
		return databaseError(ctx, "read native schema")
	}
	if err := rows.Close(); err != nil {
		return databaseError(ctx, "finish native schema read")
	}
	for table, columns := range nativeColumns {
		if err := validateColumns(ctx, tx, table, columns); err != nil {
			return err
		}
	}
	return nil
}

func validateColumns(ctx context.Context, tx *sql.Tx, table, columns string) error {
	var ddl string
	if err := tx.QueryRowContext(ctx,
		"SELECT sql FROM sqlite_schema WHERE type='table' AND name=?", table,
	).Scan(&ddl); err != nil {
		return databaseError(ctx, "inspect conversation table")
	}
	if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(ddl)), "CREATE TABLE ") {
		return incompatible("conversation table is not an ordinary table")
	}
	rows, err := tx.QueryContext(ctx, "SELECT name, type, hidden FROM pragma_table_xinfo(?) ORDER BY cid", table)
	if err != nil {
		return databaseError(ctx, "inspect conversation columns")
	}
	defer func() { _ = rows.Close() }()
	expected := strings.Fields(columns)
	index := 0
	for rows.Next() {
		var name, kind string
		var hidden int
		if err := rows.Scan(&name, &kind, &hidden); err != nil {
			return databaseError(ctx, "inspect conversation column")
		}
		if index >= len(expected) || expected[index] != name+":"+strings.ToLower(kind) || hidden != 0 {
			return incompatible("conversation columns do not match the pinned version")
		}
		index++
	}
	if rows.Err() != nil {
		return databaseError(ctx, "read conversation columns")
	}
	if index != len(expected) {
		return incompatible("conversation columns are incomplete")
	}
	return nil
}

func validateCaptureTables(ctx context.Context, tx *sql.Tx, sessionID string) error {
	checks := []struct {
		query  string
		reason string
	}{
		{"SELECT EXISTS(SELECT 1 FROM session WHERE id != ?)", "database contains another native session"},
		{"SELECT EXISTS(SELECT 1 FROM message WHERE session_id != ?)", "database contains foreign messages"},
		{"SELECT EXISTS(SELECT 1 FROM part WHERE session_id != ?)", "database contains foreign message parts"},
		{"SELECT EXISTS(SELECT 1 FROM todo WHERE session_id = ?)", "native todo state is unsupported"},
		{"SELECT EXISTS(SELECT 1 FROM session_input WHERE session_id = ?)", "native input inbox is unsupported"},
		{"SELECT EXISTS(SELECT 1 FROM session_message WHERE session_id = ?)", "native v2 history is unsupported"},
		{"SELECT EXISTS(SELECT 1 FROM session_context_epoch WHERE session_id = ?)", "native context epochs are unsupported"},
		{`SELECT EXISTS(SELECT 1 FROM session WHERE id = ? AND (
			workspace_id IS NOT NULL OR parent_id IS NOT NULL OR revert IS NOT NULL OR
			time_compacting IS NOT NULL OR time_archived IS NOT NULL OR
			(permission IS NOT NULL AND permission NOT IN ('[]', 'null'))))`, "native session has retained execution or permission state"},
	}
	for _, check := range checks {
		var exists bool
		if err := tx.QueryRowContext(ctx, check.query, sessionID).Scan(&exists); err != nil {
			return databaseError(ctx, "verify conversation isolation")
		}
		if exists {
			return incompatible(check.reason)
		}
	}
	return nil
}

func validateEmptyDatabase(ctx context.Context, tx *sql.Tx) error {
	// Schema initialization may populate only its migration journals. All
	// conversation, credential, account, permission and operational tables must
	// still be empty. In particular, never merge into an existing native session.
	for _, table := range nativeTables {
		if table == "migration" || table == "__drizzle_migrations" || table == "data_migration" {
			continue
		}
		var exists bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM "+table+")").Scan(&exists); err != nil {
			return databaseError(ctx, "verify empty restore database")
		}
		if exists {
			return incompatible("restore database already contains native state")
		}
	}
	return nil
}

func readConversation(ctx context.Context, tx *sql.Tx, saved *snapshot, sessionID string) error {
	var model string
	s := &saved.Session
	if err := tx.QueryRowContext(ctx, `SELECT id, project_id, slug, directory, path, title, version,
		agent, model, time_created, time_updated FROM session WHERE id = ?`, sessionID).Scan(
		&s.ID, &s.ProjectID, &s.Slug, &s.Directory, &s.Path, &s.Title, &s.Version,
		&s.Agent, &model, &s.TimeCreated, &s.TimeUpdated,
	); err != nil {
		return databaseError(ctx, "read selected conversation")
	}
	s.Model = json.RawMessage(model)
	p := &saved.Project
	if err := tx.QueryRowContext(ctx, `SELECT id, worktree, vcs, time_created, time_updated FROM project WHERE id = ?`,
		s.ProjectID).Scan(&p.ID, &p.Worktree, &p.VCS, &p.TimeCreated, &p.TimeUpdated); err != nil {
		return databaseError(ctx, "read conversation project identity")
	}
	if err := readMessages(ctx, tx, saved); err != nil {
		return err
	}
	return readParts(ctx, tx, saved)
}

func readMessages(ctx context.Context, tx *sql.Tx, saved *snapshot) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, session_id, time_created, time_updated, data FROM message
		WHERE session_id = ? ORDER BY time_created, id LIMIT ?`, saved.Session.ID, MaxMessages+1)
	if err != nil {
		return databaseError(ctx, "read conversation messages")
	}
	defer func() { _ = rows.Close() }()
	var total int
	for rows.Next() {
		var row messageRow
		var data string
		if err := rows.Scan(&row.ID, &row.SessionID, &row.TimeCreated, &row.TimeUpdated, &data); err != nil {
			return databaseError(ctx, "read bounded conversation message")
		}
		total += len(data)
		if len(saved.Messages) >= MaxMessages || total > MaxDecompressedBytes {
			return ErrSizeLimit
		}
		row.Data = json.RawMessage(data)
		saved.Messages = append(saved.Messages, row)
	}
	if rows.Err() != nil {
		return databaseError(ctx, "finish conversation messages")
	}
	return nil
}

func readParts(ctx context.Context, tx *sql.Tx, saved *snapshot) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, message_id, session_id, time_created, time_updated, data FROM part
		WHERE session_id = ? ORDER BY message_id, id LIMIT ?`, saved.Session.ID, MaxParts+1)
	if err != nil {
		return databaseError(ctx, "read conversation parts")
	}
	defer func() { _ = rows.Close() }()
	var total int
	for _, message := range saved.Messages {
		total += len(message.Data)
	}
	for rows.Next() {
		var row partRow
		var data string
		if err := rows.Scan(&row.ID, &row.MessageID, &row.SessionID, &row.TimeCreated, &row.TimeUpdated, &data); err != nil {
			return databaseError(ctx, "read bounded conversation part")
		}
		total += len(data)
		if len(saved.Parts) >= MaxParts || total > MaxDecompressedBytes {
			return ErrSizeLimit
		}
		row.Data = json.RawMessage(data)
		saved.Parts = append(saved.Parts, row)
	}
	if rows.Err() != nil {
		return databaseError(ctx, "finish conversation parts")
	}
	return nil
}

func writeConversation(ctx context.Context, tx *sql.Tx, saved *snapshot) error {
	p, s := saved.Project, saved.Session
	if _, err := tx.ExecContext(ctx, `INSERT INTO project
		(id, worktree, vcs, time_created, time_updated, sandboxes) VALUES (?, ?, ?, ?, ?, '[]')`,
		p.ID, p.Worktree, p.VCS, p.TimeCreated, p.TimeUpdated,
	); err != nil {
		return databaseError(ctx, "restore conversation project identity")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO session
		(id, project_id, slug, directory, path, title, version, agent, model, time_created, time_updated)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, s.ID, s.ProjectID, s.Slug, s.Directory, s.Path,
		s.Title, s.Version, Mode, string(s.Model), s.TimeCreated, s.TimeUpdated,
	); err != nil {
		return databaseError(ctx, "restore conversation identity")
	}
	for _, row := range saved.Messages {
		if _, err := tx.ExecContext(ctx, `INSERT INTO message
			(id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?)`,
			row.ID, row.SessionID, row.TimeCreated, row.TimeUpdated, string(row.Data),
		); err != nil {
			return databaseError(ctx, "restore conversation message")
		}
	}
	for _, row := range saved.Parts {
		if _, err := tx.ExecContext(ctx, `INSERT INTO part
			(id, message_id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?, ?)`,
			row.ID, row.MessageID, row.SessionID, row.TimeCreated, row.TimeUpdated, string(row.Data),
		); err != nil {
			return databaseError(ctx, "restore conversation part")
		}
	}
	return nil
}
