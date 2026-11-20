package controller

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestExternalRuntimePoolPreservesHistoricalImageAuthorization(t *testing.T) {
	for _, provenance := range []bool{false, true} {
		t.Run(map[bool]string{false: "unapproved image", true: "verified historical image"}[provenance], func(t *testing.T) {
			f := newExternalRuntimePoolFixture(t)
			f.serve(t)
			pool := runtimePoolTestGetPool(t, f.r, f.pool)
			if provenance {
				meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
					Type: acpRuntimePoolImageProvenanceCondition, Status: metav1.ConditionTrue,
					ObservedGeneration: pool.Generation, Reason: acpRuntimePoolImageProvenanceReason,
					Message: "RuntimePool image and profile match a verified immutable Task execution plan",
				})
				if err := f.r.Status().Update(t.Context(), &pool); err != nil {
					t.Fatal(err)
				}
			}
			workload := f.currentWorkspace(t).Spec.Workload.DeepCopy()
			f.r.AllowedImages.Codex = "docker.io/sozercan/orka-acp@sha256:" + strings.Repeat("9", 64)
			probes := f.supervisor.probeCalls
			for range 4 {
				runtimePoolReconcile(t, f.r, f.pool)
			}
			pool = runtimePoolTestGetPool(t, f.r, f.pool)
			if provenance {
				if pool.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleServing || pool.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionAccepting || f.supervisor.probeCalls == probes {
					t.Fatalf("historically authorized image stranded after update: %#v", pool.Status)
				}
			} else if pool.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed || f.supervisor.probeCalls != probes {
				t.Fatal("unapproved historical image received runtime admission")
			}
			if !reflect.DeepEqual(f.currentWorkspace(t).Spec.Workload, workload) {
				t.Fatal("image approval recovery rewrote immutable workload intent")
			}
		})
	}
}

func TestExternalRuntimePoolRotationPreservesQueuedDemand(t *testing.T) {
	for _, cause := range []string{"provider credential", "controller epoch"} {
		t.Run(cause, func(t *testing.T) {
			f := newExternalRuntimePoolFixture(t)
			_, pod := f.serve(t)
			pool := runtimePoolTestGetPool(t, f.r, f.pool)
			pool.Status.Capacity.QueuedTasks = 1
			if err := f.r.Status().Update(t.Context(), &pool); err != nil {
				t.Fatal(err)
			}
			if cause == "provider credential" {
				f.r.ProviderProxy.BearerToken = bytes.Clone(runtimePoolTestProviderTokenNext)
			} else {
				f.r.ControllerEpoch = 9
			}
			f.supervisor.probe = runtimePoolValidProbe(f.pool, &pod, "external-boot", true)
			for range 6 {
				runtimePoolReconcile(t, f.r, f.pool)
			}
			if f.currentWorkspace(t).Spec.Retirement == nil {
				t.Fatal("queued demand blocked exact drained retirement during rotation")
			}
			pool = runtimePoolTestGetPool(t, f.r, f.pool)
			if pool.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed || pool.Status.Capacity.QueuedTasks != 1 {
				t.Fatal("rotation reopened admission or discarded queued demand before replacement")
			}
		})
	}
}

func TestExternalRuntimePoolRotationWaitsForReservationsAndFinalization(t *testing.T) {
	for _, state := range []string{"session reservation", "prompt reservation", "reservation receipt", "finalizing session"} {
		t.Run(state, func(t *testing.T) {
			f := newExternalRuntimePoolFixture(t)
			_, pod := f.serve(t)
			pool := runtimePoolTestGetPool(t, f.r, f.pool)
			pool.Status.Capacity.QueuedTasks = 1
			switch state {
			case "session reservation":
				pool.Status.Capacity.ReservedSessions = 1
			case "prompt reservation":
				pool.Status.Capacity.ReservedPrompts = 1
			case "reservation receipt":
				pool.Status.Capacity.Reservations = []corev1alpha1.RuntimePoolCapacityReservationStatus{{TaskUID: "waiting-task"}}
			case "finalizing session":
				pool.Status.Capacity.FinalizingSessions = 1
			}
			if err := f.r.Status().Update(t.Context(), &pool); err != nil {
				t.Fatal(err)
			}
			f.r.ProviderProxy.BearerToken = bytes.Clone(runtimePoolTestProviderTokenNext)
			f.supervisor.probe = runtimePoolValidProbe(f.pool, &pod, "external-boot", true)
			for range 4 {
				runtimePoolReconcile(t, f.r, f.pool)
			}
			if f.currentWorkspace(t).Spec.Retirement != nil {
				t.Fatal("rotation retired an instance before its reservations or finalization settled")
			}
			pool = runtimePoolTestGetPool(t, f.r, f.pool)
			pool.Status.Capacity.ReservedSessions = 0
			pool.Status.Capacity.ReservedPrompts = 0
			pool.Status.Capacity.Reservations = nil
			pool.Status.Capacity.FinalizingSessions = 0
			if err := f.r.Status().Update(t.Context(), &pool); err != nil {
				t.Fatal(err)
			}
			runtimePoolReconcile(t, f.r, f.pool)
			if f.currentWorkspace(t).Spec.Retirement == nil {
				t.Fatal("settled controller work failed to permit rotation with queued demand")
			}
		})
	}
}

func TestExternalRuntimePoolTerminalRetirementStillWaitsForQueuedDemand(t *testing.T) {
	for _, deleting := range []bool{false, true} {
		t.Run(map[bool]string{false: "scale down", true: "delete workspace"}[deleting], func(t *testing.T) {
			f := newExternalRuntimePoolFixture(t)
			_, pod := f.serve(t)
			pool := runtimePoolTestGetPool(t, f.r, f.pool)
			pool.Status.Capacity.QueuedTasks = 1
			if err := f.r.Status().Update(t.Context(), &pool); err != nil {
				t.Fatal(err)
			}
			if deleting {
				workspace := f.currentWorkspace(t)
				workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredDeleted
				if err := f.r.Update(t.Context(), workspace); err != nil {
					t.Fatal(err)
				}
			} else {
				pool.Spec.DesiredReplicas = 0
				if err := f.r.Update(t.Context(), &pool); err != nil {
					t.Fatal(err)
				}
			}
			f.supervisor.probe = runtimePoolValidProbe(f.pool, &pod, "external-boot", true)
			for range 4 {
				runtimePoolReconcile(t, f.r, f.pool)
			}
			if f.currentWorkspace(t).Spec.Retirement != nil {
				t.Fatal("terminal retirement ignored unsettled queued Tasks")
			}
		})
	}
}
