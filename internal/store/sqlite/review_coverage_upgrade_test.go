package sqlite

import (
	"database/sql"
	"encoding/json"
	"slices"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

func schemaVersion9StoreDB(t *testing.T, origin string, complete bool, installation string, bound bool) (string, *sql.DB) {
	t.Helper()
	path, db := schemaVersion8StoreDB(t, origin, complete, installation, bound)
	_, err := db.Exec(migration175To176 + `; PRAGMA user_version = 9`)
	require.NoError(t, err)

	schema, err := readSchema(db)
	require.NoError(t, err)
	// A literal digest, independent of schema176, as in historicalSchemaDB.
	require.Equal(t, "4af6f8b91484395ea51813a429338640f875baf8e919f972e3324d29de5afc13", schemaFingerprint(schema))
	version, err := readSchemaVersion(db)
	require.NoError(t, err)
	require.Equal(t, 9, version)
	return path, db
}

// seedVersion9ReviewSlice writes a reviewed slice through SQL the version 9
// layout accepts, so the upgrade must carry a real row.
func seedVersion9ReviewSlice(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO security_review_slices
		(id, namespace, repository_scan, source, title, status, review_context_hash, last_scan_run_id)
		VALUES ('slice_api', 'ns', 'scan', 'deterministic', 'API', 'reviewed', 'sha256:context', 'scan_run')`)
	require.NoError(t, err)
}

// addReviewCoverageColumns records the defaults the version 9 to 10 step gives
// every existing review slice in rows, the application rows of a store before
// that upgrade: no coverage and no reviewed files.
func addReviewCoverageColumns(t *testing.T, rows map[string][]string) {
	t.Helper()
	if len(rows["security_review_slices"]) == 0 {
		return
	}
	upgraded := make([]string, 0, len(rows["security_review_slices"]))
	for _, row := range rows["security_review_slices"] {
		var values []any
		require.NoError(t, json.Unmarshal([]byte(row), &values))
		encoded, err := json.Marshal(append(values, "", "[]"))
		require.NoError(t, err)
		upgraded = append(upgraded, string(encoded))
	}
	sort.Strings(upgraded)
	rows["security_review_slices"] = upgraded
}

// A version 9 store upgrades in place: its review slices gain empty coverage,
// which supports no absence-based resolution until they are reviewed again,
// and it keeps its identity row, binding, key bootstrap state, controller
// epoch, operation receipts and every record. A second open is current and
// does not back up again.
func TestReviewCoverageSchemaVersion9StoreUpgradesInPlace(t *testing.T) {
	for _, state := range version3StoreStates {
		t.Run(state.name, func(t *testing.T) {
			path, db := schemaVersion9StoreDB(t, state.origin, state.complete, state.installation, state.bound)
			// Application rows end a store's key bootstrap, so only seed a
			// slice where bootstrap no longer applies.
			seeded := !state.canBootstrap
			if seeded {
				seedVersion9ReviewSlice(t, db)
			}
			identity, err := readDataset(db)
			require.NoError(t, err)
			binding, err := readDatasetBinding(t.Context(), db)
			require.NoError(t, err)
			require.Equal(t, state.installation, binding.installation)
			require.Equal(t, state.bound, binding.state == bindingStateBound)
			rows := applicationRows(t, db)
			if seeded {
				require.Len(t, rows["security_review_slices"], 1)
			}
			addReviewCoverageColumns(t, rows)
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
				if seeded {
					reviewSlice, err := s.GetReviewSlice(t.Context(), "ns", "scan", "slice_api")
					require.NoError(t, err)
					require.Equal(t, "reviewed", reviewSlice.Status)
					require.Empty(t, reviewSlice.Coverage)
					require.Empty(t, reviewSlice.ReviewedFiles)
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

// A failed version step leaves the version 9 store unchanged, including its
// binding and records, and the next open upgrades it.
func TestReviewCoverageSchemaVersion9UpgradeFailureRollsBack(t *testing.T) {
	path, db := schemaVersion9StoreDB(t, datasetOriginNew, true, "installation-a", true)
	seedVersion9ReviewSlice(t, db)
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
	// The final step fails after its column changes, so they roll back.
	steps[len(steps)-1].sql += `; SELECT no_such_function()`
	require.Error(t, runVersionUpgrades(t.Context(), conn, 9, schema176, steps[first:]))
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

// A fixture rebuilt from historical DDL plus every step through 175-to-176.sql
// has the version 9 layout but no identity row. It takes the version 0 path
// and is adopted, while every real version 9 store is refused there and takes
// the version path, as for earlier versions.
func TestReviewCoverageSchemaVersion9StoreDiffersFromRebuiltLayoutFixture(t *testing.T) {
	_, fixture := historicalSchemaDB(t, "abd2c040")
	_, err := fixture.Exec(migration166To167 + migration167To168 + migration168To169 + migration169To170 + migration170To171 + migration171To172 + migration172To173 + migration173To174 + migration174To175 + migration175To176)
	require.NoError(t, err)
	schema, err := readSchema(fixture)
	require.NoError(t, err)
	require.Equal(t, schema176, schemaFingerprint(schema))
	conn, err := fixture.Conn(t.Context())
	require.NoError(t, err)
	require.NoError(t, runSchemaMigrations(t.Context(), conn, schema176, migrationsFrom(t, schema176)))
	require.NoError(t, conn.Close())

	for _, state := range version3StoreStates {
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
