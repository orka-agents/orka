package sqlite

import (
	"database/sql"
	"maps"
	"testing"

	"github.com/stretchr/testify/require"
)

// feedbackLedgerApplicationRows returns the application rows before, taken
// from a store below schema version 6, as the version 6 step leaves them: it
// adds the empty feedback attempt table and changes nothing else.
func feedbackLedgerApplicationRows(t *testing.T, before map[string][]string) map[string][]string {
	t.Helper()
	require.NotContains(t, before, "monitor_feedback_attempts")
	after := maps.Clone(before)
	after["monitor_feedback_attempts"] = nil
	return after
}

// schemaVersion5StoreDB returns a schema version 5 store built only from frozen
// inputs: a version 4 store from schemaVersion4StoreDB, the immutable
// 171-to-172.sql step and user_version = 5, as the version 4 to 5 step leaves
// it. It never uses the current schema constructor, so it keeps producing
// version 5 stores after the layout moves on.
func schemaVersion5StoreDB(t *testing.T, origin string, complete bool, installation string, bound bool) (string, *sql.DB) {
	t.Helper()
	path, db := schemaVersion4StoreDB(t, origin, complete, installation, bound)
	_, err := db.Exec(migration171To172 + `; PRAGMA user_version = 5`)
	require.NoError(t, err)

	schema, err := readSchema(db)
	require.NoError(t, err)
	// A literal digest, independent of schema172, as in historicalSchemaDB.
	require.Equal(t, "8d812a8681fbf1d0139be0d700ed3722d725b5bf787576c1cf002b39fa6c402e", schemaFingerprint(schema))
	version, err := readSchemaVersion(db)
	require.NoError(t, err)
	require.Equal(t, 5, version)
	return path, db
}

// The current layout is version 5 plus exactly the immutable 172-to-173.sql
// step, so the constant for it matches what the step builds.
func TestSchemaVersion6LayoutIsVersion5PlusFeedbackLedger(t *testing.T) {
	_, db := schemaVersion5StoreDB(t, datasetOriginNew, true, "", false)
	_, err := db.Exec(migration172To173)
	require.NoError(t, err)
	schema, err := readSchema(db)
	require.NoError(t, err)
	require.Equal(t, schema173, schemaFingerprint(schema))
	require.Equal(t, "2e6f43005353439f2639c8c373868ce45f545f7c24893d690690e8d3951f6c17", schema173)
}

// A version 5 store upgrades in place to the feedback ledger layout: it keeps
// its identity row, binding, key bootstrap state, controller epoch and
// operation receipts and every record, and gains an empty feedback attempt
// table. A second open is current and does not back up again.
func TestSchemaVersion5StoreUpgradesInPlace(t *testing.T) {
	for _, state := range version3StoreStates {
		t.Run(state.name, func(t *testing.T) {
			path, db := schemaVersion5StoreDB(t, state.origin, state.complete, state.installation, state.bound)
			identity, err := readDataset(db)
			require.NoError(t, err)
			binding, err := readDatasetBinding(t.Context(), db)
			require.NoError(t, err)
			require.Equal(t, state.installation, binding.installation)
			require.Equal(t, state.bound, binding.state == bindingStateBound)
			rows := feedbackLedgerApplicationRows(t, applicationRows(t, db))
			require.NoError(t, db.Close())

			for attempt := range 2 {
				s, err := OpenLockedStore(path)
				require.NoError(t, err)
				require.Equal(t, version1StoreState{
					fingerprint:  schema173,
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
				require.Equal(t, schema172, schemaFingerprint(backupSchema))
				backupVersion, err := readSchemaVersion(backup)
				require.NoError(t, err)
				require.Equal(t, 5, backupVersion)
				backupIdentity, err := readDataset(backup)
				require.NoError(t, err)
				require.Equal(t, identity, backupIdentity)
				require.NoError(t, backup.Close())
			}
		})
	}
}

// A failed feedback ledger step leaves the version 5 store unchanged,
// including its binding and records, and the next open upgrades it.
func TestSchemaVersion5UpgradeFailureRollsBack(t *testing.T) {
	path, db := schemaVersion5StoreDB(t, datasetOriginNew, true, "installation-a", true)
	before, err := readDataset(db)
	require.NoError(t, err)
	beforeBinding, err := readDatasetBinding(t.Context(), db)
	require.NoError(t, err)
	beforeSchema := storedSchemaSQL(t, db)
	beforeRows := applicationRows(t, db)
	conn, err := db.Conn(t.Context())
	require.NoError(t, err)
	steps := versionUpgrades()
	require.Equal(t, 5, steps[len(steps)-1].from)
	steps[len(steps)-1].sql += `; SELECT no_such_function()`
	require.Error(t, runVersionUpgrades(t.Context(), conn, 5, schema172, steps[len(steps)-1:]))
	require.NoError(t, conn.Close())
	after, err := readDataset(db)
	require.NoError(t, err)
	require.Equal(t, before, after)
	afterBinding, err := readDatasetBinding(t.Context(), db)
	require.NoError(t, err)
	require.Equal(t, beforeBinding, afterBinding)
	version, err := readSchemaVersion(db)
	require.NoError(t, err)
	require.Equal(t, 5, version)
	require.Equal(t, beforeSchema, storedSchemaSQL(t, db))
	require.Equal(t, beforeRows, applicationRows(t, db))
	require.NoError(t, db.Close())

	s, err := OpenLockedStore(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.close()) }()
	require.Equal(t, schema173, readVersion1StoreState(t, s.db, path).fingerprint)
	binding, err := readDatasetBinding(t.Context(), s.db)
	require.NoError(t, err)
	require.Equal(t, beforeBinding, binding)
	assertMigrationRecords(t, s.db, path)
}

// A fixture rebuilt from historical DDL plus every step through 171-to-172.sql
// has the version 5 layout but no identity row. It takes the version 0 path
// and is adopted, while every real version 5 store is refused there and takes
// the version path, as for earlier versions.
func TestSchemaVersion5StoreDiffersFromRebuiltLayoutFixture(t *testing.T) {
	_, fixture := historicalSchemaDB(t, "abd2c040")
	_, err := fixture.Exec(migration166To167 + migration167To168 + migration168To169 + migration169To170 + migration170To171 + migration171To172)
	require.NoError(t, err)
	schema, err := readSchema(fixture)
	require.NoError(t, err)
	require.Equal(t, schema172, schemaFingerprint(schema))
	conn, err := fixture.Conn(t.Context())
	require.NoError(t, err)
	require.NoError(t, runSchemaMigrations(t.Context(), conn, schema172, migrationsFrom(t, schema172)))
	require.NoError(t, conn.Close())

	for _, state := range version3StoreStates {
		t.Run(state.name, func(t *testing.T) {
			_, db := schemaVersion5StoreDB(t, state.origin, state.complete, state.installation, state.bound)
			before, err := readDataset(db)
			require.NoError(t, err)
			beforeRows := applicationRows(t, db)
			conn, err := db.Conn(t.Context())
			require.NoError(t, err)
			require.ErrorContains(t, runSchemaMigrations(t.Context(), conn, schema172, migrationsFrom(t, schema172)),
				"unsupported SQLite schema version 5 before upgrade")
			require.NoError(t, conn.Close())
			after, err := readDataset(db)
			require.NoError(t, err)
			require.Equal(t, before, after)
			require.Equal(t, beforeRows, applicationRows(t, db))
		})
	}
}
