package controller

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

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
