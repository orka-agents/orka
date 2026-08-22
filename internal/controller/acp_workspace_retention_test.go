/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	acpworkspacev1alpha1 "github.com/orka-agents/orka/api/acp.workspace/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
)

func retentionTestWorkspace(t *testing.T, name string, mutate ...func(*workspacev1alpha1.ExecutionWorkspace)) *workspacev1alpha1.ExecutionWorkspace {
	t.Helper()
	workspace := acpAdapterWorkspace(t, "")
	workspace.Name = name
	workspace.UID = types.UID(name + "-uid")
	workspace.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour))
	workspace.Spec.Lifecycle.IdleTimeout = &metav1.Duration{Duration: 30 * time.Minute}
	workspace.Spec.Lifecycle.MaxLifetime = &metav1.Duration{Duration: 24 * time.Hour}
	for _, m := range mutate {
		m(workspace)
	}
	return workspace
}

func reconcileRetention(t *testing.T, c client.Client, workspace *workspacev1alpha1.ExecutionWorkspace) ctrl.Result {
	t.Helper()
	reconciler := &ACPWorkspaceRetentionReconciler{Client: c}
	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name},
	})
	if err != nil {
		t.Fatalf("retention reconcile: %v", err)
	}
	return result
}

func TestACPWorkspaceRetentionExpiresSuspendedWorkspaces(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	workspace := retentionTestWorkspace(t, "acp-ws-retention-a", func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
		w.Annotations[acpWorkspaceLastDetachedAnnotation] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	})
	c := acpAdapterTestClient(t, workspace)
	reconcileRetention(t, c, workspace)
	err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, &workspacev1alpha1.ExecutionWorkspace{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("suspended workspace past its idle retention must be deleted, got %v", err)
	}
}

func TestACPWorkspaceRetentionKeepsFreshWorkspaces(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	workspace := retentionTestWorkspace(t, "acp-ws-retention-b", func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
		w.Annotations[acpWorkspaceLastDetachedAnnotation] = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	})
	c := acpAdapterTestClient(t, workspace)
	result := reconcileRetention(t, c, workspace)
	if result.RequeueAfter <= 0 {
		t.Fatalf("a fresh workspace must requeue for its future deadline, got %+v", result)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, &workspacev1alpha1.ExecutionWorkspace{}); err != nil {
		t.Fatalf("fresh workspace must survive: %v", err)
	}
}

func TestACPWorkspaceRetentionEnforcesMaxLifetimeEvenWhileAttached(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	workspace := retentionTestWorkspace(t, "acp-ws-retention-c", func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.CreationTimestamp = metav1.NewTime(time.Now().Add(-25 * time.Hour))
		w.Spec.AttachmentEpoch = 1
		w.Spec.Attachment = &workspacev1alpha1.ExecutionWorkspaceAttachment{
			TaskRef:        workspacev1alpha1.ObjectIdentityReference{Name: "attached-task", UID: types.UID("task-uid")},
			Epoch:          1,
			TokenSHA256:    "sha256:" + strings.Repeat("d", 64),
			TokenSecretRef: workspacev1alpha1.SecretReference{Name: "attach"},
			ExpiresAt:      metav1.Now(),
		}
	})
	c := acpAdapterTestClient(t, workspace)
	reconcileRetention(t, c, workspace)
	err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, &workspacev1alpha1.ExecutionWorkspace{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("maxLifetime must force terminal cleanup even while attached, got %v", err)
	}
}

func TestACPWorkspaceRetentionSuspendsIdleReadyWorkspaces(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	workspace := retentionTestWorkspace(t, "acp-ws-retention-d", func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Spec.Lifecycle.DefaultOnDetach = workspacev1alpha1.WorkspaceOnDetachSuspend
		w.Spec.Lifecycle.AllowedOnDetach = []workspacev1alpha1.WorkspaceOnDetach{
			workspacev1alpha1.WorkspaceOnDetachSuspend, workspacev1alpha1.WorkspaceOnDetachDelete,
		}
		w.Annotations[acpWorkspaceDetachActionAnnotation] = string(workspacev1alpha1.WorkspaceOnDetachSuspend)
		w.Annotations[acpWorkspaceLastDetachedAnnotation] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	})
	c := acpAdapterTestClient(t, workspace)
	reconcileRetention(t, c, workspace)
	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, current); err != nil {
		t.Fatalf("idle suspendable workspace must survive as suspended: %v", err)
	}
	if current.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredSuspended {
		t.Fatalf("desired state = %s, want Suspended", current.Spec.DesiredState)
	}
}

func TestACPWorkspaceRetentionDeletesIdleReadyDeleteClasses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	workspace := retentionTestWorkspace(t, "acp-ws-retention-e", func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Annotations[acpWorkspaceLastDetachedAnnotation] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	})
	c := acpAdapterTestClient(t, workspace)
	reconcileRetention(t, c, workspace)
	err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, &workspacev1alpha1.ExecutionWorkspace{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("idle Delete-class workspace must be deleted, got %v", err)
	}
}

func TestACPWorkspaceRetentionIgnoresForeignWorkspaces(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	workspace := retentionTestWorkspace(t, "acp-ws-retention-f", func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Labels = nil
		w.Annotations[acpWorkspaceLastDetachedAnnotation] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	})
	c := acpAdapterTestClient(t, workspace)
	reconcileRetention(t, c, workspace)
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, &workspacev1alpha1.ExecutionWorkspace{}); err != nil {
		t.Fatalf("a foreign workspace must never be retention-managed: %v", err)
	}
}

func TestSettleACPClassWorkspaceEnforcesSuspendQuota(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := suspendableSubstrateFixture(t)
	fixture.profile.Spec.Retention = &acpworkspacev1alpha1.RetentionPolicy{
		MaxSuspendedWorkspaces: func() *int32 { limit := int32(1); return &limit }(),
	}
	fixture.pinProfileHash(t)
	task := suspendableSessionTask()
	// An existing suspended workspace of the same class consumes the cap.
	other := acpAdapterWorkspace(t, "")
	other.Name = "acp-ws-existing-suspended"
	other.UID = types.UID("existing-suspended-uid")
	other.Spec.ClassBinding = workspacev1alpha1.ImmutableObjectBinding{
		Name: fixture.class.Name, UID: fixture.class.UID, Generation: 1, ProfileHash: fixture.class.Status.ProfileHash,
	}
	other.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
	r := acpClassTestReconciler(t, append(fixture.objects(), task, other)...)

	// Admission rejects a prospective Suspend beyond the cap.
	if _, err := r.resolveACPWorkspaceClass(ctx, task); err == nil ||
		!strings.Contains(err.Error(), "retention cap") {
		t.Fatalf("admission error = %v, want retention cap exhaustion", err)
	}

	// With headroom the Task admits; settlement then re-checks the live count
	// and falls back to the frozen Delete disposition when the cap is gone.
	if err := r.Delete(ctx, other); err != nil {
		t.Fatalf("free the cap: %v", err)
	}
	resolved, err := r.resolveACPWorkspaceClass(ctx, task)
	if err != nil {
		t.Fatalf("resolve class with headroom: %v", err)
	}
	binding, err := resolveACPWorkspaceBindingWithClass(task, "", false, "session-uid-1", resolved)
	if err != nil {
		t.Fatalf("resolve binding: %v", err)
	}
	if binding.Class.MaxSuspendedWorkspaces == nil || *binding.Class.MaxSuspendedWorkspaces != 1 {
		t.Fatalf("frozen retention cap = %v", binding.Class.MaxSuspendedWorkspaces)
	}
	plan := ACPRuntimePlan{PoolName: acpTestSessionPoolName, Workspace: binding}
	if _, _, err := r.ensureACPClassWorkspace(ctx, task, plan); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	name := acpClassWorkspaceName(task, binding)
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: name}, workspace); err != nil {
		t.Fatalf("read workspace: %v", err)
	}
	if workspace.Annotations[acpWorkspaceMaxSuspendedAnnotation] != "1" {
		t.Fatalf("frozen cap annotation = %q", workspace.Annotations[acpWorkspaceMaxSuspendedAnnotation])
	}
	admitTestACPWorkspace(t, r, workspace)
	if _, ready, err := r.ensureACPClassWorkspace(ctx, task, plan); err != nil || !ready {
		t.Fatalf("attach = (%v, %v)", ready, err)
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, task); err != nil {
		t.Fatalf("reload task: %v", err)
	}

	// Another suspended workspace appears before settlement; the cap is gone.
	competitor := acpAdapterWorkspace(t, "")
	competitor.Name = "acp-ws-competitor-suspended"
	competitor.UID = types.UID("competitor-suspended-uid")
	competitor.Spec.ClassBinding = workspace.Spec.ClassBinding
	competitor.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
	if err := r.Create(ctx, competitor); err != nil {
		t.Fatalf("create competitor: %v", err)
	}
	done, err := r.settleACPClassWorkspace(ctx, task)
	if err != nil || !done {
		t.Fatalf("settle = (%v, %v)", done, err)
	}
	err = r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: name}, workspace)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("quota-exhausted settlement must apply the Delete disposition, got %v", err)
	}
}
