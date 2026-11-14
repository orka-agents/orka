package sqlite

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/store"
)

// schemaVersion3StoreDB builds a schema version 3 store along the real release
// chain: a version 2 store plus 169-to-170.sql.
func schemaVersion3StoreDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path, db := schemaVersion2StoreDB(t, datasetOriginNew, true, "installation-a")
	_, err := db.Exec(migration169To170 + `; PRAGMA user_version = 3`)
	require.NoError(t, err)
	schema, err := readSchema(db)
	require.NoError(t, err)
	// A literal digest, independent of schema170, as in historicalSchemaDB.
	require.Equal(t, "7d46e41ac2bf6265fa94eeaed0c70f221d407213580bec9a267bd45ee5419bf7", schemaFingerprint(schema))
	return path, db
}

func seedVersion3WorkAction(t *testing.T, db *sql.DB, id, monitor, dedupe, status string, created time.Time) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO work_actions(id, monitor_namespace, monitor_name, dedupe_key, status, created_at, updated_at)
		VALUES(?, 'demo', ?, ?, ?, ?, ?)`, id, monitor, dedupe, status, created, created)
	require.NoError(t, err)
}

// Version 3 let another active action share a retry_pending action's dedupe
// key. The upgrade cancels each redundant retry_pending action, keeps the
// newest when only retry_pending actions share a key, and then enforces the
// key for retry_pending actions too.
func TestSchemaVersion3UpgradeMakesRetryPendingOwnItsDedupeKey(t *testing.T) {
	path, db := schemaVersion3StoreDB(t)
	older := time.Date(2026, time.November, 1, 1, 0, 0, 0, time.UTC)
	newer := older.Add(time.Hour)
	seedVersion3WorkAction(t, db, "shadowed", "m", "key-live", store.RepositoryMonitorWorkActionStatusRetryPending, older)
	seedVersion3WorkAction(t, db, "live", "m", "key-live", store.RepositoryMonitorWorkActionStatusRunning, newer)
	seedVersion3WorkAction(t, db, "retry-old", "m", "key-retry", store.RepositoryMonitorWorkActionStatusRetryPending, older)
	seedVersion3WorkAction(t, db, "retry-new", "m", "key-retry", store.RepositoryMonitorWorkActionStatusRetryPending, newer)
	seedVersion3WorkAction(t, db, "solo", "m", "key-solo", store.RepositoryMonitorWorkActionStatusRetryPending, older)
	seedVersion3WorkAction(t, db, "other-monitor", "n", "key-live", store.RepositoryMonitorWorkActionStatusRetryPending, older)
	seedVersion3WorkAction(t, db, "done", "m", "key-solo", store.RepositoryMonitorWorkActionStatusSucceeded, older)
	require.NoError(t, db.Close())

	s, err := OpenLockedStore(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.close()) }()
	state := readVersion1StoreState(t, s.db, path)
	require.Equal(t, schema173, state.fingerprint)
	require.Equal(t, SchemaVersion, state.version)
	binding, err := readDatasetBinding(t.Context(), s.db)
	require.NoError(t, err)
	require.Equal(t, datasetBinding{installation: "installation-a"}, binding)

	for id, want := range map[string]string{
		"shadowed":      store.RepositoryMonitorWorkActionStatusCancelled,
		"live":          store.RepositoryMonitorWorkActionStatusRunning,
		"retry-old":     store.RepositoryMonitorWorkActionStatusCancelled,
		"retry-new":     store.RepositoryMonitorWorkActionStatusRetryPending,
		"solo":          store.RepositoryMonitorWorkActionStatusRetryPending,
		"other-monitor": store.RepositoryMonitorWorkActionStatusRetryPending,
		"done":          store.RepositoryMonitorWorkActionStatusSucceeded,
	} {
		action, err := s.GetWorkAction(t.Context(), "demo", id)
		require.NoError(t, err)
		require.Equal(t, want, action.Status, id)
		if want == store.RepositoryMonitorWorkActionStatusCancelled {
			require.Equal(t, "dedupe_superseded", action.BlockedReason, id)
			require.NotNil(t, action.CompletedAt, id)
		}
	}

	err = s.CreateWorkAction(t.Context(), &store.WorkAction{ID: "duplicate", MonitorNamespace: "demo", MonitorName: "m",
		DedupeKey: "key-solo", Status: store.RepositoryMonitorWorkActionStatusQueued})
	require.ErrorIs(t, err, store.ErrConflict, "a retry_pending action owns its dedupe key")
}
