package sqlite

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"sort"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

// stampedApplicationRows returns the application rows before, taken from a
// store below schema version 5, as the version 5 step leaves them: results and
// artifacts gain the write stamp (0, 0). Tables before does not hold are left
// out.
func stampedApplicationRows(t *testing.T, before map[string][]string) map[string][]string {
	t.Helper()
	after := maps.Clone(before)
	for _, table := range []string{"results", "artifacts"} {
		rows, ok := before[table]
		if !ok {
			continue
		}
		var stamped []string
		for _, row := range rows {
			decoder := json.NewDecoder(bytes.NewReader([]byte(row)))
			decoder.UseNumber()
			var values []any
			require.NoError(t, decoder.Decode(&values))
			encoded, err := json.Marshal(append(values, 0, 0))
			require.NoError(t, err)
			stamped = append(stamped, string(encoded))
		}
		sort.Strings(stamped)
		after[table] = stamped
	}
	return after
}

// A write envelope's sequence is unsigned, and SQLite stores the stamp as a
// signed integer. A sequence it cannot store is refused rather than wrapped
// into a stamp that orders before every other write.
func TestOrderStampRefusesASequenceSQLiteCannotStore(t *testing.T) {
	owner := store.ArtifactOwner{Namespace: "ns", TaskName: "task"}
	artifact := store.SaveArtifactRequest{Owner: owner, Filename: "out.txt", ContentType: "text/plain", Data: []byte("output")}
	for _, write := range []struct {
		method  string
		request []any
		save    func(context.Context, *Store) error
		get     func(context.Context, *Store) error
	}{{
		method:  "SaveResult",
		request: []any{"ns", "task", []byte("result")},
		save:    func(ctx context.Context, s *Store) error { return s.SaveResult(ctx, "ns", "task", []byte("result")) },
		get: func(ctx context.Context, s *Store) error {
			_, err := s.GetResult(ctx, "ns", "task")
			return err
		},
	}, {
		method:  "SaveArtifact",
		request: []any{artifact},
		save:    func(ctx context.Context, s *Store) error { return s.SaveArtifact(ctx, artifact) },
		get: func(ctx context.Context, s *Store) error {
			_, _, err := s.GetArtifact(ctx, store.ArtifactKey{Owner: owner, Filename: artifact.Filename})
			return err
		},
	}} {
		t.Run(write.method, func(t *testing.T) {
			s := setupTestStore(t)
			dataset := advanceTestEpoch(t, s, testWriteFence)
			ctx, envelope := testEnvelope(t, t.Context(), dataset, write.method, time.Now().UTC(), write.request...)
			envelope.Sequence = 1 << 63
			require.ErrorIs(t, write.save(storagev1.WithWriteEnvelope(ctx, envelope), s), store.ErrValidation)
			require.ErrorIs(t, write.get(t.Context(), s), store.ErrNotFound)
		})
	}
}

// Results and artifacts stored before the write stamp have the stamp (0, 0)
// after the upgrade, so the first stamped write for them replaces them.
func TestStampedWritesReplaceRecordsStoredBeforeTheStamp(t *testing.T) {
	path, db := schemaVersion4StoreDB(t, datasetOriginNew, true, "", false)
	_, err := db.Exec(`INSERT INTO results(namespace, task_name, data) VALUES('tenant', 'task', ?)`, []byte("earlier result"))
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO artifacts(namespace, task_name, filename, content_type, size, data)
		VALUES('tenant', 'task', 'out.txt', 'text/plain', 14, ?)`, []byte("earlier output"))
	require.NoError(t, err)
	require.NoError(t, db.Close())

	s, err := OpenLockedStore(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.close()) }()
	for _, table := range []string{"results", "artifacts"} {
		var epoch, sequence int64
		require.NoError(t, s.db.QueryRow(`SELECT write_epoch, write_seq FROM `+table+` WHERE task_name = 'task'`).Scan(&epoch, &sequence))
		require.Zero(t, epoch, table)
		require.Zero(t, sequence, table)
	}

	dataset := advanceTestEpoch(t, s, testWriteFence)
	saveCtx, saveEnvelope := testEnvelope(t, t.Context(), dataset, "SaveResult", time.Now().UTC(), "tenant", "task", []byte("later result"))
	saveEnvelope.Sequence = 1
	require.NoError(t, s.SaveResult(storagev1.WithWriteEnvelope(saveCtx, saveEnvelope), "tenant", "task", []byte("later result")))
	result, err := s.GetResult(t.Context(), "tenant", "task")
	require.NoError(t, err)
	require.Equal(t, "later result", string(result))

	owner := store.ArtifactOwner{Namespace: "tenant", TaskName: "task"}
	request := store.SaveArtifactRequest{Owner: owner, Filename: "out.txt", ContentType: "text/markdown", Data: []byte("later output")}
	artifactCtx, artifactEnvelope := testEnvelope(t, t.Context(), dataset, "SaveArtifact", time.Now().UTC(), request)
	artifactEnvelope.Sequence = 2
	require.NoError(t, s.SaveArtifact(storagev1.WithWriteEnvelope(artifactCtx, artifactEnvelope), request))
	data, contentType, err := s.GetArtifact(t.Context(), store.ArtifactKey{Owner: owner, Filename: "out.txt"})
	require.NoError(t, err)
	require.Equal(t, "later output", string(data))
	require.Equal(t, "text/markdown", contentType)
}
