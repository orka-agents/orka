package sqlite

import (
	"bytes"
	"encoding/json"
	"maps"
	"sort"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

// testResultKey returns the result key of a test Task whose UID is derived
// from its name.
func testResultKey(namespace, taskName string) store.ResultKey {
	return store.ResultKey{Namespace: namespace, TaskName: taskName, TaskUID: taskName + "-uid"}
}

// testArtifactOwner returns the artifact owner of a test Task whose UID is
// derived from its name.
func testArtifactOwner(namespace, taskName string) store.ArtifactOwner {
	return store.ArtifactOwner{Namespace: namespace, TaskName: taskName, TaskUID: taskName + "-uid"}
}

// upgradedApplicationRows returns the application rows before, taken from a
// store below schema version 3, as the version 3 step leaves them: results
// and artifacts gain an empty Task UID and the order stamp (0, 0), prompt
// result receipts and implementation jobs gain an empty Task UID, and the
// owner tombstone tables are new and empty. Tables before does not hold are
// left out, apart from the new tombstone tables.
func upgradedApplicationRows(t *testing.T, before map[string][]string) map[string][]string {
	t.Helper()
	after := maps.Clone(before)
	for table, added := range map[string][]any{
		"results":                {"", 0, 0},
		"artifacts":              {"", 0, 0},
		"prompt_result_receipts": {""},
		"implementation_jobs":    {""},
	} {
		rows, ok := before[table]
		if !ok {
			continue
		}
		var upgraded []string
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
	after["result_owner_tombstones"] = nil
	after["artifact_owner_tombstones"] = nil
	return after
}

// A store upgraded to schema version 3 keeps results, artifacts and prompt
// result receipts written without a Task UID. They belong to whichever Task
// holds their name: any UID of that name reads them until a write for the
// name replaces them, and the first delete for the name removes them.
func TestUnboundResultsAndArtifactsBelongToTheNameHolderAfterUpgrade(t *testing.T) {
	path, db := schemaVersion2StoreDB(t, datasetOriginNew, true)
	_, err := db.Exec(`INSERT INTO results(namespace, task_name, data) VALUES('tenant', 'task', ?)`, []byte("legacy result"))
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO artifacts(namespace, task_name, filename, content_type, size, data) VALUES
		('tenant', 'task', 'a.txt', 'text/plain', 8, ?), ('tenant', 'task', 'b.txt', 'text/plain', 8, ?)`,
		[]byte("legacy a"), []byte("legacy b"))
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO prompt_result_receipts(attempt_id, namespace, task_name, operation_id, operation_digest, data)
		VALUES('attempt-legacy', 'tenant', 'task', 'settling-1', ?, ?)`, controlTestDigest("settling-1"), []byte("legacy result"))
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO implementation_jobs(id, monitor_namespace, monitor_name, task_name, patch_artifact_id)
		VALUES('job', 'tenant', 'monitor', 'task', 'a.txt')`)
	require.NoError(t, err)
	before := applicationRows(t, db)
	require.NoError(t, db.Close())

	s, err := OpenLockedStore(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.close()) }()
	require.Equal(t, upgradedApplicationRows(t, before), applicationRows(t, s.db))
	ctx := t.Context()
	first := store.ResultKey{Namespace: "tenant", TaskName: "task", TaskUID: "uid-1"}
	second := store.ResultKey{Namespace: "tenant", TaskName: "task", TaskUID: "uid-2"}
	firstOwner := store.ArtifactOwner(first)
	secondOwner := store.ArtifactOwner(second)

	for _, key := range []store.ResultKey{first, second} {
		data, err := s.GetResult(ctx, key)
		require.NoError(t, err)
		require.Equal(t, "legacy result", string(data), "an unbound result belongs to any holder of the name")
	}
	_, err = s.GetResult(ctx, store.ResultKey{Namespace: "tenant", TaskName: "other", TaskUID: "uid-1"})
	require.ErrorIs(t, err, store.ErrNotFound)
	receipt, err := s.GetPromptResultReceipt(ctx, "attempt-legacy")
	require.NoError(t, err)
	require.Empty(t, receipt.TaskUID)
	job, err := s.GetImplementationJob(ctx, "tenant", "job")
	require.NoError(t, err)
	require.Equal(t, "task", job.TaskName)
	require.Empty(t, job.TaskUID)

	listed, err := s.ListArtifacts(ctx, secondOwner)
	require.NoError(t, err)
	require.Len(t, listed, 2)
	require.NoError(t, s.SaveArtifact(ctx, store.SaveArtifactRequest{
		Owner: firstOwner, Filename: "a.txt", ContentType: "text/markdown", Data: []byte("bound a"),
	}))
	data, contentType, err := s.GetArtifact(ctx, store.ArtifactKey{Owner: firstOwner, Filename: "a.txt"})
	require.NoError(t, err)
	require.Equal(t, "bound a", string(data))
	require.Equal(t, "text/markdown", contentType)
	_, _, err = s.GetArtifact(ctx, store.ArtifactKey{Owner: secondOwner, Filename: "a.txt"})
	require.ErrorIs(t, err, store.ErrNotFound, "a write for the name replaced the unbound file, and the bound one is the first UID's")
	data, _, err = s.GetArtifact(ctx, store.ArtifactKey{Owner: secondOwner, Filename: "b.txt"})
	require.NoError(t, err)
	require.Equal(t, "legacy b", string(data))
	listed, err = s.ListArtifacts(ctx, firstOwner)
	require.NoError(t, err)
	require.Equal(t, []string{"a.txt", "b.txt"}, artifactFilenames(listed))

	require.NoError(t, s.DeleteArtifacts(ctx, secondOwner))
	listed, err = s.ListArtifacts(ctx, firstOwner)
	require.NoError(t, err)
	require.Equal(t, []string{"a.txt"}, artifactFilenames(listed), "the first delete for the name removes its unbound files")

	require.NoError(t, s.DeleteResult(ctx, second))
	_, err = s.GetResult(ctx, first)
	require.ErrorIs(t, err, store.ErrNotFound, "the first delete for the name removes its unbound result")
	_, err = s.GetPromptResultReceipt(ctx, "attempt-legacy")
	require.ErrorIs(t, err, store.ErrNotFound, "and its unbound prompt result receipts")
}

func artifactFilenames(artifacts []store.ArtifactMetadata) []string {
	names := make([]string, 0, len(artifacts))
	for _, artifact := range artifacts {
		names = append(names, artifact.Filename)
	}
	return names
}

// A prompt result receipt saved before receipts recorded a UID matches a
// repeat that names its Task and a UID, so settling resumed after an upgrade
// does not fail.
func TestPromptResultReceiptWithoutUIDMatchesItsTask(t *testing.T) {
	s := setupTestStore(t)
	digest := controlTestDigest("settling-1")
	_, err := s.db.Exec(`INSERT INTO prompt_result_receipts(attempt_id, namespace, task_name, operation_id, operation_digest, data)
		VALUES('attempt', 'tenant', 'task', 'settling-1', ?, ?)`, digest, []byte("result"))
	require.NoError(t, err)
	receipt := store.PromptResultReceipt{
		AttemptID: "attempt", Namespace: "tenant", TaskName: "task", TaskUID: "task-uid",
		OperationID: "settling-1", OperationDigest: digest, Data: []byte("result"),
	}
	require.NoError(t, s.SavePromptResultReceipt(t.Context(), receipt))
	other := receipt
	other.TaskName = "other"
	require.ErrorIs(t, s.SavePromptResultReceipt(t.Context(), other), store.ErrDuplicateMismatch)
}

// Owner tombstones refuse writes for a deleted Task UID for the receipt
// retention, and maintenance removes them afterwards.
func TestOwnerTombstonesExpireAfterRetention(t *testing.T) {
	s := setupDiskStore(t)
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	s.clock = func() time.Time { return now }
	ctx := t.Context()
	key := testResultKey("ns", "task")
	owner := testArtifactOwner("ns", "task")
	require.NoError(t, s.DeleteResult(ctx, key))
	require.NoError(t, s.DeleteArtifacts(ctx, owner))

	now = now.Add(ownerTombstoneRetention - time.Second)
	require.NoError(t, s.pruneExpiredOwnerTombstones(ctx))
	require.ErrorIs(t, s.SaveResult(ctx, key, []byte("late")), store.ErrFenced)
	require.ErrorIs(t, s.SaveArtifact(ctx, store.SaveArtifactRequest{Owner: owner, Filename: "a.txt", Data: []byte("late")}), store.ErrFenced)

	// Deleting again refreshes the tombstone.
	require.NoError(t, s.DeleteArtifacts(ctx, owner))
	now = now.Add(2 * time.Second)
	require.NoError(t, s.pruneExpiredOwnerTombstones(ctx))
	require.NoError(t, s.SaveResult(ctx, key, []byte("after retention")))
	require.ErrorIs(t, s.SaveArtifact(ctx, store.SaveArtifactRequest{Owner: owner, Filename: "a.txt", Data: []byte("late")}), store.ErrFenced)
	var remaining int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM result_owner_tombstones`).Scan(&remaining))
	require.Zero(t, remaining)
}

// A write whose envelope sequence does not fit SQLite's signed integers is
// refused rather than stored with a stamp that orders wrongly.
func TestOrderStampRefusesASequenceSQLiteCannotStore(t *testing.T) {
	s := setupTestStore(t)
	dataset := advanceTestEpoch(t, s, testWriteFence)
	key := testResultKey("ns", "task")
	ctx, envelope := testEnvelope(t, t.Context(), dataset, "SaveResult", time.Now().UTC(), key, []byte("result"))
	envelope.Sequence = 1 << 63
	err := s.SaveResult(storagev1.WithWriteEnvelope(ctx, envelope), key, []byte("result"))
	require.ErrorIs(t, err, store.ErrValidation)
	_, err = s.GetResult(t.Context(), key)
	require.ErrorIs(t, err, store.ErrNotFound)
}
