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
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
)

const suspendTestRuntimePoolName = "acp-ws-session-0123456789abcdef"

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
	plan := ACPRuntimePlan{PoolName: suspendTestRuntimePoolName, Workspace: binding}
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
	plan := ACPRuntimePlan{PoolName: suspendTestRuntimePoolName, Workspace: binding}
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
	plan := ACPRuntimePlan{PoolName: suspendTestRuntimePoolName, Workspace: binding}
	if _, _, err := r.ensureACPClassWorkspace(ctx, task, plan); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	name := acpClassWorkspaceName(task, binding)
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: name}, workspace); err != nil {
		t.Fatalf("read workspace: %v", err)
	}
	admitTestACPWorkspace(t, r, workspace)
	if _, ready := attachTestACPWorkspace(t, r, task, plan, workspace.Name); !ready {
		t.Fatalf("attach = (%v)", ready)
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: name}, workspace); err != nil {
		t.Fatalf("read attached workspace: %v", err)
	}
	if workspace.Annotations[acpWorkspaceDetachActionAnnotation] != string(workspacev1alpha1.WorkspaceOnDetachSuspend) {
		t.Fatalf("detach action annotation = %q", workspace.Annotations[acpWorkspaceDetachActionAnnotation])
	}
	base := workspace.DeepCopy()
	workspace.Annotations[acpWorkspaceDurableSessionCommittedAnnotation] = "1"
	if err := r.Patch(ctx, workspace, client.MergeFrom(base)); err != nil {
		t.Fatalf("record durable session commit: %v", err)
	}
	pool := &corev1alpha1.RuntimePool{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: workspace.Namespace,
			Name:      plan.PoolName,
			Labels:    map[string]string{acpExecutionWorkspaceLinkLabel: workspace.Name},
			Annotations: map[string]string{
				acpExecutionWorkspaceUIDAnnotation: string(workspace.UID),
			},
		},
		Spec: corev1alpha1.RuntimePoolSpec{
			ExecutionWorkspace: &corev1alpha1.RuntimePoolExecutionWorkspaceSpec{
				Provider: corev1alpha1.WorkspaceProviderSubstrate,
				Substrate: &corev1alpha1.RuntimePoolSubstrateWorkspaceSpec{
					SuspendMode: string(acpworkspacev1alpha1.SubstrateSuspendModeDataOnly),
				},
			},
		},
	}
	if err := r.Create(ctx, pool); err != nil {
		t.Fatalf("create linked RuntimePool: %v", err)
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, task); err != nil {
		t.Fatalf("reload task: %v", err)
	}

	// Settlement is a multi-reconcile flow: revocation start intentionally
	// returns not-done so the next reconcile re-reads uncached state, and
	// finalization waits for the adapter to release the enforced epoch.
	done := false
	for attempt := 0; attempt < 8 && !done; attempt++ {
		var err error
		if done, err = r.settleACPClassWorkspace(ctx, task); err != nil {
			t.Fatalf("settle attempt %d: %v", attempt, err)
		}
		released := &workspacev1alpha1.ExecutionWorkspace{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: name}, released); err == nil &&
			released.Spec.Attachment == nil && released.Status.AttachedEpoch != 0 {
			// Simulate the adapter observing the revocation and releasing
			// the enforced epoch.
			base := released.DeepCopy()
			released.Status.AttachedEpoch = 0
			if err := r.Status().Patch(ctx, released, client.MergeFrom(base)); err != nil {
				t.Fatalf("release enforced epoch: %v", err)
			}
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

// A Task can terminate after creating and linking its session workspace but
// before admission allows RuntimePool creation. Suspend settlement must delete
// that empty incarnation so the next Task can recreate it.
func TestSettleACPClassWorkspaceDeletesEmptyWorkspaceBeforePoolCreation(t *testing.T) {
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
	plan := ACPRuntimePlan{PoolName: suspendTestRuntimePoolName, Workspace: binding}
	name, ready, err := r.ensureACPClassWorkspace(ctx, task, plan)
	if err != nil || ready || name != "" {
		t.Fatalf("initial workspace materialization = (%q, %v, %v), want an admission wait", name, ready, err)
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, task); err != nil {
		t.Fatalf("reload linked task: %v", err)
	}
	workspaceName := acpClassWorkspaceName(task, binding)
	if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: workspaceName},
		&workspacev1alpha1.ExecutionWorkspace{}); err != nil {
		t.Fatalf("read pre-pool workspace: %v", err)
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: plan.PoolName},
		&corev1alpha1.RuntimePool{}); !apierrors.IsNotFound(err) {
		t.Fatalf("RuntimePool exists before admission: %v", err)
	}

	done, err := r.settleACPClassWorkspace(ctx, task)
	if err != nil || !done {
		t.Fatalf("settle pre-pool workspace = (%v, %v), want completed deletion", done, err)
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: workspaceName},
		&workspacev1alpha1.ExecutionWorkspace{}); !apierrors.IsNotFound(err) {
		t.Fatalf("empty pre-pool workspace survived settlement: %v", err)
	}
}

// RuntimePool creation is not proof that a durable session was committed. A
// Task can terminate after the pool object appears but before the supervisor
// creates an actor or commits a checkpoint; settlement must still delete that
// empty incarnation.
func TestSettleACPClassWorkspaceDeletesUncommittedWorkspaceWithExistingPool(t *testing.T) {
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
	plan := ACPRuntimePlan{PoolName: suspendTestRuntimePoolName, Workspace: binding}
	name, ready, err := r.ensureACPClassWorkspace(ctx, task, plan)
	if err != nil || ready || name != "" {
		t.Fatalf("initial workspace materialization = (%q, %v, %v), want an admission wait", name, ready, err)
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, task); err != nil {
		t.Fatalf("reload linked task: %v", err)
	}
	workspaceName := acpClassWorkspaceName(task, binding)
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: workspaceName}, workspace); err != nil {
		t.Fatalf("read uncommitted workspace: %v", err)
	}
	if got := strings.TrimSpace(workspace.Annotations[acpWorkspaceDurableSessionCommittedAnnotation]); got != "" {
		t.Fatalf("durable commit stamp = %q, want empty", got)
	}
	pool := &corev1alpha1.RuntimePool{ObjectMeta: metav1.ObjectMeta{
		Namespace: workspace.Namespace,
		Name:      plan.PoolName,
		Labels:    map[string]string{acpExecutionWorkspaceLinkLabel: workspace.Name},
		Annotations: map[string]string{
			acpExecutionWorkspaceUIDAnnotation: string(workspace.UID),
		},
	}}
	if err := r.Create(ctx, pool); err != nil {
		t.Fatalf("create linked actorless RuntimePool: %v", err)
	}

	done, err := r.settleACPClassWorkspace(ctx, task)
	if err != nil || !done {
		t.Fatalf("settle uncommitted workspace = (%v, %v), want completed deletion", done, err)
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: workspaceName},
		&workspacev1alpha1.ExecutionWorkspace{}); !apierrors.IsNotFound(err) {
		t.Fatalf("uncommitted workspace with an actorless pool survived settlement: %v", err)
	}
}

// A terminally Failed workspace (for example after maxLifetime expiry tore
// its pool down) must settle destructively even under a frozen Suspend
// action: no checkpoint remains, and a Suspended/Failed incarnation would
// wedge every later Session Task instead of recreating a clean workspace.
func TestSettleACPClassWorkspaceDeletesTerminallyFailedInsteadOfSuspending(t *testing.T) {
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
	plan := ACPRuntimePlan{PoolName: suspendTestRuntimePoolName, Workspace: binding}
	if _, _, err := r.ensureACPClassWorkspace(ctx, task, plan); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	name := acpClassWorkspaceName(task, binding)
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: name}, workspace); err != nil {
		t.Fatalf("read workspace: %v", err)
	}
	admitTestACPWorkspace(t, r, workspace)
	if _, ready := attachTestACPWorkspace(t, r, task, plan, workspace.Name); !ready {
		t.Fatalf("attach = (%v)", ready)
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: name}, workspace); err != nil {
		t.Fatalf("read attached workspace: %v", err)
	}
	// The adapter marked the incarnation terminally Failed and released the
	// enforced epoch (maxLifetime expiry destroyed the pool).
	base := workspace.DeepCopy()
	workspace.Status.State = workspacev1alpha1.ExecutionWorkspaceStateFailed
	workspace.Status.AttachedEpoch = 0
	if err := r.Status().Patch(ctx, workspace, client.MergeFrom(base)); err != nil {
		t.Fatalf("mark workspace failed: %v", err)
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, task); err != nil {
		t.Fatalf("reload task: %v", err)
	}

	done := false
	for attempt := 0; attempt < 8 && !done; attempt++ {
		if done, err = r.settleACPClassWorkspace(ctx, task); err != nil {
			t.Fatalf("settle attempt %d: %v", attempt, err)
		}
	}
	if !done {
		t.Fatal("settle never completed")
	}
	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: name}, current); err == nil {
		if current.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredSuspended {
			t.Fatal("a terminally Failed workspace must never be suspended; no checkpoint exists to preserve")
		}
		if current.DeletionTimestamp.IsZero() {
			t.Fatalf("terminal failure must settle destructively, workspace still standing: desired=%s", current.Spec.DesiredState)
		}
	}
}

// A settled suspension must self-schedule its frozen maxLifetime deadline:
// the stopped pool produces no further events, so without the requeue the
// retained checkpoint could outlive the bound until an unrelated mutation.
func TestACPExecutionWorkspaceAdapterRequeuesSettledSuspensionForLifetime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	provider := acpAdapterProvider()
	workspace := acpAdapterWorkspace(t, "acp-ws-pool")
	workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
	workspace.CreationTimestamp = metav1.Now()
	workspace.Spec.Lifecycle.MaxLifetime = &metav1.Duration{Duration: 24 * time.Hour}
	pool := acpAdapterLinkedPool(workspace.Namespace, workspace.Name)
	pool.Spec.ExecutionWorkspace.Provider = corev1alpha1.WorkspaceProviderSubstrate
	pool.Spec.ExecutionWorkspace.Substrate = &corev1alpha1.RuntimePoolSubstrateWorkspaceSpec{
		BaseTemplateNamespace: acpTestSubstrateNamespace, BaseTemplateName: acpTestInfraName,
		SuspendMode: string(acpworkspacev1alpha1.SubstrateSuspendModeDataOnly),
	}
	pool.Spec.DesiredReplicas = 0
	pool.Annotations[substrateWorkspaceSuspendAnnotation] = booleanTrueValue
	pool.Annotations[substrateActorSuspendedAnnotation] = runtimePoolSubstrateActorSuffix
	c := acpAdapterTestClient(t, provider, workspace, pool)
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

	reconciler := &ACPExecutionWorkspaceAdapterReconciler{Client: c, APIReader: c}
	result, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name},
	})
	if err != nil {
		t.Fatalf("settle suspension: %v", err)
	}
	updated := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, updated); err != nil {
		t.Fatalf("read workspace: %v", err)
	}
	if updated.Status.State != workspacev1alpha1.ExecutionWorkspaceStateSuspended {
		t.Fatalf("state = %s, want Suspended", updated.Status.State)
	}
	if result.RequeueAfter <= 0 || result.RequeueAfter > 24*time.Hour {
		t.Fatalf("settled suspension requeue = %v, want a bounded maxLifetime wake-up", result.RequeueAfter)
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
	current.Annotations[substrateActorSuspendedAnnotation] = runtimePoolSubstrateActorSuffix
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

func TestACPExecutionWorkspaceAdapterFailsPermanentCheckpointRejection(t *testing.T) {
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
	pool.Spec.DesiredReplicas = 0
	pool.Annotations[substrateWorkspaceSuspendAnnotation] = booleanTrueValue
	pool.Annotations[substrateWorkspaceSuspendFailedAnnotation] = runtimePoolSubstrateActorSuffix
	c := acpAdapterTestClient(t, provider, workspace, pool)

	reconcileACPWorkspaceAdapter(t, c, workspace)
	updated := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, updated); err != nil {
		t.Fatalf("read workspace: %v", err)
	}
	if updated.Status.State != workspacev1alpha1.ExecutionWorkspaceStateFailed {
		t.Fatalf("state = %s, want Failed after a permanent checkpoint rejection", updated.Status.State)
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

// A resumed-lineage workspace whose linked pool disappears AFTER the Ready
// projection must fail closed: the pool held the only copy of the resumed
// session data, and recreating it would rematerialize an empty baseline.
func TestACPExecutionWorkspaceAdapterFailsResumedLineageOnPoolLoss(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	provider := acpAdapterProvider()
	workspace := acpAdapterWorkspace(t, "acp-ws-pool")
	workspace.Annotations[acpWorkspaceResumedLineageAnnotation] = booleanTrueValue
	// Ready already projected; no pool exists any more.
	workspace.Status.State = workspacev1alpha1.ExecutionWorkspaceStateReady
	c := acpAdapterTestClient(t, provider, workspace)

	reconcileACPWorkspaceAdapter(t, c, workspace)
	updated := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, updated); err != nil {
		t.Fatalf("read workspace: %v", err)
	}
	if updated.Status.State != workspacev1alpha1.ExecutionWorkspaceStateFailed {
		t.Fatalf("state = %s, want Failed after pool loss on a resumed lineage", updated.Status.State)
	}
}
