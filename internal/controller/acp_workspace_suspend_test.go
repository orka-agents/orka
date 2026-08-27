/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
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

// A resuming workspace must not publish Ready while the linked pool is still
// cold-booting: the checkpoint record may stand and the resumed Pod has not
// passed the authenticated Serving fence, so an early Ready would let a Task
// attach before the preserved data is actually being served.
func TestACPExecutionWorkspaceAdapterHoldsReadyUntilResumeSettles(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	provider := acpAdapterProvider()
	workspace := acpAdapterWorkspace(t, "acp-ws-pool")
	workspace.CreationTimestamp = metav1.Now()
	workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredReady
	workspace.Status.State = workspacev1alpha1.ExecutionWorkspaceStateSuspended
	pool := acpAdapterLinkedPool(workspace.Namespace, workspace.Name)
	pool.Spec.ExecutionWorkspace.Provider = corev1alpha1.WorkspaceProviderSubstrate
	pool.Spec.ExecutionWorkspace.Substrate = &corev1alpha1.RuntimePoolSubstrateWorkspaceSpec{
		BaseTemplateNamespace: acpTestSubstrateNamespace, BaseTemplateName: acpTestInfraName,
		SuspendMode: string(acpworkspacev1alpha1.SubstrateSuspendModeDataOnly),
	}
	pool.Spec.DesiredReplicas = 1
	c := acpAdapterTestClient(t, provider, workspace, pool)
	seed := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, seed); err != nil {
		t.Fatalf("read workspace: %v", err)
	}
	base := seed.DeepCopy()
	seed.Status.State = workspacev1alpha1.ExecutionWorkspaceStateSuspended
	if err := c.Status().Patch(ctx, seed, client.MergeFrom(base)); err != nil {
		t.Fatalf("seed suspended state: %v", err)
	}

	// The pool is still Stopped: the resume has not passed the Serving fence.
	currentPool := &corev1alpha1.RuntimePool{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: pool.Namespace, Name: pool.Name}, currentPool); err != nil {
		t.Fatalf("read pool: %v", err)
	}
	poolBase := currentPool.DeepCopy()
	currentPool.Status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStopped
	currentPool.Status.ObservedGeneration = currentPool.Generation
	if err := c.Status().Patch(ctx, currentPool, client.MergeFrom(poolBase)); err != nil {
		t.Fatalf("mark pool stopped: %v", err)
	}
	reconcileACPWorkspaceAdapter(t, c, workspace)
	held := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, held); err != nil {
		t.Fatalf("read held workspace: %v", err)
	}
	if held.Status.State != workspacev1alpha1.ExecutionWorkspaceStateSuspended {
		t.Fatalf("state = %s; the hold must preserve the pre-resume state so the gate re-arms every reconcile", held.Status.State)
	}
	// The hold re-arms: a second reconcile against the still-stopped pool
	// must not publish Ready either.
	reconcileACPWorkspaceAdapter(t, c, workspace)
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, held); err != nil {
		t.Fatalf("re-read held workspace: %v", err)
	}
	if held.Status.State != workspacev1alpha1.ExecutionWorkspaceStateSuspended {
		t.Fatalf("state = %s after a repeat reconcile; Ready must wait for the resumed pool's Serving fence", held.Status.State)
	}

	// The resumed pool passes the authenticated Serving fence with its
	// checkpoint consent retired: Ready may now publish.
	if err := c.Get(ctx, types.NamespacedName{Namespace: pool.Namespace, Name: pool.Name}, currentPool); err != nil {
		t.Fatalf("re-read pool: %v", err)
	}
	poolBase = currentPool.DeepCopy()
	currentPool.Status.Lifecycle = corev1alpha1.RuntimePoolLifecycleServing
	currentPool.Status.AdmissionState = corev1alpha1.RuntimePoolAdmissionAccepting
	currentPool.Status.ObservedGeneration = currentPool.Generation
	if err := c.Status().Patch(ctx, currentPool, client.MergeFrom(poolBase)); err != nil {
		t.Fatalf("mark pool serving: %v", err)
	}
	reconcileACPWorkspaceAdapter(t, c, workspace)
	settled := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, settled); err != nil {
		t.Fatalf("read settled workspace: %v", err)
	}
	if settled.Status.State != workspacev1alpha1.ExecutionWorkspaceStateReady {
		t.Fatalf("state = %s, want Ready once the resumed pool serves", settled.Status.State)
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
	pool.Annotations[runtimePoolWorkspaceSuspendAnnotation] = booleanTrueValue
	pool.Annotations[substrateActorSuspendedAnnotation] = "actor"
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
	if current.Annotations[runtimePoolWorkspaceSuspendAnnotation] == "" || current.Spec.DesiredReplicas != 0 {
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
	if current.Annotations[runtimePoolWorkspaceSuspendAnnotation] != "" || current.Spec.DesiredReplicas != 1 {
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

func suspendableSandboxFixture(t *testing.T) *acpClassFixture {
	t.Helper()
	return newACPClassFixture(t, acpworkspacev1alpha1.RuntimeProviderBackendAgentSandbox, func(f *acpClassFixture) {
		f.profile.Spec.AgentSandbox = &acpworkspacev1alpha1.AgentSandboxProfileSpec{
			Suspend: &acpworkspacev1alpha1.AgentSandboxSuspendPolicy{
				Mode:   acpworkspacev1alpha1.SubstrateSuspendModeDataOnly,
				Volume: acpworkspacev1alpha1.AgentSandboxDurableVolume{Capacity: acpTestDurableCapacity},
			},
		}
		f.class.Spec.Lifecycle.DefaultOnDetach = workspacev1alpha1.WorkspaceOnDetachSuspend
		f.class.Spec.Lifecycle.AllowedOnDetach = []workspacev1alpha1.WorkspaceOnDetach{
			workspacev1alpha1.WorkspaceOnDetachSuspend, workspacev1alpha1.WorkspaceOnDetachDelete,
		}
	})
}

func TestResolveACPClassWorkspaceBindingAdmitsSandboxPVCSuspend(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := suspendableSandboxFixture(t)
	r := acpClassTestReconciler(t, fixture.objects()...)
	resolved, err := r.resolveACPWorkspaceClass(ctx, acpClassTestTask())
	if err != nil {
		t.Fatalf("resolve class: %v", err)
	}
	if resolved.Binding.SuspendMode != string(acpworkspacev1alpha1.SubstrateSuspendModeDataOnly) ||
		resolved.Binding.SandboxVolume == nil {
		t.Fatalf("resolved binding = %+v", resolved.Binding)
	}
	if got := resolved.Binding.SandboxVolume.AccessModes; len(got) != 1 || got[0] != "ReadWriteOnce" {
		t.Fatalf("access modes = %v, want the ReadWriteOnce default", got)
	}
	binding, err := resolveACPWorkspaceBindingWithClass(suspendableSessionTask(), "", false, "session-uid-1", resolved)
	if err != nil {
		t.Fatalf("resolve suspendable binding: %v", err)
	}
	if binding.Class.EffectiveOnDetach != string(workspacev1alpha1.WorkspaceOnDetachSuspend) {
		t.Fatalf("class binding = %+v", binding.Class)
	}
	if err := validateACPWorkspaceBindingValues(binding); err != nil {
		t.Fatalf("frozen binding validation: %v", err)
	}
	frozen := snapshotWorkspaceClassFromBinding(binding.Class)
	rebuilt := workspaceClassBindingFromSnapshot(frozen)
	if rebuilt.SandboxVolume == nil || rebuilt.SandboxVolume.Capacity != acpTestDurableCapacity {
		t.Fatalf("snapshot round trip dropped the durable volume: %+v", rebuilt.SandboxVolume)
	}

	tampered := *binding
	tamperedClass := *binding.Class
	tamperedClass.SandboxVolume = nil
	tampered.Class = &tamperedClass
	if err := validateACPWorkspaceBindingValues(&tampered); err == nil {
		t.Fatal("a sandbox Suspend binding without its frozen durable volume must fail closed")
	}
}

func TestResolveACPClassWorkspaceContinuationReusesFrozenSandboxVolume(t *testing.T) {
	ctx := context.Background()
	fixture := suspendableSandboxFixture(t)
	holder := suspendableSessionTask()
	objects := fixture.objects()
	for _, object := range objects {
		if class, ok := object.(*storagev1.StorageClass); ok {
			class.UID = types.UID("original-storage-class-uid")
		}
	}
	r := acpClassTestReconciler(t, append(objects, holder)...)
	const sessionUID = "session-uid-1"
	originalResolved, err := r.resolveACPWorkspaceClassWithSessionUID(ctx, holder, sessionUID)
	if err != nil {
		t.Fatalf("resolve original class: %v", err)
	}
	originalBinding, err := resolveACPWorkspaceBindingWithClass(holder, "", false, sessionUID, originalResolved)
	if err != nil {
		t.Fatalf("resolve original binding: %v", err)
	}
	plan := ACPRuntimePlan{PoolName: suspendTestRuntimePoolName, Workspace: originalBinding}
	if _, _, err := r.ensureACPClassWorkspace(ctx, holder, plan); err != nil {
		t.Fatalf("materialize original workspace: %v", err)
	}
	workspaceName := acpClassWorkspaceName(holder, originalBinding)
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: holder.Namespace, Name: workspaceName}, workspace); err != nil {
		t.Fatalf("read original workspace: %v", err)
	}
	if workspace.UID == "" {
		workspace.UID = types.UID("workspace-uid")
		if err := r.Update(ctx, workspace); err != nil {
			t.Fatalf("assign workspace UID: %v", err)
		}
	}
	pool := &corev1alpha1.RuntimePool{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: holder.Namespace,
			Name:      plan.PoolName,
			Labels: map[string]string{
				acpExecutionWorkspaceLinkLabel: workspace.Name,
			},
			Annotations: map[string]string{
				acpExecutionWorkspaceUIDAnnotation: string(workspace.UID),
			},
		},
		Spec: corev1alpha1.RuntimePoolSpec{ExecutionWorkspace: &corev1alpha1.RuntimePoolExecutionWorkspaceSpec{
			Provider:      corev1alpha1.WorkspaceProviderAgentSandbox,
			BindingDigest: originalBinding.BindingDigest,
			AgentSandbox: &corev1alpha1.RuntimePoolAgentSandboxWorkspaceSpec{
				SuspendMode: originalBinding.Class.SuspendMode,
				SuspendVolume: &corev1alpha1.RuntimePoolSandboxDurableVolumeSpec{
					StorageClassName: originalBinding.Class.SandboxVolume.StorageClassName,
					StorageClassUID:  originalBinding.Class.SandboxVolume.StorageClassUID,
					AccessModes:      append([]string(nil), originalBinding.Class.SandboxVolume.AccessModes...),
					Capacity:         originalBinding.Class.SandboxVolume.Capacity,
				},
			},
		}},
	}
	if err := r.Create(ctx, pool); err != nil {
		t.Fatalf("create linked RuntimePool: %v", err)
	}

	originalClass := &storagev1.StorageClass{}
	if err := r.Get(ctx, types.NamespacedName{Name: originalBinding.Class.SandboxVolume.StorageClassName}, originalClass); err != nil {
		t.Fatalf("read original StorageClass: %v", err)
	}
	if err := r.Delete(ctx, originalClass); err != nil {
		t.Fatalf("delete original StorageClass: %v", err)
	}
	replacement := acpTestDefaultStorageClass()
	replacement.UID = types.UID("replacement-storage-class-uid")
	if err := r.Create(ctx, replacement); err != nil {
		t.Fatalf("create replacement StorageClass: %v", err)
	}

	continuation := holder.DeepCopy()
	continuation.Name = "continuation"
	continuation.UID = types.UID("continuation-task-uid")
	continuationResolved, err := r.resolveACPWorkspaceClassWithSessionUID(ctx, continuation, sessionUID)
	if err != nil {
		t.Fatalf("resolve continuation class: %v", err)
	}
	if got := continuationResolved.Binding.SandboxVolume.StorageClassUID; got != "original-storage-class-uid" {
		t.Fatalf("continuation StorageClass UID = %q, want original frozen UID", got)
	}
	continuationBinding, err := resolveACPWorkspaceBindingWithClass(continuation, "", false, sessionUID, continuationResolved)
	if err != nil {
		t.Fatalf("resolve continuation binding: %v", err)
	}
	if continuationBinding.BindingDigest != originalBinding.BindingDigest {
		t.Fatalf("continuation binding digest = %q, want original %q", continuationBinding.BindingDigest, originalBinding.BindingDigest)
	}

	fresh := continuation.DeepCopy()
	fresh.Name = "fresh-session"
	fresh.UID = types.UID("fresh-session-task-uid")
	freshResolved, err := r.resolveACPWorkspaceClassWithSessionUID(ctx, fresh, "fresh-session-uid")
	if err != nil {
		t.Fatalf("resolve fresh class: %v", err)
	}
	if got := freshResolved.Binding.SandboxVolume.StorageClassUID; got != "replacement-storage-class-uid" {
		t.Fatalf("fresh workspace StorageClass UID = %q, want live replacement UID", got)
	}

	assertACPClassWorkspaceCandidateReusesFrozenSandboxVolume(
		t, ctx, r, holder, continuation, replacement, sessionUID,
	)

	currentPool := &corev1alpha1.RuntimePool{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(pool), currentPool); err != nil {
		t.Fatalf("read linked RuntimePool: %v", err)
	}
	currentPool.Spec.ExecutionWorkspace.BindingDigest = "sha256:" + strings.Repeat("f", 64)
	if err := r.Update(ctx, currentPool); err != nil {
		t.Fatalf("corrupt linked RuntimePool binding digest: %v", err)
	}
	if _, err := r.resolveACPWorkspaceClassWithSessionUID(ctx, continuation, sessionUID); err == nil ||
		!strings.Contains(err.Error(), "binding digest is inconsistent") {
		t.Fatalf("digest mismatch error = %v, want fail-closed rejection", err)
	}
}

func assertACPClassWorkspaceCandidateReusesFrozenSandboxVolume(
	t *testing.T,
	ctx context.Context,
	r *TaskReconciler,
	holder *corev1alpha1.Task,
	continuation *corev1alpha1.Task,
	replacement *storagev1.StorageClass,
	sessionUID string,
) {
	t.Helper()
	bindingReconciler, durableStore := newBindingTestReconciler(t)
	r.AgentExecutionSnapshots = bindingReconciler.AgentExecutionSnapshots
	r.DurableControlStore = durableStore
	r.SessionManager = NewSessionManager(durableStore)
	r.ACPRuntimeEnabled = bindingReconciler.ACPRuntimeEnabled
	r.ACPRuntimeNamespace = bindingReconciler.ACPRuntimeNamespace
	r.ACPRuntimeImages = bindingReconciler.ACPRuntimeImages
	epochs := NewControllerEpochManager(durableStore, "workspace-class-continuation-test")
	r.ControllerEpochManager = epochs
	epochCtx, cancelEpoch := context.WithCancel(context.Background())
	epochDone := make(chan error, 1)
	go func() { epochDone <- epochs.Start(epochCtx) }()
	defer func() {
		cancelEpoch()
		if err := <-epochDone; err != nil {
			t.Errorf("stop epoch manager: %v", err)
		}
	}()
	readyCtx, cancelReady := context.WithTimeout(ctx, 5*time.Second)
	defer cancelReady()
	if _, err := epochs.CurrentFence(readyCtx); err != nil {
		t.Fatalf("start epoch manager: %v", err)
	}
	if established, err := r.ensureACPWorkspaceSessionUID(ctx, holder, sessionUID); err != nil {
		t.Fatalf("establish original Session identity: %v", err)
	} else if established != sessionUID {
		t.Fatalf("established Session UID = %q, want %q", established, sessionUID)
	}
	if err := r.Create(ctx, bindingTestNamespace()); err != nil {
		t.Fatalf("create task namespace: %v", err)
	}
	if err := r.Delete(ctx, replacement); err != nil {
		t.Fatalf("delete replacement StorageClass: %v", err)
	}
	candidate, err := r.resolveAgentExecutionCandidate(ctx, continuation, bindingTestAgent())
	if err != nil {
		t.Fatalf("resolve continuation candidate without a live StorageClass: %v", err)
	}
	var candidateBody agentExecutionSnapshotBody
	if err := json.Unmarshal(candidate.snapshotBody, &candidateBody); err != nil {
		t.Fatalf("decode continuation candidate snapshot: %v", err)
	}
	if candidate.workspaceSessionUID != sessionUID || candidateBody.ExecutionWorkspace == nil ||
		candidateBody.ExecutionWorkspace.Class == nil || candidateBody.ExecutionWorkspace.Class.SandboxVolume == nil ||
		candidateBody.ExecutionWorkspace.Class.SandboxVolume.StorageClassUID != "original-storage-class-uid" {
		t.Fatalf("continuation candidate = uid %q workspace %#v, want frozen original StorageClass identity",
			candidate.workspaceSessionUID, candidateBody.ExecutionWorkspace)
	}
}

func TestResolveACPClassWorkspaceSandboxSuspendRejections(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("invalid durable capacity", func(t *testing.T) {
		t.Parallel()
		fixture := newACPClassFixture(t, acpworkspacev1alpha1.RuntimeProviderBackendAgentSandbox, func(f *acpClassFixture) {
			f.profile.Spec.AgentSandbox = &acpworkspacev1alpha1.AgentSandboxProfileSpec{
				Suspend: &acpworkspacev1alpha1.AgentSandboxSuspendPolicy{
					Mode:   acpworkspacev1alpha1.SubstrateSuspendModeDataOnly,
					Volume: acpworkspacev1alpha1.AgentSandboxDurableVolume{Capacity: "not-a-quantity"},
				},
			}
		})
		r := acpClassTestReconciler(t, fixture.objects()...)
		if _, err := r.resolveACPWorkspaceClass(ctx, acpClassTestTask()); err == nil ||
			!strings.Contains(err.Error(), "capacity") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("sandbox inputs on a substrate class", func(t *testing.T) {
		t.Parallel()
		fixture := newACPClassFixture(t, acpworkspacev1alpha1.RuntimeProviderBackendSubstrate, func(f *acpClassFixture) {
			f.profile.Spec.AgentSandbox = &acpworkspacev1alpha1.AgentSandboxProfileSpec{}
		})
		r := acpClassTestReconciler(t, fixture.objects()...)
		if _, err := r.resolveACPWorkspaceClass(ctx, acpClassTestTask()); err == nil ||
			!strings.Contains(err.Error(), "agent-sandbox inputs") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("read-only access mode fails closed", func(t *testing.T) {
		t.Parallel()
		fixture := newACPClassFixture(t, acpworkspacev1alpha1.RuntimeProviderBackendAgentSandbox, func(f *acpClassFixture) {
			f.profile.Spec.AgentSandbox = &acpworkspacev1alpha1.AgentSandboxProfileSpec{
				Suspend: &acpworkspacev1alpha1.AgentSandboxSuspendPolicy{
					Mode: acpworkspacev1alpha1.SubstrateSuspendModeDataOnly,
					Volume: acpworkspacev1alpha1.AgentSandboxDurableVolume{
						Capacity: acpTestDurableCapacity, AccessModes: []string{"ReadOnlyMany"},
					},
				},
			}
		})
		r := acpClassTestReconciler(t, fixture.objects()...)
		if _, err := r.resolveACPWorkspaceClass(ctx, acpClassTestTask()); err == nil ||
			!strings.Contains(err.Error(), "not a writable mode") {
			t.Fatalf("error = %v, want the read-only durable mode rejected", err)
		}
	})

	t.Run("invalid access mode", func(t *testing.T) {
		t.Parallel()
		fixture := newACPClassFixture(t, acpworkspacev1alpha1.RuntimeProviderBackendAgentSandbox, func(f *acpClassFixture) {
			f.profile.Spec.AgentSandbox = &acpworkspacev1alpha1.AgentSandboxProfileSpec{
				Suspend: &acpworkspacev1alpha1.AgentSandboxSuspendPolicy{
					Mode: acpworkspacev1alpha1.SubstrateSuspendModeDataOnly,
					Volume: acpworkspacev1alpha1.AgentSandboxDurableVolume{
						Capacity: acpTestDurableCapacity, AccessModes: []string{"ReadMaybe"},
					},
				},
			}
		})
		r := acpClassTestReconciler(t, fixture.objects()...)
		if _, err := r.resolveACPWorkspaceClass(ctx, acpClassTestTask()); err == nil ||
			!strings.Contains(err.Error(), "access mode") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestACPExecutionWorkspaceAdapterDrivesSandboxSuspension(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	provider := acpAdapterProvider()
	workspace := acpAdapterWorkspace(t, "acp-ws-pool")
	workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
	pool := acpAdapterLinkedPool(workspace.Namespace, workspace.Name)
	pool.Spec.ExecutionWorkspace.AgentSandbox = &corev1alpha1.RuntimePoolAgentSandboxWorkspaceSpec{
		SuspendMode: string(acpworkspacev1alpha1.SubstrateSuspendModeDataOnly),
		SuspendVolume: &corev1alpha1.RuntimePoolSandboxDurableVolumeSpec{
			AccessModes: []string{"ReadWriteOnce"}, Capacity: acpTestDurableCapacity,
		},
	}
	pool.Spec.DesiredReplicas = 1
	c := acpAdapterTestClient(t, provider, workspace, pool)

	reconcileACPWorkspaceAdapter(t, c, workspace)
	current := &corev1alpha1.RuntimePool{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: pool.Namespace, Name: pool.Name}, current); err != nil {
		t.Fatalf("read pool: %v", err)
	}
	if current.Annotations[runtimePoolWorkspaceSuspendAnnotation] == "" || current.Spec.DesiredReplicas != 0 {
		t.Fatalf("pool intent = %+v replicas=%d, want suspension intent at zero replicas", current.Annotations, current.Spec.DesiredReplicas)
	}

	base := current.DeepCopy()
	current.Annotations[sandboxSuspendedAnnotation] = `{"name":"sandbox","uid":"sandbox-uid","pvcUID":"sandbox-pvc-uid","pvName":"pv-sandbox","pvUID":"pv-sandbox-uid"}`
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
	if current.Annotations[runtimePoolWorkspaceSuspendAnnotation] != "" || current.Spec.DesiredReplicas != 1 {
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

// A resumed lineage may recover from a transient pool outage, but it must not
// remain Ready or Attached while the exact data-bearing pool is unavailable.
// Once the pool records definitive lineage loss, the workspace becomes Failed.
func TestACPExecutionWorkspaceAdapterWithdrawsUnavailableResumedLineage(t *testing.T) {
	t.Parallel()
	for _, attached := range []bool{false, true} {
		name := "unattached"
		if attached {
			name = "attached"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			provider := acpAdapterProvider()
			workspace := acpAdapterWorkspace(t, "acp-ws-pool")
			workspace.Annotations[acpWorkspaceResumedLineageAnnotation] = booleanTrueValue
			if attached {
				workspace.Spec.AttachmentEpoch = 7
				workspace.Spec.Attachment = &workspacev1alpha1.ExecutionWorkspaceAttachment{
					TaskRef:        workspacev1alpha1.ObjectIdentityReference{Name: "attached-task", UID: types.UID("attached-task-uid")},
					Epoch:          7,
					TokenSHA256:    "sha256:" + strings.Repeat("c", 64),
					TokenSecretRef: workspacev1alpha1.SecretReference{Name: "attach-secret"},
					ExpiresAt:      metav1.NewTime(time.Now().Add(time.Hour)),
				}
			}
			pool := acpAdapterLinkedPool(workspace.Namespace, workspace.Name)
			c := acpAdapterTestClient(t, provider, workspace, pool)

			seed := &workspacev1alpha1.ExecutionWorkspace{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), seed); err != nil {
				t.Fatalf("read workspace: %v", err)
			}
			baseStatus := seed.DeepCopy()
			seed.Status.State = workspacev1alpha1.ExecutionWorkspaceStateReady
			if attached {
				seed.Status.State = workspacev1alpha1.ExecutionWorkspaceStateAttached
				seed.Status.AttachedEpoch = 7
				apimeta.SetStatusCondition(&seed.Status.Conditions, metav1.Condition{
					Type: string(workspacev1alpha1.ConditionWorkspaceAttached), Status: metav1.ConditionTrue,
					Reason:             string(workspacev1alpha1.ReasonReady),
					ObservedGeneration: seed.Generation,
				})
			}
			if err := c.Status().Patch(ctx, seed, client.MergeFrom(baseStatus)); err != nil {
				t.Fatalf("seed projected workspace state: %v", err)
			}

			currentPool := &corev1alpha1.RuntimePool{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(pool), currentPool); err != nil {
				t.Fatalf("read pool: %v", err)
			}
			poolStatusBase := currentPool.DeepCopy()
			currentPool.Status.Lifecycle = corev1alpha1.RuntimePoolLifecycleDegraded
			currentPool.Status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
			currentPool.Status.ObservedGeneration = currentPool.Generation
			if err := c.Status().Patch(ctx, currentPool, client.MergeFrom(poolStatusBase)); err != nil {
				t.Fatalf("mark resumed pool unavailable: %v", err)
			}

			reconcileACPWorkspaceAdapter(t, c, workspace)
			held := &workspacev1alpha1.ExecutionWorkspace{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), held); err != nil {
				t.Fatalf("read held workspace: %v", err)
			}
			if held.Status.State != workspacev1alpha1.ExecutionWorkspaceStateProvisioning || held.Status.AttachedEpoch != 0 {
				t.Fatalf("held state = %s epoch=%d, want Provisioning with no enforced attachment",
					held.Status.State, held.Status.AttachedEpoch)
			}
			attachedCondition := apimeta.FindStatusCondition(held.Status.Conditions, string(workspacev1alpha1.ConditionWorkspaceAttached))
			if attachedCondition == nil || attachedCondition.Status != metav1.ConditionFalse {
				t.Fatalf("attached condition = %+v, want False while the resumed pool is unavailable", attachedCondition)
			}
			if !attached && WorkspaceReusable(held) {
				t.Fatal("an unavailable resumed lineage must not remain reusable")
			}

			if err := c.Get(ctx, client.ObjectKeyFromObject(pool), currentPool); err != nil {
				t.Fatalf("re-read pool: %v", err)
			}
			poolBase := currentPool.DeepCopy()
			if currentPool.Annotations == nil {
				currentPool.Annotations = map[string]string{}
			}
			currentPool.Annotations[runtimePoolWorkspaceResumeLostAnnotation] = "recorded resumed lineage is unrecoverable"
			if err := c.Patch(ctx, currentPool, client.MergeFrom(poolBase)); err != nil {
				t.Fatalf("record resumed-lineage loss: %v", err)
			}

			reconcileACPWorkspaceAdapter(t, c, workspace)
			failed := &workspacev1alpha1.ExecutionWorkspace{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), failed); err != nil {
				t.Fatalf("read failed workspace: %v", err)
			}
			if failed.Status.State != workspacev1alpha1.ExecutionWorkspaceStateFailed {
				t.Fatalf("state = %s, want Failed after definitive resumed-lineage loss", failed.Status.State)
			}
		})
	}
}
