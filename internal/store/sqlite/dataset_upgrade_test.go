package sqlite

import (
	"bytes"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/store"
	"github.com/stretchr/testify/require"
)

func TestDatasetEverySupportedUpgradePreservesRecords(t *testing.T) {
	for _, revision := range []string{"0e567418", "1f18614c", "57583975", "abd2c040", "fdb64ecd"} {
		t.Run(revision, func(t *testing.T) {
			path, db := historicalSchemaDB(t, revision)
			seedMigrationRecords(t, db, path)
			old := NewStore(db, path)
			old.snapshotCipher = testSnapshotCipher(t)
			body := []byte(`{"prompt":"synthetic retained snapshot"}`)
			key := store.AgentExecutionSnapshotKey{TaskUID: "historical-task", Digest: store.CanonicalAgentExecutionSnapshotDigest(body)}
			require.NoError(t, old.PersistAgentExecutionSnapshot(t.Context(), store.AgentExecutionSnapshot{TaskUID: key.TaskUID, Digest: key.Digest, SchemaVersion: 1, Body: body}))
			for _, summary := range []string{"first", "second"} {
				_, err := old.AppendExecutionEvent(t.Context(), &store.ExecutionEvent{Namespace: "ns", StreamType: "task", StreamID: "task", TaskName: "task", Type: events.ExecutionEventTypeTaskStarted, Summary: summary})
				require.NoError(t, err)
			}
			require.NoError(t, old.SendMessage(t.Context(), &store.Message{Namespace: "ns", FromTask: "sender", ToTask: "receiver", ParentTask: "parent", Content: "retained message"}))
			if revision != "0e567418" && revision != "1f18614c" {
				require.NoError(t, old.PutConnectorCredential(t.Context(), testCredentialRef(), store.ConnectorCredential{AccessToken: "synthetic-migration-value"}))
			}
			if revision == "abd2c040" || revision == "fdb64ecd" {
				_, err := old.StageNativeSessionImport(t.Context(), nativeImportFixture(t, "native", "retained native history"))
				require.NoError(t, err)
			}
			before := applicationRows(t, db)
			require.NoError(t, db.Close())
			s, err := OpenLockedStore(path)
			require.NoError(t, err)
			defer func() { _ = s.Close() }()
			after := applicationRows(t, s.db)
			for table, rows := range before {
				require.Equal(t, rows, after[table], table)
			}
			info, err := s.DatasetInfo()
			require.NoError(t, err)
			require.False(t, info.CanBootstrapSnapshotKey)
			require.Equal(t, SchemaVersion, info.SchemaVersion)
			require.NoError(t, s.SetAgentExecutionSnapshotCipher(testSnapshotCipher(t)))
			snapshot, err := s.GetAgentExecutionSnapshot(t.Context(), key)
			require.NoError(t, err)
			require.True(t, bytes.Equal(body, snapshot.Body), "retained snapshot changed")
			events, err := s.ListExecutionEvents(t.Context(), store.ExecutionEventFilter{Namespace: "ns", StreamType: "task", StreamID: "task"})
			require.NoError(t, err)
			require.Len(t, events, 2)
			require.EqualValues(t, 1, events[0].Seq)
			require.EqualValues(t, 2, events[1].Seq)
			messages, err := s.GetMessages(t.Context(), "ns", "receiver", "parent", false)
			require.NoError(t, err)
			require.Len(t, messages, 1)
			if revision != "0e567418" && revision != "1f18614c" {
				got, err := s.GetConnectorCredential(t.Context(), testCredentialRef())
				require.NoError(t, err)
				require.True(t, got.AccessToken == "synthetic-migration-value", "retained credential changed")
			}
			if revision == "abd2c040" || revision == "fdb64ecd" {
				got, err := s.GetNativeSession(t.Context(), "tenant", "native", "")
				require.NoError(t, err)
				require.NotEmpty(t, got.Snapshot.Data)
			}
			require.NoError(t, s.SaveResult(t.Context(), "ns", "after-upgrade", []byte("new result")))
			require.NoError(t, s.Close())
			s, err = OpenLockedStore(path)
			require.NoError(t, err)
			reopened, err := s.DatasetInfo()
			require.NoError(t, err)
			require.Equal(t, info.ID, reopened.ID)
			require.False(t, reopened.CanBootstrapSnapshotKey)
			require.Len(t, migrationBackups(t, path), 1)
		})
	}
}

func TestDatasetMetadataInsertionFailureRollsBackUpgrade(t *testing.T) {
	path, db := historicalSchemaDB(t, "abd2c040")
	seedMigrationRecords(t, db, path)
	before := applicationRows(t, db)
	beforeSchema := storedSchemaSQL(t, db)
	conn, err := db.Conn(t.Context())
	require.NoError(t, err)
	_, err = backupSchemaDatabase(t.Context(), conn, path)
	require.NoError(t, err)
	steps := schemaMigrations()[3:]
	steps[len(steps)-1].sql += `; CREATE TEMP TRIGGER fail_dataset BEFORE INSERT ON store_dataset BEGIN SELECT RAISE(ABORT,'injected dataset insert failure'); END;`
	require.ErrorContains(t, runSchemaMigrations(t.Context(), conn, schema166, steps), "injected dataset insert failure")
	require.NoError(t, conn.Close())
	require.Equal(t, beforeSchema, storedSchemaSQL(t, db))
	require.Equal(t, before, applicationRows(t, db))
	version, err := readSchemaVersion(db)
	require.NoError(t, err)
	require.Zero(t, version)
	backup := rawMigrationDB(t, migrationBackups(t, path)[0])
	require.Equal(t, before, applicationRows(t, backup))
	require.NoError(t, backup.Close())
	require.NoError(t, db.Close())
	s, err := OpenLockedStore(path)
	require.NoError(t, err)
	defer func() { _ = s.Close() }()
	info, err := s.DatasetInfo()
	require.NoError(t, err)
	require.False(t, info.CanBootstrapSnapshotKey)
}

func TestDatasetFreshCreationFailureRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.db")
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	_, err = db.Exec(`PRAGMA max_page_count=2`)
	require.NoError(t, err)
	require.Error(t, initializeSchema(db))
	schema, err := readSchema(db)
	require.NoError(t, err)
	require.Empty(t, schema)
	version, err := readSchemaVersion(db)
	require.NoError(t, err)
	require.Zero(t, version)
	_, err = db.Exec(`PRAGMA max_page_count=2147483646`)
	require.NoError(t, err)
	require.NoError(t, initializeSchema(db))
	info, err := NewStore(db, path).DatasetInfo()
	require.NoError(t, err)
	require.True(t, info.CanBootstrapSnapshotKey)
}

func TestDatasetHistoricalVersionMarkerIsNotAdopted(t *testing.T) {
	for _, revision := range []string{"0e567418", "1f18614c", "57583975", "abd2c040", "fdb64ecd"} {
		t.Run(revision, func(t *testing.T) {
			path, db := historicalSchemaDB(t, revision)
			seedMigrationRecords(t, db, path)
			before := applicationRows(t, db)
			beforeSchema := storedSchemaSQL(t, db)
			_, err := db.Exec(`PRAGMA user_version=2`)
			require.NoError(t, err)
			require.NoError(t, db.Close())
			s, err := OpenLockedStore(path)
			require.ErrorContains(t, err, "unsupported SQLite schema version")
			require.Nil(t, s)
			require.Empty(t, migrationBackups(t, path))
			raw := rawMigrationDB(t, path)
			require.Equal(t, before, applicationRows(t, raw))
			require.Equal(t, beforeSchema, storedSchemaSQL(t, raw))
			version, err := readSchemaVersion(raw)
			require.NoError(t, err)
			require.Equal(t, 2, version)
		})
	}
}
