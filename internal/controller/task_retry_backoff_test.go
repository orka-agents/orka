package controller

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/executionmode"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
)

func TestRetryTaskBackoffSurvivesReconcileEventsAndRestart(t *testing.T) {
	task := retryPendingTaskFixture(time.Now().UTC())
	task.Status.Phase = corev1alpha1.TaskPhaseRunning
	task.Status.JobName, task.Status.JobUID = "failed-job", "failed-job-uid"
	task.Status.Conditions[0].Status, task.Status.Conditions[0].Reason = metav1.ConditionTrue, "JobCreated"
	oldJob := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: task.Status.JobName, Namespace: task.Namespace, UID: "failed-job-uid"}}
	r := newUnitReconciler(newTestScheme(), task, oldJob,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: task.Namespace, UID: "namespace-uid"}})
	r.Mode = executionmode.HarnessV2
	result, err := r.retryTask(t.Context(), task)
	require.NoError(t, err)
	require.Equal(t, 30*time.Second, result.RequeueAfter)
	key := client.ObjectKeyFromObject(task)
	current := &corev1alpha1.Task{}
	require.NoError(t, r.Get(t.Context(), key, current))
	condition := meta.FindStatusCondition(current.Status.Conditions, ConditionTypeJobCreated)
	require.NotNil(t, condition)
	require.Equal(t, taskRetryPendingReason, condition.Reason)
	retryAt := condition.LastTransitionTime
	deadline := retryAt.Time.Truncate(time.Second).Add(31 * time.Second)
	require.True(t, apierrors.IsNotFound(r.Get(t.Context(), client.ObjectKeyFromObject(oldJob), &batchv1.Job{})))
	require.ErrorIs(t, r.ResultStore.(store.TaskJobAuthorityStore).CheckTaskJobAuthority(t.Context(), store.TaskJobIdentity{
		Namespace: task.Namespace, TaskUID: string(task.UID), JobUID: string(oldJob.UID),
	}), store.ErrTaskJobRevoked)

	// A new reconciler has only persisted Task state, as after a restart.
	restarted := &TaskReconciler{Client: r.Client, APIReader: r.Client, Scheme: r.Scheme, JobBuilder: r.JobBuilder,
		Recorder: r.Recorder, ResultStore: r.ResultStore, ExecutionEventStore: r.ExecutionEventStore, Mode: executionmode.HarnessV2}
	for _, reconciler := range []*TaskReconciler{r, restarted} {
		for range 2 {
			before := time.Now()
			result, err := reconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
			after := time.Now()
			require.NoError(t, err)
			require.Positive(t, result.RequeueAfter)
			require.GreaterOrEqual(t, result.RequeueAfter, deadline.Sub(after))
			require.LessOrEqual(t, result.RequeueAfter, deadline.Sub(before))
			require.NoError(t, r.Get(t.Context(), key, current))
			require.Equal(t, corev1alpha1.TaskPhasePending, current.Status.Phase)
			require.EqualValues(t, 1, current.Status.Attempts)
			require.Empty(t, current.Status.JobName)
			require.Empty(t, current.Status.JobUID)
			require.Equal(t, retryAt, meta.FindStatusCondition(current.Status.Conditions, ConditionTypeJobCreated).LastTransitionTime)
			var jobs batchv1.JobList
			require.NoError(t, r.List(t.Context(), &jobs, client.InNamespace(task.Namespace)))
			require.Empty(t, jobs.Items, "status and old Job events must not start a retry early")
		}
	}

	// Move only the test clock anchor into the past; no sleeping is needed to
	// prove that an expired retry deadline admits exactly the next attempt.
	meta.FindStatusCondition(current.Status.Conditions, ConditionTypeJobCreated).LastTransitionTime = metav1.NewTime(time.Now().Add(-31 * time.Second))
	require.NoError(t, r.Status().Update(t.Context(), current))
	_, err = restarted.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	require.NoError(t, r.Get(t.Context(), key, current))
	require.Equal(t, corev1alpha1.TaskPhaseRunning, current.Status.Phase)
	require.EqualValues(t, 2, current.Status.Attempts)
	var jobs batchv1.JobList
	require.NoError(t, r.List(t.Context(), &jobs, client.InNamespace(task.Namespace)))
	require.Len(t, jobs.Items, 1)
	require.Equal(t, current.Status.JobName, jobs.Items[0].Name)
	require.True(t, metav1.IsControlledBy(&jobs.Items[0], task))
}

func TestRetryTaskBackoffUsesLatestTaskBeforeJobCreation(t *testing.T) {
	task := retryPendingTaskFixture(time.Now().UTC())
	r := newUnitReconciler(newTestScheme(), task)
	r.APIReader = r.Client
	stale := task.DeepCopy()
	stale.Status.Conditions = nil
	stale.Status.Attempts = 0
	result, err := r.createTaskJob(t.Context(), stale, nil, nil)
	require.NoError(t, err)
	require.Greater(t, result.RequeueAfter, 28*time.Second)
	var jobs batchv1.JobList
	require.NoError(t, r.List(t.Context(), &jobs, client.InNamespace(task.Namespace)))
	require.Empty(t, jobs.Items, "a stale Pending read must not bypass the persisted retry deadline")
	current := &corev1alpha1.Task{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), current))
	require.Equal(t, corev1alpha1.TaskPhasePending, current.Status.Phase)
	require.EqualValues(t, 1, current.Status.Attempts)
}

func TestRetryTaskBackoffPreservesCancellationAndDeletion(t *testing.T) {
	for _, terminal := range []string{"cancelled", "cancellation outcome", "deleted"} {
		t.Run(terminal, func(t *testing.T) {
			task := retryPendingTaskFixture(time.Now().UTC())
			if terminal == "cancelled" {
				task.Status.Phase = corev1alpha1.TaskPhaseCancelled
			}
			if terminal == "cancellation outcome" {
				task.Status.ExecutionOutcome = &corev1alpha1.TaskWorkloadExecutionOutcome{
					Phase: corev1alpha1.TaskPhaseCancelled, RecordedAt: metav1.Now(), Message: "cancelled during retry delay",
				}
			}
			r := newUnitReconciler(newTestScheme(), task,
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: task.Namespace, UID: "namespace-uid"}})
			if terminal == "deleted" {
				require.NoError(t, r.Delete(t.Context(), task))
			}
			key := client.ObjectKeyFromObject(task)
			result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
			require.NoError(t, err)
			require.LessOrEqual(t, result.RequeueAfter, time.Second, "terminal handling must not wait for retry backoff")
			current := &corev1alpha1.Task{}
			err = r.Get(t.Context(), key, current)
			if terminal == "deleted" {
				require.True(t, apierrors.IsNotFound(err), "deletion must remove the finalizer during backoff")
			} else {
				require.NoError(t, err)
				require.Equal(t, corev1alpha1.TaskPhaseCancelled, current.Status.Phase)
				require.EqualValues(t, 1, current.Status.Attempts)
			}
			var jobs batchv1.JobList
			require.NoError(t, r.List(t.Context(), &jobs, client.InNamespace(task.Namespace)))
			require.Empty(t, jobs.Items)
		})
	}
}

func TestRetryTaskBackoffDoesNotDelayInitialJob(t *testing.T) {
	task := retryPendingTaskFixture(time.Now().UTC())
	task.Status = corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending}
	r := newUnitReconciler(newTestScheme(), task,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: task.Namespace, UID: "namespace-uid"}})
	key := client.ObjectKeyFromObject(task)
	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
	require.NoError(t, err)
	current := &corev1alpha1.Task{}
	require.NoError(t, r.Get(t.Context(), key, current))
	require.Equal(t, corev1alpha1.TaskPhaseRunning, current.Status.Phase)
	require.EqualValues(t, 1, current.Status.Attempts)
}

func TestRetryTaskBackoffDeadlineBoundary(t *testing.T) {
	at := time.Date(2026, time.September, 19, 3, 47, 56, 0, time.UTC)
	r := &TaskReconciler{}
	task := retryPendingTaskFixture(at)
	task.Spec.Type = corev1alpha1.TaskTypeAI
	task.Spec.RetryPolicy.BackoffMultiplier = 3
	task.Status.Attempts = 2
	deadline := at.Add(91 * time.Second)
	require.Equal(t, time.Nanosecond, r.remainingRetryDelay(task, deadline.Add(-time.Nanosecond)))
	require.Zero(t, r.remainingRetryDelay(task, deadline))
	require.Zero(t, r.remainingRetryDelay(task, deadline.Add(time.Nanosecond)))
	// ACP and harness-v1 attempts own their retry/admission lifecycle.
	task.Spec.Type = corev1alpha1.TaskTypeAgent
	require.Zero(t, r.remainingRetryDelay(task, at))
}

func TestRetryTaskBackoffSurvivesTimestampSerialization(t *testing.T) {
	r := &TaskReconciler{}
	for _, offset := range []time.Duration{0, 817197 * time.Microsecond, time.Second - time.Nanosecond} {
		for _, delay := range []time.Duration{0, time.Nanosecond, 100 * time.Millisecond, 30 * time.Second} {
			t.Run(offset.String()+"/"+delay.String(), func(t *testing.T) {
				at := time.Date(2026, time.September, 19, 4, 24, 33, 0, time.UTC).Add(offset)
				task := retryPendingTaskFixture(at)
				task.Spec.Type = corev1alpha1.TaskTypeAI
				task.Spec.RetryPolicy.InitialDelay = &metav1.Duration{Duration: delay}
				raw, err := json.Marshal(task)
				require.NoError(t, err)
				var persisted corev1alpha1.Task
				require.NoError(t, json.Unmarshal(raw, &persisted))
				require.Equal(t, delay, persisted.Spec.RetryPolicy.InitialDelay.Duration)
				remaining := r.remainingRetryDelay(&persisted, at)
				if delay == 0 {
					require.Zero(t, remaining, "an explicitly immediate retry must remain immediate")
					return
				}
				require.GreaterOrEqual(t, remaining, delay, "persistence must not shorten the requested delay")
				require.LessOrEqual(t, remaining, delay+time.Second, "rounding must add at most one second")
				require.Positive(t, r.remainingRetryDelay(&persisted, at.Add(delay-time.Nanosecond)))
				require.Zero(t, r.remainingRetryDelay(&persisted, at.Add(delay+time.Second)))
			})
		}
	}
}

func retryPendingTaskFixture(at time.Time) *corev1alpha1.Task {
	task := taskJobIdentityFixture()
	task.Finalizers = []string{labels.TaskFinalizer}
	task.CreationTimestamp = metav1.NewTime(at.Add(-time.Hour))
	task.Spec.RetryPolicy = &corev1alpha1.RetryPolicy{MaxRetries: 3, InitialDelay: &metav1.Duration{Duration: 30 * time.Second}}
	task.Status.Attempts = 1
	task.Status.StartTime = new(metav1.NewTime(at.Add(-time.Minute)))
	task.Status.Conditions = []metav1.Condition{{Type: ConditionTypeJobCreated, Status: metav1.ConditionFalse,
		Reason: taskRetryPendingReason, LastTransitionTime: metav1.NewTime(at)}}
	return task
}
