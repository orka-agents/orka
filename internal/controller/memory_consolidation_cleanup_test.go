/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

// A consolidation Task that ends or is deleted without finishing its run
// leaves no transcripts behind; a finished run, and other Tasks, are left
// alone.
func TestAbandonMemoryConsolidationRunClearsAnUnfinishedRun(t *testing.T) {
	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "consolidation.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	runs := sqlite.NewStore(db, "test")
	ctx := context.Background()
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	begin := func(uid string) {
		require.NoError(t, runs.BeginMemoryConsolidationRun(ctx, &store.MemoryConsolidationRun{Namespace: "default", TaskUID: uid,
			TaskName: "consolidate", Mode: "DryRun", Status: store.MemoryConsolidationRunning, WindowStart: at.Add(-time.Hour),
			WindowEnd: at, Input: []byte(`{"transcript":"excerpt"}`), CreatedAt: time.Now().UTC()}))
	}
	task := func(uid string, consolidation bool) *corev1alpha1.Task {
		task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "consolidate", Namespace: "default", UID: types.UID(uid)},
			Spec:   corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, AI: &corev1alpha1.AISpec{}},
			Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseFailed}}
		if consolidation {
			task.Spec.AI.MemoryConsolidation = &corev1alpha1.MemoryConsolidationSpec{}
		}
		return task
	}
	r := &TaskReconciler{ResultStore: runs}

	begin("uid-plain")
	require.NoError(t, r.abandonMemoryConsolidationRun(ctx, task("uid-plain", false)))
	plain, err := runs.GetMemoryConsolidationRun(ctx, "default", "uid-plain")
	require.NoError(t, err)
	require.Equal(t, store.MemoryConsolidationRunning, plain.Status, "only a consolidation Task's run is abandoned")
	require.NoError(t, runs.AbandonMemoryConsolidationRun(ctx, "default", "uid-plain"))

	begin("uid-failed")
	require.NoError(t, r.abandonMemoryConsolidationRun(ctx, task("uid-failed", true)))
	failed, err := runs.GetMemoryConsolidationRun(ctx, "default", "uid-failed")
	require.NoError(t, err)
	require.Equal(t, store.MemoryConsolidationAbandoned, failed.Status)
	require.Empty(t, failed.Input, "the failed run's transcripts are cleared")

	require.NoError(t, r.abandonMemoryConsolidationRun(ctx, task("uid-never-began", true)), "a Task that never began a run")
	require.NoError(t, (&TaskReconciler{}).abandonMemoryConsolidationRun(ctx, task("uid-failed", true)), "no store configured")
}
