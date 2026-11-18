// Copyright (c) 2026. MIT License - see LICENSE file for details.
package controller

import (
	"testing"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ADR 0056: a Substrate profile that opts in to shared workers places several
// routed native processes on one worker Pod. Core's fences for a routed
// process name the Actor and the opaque per-sequence runtime UID; the worker
// Pod is only an attestation and eviction fence. These pin that Core neither
// requires the worker to be exclusive to, or labeled for, one instance, nor
// observes worker Pod absence to accept the provider's exact authorized
// termination claim.
func TestExternalRoutedStartupAcceptsAWorkerSharedWithAnotherInstance(t *testing.T) {
	f := newRoutedRuntimeFixture(t, true, nil)
	ctx := t.Context()
	// A shared worker carries only its pool's labels, never one instance's
	// allocation or instance identity, and another Pod may match them.
	shared := map[string]string{"ate.dev/worker-pool": "ate-shared-0123", "substrate.workspace.orka.ai/shared-pool": "0123"}
	worker := &corev1.Pod{}
	if err := f.r.Get(ctx, client.ObjectKeyFromObject(&f.worker), worker); err != nil {
		t.Fatal(err)
	}
	worker.Labels = cloneStringMap(shared)
	if err := f.r.Update(ctx, worker); err != nil {
		t.Fatal(err)
	}
	neighbour := runtimePoolReadyPod(f.pool, worker.Namespace, "neighbour", "neighbour-uid", "10.0.0.72")
	neighbour.Labels = cloneStringMap(shared)
	runtimePoolTestCreatePod(t, f.r, &neighbour)

	w := f.currentWorkspace(t)
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	attested, err := f.r.attestExternalWorkspaceStartup(ctx, w.Spec.Workload, w.Status.Allocation.Startup)
	if err != nil {
		t.Fatalf("routed startup on a shared worker was refused: %v", err)
	}
	if attested.UID != externalNativeRuntimeUID(w.Spec.Workload) || attested.UID == worker.UID {
		t.Fatalf("routed instance is fenced by the worker Pod instead of its opaque runtime UID: %s", attested.UID)
	}
	if err := f.r.bindExternalRuntimeInstanceEvidence(ctx, &pool, w); err != nil {
		t.Fatal(err)
	}
	// The provider's exact, Core-authorized terminal claim is accepted while
	// the shared worker Pod keeps running its neighbours.
	pool = runtimePoolTestGetPool(t, f.r, f.pool)
	w = f.currentWorkspace(t)
	w.Spec.Retirement = &workspacev1alpha1.WorkloadRetirement{Sequence: w.Spec.Workload.Sequence, Identity: w.Status.Allocation.Identity, Action: workspacev1alpha1.WorkloadRetirementSuspend}
	w.Status.Allocation.Startup = nil
	w.Status.Allocation.State = workspacev1alpha1.AllocationStopped
	stopped, err := f.r.externalObservedInstanceTerminated(ctx, &pool, w)
	if err != nil || !stopped {
		t.Fatalf("authorized native termination on a shared worker was not accepted: %v/%v", stopped, err)
	}
	if err := f.r.Get(ctx, client.ObjectKeyFromObject(worker), &corev1.Pod{}); err != nil {
		t.Fatalf("core removed the shared worker: %v", err)
	}
	// Without Core's authorization the same claim never releases credentials.
	w.Spec.Retirement = nil
	if stopped, _ := f.r.externalObservedInstanceTerminated(ctx, &pool, w); stopped {
		t.Fatal("an unauthorized termination claim on a shared worker was accepted")
	}
}
