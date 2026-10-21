package sqlite

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	sqliteDriver "modernc.org/sqlite"
)

func TestOpenLockedStoreMigrationLifecycle(t *testing.T) {
	for _, test := range []struct {
		name, revision, interruption string
		backups                      int
	}{
		{"149", "0e567418", "", 1},
		{"150", "1f18614c", "", 1},
		{"162", "57583975", "", 1},
		{"166", "abd2c040", "", 1},
		{"158", "fdb64ecd", "", 1},
		{"rollback and retry", "0e567418", "failure", 2},
		{"crash and retry", "0e567418", "crash", 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			path, old := historicalSchemaDB(t, test.revision)
			seedMigrationRecords(t, old, path)
			before := storedSchemaSQL(t, old)
			if test.interruption != "" {
				interruptMigration(t, old, path, test.interruption)
				require.Equal(t, before, storedSchemaSQL(t, old))
				assertMigrationRecords(t, old, path)
				require.Len(t, migrationBackups(t, path), 1)
			}
			var backups []string
			for attempt := range 2 {
				s, err := OpenLockedStore(path)
				require.NoError(t, err)
				require.NoError(t, initializeSchema(s.db))
				assertMigrationRecords(t, s.db, path)
				checkSQLiteIntegrity(t, s.db)
				require.NoError(t, s.close())
				require.NoError(t, old.Close())
				if attempt == 1 {
					require.Equal(t, backups, migrationBackups(t, path), "current reopen must not back up")
					continue
				}
				backups = migrationBackups(t, path)
				require.Len(t, backups, test.backups)
				for _, backup := range backups {
					info, err := os.Stat(backup)
					require.NoError(t, err)
					require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
					db := rawMigrationDB(t, backup)
					require.Equal(t, before, storedSchemaSQL(t, db))
					assertMigrationRecords(t, db, backup)
					checkSQLiteIntegrity(t, db)
					require.NoError(t, db.Close())
				}
			}
		})
	}
}

func TestOpenLockedStoreRefusesUnsupportedIntermediateAndUnknownSchemas(t *testing.T) {
	for _, test := range []struct {
		name, revision, change string
		unlocked               bool
	}{
		{name: "unsupported #700", revision: "b5265055"},
		{name: "same count modified index", revision: "1f18614c", change: `DROP INDEX idx_github_mutation_records_operation_sha; CREATE INDEX idx_github_mutation_records_operation_sha ON github_mutation_records(operation)`},
		{name: "extra object", revision: "0e567418", change: `CREATE TABLE unexpected(value TEXT)`},
		{name: "partial native step", revision: "57583975", change: `CREATE TABLE native_session_deleted_names(namespace TEXT)`},
		{name: "NewDB without controller lock", revision: "0e567418", unlocked: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path, db := historicalSchemaDB(t, test.revision)
			seedMigrationRecords(t, db, path)
			if test.change != "" {
				_, err := db.Exec(test.change)
				require.NoError(t, err)
			}
			before := storedSchemaSQL(t, db)
			require.NoError(t, db.Close())
			for range 2 {
				var err error
				if test.unlocked {
					opened, openErr := NewDB(path)
					require.Nil(t, opened)
					err = openErr
				} else {
					opened, openErr := OpenLockedStore(path)
					require.Nil(t, opened)
					err = openErr
				}
				require.ErrorContains(t, err, "unsupported SQLite schema")
				if test.revision == "b5265055" {
					for _, detail := range []string{"#700", "b5265055", "upgrading.md"} {
						require.ErrorContains(t, err, detail)
					}
				}
				raw := rawMigrationDB(t, path)
				require.Equal(t, before, storedSchemaSQL(t, raw))
				assertMigrationRecords(t, raw, path)
				require.NoError(t, raw.Close())
				require.Empty(t, migrationBackups(t, path))
			}
		})
	}
}

func TestOpenLockedStoreRefusesHarnessV1Records(t *testing.T) {
	for table, insert := range map[string]string{
		"harness_v1_attempts": `INSERT INTO harness_v1_attempts(id,namespace,task_uid,attempt,binding_digest,snapshot_digest,
			request_digest,turn_id,state,retry_class,version,created_at,updated_at)
			VALUES('a','ns','uid',1,'b','s','r','t','Prepared','none',1,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`,
		"runtime_sessions": `INSERT INTO runtime_sessions(id,namespace,session_name,provider,state,cleanup_policy)
			VALUES('rs','ns','session','kubernetes-service','Ready','Delete')`,
		"session_lineages": `INSERT INTO session_lineages(namespace,session_name,namespace_uid,session_uid,contract_version,
			lineage_generation,runtime_identity,version,created_at,updated_at)
			VALUES('ns','session','ns-uid','session-uid','orka.harness.v1',1,'runtime',1,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`,
	} {
		t.Run(table, func(t *testing.T) {
			path, db := historicalSchemaDB(t, "abd2c040")
			_, err := db.Exec(insert)
			require.NoError(t, err)
			before := storedSchemaSQL(t, db)
			require.NoError(t, db.Close())
			opened, err := OpenLockedStore(path)
			require.Nil(t, opened)
			require.ErrorContains(t, err, "harness v1 "+table+" records")
			raw := rawMigrationDB(t, path)
			require.Equal(t, before, storedSchemaSQL(t, raw))
			var rows int
			require.NoError(t, raw.QueryRow(`SELECT COUNT(*) FROM `+table).Scan(&rows))
			require.Equal(t, 1, rows)
			require.NoError(t, raw.Close())
		})
	}
}

func TestOpenLockedStoreKeepsHarnessV2LineagesAcrossV1TableRemoval(t *testing.T) {
	path, db := historicalSchemaDB(t, "abd2c040")
	_, err := db.Exec(`INSERT INTO session_lineages(namespace,session_name,namespace_uid,session_uid,contract_version,
		lineage_generation,runtime_identity,version,created_at,updated_at)
		VALUES('ns','session','ns-uid','session-uid','orka.harness.v2',1,'runtime',1,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	s, err := OpenLockedStore(path)
	require.NoError(t, err)
	var rows int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM session_lineages WHERE contract_version = 'orka.harness.v2'`).Scan(&rows))
	require.Equal(t, 1, rows)
	require.NoError(t, s.close())
}

func TestOpenLockedStoreCurrentDoesNotBackUp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	for range 2 {
		s, err := OpenLockedStore(path)
		require.NoError(t, err)
		require.NoError(t, s.close())
		require.Empty(t, migrationBackups(t, path))
	}
}

// The newest prior layout uses only frozen historical DDL, never current
// definitions. Changing current DDL without extending the chain fails here.
func TestNewestHistoricalMigrationReproducesCurrentSchema(t *testing.T) {
	_, db := historicalSchemaDB(t, "abd2c040")
	conn, err := db.Conn(t.Context())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	steps := schemaMigrations()
	require.NoError(t, runSchemaMigrations(t.Context(), conn, schema166, steps[len(steps)-2:]))
	schema, err := readSchema(conn)
	require.NoError(t, err)
	require.NoError(t, validateSchema(schema, currentSchemaStatements()))
}

func TestSQLiteMigrationRefusesChangedSourceAfterBackup(t *testing.T) {
	path, db := historicalSchemaDB(t, "0e567418")
	conn, err := db.Conn(t.Context())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	_, err = backupSchemaDatabase(t.Context(), conn, path)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `CREATE TABLE unexpected(value TEXT)`)
	require.NoError(t, err)
	before, err := readSchema(conn)
	require.NoError(t, err)
	require.ErrorContains(t, runSchemaMigrations(t.Context(), conn, schema149, schemaMigrations()), "schema changed after backup")
	after, err := readSchema(conn)
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestSQLiteMigrationBackupFailures(t *testing.T) {
	for _, failure := range []string{"creation", "verification"} {
		t.Run(failure, func(t *testing.T) {
			path, db := historicalSchemaDB(t, "0e567418")
			seedMigrationRecords(t, db, path)
			want := "verify SQLite backup"
			if failure == "creation" {
				if os.Getuid() == 0 {
					t.Skip("requires filesystem permission enforcement")
				}
				// WAL files are already open; reserve the lock before denying new files.
				lock, err := lockDatabaseFile(path)
				require.NoError(t, err)
				require.NoError(t, lock.Close())
				require.NoError(t, os.Chmod(filepath.Dir(path), 0o500))
				t.Cleanup(func() { _ = os.Chmod(filepath.Dir(path), 0o700) })
				want = "back up SQLite before upgrade"
			} else {
				_, err := db.Exec(`INSERT INTO session_messages(namespace,session_name,role,content) VALUES('orphan','absent','user','preserve me')`)
				require.NoError(t, err)
			}
			before := storedSchemaSQL(t, db)
			s, err := OpenLockedStore(path)
			require.Nil(t, s)
			require.ErrorContains(t, err, want)
			require.NoError(t, os.Chmod(filepath.Dir(path), 0o700))
			require.Equal(t, before, storedSchemaSQL(t, db))
			assertMigrationRecords(t, db, path)
			if failure == "verification" {
				var content string
				require.NoError(t, db.QueryRow(`SELECT content FROM session_messages WHERE namespace='orphan'`).Scan(&content))
				require.Equal(t, "preserve me", content)
			}
			require.Empty(t, migrationBackups(t, path))
			lock, err := lockDatabaseFile(path)
			require.NoError(t, err)
			require.NoError(t, lock.Close())
		})
	}
}

func interruptMigration(t *testing.T, db *sql.DB, path, mode string) {
	t.Helper()
	if mode == "crash" {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestSQLiteMigrationCrashProcess$")
		cmd.Env = append(os.Environ(), "ORKA_TEST_SQLITE_MIGRATION_CRASH_PATH="+path)
		output, err := cmd.CombinedOutput()
		var exitErr *exec.ExitError
		require.ErrorAs(t, err, &exitErr, "%s", output)
		require.Equal(t, 29, exitErr.ExitCode(), "%s", output)
		return
	}
	conn, err := db.Conn(t.Context())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	_, err = backupSchemaDatabase(t.Context(), conn, path)
	require.NoError(t, err)
	steps := schemaMigrations()
	steps[1].sql += `; SELECT no_such_migration_function()`
	require.ErrorContains(t, runSchemaMigrations(t.Context(), conn, schema149, steps), "no_such_migration_function")
}

func TestSQLiteMigrationCrashProcess(t *testing.T) {
	path := os.Getenv("ORKA_TEST_SQLITE_MIGRATION_CRASH_PATH")
	if path == "" {
		t.Skip("subprocess invoked by the crash lifecycle case")
	}
	// Exit after all DDL, before commit, without running any cleanup defers.
	require.NoError(t, sqliteDriver.RegisterScalarFunction("w275_crash", 0, func(*sqliteDriver.FunctionContext, []driver.Value) (driver.Value, error) {
		os.Exit(29)
		return nil, nil
	}))
	lock, err := lockDatabaseFile(path)
	require.NoError(t, err)
	defer func() { _ = lock.Close() }()
	conn, err := rawMigrationDB(t, path).Conn(t.Context())
	require.NoError(t, err)
	_, err = backupSchemaDatabase(t.Context(), conn, path)
	require.NoError(t, err)
	steps := schemaMigrations()
	steps[len(steps)-1].sql += `; SELECT w275_crash()`
	require.NoError(t, runSchemaMigrations(t.Context(), conn, schema149, steps))
	t.Fatal("migration did not reach crash point")
}

func historicalSchemaDB(t *testing.T, revision string) (string, *sql.DB) {
	t.Helper()
	read := func(name string) string {
		ddl, err := os.ReadFile(filepath.Join("testdata", "schema", name+".sql"))
		require.NoError(t, err)
		return string(ddl)
	}
	ddl := read("0e567418")
	if revision != "0e567418" {
		ddl += migration149To150
	}
	switch revision {
	case "57583975":
		ddl += migration150To162
	case "abd2c040":
		ddl += migration150To162 + migration162To166
	case "fdb64ecd":
		ddl += migration150To162 + migration162To166 + migration166To167
	case "b5265055":
		ddl += read("b5265055")
	}
	path := filepath.Join(t.TempDir(), "store.db")
	db := rawMigrationDB(t, path)
	_, err := db.Exec(`PRAGMA journal_mode=WAL`)
	require.NoError(t, err)
	_, err = db.Exec(ddl)
	require.NoError(t, err)
	schema, err := readSchema(db)
	require.NoError(t, err)
	// Independent historical digests also catch accidental edits to old steps.
	want := map[string]string{
		"0e567418": "ed423eaa2f45dedb1bd935dff5840016ea0d5976709a2ffecc9d2efb6a9d5ff7",
		"1f18614c": "66de67c84e6cf135ea10f5aa98ebb0a65eb920f2742be8c0bd27c5d651167df8",
		"b5265055": "c82fb870c7ed92488fbe33daf9f0693ffdee5aebff9f48db6fa115fdf99ce83b",
		"57583975": "b8ddc48210dbf5cce95a315cff79b2bad966a97e299e396f5574040e0732f6d7",
		"abd2c040": "1bf39f576acdc853d6512b62e0c526734fdf4e8a972dc1435b881b609f9a0c36",
		"fdb64ecd": "01af383a41304d65579d5221976bf479c7e8561a4ec363ba41dcb5f4d571af6c",
	}
	require.Equal(t, want[revision], schemaFingerprint(schema))
	return path, db
}

// version1StoreStates lists every schema version 1 dataset state that
// production can leave on disk, with whether DatasetInfo lets it mint a
// snapshot key. An incomplete store is one whose key loading or authentication
// failed after OpenLockedStore committed its identity row. A later upgrade of
// these stores must preserve canBootstrap.
var version1StoreStates = []struct {
	name         string
	origin       string
	complete     bool
	canBootstrap bool
}{
	{"new incomplete", datasetOriginNew, false, true},
	{"new complete", datasetOriginNew, true, false},
	{"adopted incomplete", datasetOriginAdopted, false, false},
	{"adopted complete", datasetOriginAdopted, true, false},
}

// schemaVersion1StoreDB returns a schema version 1 store built only from frozen
// inputs: the 166-object layout, the immutable 166-to-167.sql and 167-to-168.sql
// steps, one store_dataset identity row and user_version = 1. An incomplete new store is
// empty, because production writes no records before key bootstrap completes;
// the others hold seeded migration records. It never uses the current schema
// constructor, so it keeps producing version-1 stores after the layout moves
// on. Upgrade tests for the next layout must start from this, not from
// historicalSchemaDB plus 167-to-168.sql alone (see upgradeSchema).
func schemaVersion1StoreDB(t *testing.T, origin string, complete bool) (string, *sql.DB) {
	t.Helper()
	require.Contains(t, []string{datasetOriginNew, datasetOriginAdopted}, origin)
	path, db := historicalSchemaDB(t, "abd2c040")
	if origin == datasetOriginAdopted || complete {
		seedMigrationRecords(t, db, path)
	}
	_, err := db.Exec(migration166To167 + migration167To168)
	require.NoError(t, err)
	bootstrapComplete := 0
	if complete {
		bootstrapComplete = 1
	}
	_, err = db.Exec(`INSERT INTO store_dataset(singleton, dataset_id, origin, bootstrap_complete) VALUES(1, ?, ?, ?)`,
		uuid.NewString(), origin, bootstrapComplete)
	require.NoError(t, err)
	_, err = db.Exec(`PRAGMA user_version = 1`)
	require.NoError(t, err)

	schema, err := readSchema(db)
	require.NoError(t, err)
	// A literal digest, independent of schema168, as in historicalSchemaDB.
	require.Equal(t, "b7af032523fa2f524c19ceb897fc1807e6850a8c43fa6e0779d059f11fe2c133", schemaFingerprint(schema))
	record, err := readDataset(db)
	require.NoError(t, err)
	require.Equal(t, origin, record.origin)
	require.Equal(t, complete, record.complete)
	return path, db
}

type version1StoreState struct {
	fingerprint  string
	version      int
	origin       string
	complete     bool
	canBootstrap bool
}

func readVersion1StoreState(t *testing.T, db *sql.DB, path string) version1StoreState {
	t.Helper()
	schema, err := readSchema(db)
	require.NoError(t, err)
	version, err := readSchemaVersion(db)
	require.NoError(t, err)
	record, err := readDataset(db)
	require.NoError(t, err)
	info, err := NewStore(db, path).DatasetInfo()
	require.NoError(t, err)
	return version1StoreState{
		fingerprint:  schemaFingerprint(schema),
		version:      version,
		origin:       record.origin,
		complete:     record.complete,
		canBootstrap: info.CanBootstrapSnapshotKey,
	}
}

// Pins schemaVersion1StoreDB to what this release's production open path
// writes, including whether the store may still mint a key. Once SchemaVersion
// moves past 1 this release is historical: keep schemaVersion1StoreDB
// unchanged and retire this comparison.
func TestSchemaVersion1StoreMatchesProductionStore(t *testing.T) {
	for _, state := range version1StoreStates {
		t.Run(state.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "store.db")
			if state.origin == datasetOriginAdopted {
				var old *sql.DB
				path, old = historicalSchemaDB(t, "abd2c040")
				require.NoError(t, old.Close())
			}
			s, err := OpenLockedStore(path)
			require.NoError(t, err)
			defer func() { require.NoError(t, s.close()) }()
			if state.complete {
				require.NoError(t, s.SetAgentExecutionSnapshotCipher(testSnapshotCipher(t)))
			}
			production := readVersion1StoreState(t, s.db, path)
			require.Equal(t, state.canBootstrap, production.canBootstrap)

			frozenPath, db := schemaVersion1StoreDB(t, state.origin, state.complete)
			require.Equal(t, production, readVersion1StoreState(t, db, frozenPath))
		})
	}
}

// A 159-object fixture rebuilt from historical DDL plus 167-to-168.sql has the
// current layout, but it is not a store this release writes. It passes the
// version-0 migration gate and is adopted, while every real version-1 store is
// refused there, so a future migration tested only on that fixture would pass
// in tests and fail closed on real installations.
func TestSchemaVersion1StoreDiffersFromRebuiltLayoutFixture(t *testing.T) {
	_, fixture := historicalSchemaDB(t, "abd2c040")
	_, err := fixture.Exec(migration166To167 + migration167To168)
	require.NoError(t, err)
	schema, err := readSchema(fixture)
	require.NoError(t, err)
	require.Equal(t, schema168, schemaFingerprint(schema))
	version, err := readSchemaVersion(fixture)
	require.NoError(t, err)
	require.Zero(t, version)
	_, err = readDataset(fixture)
	require.ErrorContains(t, err, "exactly one record")
	conn, err := fixture.Conn(t.Context())
	require.NoError(t, err)
	require.NoError(t, runSchemaMigrations(t.Context(), conn, schema168, nil))
	require.NoError(t, conn.Close())

	for _, state := range version1StoreStates {
		t.Run(state.name, func(t *testing.T) {
			path, db := schemaVersion1StoreDB(t, state.origin, state.complete)
			before, err := readDataset(db)
			require.NoError(t, err)
			beforeRows := applicationRows(t, db)
			backups := migrationBackups(t, path)

			conn, err := db.Conn(t.Context())
			require.NoError(t, err)
			require.ErrorContains(t, runSchemaMigrations(t.Context(), conn, schema168, nil),
				"unsupported SQLite schema version 1 before upgrade")
			require.NoError(t, conn.Close())
			validationErr := errors.New("validation failed")
			require.ErrorIs(t, upgradeSchema(db, path, validationErr), validationErr)

			require.Equal(t, backups, migrationBackups(t, path))
			after, err := readDataset(db)
			require.NoError(t, err)
			require.Equal(t, before, after)
			require.Equal(t, beforeRows, applicationRows(t, db))
		})
	}
}

func rawMigrationDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func seedMigrationRecords(t *testing.T, db *sql.DB, path string) {
	t.Helper()
	// Keep this connection alive: committed rows remain in WAL during backup.
	_, err := db.Exec(`PRAGMA wal_autocheckpoint=0`)
	require.NoError(t, err)
	s := NewStore(db, path)
	require.NoError(t, s.SaveResult(t.Context(), "ns", "task", []byte("retained result")))
	require.NoError(t, s.SaveResult(t.Context(), "ns", "wal-task", []byte("in WAL")))
	_, err = db.Exec(`INSERT INTO sessions(namespace,name,session_type,message_count) VALUES('ns','retained-session','task',1);
		INSERT INTO session_messages(namespace,session_name,message_id,sort_order,role,content) VALUES('ns','retained-session','stable-message',1,'assistant','retained conversation')`)
	require.NoError(t, err)
}

func assertMigrationRecords(t *testing.T, db *sql.DB, path string) {
	t.Helper()
	s := NewStore(db, path)
	for task, want := range map[string]string{"task": "retained result", "wal-task": "in WAL"} {
		result, err := s.GetResult(t.Context(), "ns", task)
		require.NoError(t, err)
		require.Equal(t, want, string(result))
	}
	session, err := s.GetSession(t.Context(), "ns", "retained-session")
	require.NoError(t, err)
	require.Equal(t, 1, session.MessageCount)
	require.Len(t, session.Messages, 1)
	require.Equal(t, "stable-message", session.Messages[0].ID)
	require.Equal(t, "retained conversation", session.Messages[0].Content)
}

func migrationBackups(t *testing.T, path string) []string {
	t.Helper()
	files, err := filepath.Glob(path + ".pre-upgrade-*.db")
	require.NoError(t, err)
	return files
}

func checkSQLiteIntegrity(t *testing.T, db *sql.DB) {
	t.Helper()
	var integrity string
	require.NoError(t, db.QueryRowContext(t.Context(), `PRAGMA integrity_check`).Scan(&integrity))
	require.Equal(t, "ok", integrity)
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	require.False(t, rows.Next(), "foreign key violation")
	require.NoError(t, rows.Err())
}
