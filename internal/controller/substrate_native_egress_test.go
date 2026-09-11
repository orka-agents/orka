package controller

import (
	"strings"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
)

func nativeTestEgressAdmissionClosed(pool *corev1alpha1.RuntimePool, _ *substrateNativeState) bool {
	return pool.Status.AdmissionState == corev1alpha1.RuntimePoolAdmissionClosed &&
		strings.Contains(pool.Status.Message, "--substrate-direct-egress-enabled")
}

func TestNativeSubstrateRequiresDirectEgressBeforeAdmission(t *testing.T) {
	for _, phase := range []string{"fresh", "booted without credentials"} {
		t.Run(phase, func(t *testing.T) {
			h := newNativeRuntimeTestHarness(t)
			if phase == "booted without credentials" {
				h.until(t, func(_ *corev1alpha1.RuntimePool, _ *substrateNativeState) bool {
					return h.api.resumes == 1
				})
			}
			creates, resumes := h.api.creates, h.api.resumes
			h.r.SubstrateConfig.DirectEgressEnabled = false
			h.until(t, nativeTestEgressAdmissionClosed)
			if h.api.creates != creates || h.api.resumes != resumes || len(h.seeds) != 0 {
				t.Fatal("native ACP created, booted, or received credentials without direct egress acknowledgement")
			}
			h.r.SubstrateConfig.DirectEgressEnabled = true
			h.until(t, nativeTestServing)
			if h.api.creates != 1 || h.api.resumes != 1 || len(h.seeds) != 1 {
				t.Fatal("acknowledging direct egress did not admit exactly one fresh native boot")
			}
		})
	}
}

func TestNativeSubstrateDirectEgressDisabledAllowsCleanup(t *testing.T) {
	for _, action := range []string{"stop", "suspend", "delete"} {
		t.Run(action, func(t *testing.T) {
			h := newNativeRuntimeTestHarness(t)
			h.until(t, nativeTestServing)
			first := h.record(t)
			h.api.data[first.Attempt.Name] = "preserved workspace data"
			h.r.SubstrateConfig.DirectEgressEnabled = false
			h.until(t, nativeTestEgressAdmissionClosed)
			pool := runtimePoolTestGetPool(t, h.r, h.pool)
			switch action {
			case "suspend":
				substrateSuspendTestPoolIntent(t, h.r, h.pool, true)
				h.until(t, nativeTestSuspended)
				if h.api.tagData[h.record(t).Checkpoint.Name] != "preserved workspace data" {
					t.Fatal("disabled admission prevented preservation of workspace data")
				}
				// Suspension works while new cold boots remain closed.
				substrateSuspendTestPoolIntent(t, h.r, h.pool, false)
				h.until(t, nativeTestEgressAdmissionClosed)
			case "stop":
				pool.Spec.DesiredReplicas = 0
				if err := h.r.Update(t.Context(), &pool); err != nil {
					t.Fatal(err)
				}
				h.until(t, func(pool *corev1alpha1.RuntimePool, record *substrateNativeState) bool {
					return pool.Status.Lifecycle == corev1alpha1.RuntimePoolLifecycleStopped && record.Attempt == nil
				})
			case "delete":
				if err := h.r.Delete(t.Context(), &pool); err != nil {
					t.Fatal(err)
				}
				for range 70 {
					h.step(t)
					err := h.r.Get(t.Context(), types.NamespacedName{Namespace: pool.Namespace, Name: pool.Name}, &corev1alpha1.RuntimePool{})
					if apierrors.IsNotFound(err) {
						break
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				if err := h.r.Get(t.Context(), types.NamespacedName{Namespace: pool.Namespace, Name: pool.Name}, &corev1alpha1.RuntimePool{}); !apierrors.IsNotFound(err) {
					t.Fatal("disabled admission prevented RuntimePool finalization")
				}
			}
			if len(h.api.actors) != 0 || h.api.resumes != 1 || h.api.deleteWithLivePod {
				t.Fatal("disabled admission left an Actor, replayed a boot, or deleted before workload termination")
			}
			worker := first.Attempt.Worker
			if err := h.r.Get(t.Context(), types.NamespacedName{Namespace: worker.Namespace, Name: worker.Pod}, &corev1.Pod{}); !apierrors.IsNotFound(err) {
				t.Fatal("disabled admission prevented exact worker Pod cleanup")
			}
		})
	}
}
