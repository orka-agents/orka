package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidationSchemaReopensWithoutWrites(t *testing.T) {
	for _, test := range []struct {
		name       string
		initialize func(*Store, context.Context) error
		lockOnly   bool
	}{
		{name: "process", initialize: (*Store).InitializePatchVerificationStore},
		{name: "Kubernetes", initialize: (*Store).InitializeKubernetesValidationStore},
		{name: "interrupted", initialize: (*Store).InitializeKubernetesValidationStore, lockOnly: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "store.db")
			db, err := NewDB(path)
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			store := NewStore(db, path)
			if test.lockOnly {
				_, err = db.Exec(patchVerificationWriteLockSchema)
				require.NoError(t, err)
			} else {
				require.NoError(t, test.initialize(store, t.Context()))
			}
			require.NoError(t, store.SaveResult(t.Context(), "ns", "task", []byte("retained result")))
			before := storedSchemaSQL(t, db)
			_, err = db.Exec(`PRAGMA query_only = ON`)
			require.NoError(t, err)
			require.NoError(t, initializeSchema(db))
			require.NoError(t, db.Close())
			reopened, err := NewDB(path)
			require.NoError(t, err)
			t.Cleanup(func() { _ = reopened.Close() })
			require.Equal(t, before, storedSchemaSQL(t, reopened))
			resumed := NewStore(reopened, path)
			require.NoError(t, test.initialize(resumed, t.Context()))
			data, err := resumed.GetResult(t.Context(), "ns", "task")
			require.NoError(t, err)
			require.Equal(t, "retained result", string(data))
		})
	}
}

func TestValidationSchemaRejectsDamagedStateWithoutRewriting(t *testing.T) {
	for _, statement := range []string{
		`DROP TABLE patch_verification_write_lock`,
		`DROP TABLE patch_verification_blobs`,
		`DROP INDEX validation_submissions_patched_task`,
		`ALTER TABLE validation_dispatches ADD COLUMN unexpected TEXT`,
		`CREATE TABLE validation_unknown (value TEXT)`,
	} {
		t.Run(statement, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "store.db")
			db, err := NewDB(path)
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			require.NoError(t, NewStore(db, path).InitializeKubernetesValidationStore(t.Context()))
			_, err = db.Exec(statement)
			require.NoError(t, err)
			before := storedSchemaSQL(t, db)
			require.NoError(t, db.Close())
			rejected, err := NewDB(path)
			if rejected != nil {
				_ = rejected.Close()
			}
			require.ErrorContains(t, err, "unsupported SQLite schema")
			require.Nil(t, rejected)
			raw, err := sql.Open("sqlite", path)
			require.NoError(t, err)
			t.Cleanup(func() { _ = raw.Close() })
			require.Equal(t, before, storedSchemaSQL(t, raw))
		})
	}
}

func TestValidationSchemaVersionedUpgradePreservesOptionalState(t *testing.T) {
	path, db := schemaVersion1StoreDB(t, datasetOriginNew, true)
	storage := NewStore(db, path)
	require.NoError(t, storage.InitializeKubernetesValidationStore(t.Context()))
	schema, err := readSchema(db)
	require.NoError(t, err)
	_, optional, err := splitOptionalValidationSchema(schema)
	require.NoError(t, err)
	identity, err := readDataset(db)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	upgraded, err := OpenLockedStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = upgraded.close() })
	schema, err = readSchema(upgraded.db)
	require.NoError(t, err)
	core, retained, err := splitOptionalValidationSchema(schema)
	require.NoError(t, err)
	require.Equal(t, optional, retained)
	require.Equal(t, schema171, schemaFingerprint(core))
	currentIdentity, err := readDataset(upgraded.db)
	require.NoError(t, err)
	require.Equal(t, identity, currentIdentity)
	require.Len(t, migrationBackups(t, path), 1)
	require.NoError(t, upgraded.close())

	reopened, err := OpenLockedStore(path)
	require.NoError(t, err)
	require.NoError(t, reopened.close())
	require.Len(t, migrationBackups(t, path), 1)
}

func TestValidationSchemaVersionedUpgradeRejectsOptionalChangeAfterBackup(t *testing.T) {
	path, db := schemaVersion1StoreDB(t, datasetOriginNew, true)
	require.NoError(t, NewStore(db, path).InitializeKubernetesValidationStore(t.Context()))
	t.Cleanup(func() { _ = db.Close() })
	conn, err := db.Conn(t.Context())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	schema, err := readSchema(conn)
	require.NoError(t, err)
	core, optional, err := splitOptionalValidationSchema(schema)
	require.NoError(t, err)
	_, err = backupSchemaDatabase(t.Context(), conn, path)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `DROP TABLE patch_verification_blobs`)
	require.NoError(t, err)
	require.ErrorContains(t, runVersionUpgrades(t.Context(), conn, 1, schemaFingerprint(core), optional, versionUpgrades()),
		"schema changed after backup")
	version, err := readSchemaVersion(conn)
	require.NoError(t, err)
	require.Equal(t, 1, version)
}

func TestValidationSchemaLockedStoreMigrationAndReopen(t *testing.T) {
	for _, revision := range []string{"fresh", "0e567418", "1f18614c", "57583975"} {
		t.Run(revision, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "store.db")
			if revision != "fresh" {
				var old *sql.DB
				path, old = historicalSchemaDB(t, revision)
				seedMigrationRecords(t, old, path)
				require.NoError(t, old.Close())
			}

			storage, err := OpenLockedStore(path)
			require.NoError(t, err)
			t.Cleanup(func() { _ = storage.close() })
			require.NotContains(t, storedSchemaSQL(t, storage.db), "validation_submissions")
			if revision != "fresh" {
				assertMigrationRecords(t, storage.db, path)
			}
			require.NoError(t, storage.InitializeKubernetesValidationStore(t.Context()))
			require.NoError(t, storage.SaveResult(t.Context(), "validation", "retained", []byte("retained validation result")))
			expectedSchema := storedSchemaSQL(t, storage.db)
			backups := migrationBackups(t, path)
			if revision == "fresh" {
				require.Empty(t, backups)
			} else {
				require.Len(t, backups, 1)
			}
			require.NoError(t, storage.close())

			for range 2 {
				reopened, err := OpenLockedStore(path)
				require.NoError(t, err)
				t.Cleanup(func() { _ = reopened.close() })
				require.Equal(t, expectedSchema, storedSchemaSQL(t, reopened.db))
				require.Equal(t, backups, migrationBackups(t, path), "optional tables must not trigger another migration")
				require.NoError(t, reopened.InitializeKubernetesValidationStore(t.Context()))
				result, err := reopened.GetResult(t.Context(), "validation", "retained")
				require.NoError(t, err)
				require.Equal(t, "retained validation result", string(result))
				if revision != "fresh" {
					assertMigrationRecords(t, reopened.db, path)
				}
				checkSQLiteIntegrity(t, reopened.db)
				require.NoError(t, reopened.close())
			}
		})
	}
}
