package sqlite

import (
	"database/sql"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func seedOptionalMigrationState(t *testing.T, db *sql.DB, path, layout string) {
	t.Helper()
	storage := NewStore(db, path)
	switch layout {
	case "none":
		return
	case "lock-empty", "lock":
		_, err := db.Exec(patchVerificationWriteLockSchema)
		require.NoError(t, err)
		if layout == "lock" {
			_, err = db.Exec(`INSERT INTO patch_verification_write_lock(id) VALUES(1)`)
			require.NoError(t, err)
		}
	case "patch":
		require.NoError(t, storage.InitializePatchVerificationStore(t.Context()))
		manifest, binding, provenance, evidence := patchVerificationFixture(t)
		require.NoError(t, storage.CreatePatchVerificationRun(t.Context(), manifest, binding, provenance))
		for _, entry := range evidence {
			require.NoError(t, storage.RecordPatchVerificationEvidence(t.Context(), binding, entry))
		}
	case "kubernetes":
		require.NoError(t, storage.InitializeKubernetesValidationStore(t.Context()))
		submission, binding, evidence := kubernetesValidationFixture(t, "validation", "migration", true)
		createBoundKubernetesValidation(t, storage, submission, binding)
		require.NoError(t, storage.CreateKubernetesValidationDispatch(t.Context(), kubernetesValidationDispatchFixture(binding, evidence[0])))
		for _, entry := range evidence {
			require.NoError(t, storage.RecordPatchVerificationEvidence(t.Context(), binding, entry))
		}
	default:
		t.Fatalf("unknown optional fixture layout %q", layout)
	}
}

func optionalMigrationDefinitions(t *testing.T, db schemaQuerier) map[string]string {
	t.Helper()
	schema, err := readSchema(db)
	require.NoError(t, err)
	definitions := make(map[string]string)
	for name, definition := range schema {
		if strings.HasPrefix(name, "patch_verification_") || strings.HasPrefix(name, "validation_") {
			definitions[name] = definition
		}
	}
	return definitions
}

func optionalMigrationRows(t *testing.T, db schemaQuerier) map[string][]string {
	t.Helper()
	definitions := optionalMigrationDefinitions(t, db)
	data := make(map[string][]string)
	for _, table := range []string{
		"patch_verification_write_lock", "patch_verification_runs", "patch_verification_blobs",
		"patch_verification_evidence", "patch_verification_incidents",
		"validation_submissions", "validation_dispatches", "validation_unknown",
	} {
		if _, exists := definitions[table]; !exists {
			continue
		}
		rows, err := db.QueryContext(t.Context(), "SELECT * FROM "+table)
		require.NoError(t, err)
		columns, err := rows.Columns()
		require.NoError(t, err)
		data[table] = []string{}
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for index := range values {
				pointers[index] = &values[index]
			}
			require.NoError(t, rows.Scan(pointers...))
			encoded, err := json.Marshal(values)
			require.NoError(t, err)
			data[table] = append(data[table], string(encoded))
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		slices.Sort(data[table])
	}
	return data
}

func TestSQLiteMigrationPreservesOptionalValidationState(t *testing.T) {
	for _, revision := range []string{"0e567418", "1f18614c", "57583975"} {
		for _, layout := range []string{"lock-empty", "lock", "patch", "kubernetes"} {
			t.Run(revision+"/"+layout, func(t *testing.T) {
				path, old := historicalSchemaDB(t, revision)
				seedMigrationRecords(t, old, path)
				seedOptionalMigrationState(t, old, path, layout)
				before := storedSchemaSQL(t, old)
				optional := optionalMigrationDefinitions(t, old)
				data := optionalMigrationRows(t, old)
				for attempt := range 2 {
					storage, err := OpenLockedStore(path)
					require.NoError(t, err)
					require.NoError(t, initializeSchema(storage.db))
					require.Equal(t, optional, optionalMigrationDefinitions(t, storage.db))
					require.Equal(t, data, optionalMigrationRows(t, storage.db))
					assertMigrationRecords(t, storage.db, path)
					checkSQLiteIntegrity(t, storage.db)
					require.NoError(t, storage.close())
					backups := migrationBackups(t, path)
					require.Len(t, backups, 1, "reopening must not migrate optional tables or create another backup")
					if attempt == 0 {
						copy := rawMigrationDB(t, backups[0])
						require.Equal(t, before, storedSchemaSQL(t, copy))
						require.Equal(t, data, optionalMigrationRows(t, copy))
						assertMigrationRecords(t, copy, backups[0])
						require.NoError(t, copy.Close())
					}
				}
			})
		}
	}
}

func TestSQLiteMigrationRejectsInvalidOptionalSchemas(t *testing.T) {
	for _, test := range []struct {
		name, layout, change string
	}{
		{"missing lock", "kubernetes", `DROP TABLE patch_verification_write_lock`},
		{"missing patch table", "kubernetes", `DROP TABLE patch_verification_incidents`},
		{"missing Kubernetes table", "kubernetes", `DROP TABLE validation_submissions`},
		{"missing index", "kubernetes", `DROP INDEX validation_submissions_run`},
		{"changed definition", "kubernetes", `ALTER TABLE validation_dispatches ADD COLUMN unexpected TEXT`},
		{"changed index", "kubernetes", `DROP INDEX validation_submissions_active; CREATE INDEX validation_submissions_active ON validation_submissions(state)`},
		{"unknown optional object", "kubernetes", `CREATE TABLE validation_unknown(value TEXT); INSERT INTO validation_unknown VALUES('retained unknown row')`},
		{"unknown trigger", "kubernetes", `CREATE TRIGGER validation_unexpected AFTER UPDATE ON validation_submissions BEGIN SELECT 1; END`},
		{"damaged initialization lock", "lock", `ALTER TABLE patch_verification_write_lock ADD COLUMN unexpected TEXT`},
		{"incomplete patch initialization", "lock", patchVerificationSchemaStatements()[2]},
		{"incomplete Kubernetes initialization", "patch", kubernetesValidationTableStatements()[1]},
	} {
		t.Run(test.name, func(t *testing.T) {
			path, db := historicalSchemaDB(t, "0e567418")
			seedMigrationRecords(t, db, path)
			seedOptionalMigrationState(t, db, path, test.layout)
			_, err := db.Exec(test.change)
			require.NoError(t, err)
			before, data := storedSchemaSQL(t, db), optionalMigrationRows(t, db)
			for range 2 {
				storage, err := OpenLockedStore(path)
				require.Nil(t, storage)
				require.ErrorContains(t, err, "unsupported SQLite schema")
				require.Empty(t, migrationBackups(t, path), "invalid optional schemas must be rejected before backup")
				require.Equal(t, before, storedSchemaSQL(t, db))
				require.Equal(t, data, optionalMigrationRows(t, db))
				assertMigrationRecords(t, db, path)
			}
		})
	}
}

func TestSQLiteMigrationOptionalStateDoesNotRelaxCoreWhitelist(t *testing.T) {
	for _, test := range []struct {
		name, revision, change string
		unlocked               bool
	}{
		{name: "unsupported connector layout", revision: "b5265055"},
		{name: "unknown core object", revision: "0e567418", change: `CREATE TABLE unexpected(value TEXT)`},
		{name: "changed core definition", revision: "1f18614c", change: `DROP INDEX idx_github_mutation_records_operation_sha; CREATE INDEX idx_github_mutation_records_operation_sha ON github_mutation_records(operation)`},
		{name: "unlocked constructor", revision: "57583975", unlocked: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path, db := historicalSchemaDB(t, test.revision)
			seedMigrationRecords(t, db, path)
			seedOptionalMigrationState(t, db, path, "kubernetes")
			if test.change != "" {
				_, err := db.Exec(test.change)
				require.NoError(t, err)
			}
			before, data := storedSchemaSQL(t, db), optionalMigrationRows(t, db)
			var err error
			if test.unlocked {
				var opened *sql.DB
				opened, err = NewDB(path)
				require.Nil(t, opened)
			} else {
				var opened *Store
				opened, err = OpenLockedStore(path)
				require.Nil(t, opened)
			}
			require.ErrorContains(t, err, "unsupported SQLite schema")
			if test.revision == "b5265055" {
				require.ErrorContains(t, err, "#700")
			}
			require.Empty(t, migrationBackups(t, path))
			require.Equal(t, before, storedSchemaSQL(t, db))
			require.Equal(t, data, optionalMigrationRows(t, db))
			assertMigrationRecords(t, db, path)
		})
	}
}

func TestSQLiteMigrationRejectsOptionalDriftAfterBackup(t *testing.T) {
	for _, test := range []struct {
		name, layout, change string
	}{
		{"added lock", "none", patchVerificationWriteLockSchema},
		{"completed optional initialization", "lock", strings.Join(patchVerificationSchemaStatements(), ";")},
		{"removed Kubernetes schema", "kubernetes", `DROP TABLE validation_dispatches; DROP TABLE validation_submissions`},
		{"damaged optional definition", "kubernetes", `ALTER TABLE validation_dispatches ADD COLUMN unexpected TEXT`},
		{"same-shape SQL changed", "kubernetes", `DROP INDEX validation_submissions_patched_task; ` + strings.ToLower(kubernetesValidationIndexStatements()[0])},
		{"unknown optional object", "kubernetes", `CREATE TABLE validation_unknown(value TEXT); INSERT INTO validation_unknown VALUES('retained drift row')`},
	} {
		t.Run(test.name, func(t *testing.T) {
			path, db := historicalSchemaDB(t, "0e567418")
			seedMigrationRecords(t, db, path)
			seedOptionalMigrationState(t, db, path, test.layout)
			originalRows := optionalMigrationRows(t, db)
			schema, err := readSchema(db)
			require.NoError(t, err)
			core, optional, err := splitOptionalValidationSchema(schema)
			require.NoError(t, err)
			conn, err := db.Conn(t.Context())
			require.NoError(t, err)
			defer func() { _ = conn.Close() }()
			backup, err := backupSchemaDatabase(t.Context(), conn, path)
			require.NoError(t, err)
			_, err = conn.ExecContext(t.Context(), test.change)
			require.NoError(t, err)
			driftedSchema, err := readSchema(conn)
			require.NoError(t, err)
			driftedRows := optionalMigrationRows(t, conn)
			require.ErrorContains(t, runSchemaMigrations(t.Context(), conn, schemaFingerprint(core), optional, schemaMigrations()), "schema changed after backup")
			after, err := readSchema(conn)
			require.NoError(t, err)
			require.Equal(t, driftedSchema, after)
			require.Equal(t, driftedRows, optionalMigrationRows(t, conn))
			require.NoError(t, conn.Close())
			assertMigrationRecords(t, db, path)
			copy := rawMigrationDB(t, backup)
			require.Equal(t, originalRows, optionalMigrationRows(t, copy))
			require.NoError(t, copy.Close())
		})
	}
}

func TestSQLiteMigrationRollsBackOptionalDriftAtEveryTransition(t *testing.T) {
	kubernetesDDL := append(kubernetesValidationTableStatements(), kubernetesValidationIndexStatements()...)
	patchDDL := append([]string{patchVerificationWriteLockSchema}, patchVerificationSchemaStatements()...)
	for _, test := range []struct {
		name, change, restore string
	}{
		{"added known group", strings.Join(kubernetesDDL, ";"), `DROP TABLE validation_dispatches; DROP TABLE validation_submissions`},
		{"removed optional group", `DROP TABLE patch_verification_incidents; DROP TABLE patch_verification_evidence; DROP TABLE patch_verification_blobs; DROP TABLE patch_verification_runs; DROP TABLE patch_verification_write_lock`, strings.Join(patchDDL, ";")},
		{"damaged definition", `ALTER TABLE patch_verification_write_lock ADD COLUMN unexpected TEXT`, ""},
		{"unknown optional object", `CREATE TABLE validation_unknown(value TEXT)`, `DROP TABLE validation_unknown`},
	} {
		for index := range schemaMigrations() {
			t.Run(test.name+"/"+schemaMigrations()[index].to[:8], func(t *testing.T) {
				path, db := historicalSchemaDB(t, "0e567418")
				seedMigrationRecords(t, db, path)
				seedOptionalMigrationState(t, db, path, "patch")
				before, data := storedSchemaSQL(t, db), optionalMigrationRows(t, db)
				optional := optionalMigrationDefinitions(t, db)
				conn, err := db.Conn(t.Context())
				require.NoError(t, err)
				defer func() { _ = conn.Close() }()
				_, err = backupSchemaDatabase(t.Context(), conn, path)
				require.NoError(t, err)
				steps := schemaMigrations()
				steps[index].sql += ";" + test.change
				if index+1 < len(steps) && test.restore != "" {
					// A final-layout-only check would miss an intermediate change
					// that the following step undoes, including deleted data.
					steps[index+1].sql += ";" + test.restore
				}
				require.ErrorContains(t, runSchemaMigrations(t.Context(), conn, schema149, optional, steps), "unexpected layout")
				require.NoError(t, conn.Close())
				require.Equal(t, before, storedSchemaSQL(t, db))
				require.Equal(t, data, optionalMigrationRows(t, db))
				assertMigrationRecords(t, db, path)
				checkSQLiteIntegrity(t, db)
			})
		}
	}
}

func TestSQLiteMigrationOptionalStateRollbackAndRetry(t *testing.T) {
	for _, interruption := range []string{"failure", "crash"} {
		t.Run(interruption, func(t *testing.T) {
			path, db := historicalSchemaDB(t, "0e567418")
			seedMigrationRecords(t, db, path)
			seedOptionalMigrationState(t, db, path, "kubernetes")
			before, data := storedSchemaSQL(t, db), optionalMigrationRows(t, db)
			optional := optionalMigrationDefinitions(t, db)
			interruptMigration(t, db, path, interruption)
			require.Equal(t, before, storedSchemaSQL(t, db))
			require.Equal(t, data, optionalMigrationRows(t, db))
			assertMigrationRecords(t, db, path)
			require.Len(t, migrationBackups(t, path), 1)
			storage, err := OpenLockedStore(path)
			require.NoError(t, err)
			require.Equal(t, optional, optionalMigrationDefinitions(t, storage.db))
			require.Equal(t, data, optionalMigrationRows(t, storage.db))
			assertMigrationRecords(t, storage.db, path)
			checkSQLiteIntegrity(t, storage.db)
			require.NoError(t, storage.close())
			backups := migrationBackups(t, path)
			require.Len(t, backups, 2)
			for _, backup := range backups {
				copy := rawMigrationDB(t, backup)
				require.Equal(t, before, storedSchemaSQL(t, copy))
				require.Equal(t, data, optionalMigrationRows(t, copy))
				require.NoError(t, copy.Close())
			}
		})
	}
}
