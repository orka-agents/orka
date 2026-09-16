package opencodestate

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/acp"
)

const (
	testSession = "ses_native"
	testCWD     = "/workspace/sessions/owner"
	testModel   = "openai/gpt-5.5"
	testOutput  = "The diagnostic returned unique value EVIDENCE_72419."
)

func TestCaptureRestorePreservesWALToolHistoryAndExcludesConfiguration(t *testing.T) {
	if ProviderVersion != acp.OpenCodeVersion {
		t.Fatal("snapshot schema needs review for the current OpenCode pin")
	}
	source, sourcePath := newDatabaseFixture(t)
	seedConversation(t, source)
	execFixture(t, source, "PRAGMA wal_checkpoint(TRUNCATE)")
	// This value exists only in the WAL until the still-open writer checkpoints.
	execFixture(t, source, "UPDATE part SET data=json_set(data, '$.state.output', ?) WHERE id='prt_tool'", testOutput)
	main, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(main, []byte(testOutput)) {
		t.Fatal("fixture diagnostic was not confined to the WAL")
	}
	const excluded = "PRIVATE_CONFIGURATION_CANARY"
	execFixture(t, source, "INSERT INTO credential VALUES ('credential', NULL, 'label', ?, NULL, NULL, 1, 1, 1)", excluded)
	execFixture(t, source, "INSERT INTO account VALUES ('account', 'example', 'https://example.invalid', ?, ?, 1, 1, 1)", excluded, excluded)
	execFixture(t, source, "UPDATE project SET commands=?, sandboxes=?", excluded, `['ignored']`)
	execFixture(t, source, "UPDATE session SET metadata=?, share_url=?", excluded, excluded)
	execFixture(t, source, "INSERT INTO event_sequence VALUES ('ses_native', 0, NULL)")
	execFixture(t, source, "INSERT INTO event VALUES ('event', 'ses_native', 0, 'session.updated@1', ?)", excluded)
	execFixture(t, source, "INSERT INTO session_share VALUES ('ses_native', 'share', ?, ?, 1, 1)", excluded, excluded)

	data, err := Capture(t.Context(), sourcePath, testSession, testCWD, testModel)
	if err != nil {
		t.Fatal(err)
	}
	plain := decompressFixture(t, data)
	if !bytes.Contains(plain, []byte(testOutput)) || bytes.Contains(plain, []byte(excluded)) {
		t.Fatal("snapshot did not preserve tool history while excluding configuration")
	}
	if len(data) > MaxCompressedBytes || len(plain) > MaxDecompressedBytes {
		t.Fatal("snapshot exceeded its storage bounds")
	}
	target, targetPath := newDatabaseFixture(t)
	if err := Restore(t.Context(), targetPath, data, testSession, testCWD, testModel); err != nil {
		t.Fatal(err)
	}
	var output string
	if err := target.QueryRow("SELECT json_extract(data, '$.state.output') FROM part WHERE id='prt_tool'").Scan(&output); err != nil {
		t.Fatal(err)
	}
	if output != testOutput {
		t.Fatal("restored native tool result changed")
	}
	for _, query := range []string{
		"SELECT COUNT(*) FROM credential", "SELECT COUNT(*) FROM account", "SELECT COUNT(*) FROM event",
		"SELECT COUNT(*) FROM event_sequence", "SELECT COUNT(*) FROM permission", "SELECT COUNT(*) FROM session_share",
		"SELECT COUNT(*) FROM session WHERE permission IS NOT NULL OR metadata IS NOT NULL OR share_url IS NOT NULL",
		"SELECT COUNT(*) FROM project WHERE commands IS NOT NULL OR sandboxes != '[]'",
	} {
		var count int
		if err := target.QueryRow(query).Scan(&count); err != nil || count != 0 {
			t.Fatalf("restore imported unrelated native state: count=%d err=%v", count, err)
		}
	}
	var restoredSession string
	if err := target.QueryRow("SELECT id FROM session").Scan(&restoredSession); err != nil || restoredSession != testSession {
		t.Fatal("restore did not preserve the selected native conversation ID")
	}
	// A newly captured copy must carry exactly the same logical conversation.
	again, err := Capture(t.Context(), targetPath, testSession, testCWD, testModel)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, again) {
		t.Fatal("capture/restore changed the selected native conversation")
	}
}

func TestCaptureRejectsIncompleteOrUnsupportedNativeHistory(t *testing.T) {
	tests := []struct {
		name  string
		query string
	}{
		{"other session", `INSERT INTO session (id,project_id,slug,directory,title,version,time_created,time_updated)
			VALUES ('ses_other','global','other','/other','other','1.18.9',1,1)`},
		{"child session", "UPDATE session SET parent_id='ses_parent'"},
		{"native workspace", "UPDATE session SET workspace_id='workspace'"},
		{"pending tool", "UPDATE part SET data=json_set(data,'$.state.status','pending') WHERE id='prt_tool'"},
		{"running tool", "UPDATE part SET data=json_set(data,'$.state.status','running') WHERE id='prt_tool'"},
		{"unfinished assistant", "UPDATE message SET data=json_remove(data,'$.time.completed') WHERE id='msg_final'"},
		{"unfinished exchange", "UPDATE message SET data=json_set(data,'$.finish','tool-calls') WHERE id='msg_final'"},
		{"wrong user model", "UPDATE message SET data=json_set(data,'$.model.modelID','other') WHERE id='msg_user'"},
		{"missing user model", "UPDATE message SET data=json_remove(data,'$.model') WHERE id='msg_user'"},
		{"wrong session model", "UPDATE session SET model=json_set(model,'$.providerID','other')"},
		{"nondefault session variant", "UPDATE session SET model=json_set(model,'$.variant','high')"},
		{"message variant", "UPDATE message SET data=json_set(data,'$.model.variant','high') WHERE id='msg_user'"},
		{"plan mode", "UPDATE message SET data=json_set(data,'$.mode','plan') WHERE id='msg_final'"},
		{"retained permissions", `UPDATE session SET permission='[{"permission":"*","action":"allow","pattern":"*"}]'`},
		{"user tools", `UPDATE message SET data=json_set(data,'$.tools',json('{"write":true}')) WHERE id='msg_user'`},
		{"revert", "UPDATE session SET revert='{}'"},
		{"compacting", "UPDATE session SET time_compacting=1005"},
		{"compacted tool result", "UPDATE part SET data=json_set(data,'$.state.time.compacted',1005) WHERE id='prt_tool'"},
		{"archived", "UPDATE session SET time_archived=1005"},
		{"provider version", "UPDATE session SET version='1.18.10'"},
		{"working directory", "UPDATE session SET directory='/workspace/other'"},
		{"foreign message", "UPDATE message SET session_id='ses_other' WHERE id='msg_final'"},
		{"foreign part", "UPDATE part SET session_id='ses_other' WHERE id='prt_tool'"},
		{"foreign message parent", "UPDATE message SET data=json_set(data,'$.parentID','msg_other') WHERE id='msg_final'"},
		{"orphan part", "UPDATE part SET message_id='msg_other' WHERE id='prt_tool'"},
		{"tool-output file", "UPDATE part SET data=json_set(data,'$.state.metadata.outputPath','/private/tool-output/tool_1') WHERE id='prt_tool'"},
		{"unindexed tool-output file", "UPDATE part SET data=json_set(data,'$.state.output','Full output saved to: /private/file') WHERE id='prt_tool'"},
		{"native snapshot", `UPDATE part SET data='{"type":"snapshot","snapshot":"object-id"}' WHERE id='prt_tool'`},
		{"step snapshot", `UPDATE part SET data='{"type":"step-start","snapshot":"object-id"}' WHERE id='prt_tool'`},
		{"external attachment", `UPDATE part SET data='{"type":"file","mime":"image/png","url":"file:///private/file.png"}' WHERE id='prt_final'`},
		{"native child tool", "UPDATE part SET data=json_set(data,'$.tool','task') WHERE id='prt_tool'"},
		{"native todo", "INSERT INTO todo VALUES ('ses_native','old goal','pending','high',0,1,1)"},
		{"native inbox", "INSERT INTO session_input VALUES ('input','ses_native','{}','pending',1,NULL,1)"},
		{"native v2 messages", "INSERT INTO session_message VALUES ('v2','ses_native','user',1,1,1,'{}')"},
		{"native context", "INSERT INTO session_context_epoch VALUES ('ses_native','baseline','{}',1)"},
		{"new columns", "ALTER TABLE message ADD COLUMN unknown_state text"},
		{"new native table", "CREATE TABLE goal (id text)"},
		{"reserved prefix lookalike", "CREATE TABLE sqliteExtra (id text)"},
		{"native trigger", "CREATE TRIGGER copy_message AFTER INSERT ON message BEGIN SELECT 1; END"},
		{"native view", "CREATE VIEW history AS SELECT * FROM message"},
		{"malformed native JSON", "UPDATE part SET data='not json' WHERE id='prt_tool'"},
		{"duplicate native field", `UPDATE part SET data='{"type":"tool","type":"text","text":"duplicate"}' WHERE id='prt_tool'`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db, path := newDatabaseFixture(t)
			seedConversation(t, db)
			execFixture(t, db, test.query)
			if _, err := Capture(t.Context(), path, testSession, testCWD, testModel); err == nil {
				t.Fatal("accepted unsupported native history")
			}
		})
	}
}

func TestCaptureExcludesUncommittedWALChanges(t *testing.T) {
	db, path := newDatabaseFixture(t)
	seedConversation(t, db)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec("UPDATE part SET data=json_set(data,'$.state.output','UNCOMMITTED') WHERE id='prt_tool'"); err != nil {
		t.Fatal(err)
	}
	data, err := Capture(t.Context(), path, testSession, testCWD, testModel)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(decompressFixture(t, data), []byte("UNCOMMITTED")) {
		t.Fatal("capture included an uncommitted WAL transaction")
	}
}

func TestRestoreRejectsCorruptionAndMismatchedIdentity(t *testing.T) {
	source, path := newDatabaseFixture(t)
	seedConversation(t, source)
	valid, err := Capture(t.Context(), path, testSession, testCWD, testModel)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*snapshot)
	}{
		{"format", func(s *snapshot) { s.Format++ }},
		{"version", func(s *snapshot) { s.Version = "1.18.10" }},
		{"session", func(s *snapshot) { s.Session.ID = "ses_other" }},
		{"directory", func(s *snapshot) { s.CWD = "/workspace/other" }},
		{"model", func(s *snapshot) { s.Model = "other/model" }},
		{"project", func(s *snapshot) { s.Project.ID = "other" }},
		{"message session", func(s *snapshot) { s.Messages[0].SessionID = "ses_other" }},
		{"part session", func(s *snapshot) { s.Parts[0].SessionID = "ses_other" }},
		{"part message", func(s *snapshot) { s.Parts[0].MessageID = "msg_other" }},
		{"duplicate messages", func(s *snapshot) { s.Messages[1].ID = s.Messages[0].ID }},
		{"duplicate parts", func(s *snapshot) { s.Parts[1].ID = s.Parts[0].ID }},
		{"reordered messages", func(s *snapshot) { s.Messages[0], s.Messages[1] = s.Messages[1], s.Messages[0] }},
		{"missing parts", func(s *snapshot) { s.Parts = s.Parts[:1] }},
		{"missing exchange", func(s *snapshot) { s.Messages = nil }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			saved, err := decodeSnapshot(valid)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(saved)
			data, err := encodeSnapshot(saved)
			if err != nil {
				t.Fatal(err)
			}
			target, targetPath := newDatabaseFixture(t)
			if err := Restore(t.Context(), targetPath, data, testSession, testCWD, testModel); err == nil {
				t.Fatal("accepted mismatched native snapshot")
			}
			assertNoRestoredRows(t, target)
		})
	}
	plain := decompressFixture(t, valid)
	badChecksum := bytes.Clone(valid)
	badChecksum[len(badChecksum)-1] ^= 1
	for name, data := range map[string][]byte{
		"truncated":      valid[:len(valid)-3],
		"checksum":       badChecksum,
		"second member":  append(bytes.Clone(valid), valid...),
		"trailing bytes": append(bytes.Clone(valid), 0),
		"unknown field":  compressFixture(t, append([]byte(`{"unknown":true,`), plain[1:]...)),
		"duplicate key":  compressFixture(t, append([]byte(`{"format":2,`), plain[1:]...)),
		"trailing JSON":  compressFixture(t, append(bytes.Clone(plain), []byte(`{}`)...)),
	} {
		t.Run(name, func(t *testing.T) {
			if err := Restore(t.Context(), "/absent/not-created.db", data, testSession, testCWD, testModel); !errors.Is(err, ErrInvalidSnapshot) {
				t.Fatalf("expected invalid snapshot before opening a database, got %v", err)
			}
		})
	}
}

func TestSnapshotBounds(t *testing.T) {
	for name, data := range map[string][]byte{
		"compressed":   bytes.Repeat([]byte("x"), MaxCompressedBytes+1),
		"decompressed": compressFixture(t, bytes.Repeat([]byte("x"), MaxDecompressedBytes+1)),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeSnapshot(data); !errors.Is(err, ErrSizeLimit) {
				t.Fatalf("expected size limit, got %v", err)
			}
		})
	}
	t.Run("native row", func(t *testing.T) {
		db, path := newDatabaseFixture(t)
		seedConversation(t, db)
		execFixture(t, db, "UPDATE part SET data=json_set(data,'$.state.output',?) WHERE id='prt_tool'", strings.Repeat("x", MaxRowBytes+1))
		if _, err := Capture(t.Context(), path, testSession, testCWD, testModel); err == nil {
			t.Fatal("accepted oversized native row")
		}
	})
	t.Run("message count", func(t *testing.T) {
		db, path := newDatabaseFixture(t)
		seedConversation(t, db)
		execFixture(t, db, `WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<?)
			INSERT INTO message SELECT 'msg_extra_' || x, 'ses_native', x+2000, x+2000, '{}' FROM n`, MaxMessages)
		if _, err := Capture(t.Context(), path, testSession, testCWD, testModel); !errors.Is(err, ErrSizeLimit) {
			t.Fatalf("expected message count limit, got %v", err)
		}
	})
	t.Run("part count", func(t *testing.T) {
		db, path := newDatabaseFixture(t)
		seedConversation(t, db)
		execFixture(t, db, `WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<?)
			INSERT INTO part SELECT 'prt_extra_' || x, 'msg_user', 'ses_native', 1, 1, '{}' FROM n`, MaxParts)
		if _, err := Capture(t.Context(), path, testSession, testCWD, testModel); !errors.Is(err, ErrSizeLimit) {
			t.Fatalf("expected part count limit, got %v", err)
		}
	})
}

func TestRestoreRequiresEmptyDatabaseAndRollsBack(t *testing.T) {
	db, sourcePath := newDatabaseFixture(t)
	seedConversation(t, db)
	data, err := Capture(t.Context(), sourcePath, testSession, testCWD, testModel)
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		"INSERT INTO credential VALUES ('old',NULL,'old','old',NULL,NULL,1,1,1)",
		"INSERT INTO project (id,worktree,time_created,time_updated,sandboxes) VALUES ('old','/',1,1,'[]')",
		"CREATE TRIGGER reject_import BEFORE INSERT ON part BEGIN SELECT RAISE(ABORT,'private value'); END",
	} {
		target, targetPath := newDatabaseFixture(t)
		execFixture(t, target, query)
		if err := Restore(t.Context(), targetPath, data, testSession, testCWD, testModel); err == nil {
			t.Fatal("accepted nonempty or executable restore schema")
		}
		assertNoRestoredRows(t, target)
	}
	// A target constraint failure after inserting project/session/messages must
	// roll back the complete import. The schema still has the pinned columns.
	target, targetPath := newDatabaseFixture(t)
	execFixture(t, target, "CREATE UNIQUE INDEX incompatible_constraint ON part(session_id)")
	if err := Restore(t.Context(), targetPath, data, testSession, testCWD, testModel); err == nil {
		t.Fatal("expected a restore constraint failure")
	}
	assertNoRestoredRows(t, target)
}

func TestDatabasePathAndCancellationFailClosed(t *testing.T) {
	db, path := newDatabaseFixture(t)
	seedConversation(t, db)
	link := filepath.Join(filepath.Dir(path), "link.db")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{link, filepath.Dir(path), "relative.db", filepath.Join(filepath.Dir(path), "missing.db")} {
		if _, err := Capture(t.Context(), candidate, testSession, testCWD, testModel); err == nil {
			t.Fatal("accepted nonregular or missing native database")
		}
	}
	ancestor := filepath.Join(filepath.Dir(path), "linked-directory")
	if err := os.Symlink(filepath.Dir(path), ancestor); err != nil {
		t.Fatal(err)
	}
	if _, err := Capture(t.Context(), filepath.Join(ancestor, filepath.Base(path)), testSession, testCWD, testModel); err == nil {
		t.Fatal("accepted a symlink in the native database parent path")
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		candidate := filepath.Join(filepath.Dir(path), "sidecar"+strings.TrimPrefix(suffix, "-")+".db")
		if err := os.WriteFile(candidate, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(path, candidate+suffix); err != nil {
			t.Fatal(err)
		}
		if _, err := Capture(t.Context(), candidate, testSession, testCWD, testModel); err == nil {
			t.Fatal("accepted a symlinked native database sidecar")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Capture(ctx, path, testSession, testCWD, testModel); !errors.Is(err, context.Canceled) {
		t.Fatalf("capture did not honor cancellation: %v", err)
	}
}

func TestCaptureErrorsDoNotExposePrivateData(t *testing.T) {
	db, path := newDatabaseFixture(t)
	seedConversation(t, db)
	const private = "PRIVATE_ERROR_CANARY"
	execFixture(t, db, "CREATE TABLE "+private+" (value text)")
	_, err := Capture(t.Context(), path, testSession, testCWD, testModel)
	if err == nil || strings.Contains(err.Error(), private) || strings.Contains(err.Error(), path) {
		t.Fatal("capture exposed private schema content in its error")
	}
}

func newDatabaseFixture(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "opencode.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	execFixture(t, db, "PRAGMA journal_mode=WAL")
	execFixture(t, db, "PRAGMA synchronous=OFF")
	execFixture(t, db, "PRAGMA wal_autocheckpoint=0")
	execFixture(t, db, "BEGIN;"+fixtureSchema+"COMMIT;")
	return db, path
}

func seedConversation(t *testing.T, db *sql.DB) {
	t.Helper()
	execFixture(t, db, "INSERT INTO project (id,worktree,time_created,time_updated,sandboxes) VALUES ('global','/',1,1,'[]')")
	execFixture(t, db, `INSERT INTO session
		(id,project_id,slug,directory,path,title,version,agent,model,time_created,time_updated)
		VALUES (?, 'global', 'test', ?, ?, 'Diagnostic task', ?, ?, ?, 1000, 1004)`,
		testSession, testCWD, strings.TrimPrefix(testCWD, "/"), ProviderVersion, Mode,
		`{"providerID":"orka","id":"openai/gpt-5.5","variant":"default"}`)
	usage := `"cost":0,"tokens":{"input":1,"output":1,"reasoning":0,"cache":{"read":0,"write":0}}`
	rows := []struct {
		id   string
		time int64
		data string
	}{
		{"msg_user", 1000, `{"role":"user","time":{"created":1000},"agent":"build","model":{"providerID":"orka","modelID":"openai/gpt-5.5"}}`},
		{"msg_tool", 1001, fmt.Sprintf(`{"role":"assistant","time":{"created":1001,"completed":1002},
			"parentID":"msg_user","providerID":"orka","modelID":"openai/gpt-5.5","mode":"build","agent":"build",
			"path":{"cwd":%q,"root":"/"},"finish":"tool-calls",%s}`, testCWD, usage)},
		{"msg_final", 1003, fmt.Sprintf(`{"role":"assistant","time":{"created":1003,"completed":1004},
			"parentID":"msg_user","providerID":"orka","modelID":"openai/gpt-5.5","mode":"build","agent":"build",
			"path":{"cwd":%q,"root":"/"},"finish":"stop",%s}`, testCWD, usage)},
	}
	for _, row := range rows {
		execFixture(t, db, "INSERT INTO message VALUES (?, ?, ?, ?, ?)", row.id, testSession, row.time, row.time, row.data)
	}
	parts := []struct{ id, message, data string }{
		{"prt_user", "msg_user", `{"type":"text","text":"Read the diagnostic file and summarize it."}`},
		{"prt_tool", "msg_tool", `{"type":"tool","callID":"call_read","tool":"read","state":{"status":"completed",
			"input":{"filePath":"/workspace/sessions/owner/diagnostic.txt"},"output":"initial diagnostic","title":"read",
			"metadata":{},"time":{"start":1001,"end":1002}}}`},
		{"prt_final", "msg_final", `{"type":"text","text":"The investigation is complete."}`},
	}
	for _, part := range parts {
		execFixture(t, db, "INSERT INTO part VALUES (?, ?, ?, 1000, 1004, ?)", part.id, part.message, testSession, part.data)
	}
}

func execFixture(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func assertNoRestoredRows(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, table := range []string{"session", "message", "part"} {
		var count int
		if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("failed restore left conversation rows: count=%d err=%v", count, err)
		}
	}
}

func compressFixture(t *testing.T, plain []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := gzip.NewWriter(&output)
	if _, err := writer.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func decompressFixture(t *testing.T, data []byte) []byte {
	t.Helper()
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	return plain
}

func TestInlineAttachmentRetainsItsMessageIdentity(t *testing.T) {
	db, path := newDatabaseFixture(t)
	seedConversation(t, db)
	attachment := `[{"type":"file","id":"prt_attachment","sessionID":"ses_native","messageID":"msg_tool",
		"mime":"image/png","url":"data:image/png;base64,AAAA"}]`
	execFixture(t, db, "UPDATE part SET data=json_set(data,'$.state.attachments',json(?)) WHERE id='prt_tool'", attachment)
	data, err := Capture(t.Context(), path, testSession, testCWD, testModel)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := decodeSnapshot(data)
	if err != nil {
		t.Fatal(err)
	}
	for i := range saved.Parts {
		if saved.Parts[i].ID != "prt_tool" {
			continue
		}
		var part map[string]any
		if err := json.Unmarshal(saved.Parts[i].Data, &part); err != nil {
			t.Fatal(err)
		}
		state := part["state"].(map[string]any)
		state["attachments"].([]any)[0].(map[string]any)["sessionID"] = "ses_other"
		saved.Parts[i].Data, err = json.Marshal(part)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := saved.validate(testSession, testCWD, testModel); err == nil {
		t.Fatal("accepted an attachment from another native conversation")
	}
}
