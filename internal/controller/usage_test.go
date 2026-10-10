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
			beforeDeletion := time.Now().UTC()
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

func TestUsageDelayedFinalizingSnapshotPreservesCompletedWork(t *testing.T) {
	backend := setupControllerSQLiteStore(t)
	start := time.Now().UTC().Add(-120 * 24 * time.Hour).Truncate(time.Second)
	workID := store.UsageWorkID("default", "monitor", "org/repo", "issue", 1)
	require.NoError(t, backend.RegisterUsageWork(t.Context(), store.UsageWorkRequest{Namespace: "default", NamespaceUID: "namespace-uid",
		MonitorUID: "monitor", MonitorName: "monitor", Repository: "org/repo", Kind: "issue", Number: 1, StartedAt: start}))
	finalizing := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "default", UID: "task-uid", CreationTimestamp: metav1.NewTime(start)},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseFinalizing, ExecutionOutcome: &corev1alpha1.TaskWorkloadExecutionOutcome{
			Phase: corev1alpha1.TaskPhaseSucceeded, RecordedAt: metav1.NewTime(start.Add(time.Minute))}}}
	completed := finalizing.DeepCopy()
	completed.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	completed.Status.CompletionTime = new(metav1.NewTime(start.Add(2 * time.Minute)))
	recorded := make([]time.Time, 0, 3)
	for _, task := range []*corev1alpha1.Task{completed, finalizing, completed} {
		snapshot := usageTaskSnapshot(task)
		snapshot.WorkID = workID
		require.NoError(t, backend.RegisterUsageTask(t.Context(), snapshot))
		recorded = append(recorded, time.Now().UTC())
	}
	filter := store.UsageFilter{Namespaces: []string{"default"}, From: start, Until: start.Add(time.Hour), AsOf: start.Add(90 * time.Second)}
	data, err := backend.LoadUsage(t.Context(), filter)
	require.NoError(t, err)
	report, err := usage.Build(data, filter)
	require.NoError(t, err)
	require.Equal(t, "Unknown", report.Works[0].Tasks[0].Phase, "the controller had not recorded a phase at this historical time")
	for _, at := range recorded {
		filter.AsOf = at
		report, err = usage.Build(data, filter)
		require.NoError(t, err)
		require.Equal(t, "Succeeded", report.Works[0].Tasks[0].Phase)
		require.Zero(t, report.Summary.UnfinishedWork)
	}
	require.NoError(t, backend.PruneUsage(t.Context(), time.Now().Add(-90*24*time.Hour)))
	data, err = backend.LoadUsage(t.Context(), filter)
	require.NoError(t, err)
	require.Empty(t, data.Works)
	require.Empty(t, data.Tasks)
}

func TestUsageRetryPreservesPendingInterval(t *testing.T) {
	backend := setupControllerSQLiteStore(t)
	start := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "retry", Namespace: "default", UID: "retry-uid", CreationTimestamp: metav1.NewTime(start)},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, RetryPolicy: &corev1alpha1.RetryPolicy{MaxRetries: 1}},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning, Attempts: 1, StartTime: new(metav1.NewTime(start.Add(time.Minute))),
			Conditions: []metav1.Condition{{Type: ConditionTypeJobCreated, Status: metav1.ConditionTrue, Reason: "JobCreated", LastTransitionTime: metav1.NewTime(start.Add(time.Minute))}}}}
	r := newUnitReconciler(newTestScheme(), task, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", UID: "namespace-uid"}})
	r.ExecutionEventStore = backend
	workID := store.UsageWorkID("default", "monitor", "org/repo", "issue", 1)
	require.NoError(t, backend.RegisterUsageWork(t.Context(), store.UsageWorkRequest{Namespace: "default", NamespaceUID: "namespace-uid",
		MonitorUID: "monitor", MonitorName: "monitor", Repository: "org/repo", Kind: "issue", Number: 1, StartedAt: start}))
	initial := task.DeepCopy()
	initial.Status = corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending}
	recorded := make([]time.Time, 0, 2)
	for _, snapshotTask := range []*corev1alpha1.Task{initial, task} {
		snapshot := usageTaskSnapshot(snapshotTask)
		snapshot.WorkID = workID
		require.NoError(t, backend.RegisterUsageTask(t.Context(), snapshot))
		recorded = append(recorded, time.Now().UTC())
	}
	_, err := r.retryTask(t.Context(), task)
	require.NoError(t, err)
	reloaded := &corev1alpha1.Task{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), reloaded))
	require.Equal(t, corev1alpha1.TaskPhasePending, reloaded.Status.Phase)
	require.NoError(t, r.retainUsageTask(t.Context(), reloaded))
	// A late monitor create snapshot must not erase the retry interval.
	snapshot := usageTaskSnapshot(initial)
	snapshot.WorkID = workID
	require.NoError(t, backend.RegisterUsageTask(t.Context(), snapshot))
	filter := store.UsageFilter{Namespaces: []string{"default"}, From: start, Until: time.Now().Add(time.Hour), AsOf: time.Now().UTC()}
	data, err := backend.LoadUsage(t.Context(), filter)
	require.NoError(t, err)
	for _, check := range []struct {
		at    time.Time
		phase string
	}{{recorded[0], "Pending"}, {recorded[1], "Running"}, {filter.AsOf, "Pending"}} {
		filter.AsOf = check.at
		report, err := usage.Build(data, filter)
		require.NoError(t, err)
		require.Equal(t, check.phase, report.Works[0].Tasks[0].Phase)
	}
}
