package controller

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/usage"
)

func TestUsageDeletedTaskStopsPinningRetention(t *testing.T) {
	for _, phase := range []corev1alpha1.TaskPhase{corev1alpha1.TaskPhasePending, corev1alpha1.TaskPhaseRunning} {
		t.Run(string(phase), func(t *testing.T) {
			backend := setupControllerSQLiteStore(t)
			start := time.Now().UTC().Add(-120 * 24 * time.Hour)
			task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "deleted", Namespace: "default", UID: "deleted-uid",
				Finalizers: []string{labels.TaskFinalizer}, CreationTimestamp: metav1.NewTime(start)},
				Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer}, Status: corev1alpha1.TaskStatus{Phase: phase, StartTime: new(metav1.NewTime(start))}}
			r := newUnitReconciler(newTestScheme(), task, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", UID: "namespace-uid"}})
			r.ExecutionEventStore = backend
			workID := store.UsageWorkID("default", "monitor", "org/repo", "issue", 1)
			require.NoError(t, backend.RegisterUsageWork(t.Context(), store.UsageWorkRequest{Namespace: "default", NamespaceUID: "namespace-uid",
				MonitorUID: "monitor", MonitorName: "monitor", Repository: "org/repo", Kind: "issue", Number: 1, StartedAt: start}))
			snapshot := usageTaskSnapshot(task)
			snapshot.WorkID = workID
			require.NoError(t, backend.RegisterUsageTask(t.Context(), snapshot))
			require.NoError(t, backend.RecordUsage(t.Context(), store.UsageObservation{Namespace: "default", TaskUID: string(task.UID),
				ID: "call", CounterID: "call", Scope: store.UsageScopeCall, Source: store.UsageSourceProvider,
				InputTokens: new(int64(100)), OutputTokens: new(int64(0)), Complete: true, ObservedAt: start}))
			beforeDeletion := time.Now().UTC().Add(-time.Minute)
			require.NoError(t, r.Delete(t.Context(), task))
			_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)})
			require.NoError(t, err)
			require.True(t, apierrors.IsNotFound(r.Get(t.Context(), client.ObjectKeyFromObject(task), &corev1alpha1.Task{})))
			filter := store.UsageFilter{Namespaces: []string{"default"}, From: start, Until: start.Add(time.Hour), AsOf: time.Now().UTC()}
			data, err := backend.LoadUsage(t.Context(), filter)
			require.NoError(t, err)
			report, err := usage.Build(data, filter)
			require.NoError(t, err)
			require.Len(t, report.Works, 1)
			require.Equal(t, "Cancelled", report.Works[0].Tasks[0].Phase)
			require.EqualValues(t, 100, report.Summary.TotalTokens)
			filter.AsOf = beforeDeletion
			historical, err := usage.Build(data, filter)
			require.NoError(t, err)
			require.Equal(t, string(phase), historical.Works[0].Tasks[0].Phase)
			// Retain the cancellation for the configured window, then expire
			// the abandoned cohort and its counts after that window elapses.
			require.NoError(t, backend.PruneUsage(t.Context(), time.Now().Add(-90*24*time.Hour)))
			data, err = backend.LoadUsage(t.Context(), filter)
			require.NoError(t, err)
			require.Len(t, data.Works, 1)
			require.NoError(t, backend.PruneUsage(t.Context(), time.Now().Add(time.Hour)))
			data, err = backend.LoadUsage(t.Context(), filter)
			require.NoError(t, err)
			require.Empty(t, data.Works)
			require.Empty(t, data.Tasks)
			require.Empty(t, data.Observations)
		})
	}
}

func TestUsageDeletionPreservesRecordedOutcome(t *testing.T) {
	completed := metav1.NewTime(time.Now().Add(-time.Hour))
	deleted := metav1.Now()
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &deleted},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseFinalizing, ExecutionOutcome: &corev1alpha1.TaskWorkloadExecutionOutcome{
			Phase: corev1alpha1.TaskPhaseSucceeded, RecordedAt: completed}}}
	snapshot := usageTaskSnapshot(task)
	require.Equal(t, "Succeeded", snapshot.Phase)
	require.Equal(t, completed.Time, snapshot.PhaseObservedAt)
}
