package sqlite

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/store"
	"github.com/stretchr/testify/require"
)

func loadPriorSchemaFixture(t *testing.T) string {
	t.Helper()
	path, db := historicalSchemaDB(t, "abd2c040")
	seedMigrationRecords(t, db, path)
	s := NewStore(db, path)
	s.snapshotCipher = testSnapshotCipher(t) // Produce pre-metadata ciphertext with the original format.
	body := []byte(`{"prompt":"retained encrypted prompt"}`)
	require.NoError(t, s.PersistAgentExecutionSnapshot(t.Context(), store.AgentExecutionSnapshot{
		TaskUID: "fixture-task-uid", Digest: store.CanonicalAgentExecutionSnapshotDigest(body), SchemaVersion: 1, Body: body,
	}))
	require.NoError(t, s.PutConnectorCredential(t.Context(), testCredentialRef(), store.ConnectorCredential{AccessToken: "fixture-token-not-a-credential"}))
	require.NoError(t, s.SaveArtifact(t.Context(), "fixture", "task", "proof.txt", "text/plain", []byte("retained artifact")))
	_, err := s.StageNativeSessionImport(t.Context(), nativeImportFixture(t, "imported", "retained native history"))
	require.NoError(t, err)
	require.NoError(t, db.Close())
	return path
}

// Capture every application/control/artifact table, not just records selected
// for round-trip assertions. No plaintext from encrypted rows is emitted.
func applicationRows(t *testing.T, db *sql.DB) map[string][]string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_schema WHERE type='table' AND name NOT GLOB 'sqlite_*' AND name <> 'store_dataset' ORDER BY name`)
	require.NoError(t, err)
	var tables []string
	for rows.Next() {
		var table string
		require.NoError(t, rows.Scan(&table))
		tables = append(tables, table)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	result := map[string][]string{}
	for _, table := range tables {
		rows, err := db.Query(`SELECT * FROM "` + strings.ReplaceAll(table, `"`, `""`) + `"`)
		require.NoError(t, err)
		columns, err := rows.Columns()
		require.NoError(t, err)
		result[table] = nil
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			require.NoError(t, rows.Scan(pointers...))
			encoded, err := json.Marshal(values)
			require.NoError(t, err)
			result[table] = append(result[table], string(encoded))
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		sort.Strings(result[table])
	}
	return result
}

func TestDatasetAdoptsPinnedPopulatedSchemaWithoutDataLoss(t *testing.T) {
	path := loadPriorSchemaFixture(t)
	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	before := applicationRows(t, raw)
	require.NoError(t, raw.Close())
	// The upgrade drops the empty harness v1 tables; every other table is unchanged.
	for _, retired := range []string{"harness_v1_attempts", "runtime_sessions"} {
		require.Contains(t, before, retired)
		require.Empty(t, before[retired])
		delete(before, retired)
	}
	s, err := OpenLockedStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	info, err := s.DatasetInfo()
	require.NoError(t, err)
	require.NotEmpty(t, info.ID)
	require.False(t, info.CanBootstrapSnapshotKey)
	require.Equal(t, before, applicationRows(t, s.db))
	var version int
	require.NoError(t, s.db.QueryRow(`PRAGMA user_version`).Scan(&version))
	require.Equal(t, SchemaVersion, version)
	require.NoError(t, s.SetAgentExecutionSnapshotCipher(testSnapshotCipher(t)))
	body := []byte(`{"prompt":"retained encrypted prompt"}`)
	snapshot, err := s.GetAgentExecutionSnapshot(t.Context(), store.AgentExecutionSnapshotKey{TaskUID: "fixture-task-uid", Digest: store.CanonicalAgentExecutionSnapshotDigest(body)})
	require.NoError(t, err)
	require.True(t, bytes.Equal(body, snapshot.Body), "snapshot plaintext changed")
	credential, err := s.GetConnectorCredential(t.Context(), testCredentialRef())
	require.NoError(t, err)
	require.True(t, credential.AccessToken == "fixture-token-not-a-credential", "connector plaintext changed")
	data, contentType, err := s.GetArtifact(t.Context(), "fixture", "task", "proof.txt")
	require.NoError(t, err)
	require.Equal(t, "retained artifact", string(data))
	require.Equal(t, "text/plain", contentType)
	session, err := s.GetSession(t.Context(), "ns", "retained-session")
	require.NoError(t, err)
	require.Len(t, session.Messages, 1)
	require.Equal(t, "retained conversation", session.Messages[0].Content)
	require.NoError(t, s.SaveResult(t.Context(), "fixture", "new-task", []byte("new result")))
	require.NoError(t, s.Close())
	reopened, err := OpenLockedStore(path)
	require.NoError(t, err)
	defer func() { _ = reopened.Close() }()
	again, err := reopened.DatasetInfo()
	require.NoError(t, err)
	require.Equal(t, info, again)
	require.NoError(t, reopened.SetAgentExecutionSnapshotCipher(testSnapshotCipher(t)))
	data, err = reopened.GetResult(t.Context(), "fixture", "new-task")
	require.NoError(t, err)
	require.Equal(t, "new result", string(data))
}

func TestDatasetFreshAndInterruptedBootstrap(t *testing.T) {
	for _, existingFile := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "zero-byte"}[existingFile], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "store.db")
			if existingFile {
				require.NoError(t, os.WriteFile(path, nil, 0o600))
			}
			s, err := OpenLockedStore(path)
			require.NoError(t, err)
			first, err := s.DatasetInfo()
			require.NoError(t, err)
			require.True(t, first.CanBootstrapSnapshotKey)
			require.NoError(t, s.Close())
			s, err = OpenLockedStore(path)
			require.NoError(t, err)
			defer func() { _ = s.Close() }()
			again, err := s.DatasetInfo()
			require.NoError(t, err)
			require.Equal(t, first, again)
			require.NoError(t, s.SetAgentExecutionSnapshotCipher(testSnapshotCipher(t)))
			complete, err := s.DatasetInfo()
			require.NoError(t, err)
			require.Equal(t, first.ID, complete.ID)
			require.False(t, complete.CanBootstrapSnapshotKey)
			require.NoError(t, s.Close())
			s, err = OpenLockedStore(path)
			require.NoError(t, err)
			defer func() { _ = s.Close() }()
			again, err = s.DatasetInfo()
			require.NoError(t, err)
			require.Equal(t, complete, again)
		})
	}
}

// immutable=1 reads only the main database, without replaying or checkpointing
// its WAL. This proves metadata has crossed the startup synchronization boundary
// while the production store is still open, rather than on graceful close.
func requireCheckpointedDataset(t *testing.T, path, id string, complete bool) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?immutable=1")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	var version, actualComplete int
	var actualID string
	require.NoError(t, db.QueryRow(`PRAGMA user_version`).Scan(&version))
	require.Equal(t, SchemaVersion, version)
	require.NoError(t, db.QueryRow(`SELECT dataset_id, bootstrap_complete FROM store_dataset WHERE singleton = 1`).Scan(&actualID, &actualComplete))
	require.Equal(t, id, actualID)
	require.Equal(t, complete, actualComplete == 1)
}

func TestDatasetMetadataSynchronizedBeforeStartupReturns(t *testing.T) {
	for _, adopted := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "adopted"}[adopted], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "store.db")
			if adopted {
				var prior *sql.DB
				path, prior = historicalSchemaDB(t, "abd2c040")
				require.NoError(t, prior.Close())
			}
			s, err := OpenLockedStore(path)
			require.NoError(t, err)
			defer func() { _ = s.Close() }()
			info, err := s.DatasetInfo()
			require.NoError(t, err)
			requireCheckpointedDataset(t, path, info.ID, false)
			require.NoError(t, s.SetAgentExecutionSnapshotCipher(testSnapshotCipher(t)))
			requireCheckpointedDataset(t, path, info.ID, true)
			var mode int
			require.NoError(t, s.db.QueryRow(`PRAGMA synchronous`).Scan(&mode))
			require.Equal(t, 1, mode, "ordinary writes retain NORMAL synchronous mode")
		})
	}
}

func TestDatasetCompletionCheckpointBusyFailsClosedAndRetries(t *testing.T) {
	s := setupDiskStore(t)
	info, err := s.DatasetInfo()
	require.NoError(t, err)
	require.NoError(t, s.SaveResult(t.Context(), "ns", "task", []byte("retained")))
	reader, err := sql.Open("sqlite", s.dbPath)
	require.NoError(t, err)
	defer func() { _ = reader.Close() }()
	tx, err := reader.Begin()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	var complete int
	require.NoError(t, tx.QueryRow(`SELECT bootstrap_complete FROM store_dataset`).Scan(&complete))
	require.Zero(t, complete)
	_, err = s.db.Exec(`PRAGMA busy_timeout=20`)
	require.NoError(t, err)
	err = s.SetAgentExecutionSnapshotCipher(testSnapshotCipher(t))
	require.ErrorContains(t, err, "checkpoint incomplete")
	require.Nil(t, s.snapshotCipher, "consumers must not receive an activated store before synchronization")
	requireCheckpointedDataset(t, s.dbPath, info.ID, false)
	require.NoError(t, tx.Rollback())
	// The first completion transaction committed. A retry must synchronize even
	// when its read already observes bootstrap_complete=1.
	require.NoError(t, s.SetAgentExecutionSnapshotCipher(testSnapshotCipher(t)))
	requireCheckpointedDataset(t, s.dbPath, info.ID, true)
}

func TestDatasetUnbootstrappedDataRequiresExistingKey(t *testing.T) {
	for name, statement := range map[string]string{
		"application":         `INSERT INTO results(namespace,task_name,data) VALUES('ns','task',X'01')`,
		"control":             `INSERT INTO task_job_revocations(namespace,task_uid,job_uid) VALUES('ns','task','job')`,
		"connector tombstone": `INSERT INTO connector_credential_tombstones(connection_uid,deleted_at) VALUES('uid',CURRENT_TIMESTAMP)`,
		"cleanup":             `INSERT INTO task_data_cleanup_generations(namespace,generation) VALUES('ns',1)`,
	} {
		t.Run(name, func(t *testing.T) {
			s := setupDiskStore(t)
			_, err := s.db.Exec(statement)
			require.NoError(t, err)
			info, err := s.DatasetInfo()
			require.NoError(t, err)
			require.False(t, info.CanBootstrapSnapshotKey)
		})
	}
}

func TestDatasetRejectsInvalidMetadataAndVersionsIntact(t *testing.T) {
	for name, statement := range map[string]string{
		"newer":               `PRAGMA user_version=2`,
		"unknown":             `PRAGMA user_version=2147483647`,
		"negative":            `PRAGMA user_version=-1`,
		"version reset":       `PRAGMA user_version=0`,
		"missing table":       `DROP TABLE store_dataset`,
		"missing identity":    `DELETE FROM store_dataset`,
		"malformed identity":  `UPDATE store_dataset SET dataset_id='invalid'`,
		"nil identity":        `UPDATE store_dataset SET dataset_id='00000000-0000-0000-0000-000000000000'`,
		"bad origin":          `PRAGMA ignore_check_constraints=ON; UPDATE store_dataset SET origin='unknown'`,
		"bad bootstrap state": `PRAGMA ignore_check_constraints=ON; UPDATE store_dataset SET bootstrap_complete=2`,
		"extra identity":      `PRAGMA ignore_check_constraints=ON; INSERT INTO store_dataset VALUES(2,'22222222-2222-4222-8222-222222222222','new',0)`,
	} {
		t.Run(name, func(t *testing.T) {
			s := setupDiskStore(t)
			path := s.dbPath
			require.NoError(t, s.SaveResult(t.Context(), "ns", "task", []byte("retained")))
			_, err := s.db.Exec(statement)
			require.NoError(t, err)
			beforeSchema := storedSchemaSQL(t, s.db)
			beforeData := applicationRows(t, s.db)
			var version int
			require.NoError(t, s.db.QueryRow(`PRAGMA user_version`).Scan(&version))
			require.NoError(t, s.Close())
			for range 2 {
				rejected, err := OpenLockedStore(path)
				require.Error(t, err)
				require.Nil(t, rejected)
				raw, err := sql.Open("sqlite", path)
				require.NoError(t, err)
				require.Equal(t, beforeSchema, storedSchemaSQL(t, raw))
				require.Equal(t, beforeData, applicationRows(t, raw))
				var after int
				require.NoError(t, raw.QueryRow(`PRAGMA user_version`).Scan(&after))
				require.Equal(t, version, after)
				require.NoError(t, raw.Close())
			}
		})
	}
}

func TestDatasetRejectsCorruptFileWithoutReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.db")
	data := []byte("this is not a SQLite database")
	require.NoError(t, os.WriteFile(path, data, 0o600))
	s, err := OpenLockedStore(path)
	require.Error(t, err)
	require.Nil(t, s)
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, data, after)
}

func TestDatasetFreshnessUsesTableTypeNotSQLFormatting(t *testing.T) {
	s := setupDiskStore(t)
	replaceSchemaObject(t, s.db, "results", "CREATE TABLE results (", "create\n table \"results\"(")
	require.NoError(t, s.SaveResult(t.Context(), "ns", "task", []byte("retained")))
	info, err := s.DatasetInfo()
	require.NoError(t, err)
	require.False(t, info.CanBootstrapSnapshotKey)
}

func TestDatasetAdoptedEmptyLayoutStillRequiresExistingKey(t *testing.T) {
	path := loadPriorSchemaFixture(t)
	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	for table := range applicationRows(t, raw) {
		_, err := raw.Exec(`DELETE FROM "` + table + `"`)
		require.NoError(t, err)
	}
	require.NoError(t, raw.Close())
	s, err := OpenLockedStore(path)
	require.NoError(t, err)
	defer func() { _ = s.Close() }()
	info, err := s.DatasetInfo()
	require.NoError(t, err)
	require.False(t, info.CanBootstrapSnapshotKey)
}
