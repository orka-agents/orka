package sqlite

import (
	"database/sql"
	"database/sql/driver"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/orka-agents/orka/internal/store"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
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

func TestNewestHistoricalMigrationReproducesCurrentSchema(t *testing.T) {
	_, db := historicalSchemaDB(t, "abd2c040")
	conn, err := db.Conn(t.Context())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	require.NoError(t, runSchemaMigrations(t.Context(), conn, schema166, migrationsFrom(t, schema166)))
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

func TestSchemaVersion1StoreUpgradesInPlace(t *testing.T) {
	for _, state := range version1StoreStates {
		t.Run(state.name, func(t *testing.T) {
			path, db := schemaVersion1StoreDB(t, state.origin, state.complete)
			identity, err := readDataset(db)
			require.NoError(t, err)
			rows := applicationRows(t, db)
			require.NotContains(t, rows, "operation_receipts")
			rows["operation_receipts"] = nil
			rows = runTableApplicationRows(consolidatedApplicationRows(t, stampedApplicationRows(t, rows)))
			addAHPTables(t, rows)
			addSignInTables(t, rows)
			require.NoError(t, db.Close())

			for attempt := range 2 {
				s, err := OpenLockedStore(path)
				require.NoError(t, err)
				after := readVersion1StoreState(t, s.db, path)
				require.Equal(t, version1StoreState{
					fingerprint:  schema177,
					version:      SchemaVersion,
					origin:       state.origin,
					complete:     state.complete,
					canBootstrap: state.canBootstrap,
				}, after)
				upgraded, err := readDataset(s.db)
				require.NoError(t, err)
				require.Equal(t, identity, upgraded)
				binding, err := readDatasetBinding(t.Context(), s.db)
				require.NoError(t, err)
				require.Equal(t, datasetBinding{}, binding, "a version 1 store belongs to no installation")
				require.Equal(t, rows, applicationRows(t, s.db))
				if state.origin == datasetOriginAdopted || state.complete {
					assertMigrationRecords(t, s.db, path)
				}
				checkSQLiteIntegrity(t, s.db)
				require.NoError(t, s.close())
				backups := migrationBackups(t, path)
				require.Len(t, backups, 1, "attempt %d", attempt)
				backup := rawMigrationDB(t, backups[0])
				backupSchema, err := readSchema(backup)
				require.NoError(t, err)
				require.Equal(t, schema168, schemaFingerprint(backupSchema))
				backupVersion, err := readSchemaVersion(backup)
				require.NoError(t, err)
				require.Equal(t, 1, backupVersion)
				backupIdentity, err := readDataset(backup)
				require.NoError(t, err)
				require.Equal(t, identity, backupIdentity)
				require.NoError(t, backup.Close())
			}
		})
	}
}

func TestSchemaVersion1UpgradeFailureRollsBack(t *testing.T) {
	path, db := schemaVersion1StoreDB(t, datasetOriginNew, true)
	before, err := readDataset(db)
	require.NoError(t, err)
	beforeSchema := storedSchemaSQL(t, db)
	conn, err := db.Conn(t.Context())
	require.NoError(t, err)
	steps := versionUpgrades()
	steps[len(steps)-1].sql += `; SELECT no_such_function()`
	require.Error(t, runVersionUpgrades(t.Context(), conn, 1, schema168, steps))
	require.NoError(t, conn.Close())
	after, err := readDataset(db)
	require.NoError(t, err)
	require.Equal(t, before, after)
	version, err := readSchemaVersion(db)
	require.NoError(t, err)
	require.Equal(t, 1, version)
	require.Equal(t, beforeSchema, storedSchemaSQL(t, db))
	require.NoError(t, db.Close())

	s, err := OpenLockedStore(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.close()) }()
	require.Equal(t, schema177, readVersion1StoreState(t, s.db, path).fingerprint)
	assertMigrationRecords(t, s.db, path)
}

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
	steps := migrationsFrom(t, schema168)
	require.NoError(t, runSchemaMigrations(t.Context(), conn, schema168, steps))
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
			require.ErrorContains(t, runSchemaMigrations(t.Context(), conn, schema168, steps),
				"unsupported SQLite schema version 1 before upgrade")
			require.NoError(t, conn.Close())

			require.Equal(t, backups, migrationBackups(t, path))
			after, err := readDataset(db)
			require.NoError(t, err)
			require.Equal(t, before, after)
			require.Equal(t, beforeRows, applicationRows(t, db))
		})
	}
}

var version2StoreStates = []struct {
	name         string
	origin       string
	complete     bool
	canBootstrap bool
	installation string
}{
	{"new incomplete", datasetOriginNew, false, true, ""},
	{"new complete", datasetOriginNew, true, false, ""},
	{"new complete advanced", datasetOriginNew, true, false, "installation-a"},
	{"adopted incomplete", datasetOriginAdopted, false, false, ""},
	{"adopted complete", datasetOriginAdopted, true, false, ""},
	{"adopted complete advanced", datasetOriginAdopted, true, false, "installation-a"},
}

func schemaVersion2StoreDB(t *testing.T, origin string, complete bool, installation string) (string, *sql.DB) {
	t.Helper()
	path, db := schemaVersion1StoreDB(t, origin, complete)
	_, err := db.Exec(migration168To169 + `; PRAGMA user_version = 2`)
	require.NoError(t, err)
	if installation != "" {
		at := time.Date(2026, time.October, 1, 2, 3, 4, 5, time.UTC)
		_, err = db.Exec(`UPDATE store_dataset SET installation_id = ? WHERE singleton = 1`, installation)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO controller_epochs(name, epoch, holder_id, request_digest, version, acquired_at, updated_at,
			previous_epoch, previous_holder_id, advanced_at) VALUES('orka-controller', 2, 'holder-b', ?, 2, ?, ?, 1, 'holder-a', ?)`,
			store.CanonicalBytesDigest([]byte("holder-b")), at, at, at)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO operation_receipts(operation_id, method, request_digest, state, fence_name, fence_epoch,
			fence_holder, issued_at_ns, recorded_at_ns, result) VALUES('op-1', 'SaveResult', ?, 'committed', 'orka-controller', 2,
			'holder-b', ?, ?, NULL)`, store.CanonicalBytesDigest([]byte("request")), at.UnixNano(), at.UnixNano())
		require.NoError(t, err)
	}

	schema, err := readSchema(db)
	require.NoError(t, err)
	require.Equal(t, "4756d67c9c3850bd87286c81f4398f245163f5edb038cba38e0269d347dd09e2", schemaFingerprint(schema))
	version, err := readSchemaVersion(db)
	require.NoError(t, err)
	require.Equal(t, 2, version)
	return path, db
}

func readVersion2Installation(t *testing.T, db *sql.DB) string {
	t.Helper()
	var installation string
	require.NoError(t, db.QueryRow(`SELECT installation_id FROM store_dataset WHERE singleton = 1`).Scan(&installation))
	return installation
}

func TestSchemaVersion2StoreUpgradesInPlace(t *testing.T) {
	for _, state := range version2StoreStates {
		t.Run(state.name, func(t *testing.T) {
			path, db := schemaVersion2StoreDB(t, state.origin, state.complete, state.installation)
			identity, err := readDataset(db)
			require.NoError(t, err)
			rows := runTableApplicationRows(consolidatedApplicationRows(t, stampedApplicationRows(t, applicationRows(t, db))))
			addAHPTables(t, rows)
			addSignInTables(t, rows)
			require.NoError(t, db.Close())

			for attempt := range 2 {
				s, err := OpenLockedStore(path)
				require.NoError(t, err)
				require.Equal(t, version1StoreState{
					fingerprint:  schema177,
					version:      SchemaVersion,
					origin:       state.origin,
					complete:     state.complete,
					canBootstrap: state.canBootstrap,
				}, readVersion1StoreState(t, s.db, path))
				upgraded, err := readDataset(s.db)
				require.NoError(t, err)
				require.Equal(t, identity, upgraded)
				require.Equal(t, rows, applicationRows(t, s.db))
				binding, err := readDatasetBinding(t.Context(), s.db)
				require.NoError(t, err)
				require.Equal(t, datasetBinding{installation: state.installation}, binding)
				got, err := s.GetDatasetBinding(t.Context(), identity.id)
				if state.installation == "" {
					require.ErrorIs(t, err, store.ErrNotFound)
				} else {
					require.NoError(t, err)
					require.Equal(t, storagev1.DatasetBinding{InstallationID: state.installation, State: storagev1.DatasetBound}, got,
						"the advance's installation owns the dataset at generation 0, with no record and no known bind time")
				}
				if state.origin == datasetOriginAdopted || state.complete {
					assertMigrationRecords(t, s.db, path)
				}
				checkSQLiteIntegrity(t, s.db)
				require.NoError(t, s.close())
				backups := migrationBackups(t, path)
				require.Len(t, backups, 1, "attempt %d", attempt)
				backup := rawMigrationDB(t, backups[0])
				backupSchema, err := readSchema(backup)
				require.NoError(t, err)
				require.Equal(t, schema169, schemaFingerprint(backupSchema))
				backupVersion, err := readSchemaVersion(backup)
				require.NoError(t, err)
				require.Equal(t, 2, backupVersion)
				backupIdentity, err := readDataset(backup)
				require.NoError(t, err)
				require.Equal(t, identity, backupIdentity)
				require.Equal(t, state.installation, readVersion2Installation(t, backup))
				require.NoError(t, backup.Close())
			}

			s, err := OpenLockedStore(path)
			require.NoError(t, err)
			defer func() { require.NoError(t, s.close()) }()
			ctx := t.Context()
			other := storagev1.BindDatasetRequest{
				DatasetID: identity.id, InstallationID: "installation-b", RecordUID: "record-b", SwitchGeneration: 1,
				Mode: storagev1.BindEmbedded,
			}
			if state.installation == "" {
				bound, err := s.BindDataset(ctx, other)
				require.NoError(t, err)
				require.Equal(t, "installation-b", bound.InstallationID)
				return
			}
			_, err = s.BindDataset(ctx, other)
			var conflict storagev1.BindingConflictError
			require.ErrorAs(t, err, &conflict, "another installation never claims the upgraded dataset")
			_, err = s.AdvanceControllerEpoch(ctx, storagev1.AdvanceControllerEpochRequest{
				DatasetID: identity.id, InstallationID: "installation-b",
				Epoch: store.ControllerEpoch{Name: store.DefaultControllerEpochName, Epoch: 3, HolderID: "holder-c",
					RequestDigest: store.CanonicalBytesDigest([]byte("holder-c")), Version: 3,
					AcquiredAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()},
			})
			require.ErrorIs(t, err, store.ErrFenced)
			mirror, err := s.GetControllerEpochMirror(ctx, store.DefaultControllerEpochName)
			require.NoError(t, err)
			require.EqualValues(t, 2, mirror.Epoch.Epoch)
			require.Equal(t, &store.ControllerEpochFence{Name: store.DefaultControllerEpochName, Epoch: 1, HolderID: "holder-a"}, mirror.Previous)
			require.Equal(t, state.installation, mirror.InstallationID)

			own := other
			own.InstallationID = state.installation
			bound, err := s.BindDataset(ctx, own)
			require.NoError(t, err, "the installation the advance recorded binds the upgraded dataset")
			require.Equal(t, state.installation, bound.InstallationID)
			require.EqualValues(t, 1, bound.SwitchGeneration)
		})
	}
}

func TestSchemaVersion2UpgradeFailureRollsBack(t *testing.T) {
	path, db := schemaVersion2StoreDB(t, datasetOriginNew, true, "installation-a")
	before, err := readDataset(db)
	require.NoError(t, err)
	beforeSchema := storedSchemaSQL(t, db)
	beforeRows := applicationRows(t, db)
	conn, err := db.Conn(t.Context())
	require.NoError(t, err)
	steps := versionUpgrades()
	first := slices.IndexFunc(steps, func(step versionUpgrade) bool { return step.from == 2 })
	require.GreaterOrEqual(t, first, 0)
	steps[len(steps)-1].sql += `; SELECT no_such_function()`
	require.Error(t, runVersionUpgrades(t.Context(), conn, 2, schema169, steps[first:]))
	require.NoError(t, conn.Close())
	after, err := readDataset(db)
	require.NoError(t, err)
	require.Equal(t, before, after)
	version, err := readSchemaVersion(db)
	require.NoError(t, err)
	require.Equal(t, 2, version)
	require.Equal(t, beforeSchema, storedSchemaSQL(t, db))
	require.Equal(t, beforeRows, applicationRows(t, db))
	require.Equal(t, "installation-a", readVersion2Installation(t, db))
	require.NoError(t, db.Close())

	s, err := OpenLockedStore(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.close()) }()
	require.Equal(t, schema177, readVersion1StoreState(t, s.db, path).fingerprint)
	binding, err := readDatasetBinding(t.Context(), s.db)
	require.NoError(t, err)
	require.Equal(t, datasetBinding{installation: "installation-a"}, binding)
	assertMigrationRecords(t, s.db, path)
}

func TestSchemaVersion2StoreDiffersFromRebuiltLayoutFixture(t *testing.T) {
	_, fixture := historicalSchemaDB(t, "abd2c040")
	_, err := fixture.Exec(migration166To167 + migration167To168 + migration168To169)
	require.NoError(t, err)
	schema, err := readSchema(fixture)
	require.NoError(t, err)
	require.Equal(t, schema169, schemaFingerprint(schema))
	conn, err := fixture.Conn(t.Context())
	require.NoError(t, err)
	require.NoError(t, runSchemaMigrations(t.Context(), conn, schema169, migrationsFrom(t, schema169)))
	require.NoError(t, conn.Close())

	for _, state := range version2StoreStates {
		t.Run(state.name, func(t *testing.T) {
			_, db := schemaVersion2StoreDB(t, state.origin, state.complete, state.installation)
			before, err := readDataset(db)
			require.NoError(t, err)
			beforeRows := applicationRows(t, db)
			conn, err := db.Conn(t.Context())
			require.NoError(t, err)
			require.ErrorContains(t, runSchemaMigrations(t.Context(), conn, schema169, migrationsFrom(t, schema169)),
				"unsupported SQLite schema version 2 before upgrade")
			require.NoError(t, conn.Close())
			after, err := readDataset(db)
			require.NoError(t, err)
			require.Equal(t, before, after)
			require.Equal(t, beforeRows, applicationRows(t, db))
			require.Equal(t, state.installation, readVersion2Installation(t, db))
		})
	}
}

var version3StoreStates = []struct {
	name         string
	origin       string
	complete     bool
	canBootstrap bool
	installation string
	bound        bool
}{
	{"new incomplete", datasetOriginNew, false, true, "", false},
	{"new complete", datasetOriginNew, true, false, "", false},
	{"new complete advanced", datasetOriginNew, true, false, "installation-a", false},
	{"new complete bound", datasetOriginNew, true, false, "installation-a", true},
	{"adopted incomplete", datasetOriginAdopted, false, false, "", false},
	{"adopted complete", datasetOriginAdopted, true, false, "", false},
	{"adopted complete advanced", datasetOriginAdopted, true, false, "installation-a", false},
	{"adopted complete bound", datasetOriginAdopted, true, false, "installation-a", true},
}

func schemaVersion3StateStoreDB(t *testing.T, origin string, complete bool, installation string, bound bool) (string, *sql.DB) {
	t.Helper()
	path, db := schemaVersion2StoreDB(t, origin, complete, installation)
	_, err := db.Exec(migration169To170 + `; PRAGMA user_version = 3`)
	require.NoError(t, err)
	if bound {
		require.NotEmpty(t, installation)
		at := time.Date(2026, time.October, 2, 3, 4, 5, 6, time.UTC)
		_, err = db.Exec(`UPDATE store_dataset SET record_uid = 'record-a', switch_generation = 1,
			key_fingerprint = 'key-a', binding_state = 'bound', bound_at_ns = ? WHERE singleton = 1`, at.UnixNano())
		require.NoError(t, err)
	}

	schema, err := readSchema(db)
	require.NoError(t, err)
	require.Equal(t, "7d46e41ac2bf6265fa94eeaed0c70f221d407213580bec9a267bd45ee5419bf7", schemaFingerprint(schema))
	version, err := readSchemaVersion(db)
	require.NoError(t, err)
	require.Equal(t, 3, version)
	return path, db
}

func TestSchemaVersion3StoreUpgradesInPlace(t *testing.T) {
	for _, state := range version3StoreStates {
		t.Run(state.name, func(t *testing.T) {
			path, db := schemaVersion3StateStoreDB(t, state.origin, state.complete, state.installation, state.bound)
			identity, err := readDataset(db)
			require.NoError(t, err)
			binding, err := readDatasetBinding(t.Context(), db)
			require.NoError(t, err)
			require.Equal(t, state.installation, binding.installation)
			require.Equal(t, state.bound, binding.state == bindingStateBound)
			rows := runTableApplicationRows(consolidatedApplicationRows(t, stampedApplicationRows(t, applicationRows(t, db))))
			addAHPTables(t, rows)
			addSignInTables(t, rows)
			require.NoError(t, db.Close())

			for attempt := range 2 {
				s, err := OpenLockedStore(path)
				require.NoError(t, err)
				require.Equal(t, version1StoreState{
					fingerprint:  schema177,
					version:      SchemaVersion,
					origin:       state.origin,
					complete:     state.complete,
					canBootstrap: state.canBootstrap,
				}, readVersion1StoreState(t, s.db, path))
				upgraded, err := readDataset(s.db)
				require.NoError(t, err)
				require.Equal(t, identity, upgraded)
				upgradedBinding, err := readDatasetBinding(t.Context(), s.db)
				require.NoError(t, err)
				require.Equal(t, binding, upgradedBinding)
				require.Equal(t, rows, applicationRows(t, s.db))
				if state.origin == datasetOriginAdopted || state.complete {
					assertMigrationRecords(t, s.db, path)
				}
				checkSQLiteIntegrity(t, s.db)
				require.NoError(t, s.close())
				backups := migrationBackups(t, path)
				require.Len(t, backups, 1, "attempt %d", attempt)
				backup := rawMigrationDB(t, backups[0])
				backupSchema, err := readSchema(backup)
				require.NoError(t, err)
				require.Equal(t, schema170, schemaFingerprint(backupSchema))
				backupVersion, err := readSchemaVersion(backup)
				require.NoError(t, err)
				require.Equal(t, 3, backupVersion)
				backupIdentity, err := readDataset(backup)
				require.NoError(t, err)
				require.Equal(t, identity, backupIdentity)
				require.NoError(t, backup.Close())
			}
		})
	}
}

func TestSchemaVersion3UpgradeFailureRollsBack(t *testing.T) {
	path, db := schemaVersion3StateStoreDB(t, datasetOriginNew, true, "installation-a", true)
	before, err := readDataset(db)
	require.NoError(t, err)
	beforeBinding, err := readDatasetBinding(t.Context(), db)
	require.NoError(t, err)
	beforeSchema := storedSchemaSQL(t, db)
	beforeRows := applicationRows(t, db)
	conn, err := db.Conn(t.Context())
	require.NoError(t, err)
	steps := versionUpgrades()
	first := slices.IndexFunc(steps, func(step versionUpgrade) bool { return step.from == 3 })
	require.GreaterOrEqual(t, first, 0)
	steps[len(steps)-1].sql += `; SELECT no_such_function()`
	require.Error(t, runVersionUpgrades(t.Context(), conn, 3, schema170, steps[first:]))
	require.NoError(t, conn.Close())
	after, err := readDataset(db)
	require.NoError(t, err)
	require.Equal(t, before, after)
	afterBinding, err := readDatasetBinding(t.Context(), db)
	require.NoError(t, err)
	require.Equal(t, beforeBinding, afterBinding)
	version, err := readSchemaVersion(db)
	require.NoError(t, err)
	require.Equal(t, 3, version)
	require.Equal(t, beforeSchema, storedSchemaSQL(t, db))
	require.Equal(t, beforeRows, applicationRows(t, db))
	require.NoError(t, db.Close())

	s, err := OpenLockedStore(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.close()) }()
	require.Equal(t, schema177, readVersion1StoreState(t, s.db, path).fingerprint)
	binding, err := readDatasetBinding(t.Context(), s.db)
	require.NoError(t, err)
	require.Equal(t, beforeBinding, binding)
	assertMigrationRecords(t, s.db, path)
}

func TestSchemaVersion3StoreDiffersFromRebuiltLayoutFixture(t *testing.T) {
	_, fixture := historicalSchemaDB(t, "abd2c040")
	_, err := fixture.Exec(migration166To167 + migration167To168 + migration168To169 + migration169To170)
	require.NoError(t, err)
	schema, err := readSchema(fixture)
	require.NoError(t, err)
	require.Equal(t, schema170, schemaFingerprint(schema))
	conn, err := fixture.Conn(t.Context())
	require.NoError(t, err)
	require.NoError(t, runSchemaMigrations(t.Context(), conn, schema170, migrationsFrom(t, schema170)))
	require.NoError(t, conn.Close())

	for _, state := range version3StoreStates {
		t.Run(state.name, func(t *testing.T) {
			_, db := schemaVersion3StateStoreDB(t, state.origin, state.complete, state.installation, state.bound)
			before, err := readDataset(db)
			require.NoError(t, err)
			beforeBinding, err := readDatasetBinding(t.Context(), db)
			require.NoError(t, err)
			beforeRows := applicationRows(t, db)
			conn, err := db.Conn(t.Context())
			require.NoError(t, err)
			require.ErrorContains(t, runSchemaMigrations(t.Context(), conn, schema170, migrationsFrom(t, schema170)),
				"unsupported SQLite schema version 3 before upgrade")
			require.NoError(t, conn.Close())
			after, err := readDataset(db)
			require.NoError(t, err)
			require.Equal(t, before, after)
			afterBinding, err := readDatasetBinding(t.Context(), db)
			require.NoError(t, err)
			require.Equal(t, beforeBinding, afterBinding)
			require.Equal(t, beforeRows, applicationRows(t, db))
		})
	}
}

func schemaVersion4StoreDB(t *testing.T, origin string, complete bool, installation string, bound bool) (string, *sql.DB) {
	t.Helper()
	path, db := schemaVersion3StateStoreDB(t, origin, complete, installation, bound)
	_, err := db.Exec(migration170To171 + `; PRAGMA user_version = 4`)
	require.NoError(t, err)

	schema, err := readSchema(db)
	require.NoError(t, err)
	require.Equal(t, "bb9e3ee12fe041e1bf1dced0c0f61f49b4525ea04825d2806a9f544fbb432ac2", schemaFingerprint(schema))
	version, err := readSchemaVersion(db)
	require.NoError(t, err)
	require.Equal(t, 4, version)
	return path, db
}

func TestSchemaVersion4StoreUpgradesInPlace(t *testing.T) {
	for _, state := range version3StoreStates {
		t.Run(state.name, func(t *testing.T) {
			path, db := schemaVersion4StoreDB(t, state.origin, state.complete, state.installation, state.bound)
			identity, err := readDataset(db)
			require.NoError(t, err)
			binding, err := readDatasetBinding(t.Context(), db)
			require.NoError(t, err)
			require.Equal(t, state.installation, binding.installation)
			require.Equal(t, state.bound, binding.state == bindingStateBound)
			rows := runTableApplicationRows(consolidatedApplicationRows(t, stampedApplicationRows(t, applicationRows(t, db))))
			addAHPTables(t, rows)
			addSignInTables(t, rows)
			require.NoError(t, db.Close())

			for attempt := range 2 {
				s, err := OpenLockedStore(path)
				require.NoError(t, err)
				require.Equal(t, version1StoreState{
					fingerprint:  schema177,
					version:      SchemaVersion,
					origin:       state.origin,
					complete:     state.complete,
					canBootstrap: state.canBootstrap,
				}, readVersion1StoreState(t, s.db, path))
				upgraded, err := readDataset(s.db)
				require.NoError(t, err)
				require.Equal(t, identity, upgraded)
				upgradedBinding, err := readDatasetBinding(t.Context(), s.db)
				require.NoError(t, err)
				require.Equal(t, binding, upgradedBinding)
				require.Equal(t, rows, applicationRows(t, s.db))
				if state.origin == datasetOriginAdopted || state.complete {
					assertMigrationRecords(t, s.db, path)
				}
				checkSQLiteIntegrity(t, s.db)
				require.NoError(t, s.close())
				backups := migrationBackups(t, path)
				require.Len(t, backups, 1, "attempt %d", attempt)
				backup := rawMigrationDB(t, backups[0])
				backupSchema, err := readSchema(backup)
				require.NoError(t, err)
				require.Equal(t, schema171, schemaFingerprint(backupSchema))
				backupVersion, err := readSchemaVersion(backup)
				require.NoError(t, err)
				require.Equal(t, 4, backupVersion)
				backupIdentity, err := readDataset(backup)
				require.NoError(t, err)
				require.Equal(t, identity, backupIdentity)
				require.NoError(t, backup.Close())
			}
		})
	}
}

func TestSchemaVersion4UpgradeFailureRollsBack(t *testing.T) {
	path, db := schemaVersion4StoreDB(t, datasetOriginNew, true, "installation-a", true)
	before, err := readDataset(db)
	require.NoError(t, err)
	beforeBinding, err := readDatasetBinding(t.Context(), db)
	require.NoError(t, err)
	beforeSchema := storedSchemaSQL(t, db)
	beforeRows := applicationRows(t, db)
	conn, err := db.Conn(t.Context())
	require.NoError(t, err)
	steps := versionUpgrades()
	first := slices.IndexFunc(steps, func(step versionUpgrade) bool { return step.from == 4 })
	require.GreaterOrEqual(t, first, 0)
	steps[len(steps)-1].sql += `; SELECT no_such_function()`
	require.Error(t, runVersionUpgrades(t.Context(), conn, 4, schema171, steps[first:]))
	require.NoError(t, conn.Close())
	after, err := readDataset(db)
	require.NoError(t, err)
	require.Equal(t, before, after)
	afterBinding, err := readDatasetBinding(t.Context(), db)
	require.NoError(t, err)
	require.Equal(t, beforeBinding, afterBinding)
	version, err := readSchemaVersion(db)
	require.NoError(t, err)
	require.Equal(t, 4, version)
	require.Equal(t, beforeSchema, storedSchemaSQL(t, db))
	require.Equal(t, beforeRows, applicationRows(t, db))
	require.NoError(t, db.Close())

	s, err := OpenLockedStore(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.close()) }()
	require.Equal(t, schema177, readVersion1StoreState(t, s.db, path).fingerprint)
	binding, err := readDatasetBinding(t.Context(), s.db)
	require.NoError(t, err)
	require.Equal(t, beforeBinding, binding)
	assertMigrationRecords(t, s.db, path)
}

func TestSchemaVersion4StoreDiffersFromRebuiltLayoutFixture(t *testing.T) {
	_, fixture := historicalSchemaDB(t, "abd2c040")
	_, err := fixture.Exec(migration166To167 + migration167To168 + migration168To169 + migration169To170 + migration170To171)
	require.NoError(t, err)
	schema, err := readSchema(fixture)
	require.NoError(t, err)
	require.Equal(t, schema171, schemaFingerprint(schema))
	conn, err := fixture.Conn(t.Context())
	require.NoError(t, err)
	require.NoError(t, runSchemaMigrations(t.Context(), conn, schema171, migrationsFrom(t, schema171)))
	require.NoError(t, conn.Close())

	for _, state := range version3StoreStates {
		t.Run(state.name, func(t *testing.T) {
			_, db := schemaVersion4StoreDB(t, state.origin, state.complete, state.installation, state.bound)
			before, err := readDataset(db)
			require.NoError(t, err)
			beforeBinding, err := readDatasetBinding(t.Context(), db)
			require.NoError(t, err)
			beforeRows := applicationRows(t, db)
			conn, err := db.Conn(t.Context())
			require.NoError(t, err)
			require.ErrorContains(t, runSchemaMigrations(t.Context(), conn, schema171, migrationsFrom(t, schema171)),
				"unsupported SQLite schema version 4 before upgrade")
			require.NoError(t, conn.Close())
			after, err := readDataset(db)
			require.NoError(t, err)
			require.Equal(t, before, after)
			afterBinding, err := readDatasetBinding(t.Context(), db)
			require.NoError(t, err)
			require.Equal(t, beforeBinding, afterBinding)
			require.Equal(t, beforeRows, applicationRows(t, db))
		})
	}
}

func schemaVersion7StoreDB(t *testing.T, origin string, complete bool, installation string, bound bool) (string, *sql.DB) {
	t.Helper()
	path, db := schemaVersion6StoreDB(t, origin, complete, installation, bound)
	_, err := db.Exec(migration173To174 + `; PRAGMA user_version = 7`)
	require.NoError(t, err)

	schema, err := readSchema(db)
	require.NoError(t, err)
	require.Equal(t, "ce3aacb60e5f3f40f03cbe071637ac1218ff4255fb7fff314642e6b7b416317d", schemaFingerprint(schema))
	version, err := readSchemaVersion(db)
	require.NoError(t, err)
	require.Equal(t, 7, version)
	return path, db
}

func TestAHPSchemaVersion7StoreUpgradesInPlace(t *testing.T) {
	for _, state := range version3StoreStates {
		t.Run(state.name, func(t *testing.T) {
			path, db := schemaVersion7StoreDB(t, state.origin, state.complete, state.installation, state.bound)
			identity, err := readDataset(db)
			require.NoError(t, err)
			binding, err := readDatasetBinding(t.Context(), db)
			require.NoError(t, err)
			require.Equal(t, state.installation, binding.installation)
			require.Equal(t, state.bound, binding.state == bindingStateBound)
			rows := applicationRows(t, db)
			addAHPTables(t, rows)
			addSignInTables(t, rows)
			require.NoError(t, db.Close())

			for attempt := range 2 {
				s, err := OpenLockedStore(path)
				require.NoError(t, err)
				require.Equal(t, version1StoreState{
					fingerprint:  schema177,
					version:      SchemaVersion,
					origin:       state.origin,
					complete:     state.complete,
					canBootstrap: state.canBootstrap,
				}, readVersion1StoreState(t, s.db, path))
				upgraded, err := readDataset(s.db)
				require.NoError(t, err)
				require.Equal(t, identity, upgraded)
				upgradedBinding, err := readDatasetBinding(t.Context(), s.db)
				require.NoError(t, err)
				require.Equal(t, binding, upgradedBinding)
				require.Equal(t, rows, applicationRows(t, s.db))
				if state.origin == datasetOriginAdopted || state.complete {
					assertMigrationRecords(t, s.db, path)
				}
				checkSQLiteIntegrity(t, s.db)
				require.NoError(t, s.close())
				backups := migrationBackups(t, path)
				require.Len(t, backups, 1, "attempt %d", attempt)
				backup := rawMigrationDB(t, backups[0])
				backupSchema, err := readSchema(backup)
				require.NoError(t, err)
				require.Equal(t, schema174, schemaFingerprint(backupSchema))
				backupVersion, err := readSchemaVersion(backup)
				require.NoError(t, err)
				require.Equal(t, 7, backupVersion)
				backupIdentity, err := readDataset(backup)
				require.NoError(t, err)
				require.Equal(t, identity, backupIdentity)
				require.NoError(t, backup.Close())
			}
		})
	}
}

func TestAHPSchemaVersion7UpgradeFailureRollsBack(t *testing.T) {
	path, db := schemaVersion7StoreDB(t, datasetOriginNew, true, "installation-a", true)
	before, err := readDataset(db)
	require.NoError(t, err)
	beforeBinding, err := readDatasetBinding(t.Context(), db)
	require.NoError(t, err)
	beforeSchema := storedSchemaSQL(t, db)
	beforeRows := applicationRows(t, db)
	conn, err := db.Conn(t.Context())
	require.NoError(t, err)
	steps := versionUpgrades()
	first := slices.IndexFunc(steps, func(step versionUpgrade) bool { return step.from == 7 })
	require.GreaterOrEqual(t, first, 0)
	steps[len(steps)-1].sql += `; SELECT no_such_function()`
	require.Error(t, runVersionUpgrades(t.Context(), conn, 7, schema174, steps[first:]))
	require.NoError(t, conn.Close())
	after, err := readDataset(db)
	require.NoError(t, err)
	require.Equal(t, before, after)
	afterBinding, err := readDatasetBinding(t.Context(), db)
	require.NoError(t, err)
	require.Equal(t, beforeBinding, afterBinding)
	version, err := readSchemaVersion(db)
	require.NoError(t, err)
	require.Equal(t, 7, version)
	require.Equal(t, beforeSchema, storedSchemaSQL(t, db))
	require.Equal(t, beforeRows, applicationRows(t, db))
	require.NoError(t, db.Close())

	s, err := OpenLockedStore(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.close()) }()
	require.Equal(t, schema177, readVersion1StoreState(t, s.db, path).fingerprint)
	binding, err := readDatasetBinding(t.Context(), s.db)
	require.NoError(t, err)
	require.Equal(t, beforeBinding, binding)
	assertMigrationRecords(t, s.db, path)
}

func TestAHPSchemaVersion7StoreDiffersFromRebuiltLayoutFixture(t *testing.T) {
	_, fixture := historicalSchemaDB(t, "abd2c040")
	_, err := fixture.Exec(migration166To167 + migration167To168 + migration168To169 + migration169To170 + migration170To171 + migration171To172 + migration172To173 + migration173To174)
	require.NoError(t, err)
	schema, err := readSchema(fixture)
	require.NoError(t, err)
	require.Equal(t, schema174, schemaFingerprint(schema))
	conn, err := fixture.Conn(t.Context())
	require.NoError(t, err)
	require.NoError(t, runSchemaMigrations(t.Context(), conn, schema174, migrationsFrom(t, schema174)))
	require.NoError(t, conn.Close())

	for _, state := range version3StoreStates {
		t.Run(state.name, func(t *testing.T) {
			_, db := schemaVersion7StoreDB(t, state.origin, state.complete, state.installation, state.bound)
			before, err := readDataset(db)
			require.NoError(t, err)
			beforeBinding, err := readDatasetBinding(t.Context(), db)
			require.NoError(t, err)
			beforeRows := applicationRows(t, db)
			conn, err := db.Conn(t.Context())
			require.NoError(t, err)
			require.ErrorContains(t, runSchemaMigrations(t.Context(), conn, schema174, migrationsFrom(t, schema174)),
				"unsupported SQLite schema version 7 before upgrade")
			require.NoError(t, conn.Close())
			after, err := readDataset(db)
			require.NoError(t, err)
			require.Equal(t, before, after)
			afterBinding, err := readDatasetBinding(t.Context(), db)
			require.NoError(t, err)
			require.Equal(t, beforeBinding, afterBinding)
			require.Equal(t, beforeRows, applicationRows(t, db))
		})
	}
}

func schemaVersion8StoreDB(t *testing.T, origin string, complete bool, installation string, bound bool) (string, *sql.DB) {
	t.Helper()
	path, db := schemaVersion7StoreDB(t, origin, complete, installation, bound)
	_, err := db.Exec(migration174To175 + `; PRAGMA user_version = 8`)
	require.NoError(t, err)

	schema, err := readSchema(db)
	require.NoError(t, err)
	require.Equal(t, "54d6dcddb755001aa9ba36a92056d29c9a6a044d60f863e9d5de64cd96ea4d6e", schemaFingerprint(schema))
	version, err := readSchemaVersion(db)
	require.NoError(t, err)
	require.Equal(t, 8, version)
	return path, db
}

func TestSignInSchemaVersion8StoreUpgradesInPlace(t *testing.T) {
	for _, state := range version3StoreStates {
		t.Run(state.name, func(t *testing.T) {
			path, db := schemaVersion8StoreDB(t, state.origin, state.complete, state.installation, state.bound)
			identity, err := readDataset(db)
			require.NoError(t, err)
			binding, err := readDatasetBinding(t.Context(), db)
			require.NoError(t, err)
			require.Equal(t, state.installation, binding.installation)
			require.Equal(t, state.bound, binding.state == bindingStateBound)
			rows := applicationRows(t, db)
			addSignInTables(t, rows)
			require.NoError(t, db.Close())

			for attempt := range 2 {
				s, err := OpenLockedStore(path)
				require.NoError(t, err)
				require.Equal(t, version1StoreState{
					fingerprint:  schema177,
					version:      SchemaVersion,
					origin:       state.origin,
					complete:     state.complete,
					canBootstrap: state.canBootstrap,
				}, readVersion1StoreState(t, s.db, path))
				upgraded, err := readDataset(s.db)
				require.NoError(t, err)
				require.Equal(t, identity, upgraded)
				upgradedBinding, err := readDatasetBinding(t.Context(), s.db)
				require.NoError(t, err)
				require.Equal(t, binding, upgradedBinding)
				require.Equal(t, rows, applicationRows(t, s.db))
				if state.origin == datasetOriginAdopted || state.complete {
					assertMigrationRecords(t, s.db, path)
				}
				checkSQLiteIntegrity(t, s.db)
				require.NoError(t, s.close())
				backups := migrationBackups(t, path)
				require.Len(t, backups, 1, "attempt %d", attempt)
				backup := rawMigrationDB(t, backups[0])
				backupSchema, err := readSchema(backup)
				require.NoError(t, err)
				require.Equal(t, schema175, schemaFingerprint(backupSchema))
				backupVersion, err := readSchemaVersion(backup)
				require.NoError(t, err)
				require.Equal(t, 8, backupVersion)
				backupIdentity, err := readDataset(backup)
				require.NoError(t, err)
				require.Equal(t, identity, backupIdentity)
				require.NoError(t, backup.Close())
			}
		})
	}
}

func TestSignInSchemaVersion8UpgradeFailureRollsBack(t *testing.T) {
	path, db := schemaVersion8StoreDB(t, datasetOriginNew, true, "installation-a", true)
	before, err := readDataset(db)
	require.NoError(t, err)
	beforeBinding, err := readDatasetBinding(t.Context(), db)
	require.NoError(t, err)
	beforeSchema := storedSchemaSQL(t, db)
	beforeRows := applicationRows(t, db)
	conn, err := db.Conn(t.Context())
	require.NoError(t, err)
	steps := versionUpgrades()
	first := slices.IndexFunc(steps, func(step versionUpgrade) bool { return step.from == 8 })
	require.GreaterOrEqual(t, first, 0)
	steps[len(steps)-1].sql += `; SELECT no_such_function()`
	require.Error(t, runVersionUpgrades(t.Context(), conn, 8, schema175, steps[first:]))
	require.NoError(t, conn.Close())
	after, err := readDataset(db)
	require.NoError(t, err)
	require.Equal(t, before, after)
	afterBinding, err := readDatasetBinding(t.Context(), db)
	require.NoError(t, err)
	require.Equal(t, beforeBinding, afterBinding)
	version, err := readSchemaVersion(db)
	require.NoError(t, err)
	require.Equal(t, 8, version)
	require.Equal(t, beforeSchema, storedSchemaSQL(t, db))
	require.Equal(t, beforeRows, applicationRows(t, db))
	require.NoError(t, db.Close())

	s, err := OpenLockedStore(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.close()) }()
	require.Equal(t, schema177, readVersion1StoreState(t, s.db, path).fingerprint)
	binding, err := readDatasetBinding(t.Context(), s.db)
	require.NoError(t, err)
	require.Equal(t, beforeBinding, binding)
	assertMigrationRecords(t, s.db, path)
}

func TestSignInSchemaVersion8StoreDiffersFromRebuiltLayoutFixture(t *testing.T) {
	_, fixture := historicalSchemaDB(t, "abd2c040")
	_, err := fixture.Exec(migration166To167 + migration167To168 + migration168To169 + migration169To170 + migration170To171 + migration171To172 + migration172To173 + migration173To174 + migration174To175)
	require.NoError(t, err)
	schema, err := readSchema(fixture)
	require.NoError(t, err)
	require.Equal(t, schema175, schemaFingerprint(schema))
	conn, err := fixture.Conn(t.Context())
	require.NoError(t, err)
	require.NoError(t, runSchemaMigrations(t.Context(), conn, schema175, migrationsFrom(t, schema175)))
	require.NoError(t, conn.Close())

	for _, state := range version3StoreStates {
		t.Run(state.name, func(t *testing.T) {
			_, db := schemaVersion8StoreDB(t, state.origin, state.complete, state.installation, state.bound)
			before, err := readDataset(db)
			require.NoError(t, err)
			beforeBinding, err := readDatasetBinding(t.Context(), db)
			require.NoError(t, err)
			beforeRows := applicationRows(t, db)
			conn, err := db.Conn(t.Context())
			require.NoError(t, err)
			require.ErrorContains(t, runSchemaMigrations(t.Context(), conn, schema175, migrationsFrom(t, schema175)),
				"unsupported SQLite schema version 8 before upgrade")
			require.NoError(t, conn.Close())
			after, err := readDataset(db)
			require.NoError(t, err)
			require.Equal(t, before, after)
			afterBinding, err := readDatasetBinding(t.Context(), db)
			require.NoError(t, err)
			require.Equal(t, beforeBinding, afterBinding)
			require.Equal(t, beforeRows, applicationRows(t, db))
		})
	}
}

var ahpTables = []string{
	"ahp_channel_states", "ahp_clients", "ahp_journal", "ahp_records", "ahp_sessions", "ahp_tokens", "ahp_users",
}

func addAHPTables(t *testing.T, rows map[string][]string) {
	t.Helper()
	for _, table := range ahpTables {
		require.NotContains(t, rows, table)
		rows[table] = nil
	}
}

func addSignInTables(t *testing.T, rows map[string][]string) {
	t.Helper()
	require.NotContains(t, rows, "auth_sessions")
	rows["auth_sessions"] = nil
}

var version9StoreStates = version3StoreStates

func schemaVersion9StoreDB(t *testing.T, origin string, complete bool, installation string, bound bool) (string, *sql.DB) {
	t.Helper()
	path, db := schemaVersion8StoreDB(t, origin, complete, installation, bound)
	_, err := db.Exec(migration175To176 + `; PRAGMA user_version = 9`)
	require.NoError(t, err)

	schema, err := readSchema(db)
	require.NoError(t, err)
	require.Equal(t, "4af6f8b91484395ea51813a429338640f875baf8e919f972e3324d29de5afc13", schemaFingerprint(schema))
	version, err := readSchemaVersion(db)
	require.NoError(t, err)
	require.Equal(t, 9, version)
	return path, db
}

func TestSchemaVersion9StoreUpgradesInPlace(t *testing.T) {
	for _, state := range version9StoreStates {
		t.Run(state.name, func(t *testing.T) {
			path, db := schemaVersion9StoreDB(t, state.origin, state.complete, state.installation, state.bound)
			identity, err := readDataset(db)
			require.NoError(t, err)
			binding, err := readDatasetBinding(t.Context(), db)
			require.NoError(t, err)
			require.Equal(t, state.installation, binding.installation)
			require.Equal(t, state.bound, binding.state == bindingStateBound)
			rows := applicationRows(t, db)
			if state.installation != "" {
				require.Len(t, rows["controller_epochs"], 1)
				require.Len(t, rows["operation_receipts"], 1)
			}
			require.NoError(t, db.Close())

			for attempt := range 2 {
				s, err := OpenLockedStore(path)
				require.NoError(t, err)
				require.Equal(t, version1StoreState{
					fingerprint:  schema177,
					version:      SchemaVersion,
					origin:       state.origin,
					complete:     state.complete,
					canBootstrap: state.canBootstrap,
				}, readVersion1StoreState(t, s.db, path))
				upgraded, err := readDataset(s.db)
				require.NoError(t, err)
				require.Equal(t, identity, upgraded)
				upgradedBinding, err := readDatasetBinding(t.Context(), s.db)
				require.NoError(t, err)
				require.Equal(t, binding, upgradedBinding)
				require.Equal(t, rows, applicationRows(t, s.db))
				watermark, err := s.GetWriteWatermark(t.Context())
				require.NoError(t, err)
				require.Equal(t, storagev1.WriteWatermark{DatasetID: identity.id}, watermark)
				if state.origin == datasetOriginAdopted || state.complete {
					assertMigrationRecords(t, s.db, path)
				}
				checkSQLiteIntegrity(t, s.db)
				require.NoError(t, s.close())
				backups := migrationBackups(t, path)
				require.Len(t, backups, 1, "attempt %d", attempt)
				backup := rawMigrationDB(t, backups[0])
				backupSchema, err := readSchema(backup)
				require.NoError(t, err)
				require.Equal(t, schema176, schemaFingerprint(backupSchema))
				backupVersion, err := readSchemaVersion(backup)
				require.NoError(t, err)
				require.Equal(t, 9, backupVersion)
				backupIdentity, err := readDataset(backup)
				require.NoError(t, err)
				require.Equal(t, identity, backupIdentity)
				require.NoError(t, backup.Close())
			}
		})
	}
}

func TestSchemaVersion9UpgradeFailureRollsBack(t *testing.T) {
	path, db := schemaVersion9StoreDB(t, datasetOriginNew, true, "installation-a", true)
	before, err := readDataset(db)
	require.NoError(t, err)
	beforeBinding, err := readDatasetBinding(t.Context(), db)
	require.NoError(t, err)
	beforeSchema := storedSchemaSQL(t, db)
	beforeRows := applicationRows(t, db)
	conn, err := db.Conn(t.Context())
	require.NoError(t, err)
	steps := versionUpgrades()
	first := slices.IndexFunc(steps, func(step versionUpgrade) bool { return step.from == 9 })
	require.GreaterOrEqual(t, first, 0)
	steps[len(steps)-1].sql += `; SELECT no_such_function()`
	require.ErrorContains(t, runVersionUpgrades(t.Context(), conn, 9, schema176, steps[first:]), "no_such_function",
		"the failing step ran and rolled back, rather than the upgrade refusing the layout")
	require.NoError(t, conn.Close())
	after, err := readDataset(db)
	require.NoError(t, err)
	require.Equal(t, before, after)
	afterBinding, err := readDatasetBinding(t.Context(), db)
	require.NoError(t, err)
	require.Equal(t, beforeBinding, afterBinding)
	version, err := readSchemaVersion(db)
	require.NoError(t, err)
	require.Equal(t, 9, version)
	require.Equal(t, beforeSchema, storedSchemaSQL(t, db))
	require.Equal(t, beforeRows, applicationRows(t, db))
	require.NoError(t, db.Close())

	s, err := OpenLockedStore(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.close()) }()
	require.Equal(t, schema177, readVersion1StoreState(t, s.db, path).fingerprint)
	binding, err := readDatasetBinding(t.Context(), s.db)
	require.NoError(t, err)
	require.Equal(t, beforeBinding, binding)
	assertMigrationRecords(t, s.db, path)
}

func TestSchemaVersion9StoreDiffersFromRebuiltLayoutFixture(t *testing.T) {
	_, fixture := historicalSchemaDB(t, "abd2c040")
	_, err := fixture.Exec(migration166To167 + migration167To168 + migration168To169 + migration169To170 + migration170To171 +
		migration171To172 + migration172To173 + migration173To174 + migration174To175 + migration175To176)
	require.NoError(t, err)
	schema, err := readSchema(fixture)
	require.NoError(t, err)
	require.Equal(t, schema176, schemaFingerprint(schema))
	conn, err := fixture.Conn(t.Context())
	require.NoError(t, err)
	require.NoError(t, runSchemaMigrations(t.Context(), conn, schema176, migrationsFrom(t, schema176)))
	require.NoError(t, conn.Close())

	for _, state := range version9StoreStates {
		t.Run(state.name, func(t *testing.T) {
			_, db := schemaVersion9StoreDB(t, state.origin, state.complete, state.installation, state.bound)
			before, err := readDataset(db)
			require.NoError(t, err)
			beforeBinding, err := readDatasetBinding(t.Context(), db)
			require.NoError(t, err)
			beforeRows := applicationRows(t, db)
			conn, err := db.Conn(t.Context())
			require.NoError(t, err)
			require.ErrorContains(t, runSchemaMigrations(t.Context(), conn, schema176, migrationsFrom(t, schema176)),
				"unsupported SQLite schema version 9 before upgrade")
			require.NoError(t, conn.Close())
			after, err := readDataset(db)
			require.NoError(t, err)
			require.Equal(t, before, after)
			afterBinding, err := readDatasetBinding(t.Context(), db)
			require.NoError(t, err)
			require.Equal(t, beforeBinding, afterBinding)
			require.Equal(t, beforeRows, applicationRows(t, db))
		})
	}
}

func migrationsFrom(t *testing.T, from string) []schemaMigration {
	t.Helper()
	steps := schemaMigrations()
	for i, step := range steps {
		if step.from == from {
			return steps[i:]
		}
	}
	t.Fatalf("no migration starts at layout %s", from)
	return nil
}

func rawMigrationDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func seedMigrationRecords(t *testing.T, db *sql.DB, _ string) {
	t.Helper()
	_, err := db.Exec(`PRAGMA wal_autocheckpoint=0`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO results(namespace, task_name, data) VALUES(?, ?, ?), (?, ?, ?)`,
		"ns", "task", []byte("retained result"), "ns", "wal-task", []byte("in WAL"))
	require.NoError(t, err)
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
