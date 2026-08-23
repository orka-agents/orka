/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	acpworkspacev1alpha1 "github.com/orka-agents/orka/api/acp.workspace/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
)

func suspendableSubstrateFixture(t *testing.T) *acpClassFixture {
	t.Helper()
	return newACPClassFixture(t, acpworkspacev1alpha1.RuntimeProviderBackendSubstrate, func(f *acpClassFixture) {
		f.profile.Spec.Substrate.Suspend = &acpworkspacev1alpha1.SubstrateSuspendPolicy{
			Mode: acpworkspacev1alpha1.SubstrateSuspendModeDataOnly,
		}
		f.class.Spec.Lifecycle.DefaultOnDetach = workspacev1alpha1.WorkspaceOnDetachSuspend
		f.class.Spec.Lifecycle.AllowedOnDetach = []workspacev1alpha1.WorkspaceOnDetach{
			workspacev1alpha1.WorkspaceOnDetachSuspend, workspacev1alpha1.WorkspaceOnDetachDelete,
		}
	})
}

func suspendableSessionTask() *corev1alpha1.Task {
	return acpClassTestTask(func(task *corev1alpha1.Task) {
		task.Spec.Execution.Workspace.ReusePolicy = corev1alpha1.WorkspaceReusePolicySession
		task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: acpTestSessionName, Create: true}
	})
}

func TestResolveACPClassWorkspaceBindingAdmitsDataOnlySuspend(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := suspendableSubstrateFixture(t)
	r := acpClassTestReconciler(t, fixture.objects()...)
	resolved, err := r.resolveACPWorkspaceClass(ctx, acpClassTestTask())
	if err != nil {
		t.Fatalf("resolve class: %v", err)
	}
	if resolved.SubstrateSuspendMode != string(acpworkspacev1alpha1.SubstrateSuspendModeDataOnly) {
		t.Fatalf("suspend mode = %q", resolved.SubstrateSuspendMode)
	}

	binding, err := resolveACPWorkspaceBindingWithClass(suspendableSessionTask(), "", false, "session-uid-1", resolved)
	if err != nil {
		t.Fatalf("resolve suspendable binding: %v", err)
	}
	if binding.Class.EffectiveOnDetach != string(workspacev1alpha1.WorkspaceOnDetachSuspend) ||
		binding.Class.SuspendMode != string(acpworkspacev1alpha1.SubstrateSuspendModeDataOnly) {
		t.Fatalf("class binding = %+v", binding.Class)
	}
	if err := validateACPWorkspaceBindingValues(binding); err != nil {
		t.Fatalf("frozen binding validation: %v", err)
	}

	frozen := snapshotWorkspaceClassFromBinding(binding.Class)
	rebuilt := workspaceClassBindingFromSnapshot(frozen)
	if rebuilt.SuspendMode != binding.Class.SuspendMode {
		t.Fatalf("snapshot round trip dropped the suspension mode")
	}
}

func TestResolveACPClassWorkspaceBindingSuspendRejections(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("per-task reuse cannot suspend", func(t *testing.T) {
		t.Parallel()
		fixture := suspendableSubstrateFixture(t)
		r := acpClassTestReconciler(t, fixture.objects()...)
		resolved, err := r.resolveACPWorkspaceClass(ctx, acpClassTestTask())
		if err != nil {
			t.Fatalf("resolve class: %v", err)
		}
		if _, err := resolveACPWorkspaceBindingWithClass(acpClassTestTask(), "", false, "", resolved); err == nil ||
			!strings.Contains(err.Error(), "requires reusePolicy session") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("suspend without a data-only policy stays rejected", func(t *testing.T) {
		t.Parallel()
		fixture := newACPClassFixture(t, acpworkspacev1alpha1.RuntimeProviderBackendSubstrate, func(f *acpClassFixture) {
			f.class.Spec.Lifecycle.DefaultOnDetach = workspacev1alpha1.WorkspaceOnDetachSuspend
			f.class.Spec.Lifecycle.AllowedOnDetach = []workspacev1alpha1.WorkspaceOnDetach{
				workspacev1alpha1.WorkspaceOnDetachSuspend, workspacev1alpha1.WorkspaceOnDetachDelete,
			}
		})
		r := acpClassTestReconciler(t, fixture.objects()...)
		resolved, err := r.resolveACPWorkspaceClass(ctx, acpClassTestTask())
		if err != nil {
			t.Fatalf("resolve class: %v", err)
		}
		if _, err := resolveACPWorkspaceBindingWithClass(suspendableSessionTask(), "", false, "session-uid-1", resolved); err == nil ||
			!strings.Contains(err.Error(), "permits DataOnly suspension") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("tampered frozen suspend binding fails validation", func(t *testing.T) {
		t.Parallel()
		fixture := suspendableSubstrateFixture(t)
		r := acpClassTestReconciler(t, fixture.objects()...)
		resolved, err := r.resolveACPWorkspaceClass(ctx, acpClassTestTask())
		if err != nil {
			t.Fatalf("resolve class: %v", err)
		}
		binding, err := resolveACPWorkspaceBindingWithClass(suspendableSessionTask(), "", false, "session-uid-1", resolved)
		if err != nil {
			t.Fatalf("resolve binding: %v", err)
		}
		tampered := *binding
		tamperedClass := *binding.Class
		tamperedClass.SuspendMode = ""
		tampered.Class = &tamperedClass
		if err := validateACPWorkspaceBindingValues(&tampered); err == nil {
			t.Fatal("a Suspend binding without its DataOnly policy must fail closed")
		}
	})
}

func TestEnsureACPClassWorkspaceResumesSuspendedWorkspace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := suspendableSubstrateFixture(t)
	task := suspendableSessionTask()
	r := acpClassTestReconciler(t, append(fixture.objects(), task)...)
	resolved, err := r.resolveACPWorkspaceClass(ctx, task)
	if err != nil {
		t.Fatalf("resolve class: %v", err)
	}
	binding, err := resolveACPWorkspaceBindingWithClass(task, "", false, "session-uid-1", resolved)
	if err != nil {
		t.Fatalf("resolve binding: %v", err)
	}
	plan := ACPRuntimePlan{PoolName: "acp-ws-session-0123456789abcdef", Workspace: binding}
	if _, _, err := r.ensureACPClassWorkspace(ctx, task, plan); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	name := acpClassWorkspaceName(task, binding)
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: name}, workspace); err != nil {
		t.Fatalf("read workspace: %v", err)
	}
	base := workspace.DeepCopy()
	workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
	if err := r.Patch(ctx, workspace, client.MergeFrom(base)); err != nil {
		t.Fatalf("suspend workspace: %v", err)
	}

	// While the backend is still draining or checkpointing, the resume waits:
	// the desired state stays Suspended and no attachment is offered.
	workspace.Status.State = workspacev1alpha1.ExecutionWorkspaceStateSuspending
	if err := r.Status().Update(ctx, workspace); err != nil {
		t.Fatalf("mark suspending: %v", err)
	}
	if _, ready, err := r.ensureACPClassWorkspace(ctx, task, plan); err != nil || ready {
		t.Fatalf("ensure over a settling suspension = (%v, %v), want a quiet wait", ready, err)
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: name}, workspace); err != nil {
		t.Fatalf("read settling workspace: %v", err)
	}
	if workspace.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredSuspended {
		t.Fatalf("desired state = %s, want the suspension left to finish", workspace.Spec.DesiredState)
	}

	// Once the checkpoint settles, the continuation flips the workspace back
	// to Ready for a cold resume.
	workspace.Status.State = workspacev1alpha1.ExecutionWorkspaceStateSuspended
	if err := r.Status().Update(ctx, workspace); err != nil {
		t.Fatalf("mark suspended: %v", err)
	}
	if _, ready, err := r.ensureACPClassWorkspace(ctx, task, plan); err != nil || ready {
		t.Fatalf("ensure over a suspended workspace = (%v, %v), want a resume request", ready, err)
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: name}, workspace); err != nil {
		t.Fatalf("read resumed workspace: %v", err)
	}
	if workspace.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady {
		t.Fatalf("desired state = %s, want Ready", workspace.Spec.DesiredState)
	}
}

func TestEnsureACPClassWorkspaceFailsClosedOnFailedSuspension(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := suspendableSubstrateFixture(t)
	task := suspendableSessionTask()
	r := acpClassTestReconciler(t, append(fixture.objects(), task)...)
	resolved, err := r.resolveACPWorkspaceClass(ctx, task)
	if err != nil {
		t.Fatalf("resolve class: %v", err)
	}
	binding, err := resolveACPWorkspaceBindingWithClass(task, "", false, "session-uid-1", resolved)
	if err != nil {
		t.Fatalf("resolve binding: %v", err)
	}
	plan := ACPRuntimePlan{PoolName: "acp-ws-session-0123456789abcdef", Workspace: binding}
	if _, _, err := r.ensureACPClassWorkspace(ctx, task, plan); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	name := acpClassWorkspaceName(task, binding)
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: name}, workspace); err != nil {
		t.Fatalf("read workspace: %v", err)
	}
	base := workspace.DeepCopy()
	workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
	if err := r.Patch(ctx, workspace, client.MergeFrom(base)); err != nil {
		t.Fatalf("suspend workspace: %v", err)
	}
	workspace.Status.State = workspacev1alpha1.ExecutionWorkspaceStateFailed
	if err := r.Status().Update(ctx, workspace); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	if _, _, err := r.ensureACPClassWorkspace(ctx, task, plan); err == nil ||
		!strings.Contains(err.Error(), "cold resume is impossible") {
		t.Fatalf("error = %v, want fail-closed resume rejection", err)
	}
}

func TestSettleACPClassWorkspaceAppliesSuspendAction(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := suspendableSubstrateFixture(t)
	task := suspendableSessionTask()
	r := acpClassTestReconciler(t, append(fixture.objects(), task)...)
	resolved, err := r.resolveACPWorkspaceClass(ctx, task)
	if err != nil {
		t.Fatalf("resolve class: %v", err)
	}
	binding, err := resolveACPWorkspaceBindingWithClass(task, "", false, "session-uid-1", resolved)
	if err != nil {
		t.Fatalf("resolve binding: %v", err)
	}
	plan := ACPRuntimePlan{PoolName: "acp-ws-session-0123456789abcdef", Workspace: binding}
	if _, _, err := r.ensureACPClassWorkspace(ctx, task, plan); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	name := acpClassWorkspaceName(task, binding)
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: name}, workspace); err != nil {
		t.Fatalf("read workspace: %v", err)
	}
	admitTestACPWorkspace(t, r, workspace)
	if _, ready, err := r.ensureACPClassWorkspace(ctx, task, plan); err != nil || !ready {
		t.Fatalf("attach = (%v, %v)", ready, err)
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: name}, workspace); err != nil {
		t.Fatalf("read attached workspace: %v", err)
	}
	if workspace.Annotations[acpWorkspaceDetachActionAnnotation] != string(workspacev1alpha1.WorkspaceOnDetachSuspend) {
		t.Fatalf("detach action annotation = %q", workspace.Annotations[acpWorkspaceDetachActionAnnotation])
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, task); err != nil {
		t.Fatalf("reload task: %v", err)
	}

	// Settlement is a multi-reconcile flow: revocation start intentionally
	// returns not-done so the next reconcile re-reads uncached state.
	done := false
	for attempt := 0; attempt < 5 && !done; attempt++ {
		var err error
		if done, err = r.settleACPClassWorkspace(ctx, task); err != nil {
			t.Fatalf("settle attempt %d: %v", attempt, err)
		}
	}
	if !done {
		t.Fatal("settle never completed")
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: name}, workspace); err != nil {
		t.Fatalf("workspace must survive a Suspend detach: %v", err)
	}
	if workspace.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredSuspended {
		t.Fatalf("desired state = %s, want Suspended", workspace.Spec.DesiredState)
	}
	if workspace.Spec.Attachment != nil {
		t.Fatal("attachment must be revoked before suspension")
	}

	// Settlement is idempotent on an already suspended workspace.
	if done, err := r.settleACPClassWorkspace(ctx, task); err != nil || !done {
		t.Fatalf("repeat settle = (%v, %v)", done, err)
	}
}

func TestACPExecutionWorkspaceAdapterDrivesSuspension(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	provider := acpAdapterProvider()
	workspace := acpAdapterWorkspace(t, "acp-ws-pool")
	workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
	pool := acpAdapterLinkedPool(workspace.Namespace, workspace.Name)
	pool.Spec.ExecutionWorkspace.Provider = corev1alpha1.WorkspaceProviderSubstrate
	pool.Spec.ExecutionWorkspace.Substrate = &corev1alpha1.RuntimePoolSubstrateWorkspaceSpec{
		BaseTemplateNamespace: acpTestSubstrateNamespace, BaseTemplateName: acpTestInfraName,
		SuspendMode: string(acpworkspacev1alpha1.SubstrateSuspendModeDataOnly),
	}
	pool.Spec.DesiredReplicas = 1
	c := acpAdapterTestClient(t, provider, workspace, pool)

	reconcileACPWorkspaceAdapter(t, c, workspace)
	current := &corev1alpha1.RuntimePool{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: pool.Namespace, Name: pool.Name}, current); err != nil {
		t.Fatalf("read pool: %v", err)
	}
	if current.Annotations[substrateWorkspaceSuspendAnnotation] == "" || current.Spec.DesiredReplicas != 0 {
		t.Fatalf("pool intent = %+v replicas=%d, want suspension intent at zero replicas", current.Annotations, current.Spec.DesiredReplicas)
	}

	// The backend completes the checkpoint: consent recorded, pool Stopped.
	base := current.DeepCopy()
	current.Annotations[substrateActorSuspendedAnnotation] = "actor"
	if err := c.Patch(ctx, current, client.MergeFrom(base)); err != nil {
		t.Fatalf("record consent: %v", err)
	}
	baseStatus := current.DeepCopy()
	current.Status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStopped
	current.Status.ObservedGeneration = current.Generation
	if err := c.Status().Patch(ctx, current, client.MergeFrom(baseStatus)); err != nil {
		t.Fatalf("mark pool stopped: %v", err)
	}
	reconcileACPWorkspaceAdapter(t, c, workspace)
	updated := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, updated); err != nil {
		t.Fatalf("read workspace: %v", err)
	}
	if updated.Status.State != workspacev1alpha1.ExecutionWorkspaceStateSuspended {
		t.Fatalf("state = %s, want Suspended", updated.Status.State)
	}

	// Resume: desired Ready lifts the intent and restores the desired scale.
	baseSpec := updated.DeepCopy()
	updated.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredReady
	if err := c.Patch(ctx, updated, client.MergeFrom(baseSpec)); err != nil {
		t.Fatalf("request resume: %v", err)
	}
	reconcileACPWorkspaceAdapter(t, c, updated)
	if err := c.Get(ctx, types.NamespacedName{Namespace: pool.Namespace, Name: pool.Name}, current); err != nil {
		t.Fatalf("read resumed pool: %v", err)
	}
	if current.Annotations[substrateWorkspaceSuspendAnnotation] != "" || current.Spec.DesiredReplicas != 1 {
		t.Fatalf("resume intent = %+v replicas=%d, want lifted intent at one replica", current.Annotations, current.Spec.DesiredReplicas)
	}
}

func TestACPExecutionWorkspaceAdapterFailsSuspensionWithoutConsent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	provider := acpAdapterProvider()
	workspace := acpAdapterWorkspace(t, "acp-ws-pool")
	workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
	pool := acpAdapterLinkedPool(workspace.Namespace, workspace.Name)
	pool.Spec.ExecutionWorkspace.Provider = corev1alpha1.WorkspaceProviderSubstrate
	pool.Spec.ExecutionWorkspace.Substrate = &corev1alpha1.RuntimePoolSubstrateWorkspaceSpec{
		BaseTemplateNamespace: acpTestSubstrateNamespace, BaseTemplateName: acpTestInfraName,
		SuspendMode: string(acpworkspacev1alpha1.SubstrateSuspendModeDataOnly),
	}
	c := acpAdapterTestClient(t, provider, workspace, pool)
	reconcileACPWorkspaceAdapter(t, c, workspace)

	current := &corev1alpha1.RuntimePool{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: pool.Namespace, Name: pool.Name}, current); err != nil {
		t.Fatalf("read pool: %v", err)
	}
	base := current.DeepCopy()
	current.Status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStopped
	current.Status.ObservedGeneration = current.Generation
	if err := c.Status().Patch(ctx, current, client.MergeFrom(base)); err != nil {
		t.Fatalf("mark pool stopped: %v", err)
	}
	reconcileACPWorkspaceAdapter(t, c, workspace)
	updated := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, updated); err != nil {
		t.Fatalf("read workspace: %v", err)
	}
	if updated.Status.State != workspacev1alpha1.ExecutionWorkspaceStateFailed {
		t.Fatalf("state = %s, want Failed when the pool settled without a checkpoint", updated.Status.State)
	}
}

func TestACPExecutionWorkspaceAdapterFailsSuspensionWithoutSuspendCapablePool(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	provider := acpAdapterProvider()
	workspace := acpAdapterWorkspace(t, "acp-ws-pool")
	workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
	pool := acpAdapterLinkedPool(workspace.Namespace, workspace.Name)
	c := acpAdapterTestClient(t, provider, workspace, pool)
	reconcileACPWorkspaceAdapter(t, c, workspace)
	updated := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, updated); err != nil {
		t.Fatalf("read workspace: %v", err)
	}
	if updated.Status.State != workspacev1alpha1.ExecutionWorkspaceStateFailed {
		t.Fatalf("state = %s, want Failed for a non-suspend-capable pool", updated.Status.State)
	}
}

// A same-name pool of a different workspace incarnation must never receive
// suspension intent, replica changes, or resume: linkedRuntimePool requires
// the controller-stamped incarnation pin.
func TestACPExecutionWorkspaceAdapterIgnoresUIDMismatchedLinkedPool(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	provider := acpAdapterProvider()
	workspace := acpAdapterWorkspace(t, "acp-ws-pool")
	workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
	pool := acpAdapterLinkedPool(workspace.Namespace, workspace.Name)
	pool.Annotations[acpExecutionWorkspaceUIDAnnotation] = "different-incarnation-uid"
	pool.Spec.ExecutionWorkspace.Provider = corev1alpha1.WorkspaceProviderSubstrate
	pool.Spec.ExecutionWorkspace.Substrate = &corev1alpha1.RuntimePoolSubstrateWorkspaceSpec{
		BaseTemplateNamespace: acpTestSubstrateNamespace, BaseTemplateName: acpTestInfraName,
		SuspendMode: string(acpworkspacev1alpha1.SubstrateSuspendModeDataOnly),
	}
	pool.Spec.DesiredReplicas = 1
	c := acpAdapterTestClient(t, provider, workspace, pool)

	reconcileACPWorkspaceAdapter(t, c, workspace)
	current := &corev1alpha1.RuntimePool{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: pool.Namespace, Name: pool.Name}, current); err != nil {
		t.Fatalf("read pool: %v", err)
	}
	if current.Annotations[substrateWorkspaceSuspendAnnotation] != "" || current.Spec.DesiredReplicas != 1 {
		t.Fatalf("foreign-incarnation pool was mutated: annotations=%v replicas=%d", current.Annotations, current.Spec.DesiredReplicas)
	}
}
