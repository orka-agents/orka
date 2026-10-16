package nativesession

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/acp"
)

const (
	openCodeFixtureSession = "ses_ecc5ff852ffer0NIBWVcS4TRJl"
	openCodeFixtureLimit   = 1 << 20
	openCodeFixtureSpill   = "tool_e5f1b2c3d4"
)

// openCodeFixture is a stopped OpenCode session home.
type openCodeFixture struct {
	dirs  Dirs
	db    *sql.DB
	spill []byte
}

// newOpenCodeFixture builds a session home whose conversation rows exist only
// in the write-ahead log, as they do after OpenCode is killed.
func newOpenCodeFixture(t *testing.T) openCodeFixture {
	t.Helper()
	root := realDir(t)
	dirs := Dirs{State: filepath.Join(root, "xdg", "data"), Workspace: filepath.Join(root, "workspace")}
	for _, dir := range []string{filepath.Join(dirs.State, "opencode", "tool-output"), dirs.Workspace} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	spill := []byte(strings.Repeat("spilled tool output\n", 64))
	spillPath := filepath.Join(dirs.State, "opencode", "tool-output", openCodeFixtureSpill)
	if err := os.WriteFile(spillPath, spill, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dirs.State, "opencode", "opencode.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	for _, statement := range []string{`PRAGMA journal_mode=WAL`, `PRAGMA wal_autocheckpoint=0`, openCodeSchemaSQL, `PRAGMA wal_checkpoint(TRUNCATE)`} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	path := strings.TrimPrefix(dirs.Workspace, "/")
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO project (id, worktree, time_created, time_updated, sandboxes) VALUES ('global', '/', 1, 1, '[]')`, nil},
		{`INSERT INTO session (id, project_id, slug, directory, path, title, version, agent, time_created, time_updated)
 VALUES (?, 'global', 'crisp-panda', ?, ?, 'Read LICENSE', '1.19.0', 'build', 1, 1)`, []any{openCodeFixtureSession, dirs.Workspace, path}},
		{`INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES ('msg_user', ?, 1, 1, ?)`,
			[]any{openCodeFixtureSession, `{"role":"user","text":"Remember nonce-fixture"}`}},
		{`INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES ('msg_assistant', ?, 2, 2, ?)`,
			[]any{openCodeFixtureSession, `{"role":"assistant","path":{"cwd":"` + dirs.Workspace + `","root":"/"}}`}},
		{`INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES ('prt_tool', 'msg_assistant', ?, 2, 2, ?)`,
			[]any{openCodeFixtureSession, `{"type":"tool","state":{"input":{"filePath":"` + dirs.Workspace + `/LICENSE"},"metadata":{"outputPath":"` + spillPath + `"},"output":"Full output saved to: ` + spillPath + `","title":"` + path + `/LICENSE"}}`}},
		{`INSERT INTO event_sequence (aggregate_id, seq) VALUES (?, 1)`, []any{openCodeFixtureSession}},
		{`INSERT INTO event (id, aggregate_id, seq, type, data) VALUES ('evt_1', ?, 1, 'session.updated', '{}')`, []any{openCodeFixtureSession}},
		{`INSERT INTO permission (id, project_id, action, resource, time_created, time_updated) VALUES ('per_fixture_grant', 'global', 'allow', 'bash', 1, 1)`, nil},
	} {
		if _, err := db.ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	return openCodeFixture{dirs: dirs, db: db, spill: spill}
}

func (f openCodeFixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := f.db.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatal(err)
	}
}

func freshOpenCodeDirs(t *testing.T) Dirs {
	t.Helper()
	root := realDir(t)
	dirs := Dirs{State: filepath.Join(root, "xdg", "data"), Workspace: filepath.Join(root, "workspace")}
	for _, dir := range []string{dirs.State, dirs.Workspace} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return dirs
}

func TestOpenCodeVersionMatchesPinnedRuntime(t *testing.T) {
	if openCodeVersion != acp.OpenCodeVersion {
		t.Fatalf("native OpenCode format %s does not match the pinned OpenCode %s", openCodeVersion, acp.OpenCodeVersion)
	}
}

func TestOpenCodeFormatCarriesWALConversationIntoANewWorkspace(t *testing.T) {
	format, ok := For("opencode")
	if !ok {
		t.Fatal("opencode must save and restore native sessions")
	}
	source := newOpenCodeFixture(t)
	data, err := format.Capture(t.Context(), source.dirs, openCodeFixtureSession, openCodeFixtureLimit)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"per_fixture_grant", "session.updated"} {
		if bytes.Contains(gunzipForTest(t, data), []byte(private)) {
			t.Fatalf("capture carried %q", private)
		}
	}
	summary, err := format.Inspect(t.Context(), data, openCodeFixtureLimit)
	if err != nil {
		t.Fatal(err)
	}
	if summary.ProviderSessionID != openCodeFixtureSession || summary.ProviderVersion != "1.19.0" || summary.DataDigest != dataDigest(data) {
		t.Fatalf("unexpected summary: %+v", summary)
	}

	destination := freshOpenCodeDirs(t)
	if err := format.Install(t.Context(), data, destination, realDir(t), openCodeFixtureLimit); err != nil {
		t.Fatal(err)
	}
	restored, err := sql.Open("sqlite", "file:"+filepath.Join(destination.State, "opencode", "opencode.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restored.Close() }()
	var directory, path string
	if err := restored.QueryRow(`SELECT directory, path FROM session WHERE id = ?`, openCodeFixtureSession).Scan(&directory, &path); err != nil {
		t.Fatal(err)
	}
	if directory != destination.Workspace || path != strings.TrimPrefix(destination.Workspace, "/") {
		t.Fatalf("session not moved to the new workspace: directory=%q path=%q", directory, path)
	}
	newSpill := filepath.Join(destination.State, "opencode", "tool-output", openCodeFixtureSpill)
	var assistant, tool string
	if err := restored.QueryRow(`SELECT data FROM message WHERE id = 'msg_assistant'`).Scan(&assistant); err != nil {
		t.Fatal(err)
	}
	if err := restored.QueryRow(`SELECT data FROM part WHERE id = 'prt_tool'`).Scan(&tool); err != nil {
		t.Fatal(err)
	}
	previousRelative := strings.TrimPrefix(source.dirs.Workspace, "/")
	if strings.Contains(assistant+tool, previousRelative) || strings.Contains(tool, source.dirs.State) ||
		!strings.Contains(tool, destination.Workspace+"/LICENSE") || strings.Count(tool, newSpill) != 2 ||
		!strings.Contains(tool, `"title":"`+strings.TrimPrefix(destination.Workspace, "/")+`/LICENSE"`) {
		t.Fatalf("conversation still names the previous directories: %s %s", assistant, tool)
	}
	var messages, parts, events, permissions int
	if err := restored.QueryRow(`SELECT (SELECT count(*) FROM message), (SELECT count(*) FROM part), (SELECT count(*) FROM event), (SELECT count(*) FROM permission)`).
		Scan(&messages, &parts, &events, &permissions); err != nil {
		t.Fatal(err)
	}
	if messages != 2 || parts != 1 || events != 0 || permissions != 0 {
		t.Fatalf("messages=%d parts=%d events=%d permissions=%d", messages, parts, events, permissions)
	}
	spill, err := os.ReadFile(newSpill)
	if err != nil || !bytes.Equal(spill, source.spill) {
		t.Fatalf("spilled tool output was not carried: %v", err)
	}
	if err := format.Install(t.Context(), data, destination, realDir(t), openCodeFixtureLimit); err == nil {
		t.Fatal("install must not overwrite an existing OpenCode database")
	}
}

// A developer's database holds every session they ran in every project, and
// their tool-output directory every session's spills.
func TestOpenCodeCaptureKeepsOnlyTheSessionFromADeveloperDatabase(t *testing.T) {
	format, _ := For("opencode")
	source := newOpenCodeFixture(t)
	toolOutput := filepath.Join(source.dirs.State, "opencode", "tool-output")
	otherSpill := filepath.Join(toolOutput, "tool_other")
	for _, name := range []string{otherSpill, filepath.Join(toolOutput, ".DS_Store")} {
		if err := os.WriteFile(name, []byte("another session's output"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Only a tool part's own metadata selects a spill: a path the conversation
	// merely mentions does not, and a spill OpenCode deleted is skipped.
	source.exec(t, `UPDATE message SET data = ? WHERE id = 'msg_user'`, `{"role":"user","text":"Compare `+otherSpill+`"}`)
	source.exec(t, `INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES ('prt_gone', 'msg_assistant', ?, 3, 3, ?)`,
		openCodeFixtureSession, `{"type":"tool","state":{"metadata":{"truncated":true,"outputPath":"`+filepath.Join(toolOutput, "tool_gone")+`"}}}`)
	source.exec(t, `INSERT INTO project (id, worktree, vcs, time_created, time_updated, sandboxes) VALUES ('a1b2c3', '/src/other', 'git', 1, 1, '[]')`)
	for _, session := range [][]any{{"ses_sibling", "global", nil}, {"ses_siblingchild", "global", "ses_sibling"}, {"ses_otherproject", "a1b2c3", nil}} {
		source.exec(t, `INSERT INTO session (id, project_id, parent_id, slug, directory, title, version, time_created, time_updated)
 VALUES (?, ?, ?, 'other', '/src/other', 'other', '1.19.0', 1, 1)`, session...)
		source.exec(t, `INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES (?, ?, 1, 1, ?)`,
			"msg_"+session[0].(string), session[0], `{"role":"user","text":"other-private"}`)
		source.exec(t, `INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, 1, 1, ?)`,
			"prt_"+session[0].(string), "msg_"+session[0].(string), session[0], `{"type":"tool","state":{"metadata":{"truncated":true,"outputPath":"`+otherSpill+`"}}}`)
		source.exec(t, `INSERT INTO todo (session_id, content, status, priority, position, time_created, time_updated) VALUES (?, 'other-private', 'pending', 'high', 0, 1, 1)`, session[0])
	}
	source.exec(t, `INSERT INTO workspace (id, type, project_id, time_used) VALUES ('wrk_other', 'worktree', 'a1b2c3', 1)`)
	source.exec(t, `INSERT INTO permission (id, project_id, action, resource, time_created, time_updated) VALUES ('per_other', 'a1b2c3', 'allow', 'bash', 1, 1)`)

	data, err := format.Capture(t.Context(), source.dirs, openCodeFixtureSession, openCodeFixtureLimit)
	if err != nil {
		t.Fatal(err)
	}
	entries := tarEntriesForTest(t, data)
	if len(entries) != 3 || entries["tool-output/"+openCodeFixtureSpill] == nil {
		t.Fatalf("capture must carry only the session's spilled output: %v", slices.Collect(maps.Keys(entries)))
	}
	database := filepath.Join(realDir(t), "captured.db")
	if err := os.WriteFile(database, entries["opencode.db"], 0o600); err != nil {
		t.Fatal(err)
	}
	captured, err := sql.Open("sqlite", "file:"+database+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = captured.Close() }()
	var sessions, projects, messages, parts, rest int
	if err := captured.QueryRow(`SELECT (SELECT count(*) FROM session), (SELECT count(*) FROM project), (SELECT count(*) FROM message),
 (SELECT count(*) FROM part), (SELECT count(*) FROM todo) + (SELECT count(*) FROM workspace) + (SELECT count(*) FROM permission)`).
		Scan(&sessions, &projects, &messages, &parts, &rest); err != nil {
		t.Fatal(err)
	}
	if sessions != 1 || projects != 1 || messages != 2 || parts != 2 || rest != 0 {
		t.Fatalf("sessions=%d projects=%d messages=%d parts=%d other rows=%d", sessions, projects, messages, parts, rest)
	}
	if bytes.Contains(gunzipForTest(t, data), []byte("other-private")) {
		t.Fatal("capture carried another session's conversation")
	}
	if _, err := format.Inspect(t.Context(), data, openCodeFixtureLimit); err != nil {
		t.Fatal(err)
	}

	// Without a workspace, capture takes the directory the session recorded.
	data, err = format.Capture(t.Context(), Dirs{State: source.dirs.State}, openCodeFixtureSession, openCodeFixtureLimit)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := decodeOpenCodeBundle(data, openCodeFixtureLimit)
	if err != nil || bundle.manifest.Workspace != source.dirs.Workspace {
		t.Fatalf("capture without a workspace must record the session's directory: %+v %v", bundle.manifest, err)
	}
}

// `opencode run` denies its interactive tools on every session it creates.
// Deny rules only narrow the runtime's own policy, so they carry over.
func TestOpenCodeFormatCarriesDenyOnlySessionPermission(t *testing.T) {
	const runRules = `[{"permission":"question","pattern":"*","action":"deny"},{"permission":"plan_enter","pattern":"*","action":"deny"},{"permission":"plan_exit","pattern":"*","action":"deny"}]`
	format, _ := For("opencode")
	source := newOpenCodeFixture(t)
	source.exec(t, `UPDATE session SET permission = ?`, runRules)
	data, err := format.Capture(t.Context(), source.dirs, openCodeFixtureSession, openCodeFixtureLimit)
	if err != nil {
		t.Fatal(err)
	}
	destination := freshOpenCodeDirs(t)
	if err := format.Install(t.Context(), data, destination, realDir(t), openCodeFixtureLimit); err != nil {
		t.Fatal(err)
	}
	restored, err := sql.Open("sqlite", "file:"+filepath.Join(destination.State, "opencode", "opencode.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restored.Close() }()
	var permission string
	if err := restored.QueryRow(`SELECT permission FROM session WHERE id = ?`, openCodeFixtureSession).Scan(&permission); err != nil {
		t.Fatal(err)
	}
	if permission != runRules {
		t.Fatalf("restored session permission = %s, want %s", permission, runRules)
	}
}

func TestOpenCodeInstallMovesAGitProjectWorktree(t *testing.T) {
	format, _ := For("opencode")
	source := newOpenCodeFixture(t)
	// A git workspace is its own project worktree, so OpenCode records an
	// empty relative path.
	source.exec(t, `INSERT INTO project (id, worktree, vcs, time_created, time_updated, sandboxes) VALUES ('3f1c0d', ?, 'git', 1, 1, '[]')`, source.dirs.Workspace)
	source.exec(t, `UPDATE session SET project_id = '3f1c0d', path = ''`)
	data, err := format.Capture(t.Context(), source.dirs, openCodeFixtureSession, openCodeFixtureLimit)
	if err != nil {
		t.Fatal(err)
	}
	destination := freshOpenCodeDirs(t)
	if err := format.Install(t.Context(), data, destination, realDir(t), openCodeFixtureLimit); err != nil {
		t.Fatal(err)
	}
	restored, err := sql.Open("sqlite", "file:"+filepath.Join(destination.State, "opencode", "opencode.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restored.Close() }()
	var worktree, directory string
	var path sql.NullString
	var projects int
	if err := restored.QueryRow(`SELECT p.worktree, s.directory, s.path, (SELECT count(*) FROM project) FROM session s JOIN project p ON p.id = s.project_id`).
		Scan(&worktree, &directory, &path, &projects); err != nil {
		t.Fatal(err)
	}
	if worktree != destination.Workspace || directory != destination.Workspace || !path.Valid || path.String != "" || projects != 1 {
		t.Fatalf("worktree=%q directory=%q path=%+v projects=%d", worktree, directory, path, projects)
	}
}

func TestOpenCodeCaptureFallsBackOnStateItDoesNotCarry(t *testing.T) {
	format, _ := For("opencode")
	for name, test := range map[string]struct {
		mutate func(*testing.T, openCodeFixture)
		reason string
	}{
		"todo list": {reason: "table todo", mutate: func(t *testing.T, f openCodeFixture) {
			f.exec(t, `INSERT INTO todo (session_id, content, status, priority, position, time_created, time_updated) VALUES (?, 'x', 'pending', 'high', 0, 1, 1)`, openCodeFixtureSession)
		}},
		"child session": {reason: "more than the captured session", mutate: func(t *testing.T, f openCodeFixture) {
			f.exec(t, `INSERT INTO session (id, project_id, parent_id, slug, directory, title, version, time_created, time_updated)
 VALUES ('ses_child', 'global', ?, 'child', '/', 'child', '1.19.0', 1, 1)`, openCodeFixtureSession)
		}},
		"session permission": {reason: "permission state", mutate: func(t *testing.T, f openCodeFixture) {
			f.exec(t, `UPDATE session SET permission = '[{"permission":"bash","action":"allow"}]'`)
		}},
		"session ask rule": {reason: "permission state", mutate: func(t *testing.T, f openCodeFixture) {
			f.exec(t, `UPDATE session SET permission = ?`, `[{"permission":"bash","pattern":"*","action":"ask"}]`)
		}},
		"session allow after a deny rule": {reason: "permission state", mutate: func(t *testing.T, f openCodeFixture) {
			f.exec(t, `UPDATE session SET permission = ?`, `[{"permission":"bash","pattern":"*","action":"deny"},{"permission":"bash","pattern":"*","action":"allow"}]`)
		}},
		"session deny rule with another field": {reason: "permission state", mutate: func(t *testing.T, f openCodeFixture) {
			f.exec(t, `UPDATE session SET permission = ?`, `[{"permission":"bash","pattern":"*","action":"deny","note":"x"}]`)
		}},
		"session deny rule without a pattern": {reason: "permission state", mutate: func(t *testing.T, f openCodeFixture) {
			f.exec(t, `UPDATE session SET permission = ?`, `[{"permission":"bash","action":"deny"}]`)
		}},
		"session rule with a differently cased action": {reason: "permission state", mutate: func(t *testing.T, f openCodeFixture) {
			f.exec(t, `UPDATE session SET permission = ?`, `[{"permission":"bash","pattern":"*","Action":"deny"}]`)
		}},
		"session rule whose last action allows": {reason: "permission state", mutate: func(t *testing.T, f openCodeFixture) {
			f.exec(t, `UPDATE session SET permission = ?`, `[{"permission":"bash","pattern":"*","action":"deny","action":"allow"}]`)
		}},
		"session null rule": {reason: "permission state", mutate: func(t *testing.T, f openCodeFixture) {
			f.exec(t, `UPDATE session SET permission = ?`, `[null]`)
		}},
		"session rule object": {reason: "permission state", mutate: func(t *testing.T, f openCodeFixture) {
			f.exec(t, `UPDATE session SET permission = ?`, `{"permission":"bash","pattern":"*","action":"deny"}`)
		}},
		"trigger": {reason: "schema differs", mutate: func(t *testing.T, f openCodeFixture) {
			f.exec(t, `CREATE TRIGGER mark AFTER INSERT ON message BEGIN SELECT 1; END`)
		}},
		"expression index": {reason: "schema differs", mutate: func(t *testing.T, f openCodeFixture) {
			f.exec(t, `CREATE INDEX costly ON message(length(hex(zeroblob(1000))))`)
		}},
		"reserved-name trigger": {reason: "schema differs", mutate: func(t *testing.T, f openCodeFixture) {
			f.exec(t, `PRAGMA writable_schema=ON`)
			f.exec(t, `INSERT INTO sqlite_schema (type, name, tbl_name, rootpage, sql)
 VALUES ('trigger', 'sqlite_costly', 'event', 0, 'CREATE TRIGGER sqlite_costly AFTER DELETE ON event BEGIN SELECT 1; END')`)
			f.exec(t, `PRAGMA writable_schema=OFF`)
		}},
		"corrupt pages": {reason: "unreadable or corrupt", mutate: func(t *testing.T, f openCodeFixture) {
			// Garble every page after the schema page that the write-ahead
			// log does not supersede.
			path := filepath.Join(f.dirs.State, "opencode", "opencode.db")
			database, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			pageSize := int(database[16])<<8 | int(database[17])
			for offset := 2 * pageSize; offset+64 <= len(database); offset += pageSize {
				copy(database[offset:offset+64], bytes.Repeat([]byte{0xff}, 64))
			}
			if err := os.WriteFile(path, database, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		"directory outside the workspace": {reason: "outside the given workspace", mutate: func(t *testing.T, f openCodeFixture) {
			f.exec(t, `UPDATE session SET directory = '/'`)
		}},
		"too many spilled outputs": {reason: "spilled more than 256 tool outputs", mutate: func(t *testing.T, f openCodeFixture) {
			for i := range maxOpenCodeToolOutputs {
				spill := filepath.Join(f.dirs.State, "opencode", "tool-output", fmt.Sprintf("tool_extra%03d", i))
				f.exec(t, `INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES (?, 'msg_assistant', ?, 3, 3, ?)`,
					fmt.Sprintf("prt_extra%03d", i), openCodeFixtureSession, `{"type":"tool","state":{"metadata":{"truncated":true,"outputPath":"`+spill+`"}}}`)
			}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newOpenCodeFixture(t)
			test.mutate(t, fixture)
			_, err := format.Capture(t.Context(), fixture.dirs, openCodeFixtureSession, openCodeFixtureLimit)
			if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), test.reason) {
				t.Fatalf("capture must fall back to the transcript because of %q: %v", test.reason, err)
			}
		})
	}
	fixture := newOpenCodeFixture(t)
	if _, err := format.Capture(t.Context(), fixture.dirs, "ses_missing", openCodeFixtureLimit); err == nil || errors.Is(err, ErrUnsupported) {
		t.Fatalf("a missing session is a capture failure, not an unsupported conversation: %v", err)
	}
	if _, err := format.Capture(t.Context(), fixture.dirs, openCodeFixtureSession, 512); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("an oversized conversation must fall back to the transcript: %v", err)
	}
	// Spilled output compresses well, but it shares the expanded budget that
	// Inspect enforces with the database.
	spill := filepath.Join(fixture.dirs.State, "opencode", "tool-output", openCodeFixtureSpill)
	if err := os.WriteFile(spill, bytes.Repeat([]byte("a"), 2<<20-64<<10), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := format.Capture(t.Context(), fixture.dirs, openCodeFixtureSession, 256<<10); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("a conversation over the expanded budget must fall back to the transcript: %v", err)
	}
}

func TestOpenCodeInstallBoundsRelocationGrowth(t *testing.T) {
	format, _ := For("opencode")
	source := newOpenCodeFixture(t)
	// Many references to the old workspace, each of which grows when the
	// new workspace path is much longer.
	source.exec(t, `UPDATE part SET data = ?`, strings.Repeat(source.dirs.Workspace+" ", 6000))
	const limit = 128 << 10
	data, err := format.Capture(t.Context(), source.dirs, openCodeFixtureSession, limit)
	if err != nil {
		t.Fatal(err)
	}
	destination := freshOpenCodeDirs(t)
	destination.Workspace = filepath.Join(destination.Workspace, strings.Repeat("w", 200))
	if err := os.Mkdir(destination.Workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := format.Install(t.Context(), data, destination, realDir(t), limit); err == nil || !strings.Contains(err.Error(), "expanded budget") {
		t.Fatalf("install must refuse to grow the conversation past its expanded budget: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(destination.State, "opencode", "opencode.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a failed install published a database: %v", err)
	}
}

func TestOpenCodeCaptureRejectsSymlinks(t *testing.T) {
	format, _ := For("opencode")
	outside := filepath.Join(realDir(t), "secret")
	if err := os.WriteFile(outside, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, plant := range map[string]func(Dirs) string{
		"database":    func(d Dirs) string { return filepath.Join(d.State, "opencode", "opencode.db") },
		"tool output": func(d Dirs) string { return filepath.Join(d.State, "opencode", "tool-output", openCodeFixtureSpill) },
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newOpenCodeFixture(t)
			target := plant(fixture.dirs)
			_ = os.Remove(target)
			if err := os.Symlink(outside, target); err != nil {
				t.Fatal(err)
			}
			if _, err := format.Capture(t.Context(), fixture.dirs, openCodeFixtureSession, openCodeFixtureLimit); err == nil {
				t.Fatal("capture followed a symlink")
			}
		})
	}
}

func TestOpenCodeInspectRejectsTamperedBundles(t *testing.T) {
	format, _ := For("opencode")
	fixture := newOpenCodeFixture(t)
	data, err := format.Capture(t.Context(), fixture.dirs, openCodeFixtureSession, openCodeFixtureLimit)
	if err != nil {
		t.Fatal(err)
	}
	entries := tarEntriesForTest(t, data)
	rebuild := func(change func(map[string][]byte)) []byte {
		copied := map[string][]byte{}
		maps.Copy(copied, entries)
		change(copied)
		return tarGzipForTest(t, copied)
	}
	if _, err := format.Inspect(t.Context(), rebuild(func(map[string][]byte) {}), openCodeFixtureLimit); err != nil {
		t.Fatalf("a faithful rebuild must inspect: %v", err)
	}
	unsupportedVersion := rebuild(func(e map[string][]byte) {
		e["manifest.json"] = bytes.Replace(e["manifest.json"], []byte(`"1.19.0"`), []byte(`"1.18.9"`), 1)
	})
	if _, err := format.Inspect(t.Context(), unsupportedVersion, openCodeFixtureLimit); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("another OpenCode version must be unsupported: %v", err)
	}
	for name, tampered := range map[string][]byte{
		"unknown entry": rebuild(func(e map[string][]byte) { e["extra"] = []byte("x") }),
		"other session": rebuild(func(e map[string][]byte) {
			e["manifest.json"] = bytes.Replace(e["manifest.json"], []byte(openCodeFixtureSession), []byte("ses_other"), 1)
		}),
		"no database":      rebuild(func(e map[string][]byte) { delete(e, "opencode.db") }),
		"corrupt database": rebuild(func(e map[string][]byte) { e["opencode.db"] = []byte("SQLite format 3\x00 truncated") }),
		"manifest field": rebuild(func(e map[string][]byte) {
			e["manifest.json"] = bytes.Replace(e["manifest.json"], []byte(`{`), []byte(`{"extra":1,`), 1)
		}),
		"trailing data": append(append([]byte(nil), data...), data...),
		"changed schema": rebuild(func(e map[string][]byte) {
			e["opencode.db"] = alterDatabaseForTest(t, e["opencode.db"], `CREATE INDEX costly ON part(length(hex(zeroblob(1000))))`)
		}),
		"not gzip": []byte("plain"),
	} {
		if _, err := format.Inspect(t.Context(), tampered, openCodeFixtureLimit); err == nil || errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: must be rejected as invalid: %v", name, err)
		}
	}
	if _, err := format.Inspect(t.Context(), data, len(data)-1); err == nil {
		t.Fatal("an oversized bundle must be rejected")
	}
}

// alterDatabaseForTest runs statement against a copy of database.
func alterDatabaseForTest(t *testing.T, database []byte, statement string) []byte {
	t.Helper()
	path := filepath.Join(realDir(t), "altered.db")
	if err := os.WriteFile(path, database, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(statement); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	altered, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return altered
}

func gunzipForTest(t *testing.T, data []byte) []byte {
	t.Helper()
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func tarEntriesForTest(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	archive := tar.NewReader(bytes.NewReader(gunzipForTest(t, data)))
	entries := map[string][]byte{}
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			return entries
		}
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(archive)
		if err != nil {
			t.Fatal(err)
		}
		entries[header.Name] = content
	}
}

func tarGzipForTest(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var out bytes.Buffer
	compressed := gzip.NewWriter(&out)
	archive := tar.NewWriter(compressed)
	for name, content := range entries {
		if err := archive.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
