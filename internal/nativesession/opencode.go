package nativesession

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	_ "embed"

	// Registers the "sqlite" database/sql driver.
	_ "modernc.org/sqlite"
)

const (
	openCodeProviderKind = "opencode"
	// openCodeVersion is the OpenCode release whose database this format
	// carries. It must equal acp.OpenCodeVersion.
	openCodeVersion      = "1.19.0"
	openCodeBundleFormat = "orka/opencode-session-v1"

	openCodeDatabaseEntry   = "opencode.db"
	openCodeToolOutputEntry = "tool-output/"
	openCodeToolOutputDir   = "tool-output"
	maxOpenCodeToolOutputs  = 256

	openCodeMessageTable = "message"
	openCodePartTable    = "part"
)

var openCodeSessionIDPattern = regexp.MustCompile(`^ses_[A-Za-z0-9]{1,64}$`)

// openCodeSchemaSQL is the schema of a database OpenCode 1.19.0 created. A
// captured database is agent-written, so it must match this schema exactly
// before anything that evaluates its expressions runs, in the supervisor or
// in the controller.
//
//go:embed opencode_schema.sql
var openCodeSchemaSQL string

type openCodeSchema struct {
	entries []string
	tables  []string
	// sessionTables and projectTables are the tables with a session_id or a
	// project_id column.
	sessionTables, projectTables []string
}

// expectedOpenCodeSchema returns the pinned schema's sqlite_schema entries.
var expectedOpenCodeSchema = sync.OnceValues(func() (openCodeSchema, error) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return openCodeSchema{}, err
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, openCodeSchemaSQL); err != nil {
		return openCodeSchema{}, fmt.Errorf("load the pinned OpenCode schema: %w", err)
	}
	schema, err := readOpenCodeSchema(ctx, db)
	if err != nil {
		return openCodeSchema{}, err
	}
	keyedBy := func(column string) ([]string, error) {
		var tables []string
		for _, table := range schema.tables {
			has, err := sqliteExists(ctx, db, `SELECT EXISTS(SELECT 1 FROM pragma_table_info(?) WHERE name = ?)`, table, column)
			if err != nil {
				return nil, err
			}
			if has {
				tables = append(tables, table)
			}
		}
		return tables, nil
	}
	if schema.sessionTables, err = keyedBy("session_id"); err != nil {
		return openCodeSchema{}, err
	}
	if schema.projectTables, err = keyedBy("project_id"); err != nil {
		return openCodeSchema{}, err
	}
	return schema, nil
})

// readOpenCodeSchema reads every sqlite_schema entry, SQLite's own included,
// since a crafted file can give any object a reserved name. Reading it
// evaluates nothing the schema defines.
func readOpenCodeSchema(ctx context.Context, db *sql.DB) (openCodeSchema, error) {
	rows, err := db.QueryContext(ctx, `SELECT type, name, tbl_name, COALESCE(sql, '') FROM sqlite_schema ORDER BY type, name`)
	if err != nil {
		return openCodeSchema{}, err
	}
	defer func() { _ = rows.Close() }()
	var schema openCodeSchema
	for rows.Next() {
		var kind, name, table, statement string
		if err := rows.Scan(&kind, &name, &table, &statement); err != nil {
			return openCodeSchema{}, err
		}
		schema.entries = append(schema.entries, kind+"\x00"+name+"\x00"+table+"\x00"+statement)
		if kind == "table" {
			schema.tables = append(schema.tables, name)
		}
	}
	return schema, rows.Err()
}

// requireOpenCodeSchema returns the schema of db, which must equal the
// pinned OpenCode schema.
func requireOpenCodeSchema(ctx context.Context, db *sql.DB) (openCodeSchema, error) {
	expected, err := expectedOpenCodeSchema()
	if err != nil {
		return openCodeSchema{}, err
	}
	actual, err := readOpenCodeSchema(ctx, db)
	if err != nil {
		return openCodeSchema{}, err
	}
	if !slices.Equal(actual.entries, expected.entries) {
		return openCodeSchema{}, fmt.Errorf("%w: OpenCode database schema differs from OpenCode %s", ErrUnsupported, openCodeVersion)
	}
	return expected, nil
}

// openCodeCarriedTables hold the conversation. Capture keeps only the
// session's rows; the migration tables tell OpenCode its schema is current.
var openCodeCarriedTables = []string{"project", "session", openCodeMessageTable, openCodePartTable, "migration", "data_migration"}

// openCodeDroppedTables are not needed to resume, or must not cross turns:
// the duplicate update log, the directory index, persisted approvals,
// credentials, and sharing state. Rows in any other table make a capture
// unsupported, so state a newer OpenCode adds is never silently dropped.
var openCodeDroppedTables = []string{
	"event", "event_sequence", "project_directory", "permission",
	"account", "account_state", "control_account", "credential", "session_share",
}

// openCodeManifest describes a captured OpenCode conversation.
type openCodeManifest struct {
	Format    string `json:"format"`
	Version   string `json:"version"`
	SessionID string `json:"sessionID"`
	// Workspace is the captured RuntimeSession's working directory, which
	// OpenCode recorded as the session's directory.
	Workspace string `json:"workspace"`
	// ToolOutputDir is where the captured session spilled truncated tool
	// output. Parts reference those files by absolute path.
	ToolOutputDir string `json:"toolOutputDir"`
}

type openCodeBundle struct {
	manifest   openCodeManifest
	database   []byte
	toolOutput map[string][]byte
}

// openCodeFormat carries OpenCode's SQLite conversation: a consistent copy of
// opencode.db holding only the session's rows, plus the tool output it
// spilled. Dirs.State is XDG_DATA_HOME.
type openCodeFormat struct{}

func (openCodeFormat) Local() Local {
	return Local{
		Binary: "opencode", VersionLine: regexp.MustCompile(`^([0-9]+\.[0-9]+\.[0-9]+)$`), Release: openCodeVersion,
		StateVar: "XDG_DATA_HOME", HomeState: filepath.Join(".local", "share"),
	}
}

func (openCodeFormat) runtimeState(_, data string) string { return data }

func openCodeRoot(dirs Dirs) string { return filepath.Join(dirs.State, "opencode") }

func (openCodeFormat) Capture(ctx context.Context, dirs Dirs, providerSessionID string, maxBytes int) ([]byte, error) {
	limit, err := normalizeLimit(maxBytes)
	if err != nil {
		return nil, err
	}
	if !openCodeSessionIDPattern.MatchString(providerSessionID) {
		return nil, errors.New("OpenCode session ID is invalid")
	}
	if dirs.Workspace != "" && !absoluteCleanPath(dirs.Workspace) {
		return nil, errors.New("OpenCode workspace must be an absolute, clean path")
	}
	root := openCodeRoot(dirs)
	source := filepath.Join(root, openCodeDatabaseEntry)
	if err := requireRealPath(source, false); err != nil {
		return nil, err
	}
	// The live database also holds the duplicate event log, so it may be
	// larger than what Capture keeps, but not unboundedly so.
	stored := int64(0)
	for _, name := range []string{source, source + "-wal", source + "-shm", source + "-journal"} {
		if err := requireRealPathIfPresent(name, false); err != nil {
			return nil, err
		}
		if info, err := os.Lstat(name); err == nil {
			stored += info.Size()
		}
	}
	if stored > 2*int64(expandedLimit(limit)) {
		return nil, fmt.Errorf("%w: OpenCode database exceeds the capture budget", ErrUnsupported)
	}
	temp, err := privateTempDir()
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(temp) }()
	copied := filepath.Join(temp, openCodeDatabaseEntry)
	if err := copyOpenCodeDatabase(ctx, source, copied); err != nil {
		return nil, err
	}
	toolOutputDir := filepath.Join(root, openCodeToolOutputDir)
	workspace, spilled, err := pruneOpenCodeDatabase(ctx, copied, providerSessionID, dirs.Workspace, toolOutputDir)
	if err != nil {
		return nil, err
	}
	toolOutput, err := readOpenCodeToolOutput(toolOutputDir, spilled, limit)
	if err != nil {
		return nil, err
	}
	toolOutputBytes := 0
	for _, content := range toolOutput {
		toolOutputBytes += len(content)
	}
	database, err := readBoundedFile(copied, max(0, expandedLimit(limit)-toolOutputBytes))
	if err != nil {
		return nil, err
	}
	data, err := encodeOpenCodeBundle(limit, openCodeBundle{
		manifest: openCodeManifest{
			Format: openCodeBundleFormat, Version: openCodeVersion, SessionID: providerSessionID,
			Workspace: workspace, ToolOutputDir: toolOutputDir,
		},
		database: database, toolOutput: toolOutput,
	})
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, fmt.Errorf("%w: OpenCode conversation exceeds Orka's %d-byte transport limit", ErrUnsupported, limit)
	}
	return data, nil
}

func (openCodeFormat) Inspect(ctx context.Context, data []byte, maxBytes int) (Summary, error) {
	bundle, err := decodeOpenCodeBundle(data, maxBytes)
	if err != nil {
		return Summary{}, err
	}
	temp, err := privateTempDir()
	if err != nil {
		return Summary{}, err
	}
	defer func() { _ = os.RemoveAll(temp) }()
	database := filepath.Join(temp, openCodeDatabaseEntry)
	if err := writeNewFile(database, bundle.database); err != nil {
		return Summary{}, err
	}
	if err := verifyOpenCodeDatabase(ctx, database, bundle.manifest); err != nil {
		return Summary{}, err
	}
	return Summary{ProviderSessionID: bundle.manifest.SessionID, ProviderVersion: bundle.manifest.Version, DataDigest: dataDigest(data)}, nil
}

// Install writes the conversation into an XDG data directory without an
// OpenCode database and points it at the new working directory, because
// OpenCode resumes in the session's recorded directory rather than the cwd it
// is given. Every file is created new and a failed install removes those it
// created, so journalDir is unused.
func (openCodeFormat) Install(ctx context.Context, data []byte, dirs Dirs, _ string, maxBytes int) (err error) {
	limit, err := normalizeLimit(maxBytes)
	if err != nil {
		return err
	}
	bundle, err := decodeOpenCodeBundle(data, limit)
	if err != nil {
		return err
	}
	for _, dir := range []string{dirs.State, dirs.Workspace} {
		if err := requireRealPath(dir, true); err != nil {
			return err
		}
	}
	root := openCodeRoot(dirs)
	// A failed install removes what it created, so a developer can retry it
	// into the same directory.
	var created []string
	defer func() {
		if err != nil {
			for _, path := range slices.Backward(created) {
				_ = os.Remove(path)
			}
		}
	}()
	if err = os.Mkdir(root, 0o700); err == nil {
		created = append(created, root)
	} else if !errors.Is(err, os.ErrExist) {
		return err
	}
	if err = requireRealPath(root, true); err != nil {
		return err
	}
	destination := filepath.Join(root, openCodeDatabaseEntry)
	if _, statErr := os.Lstat(destination); !errors.Is(statErr, os.ErrNotExist) {
		return errors.New("OpenCode state directory already holds a database")
	}
	toolOutputDir := filepath.Join(root, openCodeToolOutputDir)
	if len(bundle.toolOutput) > 0 {
		if err = os.Mkdir(toolOutputDir, 0o700); err != nil {
			return err
		}
		created = append(created, toolOutputDir)
		for name, content := range bundle.toolOutput {
			path := filepath.Join(toolOutputDir, name)
			if err = writeNewFile(path, content); err != nil {
				return err
			}
			created = append(created, path)
		}
	}
	staged := filepath.Join(root, ".opencode.db.restore")
	if err = writeNewFile(staged, bundle.database); err != nil {
		return err
	}
	created = append(created, staged)
	if err = verifyOpenCodeDatabase(ctx, staged, bundle.manifest); err != nil {
		return err
	}
	// Relocation may grow the conversation only into the budget the carried
	// state leaves.
	carried := len(bundle.database)
	for _, content := range bundle.toolOutput {
		carried += len(content)
	}
	if err = relocateOpenCodeSession(ctx, staged, bundle.manifest, dirs.Workspace, toolOutputDir, expandedLimit(limit)-carried); err != nil {
		return err
	}
	if err = os.Rename(staged, destination); err != nil {
		return err
	}
	// Until the directories are synced, a failure still removes the published
	// database, so a retry finds no partial install.
	created[len(created)-1] = destination
	synced := []string{root, dirs.State}
	if len(bundle.toolOutput) > 0 {
		synced = append([]string{toolOutputDir}, synced...)
	}
	if err = syncDirs(synced...); err != nil {
		return err
	}
	created = nil
	return nil
}

// copyOpenCodeDatabase takes a consistent copy of a stopped OpenCode
// database. Committed rows still in the write-ahead log are included; a plain
// file copy would lose them.
func copyOpenCodeDatabase(ctx context.Context, source, target string) error {
	db, err := openSQLite(source, "mode=ro")
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	// A source Orka cannot read or verify will not become readable on retry,
	// so it falls back to the transcript. A structurally sound database holds
	// no page twice, so its copy is no larger than the bounded source; the
	// check reads each page once.
	if _, err := requireOpenCodeSchema(ctx, db); err != nil {
		if errors.Is(err, ErrUnsupported) {
			return err
		}
		return fmt.Errorf("%w: OpenCode database is unreadable or corrupt: %w", ErrUnsupported, err)
	}
	if err := requireIntactSQLite(ctx, db); err != nil {
		return fmt.Errorf("%w: OpenCode database is unreadable or corrupt: %w", ErrUnsupported, err)
	}
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, target); err != nil {
		return fmt.Errorf("copy OpenCode database: %w", err)
	}
	return nil
}

// pruneOpenCodeDatabase leaves only providerSessionID's conversation in a
// private copy of the database, and returns the session's recorded directory,
// which must be workspace unless that is empty, and the names of the spilled
// tool outputs in toolOutputDir that it refers to. A developer's database
// holds every session they ran in every project; a RuntimeSession's holds one.
func pruneOpenCodeDatabase(ctx context.Context, path, providerSessionID, workspace, toolOutputDir string) (string, []string, error) {
	db, err := openSQLite(path, "")
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = db.Close() }()
	schema, err := requireOpenCodeSchema(ctx, db)
	if err != nil {
		return "", nil, err
	}
	if _, err := db.ExecContext(ctx, `PRAGMA journal_mode=DELETE`); err != nil {
		return "", nil, err
	}
	var projectID string
	err = db.QueryRowContext(ctx, `SELECT project_id FROM session WHERE id = ?`, providerSessionID).Scan(&projectID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, errors.New("OpenCode database does not hold the session")
	}
	if err != nil {
		return "", nil, err
	}
	if err := deleteOtherOpenCodeState(ctx, db, schema, providerSessionID, projectID); err != nil {
		return "", nil, err
	}
	for _, table := range schema.tables {
		if slices.Contains(openCodeCarriedTables, table) || slices.Contains(openCodeDroppedTables, table) {
			continue
		}
		populated, err := sqliteExists(ctx, db, `SELECT EXISTS(SELECT 1 FROM "`+table+`")`)
		if err != nil {
			return "", nil, err
		}
		if populated {
			return "", nil, fmt.Errorf("%w: OpenCode table %s holds state Orka does not carry", ErrUnsupported, table)
		}
	}
	session, err := readOpenCodeSession(ctx, db, providerSessionID)
	if err != nil {
		return "", nil, err
	}
	switch {
	case workspace != "" && session.directory != workspace:
		return "", nil, fmt.Errorf("%w: OpenCode session runs outside the given workspace", ErrUnsupported)
	case !absoluteCleanPath(session.directory):
		return "", nil, fmt.Errorf("%w: OpenCode session's recorded directory is not an absolute path", ErrUnsupported)
	}
	spilled, err := openCodeToolOutputReferences(ctx, db, toolOutputDir)
	if err != nil {
		return "", nil, err
	}
	if _, err := db.ExecContext(ctx, `VACUUM`); err != nil {
		return "", nil, err
	}
	return session.directory, spilled, nil
}

// deleteOtherOpenCodeState deletes the rows of every other session and
// project, and every dropped table's rows. The session's descendants stay, so
// readOpenCodeSession can refuse them.
func deleteOtherOpenCodeState(ctx context.Context, db *sql.DB, schema openCodeSchema, providerSessionID, projectID string) error {
	exec := func(query string, args ...any) error {
		_, err := db.ExecContext(ctx, query, args...)
		return err
	}
	if err := exec(`DELETE FROM session WHERE id NOT IN (WITH RECURSIVE kept(id) AS
 (SELECT ? UNION SELECT session.id FROM session JOIN kept ON session.parent_id = kept.id) SELECT id FROM kept)`, providerSessionID); err != nil {
		return err
	}
	// Project rows go first, since session is one of them.
	for _, table := range schema.projectTables {
		if err := exec(`DELETE FROM "`+table+`" WHERE project_id != ?`, projectID); err != nil {
			return err
		}
	}
	for _, table := range schema.sessionTables {
		if err := exec(`DELETE FROM "` + table + `" WHERE session_id NOT IN (SELECT id FROM session)`); err != nil {
			return err
		}
	}
	if err := exec(`DELETE FROM project WHERE id != ?`, projectID); err != nil {
		return err
	}
	for _, table := range openCodeDroppedTables {
		if err := exec(`DELETE FROM "` + table + `"`); err != nil {
			return err
		}
	}
	return nil
}

// openCodeToolOutputReferences returns the names of the files in dir that the
// session's own tool calls spilled their output to. OpenCode records each in
// its tool part's metadata.outputPath, its only link from a spill to a session.
// A developer's tool-output directory holds every session's spills, so a path
// that only appears in conversation text never selects a file. Whatever can
// write the database runs as the developer or as the session's agent, and can
// already read any file in dir into the conversation, so a forged reference
// discloses nothing more.
func openCodeToolOutputReferences(ctx context.Context, db *sql.DB, dir string) ([]string, error) {
	prefix := dir + string(filepath.Separator)
	rows, err := db.QueryContext(ctx, `SELECT data FROM part WHERE instr(data, ?) > 0`, prefix)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	names := map[string]bool{}
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var part struct {
			Type  string `json:"type"`
			State struct {
				Metadata struct {
					OutputPath string `json:"outputPath"`
				} `json:"metadata"`
			} `json:"state"`
		}
		if json.Unmarshal([]byte(data), &part) != nil || part.Type != "tool" {
			continue
		}
		name, ok := strings.CutPrefix(part.State.Metadata.OutputPath, prefix)
		if !ok || !spilledFilePattern.MatchString(name) {
			continue
		}
		names[name] = true
		if len(names) > maxOpenCodeToolOutputs {
			return nil, fmt.Errorf("%w: OpenCode conversation spilled more than %d tool outputs", ErrUnsupported, maxOpenCodeToolOutputs)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	slices.Sort(sorted)
	return sorted, nil
}

// verifyOpenCodeDatabase checks that path is an intact OpenCode database
// holding exactly the manifest's conversation and nothing else.
func verifyOpenCodeDatabase(ctx context.Context, path string, manifest openCodeManifest) error {
	db, err := openSQLite(path, "mode=ro&immutable=1")
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	schema, err := requireOpenCodeSchema(ctx, db)
	if errors.Is(err, ErrUnsupported) {
		// Capture checked the same schema, so this bundle is invalid.
		return errors.New("OpenCode database schema differs from the captured version")
	}
	if err != nil {
		return err
	}
	if err := requireIntactSQLite(ctx, db); err != nil {
		return err
	}
	for _, table := range schema.tables {
		if slices.Contains(openCodeCarriedTables, table) {
			continue
		}
		populated, err := sqliteExists(ctx, db, `SELECT EXISTS(SELECT 1 FROM "`+table+`")`)
		if err != nil {
			return err
		}
		if populated {
			return fmt.Errorf("OpenCode database carries rows in %s", table)
		}
	}
	session, err := readOpenCodeSession(ctx, db, manifest.SessionID)
	if errors.Is(err, ErrUnsupported) {
		// Capture never produces this, so the bundle is invalid, not merely unsupported.
		return errors.New("OpenCode database carries state outside the captured session")
	}
	if err != nil {
		return err
	}
	if session.directory != manifest.Workspace {
		return errors.New("OpenCode session directory differs from the captured workspace")
	}
	foreign, err := sqliteExists(ctx, db, `SELECT EXISTS(SELECT 1 FROM project WHERE id != ?)`, session.projectID)
	if err != nil || foreign {
		return errors.New("OpenCode database carries another project")
	}
	return nil
}

type openCodeSession struct {
	projectID, directory, path string
}

// readOpenCodeSession requires the database to hold exactly one top-level
// session, providerSessionID, whose messages and parts are its own.
func readOpenCodeSession(ctx context.Context, db *sql.DB, providerSessionID string) (openCodeSession, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, project_id, directory, COALESCE(path, ''), parent_id IS NULL AND workspace_id IS NULL,
 COALESCE(permission, 'null') IN ('null', '[]') FROM session`)
	if err != nil {
		return openCodeSession{}, err
	}
	defer func() { _ = rows.Close() }()
	var session openCodeSession
	found, count, restricted := false, 0, false
	for rows.Next() {
		var current openCodeSession
		var id string
		var topLevel, unrestricted bool
		if err := rows.Scan(&id, &current.projectID, &current.directory, &current.path, &topLevel, &unrestricted); err != nil {
			return openCodeSession{}, err
		}
		count++
		if id == providerSessionID {
			found, session, restricted = true, current, !topLevel || !unrestricted
		}
	}
	if err := rows.Err(); err != nil {
		return openCodeSession{}, err
	}
	switch {
	case !found:
		return openCodeSession{}, errors.New("OpenCode database does not hold the session")
	case count != 1:
		return openCodeSession{}, fmt.Errorf("%w: OpenCode database holds more than the captured session", ErrUnsupported)
	case restricted:
		return openCodeSession{}, fmt.Errorf("%w: OpenCode session carries child, workspace, or permission state", ErrUnsupported)
	}
	for _, query := range []string{
		`SELECT EXISTS(SELECT 1 FROM message WHERE session_id != ?)`,
		`SELECT EXISTS(SELECT 1 FROM part WHERE session_id != ? OR message_id NOT IN (SELECT id FROM message))`,
	} {
		foreign, err := sqliteExists(ctx, db, query, providerSessionID)
		if err != nil {
			return openCodeSession{}, err
		}
		if foreign {
			return openCodeSession{}, errors.New("OpenCode database holds messages of another session")
		}
	}
	return session, nil
}

// relocateOpenCodeSession points the restored session at workspace. OpenCode
// runs tools in session.directory and ignores the cwd passed to
// session/resume, and the previous turn's directories no longer exist.
// Every string it replaces is a path Orka chose, and the growth the
// replacements add stays within budget.
func relocateOpenCodeSession(ctx context.Context, path string, manifest openCodeManifest, workspace, toolOutputDir string, budget int) error {
	db, err := openSQLite(path, "")
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var projectID, worktree string
	var sessionPath sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT s.project_id, s.path, p.worktree
 FROM session s JOIN project p ON p.id = s.project_id WHERE s.id = ?`, manifest.SessionID).
		Scan(&projectID, &sessionPath, &worktree); err != nil {
		return fmt.Errorf("read OpenCode session location: %w", err)
	}
	// verifyOpenCodeDatabase proved session.directory is the captured workspace.
	directory := manifest.Workspace
	// The global project's worktree is "/", and OpenCode titles tools with
	// the worktree-relative path, which is then the workspace without its
	// leading slash. Other worktree-relative forms are left alone.
	previousPath := ""
	if worktree == "/" && sessionPath.Valid && sessionPath.String == strings.TrimPrefix(directory, "/") {
		previousPath = sessionPath.String
	}
	// A git project's worktree is the workspace itself; the global project's
	// is the filesystem root and stays.
	newWorktree := worktree
	if worktree == directory {
		newWorktree = workspace
	}
	if sessionPath.Valid {
		relative, err := filepath.Rel(newWorktree, workspace)
		if err != nil {
			return fmt.Errorf("relocate OpenCode session path: %w", err)
		}
		// OpenCode records path.relative(worktree, directory), which is empty
		// rather than "." for the worktree itself.
		if relative == "." {
			relative = ""
		}
		sessionPath.String = filepath.ToSlash(relative)
	}
	statements := []struct {
		query string
		args  []any
	}{
		{`UPDATE project SET worktree = ? WHERE id = ?`, []any{newWorktree, projectID}},
		{`UPDATE session SET directory = ?, path = ? WHERE id = ?`, []any{workspace, sessionPath, manifest.SessionID}},
	}
	// Absolute paths move first, so the worktree-relative form OpenCode uses
	// in tool titles only matches where no absolute path remains.
	growth := int64(0)
	for _, move := range [][2]string{{directory, workspace}, {manifest.ToolOutputDir, toolOutputDir}, {previousPath, sessionPath.String}} {
		if move[0] == "" || move[0] == move[1] {
			continue
		}
		for _, table := range []string{openCodeMessageTable, openCodePartTable} {
			if len(move[1]) > len(move[0]) {
				var occurrences int64
				if err := tx.QueryRowContext(ctx, `SELECT COALESCE(sum((length(CAST(data AS BLOB)) - length(CAST(replace(data, ?, '') AS BLOB))) / ?), 0)
 FROM `+table+` WHERE instr(data, ?) > 0`, move[0], len(move[0]), move[0]).Scan(&occurrences); err != nil {
					return err
				}
				growth += occurrences * int64(len(move[1])-len(move[0]))
			}
			statements = append(statements, struct {
				query string
				args  []any
			}{`UPDATE ` + table + ` SET data = replace(data, ?, ?) WHERE instr(data, ?) > 0`, []any{move[0], move[1], move[0]}})
		}
	}
	if growth > int64(budget) {
		return errors.New("relocating the OpenCode session would exceed its expanded budget")
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement.query, statement.args...); err != nil {
			return fmt.Errorf("relocate OpenCode session: %w", err)
		}
	}
	return tx.Commit()
}

func requireIntactSQLite(ctx context.Context, db *sql.DB) error {
	var check string
	if err := db.QueryRowContext(ctx, `PRAGMA quick_check(1)`).Scan(&check); err != nil || check != "ok" {
		return errors.New("OpenCode database failed its integrity check")
	}
	return nil
}

func sqliteExists(ctx context.Context, db *sql.DB, query string, args ...any) (bool, error) {
	var exists bool
	err := db.QueryRowContext(ctx, query, args...).Scan(&exists)
	return exists, err
}

// openSQLite opens path without trusting functions its schema names.
func openSQLite(path, query string) (*sql.DB, error) {
	if strings.ContainsAny(path, "?#%") {
		return nil, errors.New("SQLite path contains URI metacharacters")
	}
	parameters := "_pragma=trusted_schema(0)&_pragma=busy_timeout(250)"
	if query != "" {
		parameters = query + "&" + parameters
	}
	db, err := sql.Open("sqlite", "file:"+path+"?"+parameters)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// readOpenCodeToolOutput reads those of the named spilled tool outputs that
// exist.
func readOpenCodeToolOutput(dir string, names []string, limit int) (map[string][]byte, error) {
	if len(names) == 0 {
		return nil, nil
	}
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err := requireRealPath(dir, true); err != nil {
		return nil, err
	}
	files := make(map[string][]byte, len(names))
	total := 0
	for _, name := range names {
		path := filepath.Join(dir, name)
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := requireRealPath(path, false); err != nil {
			return nil, err
		}
		content, err := readBoundedFile(path, expandedLimit(limit)-total)
		if err != nil {
			return nil, err
		}
		total += len(content)
		files[name] = content
	}
	return files, nil
}

// encodeOpenCodeBundle fails with ErrUnsupported when the archive exceeds
// the expanded budget decodeOpenCodeBundle enforces.
func encodeOpenCodeBundle(limit int, bundle openCodeBundle) ([]byte, error) {
	files := make([]bundleFile, 0, 1+len(bundle.toolOutput))
	files = append(files, bundleFile{openCodeDatabaseEntry, bundle.database})
	for _, name := range slices.Sorted(maps.Keys(bundle.toolOutput)) {
		files = append(files, bundleFile{openCodeToolOutputEntry + name, bundle.toolOutput[name]})
	}
	return writeBundle(limit, bundle.manifest, files)
}

// decodeOpenCodeBundle reads a bundle holding a manifest, the database, and
// at most maxOpenCodeToolOutputs spilled tool outputs.
func decodeOpenCodeBundle(data []byte, maxBytes int) (openCodeBundle, error) {
	toolOutputs := 0
	manifest, files, err := readBundle(data, maxBytes, func(name string) bool {
		if name == openCodeDatabaseEntry {
			return true
		}
		toolOutput, ok := strings.CutPrefix(name, openCodeToolOutputEntry)
		toolOutputs++
		return ok && spilledFilePattern.MatchString(toolOutput) && toolOutputs <= maxOpenCodeToolOutputs
	})
	if err != nil {
		return openCodeBundle{}, err
	}
	bundle := openCodeBundle{database: files[openCodeDatabaseEntry], toolOutput: map[string][]byte{}}
	if len(bundle.database) == 0 {
		return openCodeBundle{}, errors.New("OpenCode bundle is incomplete")
	}
	for name, content := range files {
		if toolOutput, ok := strings.CutPrefix(name, openCodeToolOutputEntry); ok {
			bundle.toolOutput[toolOutput] = content
		}
	}
	if bundle.manifest, err = parseOpenCodeManifest(manifest); err != nil {
		return openCodeBundle{}, err
	}
	return bundle, nil
}

func parseOpenCodeManifest(content []byte) (openCodeManifest, error) {
	var manifest openCodeManifest
	if err := decodeManifest(content, &manifest); err != nil {
		return openCodeManifest{}, err
	}
	if manifest.Format != openCodeBundleFormat || !openCodeSessionIDPattern.MatchString(manifest.SessionID) ||
		!absoluteCleanPath(manifest.Workspace) || !absoluteCleanPath(manifest.ToolOutputDir) {
		return openCodeManifest{}, errors.New("OpenCode bundle manifest is invalid")
	}
	if manifest.Version != openCodeVersion {
		return openCodeManifest{}, fmt.Errorf("%w: OpenCode %s conversations are not supported", ErrUnsupported, manifest.Version)
	}
	return manifest, nil
}
