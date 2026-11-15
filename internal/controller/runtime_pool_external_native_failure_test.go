// Copyright (c) 2026. MIT License - see LICENSE file for details.

package controller

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
)

// failExternalNativeInstance models the provider's terminal report for a lost
// native process: startup is withdrawn and the exact fence is preserved.
func failExternalNativeInstance(t *testing.T, f *externalRuntimePoolFixture, mutate func(*workspacev1alpha1.ExecutionWorkspace)) {
	t.Helper()
	w := f.currentWorkspace(t)
	// Settlement already asked to keep the data; a failed instance has none.
	w.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
	if err := f.r.Update(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	w = f.currentWorkspace(t)
	w.Status.ObservedGeneration = w.Generation
	w.Status.State = workspacev1alpha1.ExecutionWorkspaceStateFailed
	w.Status.Allocation.State = workspacev1alpha1.AllocationPending
	w.Status.Allocation.Startup = nil
	if mutate != nil {
		mutate(w)
	}
	if err := f.r.Status().Update(t.Context(), w); err != nil {
		t.Fatal(err)
	}
}

func TestExternalNativeFailedInstanceRetiresWithoutDrainAndIsNeverReplaced(t *testing.T) {
	f, w, _, _ := nativeRecycleReviewFixture(t)
	identity, sequence := w.Status.Allocation.Identity, w.Spec.Workload.Sequence
	f.supervisor.probeErr = errors.New("lost native process cannot answer")
	failExternalNativeInstance(t, f, nil)
	for range 4 {
		runtimePoolReconcile(t, f.r, f.pool)
	}
	w = f.currentWorkspace(t)
	want := &workspacev1alpha1.WorkloadRetirement{Sequence: sequence, Identity: identity, Action: workspacev1alpha1.WorkloadRetirementStop}
	if !reflect.DeepEqual(w.Spec.Retirement, want) {
		t.Fatalf("failed native instance was not retired by exact Stop: %#v", w.Spec.Retirement)
	}
	if f.supervisor.drainCalls != 0 {
		t.Fatal("Core requested a drain from a lost native process")
	}
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	if pool.Status.ActiveInstance == nil || pool.Status.AdmissionState == corev1alpha1.RuntimePoolAdmissionAccepting {
		t.Fatalf("runtime admission or credentials changed before provider termination: %#v", pool.Status)
	}

	w.Status.Allocation.State = workspacev1alpha1.AllocationStopped
	w.Status.ObservedGeneration = w.Generation
	if err := f.r.Status().Update(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		runtimePoolReconcile(t, f.r, f.pool)
	}
	pool = runtimePoolTestGetPool(t, f.r, f.pool)
	if pool.Status.ActiveInstance != nil || pool.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleStopped {
		t.Fatalf("confirmed native termination did not release the instance: %#v", pool.Status)
	}
	if w = f.currentWorkspace(t); w.Spec.Workload.Sequence != sequence || !reflect.DeepEqual(w.Spec.Retirement, want) {
		t.Fatalf("failed native workspace was replaced: sequence %d retirement %#v", w.Spec.Workload.Sequence, w.Spec.Retirement)
	}
}

func TestExternalNativeFailedClaimForAnotherInstanceKeepsDrainFence(t *testing.T) {
	f, _, _, _ := nativeRecycleReviewFixture(t)
	f.supervisor.probeErr = errors.New("unreachable")
	failExternalNativeInstance(t, f, func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Status.Allocation.Identity.InstanceID = "another-native-instance"
	})
	for range 3 {
		runtimePoolReconcile(t, f.r, f.pool)
	}
	if w := f.currentWorkspace(t); w.Spec.Retirement != nil {
		t.Fatalf("a Failed claim for another instance authorized retirement: %#v", w.Spec.Retirement)
	}
	if pool := runtimePoolTestGetPool(t, f.r, f.pool); pool.Status.ActiveInstance == nil {
		t.Fatal("a Failed claim for another instance released the bound instance")
	}
}

func TestSettleACPClassWorkspaceKeepsFailedIncarnationWithRetainedData(t *testing.T) {
	t.Parallel()
	for _, retained := range []bool{true, false} {
		t.Run(map[bool]string{true: "retained", false: "fresh"}[retained], func(t *testing.T) {
			ctx := context.Background()
			fixture := newACPClassFixture(t, RuntimeProviderBackendAgentSandbox)
			task := acpClassTestTask()
			r := acpClassTestReconciler(t, append(fixture.objects(), task)...)
			resolved, err := r.resolveACPWorkspaceClass(ctx, task)
			if err != nil {
				t.Fatal(err)
			}
			binding, err := resolveACPWorkspaceBindingWithClass(task, "", resolved)
			if err != nil {
				t.Fatal(err)
			}
			plan := ACPRuntimePlan{PoolName: acpTestSandboxPoolName, Workspace: binding}
			if _, _, err := r.ensureACPClassWorkspace(ctx, task, plan); err != nil {
				t.Fatal(err)
			}
			key := types.NamespacedName{Namespace: task.Namespace, Name: acpClassWorkspaceName(task, binding)}
			workspace := &workspacev1alpha1.ExecutionWorkspace{}
			if err := r.Get(ctx, key, workspace); err != nil {
				t.Fatal(err)
			}
			admitTestACPWorkspace(t, r, workspace)
			if _, ready := attachTestACPWorkspace(t, r, task, plan, workspace.Name); !ready {
				t.Fatal("attach failed")
			}
			if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, task); err != nil {
				t.Fatal(err)
			}
			// The Task executed on the workspace's pool before the instance failed.
			task.Status.Execution = &corev1alpha1.TaskExecutionStatus{RuntimePoolName: acpTestSandboxPoolName}
			if err := r.Status().Update(ctx, task); err != nil {
				t.Fatal(err)
			}
			if err := r.Get(ctx, key, workspace); err != nil {
				t.Fatal(err)
			}
			// The Session Task froze Suspend; its incarnation resumed from a
			// verified checkpoint unless it is a fresh first boot.
			workspace.Annotations[acpWorkspaceDetachActionAnnotation] = string(workspacev1alpha1.WorkspaceOnDetachSuspend)
			workspace.Spec.Workload = &workspacev1alpha1.WorkloadRequest{Sequence: 2}
			if retained {
				workspace.Spec.Workload.RetainedData = &workspacev1alpha1.RetainedDataReference{ID: "orka-data-checkpoint",
					SourceInstance: workspacev1alpha1.InstanceIdentity{AllocationID: "allocation", InstanceID: "predecessor", RequestRevision: "sha256:" + strings.Repeat("a", 64)},
					ProofSHA256:    "sha256:" + strings.Repeat("b", 64)}
			}
			if err := r.Update(ctx, workspace); err != nil {
				t.Fatal(err)
			}
			workspace.Status.State = workspacev1alpha1.ExecutionWorkspaceStateFailed
			if err := r.Status().Update(ctx, workspace); err != nil {
				t.Fatal(err)
			}
			// The first pass revokes the attachment; the adapter then releases
			// the enforced epoch.
			if done, err := r.settleACPClassWorkspace(ctx, task); err != nil || done {
				t.Fatalf("revocation pass = (%v, %v)", done, err)
			}
			if err := r.Get(ctx, key, workspace); err != nil {
				t.Fatal(err)
			}
			workspace.Status.AttachedEpoch = 0
			workspace.Status.Conditions = nil
			if err := r.Status().Update(ctx, workspace); err != nil {
				t.Fatal(err)
			}
			if done, err := r.settleACPClassWorkspace(ctx, task); err != nil || !done {
				t.Fatalf("failed workspace settlement = (%v, %v)", done, err)
			}
			err = r.Get(ctx, key, workspace)
			if retained && (err != nil || !workspace.DeletionTimestamp.IsZero() || workspace.Spec.Attachment != nil) {
				t.Fatalf("failed incarnation with a checkpoint was not kept for recovery: %v", err)
			}
			if !retained && !apierrors.IsNotFound(err) {
				t.Fatalf("failed incarnation without a checkpoint was not deleted: %v", err)
			}
		})
	}
}

func TestSettleACPClassWorkspaceLeavesDeletedDesiredStateAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newACPClassFixture(t, RuntimeProviderBackendAgentSandbox)
	task := acpClassTestTask()
	r := acpClassTestReconciler(t, append(fixture.objects(), task)...)
	resolved, err := r.resolveACPWorkspaceClass(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := resolveACPWorkspaceBindingWithClass(task, "", resolved)
	if err != nil {
		t.Fatal(err)
	}
	plan := ACPRuntimePlan{PoolName: acpTestSandboxPoolName, Workspace: binding}
	if _, _, err := r.ensureACPClassWorkspace(ctx, task, plan); err != nil {
		t.Fatal(err)
	}
	key := types.NamespacedName{Namespace: task.Namespace, Name: acpClassWorkspaceName(task, binding)}
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	if err := r.Get(ctx, key, workspace); err != nil {
		t.Fatal(err)
	}
	admitTestACPWorkspace(t, r, workspace)
	if _, ready := attachTestACPWorkspace(t, r, task, plan, workspace.Name); !ready {
		t.Fatal("attach failed")
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, task); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, key, workspace); err != nil {
		t.Fatal(err)
	}
	// Deleting the linked RuntimePool retires the workspace this way; the API
	// server rejects any later desired state.
	workspace.Annotations[acpWorkspaceDetachActionAnnotation] = string(workspacev1alpha1.WorkspaceOnDetachSuspend)
	workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredDeleted
	workspace.Spec.Attachment = nil
	if err := r.Update(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, key, workspace); err != nil {
		t.Fatal(err)
	}
	before := workspace.ResourceVersion
	if done, err := r.settleACPClassWorkspace(ctx, task); err != nil || !done {
		t.Fatalf("settlement of a Deleted workspace = (%v, %v)", done, err)
	}
	if err := r.Get(ctx, key, workspace); err != nil {
		t.Fatal(err)
	}
	if workspace.ResourceVersion != before || workspace.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredDeleted {
		t.Fatal("settlement wrote to a workspace whose Deleted desired state is terminal")
	}
}

func TestEnsureACPClassWorkspaceWaitsForDeletedPredecessor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newACPClassFixture(t, RuntimeProviderBackendAgentSandbox)
	task := acpClassTestTask()
	r := acpClassTestReconciler(t, append(fixture.objects(), task)...)
	resolved, err := r.resolveACPWorkspaceClass(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := resolveACPWorkspaceBindingWithClass(task, "", resolved)
	if err != nil {
		t.Fatal(err)
	}
	plan := ACPRuntimePlan{PoolName: acpTestSandboxPoolName, Workspace: binding}
	if _, _, err := r.ensureACPClassWorkspace(ctx, task, plan); err != nil {
		t.Fatal(err)
	}
	key := types.NamespacedName{Namespace: task.Namespace, Name: acpClassWorkspaceName(task, binding)}
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	if err := r.Get(ctx, key, workspace); err != nil {
		t.Fatal(err)
	}
	workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredDeleted
	if err := r.Update(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	if _, ready, err := r.ensureACPClassWorkspace(ctx, task, plan); err != nil || ready {
		t.Fatalf("Task against a Deleted predecessor = (ready %v, %v), want a wait for its replacement", ready, err)
	}
}
