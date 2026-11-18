package sqlite

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"maps"
	"slices"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

// consolidatedApplicationRows returns the application rows before, taken
// from a store below schema version 6, as the version 6 step leaves them:
// memories gain generation 1 and no superseding memory, and proposals gain no
// targets or sources. Tables before does not hold are left out.
func consolidatedApplicationRows(t *testing.T, before map[string][]string) map[string][]string {
	t.Helper()
	after := maps.Clone(before)
	for table, added := range map[string][]any{
		"memories":         {1, ""},
		"memory_proposals": {"[]", "[]"},
	} {
		rows, ok := before[table]
		if !ok || len(rows) == 0 {
			continue
		}
		upgraded := make([]string, 0, len(rows))
		for _, row := range rows {
			decoder := json.NewDecoder(bytes.NewReader([]byte(row)))
			decoder.UseNumber()
			var values []any
			require.NoError(t, decoder.Decode(&values))
			encoded, err := json.Marshal(append(values, added...))
			require.NoError(t, err)
			upgraded = append(upgraded, string(encoded))
		}
		sort.Strings(upgraded)
		after[table] = upgraded
	}
	return after
}

// seedVersion5Memories writes a memory and a proposal through SQL the
// version 5 layout accepts, so the upgrade has records to carry. Only a store
// whose key bootstrap finished may hold rows: rows would keep an unfinished
// one from minting its key.
func seedVersion5Memories(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO memories (id, namespace, source, content, tags_json, recalled_count)
		VALUES ('mem-upgrade', 'upgrade', 'manual', 'kept across the upgrade', '["ops"]', 2)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO memory_proposals (id, namespace, type, title, content, status)
		VALUES ('mprop-upgrade', 'upgrade', 'memory', 'Kept', 'kept proposal', 'pending')`)
	require.NoError(t, err)
}

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

// A version 5 store upgrades in place to the memory generation layout: it
// keeps its identity row, binding, key bootstrap state, controller epoch and
// operation receipts and every record, its memories start at generation 1
// and its proposals name no targets. A second open is current and does not
// back up again.
func TestSchemaVersion5StoreUpgradesInPlace(t *testing.T) {
	for _, state := range version3StoreStates {
		t.Run(state.name, func(t *testing.T) {
			path, db := schemaVersion5StoreDB(t, state.origin, state.complete, state.installation, state.bound)
			seeded := !state.canBootstrap
			if seeded {
				seedVersion5Memories(t, db)
			}
			identity, err := readDataset(db)
			require.NoError(t, err)
			binding, err := readDatasetBinding(t.Context(), db)
			require.NoError(t, err)
			rows := runTableApplicationRows(consolidatedApplicationRows(t, applicationRows(t, db)))
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
					require.Equal(t, uint64(1), memory.Generation, "an existing memory starts at generation 1")
					require.Equal(t, 2, memory.RecalledCount)
					proposal, err := s.GetMemoryProposal(t.Context(), "upgrade", "mprop-upgrade")
					require.NoError(t, err)
					require.Empty(t, proposal.Targets, "an existing proposal names no targets")
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
				require.Equal(t, schema172, schemaFingerprint(backupSchema))
				backupVersion, err := readSchemaVersion(backup)
				require.NoError(t, err)
				require.Equal(t, 5, backupVersion)
				require.NoError(t, backup.Close())
			}
		})
	}
}

// A failed memory generation step leaves the version 5 store unchanged,
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
	first := slices.IndexFunc(steps, func(step versionUpgrade) bool { return step.from == 5 })
	require.GreaterOrEqual(t, first, 0)
	// The final step fails, so every step from version 5 rolls back together.
	steps[len(steps)-1].sql += `; SELECT no_such_function()`
	require.ErrorContains(t, runVersionUpgrades(t.Context(), conn, 5, schema172, nil, steps[first:]), "no_such_function",
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
	require.Equal(t, 5, version)
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
	require.NoError(t, runSchemaMigrations(t.Context(), conn, schema172, nil, migrationsFrom(t, schema172)))
	require.NoError(t, conn.Close())

	for _, state := range version3StoreStates {
		t.Run(state.name, func(t *testing.T) {
			_, db := schemaVersion5StoreDB(t, state.origin, state.complete, state.installation, state.bound)
			before, err := readDataset(db)
			require.NoError(t, err)
			beforeRows := applicationRows(t, db)
			conn, err := db.Conn(t.Context())
			require.NoError(t, err)
			require.ErrorContains(t, runSchemaMigrations(t.Context(), conn, schema172, nil, migrationsFrom(t, schema172)),
				"unsupported SQLite schema version 5 before upgrade")
			require.NoError(t, conn.Close())
			after, err := readDataset(db)
			require.NoError(t, err)
			require.Equal(t, before, after)
			require.Equal(t, beforeRows, applicationRows(t, db))
		})
	}
}
