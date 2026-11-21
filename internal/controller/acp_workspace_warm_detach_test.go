package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/operatoraccess"
	storekube "github.com/orka-agents/orka/internal/store/kube"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const warmTestWindow = "10m"

func warmTestProfile(retention map[string]any) *unstructured.Unstructured {
	spec := map[string]any{"suspend": map[string]any{"mode": "DataOnly"}}
	if retention != nil {
		spec["retention"] = retention
	}
	return &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
}

func warmTestClass(mutate ...func(*workspacev1alpha1.ExecutionWorkspaceClass)) *workspacev1alpha1.ExecutionWorkspaceClass {
	class := &workspacev1alpha1.ExecutionWorkspaceClass{Spec: workspacev1alpha1.ExecutionWorkspaceClassSpec{
		AllowedReuseScopes: []workspacev1alpha1.WorkspaceReuseScope{workspacev1alpha1.WorkspaceReuseScopeSession},
		Lifecycle: workspacev1alpha1.ExecutionWorkspaceLifecycle{
			DefaultOnDetach: workspacev1alpha1.WorkspaceOnDetachSuspend,
			AllowedOnDetach: []workspacev1alpha1.WorkspaceOnDetach{workspacev1alpha1.WorkspaceOnDetachSuspend, workspacev1alpha1.WorkspaceOnDetachDelete},
			IdleTimeout:     &metav1.Duration{Duration: 30 * time.Minute},
			MaxLifetime:     &metav1.Duration{Duration: 8 * time.Hour},
		},
	}}
	for _, m := range mutate {
		m(class)
	}
	return class
}

func TestExternalACPProfilePolicyValidatesSuspendAfterDetach(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		profile    *unstructured.Unstructured
		class      *workspacev1alpha1.ExecutionWorkspaceClass
		wantWindow time.Duration
		wantErr    string
	}{
		{name: "unset", profile: warmTestProfile(nil), class: warmTestClass()},
		{name: "unset with cap", profile: warmTestProfile(map[string]any{"maxSuspendedWorkspaces": int64(2)}), class: warmTestClass()},
		{name: "valid", profile: warmTestProfile(map[string]any{"suspendAfterDetach": warmTestWindow}), class: warmTestClass(), wantWindow: 10 * time.Minute},
		{name: "one hour", profile: warmTestProfile(map[string]any{"suspendAfterDetach": "1h"}), class: warmTestClass(func(c *workspacev1alpha1.ExecutionWorkspaceClass) {
			c.Spec.Lifecycle.IdleTimeout = &metav1.Duration{Duration: 2 * time.Hour}
		}), wantWindow: time.Hour},
		{name: "over one hour", profile: warmTestProfile(map[string]any{"suspendAfterDetach": "61m"}), class: warmTestClass(func(c *workspacev1alpha1.ExecutionWorkspaceClass) {
			c.Spec.Lifecycle.IdleTimeout = &metav1.Duration{Duration: 2 * time.Hour}
		}), wantErr: "at most"},
		{name: "zero", profile: warmTestProfile(map[string]any{"suspendAfterDetach": "0s"}), class: warmTestClass(), wantErr: "positive"},
		{name: "negative", profile: warmTestProfile(map[string]any{"suspendAfterDetach": "-1m"}), class: warmTestClass(), wantErr: "positive"},
		{name: "empty", profile: warmTestProfile(map[string]any{"suspendAfterDetach": ""}), class: warmTestClass(), wantErr: "not a duration"},
		{name: "not a duration", profile: warmTestProfile(map[string]any{"suspendAfterDetach": "ten minutes"}), class: warmTestClass(), wantErr: "not a duration"},
		{name: "not a string", profile: warmTestProfile(map[string]any{"suspendAfterDetach": int64(600)}), class: warmTestClass(), wantErr: "duration string"},
		{name: "suspend not allowed", profile: warmTestProfile(map[string]any{"suspendAfterDetach": warmTestWindow}), class: warmTestClass(func(c *workspacev1alpha1.ExecutionWorkspaceClass) {
			c.Spec.Lifecycle.DefaultOnDetach = workspacev1alpha1.WorkspaceOnDetachDelete
			c.Spec.Lifecycle.AllowedOnDetach = []workspacev1alpha1.WorkspaceOnDetach{workspacev1alpha1.WorkspaceOnDetachDelete}
		}), wantErr: "requires a class that allows Suspend"},
		{name: "not data only", profile: &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{
			"retention": map[string]any{"suspendAfterDetach": warmTestWindow},
		}}}, class: warmTestClass(func(c *workspacev1alpha1.ExecutionWorkspaceClass) {
			c.Spec.Lifecycle.DefaultOnDetach = workspacev1alpha1.WorkspaceOnDetachDelete
			c.Spec.Lifecycle.AllowedOnDetach = []workspacev1alpha1.WorkspaceOnDetach{workspacev1alpha1.WorkspaceOnDetachDelete}
		}), wantErr: "requires a class that allows Suspend"},
		{name: "not idle-shorter", profile: warmTestProfile(map[string]any{"suspendAfterDetach": "30m"}), class: warmTestClass(), wantErr: "shorter than idleTimeout"},
		{name: "not lifetime-shorter", profile: warmTestProfile(map[string]any{"suspendAfterDetach": "20m"}), class: warmTestClass(func(c *workspacev1alpha1.ExecutionWorkspaceClass) {
			c.Spec.Lifecycle.IdleTimeout = nil
			c.Spec.Lifecycle.MaxLifetime = &metav1.Duration{Duration: 20 * time.Minute}
		}), wantErr: "shorter than maxLifetime"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			policy, err := externalACPProfilePolicy(test.profile, test.class)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("policy: %v", err)
			}
			if policy.SuspendAfterDetach != test.wantWindow {
				t.Fatalf("window = %s, want %s", policy.SuspendAfterDetach, test.wantWindow)
			}
		})
	}
}

// resolveWarmTestBinding resolves the frozen workspace binding for one
// suspendable Substrate fixture whose profile sets window (empty = unset).
func resolveWarmTestBinding(t *testing.T, window string) (*TaskReconciler, *corev1alpha1.Task, *ACPRuntimeWorkspaceBinding) {
	t.Helper()
	fixture := suspendableSubstrateFixture(t)
	if window != "" {
		fixture.profile.Spec.Retention = &RetentionPolicy{SuspendAfterDetach: window}
		fixture.pinProfileHash(t)
	}
	task := suspendableSessionTask()
	r := acpClassTestReconciler(t, append(fixture.objects(), task)...)
	resolved, err := r.resolveACPWorkspaceClass(context.Background(), task)
	if err != nil {
		t.Fatalf("resolve class: %v", err)
	}
	binding, err := resolveACPWorkspaceBindingWithClass(task, suspendTestSessionUID, resolved)
	if err != nil {
		t.Fatalf("resolve binding: %v", err)
	}
	return r, task, binding
}

func TestACPWorkspaceBindingFreezesSuspendAfterDetachOnlyWhenSet(t *testing.T) {
	t.Parallel()
	_, _, unset := resolveWarmTestBinding(t, "")
	_, _, set := resolveWarmTestBinding(t, warmTestWindow)
	if unset.Class.SuspendAfterDetach != "" {
		t.Fatalf("unset window froze %q", unset.Class.SuspendAfterDetach)
	}
	if set.Class.SuspendAfterDetach != (10 * time.Minute).String() {
		t.Fatalf("frozen window = %q, want %q", set.Class.SuspendAfterDetach, (10 * time.Minute).String())
	}
	if err := validateACPWorkspaceClassBindingValues(set.Class); err != nil {
		t.Fatalf("frozen window binding invalid: %v", err)
	}

	// An unset window adds no digest field and no snapshot field.
	withoutField := *unset
	class := *unset.Class
	withoutField.Class = &class
	baseline, err := acpWorkspaceBindingDigest(&withoutField)
	if err != nil {
		t.Fatal(err)
	}
	if baseline != unset.BindingDigest {
		t.Fatalf("unset window changed the binding digest")
	}
	frozen := snapshotWorkspaceClassFromBinding(unset.Class)
	if frozen.SuspendAfterDetach != "" {
		t.Fatal("unset window entered the execution snapshot")
	}
	encoded, err := json.Marshal(frozen)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "suspendAfterDetach") {
		t.Fatalf("unset window is not omitted from the snapshot: %s", encoded)
	}

	// A set window changes the digest and round-trips through the snapshot.
	class.SuspendAfterDetach = set.Class.SuspendAfterDetach
	withField, err := acpWorkspaceBindingDigest(&withoutField)
	if err != nil {
		t.Fatal(err)
	}
	if withField == baseline {
		t.Fatal("set window did not change the binding digest")
	}
	roundTrip := workspaceClassBindingFromSnapshot(snapshotWorkspaceClassFromBinding(set.Class))
	if roundTrip.SuspendAfterDetach != set.Class.SuspendAfterDetach {
		t.Fatalf("snapshot round trip window = %q", roundTrip.SuspendAfterDetach)
	}

	invalid := *set.Class
	invalid.SuspendAfterDetach = "2h"
	if err := validateACPWorkspaceClassBindingValues(&invalid); err == nil {
		t.Fatal("frozen window over one hour was accepted")
	}
	invalid.SuspendAfterDetach = "8h"
	invalid.SuspendMode = ""
	if err := validateACPWorkspaceClassBindingValues(&invalid); err == nil {
		t.Fatal("frozen window without DataOnly suspension was accepted")
	}
}

func TestEnsureACPClassWorkspaceFreezesSuspendAfterDetach(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, task, binding := resolveWarmTestBinding(t, warmTestWindow)
	plan := ACPRuntimePlan{PoolName: acpTestSessionPoolName, Workspace: binding}
	if _, _, err := r.ensureACPClassWorkspace(ctx, task, plan); err != nil {
		t.Fatalf("materialize workspace: %v", err)
	}
	key := types.NamespacedName{Namespace: task.Namespace, Name: acpClassWorkspaceName(task, binding)}
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	if err := r.Get(ctx, key, workspace); err != nil {
		t.Fatal(err)
	}
	if got := workspace.Annotations[acpWorkspaceSuspendAfterDetachAnnotation]; got != binding.Class.SuspendAfterDetach {
		t.Fatalf("creation froze window %q, want %q", got, binding.Class.SuspendAfterDetach)
	}

	delete(workspace.Annotations, acpWorkspaceSuspendAfterDetachAnnotation)
	if err := r.Update(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.ensureACPClassWorkspace(ctx, task, plan); err != nil {
		t.Fatalf("adopt workspace without window: %v", err)
	}
	if err := r.Get(ctx, key, workspace); err != nil {
		t.Fatal(err)
	}
	if got := workspace.Annotations[acpWorkspaceSuspendAfterDetachAnnotation]; got != binding.Class.SuspendAfterDetach {
		t.Fatalf("backfilled window = %q", got)
	}

	workspace.Annotations[acpWorkspaceSuspendAfterDetachAnnotation] = "5m0s"
	if err := r.Update(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.ensureACPClassWorkspace(ctx, task, plan); err == nil ||
		!strings.Contains(err.Error(), "warm-detach window does not match") {
		t.Fatalf("mismatched window error = %v", err)
	}

	_, unsetTask, unset := resolveWarmTestBinding(t, "")
	annotations := workspaceCreationAnnotations(unset, acpTestSessionPoolName, unsetTask.Name, string(unsetTask.UID), acpTestRuntimeNamespace)
	if _, present := annotations[acpWorkspaceSuspendAfterDetachAnnotation]; present {
		t.Fatal("unset window was materialized")
	}
}

// warmSettlementFixture is one attached, committed session workspace whose
// Task executed a turn and is ready to settle.
type warmSettlementFixture struct {
	r       *TaskReconciler
	task    *corev1alpha1.Task
	plan    ACPRuntimePlan
	binding *ACPRuntimeWorkspaceBinding
	key     types.NamespacedName
}

func newWarmSettlementFixture(t *testing.T, window string) *warmSettlementFixture {
	t.Helper()
	ctx := context.Background()
	fixture := suspendableSubstrateFixture(t)
	if window != "" {
		fixture.profile.Spec.Retention = &RetentionPolicy{SuspendAfterDetach: window}
		fixture.pinProfileHash(t)
	}
	task := suspendableSessionTask()
	r := acpClassTestReconciler(t, append(fixture.objects(), task)...)
	task = bindSuspendableSessionTaskForSettlement(t, r, task)
	resolved, err := r.resolveACPWorkspaceClass(ctx, task)
	if err != nil {
		t.Fatalf("resolve class: %v", err)
	}
	binding, err := resolveACPWorkspaceBindingWithClass(task, suspendTestSessionUID, resolved)
	if err != nil {
		t.Fatalf("resolve binding: %v", err)
	}
	plan := ACPRuntimePlan{PoolName: acpTestSessionPoolName, Workspace: binding}
	if _, _, err := r.ensureACPClassWorkspace(ctx, task, plan); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	f := &warmSettlementFixture{r: r, plan: plan, binding: binding,
		key: types.NamespacedName{Namespace: task.Namespace, Name: acpClassWorkspaceName(task, binding)}}
	workspace := f.workspace(t)
	admitTestACPWorkspace(t, r, workspace)
	if _, ready := attachTestACPWorkspace(t, r, task, plan, workspace.Name); !ready {
		t.Fatal("attach did not become ready")
	}
	workspace = f.workspace(t)
	base := workspace.DeepCopy()
	// The core controller installs the cleanup finalizer retention waits for.
	workspace.Finalizers = append(workspace.Finalizers, executionWorkspaceFinalizer)
	workspace.Annotations[acpWorkspaceDurableSessionCommittedAnnotation] = "1"
	if err := r.Patch(ctx, workspace, client.MergeFrom(base)); err != nil {
		t.Fatalf("record durable session commit: %v", err)
	}
	f.task = f.reloadTask(t, task)
	f.task.Status.Execution = &corev1alpha1.TaskExecutionStatus{RuntimePoolName: acpTestSessionPoolName}
	if err := r.Status().Update(ctx, f.task); err != nil {
		t.Fatalf("record executed turn: %v", err)
	}
	f.task = f.reloadTask(t, f.task)
	return f
}

func (f *warmSettlementFixture) workspace(t *testing.T) *workspacev1alpha1.ExecutionWorkspace {
	t.Helper()
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	if err := f.r.Get(context.Background(), f.key, workspace); err != nil {
		t.Fatalf("read workspace: %v", err)
	}
	return workspace
}

func (f *warmSettlementFixture) reloadTask(t *testing.T, task *corev1alpha1.Task) *corev1alpha1.Task {
	t.Helper()
	current := &corev1alpha1.Task{}
	if err := f.r.Get(context.Background(), client.ObjectKeyFromObject(task), current); err != nil {
		t.Fatalf("reload task: %v", err)
	}
	return current
}

// settle drives settlement through revocation and the adapter's epoch release.
func (f *warmSettlementFixture) settle(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	f.task = f.reloadTask(t, f.task)
	if done, err := f.r.settleACPClassWorkspace(ctx, f.task); err != nil || done {
		t.Fatalf("settle while attached = (%v, %v), want revocation retry", done, err)
	}
	f.releaseEpoch(t)
	f.task = f.reloadTask(t, f.task)
	if done, err := f.r.settleACPClassWorkspace(ctx, f.task); err != nil || !done {
		t.Fatalf("settle after release = (%v, %v), want completion", done, err)
	}
	f.task = f.reloadTask(t, f.task)
}

// releaseEpoch stands in for the adapter: it stops enforcing the revoked
// epoch and reports the detached workspace Ready again.
func (f *warmSettlementFixture) releaseEpoch(t *testing.T) {
	t.Helper()
	workspace := f.workspace(t)
	base := workspace.DeepCopy()
	workspace.Status.AttachedEpoch = 0
	if workspace.Status.State == workspacev1alpha1.ExecutionWorkspaceStateAttached {
		workspace.Status.State = workspacev1alpha1.ExecutionWorkspaceStateReady
	}
	if err := f.r.Status().Patch(context.Background(), workspace, client.MergeFrom(base)); err != nil {
		t.Fatalf("release enforced epoch: %v", err)
	}
}

func TestSettleACPClassWorkspaceKeepsEligibleDetachWarm(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newWarmSettlementFixture(t, warmTestWindow)
	revokeStart := time.Now()
	if done, err := f.r.settleACPClassWorkspace(ctx, f.task); err != nil || done {
		t.Fatalf("settle while attached = (%v, %v), want revocation retry", done, err)
	}
	revoking := f.workspace(t)
	deadline, open := acpWorkspaceSuspendAtDeadline(revoking)
	if !open || deadline.Before(revokeStart.Add(10*time.Minute)) || deadline.After(time.Now().Add(10*time.Minute)) {
		t.Fatalf("revocation deadline = %v (open %v), want revocation start + window", deadline, open)
	}
	if revoking.Spec.Attachment != nil || !acpWorkspaceWarmWindowHoldsPool(revoking) {
		t.Fatal("the RuntimePool demand rule must hold the runtime while the epoch releases")
	}
	stamp := revoking.Annotations[acpWorkspaceSuspendAtAnnotation]

	f.releaseEpoch(t)
	f.task = f.reloadTask(t, f.task)
	if done, err := f.r.settleACPClassWorkspace(ctx, f.task); err != nil || !done {
		t.Fatalf("warm settle = (%v, %v), want completion", done, err)
	}
	warm := f.workspace(t)
	if warm.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady || !warm.DeletionTimestamp.IsZero() {
		t.Fatalf("warm workspace desired=%s deleting=%v, want Ready", warm.Spec.DesiredState, warm.DeletionTimestamp)
	}
	if warm.Annotations[acpWorkspaceSuspendAtAnnotation] != stamp {
		t.Fatalf("settlement changed the deadline from %q to %q", stamp, warm.Annotations[acpWorkspaceSuspendAtAnnotation])
	}
	if _, present := warm.Annotations[acpWorkspaceRevocationStartedAnnotation]; present {
		t.Fatal("warm settlement kept the revocation stamp")
	}
	if _, present := warm.Annotations[acpWorkspaceResumeRequestedAnnotation]; present {
		t.Fatal("warm settlement kept resume demand")
	}
	if !acpWorkspaceSettlementReceiptCoversTask(warm.Annotations[acpWorkspaceLastSettledTaskAnnotation], string(f.task.UID), acpTaskRecordedAttachmentEpoch(f.task)) {
		t.Fatalf("receipt = %q", warm.Annotations[acpWorkspaceLastSettledTaskAnnotation])
	}
	if _, err := time.Parse(time.RFC3339Nano, warm.Annotations[acpWorkspaceLastDetachedAnnotation]); err != nil {
		t.Fatalf("last-detached-at = %q", warm.Annotations[acpWorkspaceLastDetachedAnnotation])
	}
	if f.reloadTask(t, f.task).Annotations[acpTaskWorkspaceSettledAnnotation] == "" {
		t.Fatal("Task was not marked settled")
	}
	if !acpWorkspaceWarmWindowHoldsPool(warm) {
		t.Fatal("warm workspace does not hold its RuntimePool")
	}

	// Restart safety: a lost Task marker completes from the receipt and
	// never moves the deadline.
	task := f.reloadTask(t, f.task)
	base2 := task.DeepCopy()
	delete(task.Annotations, acpTaskWorkspaceSettledAnnotation)
	if err := f.r.Patch(ctx, task, client.MergeFrom(base2)); err != nil {
		t.Fatal(err)
	}
	task = f.reloadTask(t, task)
	if done, err := f.r.settleACPClassWorkspace(ctx, task); err != nil || !done {
		t.Fatalf("restarted settle = (%v, %v)", done, err)
	}
	if got := f.workspace(t); got.Annotations[acpWorkspaceSuspendAtAnnotation] != stamp ||
		got.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady {
		t.Fatal("restarted settlement changed the warm window")
	}
}

func TestSettleACPClassWorkspaceSkipsWarmWindowWhenIneligible(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		window      string
		mutate      func(*testing.T, *warmSettlementFixture)
		wantDeleted bool
	}{
		{name: "window unset"},
		{name: "parked turn", window: warmTestWindow, mutate: func(t *testing.T, f *warmSettlementFixture) {
			f.task.Status.Wait = &corev1alpha1.TaskWaitStatus{State: corev1alpha1.TaskWaitStateRequested, Kind: corev1alpha1.TaskWaitKindTimer, Attempt: 1, PromptID: "prompt"}
			if err := f.r.Status().Update(context.Background(), f.task); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "operator tainted", window: warmTestWindow, wantDeleted: true, mutate: func(t *testing.T, f *warmSettlementFixture) {
			f.patchWorkspace(t, func(w *workspacev1alpha1.ExecutionWorkspace) { w.Annotations[operatoraccess.TaintAnnotation] = "x" })
		}},
		{name: "uncommitted", window: warmTestWindow, wantDeleted: true, mutate: func(t *testing.T, f *warmSettlementFixture) {
			f.patchWorkspace(t, func(w *workspacev1alpha1.ExecutionWorkspace) {
				delete(w.Annotations, acpWorkspaceDurableSessionCommittedAnnotation)
			})
		}},
		{name: "failed", window: warmTestWindow, wantDeleted: true, mutate: func(t *testing.T, f *warmSettlementFixture) {
			workspace := f.workspace(t)
			base := workspace.DeepCopy()
			workspace.Status.State = workspacev1alpha1.ExecutionWorkspaceStateFailed
			if err := f.r.Status().Patch(context.Background(), workspace, client.MergeFrom(base)); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newWarmSettlementFixture(t, test.window)
			if test.mutate != nil {
				test.mutate(t, f)
			}
			f.settle(t)
			workspace := &workspacev1alpha1.ExecutionWorkspace{}
			err := f.r.Get(context.Background(), f.key, workspace)
			if test.wantDeleted {
				if !apierrors.IsNotFound(err) && (err != nil || workspace.DeletionTimestamp.IsZero()) {
					t.Fatalf("workspace err=%v deleting=%v, want deletion", err, workspace.DeletionTimestamp)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if workspace.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredSuspended {
				t.Fatalf("desired state = %s, want the frozen Suspend applied at detach", workspace.Spec.DesiredState)
			}
			if _, present := workspace.Annotations[acpWorkspaceSuspendAtAnnotation]; present {
				t.Fatal("ineligible detach left a warm deadline")
			}
		})
	}
}

func TestSettleACPClassWorkspaceEndsElapsedWarmWindowWhenQuotaIsExhausted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := suspendableSubstrateFixture(t)
	limit := int32(1)
	fixture.profile.Spec.Retention = &RetentionPolicy{MaxSuspendedWorkspaces: &limit, SuspendAfterDetach: warmTestWindow}
	fixture.pinProfileHash(t)
	task := suspendableSessionTask()
	r := acpClassTestReconciler(t, append(fixture.objects(), task)...)
	task = bindSuspendableSessionTaskForSettlement(t, r, task)
	resolved, err := r.resolveACPWorkspaceClass(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := resolveACPWorkspaceBindingWithClass(task, suspendTestSessionUID, resolved)
	if err != nil {
		t.Fatal(err)
	}
	plan := ACPRuntimePlan{PoolName: acpTestSessionPoolName, Workspace: binding}
	if _, _, err := r.ensureACPClassWorkspace(ctx, task, plan); err != nil {
		t.Fatal(err)
	}
	f := &warmSettlementFixture{r: r, task: task, plan: plan, binding: binding,
		key: types.NamespacedName{Namespace: task.Namespace, Name: acpClassWorkspaceName(task, binding)}}
	workspace := f.workspace(t)
	admitTestACPWorkspace(t, r, workspace)
	if _, ready := attachTestACPWorkspace(t, r, task, plan, workspace.Name); !ready {
		t.Fatal("attach did not become ready")
	}
	f.patchWorkspace(t, func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Annotations[acpWorkspaceDurableSessionCommittedAnnotation] = "1"
	})
	f.task = f.reloadTask(t, task)
	f.task.Status.Execution = &corev1alpha1.TaskExecutionStatus{RuntimePoolName: acpTestSessionPoolName}
	if err := r.Status().Update(ctx, f.task); err != nil {
		t.Fatal(err)
	}
	f.task = f.reloadTask(t, f.task)
	if done, err := r.settleACPClassWorkspace(ctx, f.task); err != nil || done {
		t.Fatalf("settle while attached = (%v, %v)", done, err)
	}
	// Revocation outlasted the window, and another workspace took the cap.
	f.patchWorkspace(t, func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Annotations[acpWorkspaceSuspendAtAnnotation] = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	})
	occupant := acpAdapterWorkspace(t, "")
	occupant.Name = "acp-ws-warm-settle-occupant"
	occupant.UID = types.UID("acp-ws-warm-settle-occupant-uid")
	occupant.Spec.ClassBinding = f.workspace(t).Spec.ClassBinding
	occupant.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
	if err := r.Create(ctx, occupant); err != nil {
		t.Fatal(err)
	}
	f.releaseEpoch(t)
	f.task = f.reloadTask(t, f.task)
	if done, err := r.settleACPClassWorkspace(ctx, f.task); err != nil || done {
		t.Fatalf("quota-blocked settle = (%v, %v), want pending Suspend", done, err)
	}
	current := f.workspace(t)
	if current.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady {
		t.Fatalf("desired state = %s, want Ready with Suspend pending", current.Spec.DesiredState)
	}
	if _, present := current.Annotations[acpWorkspaceSuspendAtAnnotation]; present || acpWorkspaceWarmWindowHoldsPool(current) {
		t.Fatal("an elapsed warm window still holds the runtime while the Suspend is quota-blocked")
	}
}

func (f *warmSettlementFixture) patchWorkspace(t *testing.T, mutate func(*workspacev1alpha1.ExecutionWorkspace)) {
	t.Helper()
	workspace := f.workspace(t)
	base := workspace.DeepCopy()
	mutate(workspace)
	if err := f.r.Patch(context.Background(), workspace, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}
}

func TestWarmDetachContinuationAttachesWithoutColdResume(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newWarmSettlementFixture(t, warmTestWindow)
	f.settle(t)
	warm := f.workspace(t)
	if _, open := acpWorkspaceWarmWindowOpen(warm); !open {
		t.Fatal("settlement did not open a warm window")
	}
	workload := warm.Spec.Workload.DeepCopy()
	epoch := warm.Spec.AttachmentEpoch

	continuation := f.task.DeepCopy()
	continuation.ObjectMeta = metav1.ObjectMeta{Namespace: f.task.Namespace, Name: "warm-continuation", UID: types.UID("warm-continuation-uid")}
	continuation.Status = corev1alpha1.TaskStatus{}
	if err := f.r.Create(ctx, continuation); err != nil {
		t.Fatal(err)
	}
	if _, ready := attachTestACPWorkspace(t, f.r, continuation, f.plan, f.key.Name); !ready {
		t.Fatal("continuation did not attach the warm workspace")
	}
	attached := f.workspace(t)
	if attached.Spec.Attachment == nil || attached.Spec.Attachment.TaskRef.UID != continuation.UID ||
		attached.Spec.AttachmentEpoch != epoch+1 {
		t.Fatalf("attachment = %#v epoch %d, want the continuation at epoch %d", attached.Spec.Attachment, attached.Spec.AttachmentEpoch, epoch+1)
	}
	if _, present := attached.Annotations[acpWorkspaceSuspendAtAnnotation]; present {
		t.Fatal("attaching continuation kept the warm deadline")
	}
	if attached.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady ||
		attached.Annotations[acpWorkspaceResumedLineageAnnotation] != "" ||
		attached.Annotations[acpWorkspaceResumeRequestedAnnotation] != "" {
		t.Fatal("continuation took the cold-resume path")
	}
	if (workload == nil) != (attached.Spec.Workload == nil) ||
		(workload != nil && attached.Spec.Workload.Sequence != workload.Sequence) {
		t.Fatal("continuation started a new workload sequence")
	}
}

func TestWarmDetachAttachAndExpiryResolveToOneOutcome(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Retention reads the expired window, then the continuation's attachment
	// lands before retention's optimistic suspension write.
	f := newWarmSettlementFixture(t, warmTestWindow)
	f.settle(t)
	continuation := f.task.DeepCopy()
	continuation.ObjectMeta = metav1.ObjectMeta{Namespace: f.task.Namespace, Name: "racing-continuation", UID: types.UID("racing-continuation-uid")}
	continuation.Status = corev1alpha1.TaskStatus{}
	if err := f.r.Create(ctx, continuation); err != nil {
		t.Fatal(err)
	}
	raced := false
	racing := interceptor.NewClient(f.r.APIReader.(client.WithWatch), interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if _, ok := obj.(*workspacev1alpha1.ExecutionWorkspace); ok && !raced {
				raced = true
				if _, _, err := f.r.ensureACPClassWorkspace(ctx, continuation, f.plan); err != nil {
					return err
				}
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})
	later := time.Now().Add(time.Hour)
	retention := &ACPWorkspaceRetentionReconciler{Client: racing, APIReader: racing, Now: func() time.Time { return later }}
	if _, err := retention.Reconcile(ctx, ctrl.Request{NamespacedName: f.key}); err != nil {
		t.Fatalf("racing retention: %v", err)
	}
	if !raced {
		t.Fatal("retention never attempted the delayed suspension")
	}
	if _, err := retention.Reconcile(ctx, ctrl.Request{NamespacedName: f.key}); err != nil {
		t.Fatalf("retention after attach: %v", err)
	}
	won := f.workspace(t)
	if won.Spec.Attachment == nil || won.Spec.Attachment.TaskRef.UID != continuation.UID ||
		won.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady {
		t.Fatalf("attach-first race: attachment=%#v desired=%s, want the attachment only", won.Spec.Attachment, won.Spec.DesiredState)
	}

	// The suspension lands first: the continuation can no longer attach the
	// workspace directly and takes today's cold-resume path.
	g := newWarmSettlementFixture(t, warmTestWindow)
	g.settle(t)
	expiry := &ACPWorkspaceRetentionReconciler{Client: g.r.Client, APIReader: g.r.APIReader, Now: func() time.Time { return later }}
	if _, err := expiry.Reconcile(ctx, ctrl.Request{NamespacedName: g.key}); err != nil {
		t.Fatalf("expire window: %v", err)
	}
	suspended := g.workspace(t)
	if suspended.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredSuspended {
		t.Fatalf("desired state = %s, want Suspended", suspended.Spec.DesiredState)
	}
	late := g.task.DeepCopy()
	late.ObjectMeta = metav1.ObjectMeta{Namespace: g.task.Namespace, Name: "late-continuation", UID: types.UID("late-continuation-uid")}
	late.Status = corev1alpha1.TaskStatus{}
	if err := g.r.Create(ctx, late); err != nil {
		t.Fatal(err)
	}
	if _, ready, err := g.r.ensureACPClassWorkspace(ctx, late, g.plan); err != nil || ready {
		t.Fatalf("late continuation = (%v, %v), want a pending cold resume", ready, err)
	}
	if got := g.workspace(t); got.Spec.Attachment != nil ||
		!acpWorkspaceResumeDemandBelongsToTask(got.Annotations[acpWorkspaceResumeRequestedAnnotation], string(late.UID)) {
		t.Fatal("late continuation attached the suspended workspace instead of requesting cold resume")
	}

	// An attach retry that rereads a just-suspended workspace refuses it.
	if err := validateACPClassWorkspaceAttachmentAttempt(suspended, late, g.binding, g.plan.PoolName, time.Now()); err == nil ||
		!strings.Contains(err.Error(), "not reusable") {
		t.Fatalf("direct attach of a Suspended workspace error = %v", err)
	}
}

func warmRetentionWorkspace(t *testing.T, name string, deadline time.Time, mutate ...func(*workspacev1alpha1.ExecutionWorkspace)) *workspacev1alpha1.ExecutionWorkspace {
	t.Helper()
	return retentionTestWorkspace(t, name, append([]func(*workspacev1alpha1.ExecutionWorkspace){func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Spec.SessionRef = &workspacev1alpha1.ObjectIdentityReference{Name: acpTestSessionName, UID: types.UID(name + "-session-uid")}
		w.Spec.Lifecycle.DefaultOnDetach = workspacev1alpha1.WorkspaceOnDetachSuspend
		w.Spec.Lifecycle.AllowedOnDetach = []workspacev1alpha1.WorkspaceOnDetach{
			workspacev1alpha1.WorkspaceOnDetachSuspend, workspacev1alpha1.WorkspaceOnDetachDelete,
		}
		w.Spec.AttachmentEpoch = 1
		w.Status.State = workspacev1alpha1.ExecutionWorkspaceStateReady
		w.Annotations[acpWorkspaceDetachActionAnnotation] = string(workspacev1alpha1.WorkspaceOnDetachSuspend)
		w.Annotations[acpWorkspaceDurableAnnotation] = booleanTrueValue
		w.Annotations[acpWorkspaceDurableSessionCommittedAnnotation] = "1"
		w.Annotations[acpWorkspaceSuspendAfterDetachAnnotation] = "10m0s"
		w.Annotations[acpWorkspaceLastDetachedAnnotation] = deadline.Add(-10 * time.Minute).UTC().Format(time.RFC3339Nano)
		w.Annotations[acpWorkspaceSuspendAtAnnotation] = deadline.UTC().Format(time.RFC3339Nano)
	}}, mutate...)...)
}

func TestACPWorkspaceRetentionSuspendsWarmWorkspaceAtDeadline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	deadline := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	workspace := warmRetentionWorkspace(t, "acp-ws-warm-deadline", deadline, func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.CreationTimestamp = metav1.NewTime(deadline.Add(-time.Hour))
		// The window is independent of idle retention.
		w.Spec.Lifecycle.IdleTimeout = nil
	})
	c := acpAdapterTestClient(t, workspace)
	now := deadline.Add(-3 * time.Minute)
	retention := &ACPWorkspaceRetentionReconciler{Client: c, APIReader: c, Now: func() time.Time { return now }}
	result, err := retention.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workspace)})
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != 3*time.Minute+time.Second {
		t.Fatalf("requeue = %s, want the deadline", result.RequeueAfter)
	}
	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), current); err != nil {
		t.Fatal(err)
	}
	if current.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady {
		t.Fatal("retention suspended before the deadline")
	}

	now = deadline.Add(time.Second)
	if _, err := retention.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workspace)}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), current); err != nil {
		t.Fatal(err)
	}
	if current.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredSuspended {
		t.Fatalf("desired state = %s, want Suspended at the deadline", current.Spec.DesiredState)
	}
	if _, present := current.Annotations[acpWorkspaceSuspendAtAnnotation]; present {
		t.Fatal("suspension kept the warm deadline")
	}
	if got := current.Annotations[acpWorkspaceLastDetachedAnnotation]; got != now.Format(time.RFC3339Nano) {
		t.Fatalf("last-detached-at = %q, want the suspension instant", got)
	}
}

func TestACPWorkspaceRetentionEndsWarmWindowOnQuotaExhaustion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	deadline := time.Now().Add(-time.Minute)
	workspace := warmRetentionWorkspace(t, "acp-ws-warm-quota", deadline, func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Annotations[acpWorkspaceMaxSuspendedAnnotation] = "1"
	})
	occupant := acpAdapterWorkspace(t, "")
	occupant.Name = "acp-ws-warm-quota-occupant"
	occupant.UID = types.UID("acp-ws-warm-quota-occupant-uid")
	occupant.Spec.ClassBinding = workspace.Spec.ClassBinding
	occupant.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
	c := acpAdapterTestClient(t, workspace, occupant)
	reconcileRetention(t, c, workspace)
	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), current); err != nil {
		t.Fatal(err)
	}
	if current.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady || !current.DeletionTimestamp.IsZero() {
		t.Fatalf("quota exhaustion changed the workspace: desired=%s", current.Spec.DesiredState)
	}
	if _, present := current.Annotations[acpWorkspaceSuspendAtAnnotation]; present {
		t.Fatal("quota exhaustion kept the warm deadline, so the runtime would keep running")
	}
	if acpWorkspaceWarmWindowHoldsPool(current) {
		t.Fatal("the RuntimePool is still held after quota exhaustion")
	}
	if _, pending := current.Annotations[acpWorkspaceSuspendPendingAnnotation]; !pending {
		t.Fatal("quota exhaustion dropped the pending frozen Suspend")
	}

	// Still exhausted: the Suspend stays pending without holding the runtime.
	reconcileRetention(t, c, workspace)
	if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), current); err != nil {
		t.Fatal(err)
	}
	if current.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady {
		t.Fatalf("desired state = %s while the cap is exhausted", current.Spec.DesiredState)
	}

	// Capacity opens: retention retries and applies the frozen Suspend, even
	// for a class without idle retention.
	base := current.DeepCopy()
	current.Spec.Lifecycle.IdleTimeout = nil
	if err := c.Patch(ctx, current, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, occupant); err != nil {
		t.Fatal(err)
	}
	reconcileRetention(t, c, workspace)
	if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), current); err != nil {
		t.Fatal(err)
	}
	if current.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredSuspended {
		t.Fatalf("desired state = %s, want the pending Suspend applied once capacity opens", current.Spec.DesiredState)
	}
	if _, pending := current.Annotations[acpWorkspaceSuspendPendingAnnotation]; pending {
		t.Fatal("suspension kept the pending marker")
	}
}

func TestACPWorkspaceRetentionExpiresTaintedWarmWorkspaceImmediately(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	workspace := warmRetentionWorkspace(t, "acp-ws-warm-tainted", time.Now().Add(9*time.Minute), func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Annotations[operatoraccess.TaintAnnotation] = "x"
	})
	c := acpAdapterTestClient(t, workspace)
	reconcileRetention(t, c, workspace)
	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), current); err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	} else if err == nil && current.DeletionTimestamp.IsZero() {
		t.Fatal("tainted warm workspace was not deleted before its deadline")
	}
}

// A fenced warm-window expiry that a queued Session continuation vetoes
// deletes nothing. Retention watches only workspaces, so it must keep its
// bounded lifetime requeue instead of leaving idle and maxLifetime
// enforcement unscheduled if that continuation dies without touching the
// workspace.
func TestACPWorkspaceRetentionWarmExpiryVetoedByContinuationKeepsRequeue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Now().UTC()
	for _, test := range []struct {
		name   string
		mutate func(*workspacev1alpha1.ExecutionWorkspace)
	}{
		{name: "tainted", mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Annotations[operatoraccess.TaintAnnotation] = "x"
		}},
		{name: "failed", mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Status.State = workspacev1alpha1.ExecutionWorkspaceStateFailed
		}},
		{name: "uncommitted", mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			delete(w.Annotations, acpWorkspaceDurableSessionCommittedAnnotation)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			name := "acp-ws-warm-veto-" + test.name
			// The tainted case cuts an open window short; the others expire.
			deadline := now.Add(-time.Minute)
			if test.name == "tainted" {
				deadline = now.Add(5 * time.Minute)
			}
			workspace := warmRetentionWorkspace(t, name, deadline, test.mutate)
			sessionUID := string(workspace.Spec.SessionRef.UID)
			control := &corev1alpha1.RuntimeSessionControl{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: workspace.Namespace,
					Name:      storekube.RuntimeSessionControlObjectName(acpTestSessionName),
					UID:       types.UID(name + "-control-uid"),
				},
				Spec: corev1alpha1.RuntimeSessionControlSpec{SessionName: acpTestSessionName, SessionUID: sessionUID},
			}
			continuation := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Namespace:         workspace.Namespace,
					Name:              name + "-continuation",
					UID:               types.UID(name + "-continuation-uid"),
					CreationTimestamp: metav1.NewTime(now.Add(-time.Second)),
					Labels:            map[string]string{acpExecutionWorkspaceLinkLabel: workspace.Name},
					Annotations:       map[string]string{acpExecutionWorkspaceUIDAnnotation: string(workspace.UID)},
				},
				Spec: corev1alpha1.TaskSpec{
					Type:       corev1alpha1.TaskTypeAgent,
					SessionRef: &corev1alpha1.SessionReference{Name: acpTestSessionName},
				},
			}
			base := acpAdapterTestClient(t, workspace, control, continuation)
			c := retentionFenceTimestampClient(base, func() time.Time { return now })
			retention := &ACPWorkspaceRetentionReconciler{Client: c, APIReader: c, Now: func() time.Time { return now }}
			result, err := retention.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workspace)})
			if err != nil {
				t.Fatal(err)
			}
			current := &workspacev1alpha1.ExecutionWorkspace{}
			if err := base.Get(ctx, client.ObjectKeyFromObject(workspace), current); err != nil {
				t.Fatal(err)
			}
			if !current.DeletionTimestamp.IsZero() {
				t.Fatal("expiry deleted a workspace a queued continuation still demands")
			}
			fence := &coordinationv1.Lease{}
			if err := base.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: acpWorkspaceRetentionFenceLeaseName(workspace)}, fence); !apierrors.IsNotFound(err) {
				t.Fatalf("vetoed expiry kept its retention fence: %v", err)
			}
			if result.RequeueAfter != acpWorkspaceRetentionRequeue {
				t.Fatalf("requeue = %s, want the bounded lifetime requeue %s", result.RequeueAfter, acpWorkspaceRetentionRequeue)
			}
		})
	}
}

func TestACPWorkspaceRetentionWarmExpiryAppliesIdleGuards(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, test := range []struct {
		name   string
		mutate func(*workspacev1alpha1.ExecutionWorkspace)
	}{
		{name: "failed", mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Status.State = workspacev1alpha1.ExecutionWorkspaceStateFailed
		}},
		{name: "uncommitted", mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			delete(w.Annotations, acpWorkspaceDurableSessionCommittedAnnotation)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			workspace := warmRetentionWorkspace(t, "acp-ws-warm-"+test.name, time.Now().Add(-time.Minute), test.mutate)
			c := acpAdapterTestClient(t, workspace)
			reconcileRetention(t, c, workspace)
			current := &workspacev1alpha1.ExecutionWorkspace{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), current); err != nil && !apierrors.IsNotFound(err) {
				t.Fatal(err)
			} else if err == nil && current.DeletionTimestamp.IsZero() {
				t.Fatalf("expired %s warm workspace was not deleted", test.name)
			}
		})
	}
}

func TestWarmReadyWorkspaceDoesNotConsumeSuspendedQuota(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	warm := warmRetentionWorkspace(t, "acp-ws-warm-uncounted", time.Now().Add(5*time.Minute))
	c := acpAdapterTestClient(t, warm)
	if workspaceConsumesSuspendedQuota(warm) {
		t.Fatal("warm Ready workspace consumes suspended quota")
	}
	count, err := countSuspendedClassWorkspaces(ctx, c, warm.Namespace, warm.Spec.ClassBinding.UID, nil)
	if err != nil || count != 0 {
		t.Fatalf("suspended count = (%d, %v), want 0", count, err)
	}
}
