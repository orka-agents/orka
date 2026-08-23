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
	"github.com/orka-agents/orka/internal/store"
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

func TestACPWorkspaceRetentionWaitsForEnforcedEpoch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// Revocation cleared the attachment but the adapter still enforces the
	// epoch, and no detach instant is recorded: idle evaluation must wait
	// instead of falling back to the creation timestamp and expiring a
	// workspace whose Task simply ran longer than idleTimeout.
	workspace := retentionTestWorkspace(t, "acp-ws-retention-g", func(w *workspacev1alpha1.ExecutionWorkspace) {
		// The high-water mark alone never defers idle handling; the
		// adapter-enforced epoch does.
		w.Spec.AttachmentEpoch = 3
		w.Status.AttachedEpoch = 3
	})
	c := acpAdapterTestClient(t, workspace)
	result := reconcileRetention(t, c, workspace)
	if result.RequeueAfter <= 0 {
		t.Fatalf("an enforced epoch must requeue idle evaluation, got %+v", result)
	}
	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, current); err != nil {
		t.Fatalf("workspace must survive while the epoch settles: %v", err)
	}
	if current.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady {
		t.Fatalf("desired state = %s, want it untouched while the epoch settles", current.Spec.DesiredState)
	}
}

func TestACPWorkspaceRetentionIdleSuspensionHonorsQuota(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	suspendable := func(name string) *workspacev1alpha1.ExecutionWorkspace {
		return retentionTestWorkspace(t, name, func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Spec.Lifecycle.DefaultOnDetach = workspacev1alpha1.WorkspaceOnDetachSuspend
			w.Spec.Lifecycle.AllowedOnDetach = []workspacev1alpha1.WorkspaceOnDetach{
				workspacev1alpha1.WorkspaceOnDetachSuspend, workspacev1alpha1.WorkspaceOnDetachDelete,
			}
			w.Annotations[acpWorkspaceDetachActionAnnotation] = string(workspacev1alpha1.WorkspaceOnDetachSuspend)
			w.Annotations[acpWorkspaceMaxSuspendedAnnotation] = "1"
			w.Annotations[acpWorkspaceLastDetachedAnnotation] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
		})
	}
	idle := suspendable("acp-ws-retention-h")
	occupant := suspendable("acp-ws-retention-i")
	occupant.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
	occupant.Annotations[acpWorkspaceLastDetachedAnnotation] = time.Now().UTC().Format(time.RFC3339Nano)
	c := acpAdapterTestClient(t, idle, occupant)
	reconcileRetention(t, c, idle)
	err := c.Get(ctx, types.NamespacedName{Namespace: idle.Namespace, Name: idle.Name}, &workspacev1alpha1.ExecutionWorkspace{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("idle suspension over an exhausted cap must apply the Delete disposition, got %v", err)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: occupant.Namespace, Name: occupant.Name}, &workspacev1alpha1.ExecutionWorkspace{}); err != nil {
		t.Fatalf("the quota occupant must survive: %v", err)
	}
}

func TestACPWorkspaceRetentionSuspendStampsFreshDetachInstant(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	workspace := retentionTestWorkspace(t, "acp-ws-retention-j", func(w *workspacev1alpha1.ExecutionWorkspace) {
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
		t.Fatalf("read suspended workspace: %v", err)
	}
	if current.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredSuspended {
		t.Fatalf("desired state = %s, want Suspended", current.Spec.DesiredState)
	}
	stamp, err := time.Parse(time.RFC3339Nano, current.Annotations[acpWorkspaceLastDetachedAnnotation])
	if err != nil {
		t.Fatalf("parse refreshed detach instant: %v", err)
	}
	if time.Since(stamp) > time.Minute {
		t.Fatalf("idle suspension must stamp a fresh detach instant so the suspended state earns a full retention interval, got %s", stamp)
	}
	// The freshly suspended workspace is not immediately expired by the next
	// retention pass.
	if result := reconcileRetention(t, c, current); result.RequeueAfter <= 0 {
		t.Fatalf("freshly suspended workspace must requeue, got %+v", result)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, current); err != nil {
		t.Fatalf("freshly suspended workspace must survive the next pass: %v", err)
	}
}

func TestACPWorkspaceRetentionSuspendsNeverAttachedDefaultSuspendWorkspaces(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// A Task materialized the workspace and was cancelled before attachment:
	// the creation-stamped frozen detach action still honors the class
	// default Suspend once idleTimeout elapses.
	workspace := retentionTestWorkspace(t, "acp-ws-retention-k", func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Spec.Lifecycle.DefaultOnDetach = workspacev1alpha1.WorkspaceOnDetachSuspend
		w.Spec.Lifecycle.AllowedOnDetach = []workspacev1alpha1.WorkspaceOnDetach{
			workspacev1alpha1.WorkspaceOnDetachSuspend, workspacev1alpha1.WorkspaceOnDetachDelete,
		}
		w.Annotations[acpWorkspaceDetachActionAnnotation] = string(workspacev1alpha1.WorkspaceOnDetachSuspend)
	})
	c := acpAdapterTestClient(t, workspace)
	reconcileRetention(t, c, workspace)
	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, current); err != nil {
		t.Fatalf("never-attached suspendable workspace must survive as suspended: %v", err)
	}
	if current.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredSuspended {
		t.Fatalf("desired state = %s, want the class default Suspend honored", current.Spec.DesiredState)
	}
}

// A cold resume in flight (DesiredState Ready, observed state still
// Suspended/Suspending) is active demand: idle retention must wait.
func TestACPWorkspaceRetentionWaitsForColdResumeInFlight(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	workspace := retentionTestWorkspace(t, "acp-ws-retention-l", func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Spec.Lifecycle.DefaultOnDetach = workspacev1alpha1.WorkspaceOnDetachSuspend
		w.Annotations[acpWorkspaceDetachActionAnnotation] = string(workspacev1alpha1.WorkspaceOnDetachSuspend)
		w.Annotations[acpWorkspaceLastDetachedAnnotation] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
		w.Status.State = workspacev1alpha1.ExecutionWorkspaceStateSuspended
	})
	c := acpAdapterTestClient(t, workspace)
	result := reconcileRetention(t, c, workspace)
	if result.RequeueAfter <= 0 {
		t.Fatalf("resume in flight must requeue, got %+v", result)
	}
	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, current); err != nil {
		t.Fatalf("workspace must survive a resume in flight: %v", err)
	}
	if current.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady {
		t.Fatalf("desired state = %s, want the resume request untouched", current.Spec.DesiredState)
	}
}

// A terminally failed suspension preserves no data and must not hold a quota
// slot, and a quarantined workspace still expires at its frozen maxLifetime.
func TestACPWorkspaceRetentionFailedAndQuarantinedHandling(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	failed := retentionTestWorkspace(t, "acp-ws-retention-m", func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
		w.Status.State = workspacev1alpha1.ExecutionWorkspaceStateFailed
	})
	c := acpAdapterTestClient(t, failed)
	count, err := countSuspendedClassWorkspaces(ctx, c, failed.Namespace, failed.Spec.ClassBinding.UID, nil)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("count = %d, want a failed suspension excluded from the quota", count)
	}

	quarantined := retentionTestWorkspace(t, "acp-ws-retention-n", func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined
		w.CreationTimestamp = metav1.NewTime(time.Now().Add(-25 * time.Hour))
	})
	qc := acpAdapterTestClient(t, quarantined)
	reconcileRetention(t, qc, quarantined)
	getErr := qc.Get(ctx, types.NamespacedName{Namespace: quarantined.Namespace, Name: quarantined.Name}, &workspacev1alpha1.ExecutionWorkspace{})
	if !apierrors.IsNotFound(getErr) {
		t.Fatalf("quarantined workspace past maxLifetime must be deleted, got %v", getErr)
	}

	// Before maxLifetime, quarantine skips idle handling but survives.
	fresh := retentionTestWorkspace(t, "acp-ws-retention-o", func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined
		w.Annotations[acpWorkspaceLastDetachedAnnotation] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	})
	fc := acpAdapterTestClient(t, fresh)
	reconcileRetention(t, fc, fresh)
	if err := fc.Get(ctx, types.NamespacedName{Namespace: fresh.Namespace, Name: fresh.Name}, &workspacev1alpha1.ExecutionWorkspace{}); err != nil {
		t.Fatalf("quarantined workspace inside maxLifetime must survive: %v", err)
	}
}

// quotaSessionControlStore serves exactly the GetSessionControl lookup the
// suspend-quota exclusion performs; sessions maps name -> immutable UID.
type quotaSessionControlStore struct {
	store.DurableControlStore
	namespace string
	sessions  map[string]string
}

func (s *quotaSessionControlStore) GetSessionControl(_ context.Context, namespace, name string) (*store.SessionControl, error) {
	uid, ok := s.sessions[name]
	if !ok || namespace != s.namespace {
		return nil, store.ErrNotFound
	}
	return &store.SessionControl{
		Namespace: namespace, SessionName: name, SessionUID: uid,
		Availability: store.SessionAvailable,
	}, nil
}

type quotaSessionTranscriptStore struct {
	store.SessionStore
	namespace string
	sessions  map[string]string
}

func (s *quotaSessionTranscriptStore) GetSession(_ context.Context, namespace, name string) (*store.SessionRecord, error) {
	if _, ok := s.sessions[name]; !ok || namespace != s.namespace {
		return nil, store.ErrNotFound
	}
	return &store.SessionRecord{Namespace: namespace, Name: name, SessionType: defaultACPSessionType}, nil
}

// attachQuotaSessionStores wires minimal durable-session fakes so class
// resolution can resolve immutable Session UIDs for quota exclusions.
func attachQuotaSessionStores(r *TaskReconciler, sessions map[string]string) {
	r.DurableControlStore = &quotaSessionControlStore{namespace: acpTestNamespace, sessions: sessions}
	r.SessionManager = &SessionManager{store: &quotaSessionTranscriptStore{namespace: acpTestNamespace, sessions: sessions}}
	r.ControllerEpochManager = &ControllerEpochManager{}
}

func TestACPWorkspaceSuspendQuotaAdmitsOwnSessionContinuation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := suspendableSubstrateFixture(t)
	fixture.profile.Spec.Retention = &acpworkspacev1alpha1.RetentionPolicy{
		MaxSuspendedWorkspaces: func() *int32 { limit := int32(1); return &limit }(),
	}
	fixture.pinProfileHash(t)
	task := suspendableSessionTask()
	// The last quota slot is held by this very session's suspended workspace:
	// the continuation that would resume it must still be admitted, or it
	// could never reach ensureACPClassWorkspace to free the slot.
	own := acpAdapterWorkspace(t, "")
	own.Name = "acp-ws-own-session-suspended"
	own.UID = types.UID("own-session-suspended-uid")
	own.Spec.ClassBinding = workspacev1alpha1.ImmutableObjectBinding{
		Name: fixture.class.Name, UID: fixture.class.UID, Generation: 1, ProfileHash: fixture.class.Status.ProfileHash,
	}
	own.Spec.SessionRef = &workspacev1alpha1.ObjectIdentityReference{Name: acpTestSessionName, UID: types.UID("session-uid-1")}
	own.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
	r := acpClassTestReconciler(t, append(fixture.objects(), task, own)...)
	attachQuotaSessionStores(r, map[string]string{acpTestSessionName: "session-uid-1"})
	if _, err := r.resolveACPWorkspaceClass(ctx, task); err != nil {
		t.Fatalf("a continuation of the suspended session must be admitted, got %v", err)
	}

	// A Task from another session still sees the exhausted cap.
	foreignSession := acpClassTestTask(func(other *corev1alpha1.Task) {
		other.Name = "other-session-task"
		other.UID = types.UID("other-session-task-uid")
		other.Spec.Execution.Workspace.ReusePolicy = corev1alpha1.WorkspaceReusePolicySession
		other.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "session-b", Create: true}
	})
	if err := r.Create(ctx, foreignSession); err != nil {
		t.Fatalf("create foreign-session task: %v", err)
	}
	if _, err := r.resolveACPWorkspaceClass(ctx, foreignSession); err == nil ||
		!strings.Contains(err.Error(), "retention cap") {
		t.Fatalf("foreign-session admission error = %v, want retention cap exhaustion", err)
	}

	// A Session recreated under the same NAME resolves a different immutable
	// UID: the old incarnation's suspended workspace still consumes the cap,
	// so exclusion never matches on the reusable name alone.
	attachQuotaSessionStores(r, map[string]string{acpTestSessionName: "session-uid-2"})
	if _, err := r.resolveACPWorkspaceClass(ctx, task); err == nil ||
		!strings.Contains(err.Error(), "retention cap") {
		t.Fatalf("recreated-session admission error = %v, want the old-UID workspace still counted", err)
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
	if workspace.Annotations[acpWorkspaceDetachActionAnnotation] != string(workspacev1alpha1.WorkspaceOnDetachSuspend) {
		t.Fatalf("creation must freeze the effective detach action, got %q",
			workspace.Annotations[acpWorkspaceDetachActionAnnotation])
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
	// Settlement is a multi-reconcile flow: revocation start intentionally
	// returns not-done so the next reconcile re-reads uncached state.
	done := false
	for attempt := 0; attempt < 5 && !done; attempt++ {
		var settleErr error
		if done, settleErr = r.settleACPClassWorkspace(ctx, task); settleErr != nil {
			t.Fatalf("settle attempt %d: %v", attempt, settleErr)
		}
	}
	if !done {
		t.Fatal("settle never completed")
	}
	err = r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: name}, workspace)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("quota-exhausted settlement must apply the Delete disposition, got %v", err)
	}
}
