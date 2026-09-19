package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/stretchr/testify/require"
)

func TestNativeSubstrateDeletionWaitsForControllerSettlement(t *testing.T) {
	h := newNativeRuntimeTestHarness(t)
	nativeRuntimeWithClock(h)
	h.until(t, nativeTestServing)
	pool := runtimePoolTestGetPool(t, h.r, h.pool)
	original := h.record(t)
	task := runtimePoolRetirementTask(t, &pool, "pending-cancellation")
	task.Status.Phase = corev1alpha1.TaskPhaseRunning
	task.Status.Execution.State = corev1alpha1.TaskExecutionStateRunning
	task.Status.Execution.Outcome = ""
	require.NoError(t, h.r.Create(t.Context(), task))
	require.NoError(t, h.r.Delete(t.Context(), &pool))

	// A drained supervisor retains cancellation receipts in tombstones. Its
	// empty probe does not prove that the controller consumed that receipt;
	// deleting the worker now would destroy the remaining settlement evidence.
	for range 8 {
		h.step(t)
		require.NoError(t, h.r.Get(t.Context(), client.ObjectKey{
			Namespace: original.Attempt.Worker.Namespace, Name: original.Attempt.Worker.Pod,
		}, &corev1.Pod{}), "deletion removed the runtime before cancellation settled")
		require.NoError(t, h.r.Get(t.Context(), client.ObjectKeyFromObject(task), task))
		require.Equal(t, corev1alpha1.TaskExecutionStateRunning, task.Status.Execution.State)
		require.Empty(t, task.Status.Execution.Outcome)
	}
	require.Positive(t, h.supervisor.drainCalls)
	require.Zero(t, h.api.deletes)
	waiting := runtimePoolTestGetPool(t, h.r, h.pool)
	require.NotNil(t, waiting.Status.ActiveInstance, "receipt consumption still needs the admitted runtime")
	require.Equal(t, task.Status.Execution.RuntimeInstanceID, waiting.Status.ActiveInstance.RuntimeInstanceID)

	require.NoError(t, h.r.Get(t.Context(), client.ObjectKeyFromObject(task), task))
	task.Status.Phase = corev1alpha1.TaskPhaseCancelled
	task.Status.Execution.State = corev1alpha1.TaskExecutionStateCancelled
	task.Status.Execution.Outcome = corev1alpha1.TaskExecutionOutcomeCancelled
	require.NoError(t, h.r.Status().Update(t.Context(), task))
	nativeDeletePool(t, h, &pool)
	require.Empty(t, h.api.actors)
	require.False(t, h.api.deleteWithLivePod)
}

func TestNativeSubstrateDeletionSettlementDeadlineSurvivesRestart(t *testing.T) {
	for _, deletingTask := range []bool{false, true} {
		t.Run(map[bool]string{false: "running", true: "deleted dispatcher Task"}[deletingTask], func(t *testing.T) {
			h := newNativeRuntimeTestHarness(t)
			clock := nativeRuntimeWithClock(h)
			h.until(t, nativeTestServing)
			pool, task, worker := nativeSettlementDeletion(t, h)
			if deletingTask {
				require.NoError(t, h.r.Delete(t.Context(), task))
			}
			h.until(t, func(_ *corev1alpha1.RuntimePool, record *substrateNativeState) bool {
				return record.Attempt.SettlementWaitStartedAt != nil
			})
			startedAt := h.record(t).Attempt.SettlementWaitStartedAt.Time
			// Recreate the reconciler from its external dependencies, not a copy
			// that could accidentally preserve process-local deadline state.
			old := h.r
			h.r = &RuntimePoolReconciler{
				Client: old.Client, APIReader: old.APIReader, Scheme: old.Scheme,
				RuntimeNamespace: old.RuntimeNamespace, ControllerNamespace: old.ControllerNamespace,
				ControllerAPIURL: old.ControllerAPIURL, ControllerAPIPort: old.ControllerAPIPort,
				ControllerEpoch: old.ControllerEpoch, AllowedImages: old.AllowedImages,
				WorkspaceArtifactMaxBytes: old.WorkspaceArtifactMaxBytes,
				ProviderProxy:             old.ProviderProxy, SubstrateEnabled: old.SubstrateEnabled,
				SubstrateConfig: old.SubstrateConfig, SubstrateNativeClientFactory: old.SubstrateNativeClientFactory,
				SubstrateCredentialSeeder: old.SubstrateCredentialSeeder, SupervisorClient: old.SupervisorClient,
				Rand: old.Rand, Now: old.Now,
			}
			clock.now = startedAt.Add(25*time.Second - time.Nanosecond)
			for range 8 {
				h.step(t)
				nativeSettlementWorkerPresent(t, h, worker)
				require.Equal(t, startedAt, h.record(t).Attempt.SettlementWaitStartedAt.Time)
			}
			clock.now = startedAt.Add(25 * time.Second)
			h.step(t)
			current := runtimePoolTestGetPool(t, h.r, h.pool)
			require.Equal(t, corev1alpha1.RuntimePoolLifecycleQuiescent, current.Status.Lifecycle, "expiry releases only the Task-status wait")
			nativeDeletePool(t, h, &pool)
			require.True(t, apierrors.IsNotFound(h.r.Get(t.Context(), worker, &corev1.Pod{})))
			require.Empty(t, h.api.actors)
			require.False(t, h.api.deleteWithLivePod)
			require.NoError(t, h.r.Get(t.Context(), client.ObjectKeyFromObject(task), task))
			require.Equal(t, corev1alpha1.TaskExecutionStateRunning, task.Status.Execution.State)
			require.Empty(t, task.Status.Execution.Outcome, "deletion must not manufacture cancellation")
		})
	}
}

func TestNativeSubstrateDeletionSettlementWaitsForSuccessDelivery(t *testing.T) {
	for _, deliveryMissing := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending delivery", true: "missing delivery"}[deliveryMissing], func(t *testing.T) {
			h := newNativeRuntimeTestHarness(t)
			nativeRuntimeWithClock(h)
			h.until(t, nativeTestServing)
			pool, task, worker := nativeSettlementDeletion(t, h)
			task.Status.Phase = corev1alpha1.TaskPhaseSucceeded
			task.Status.Execution.State = corev1alpha1.TaskExecutionStateSucceeded
			task.Status.Execution.Outcome = corev1alpha1.TaskExecutionOutcomeSucceeded
			task.Status.Delivery = &corev1alpha1.TaskDeliveryStatus{State: corev1alpha1.TaskDeliveryStatePublishing}
			if deliveryMissing {
				task.Status.Delivery = nil
			}
			require.NoError(t, h.r.Status().Update(t.Context(), task))
			for range 8 {
				h.step(t)
				nativeSettlementWorkerPresent(t, h, worker)
			}
			require.NoError(t, h.r.Get(t.Context(), client.ObjectKeyFromObject(task), task))
			task.Status.Delivery = &corev1alpha1.TaskDeliveryStatus{State: corev1alpha1.TaskDeliveryStateNotRequested}
			require.NoError(t, h.r.Status().Update(t.Context(), task))
			nativeDeletePool(t, h, &pool)
			require.Empty(t, h.api.actors)
		})
	}
}

func TestNativeSubstrateDeletionSettlementIgnoresUnrelatedTask(t *testing.T) {
	for _, field := range []string{"pool name", "pool UID", "instance"} {
		t.Run(field, func(t *testing.T) {
			h := newNativeRuntimeTestHarness(t)
			nativeRuntimeWithClock(h)
			h.until(t, nativeTestServing)
			pool, task, _ := nativeSettlementDeletion(t, h)
			switch field {
			case "pool name":
				task.Status.Execution.RuntimePoolName = "another-pool"
			case "pool UID":
				task.Status.Execution.RuntimePoolUID = "replaced-pool-uid"
			case "instance":
				task.Status.Execution.RuntimeInstanceID = "another-instance"
			}
			require.NoError(t, h.r.Status().Update(t.Context(), task))
			nativeDeletePool(t, h, &pool)
			require.Empty(t, h.api.actors)
		})
	}
}

func TestNativeSubstrateDeletionSettlementDoesNotReuseDrainDeadline(t *testing.T) {
	h := newNativeRuntimeTestHarness(t)
	clock := nativeRuntimeWithClock(h)
	h.until(t, nativeTestServing)
	_, _, worker := nativeSettlementDeletion(t, h)
	h.until(t, func(_ *corev1alpha1.RuntimePool, record *substrateNativeState) bool {
		return !record.Attempt.DrainStartedAt.IsZero() && h.supervisor.drainCalls > 0
	})
	// Drain may already have been requested for much longer than the new
	// retention window. Only authenticated quiescence starts this deadline.
	clock.now = clock.now.Add(7 * time.Minute)
	firstQuiescent := clock.now
	for range 8 {
		h.step(t)
		nativeSettlementWorkerPresent(t, h, worker)
	}
	require.Equal(t, firstQuiescent.UTC(), h.record(t).Attempt.SettlementWaitStartedAt.UTC())
}

type nativeSettlementTaskClient struct {
	client.Client
	taskListError error
	omitTasks     bool
}

func (c *nativeSettlementTaskClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if tasks, ok := list.(*corev1alpha1.TaskList); ok {
		if c.taskListError != nil {
			return c.taskListError
		}
		if c.omitTasks {
			tasks.Items = nil
			return nil
		}
	}
	return c.Client.List(ctx, list, opts...)
}

func TestNativeSubstrateDeletionSettlementUsesUncachedTasks(t *testing.T) {
	h := newNativeRuntimeTestHarness(t)
	nativeRuntimeWithClock(h)
	h.until(t, nativeTestServing)
	pool, task, worker := nativeSettlementDeletion(t, h)
	h.r.APIReader = h.r.Client
	h.r.Client = &nativeSettlementTaskClient{Client: h.r.Client, omitTasks: true}
	for range 8 {
		h.step(t)
		nativeSettlementWorkerPresent(t, h, worker)
	}
	require.NoError(t, h.r.APIReader.Get(t.Context(), client.ObjectKeyFromObject(task), task))
	task.Status.Phase = corev1alpha1.TaskPhaseCancelled
	task.Status.Execution.State = corev1alpha1.TaskExecutionStateCancelled
	task.Status.Execution.Outcome = corev1alpha1.TaskExecutionOutcomeCancelled
	require.NoError(t, h.r.Status().Update(t.Context(), task))
	nativeDeletePool(t, h, &pool)
	require.Empty(t, h.api.actors)
}

func TestNativeSubstrateDeletionSettlementReadFailureIsBoundedButPreservesCleanupGuard(t *testing.T) {
	h := newNativeRuntimeTestHarness(t)
	clock := nativeRuntimeWithClock(h)
	h.until(t, nativeTestServing)
	_, _, worker := nativeSettlementDeletion(t, h)
	h.until(t, func(_ *corev1alpha1.RuntimePool, record *substrateNativeState) bool {
		return record.Attempt.SettlementWaitStartedAt != nil
	})
	startedAt := h.record(t).Attempt.SettlementWaitStartedAt.Time
	pool := runtimePoolTestGetPool(t, h.r, h.pool)
	failure := errors.New("Task API unavailable")
	h.r.APIReader = &nativeSettlementTaskClient{Client: h.r.Client, taskListError: failure}
	clock.now = startedAt.Add(25*time.Second - time.Nanosecond)
	settled, err := h.r.nativeSubstrateDeletionTasksSettled(t.Context(), &pool, pool.Status.ActiveInstance, startedAt)
	require.ErrorIs(t, err, failure)
	require.False(t, settled)
	_, err = h.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&pool)})
	require.ErrorIs(t, err, failure)
	nativeSettlementWorkerPresent(t, h, worker)
	clock.now = startedAt.Add(25 * time.Second)
	settled, err = h.r.nativeSubstrateDeletionTasksSettled(t.Context(), &pool, pool.Status.ActiveInstance, startedAt)
	require.NoError(t, err)
	require.True(t, settled, "a persisted deadline bounds the new status read, including failures")
	_, err = h.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&pool)})
	require.ErrorIs(t, err, failure, "expiry must not bypass existing retirement cleanup evidence")
	nativeSettlementWorkerPresent(t, h, worker)
	h.r.APIReader = h.r.Client
	nativeDeletePool(t, h, &pool)
}

func TestNativeSubstrateDeletionSettlementRequiresExactBoot(t *testing.T) {
	h := newNativeRuntimeTestHarness(t)
	clock := nativeRuntimeWithClock(h)
	h.until(t, nativeTestServing)
	pool, task, _ := nativeSettlementDeletion(t, h)
	task.Status.Execution.RuntimeSessionSupervisorBootID = "another-boot"
	require.NoError(t, h.r.Status().Update(t.Context(), task))
	settled, err := h.r.nativeSubstrateDeletionTasksSettled(t.Context(), &pool, pool.Status.ActiveInstance, clock.now)
	require.NoError(t, err)
	require.True(t, settled, "another boot is not a controller-settlement hold for this worker")
}

func TestNativeSubstrateDeletionSettlementExpiryCannotBypassQuiescence(t *testing.T) {
	for _, guard := range []string{"active prompt", "resident session", "authentication", "boot identity", "reserved capacity"} {
		t.Run(guard, func(t *testing.T) {
			h := newNativeRuntimeTestHarness(t)
			clock := nativeRuntimeWithClock(h)
			h.until(t, nativeTestServing)
			_, _, worker := nativeSettlementDeletion(t, h)
			h.until(t, func(_ *corev1alpha1.RuntimePool, record *substrateNativeState) bool {
				return record.Attempt.SettlementWaitStartedAt != nil
			})
			clock.now = h.record(t).Attempt.SettlementWaitStartedAt.Add(25 * time.Second)
			switch guard {
			case "active prompt":
				h.supervisor.afterProbe = func() { h.supervisor.probe.Status.Pressure.ActivePrompts = 1 }
			case "resident session":
				h.supervisor.afterProbe = func() { h.supervisor.probe.Status.Pressure.ResidentSessions = 1 }
			case "authentication":
				h.supervisor.probeErr = errors.New("supervisor authentication failed")
			case "boot identity":
				h.supervisor.afterProbe = func() { h.supervisor.probe.Status.Fence.SupervisorBootID = "another-boot" }
			case "reserved capacity":
				pool := runtimePoolTestGetPool(t, h.r, h.pool)
				pool.Status.Capacity.ReservedSessions = 1
				require.NoError(t, h.r.Status().Update(t.Context(), &pool))
			}
			for range 8 {
				h.step(t)
				nativeSettlementWorkerPresent(t, h, worker)
			}
		})
	}
}

func TestNativeSubstrateDeletionSettlementStartsOnlyAfterQuiescence(t *testing.T) {
	h := newNativeRuntimeTestHarness(t)
	clock := nativeRuntimeWithClock(h)
	h.until(t, nativeTestServing)
	_, _, worker := nativeSettlementDeletion(t, h)
	h.supervisor.afterProbe = func() { h.supervisor.probe.Status.Pressure.ActivePrompts = 1 }
	for range 8 {
		h.step(t)
		clock.now = clock.now.Add(time.Minute)
		nativeSettlementWorkerPresent(t, h, worker)
		require.Nil(t, h.record(t).Attempt.SettlementWaitStartedAt, "live work cannot start or exhaust the status grace")
	}
	h.supervisor.afterProbe = nil
	firstQuiescent := clock.now
	for range 8 {
		h.step(t)
		nativeSettlementWorkerPresent(t, h, worker)
	}
	require.Equal(t, firstQuiescent.UTC(), h.record(t).Attempt.SettlementWaitStartedAt.UTC())
}

func nativeSettlementDeletion(t *testing.T, h *nativeRuntimeTestHarness) (corev1alpha1.RuntimePool, *corev1alpha1.Task, client.ObjectKey) {
	t.Helper()
	pool := runtimePoolTestGetPool(t, h.r, h.pool)
	fence := h.record(t).Attempt.Worker
	task := runtimePoolRetirementTask(t, &pool, "pending-settlement")
	task.Status.Phase = corev1alpha1.TaskPhaseRunning
	task.Status.Execution.State = corev1alpha1.TaskExecutionStateRunning
	task.Status.Execution.Outcome = ""
	require.NoError(t, h.r.Create(t.Context(), task))
	require.NoError(t, h.r.Delete(t.Context(), &pool))
	return pool, task, client.ObjectKey{Namespace: fence.Namespace, Name: fence.Pod}
}

func nativeSettlementWorkerPresent(t *testing.T, h *nativeRuntimeTestHarness, worker client.ObjectKey) {
	t.Helper()
	pod := &corev1.Pod{}
	require.NoError(t, h.r.Get(t.Context(), worker, pod), "settlement wait removed the exact worker")
	require.True(t, pod.DeletionTimestamp.IsZero())
	require.Zero(t, h.api.deletes)
}
