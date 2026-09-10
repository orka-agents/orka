package controller

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestNativeSubstrateBootRecyclePreservesQueuedDemand(t *testing.T) {
	for _, restoring := range []bool{false, true} {
		name := "fresh boot"
		if restoring {
			name = "checkpoint restore"
		}
		t.Run(name, func(t *testing.T) {
			h := newNativeRuntimeTestHarness(t)
			if restoring {
				h.until(t, nativeTestServing)
				h.api.data[h.record(t).Attempt.Name] = "queued restore data"
				substrateSuspendTestPoolIntent(t, h.r, h.pool, true)
				h.until(t, nativeTestSuspended)
				substrateSuspendTestPoolIntent(t, h.r, h.pool, false)
			}
			h.until(t, func(_ *corev1alpha1.RuntimePool, record *substrateNativeState) bool {
				return record != nil && record.Attempt != nil && record.Attempt.BootRequested && record.Attempt.BootID == ""
			})
			attempt := h.record(t).Attempt
			worker := h.api.actors[attempt.Name].GetStatus().GetWorkerAssignment()
			pool := runtimePoolTestGetPool(t, h.r, h.pool)
			pool.Status.Capacity.QueuedTasks = 1
			require.NoError(t, h.r.Status().Update(t.Context(), &pool))
			creates, resumes, suspends := h.api.creates, h.api.resumes, h.api.suspends

			// A controller restart fences the unfinished boot. Queued demand
			// needs its replacement and must not prevent retiring that boot.
			h.r.ControllerEpoch++
			h.until(t, func(pool *corev1alpha1.RuntimePool, record *substrateNativeState) bool {
				return nativeTestServing(pool, record) && record.Attempt.UID != attempt.UID
			})
			pool = runtimePoolTestGetPool(t, h.r, h.pool)
			require.EqualValues(t, 1, pool.Status.Capacity.QueuedTasks)
			require.Equal(t, creates+1, h.api.creates)
			require.Equal(t, resumes+1, h.api.resumes)
			require.Equal(t, suspends, h.api.suspends, "an unadmitted boot cannot replace verified data")
			require.NotContains(t, h.api.actors, attempt.Name)
			require.False(t, h.api.deleteWithLivePod)
			err := h.r.Get(t.Context(), types.NamespacedName{Namespace: worker.GetWorkerNamespace(), Name: worker.GetWorkerPod()}, &corev1.Pod{})
			require.True(t, apierrors.IsNotFound(err), "the old worker must be absent before replacement admits work")
			if restoring {
				require.Equal(t, "queued restore data", h.api.data[h.record(t).Attempt.Name])
			}
		})
	}
}

func TestNativeSubstrateRecycleWaitsForReservationsButNotQueuedDemand(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reserve func(*corev1alpha1.RuntimePoolCapacityStatus)
	}{
		{"session", func(c *corev1alpha1.RuntimePoolCapacityStatus) { c.ReservedSessions = 1 }},
		{"prompt", func(c *corev1alpha1.RuntimePoolCapacityStatus) { c.ReservedPrompts = 1 }},
		{"reservation", func(c *corev1alpha1.RuntimePoolCapacityStatus) {
			c.Reservations = []corev1alpha1.RuntimePoolCapacityReservationStatus{{TaskUID: "reserved-task"}}
		}},
		{"finalization", func(c *corev1alpha1.RuntimePoolCapacityStatus) { c.FinalizingSessions = 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newNativeRuntimeTestHarness(t)
			h.until(t, nativeTestServing)
			attempt := h.record(t).Attempt
			h.api.data[attempt.Name] = "admitted runtime data"
			pool := runtimePoolTestGetPool(t, h.r, h.pool)
			pool.Status.Capacity.QueuedTasks = 3
			tc.reserve(&pool.Status.Capacity)
			require.NoError(t, h.r.Status().Update(t.Context(), &pool))
			creates, suspends := h.api.creates, h.api.suspends
			h.r.ControllerEpoch++
			for range 6 {
				h.step(t)
			}
			require.True(t, h.record(t).RecycleRequested)
			require.Equal(t, attempt.UID, h.record(t).Attempt.UID)
			require.Equal(t, creates, h.api.creates)
			require.Equal(t, suspends, h.api.suspends)
			require.Contains(t, h.api.actors, attempt.Name)

			pool = runtimePoolTestGetPool(t, h.r, h.pool)
			pool.Status.Capacity.ReservedSessions = 0
			pool.Status.Capacity.ReservedPrompts = 0
			pool.Status.Capacity.Reservations = nil
			pool.Status.Capacity.FinalizingSessions = 0
			require.NoError(t, h.r.Status().Update(t.Context(), &pool))
			h.until(t, func(pool *corev1alpha1.RuntimePool, record *substrateNativeState) bool {
				return nativeTestServing(pool, record) && record.Attempt.UID != attempt.UID
			})
			pool = runtimePoolTestGetPool(t, h.r, h.pool)
			require.EqualValues(t, 3, pool.Status.Capacity.QueuedTasks)
			require.Equal(t, creates+1, h.api.creates)
			require.Equal(t, suspends+1, h.api.suspends)
			require.Equal(t, "admitted runtime data", h.api.data[h.record(t).Attempt.Name])
			require.False(t, h.api.deleteWithLivePod)
		})
	}
}

func TestNativeSubstrateSuspensionBeforeAdmissionRetiresAttempt(t *testing.T) {
	for _, restoring := range []bool{false, true} {
		name := "fresh boot"
		if restoring {
			name = "checkpoint restore"
		}
		t.Run(name, func(t *testing.T) {
			h := newNativeRuntimeTestHarness(t)
			var saved *substrateNativeCheckpoint
			if restoring {
				h.until(t, nativeTestServing)
				h.api.data[h.record(t).Attempt.Name] = "last verified data"
				substrateSuspendTestPoolIntent(t, h.r, h.pool, true)
				h.until(t, nativeTestSuspended)
				saved = h.record(t).Checkpoint
				substrateSuspendTestPoolIntent(t, h.r, h.pool, false)
			}
			h.until(t, func(_ *corev1alpha1.RuntimePool, record *substrateNativeState) bool {
				return record != nil && record.Attempt != nil && record.Attempt.BootRequested && record.Attempt.BootID == ""
			})
			attempt := h.record(t).Attempt
			actor := h.api.actors[attempt.Name]
			worker := actor.GetStatus().GetWorkerAssignment()
			creates, resumes, suspends, seeds := h.api.creates, h.api.resumes, h.api.suspends, len(h.seeds)

			// Task recovery or cancellation can remove demand before this boot
			// acquires its admitted runtime identity. It cannot be checkpointed.
			substrateSuspendTestPoolIntent(t, h.r, h.pool, true)
			h.until(t, func(pool *corev1alpha1.RuntimePool, record *substrateNativeState) bool {
				return record.Phase == substrateNativeFailed && record.Attempt == nil && pool.Status.Lifecycle == corev1alpha1.RuntimePoolLifecycleDegraded
			})
			require.NotEmpty(t, h.record(t).Failure)
			require.Equal(t, saved, h.record(t).Checkpoint)
			require.Empty(t, h.api.actors)
			require.False(t, h.api.deleteWithLivePod)
			require.Equal(t, creates, h.api.creates)
			require.Equal(t, resumes, h.api.resumes)
			require.Equal(t, suspends, h.api.suspends, "unadmitted data must never replace the last verified checkpoint")
			require.Len(t, h.seeds, seeds, "cancelled admission must not deliver credentials")
			err := h.r.Get(t.Context(), types.NamespacedName{Namespace: worker.GetWorkerNamespace(), Name: worker.GetWorkerPod()}, &corev1.Pod{})
			require.True(t, apierrors.IsNotFound(err), "cleanup must prove the exact worker Pod is absent")
			if restoring {
				require.Equal(t, "last verified data", h.api.tagData[saved.Name])
			}
		})
	}
}
