package sqlite

import (
	"database/sql"
	"maps"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/store"
)

// runTableApplicationRows returns the application rows before, taken from a
// store below schema version 7, as the version 7 step leaves them: with an
// empty memory consolidation run table.
func runTableApplicationRows(before map[string][]string) map[string][]string {
	after := maps.Clone(before)
	after["memory_consolidation_runs"] = nil
	return after
}

func schemaVersion6StoreDB(t *testing.T, origin string, complete bool, installation string, bound bool) (string, *sql.DB) {
	t.Helper()
	path, db := schemaVersion5StoreDB(t, origin, complete, installation, bound)
	_, err := db.Exec(migration172To173 + `; PRAGMA user_version = 6`)
	require.NoError(t, err)

	schema, err := readSchema(db)
	require.NoError(t, err)
	// A literal digest, independent of schema174, as in historicalSchemaDB.
	require.Equal(t, "44db6c191e5b2d8c2570a251762e50e89231b2e61ad7a5f40a42d6d5784ed75b", schemaFingerprint(schema))
	version, err := readSchemaVersion(db)
	require.NoError(t, err)
	require.Equal(t, 6, version)
	return path, db
}

// A version 6 store upgrades in place to the memory consolidation run
// layout: it keeps its identity row, binding, key bootstrap state,
// controller epoch, operation receipts and every record, and gains an empty
// run table. A second open is current and does not back up again.
func TestSchemaVersion6StoreUpgradesInPlace(t *testing.T) {
	for _, state := range version3StoreStates {
		t.Run(state.name, func(t *testing.T) {
			path, db := schemaVersion6StoreDB(t, state.origin, state.complete, state.installation, state.bound)
			seeded := !state.canBootstrap
			if seeded {
				seedVersion5Memories(t, db)
			}
			identity, err := readDataset(db)
			require.NoError(t, err)
			binding, err := readDatasetBinding(t.Context(), db)
			require.NoError(t, err)
			rows := runTableApplicationRows(applicationRows(t, db))
			require.NoError(t, db.Close())

			for attempt := range 2 {
				s, err := OpenLockedStore(path)
				require.NoError(t, err)
				require.Equal(t, version1StoreState{
					fingerprint:  schema174,
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
				if seeded {
					memory, err := s.GetMemory(t.Context(), "upgrade", "mem-upgrade")
					require.NoError(t, err)
					require.Equal(t, 2, memory.RecalledCount, "existing memories are kept")
					_, err = s.GetMemoryConsolidationRun(t.Context(), "upgrade", "uid")
					require.ErrorIs(t, err, store.ErrNotFound, "a store starts with no consolidation runs")
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
				require.Equal(t, schema173, schemaFingerprint(backupSchema))
				backupVersion, err := readSchemaVersion(backup)
				require.NoError(t, err)
				require.Equal(t, 6, backupVersion)
				require.NoError(t, backup.Close())
			}
		})
	}
}

// A failed memory consolidation run step leaves the version 6 store unchanged,
// including its binding and records, and the next open upgrades it.
func TestSchemaVersion6UpgradeFailureRollsBack(t *testing.T) {
	path, db := schemaVersion6StoreDB(t, datasetOriginNew, true, "installation-a", true)
	before, err := readDataset(db)
	require.NoError(t, err)
	beforeBinding, err := readDatasetBinding(t.Context(), db)
	require.NoError(t, err)
	beforeSchema := storedSchemaSQL(t, db)
	beforeRows := applicationRows(t, db)
	conn, err := db.Conn(t.Context())
	require.NoError(t, err)
	steps := versionUpgrades()
	require.Equal(t, 6, steps[len(steps)-1].from)
	steps[len(steps)-1].sql += `; SELECT no_such_function()`
	require.ErrorContains(t, runVersionUpgrades(t.Context(), conn, 6, schema173, nil, steps[len(steps)-1:]), "no_such_function",
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
	require.Equal(t, 6, version)
	require.Equal(t, beforeSchema, storedSchemaSQL(t, db))
	require.Equal(t, beforeRows, applicationRows(t, db))
	require.NoError(t, db.Close())

	s, err := OpenLockedStore(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.close()) }()
	require.Equal(t, schema174, readVersion1StoreState(t, s.db, path).fingerprint)
	binding, err := readDatasetBinding(t.Context(), s.db)
	require.NoError(t, err)
	require.Equal(t, beforeBinding, binding)
	assertMigrationRecords(t, s.db, path)
}

// A fixture rebuilt from historical DDL plus every step through 172-to-173.sql
// has the version 6 layout but no identity row. It takes the version 0 path
// and is adopted, while every real version 6 store is refused there and takes
// the version path, as for earlier versions.
func TestSchemaVersion6StoreDiffersFromRebuiltLayoutFixture(t *testing.T) {
	_, fixture := historicalSchemaDB(t, "abd2c040")
	_, err := fixture.Exec(migration166To167 + migration167To168 + migration168To169 + migration169To170 + migration170To171 + migration171To172 + migration172To173)
	require.NoError(t, err)
	schema, err := readSchema(fixture)
	require.NoError(t, err)
	require.Equal(t, schema173, schemaFingerprint(schema))
	conn, err := fixture.Conn(t.Context())
	require.NoError(t, err)
	require.NoError(t, runSchemaMigrations(t.Context(), conn, schema173, nil, migrationsFrom(t, schema173)))
	require.NoError(t, conn.Close())

	for _, state := range version3StoreStates {
		t.Run(state.name, func(t *testing.T) {
			_, db := schemaVersion6StoreDB(t, state.origin, state.complete, state.installation, state.bound)
			before, err := readDataset(db)
			require.NoError(t, err)
			beforeRows := applicationRows(t, db)
			conn, err := db.Conn(t.Context())
			require.NoError(t, err)
			require.ErrorContains(t, runSchemaMigrations(t.Context(), conn, schema173, nil, migrationsFrom(t, schema173)),
				"unsupported SQLite schema version 6 before upgrade")
			require.NoError(t, conn.Close())
			after, err := readDataset(db)
			require.NoError(t, err)
			require.Equal(t, before, after)
			require.Equal(t, beforeRows, applicationRows(t, db))
		})
	}
}
