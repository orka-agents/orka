/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package sqlite

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/store"
)

// A run's input holds transcripts until the run finishes. Its begin receipt
// keeps only the run's key, so finishing the run leaves no copy behind, and a
// replay returns the run as it is now.
func TestBeginMemoryConsolidationRunReceiptKeepsNoInput(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	datasetID := advanceTestEpoch(t, s, testWriteFence)
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	marker := []byte(`{"transcript":"secret-transcript-marker"}`)
	newRun := func() *store.MemoryConsolidationRun {
		return &store.MemoryConsolidationRun{Namespace: "tenant", TaskUID: "task-uid", TaskName: "consolidate", Mode: "DryRun",
			Status: store.MemoryConsolidationRunning, WindowStart: at.Add(-time.Hour), WindowEnd: at, Input: marker, CreatedAt: at}
	}
	run := newRun()
	beginCtx, envelope := testEnvelope(t, ctx, datasetID, "BeginMemoryConsolidationRun", time.Now().UTC(), run)
	require.NoError(t, s.BeginMemoryConsolidationRun(beginCtx, run))
	require.Equal(t, marker, run.Input)

	var receipt []byte
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT result FROM operation_receipts WHERE operation_id = ?`,
		envelope.OperationID).Scan(&receipt))
	require.False(t, bytes.Contains(receipt, []byte("secret-transcript-marker")), "the receipt keeps no input: %s", receipt)

	_, err := s.CompleteMemoryConsolidationRun(ctx, store.MemoryConsolidationCompletion{Namespace: "tenant", TaskUID: "task-uid",
		Status: store.MemoryConsolidationCompleted, Report: []byte(`{}`), CompletedAt: at.Add(time.Minute)})
	require.NoError(t, err)

	replayed := newRun()
	require.NoError(t, s.BeginMemoryConsolidationRun(beginCtx, replayed))
	require.Equal(t, store.MemoryConsolidationCompleted, replayed.Status, "a replay reads the run as it is now")
	require.Empty(t, replayed.Input)
}

// A run's report holds text from every Session and memory it read. Its
// completion receipt keeps only the run's key, so releasing the run's report
// when its Task is deleted leaves no copy behind, and a replay returns the
// run as it is now.
func TestCompleteMemoryConsolidationRunReceiptKeepsNoReport(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	datasetID := advanceTestEpoch(t, s, testWriteFence)
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	require.NoError(t, s.BeginMemoryConsolidationRun(ctx, &store.MemoryConsolidationRun{Namespace: "tenant", TaskUID: "task-uid",
		TaskName: "consolidate", Mode: "DryRun", Status: store.MemoryConsolidationRunning, WindowStart: at.Add(-time.Hour),
		WindowEnd: at, Input: []byte(`{}`), CreatedAt: at}))
	completion := store.MemoryConsolidationCompletion{Namespace: "tenant", TaskUID: "task-uid", Status: store.MemoryConsolidationCompleted,
		Report: []byte(`{"summary":"secret-report-marker"}`), CompletedAt: at.Add(time.Minute)}
	completeCtx, envelope := testEnvelope(t, ctx, datasetID, "CompleteMemoryConsolidationRun", time.Now().UTC(), completion)
	completed, err := s.CompleteMemoryConsolidationRun(completeCtx, completion)
	require.NoError(t, err)
	require.Contains(t, string(completed.Report), "secret-report-marker")

	var receipt []byte
	require.NoError(t, s.db.QueryRowContext(ctx, `SELECT result FROM operation_receipts WHERE operation_id = ?`,
		envelope.OperationID).Scan(&receipt))
	require.False(t, bytes.Contains(receipt, []byte("secret-report-marker")), "the receipt keeps no report: %s", receipt)

	require.NoError(t, s.ReleaseMemoryConsolidationRun(ctx, "tenant", "task-uid", true))
	replayed, err := s.CompleteMemoryConsolidationRun(completeCtx, completion)
	if err == nil {
		require.NotContains(t, string(replayed.Report), "secret-report-marker", "a replay reads the run as it is now")
	}
}
