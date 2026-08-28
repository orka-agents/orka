/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	acpworkspacev1alpha1 "github.com/orka-agents/orka/api/acp.workspace/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
)

const (
	emptyCaseName       = "empty"
	whitespaceCaseName  = "whitespace"
	whitespaceOnlyValue = " \t "
)

func retentionTestWorkspace(t *testing.T, name string, mutate ...func(*workspacev1alpha1.ExecutionWorkspace)) *workspacev1alpha1.ExecutionWorkspace {
	t.Helper()
	workspace := acpAdapterWorkspace(t, "")
	workspace.Name = name
	workspace.UID = types.UID(name + "-uid")
	workspace.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour))
	// Retention acts only once the core cleanup finalizer is installed.
	workspace.Finalizers = append(workspace.Finalizers, executionWorkspaceFinalizer)
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
	deleting := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, deleting); err != nil || deleting.DeletionTimestamp.IsZero() {
		t.Fatalf("suspended workspace past its idle retention must be deleting (finalizer-held), got err=%v deleting=%v", err, deleting.DeletionTimestamp)
	}
}

// A malformed controller-written last-detached-at stamp means the
// admission-protected metadata was corrupted: idle retention must fail closed
// on the bounded maxLifetime path instead of treating the workspace as
// instantly idle-expired via the creation-time fallback.
func TestACPWorkspaceRetentionFailsClosedOnMalformedIdleStamp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	workspace := retentionTestWorkspace(t, "acp-ws-retention-badstamp", func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
		w.Annotations[acpWorkspaceLastDetachedAnnotation] = "not-a-timestamp"
	})
	c := acpAdapterTestClient(t, workspace)
	result := reconcileRetention(t, c, workspace)
	if result.RequeueAfter <= 0 {
		t.Fatalf("a malformed idle stamp must hold on a bounded requeue, got %+v", result)
	}
	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, current); err != nil {
		t.Fatalf("workspace must survive a malformed idle stamp: %v", err)
	}
	if !current.DeletionTimestamp.IsZero() {
		t.Fatal("a malformed idle stamp must never expire the workspace through the creation-time fallback")
	}
}

func TestACPWorkspaceRetentionFailsClosedOnEmptyIdleStamp(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		stamp string
	}{
		{name: emptyCaseName, stamp: ""},
		{name: whitespaceCaseName, stamp: whitespaceOnlyValue},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			workspace := retentionTestWorkspace(t, "acp-ws-retention-empty-stamp-"+test.name, func(w *workspacev1alpha1.ExecutionWorkspace) {
				w.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
				w.Annotations[acpWorkspaceLastDetachedAnnotation] = test.stamp
			})
			c := acpAdapterTestClient(t, workspace)
			result := reconcileRetention(t, c, workspace)
			if result.RequeueAfter <= 0 {
				t.Fatalf("an empty idle stamp must hold on a bounded requeue, got %+v", result)
			}
			current := &workspacev1alpha1.ExecutionWorkspace{}
			if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, current); err != nil {
				t.Fatalf("workspace must survive an empty idle stamp: %v", err)
			}
			if !current.DeletionTimestamp.IsZero() {
				t.Fatal("an empty idle stamp must never expire the workspace through the creation-time fallback")
			}
		})
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
			TaskRef:        workspacev1alpha1.ObjectIdentityReference{Name: acpTestAttachedTask, UID: types.UID("task-uid")},
			Epoch:          1,
			TokenSHA256:    "sha256:" + strings.Repeat("d", 64),
			TokenSecretRef: workspacev1alpha1.SecretReference{Name: "attach"},
			ExpiresAt:      metav1.Now(),
		}
	})
	c := acpAdapterTestClient(t, workspace)
	reconcileRetention(t, c, workspace)
	deleting := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, deleting); err != nil || deleting.DeletionTimestamp.IsZero() {
		t.Fatalf("maxLifetime must force terminal cleanup even while attached, got err=%v deleting=%v", err, deleting.DeletionTimestamp)
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

func TestACPWorkspaceRetentionHonorsDefaultSuspendWithoutActionStamp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	workspace := retentionTestWorkspace(t, "acp-ws-retention-default-suspend", func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Spec.Lifecycle.DefaultOnDetach = workspacev1alpha1.WorkspaceOnDetachSuspend
		w.Spec.Lifecycle.AllowedOnDetach = []workspacev1alpha1.WorkspaceOnDetach{
			workspacev1alpha1.WorkspaceOnDetachSuspend, workspacev1alpha1.WorkspaceOnDetachDelete,
		}
		w.Annotations[acpWorkspaceDurableAnnotation] = booleanTrueValue
		w.Annotations[acpWorkspaceLastDetachedAnnotation] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	})
	c := acpAdapterTestClient(t, workspace)
	reconcileRetention(t, c, workspace)
	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), current); err != nil {
		t.Fatalf("default-Suspend workspace without an action stamp must survive: %v", err)
	}
	if current.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredSuspended {
		t.Fatalf("desired state = %s, want the frozen default Suspend action honored", current.Spec.DesiredState)
	}
}

func TestACPWorkspaceRetentionFailsClosedOnMalformedDurableCapability(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"", whitespaceOnlyValue, testFalseValue} {
		t.Run(value, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			workspace := retentionTestWorkspace(t, "acp-ws-retention-invalid-durable", func(w *workspacev1alpha1.ExecutionWorkspace) {
				w.Spec.Lifecycle.DefaultOnDetach = workspacev1alpha1.WorkspaceOnDetachSuspend
				w.Spec.Lifecycle.AllowedOnDetach = []workspacev1alpha1.WorkspaceOnDetach{
					workspacev1alpha1.WorkspaceOnDetachSuspend, workspacev1alpha1.WorkspaceOnDetachDelete,
				}
				delete(w.Annotations, acpWorkspaceDetachActionAnnotation)
				w.Annotations[acpWorkspaceDurableAnnotation] = value
				w.Annotations[acpWorkspaceLastDetachedAnnotation] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
			})
			c := acpAdapterTestClient(t, workspace)
			result := reconcileRetention(t, c, workspace)
			if result.RequeueAfter <= 0 {
				t.Fatalf("an invalid durable marker must hold on a bounded requeue, got %+v", result)
			}
			current := &workspacev1alpha1.ExecutionWorkspace{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), current); err != nil {
				t.Fatalf("workspace must survive an invalid durable marker: %v", err)
			}
			if !current.DeletionTimestamp.IsZero() || current.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady {
				t.Fatalf("invalid durable marker changed the workspace: deleting=%v desired=%s",
					current.DeletionTimestamp, current.Spec.DesiredState)
			}
		})
	}
}

func TestACPWorkspaceRetentionUsesClassDefaultAfterTaskOverride(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	workspace := retentionTestWorkspace(t, "acp-ws-retention-class-default", func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Spec.Lifecycle.DefaultOnDetach = workspacev1alpha1.WorkspaceOnDetachDelete
		w.Spec.Lifecycle.AllowedOnDetach = []workspacev1alpha1.WorkspaceOnDetach{
			workspacev1alpha1.WorkspaceOnDetachSuspend, workspacev1alpha1.WorkspaceOnDetachDelete,
		}
		w.Annotations[acpWorkspaceDetachActionAnnotation] = string(workspacev1alpha1.WorkspaceOnDetachSuspend)
		w.Annotations[acpWorkspaceDurableAnnotation] = booleanTrueValue
		w.Annotations[acpWorkspaceLastDetachedAnnotation] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	})
	c := acpAdapterTestClient(t, workspace)
	reconcileRetention(t, c, workspace)
	deleting := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), deleting); err != nil || deleting.DeletionTimestamp.IsZero() {
		t.Fatalf("idle retention must apply the class Delete default after a Task Suspend override, got err=%v deleting=%v",
			err, deleting.DeletionTimestamp)
	}
}

func TestACPWorkspaceRetentionDeletesIdleFailedReadyWorkspaces(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	workspace := retentionTestWorkspace(t, "acp-ws-retention-failed-ready", func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Spec.Lifecycle.DefaultOnDetach = workspacev1alpha1.WorkspaceOnDetachSuspend
		w.Spec.Lifecycle.AllowedOnDetach = []workspacev1alpha1.WorkspaceOnDetach{
			workspacev1alpha1.WorkspaceOnDetachSuspend, workspacev1alpha1.WorkspaceOnDetachDelete,
		}
		w.Annotations[acpWorkspaceDetachActionAnnotation] = string(workspacev1alpha1.WorkspaceOnDetachSuspend)
		w.Annotations[acpWorkspaceDurableAnnotation] = booleanTrueValue
		w.Annotations[acpWorkspaceResumedLineageAnnotation] = booleanTrueValue
		w.Annotations[acpWorkspaceLastDetachedAnnotation] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
		w.Status.State = workspacev1alpha1.ExecutionWorkspaceStateFailed
	})
	c := acpAdapterTestClient(t, workspace)
	reconcileRetention(t, c, workspace)
	deleting := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), deleting); err != nil || deleting.DeletionTimestamp.IsZero() {
		t.Fatalf("idle terminal failure must enter deletion without another suspension interval, got err=%v deleting=%v",
			err, deleting.DeletionTimestamp)
	}
}

func TestACPWorkspaceRetentionFailsClosedOnEmptyDetachAction(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		action string
	}{
		{name: emptyCaseName, action: ""},
		{name: whitespaceCaseName, action: whitespaceOnlyValue},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			workspace := retentionTestWorkspace(t, "acp-ws-retention-empty-action-"+test.name, func(w *workspacev1alpha1.ExecutionWorkspace) {
				w.Annotations[acpWorkspaceDetachActionAnnotation] = test.action
				w.Annotations[acpWorkspaceLastDetachedAnnotation] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
			})
			c := acpAdapterTestClient(t, workspace)
			result := reconcileRetention(t, c, workspace)
			if result.RequeueAfter <= 0 {
				t.Fatalf("an invalid frozen detach action must hold on a bounded requeue, got %+v", result)
			}
			current := &workspacev1alpha1.ExecutionWorkspace{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), current); err != nil {
				t.Fatalf("workspace must survive an invalid frozen detach action: %v", err)
			}
			if !current.DeletionTimestamp.IsZero() || current.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady {
				t.Fatalf("invalid frozen detach action changed the workspace: deleting=%v desired=%s",
					current.DeletionTimestamp, current.Spec.DesiredState)
			}
		})
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
	deleting := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, deleting); err != nil || deleting.DeletionTimestamp.IsZero() {
		t.Fatalf("idle Delete-class workspace must be deleting (finalizer-held), got err=%v deleting=%v", err, deleting.DeletionTimestamp)
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

func TestACPWorkspaceRetentionStartsIdleClockAfterEnforcedEpochClears(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	revocationStartedAt := time.Now().Add(-time.Hour).UTC()
	detachedAt := revocationStartedAt.Format(time.RFC3339Nano)
	now := time.Now().UTC()
	workspace := retentionTestWorkspace(t, "acp-ws-retention-released-epoch", func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Spec.AttachmentEpoch = 3
		w.Status.AttachedEpoch = 0
		w.Annotations[acpWorkspaceRevocationStartedAnnotation] = fmt.Sprintf("3 %s", detachedAt)
		w.Annotations[acpWorkspaceLastDetachedAnnotation] = detachedAt
	})
	c := acpAdapterTestClient(t, workspace)
	reconciler := &ACPWorkspaceRetentionReconciler{
		Client: c,
		Now:    func() time.Time { return now },
	}
	result, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(workspace),
	})
	if err != nil {
		t.Fatalf("retention reconcile: %v", err)
	}
	if result.RequeueAfter != time.Second {
		t.Fatalf("released epoch requeue = %s, want 1s", result.RequeueAfter)
	}
	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), current); err != nil {
		t.Fatalf("workspace must survive epoch-release handoff: %v", err)
	}
	if !current.DeletionTimestamp.IsZero() {
		t.Fatal("workspace must not expire from the provisional revocation-start clock")
	}
	if got, want := current.Annotations[acpWorkspaceLastDetachedAnnotation], now.Format(time.RFC3339Nano); got != want {
		t.Fatalf("last-detached-at = %q, want epoch release instant %q", got, want)
	}
	if got := current.Annotations[acpWorkspaceRevocationStartedAnnotation]; got != fmt.Sprintf("3 %s", detachedAt) {
		t.Fatalf("revocation marker = %q, want it retained for Task settlement", got)
	}
}

func TestACPWorkspaceRetentionFailsClosedOnMalformedRevocationStamp(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		stamp string
	}{
		{name: emptyCaseName, stamp: ""},
		{name: "malformed", stamp: "not-an-epoch-and-time"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			detachedAt := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
			workspace := retentionTestWorkspace(t, "acp-ws-retention-bad-revocation-"+test.name, func(w *workspacev1alpha1.ExecutionWorkspace) {
				w.Spec.AttachmentEpoch = 3
				w.Annotations[acpWorkspaceRevocationStartedAnnotation] = test.stamp
				w.Annotations[acpWorkspaceLastDetachedAnnotation] = detachedAt
			})
			c := acpAdapterTestClient(t, workspace)
			result := reconcileRetention(t, c, workspace)
			if result.RequeueAfter <= 0 {
				t.Fatalf("malformed revocation stamp must hold on a bounded requeue, got %+v", result)
			}
			current := &workspacev1alpha1.ExecutionWorkspace{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), current); err != nil {
				t.Fatalf("workspace must survive malformed revocation metadata: %v", err)
			}
			if !current.DeletionTimestamp.IsZero() || current.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady {
				t.Fatalf("malformed revocation metadata changed workspace: deleting=%v desired=%s",
					current.DeletionTimestamp, current.Spec.DesiredState)
			}
			if got := current.Annotations[acpWorkspaceLastDetachedAnnotation]; got != detachedAt {
				t.Fatalf("last-detached-at = %q, want protected stamp %q unchanged", got, detachedAt)
			}
		})
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
	deleting := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: idle.Namespace, Name: idle.Name}, deleting); err != nil || deleting.DeletionTimestamp.IsZero() {
		t.Fatalf("idle suspension over an exhausted cap must apply the Delete disposition, got err=%v deleting=%v", err, deleting.DeletionTimestamp)
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

func TestACPWorkspaceRetentionPreservesDetachInstantForDeadColdResume(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	detachedAt := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	workspace := retentionTestWorkspace(t, "acp-ws-retention-dead-resume-clock", func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Spec.Lifecycle.DefaultOnDetach = workspacev1alpha1.WorkspaceOnDetachSuspend
		w.Spec.Lifecycle.AllowedOnDetach = []workspacev1alpha1.WorkspaceOnDetach{
			workspacev1alpha1.WorkspaceOnDetachSuspend, workspacev1alpha1.WorkspaceOnDetachDelete,
		}
		w.Annotations[acpWorkspaceDetachActionAnnotation] = string(workspacev1alpha1.WorkspaceOnDetachSuspend)
		w.Annotations[acpWorkspaceDurableAnnotation] = booleanTrueValue
		w.Annotations[acpWorkspaceResumedLineageAnnotation] = booleanTrueValue
		w.Annotations[acpWorkspaceResumeRequestedAnnotation] = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano) +
			" vanished-continuation vanished-continuation-uid"
		w.Annotations[acpWorkspaceLastDetachedAnnotation] = detachedAt
	})
	c := acpAdapterTestClient(t, workspace)
	reconcileRetention(t, c, workspace)
	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), current); err != nil {
		t.Fatalf("read re-suspended workspace: %v", err)
	}
	if current.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredSuspended {
		t.Fatalf("desired state = %s, want Suspended", current.Spec.DesiredState)
	}
	if got := current.Annotations[acpWorkspaceLastDetachedAnnotation]; got != detachedAt {
		t.Fatalf("dead cold resume changed last-detached-at to %q, want %q", got, detachedAt)
	}
}

func TestACPSuspendQuotaLockRetiresIdleEntry(t *testing.T) {
	t.Parallel()
	namespace := "quota-lock-retire"
	classUID := types.UID("quota-lock-retire-uid")
	key := namespace + "/" + string(classUID)

	unlock := lockACPSuspendQuota(namespace, classUID)
	acpSuspendQuotaLocksMu.Lock()
	entry, presentWhileHeld := acpSuspendQuotaLocks[key]
	referencesWhileHeld := 0
	if entry != nil {
		referencesWhileHeld = entry.references
	}
	acpSuspendQuotaLocksMu.Unlock()
	unlock()

	if !presentWhileHeld || referencesWhileHeld != 1 {
		t.Fatalf("held lock entry = (present=%v references=%d), want one live reference", presentWhileHeld, referencesWhileHeld)
	}
	acpSuspendQuotaLocksMu.Lock()
	_, presentAfterRelease := acpSuspendQuotaLocks[key]
	acpSuspendQuotaLocksMu.Unlock()
	if presentAfterRelease {
		t.Fatal("idle quota lock entry survived its final release")
	}
}

func TestACPSuspendQuotaLeaseSerializesClaimsAcrossLeaders(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	classUID := types.UID("cross-leader-quota-class-uid")
	shape := func(name string) *workspacev1alpha1.ExecutionWorkspace {
		return retentionTestWorkspace(t, name, func(workspace *workspacev1alpha1.ExecutionWorkspace) {
			workspace.Spec.ClassBinding.UID = classUID
			workspace.Annotations[acpWorkspaceMaxSuspendedAnnotation] = "1"
		})
	}
	first := shape("acp-ws-quota-first")
	second := shape("acp-ws-quota-second")
	c := acpAdapterTestClient(t, first, second)
	if err := c.Get(ctx, client.ObjectKeyFromObject(first), first); err != nil {
		t.Fatalf("read first claimant: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(second), second); err != nil {
		t.Fatalf("read second claimant: %v", err)
	}

	if err := claimACPSuspendQuotaSlot(ctx, c, c, first, 1); err != nil {
		t.Fatalf("record first pending claim: %v", err)
	}
	if err := suspendACPWorkspaceWithinQuota(ctx, c, c, second, time.Now(), false); !errors.Is(err, errACPSuspendQuotaExhausted) {
		t.Fatalf("second claim while the first is pending = %v, want quota exhaustion", err)
	}
	currentSecond := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(second), currentSecond); err != nil {
		t.Fatalf("read blocked claimant: %v", err)
	}
	if currentSecond.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady {
		t.Fatalf("blocked claimant desired state = %s, want Ready", currentSecond.Spec.DesiredState)
	}

	if err := suspendACPWorkspaceWithinQuota(ctx, c, c, first, time.Now(), false); err != nil {
		t.Fatalf("recover first claim: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(second), currentSecond); err != nil {
		t.Fatalf("refresh second claimant: %v", err)
	}
	if err := suspendACPWorkspaceWithinQuota(ctx, c, c, currentSecond, time.Now(), false); !errors.Is(err, errACPSuspendQuotaExhausted) {
		t.Fatalf("second claim after the first committed = %v, want quota exhaustion", err)
	}
	lease := &coordinationv1.Lease{}
	err := c.Get(ctx, types.NamespacedName{
		Namespace: first.Namespace,
		Name:      acpSuspendQuotaLeaseName(classUID),
	}, lease)
	if err != nil {
		t.Fatalf("read persistent quota Lease: %v", err)
	}
	claims, err := readACPSuspendQuotaClaims(lease, first.Spec.ClassBinding.Name, classUID)
	if err != nil {
		t.Fatalf("read persistent quota claims: %v", err)
	}
	if len(claims) != 0 {
		t.Fatalf("persistent claims = %#v, want settled occupancy counted only from live workspaces", claims)
	}
}

func TestACPSuspendQuotaLeaseStoresOnlyOnePendingClaim(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	classUID := types.UID("single-pending-quota-class-uid")
	shape := func(name string) *workspacev1alpha1.ExecutionWorkspace {
		return retentionTestWorkspace(t, name, func(workspace *workspacev1alpha1.ExecutionWorkspace) {
			workspace.Spec.ClassBinding.UID = classUID
			workspace.Annotations[acpWorkspaceMaxSuspendedAnnotation] = "2"
		})
	}
	first := shape("acp-ws-pending-first")
	second := shape("acp-ws-pending-second")
	c := acpAdapterTestClient(t, first, second)
	if err := c.Get(ctx, client.ObjectKeyFromObject(first), first); err != nil {
		t.Fatalf("read first claimant: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(second), second); err != nil {
		t.Fatalf("read second claimant: %v", err)
	}
	if err := claimACPSuspendQuotaSlot(ctx, c, c, first, 2); err != nil {
		t.Fatalf("record first pending claim: %v", err)
	}
	if err := claimACPSuspendQuotaSlot(ctx, c, c, second, 2); !errors.Is(err, errACPSuspendQuotaBusy) {
		t.Fatalf("second pending claim with quota headroom = %v, want serialized retry", err)
	}
	lease := &coordinationv1.Lease{}
	if err := c.Get(ctx, types.NamespacedName{
		Namespace: first.Namespace,
		Name:      acpSuspendQuotaLeaseName(classUID),
	}, lease); err != nil {
		t.Fatalf("read quota Lease: %v", err)
	}
	claims, err := readACPSuspendQuotaClaims(lease, first.Spec.ClassBinding.Name, classUID)
	if err != nil {
		t.Fatalf("read quota claims: %v", err)
	}
	if len(claims) != 1 {
		t.Fatalf("pending claims = %#v, want one constant-size claim", claims)
	}
	if _, ok := claims[string(first.UID)]; !ok {
		t.Fatalf("pending claims = %#v, want only the first transaction", claims)
	}
}

func TestACPSuspendQuotaLeasePrunesFencedPendingClaim(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	classUID := types.UID("stale-quota-class-uid")
	shape := func(name string) *workspacev1alpha1.ExecutionWorkspace {
		return retentionTestWorkspace(t, name, func(workspace *workspacev1alpha1.ExecutionWorkspace) {
			workspace.Spec.ClassBinding.UID = classUID
			workspace.Annotations[acpWorkspaceMaxSuspendedAnnotation] = "1"
		})
	}
	stale := shape("acp-ws-quota-stale")
	replacement := shape("acp-ws-quota-replacement")
	c := acpAdapterTestClient(t, stale, replacement)
	if err := c.Get(ctx, client.ObjectKeyFromObject(stale), stale); err != nil {
		t.Fatalf("read stale claimant: %v", err)
	}
	if err := claimACPSuspendQuotaSlot(ctx, c, c, stale, 1); err != nil {
		t.Fatalf("record stale pending claim: %v", err)
	}
	base := stale.DeepCopy()
	stale.Annotations["test.orka.ai/fence"] = "advanced"
	if err := c.Patch(ctx, stale, client.MergeFrom(base)); err != nil {
		t.Fatalf("advance stale claimant resourceVersion: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(replacement), replacement); err != nil {
		t.Fatalf("read replacement claimant: %v", err)
	}
	if err := suspendACPWorkspaceWithinQuota(ctx, c, c, replacement, time.Now(), false); err != nil {
		t.Fatalf("claim after the old workspace fence advanced: %v", err)
	}
	lease := &coordinationv1.Lease{}
	if err := c.Get(ctx, types.NamespacedName{
		Namespace: replacement.Namespace,
		Name:      acpSuspendQuotaLeaseName(classUID),
	}, lease); err != nil {
		t.Fatalf("read quota Lease: %v", err)
	}
	claims, err := readACPSuspendQuotaClaims(lease, replacement.Spec.ClassBinding.Name, classUID)
	if err != nil {
		t.Fatalf("read quota claims: %v", err)
	}
	if _, ok := claims[string(stale.UID)]; ok {
		t.Fatalf("stale pending claim survived its workspace fence: %#v", claims)
	}
	if _, ok := claims[string(replacement.UID)]; !ok {
		t.Fatalf("replacement claim missing after stale-claim recovery: %#v", claims)
	}
}

func TestSettleACPClassWorkspacePreservesDetachClockBeforeAttachment(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := suspendableSubstrateFixture(t)
	workspace := acpAdapterWorkspace(t, "")
	workspace.Name = "acp-ws-pre-attachment-suspend"
	workspace.UID = types.UID("acp-ws-pre-attachment-suspend-uid")
	workspace.Spec.SessionRef = &workspacev1alpha1.ObjectIdentityReference{
		Name: acpTestSessionName, UID: types.UID(suspendTestSessionUID),
	}
	workspace.Spec.Lifecycle.AllowedOnDetach = append(
		workspace.Spec.Lifecycle.AllowedOnDetach,
		workspacev1alpha1.WorkspaceOnDetachSuspend,
	)
	detachedAt := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	workspace.Annotations[acpWorkspaceDetachActionAnnotation] = string(workspacev1alpha1.WorkspaceOnDetachSuspend)
	workspace.Annotations[acpWorkspaceDurableAnnotation] = booleanTrueValue
	workspace.Annotations[acpWorkspaceDurableSessionCommittedAnnotation] = "1"
	workspace.Annotations[acpWorkspaceMaxSuspendedAnnotation] = "1"
	workspace.Annotations[acpWorkspaceLastDetachedAnnotation] = detachedAt
	task := retentionSettlementTask(
		"pre-attachment-suspend-task",
		"pre-attachment-suspend-task-uid",
		workspace,
		workspacev1alpha1.WorkspaceOnDetachSuspend,
	)
	r := acpClassTestReconciler(t, append(fixture.objects(), workspace, task)...)
	task = bindSuspendableSessionTaskForSettlement(t, r, task)
	if !taskNeverHeldACPWorkspaceAttachment(task) {
		t.Fatal("fixture unexpectedly records a workspace attachment")
	}

	done, err := r.settleACPClassWorkspace(ctx, task)
	if err != nil || !done {
		t.Fatalf("pre-attachment settlement = (%v, %v), want a completed suspension", done, err)
	}
	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(workspace), current); err != nil {
		t.Fatalf("read suspended workspace: %v", err)
	}
	if current.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredSuspended {
		t.Fatalf("desired state = %s, want Suspended", current.Spec.DesiredState)
	}
	if got := current.Annotations[acpWorkspaceLastDetachedAnnotation]; got != detachedAt {
		t.Fatalf("pre-attachment settlement changed last-detached-at to %q, want %q", got, detachedAt)
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

// A live cold-resume requester is active demand even while the observed state
// remains Suspended; idle retention must wait for that exact Task.
func TestACPWorkspaceRetentionWaitsForColdResumeInFlight(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	requester := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
		Namespace: acpTestNamespace, Name: "cold-resume-requester", UID: types.UID("cold-resume-requester-uid"),
	}}
	workspace := retentionTestWorkspace(t, "acp-ws-retention-l", func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Spec.Lifecycle.DefaultOnDetach = workspacev1alpha1.WorkspaceOnDetachSuspend
		w.Annotations[acpWorkspaceDetachActionAnnotation] = string(workspacev1alpha1.WorkspaceOnDetachSuspend)
		w.Annotations[acpWorkspaceLastDetachedAnnotation] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
		w.Annotations[acpWorkspaceResumeRequestedAnnotation] = time.Now().UTC().Format(time.RFC3339Nano) +
			" cold-resume-requester cold-resume-requester-uid"
		w.Status.State = workspacev1alpha1.ExecutionWorkspaceStateSuspended
	})
	c := acpAdapterTestClient(t, workspace, requester)
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

// A dead cold-resume requester must not turn stale observed suspension into
// permanent demand. With no maxLifetime, idleTimeout still reclaims the
// workspace from either provider transition state.
func TestACPWorkspaceRetentionExpiresDeadColdResumeState(t *testing.T) {
	t.Parallel()
	for _, state := range []workspacev1alpha1.ExecutionWorkspaceState{
		workspacev1alpha1.ExecutionWorkspaceStateSuspended,
		workspacev1alpha1.ExecutionWorkspaceStateSuspending,
	} {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			workspace := retentionTestWorkspace(t, "acp-ws-dead-resume-"+strings.ToLower(string(state)), func(w *workspacev1alpha1.ExecutionWorkspace) {
				w.Spec.Lifecycle.MaxLifetime = nil
				w.Annotations[acpWorkspaceLastDetachedAnnotation] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
				w.Annotations[acpWorkspaceResumeRequestedAnnotation] = time.Now().UTC().Format(time.RFC3339Nano) +
					" vanished-resume-requester vanished-resume-requester-uid"
				w.Status.State = state
			})
			c := acpAdapterTestClient(t, workspace)
			reconcileRetention(t, c, workspace)
			deleting := &workspacev1alpha1.ExecutionWorkspace{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), deleting); err != nil || deleting.DeletionTimestamp.IsZero() {
				t.Fatalf("dead resume in %s must idle out, got err=%v deleting=%v", state, err, deleting.DeletionTimestamp)
			}
		})
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
	deletingQuarantined := &workspacev1alpha1.ExecutionWorkspace{}
	if getErr := qc.Get(ctx, types.NamespacedName{Namespace: quarantined.Namespace, Name: quarantined.Name}, deletingQuarantined); getErr != nil || deletingQuarantined.DeletionTimestamp.IsZero() {
		t.Fatalf("quarantined workspace past maxLifetime must be deleting, got err=%v deleting=%v", getErr, deletingQuarantined.DeletionTimestamp)
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

func TestResolveACPWorkspaceClassRejectsUnboundedSuspendableClass(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := suspendableSubstrateFixture(t)
	fixture.class.Spec.Lifecycle.IdleTimeout = nil
	fixture.class.Spec.Lifecycle.MaxLifetime = nil
	fixture.profile.Spec.Retention = nil
	fixture.pinProfileHash(t)
	task := suspendableSessionTask()
	r := acpClassTestReconciler(t, append(fixture.objects(), task)...)
	if _, err := r.resolveACPWorkspaceClass(ctx, task); err == nil ||
		!strings.Contains(err.Error(), "requires at least one retention bound") {
		t.Fatalf("unbounded suspend-capable class error = %v, want a retention-bound rejection", err)
	}
}

func TestValidateACPWorkspaceClassBindingAllowsLegacyUnboundedRetention(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := suspendableSubstrateFixture(t)
	task := suspendableSessionTask()
	r := acpClassTestReconciler(t, fixture.objects()...)
	resolved, err := r.resolveACPWorkspaceClass(ctx, task)
	if err != nil {
		t.Fatalf("resolve bounded class: %v", err)
	}
	binding, err := resolveACPWorkspaceBindingWithClass(task, "", false, suspendTestSessionUID, resolved)
	if err != nil {
		t.Fatalf("resolve bounded workspace binding: %v", err)
	}
	legacy := *binding.Class
	legacy.IdleTimeout = ""
	legacy.MaxLifetime = ""
	legacy.MaxSuspendedWorkspaces = nil
	if err := validateACPWorkspaceClassBindingValues(&legacy); err != nil {
		t.Fatalf("legacy unbounded frozen binding must remain executable after upgrade: %v", err)
	}
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
	task = bindSuspendableSessionTaskForSettlement(t, r, task)
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
	if _, ready := attachTestACPWorkspace(t, r, task, plan, workspace.Name); !ready {
		t.Fatalf("attach = (%v)", ready)
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
	// returns not-done so the next reconcile re-reads uncached state, and
	// finalization waits for the adapter to release the enforced epoch.
	done := false
	for attempt := 0; attempt < 8 && !done; attempt++ {
		var settleErr error
		if done, settleErr = r.settleACPClassWorkspace(ctx, task); settleErr != nil {
			t.Fatalf("settle attempt %d: %v", attempt, settleErr)
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
	err = r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: name}, workspace)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("quota-exhausted settlement must apply the Delete disposition, got %v", err)
	}
}

// Pending demand whose requesting Task disappeared (or settled terminally)
// must not hold the workspace forever; live demand still defers idle expiry.
func TestACPWorkspaceRetentionRetiresDeadPendingDemand(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Dead requester: the named Task does not exist, so the creation-stamped
	// demand no longer blocks idle handling and the Delete-class workspace
	// enters terminal cleanup.
	dead := retentionTestWorkspace(t, "acp-ws-demand-dead", func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Annotations[acpWorkspaceResumeRequestedAnnotation] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano) + " vanished-task"
		w.Annotations[acpWorkspaceLastDetachedAnnotation] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	})
	c := acpAdapterTestClient(t, dead)
	reconcileRetention(t, c, dead)
	deleting := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: dead.Namespace, Name: dead.Name}, deleting); err != nil || deleting.DeletionTimestamp.IsZero() {
		t.Fatalf("dead-demand workspace must idle out, got err=%v deleting=%v", err, deleting.DeletionTimestamp)
	}

	// Live requester: demand defers idle expiry.
	requester := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
		Namespace: acpTestNamespace, Name: "live-requester", UID: types.UID("live-requester-uid"),
	}}
	live := retentionTestWorkspace(t, "acp-ws-demand-live", func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Annotations[acpWorkspaceResumeRequestedAnnotation] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano) + " live-requester live-requester-uid"
		w.Annotations[acpWorkspaceLastDetachedAnnotation] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	})
	c = acpAdapterTestClient(t, live, requester)
	reconcileRetention(t, c, live)
	kept := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: live.Namespace, Name: live.Name}, kept); err != nil || !kept.DeletionTimestamp.IsZero() {
		t.Fatalf("live demand must defer idle expiry, got err=%v deleting=%v", err, kept.DeletionTimestamp)
	}

	// A replacement Task recycled under the requester's namespace/name is
	// not the requester: the UID mismatch retires the stale demand.
	replacement := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
		Namespace: acpTestNamespace, Name: "recycled-requester", UID: types.UID("replacement-uid"),
	}}
	stale := retentionTestWorkspace(t, "acp-ws-demand-recycled", func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Annotations[acpWorkspaceResumeRequestedAnnotation] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano) + " recycled-requester original-uid"
		w.Annotations[acpWorkspaceLastDetachedAnnotation] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	})
	c = acpAdapterTestClient(t, stale, replacement)
	reconcileRetention(t, c, stale)
	recycled := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: stale.Namespace, Name: stale.Name}, recycled); err != nil || recycled.DeletionTimestamp.IsZero() {
		t.Fatalf("UID-mismatched demand must idle out, got err=%v deleting=%v", err, recycled.DeletionTimestamp)
	}
}

func TestACPWorkspaceRetentionFailsClosedOnEmptyPendingDemand(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		stamp string
	}{
		{name: emptyCaseName, stamp: ""},
		{name: whitespaceCaseName, stamp: whitespaceOnlyValue},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			workspace := retentionTestWorkspace(t, "acp-ws-demand-empty-"+test.name, func(w *workspacev1alpha1.ExecutionWorkspace) {
				w.Annotations[acpWorkspaceResumeRequestedAnnotation] = test.stamp
				w.Annotations[acpWorkspaceLastDetachedAnnotation] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
			})
			c := acpAdapterTestClient(t, workspace)
			result := reconcileRetention(t, c, workspace)
			if result.RequeueAfter <= 0 {
				t.Fatalf("an empty pending-demand stamp must hold on a bounded requeue, got %+v", result)
			}
			current := &workspacev1alpha1.ExecutionWorkspace{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), current); err != nil {
				t.Fatalf("workspace must survive an empty pending-demand stamp: %v", err)
			}
			if !current.DeletionTimestamp.IsZero() {
				t.Fatal("an empty pending-demand stamp must not trigger idle deletion")
			}
		})
	}
}

// The single UID-bound demand stamp records only the LAST writer: when the
// recorded requester settles terminally while another live continuation on
// the same Session still waits, retention must keep the workspace instead of
// expiring it out from under the surviving waiter.
func TestACPWorkspaceRetentionHonorsSurvivingSessionContinuations(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	terminalRequester := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: acpTestNamespace, Name: "lc-writer", UID: types.UID("lc-writer-uid"),
		},
		Spec: corev1alpha1.TaskSpec{
			SessionRef: &corev1alpha1.SessionReference{Name: acpTestSessionName},
		},
	}
	terminalRequester.Status.Phase = corev1alpha1.TaskPhaseFailed
	waiter := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: acpTestNamespace, Name: "lc-waiter", UID: types.UID("lc-waiter-uid"),
			// The waiter was reconciled at least once and carries the
			// controller-written link to this exact workspace incarnation.
			Labels:      map[string]string{acpExecutionWorkspaceLinkLabel: "acp-ws-demand-survivor"},
			Annotations: map[string]string{acpExecutionWorkspaceUIDAnnotation: "acp-ws-demand-survivor-uid"},
		},
		Spec: corev1alpha1.TaskSpec{
			SessionRef: &corev1alpha1.SessionReference{Name: acpTestSessionName},
		},
	}
	workspace := retentionTestWorkspace(t, "acp-ws-demand-survivor", func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Spec.SessionRef = &workspacev1alpha1.ObjectIdentityReference{
			Name: acpTestSessionName, UID: types.UID("session-uid-1"),
		}
		w.Annotations[acpWorkspaceResumeRequestedAnnotation] =
			time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano) + " lc-writer lc-writer-uid"
		w.Annotations[acpWorkspaceLastDetachedAnnotation] =
			time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	})
	c := acpAdapterTestClient(t, workspace, terminalRequester, waiter)
	reconcileRetention(t, c, workspace)
	kept := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, kept); err != nil ||
		!kept.DeletionTimestamp.IsZero() {
		t.Fatalf("a live Session continuation must keep the workspace, got err=%v deleting=%v", err, kept.DeletionTimestamp)
	}

	// Once the surviving waiter settles terminally too, no demand remains and
	// idle expiry proceeds.
	currentWaiter := &corev1alpha1.Task{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: waiter.Namespace, Name: waiter.Name}, currentWaiter); err != nil {
		t.Fatalf("read waiter: %v", err)
	}
	currentWaiter.Status.Phase = corev1alpha1.TaskPhaseCancelled
	if err := c.Update(ctx, currentWaiter); err != nil {
		t.Fatalf("settle waiter: %v", err)
	}
	reconcileRetention(t, c, workspace)
	expired := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, expired); err != nil ||
		expired.DeletionTimestamp.IsZero() {
		t.Fatalf("with no live continuation the workspace must idle out, got err=%v deleting=%v", err, expired.DeletionTimestamp)
	}
}

// A terminally failed durable suspension keeps its quota slot. Present but
// invalid protected markers also fail closed as potentially durable, while an
// absent marker or an explicit durable-data-absent proof frees the slot.
func TestCountSuspendedClassWorkspacesChargesDurableFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	classUID := types.UID("count-class-uid")
	shape := func(name string, mutate func(*workspacev1alpha1.ExecutionWorkspace)) *workspacev1alpha1.ExecutionWorkspace {
		w := acpAdapterWorkspace(t, "")
		w.Name = name
		w.UID = types.UID(name + "-uid")
		w.Spec.ClassBinding.UID = classUID
		w.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
		mutate(w)
		return w
	}
	durableFailed := shape("acp-ws-durable-failed", func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Annotations[acpWorkspaceDurableAnnotation] = booleanTrueValue
		w.Status.State = workspacev1alpha1.ExecutionWorkspaceStateFailed
	})
	emptyMarkerFailed := shape("acp-ws-empty-marker-failed", func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Annotations[acpWorkspaceDurableAnnotation] = ""
		w.Status.State = workspacev1alpha1.ExecutionWorkspaceStateFailed
	})
	whitespaceMarkerFailed := shape("acp-ws-whitespace-marker-failed", func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Annotations[acpWorkspaceDurableAnnotation] = whitespaceOnlyValue
		w.Status.State = workspacev1alpha1.ExecutionWorkspaceStateFailed
	})
	invalidMarkerFailed := shape("acp-ws-invalid-marker-failed", func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Annotations[acpWorkspaceDurableAnnotation] = "not-a-boolean"
		w.Status.State = workspacev1alpha1.ExecutionWorkspaceStateFailed
	})
	provenEmptyFailed := shape("acp-ws-proven-empty-failed", func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Annotations[acpWorkspaceDurableAnnotation] = whitespaceOnlyValue
		w.Annotations[acpWorkspaceDurableDataAbsentAnnotation] = booleanTrueValue
		w.Status.State = workspacev1alpha1.ExecutionWorkspaceStateFailed
	})
	plainFailed := shape("acp-ws-plain-failed", func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Status.State = workspacev1alpha1.ExecutionWorkspaceStateFailed
	})
	suspended := shape("acp-ws-clean-suspended", func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Status.State = workspacev1alpha1.ExecutionWorkspaceStateSuspended
	})
	c := acpAdapterTestClient(t, durableFailed, emptyMarkerFailed, whitespaceMarkerFailed, invalidMarkerFailed,
		provenEmptyFailed, plainFailed, suspended)
	count, err := countSuspendedClassWorkspaces(ctx, c, acpTestNamespace, classUID, nil)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 5 {
		t.Fatalf("count = %d, want durable and malformed failures plus the suspended workspace charged", count)
	}
}

// A cold resume lifts DesiredState to Ready before the preserved runtime is
// serving. Its prior Suspended/Suspending state must keep consuming capacity
// until the adapter projects Ready or Attached, or proves the data absent.
func TestCountSuspendedClassWorkspacesChargesColdResumesInFlight(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	classUID := types.UID("cold-resume-class-uid")
	shape := func(name string, state workspacev1alpha1.ExecutionWorkspaceState, dataAbsent bool) *workspacev1alpha1.ExecutionWorkspace {
		w := acpAdapterWorkspace(t, "")
		w.Name = name
		w.UID = types.UID(name + "-uid")
		w.Spec.ClassBinding.UID = classUID
		w.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredReady
		w.Annotations[acpWorkspaceDurableAnnotation] = booleanTrueValue
		if dataAbsent {
			w.Annotations[acpWorkspaceDurableDataAbsentAnnotation] = booleanTrueValue
		}
		w.Status.State = state
		return w
	}
	objects := []client.Object{
		shape("acp-ws-resume-suspended", workspacev1alpha1.ExecutionWorkspaceStateSuspended, false),
		shape("acp-ws-resume-suspending", workspacev1alpha1.ExecutionWorkspaceStateSuspending, false),
		shape("acp-ws-resume-ready", workspacev1alpha1.ExecutionWorkspaceStateReady, false),
		shape("acp-ws-resume-attached", workspacev1alpha1.ExecutionWorkspaceStateAttached, false),
		shape("acp-ws-resume-proven-empty", workspacev1alpha1.ExecutionWorkspaceStateSuspended, true),
	}
	c := acpAdapterTestClient(t, objects...)
	count, err := countSuspendedClassWorkspaces(ctx, c, acpTestNamespace, classUID, nil)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Fatalf("count = %d, want only the two in-flight cold resumes charged", count)
	}
}

func TestCountSuspendedClassWorkspacesIgnoresForeignWorkspaces(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	classUID := types.UID("owned-count-class-uid")
	shape := func(name string) *workspacev1alpha1.ExecutionWorkspace {
		workspace := acpAdapterWorkspace(t, "")
		workspace.Name = name
		workspace.UID = types.UID(name + "-uid")
		workspace.Spec.ClassBinding.UID = classUID
		workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
		workspace.Status.State = workspacev1alpha1.ExecutionWorkspaceStateSuspended
		return workspace
	}
	owned := shape("acp-ws-owned-quota")
	foreign := shape("foreign-ws-copied-class")
	delete(foreign.Labels, workspacev1alpha1.ProviderControllerLabel)
	c := acpAdapterTestClient(t, owned, foreign)
	count, err := countSuspendedClassWorkspaces(ctx, c, acpTestNamespace, classUID, nil)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want only the ACP-owned workspace charged", count)
	}
}

func TestACPWorkspaceSuspendedCapFailsClosedOnEmptyAnnotation(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"", whitespaceOnlyValue} {
		workspace := acpAdapterWorkspace(t, "")
		workspace.Annotations[acpWorkspaceMaxSuspendedAnnotation] = value
		cap := acpWorkspaceSuspendedCapFromAnnotation(workspace)
		if cap == nil || *cap != 0 {
			t.Fatalf("cap for present value %q = %v, want exhausted zero cap", value, cap)
		}
	}

	workspace := acpAdapterWorkspace(t, "")
	if cap := acpWorkspaceSuspendedCapFromAnnotation(workspace); cap != nil {
		t.Fatalf("cap without an annotation = %d, want unbounded nil", *cap)
	}
}

// Demand binds to the workspace incarnation: a waiter already linked to a
// DIFFERENT workspace (for example a recreated Session's fresh incarnation)
// never counts as demand for this one.
func TestLiveSessionContinuationRequiresExactIncarnation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	workspace := retentionTestWorkspace(t, "acp-ws-incarnation", func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Spec.SessionRef = &workspacev1alpha1.ObjectIdentityReference{
			Name: acpTestSessionName, UID: types.UID("session-uid-old"),
		}
	})
	foreignLinked := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: acpTestNamespace, Name: "lc-foreign", UID: types.UID("lc-foreign-uid"),
			Labels:      map[string]string{acpExecutionWorkspaceLinkLabel: "acp-ws-other"},
			Annotations: map[string]string{acpExecutionWorkspaceUIDAnnotation: "other-uid"},
		},
		Spec: corev1alpha1.TaskSpec{SessionRef: &corev1alpha1.SessionReference{Name: acpTestSessionName}},
	}
	c := acpAdapterTestClient(t, workspace, foreignLinked)
	live, err := liveACPSessionContinuationExists(ctx, c, workspace)
	if err != nil {
		t.Fatalf("continuation check: %v", err)
	}
	if live {
		t.Fatal("a waiter linked to a different workspace incarnation must not count as demand")
	}

	exact := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: acpTestNamespace, Name: "lc-exact", UID: types.UID("lc-exact-uid"),
			Labels:      map[string]string{acpExecutionWorkspaceLinkLabel: workspace.Name},
			Annotations: map[string]string{acpExecutionWorkspaceUIDAnnotation: string(workspace.UID)},
		},
		Spec: corev1alpha1.TaskSpec{SessionRef: &corev1alpha1.SessionReference{Name: acpTestSessionName}},
	}
	if err := c.Create(ctx, exact); err != nil {
		t.Fatalf("create exact waiter: %v", err)
	}
	live, err = liveACPSessionContinuationExists(ctx, c, workspace)
	if err != nil || !live {
		t.Fatalf("exact-incarnation waiter must count as demand, got (%v, %v)", live, err)
	}
}

type failingListReader struct {
	client.Reader
}

func (f *failingListReader) List(_ context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	return fmt.Errorf("simulated apiserver outage")
}

type conflictSuccessorLinkClient struct {
	client.Client
	target     types.NamespacedName
	conflicted bool
}

func (c *conflictSuccessorLinkClient) Patch(
	ctx context.Context,
	object client.Object,
	patch client.Patch,
	options ...client.PatchOption,
) error {
	if task, ok := object.(*corev1alpha1.Task); ok && !c.conflicted &&
		client.ObjectKeyFromObject(task) == c.target {
		c.conflicted = true
		return apierrors.NewConflict(
			schema.GroupResource{Group: corev1alpha1.GroupVersion.Group, Resource: "tasks"},
			task.Name,
			errors.New("simulated successor link conflict"),
		)
	}
	return c.Client.Patch(ctx, object, patch, options...)
}

type conflictWorkspaceDeleteClient struct {
	client.Client
	target     types.NamespacedName
	conflicted bool
}

func retentionSettlementTask(
	name string,
	uid string,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	action workspacev1alpha1.WorkspaceOnDetach,
) *corev1alpha1.Task {
	task := retentionContinuationTask(name, uid, action)
	task.Labels = map[string]string{acpExecutionWorkspaceLinkLabel: workspace.Name}
	task.Annotations = map[string]string{acpExecutionWorkspaceUIDAnnotation: string(workspace.UID)}
	return task
}

func retentionContinuationTask(
	name string,
	uid string,
	action workspacev1alpha1.WorkspaceOnDetach,
) *corev1alpha1.Task {
	task := suspendableSessionTask()
	task.Name = name
	task.UID = types.UID(uid)
	task.Spec.Execution.Workspace.OnDetach = corev1alpha1.WorkspaceOnDetachPolicy(action)
	return task
}

func (c *conflictWorkspaceDeleteClient) Delete(
	ctx context.Context,
	object client.Object,
	options ...client.DeleteOption,
) error {
	if workspace, ok := object.(*workspacev1alpha1.ExecutionWorkspace); ok && !c.conflicted &&
		client.ObjectKeyFromObject(workspace) == c.target {
		c.conflicted = true
		return apierrors.NewConflict(
			schema.GroupResource{Group: workspacev1alpha1.GroupVersion.Group, Resource: "executionworkspaces"},
			workspace.Name,
			errors.New("simulated workspace delete conflict"),
		)
	}
	return c.Client.Delete(ctx, object, options...)
}

// A transient quota-read failure must requeue the Task, never permanently
// reject it: only actual cap exhaustion and real validation failures are
// terminal.
func TestSuspendQuotaReadFailureIsTransient(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	limit := int32(1)
	task := acpClassTestTask()
	task.Spec.Execution.Workspace.OnDetach = corev1alpha1.WorkspaceOnDetachPolicy(workspacev1alpha1.WorkspaceOnDetachSuspend)
	r := acpClassTestReconciler(t, task)
	resolved := &acpResolvedWorkspaceClass{
		Binding:         ACPWorkspaceClassBinding{MaxSuspendedWorkspaces: &limit},
		DefaultOnDetach: workspacev1alpha1.WorkspaceOnDetachSuspend,
	}
	class := &workspacev1alpha1.ExecutionWorkspaceClass{ObjectMeta: metav1.ObjectMeta{Name: acpTestClassName, UID: "class-uid"}}
	err := r.enforceACPWorkspaceSuspendQuota(ctx, &failingListReader{}, task, class, resolved)
	if err == nil || !errors.Is(err, errACPWorkspacePlanningTransient) {
		t.Fatalf("quota-read outage must classify transient, got %v", err)
	}

	// The plan consumer requeues a transient plan instead of failing the Task.
	result, planErr := r.rejectPlannedAgentExecution(ctx, task,
		agentExecutionPlan{path: agentExecutionPathRejected, transientError: err})
	if planErr == nil || result.RequeueAfter != 0 {
		t.Fatalf("transient plan must return the error for backoff requeue, got (%v, %v)", result, planErr)
	}
	current := &corev1alpha1.Task{}
	if getErr := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, current); getErr != nil {
		t.Fatalf("reload task: %v", getErr)
	}
	if current.Status.Phase == corev1alpha1.TaskPhaseFailed {
		t.Fatal("a transient planning failure must never permanently fail the Task")
	}
}

// A requester that terminated before it ever executed against the workspace
// must not destroy the retained repository while another live continuation is
// queued on the same incarnation: settlement completes without the
// destructive action and the successor's own frozen action governs.
func TestSettleACPClassWorkspaceDefersDeleteToQueuedContinuation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := suspendableSubstrateFixture(t)
	workspace := acpAdapterWorkspace(t, "")
	workspace.Name = "acp-ws-deferred-delete"
	workspace.UID = types.UID("deferred-delete-uid")
	workspace.Spec.SessionRef = &workspacev1alpha1.ObjectIdentityReference{
		Name: acpTestSessionName, UID: types.UID("session-uid-1"),
	}
	// The waiter's Suspend override must be class-allowed, or it would be
	// rejected as a non-successor by the AllowedOnDetach validation.
	workspace.Spec.Lifecycle.AllowedOnDetach = append(workspace.Spec.Lifecycle.AllowedOnDetach,
		workspacev1alpha1.WorkspaceOnDetachSuspend)
	workspace.Annotations[acpWorkspaceDetachActionAnnotation] = string(workspacev1alpha1.WorkspaceOnDetachDelete)
	dead := retentionSettlementTask(
		"lc-dead-requester", "lc-dead-uid", workspace,
		workspacev1alpha1.WorkspaceOnDetachDelete,
	)
	waiter := retentionSettlementTask(
		"lc-live-waiter", "lc-waiter-uid", workspace,
		workspacev1alpha1.WorkspaceOnDetachSuspend,
	)
	r := acpClassTestReconciler(t, append(fixture.objects(), workspace, dead, waiter)...)
	dead = bindSuspendableSessionTaskForSettlement(t, r, dead)
	waiter = bindSuspendableSessionTaskForSettlement(t, r, waiter)

	done, err := r.settleACPClassWorkspace(ctx, dead)
	if err != nil || !done {
		t.Fatalf("settle with a queued continuation = (%v, %v), want done without destruction", done, err)
	}
	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, current); err != nil {
		t.Fatalf("the workspace must survive for the queued continuation: %v", err)
	}
	// The deferral transfers the successor's policy: the dead requester's
	// Delete must not survive to destroy the retained workspace if the
	// successor also terminates before attaching. The waiter's explicit
	// Suspend override governs from here.
	if got := current.Annotations[acpWorkspaceDetachActionAnnotation]; got != string(workspacev1alpha1.WorkspaceOnDetachSuspend) {
		t.Fatalf("deferred detach action = %q, want the successor's Suspend override", got)
	}
	// Restore Delete for the final-phase assertion below (the successor
	// policy path is exercised above; the remaining check proves the
	// destructive settle still runs once no continuation is left).
	base := current.DeepCopy()
	current.Annotations[acpWorkspaceDetachActionAnnotation] = string(workspacev1alpha1.WorkspaceOnDetachDelete)
	if err := r.Patch(ctx, current, client.MergeFrom(base)); err != nil {
		t.Fatalf("restore Delete action: %v", err)
	}

	// With the last continuation gone, the stored Delete executes normally.
	if err := r.Delete(ctx, waiter); err != nil {
		t.Fatalf("remove waiter: %v", err)
	}
	done, err = r.settleACPClassWorkspace(ctx, dead)
	if err != nil || !done {
		t.Fatalf("settle without continuations = (%v, %v), want destructive completion", done, err)
	}
	err = r.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, current)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("with no live continuation the frozen Delete must execute, got %v", err)
	}
}

// A terminally failed suspension that PROVED no durable data exists (the
// adapter's proven-empty marker) frees its quota slot even on a durable
// class; resume-loss failures that preserve a claim stay charged.
func TestCountSuspendedClassWorkspacesFreesProvenEmptyFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	classUID := types.UID("count-empty-class-uid")
	provenEmpty := acpAdapterWorkspace(t, "")
	provenEmpty.Name = "acp-ws-proven-empty"
	provenEmpty.UID = types.UID("acp-ws-proven-empty-uid")
	provenEmpty.Spec.ClassBinding.UID = classUID
	provenEmpty.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
	provenEmpty.Annotations[acpWorkspaceDurableAnnotation] = booleanTrueValue
	provenEmpty.Annotations[acpWorkspaceDurableDataAbsentAnnotation] = booleanTrueValue
	provenEmpty.Status.State = workspacev1alpha1.ExecutionWorkspaceStateFailed
	c := acpAdapterTestClient(t, provenEmpty)
	count, err := countSuspendedClassWorkspaces(ctx, c, acpTestNamespace, classUID, nil)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("count = %d, want a proven-empty failure to free its quota slot", count)
	}

	failedResume := acpAdapterWorkspace(t, "")
	failedResume.Name = "acp-ws-failed-resume"
	failedResume.UID = types.UID("acp-ws-failed-resume-uid")
	failedResume.Spec.ClassBinding.UID = classUID
	failedResume.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredReady
	failedResume.Annotations[acpWorkspaceDurableAnnotation] = booleanTrueValue
	failedResume.Status.State = workspacev1alpha1.ExecutionWorkspaceStateFailed
	if err := c.Create(ctx, failedResume); err != nil {
		t.Fatalf("create failed resume: %v", err)
	}
	count, err = countSuspendedClassWorkspaces(ctx, c, acpTestNamespace, classUID, nil)
	if err != nil {
		t.Fatalf("count failed resume: %v", err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want a failed durable resume charged against the quota", count)
	}
}

// A live Task that merely shares the Session name without requesting a
// session-reused execution workspace can never attach the workspace and must
// not count as demand.
func TestLiveSessionContinuationIgnoresNonWorkspaceTasks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	workspace := retentionTestWorkspace(t, "acp-ws-nonws-demand", func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Spec.SessionRef = &workspacev1alpha1.ObjectIdentityReference{
			Name: acpTestSessionName, UID: types.UID("session-uid-1"),
		}
	})
	transcriptOnly := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: acpTestNamespace, Name: "lc-transcript-only", UID: types.UID("lc-transcript-only-uid"),
		},
		Spec: corev1alpha1.TaskSpec{SessionRef: &corev1alpha1.SessionReference{Name: acpTestSessionName}},
	}
	c := acpAdapterTestClient(t, workspace, transcriptOnly)
	live, err := liveACPSessionContinuationExists(ctx, c, workspace)
	if err != nil {
		t.Fatalf("continuation check: %v", err)
	}
	if live {
		t.Fatal("a transcript-only session Task must never count as workspace demand")
	}
}

// An unlinked continuation counts as demand only when its classRef resolves
// to THIS workspace's class: a different class (or the legacy enabled path)
// deliberately produces a separate workspace incarnation and must not defer
// settlement or retention of this one.
func TestLiveSessionContinuationRequiresMatchingClass(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	workspace := retentionTestWorkspace(t, "acp-ws-class-demand", func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Spec.SessionRef = &workspacev1alpha1.ObjectIdentityReference{
			Name: acpTestSessionName, UID: types.UID("session-uid-1"),
		}
	})
	otherClass := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: acpTestNamespace, Name: "lc-other-class", UID: types.UID("lc-other-class-uid"),
		},
		Spec: corev1alpha1.TaskSpec{
			SessionRef: &corev1alpha1.SessionReference{Name: acpTestSessionName},
			Execution: &corev1alpha1.ExecutionSpec{Workspace: &corev1alpha1.ExecutionWorkspaceSpec{
				ClassRef:    &corev1alpha1.WorkspaceClassReference{Name: "some-other-class"},
				ReusePolicy: corev1alpha1.WorkspaceReusePolicySession,
			}},
		},
	}
	// The bound class exists at its frozen immutable identity: unlinked
	// demand additionally verifies the live class UID against the binding.
	boundClass := &workspacev1alpha1.ExecutionWorkspaceClass{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: acpTestNamespace, Name: workspace.Spec.ClassBinding.Name,
			UID: workspace.Spec.ClassBinding.UID,
		},
	}
	c := acpAdapterTestClient(t, workspace, otherClass, boundClass)
	live, err := liveACPSessionContinuationExists(ctx, c, workspace)
	if err != nil {
		t.Fatalf("continuation check: %v", err)
	}
	if live {
		t.Fatal("a different-class waiter must never count as demand for this workspace")
	}

	matching := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: acpTestNamespace, Name: "lc-matching-class", UID: types.UID("lc-matching-class-uid"),
		},
		Spec: corev1alpha1.TaskSpec{
			SessionRef: &corev1alpha1.SessionReference{Name: acpTestSessionName},
			Execution: &corev1alpha1.ExecutionSpec{Workspace: &corev1alpha1.ExecutionWorkspaceSpec{
				ClassRef:    &corev1alpha1.WorkspaceClassReference{Name: workspace.Spec.ClassBinding.Name},
				ReusePolicy: corev1alpha1.WorkspaceReusePolicySession,
			}},
		},
	}
	if err := c.Create(ctx, matching); err != nil {
		t.Fatalf("create matching waiter: %v", err)
	}
	live, err = liveACPSessionContinuationExists(ctx, c, workspace)
	if err != nil || !live {
		t.Fatalf("a matching-class unlinked waiter must count as demand, got (%v, %v)", live, err)
	}

	// The class is deleted and recreated under the same name: the waiter's
	// classRef now resolves the REPLACEMENT class and a different workspace
	// incarnation, so it must not suppress this workspace's retention.
	if err := c.Delete(ctx, boundClass); err != nil {
		t.Fatalf("delete bound class: %v", err)
	}
	recreated := &workspacev1alpha1.ExecutionWorkspaceClass{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: acpTestNamespace, Name: workspace.Spec.ClassBinding.Name,
			UID: types.UID("recreated-class-uid"),
		},
	}
	if err := c.Create(ctx, recreated); err != nil {
		t.Fatalf("recreate class: %v", err)
	}
	live, err = liveACPSessionContinuationExists(ctx, c, workspace)
	if err != nil || live {
		t.Fatalf("a waiter resolving a recreated class must not count as demand, got (%v, %v)", live, err)
	}
}

func TestDeferACPSettlementWaitsForSuccessorBinding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := suspendableSubstrateFixture(t)
	workspace := acpAdapterWorkspace(t, "")
	workspace.Name = "acp-ws-unbound-successor"
	workspace.UID = types.UID("acp-ws-unbound-successor-uid")
	workspace.Spec.SessionRef = &workspacev1alpha1.ObjectIdentityReference{
		Name: acpTestSessionName, UID: types.UID(suspendTestSessionUID),
	}
	workspace.Annotations[acpWorkspaceDetachActionAnnotation] = string(workspacev1alpha1.WorkspaceOnDetachDelete)
	dead := retentionSettlementTask(
		"unbound-successor-dead", "unbound-successor-dead-uid", workspace,
		workspacev1alpha1.WorkspaceOnDetachDelete,
	)
	waiter := retentionContinuationTask(
		"unbound-successor-waiter", "unbound-successor-waiter-uid",
		workspacev1alpha1.WorkspaceOnDetachDelete,
	)
	r := acpClassTestReconciler(t, append(fixture.objects(), workspace, dead, waiter)...)

	deferred, retry, err := r.deferACPSettlementToSuccessor(ctx, workspace, dead)
	if err != nil || deferred || !retry {
		t.Fatalf("unbound successor deferral = (deferred=%v retry=%v err=%v), want a retry without transfer", deferred, retry, err)
	}
	currentWaiter := &corev1alpha1.Task{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(waiter), currentWaiter); err != nil {
		t.Fatalf("read unbound successor: %v", err)
	}
	if currentWaiter.Labels[acpExecutionWorkspaceLinkLabel] != "" ||
		currentWaiter.Annotations[acpExecutionWorkspaceUIDAnnotation] != "" ||
		controllerutil.ContainsFinalizer(currentWaiter, labels.TaskFinalizer) {
		t.Fatalf("unbound successor acquired settlement ownership: labels=%#v annotations=%#v finalizers=%#v",
			currentWaiter.Labels, currentWaiter.Annotations, currentWaiter.Finalizers)
	}
	if err := r.Delete(ctx, currentWaiter); err != nil {
		t.Fatalf("delete unbound successor: %v", err)
	}
	dead = bindSuspendableSessionTaskForSettlement(t, r, dead)
	done, err := r.settleACPClassWorkspace(ctx, dead)
	if err != nil || !done {
		t.Fatalf("settle after unbound successor vanished = (%v, %v), want predecessor cleanup", done, err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(workspace), &workspacev1alpha1.ExecutionWorkspace{}); !apierrors.IsNotFound(err) {
		t.Fatalf("predecessor must delete the workspace after the unbound successor vanishes, got %v", err)
	}
}

// A queued waiter whose explicit onDetach override is outside the class's
// AllowedOnDetach can never attach this workspace: it is NOT a successor, and
// settlement must keep cleanup ownership instead of transferring a policy the
// class forbids.
func TestDeferACPSettlementRejectsPolicyForbiddenSuccessorOverride(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	shape := func(name string, onDetach corev1alpha1.WorkspaceOnDetachPolicy) (bool, *workspacev1alpha1.ExecutionWorkspace, *TaskReconciler) {
		fixture := suspendableSubstrateFixture(t)
		workspace := acpAdapterWorkspace(t, "")
		workspace.Name = name
		workspace.UID = types.UID(name + "-uid")
		workspace.Spec.SessionRef = &workspacev1alpha1.ObjectIdentityReference{
			Name: acpTestSessionName, UID: types.UID("session-uid-1"),
		}
		dead := retentionSettlementTask(name+"-dead", name+"-dead-uid", workspace, workspacev1alpha1.WorkspaceOnDetachDelete)
		waiter := retentionSettlementTask(name+"-waiter", name+"-waiter-uid", workspace, workspacev1alpha1.WorkspaceOnDetach(onDetach))
		r := acpClassTestReconciler(t, append(fixture.objects(), workspace, dead, waiter)...)
		bindSuspendableSessionTaskForSettlement(t, r, waiter)
		deferred, retry, err := r.deferACPSettlementToSuccessor(ctx, workspace, dead)
		if err != nil || retry {
			t.Fatalf("defer(%s) = (deferred=%v retry=%v err=%v), want no retry and no error", name, deferred, retry, err)
		}
		return deferred, workspace, r
	}

	// AllowedOnDetach is [Delete]: a Suspend override is class-forbidden.
	if deferred, _, _ := shape("acp-ws-forbidden-override", corev1alpha1.WorkspaceOnDetachPolicy(workspacev1alpha1.WorkspaceOnDetachSuspend)); deferred {
		t.Fatal("a class-forbidden successor override must not defer settlement")
	}

	// A valid continuation queued BEHIND an ineligible one must still defer:
	// settlement scans past the forbidden override instead of concluding no
	// successor exists.
	t.Run("scans past an ineligible candidate", func(t *testing.T) {
		t.Parallel()
		fixture := suspendableSubstrateFixture(t)
		workspace := acpAdapterWorkspace(t, "")
		workspace.Name = "acp-ws-scan-candidates"
		workspace.UID = types.UID("acp-ws-scan-candidates-uid")
		workspace.Spec.SessionRef = &workspacev1alpha1.ObjectIdentityReference{
			Name: acpTestSessionName, UID: types.UID("session-uid-1"),
		}
		dead := retentionSettlementTask("scan-dead", "scan-dead-uid", workspace, workspacev1alpha1.WorkspaceOnDetachDelete)
		forbidden := retentionSettlementTask("scan-forbidden", "scan-forbidden-uid", workspace, workspacev1alpha1.WorkspaceOnDetachSuspend)
		valid := retentionSettlementTask("scan-valid", "scan-valid-uid", workspace, workspacev1alpha1.WorkspaceOnDetachDelete)
		r := acpClassTestReconciler(t, append(fixture.objects(), workspace, dead, forbidden, valid)...)
		bindSuspendableSessionTaskForSettlement(t, r, valid)
		deferred, retry, err := r.deferACPSettlementToSuccessor(context.Background(), workspace, dead)
		if err != nil || retry {
			t.Fatalf("defer = (deferred=%v retry=%v err=%v)", deferred, retry, err)
		}
		if !deferred {
			t.Fatal("a valid continuation behind an ineligible candidate must still defer settlement")
		}
	})
	deferred, workspace, r := shape("acp-ws-allowed-override", corev1alpha1.WorkspaceOnDetachPolicy(workspacev1alpha1.WorkspaceOnDetachDelete))
	if !deferred {
		t.Fatal("a class-allowed successor override must defer settlement")
	}
	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, current); err != nil {
		t.Fatalf("read deferred workspace: %v", err)
	}
	if current.Annotations[acpWorkspaceDetachActionAnnotation] != string(workspacev1alpha1.WorkspaceOnDetachDelete) {
		t.Fatalf("transferred detach action = %q, want the successor's allowed Delete", current.Annotations[acpWorkspaceDetachActionAnnotation])
	}
	// Ownership is transferred durably: the selected successor carries the
	// cleanup finalizer and exact workspace link before the predecessor's
	// settlement completes.
	linkedSuccessor := &corev1alpha1.Task{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: acpTestNamespace, Name: "acp-ws-allowed-override-waiter"}, linkedSuccessor); err != nil {
		t.Fatalf("read successor: %v", err)
	}
	if linkedSuccessor.Labels[acpExecutionWorkspaceLinkLabel] != workspace.Name ||
		linkedSuccessor.Annotations[acpExecutionWorkspaceUIDAnnotation] != string(workspace.UID) {
		t.Fatalf("successor link = %q/%q, want the deferred workspace's exact link",
			linkedSuccessor.Labels[acpExecutionWorkspaceLinkLabel], linkedSuccessor.Annotations[acpExecutionWorkspaceUIDAnnotation])
	}
	if !controllerutil.ContainsFinalizer(linkedSuccessor, labels.TaskFinalizer) {
		t.Fatal("successor cleanup finalizer must be installed before settlement ownership transfers")
	}
}

// A successor-link conflict can leave the candidate's action on the
// workspace. If that successor then disappears, the predecessor must restore
// its own effective action before settling instead of preserving stale data.
func TestSettleACPClassWorkspaceRestoresPolicyAfterSuccessorLinkConflict(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := suspendableSubstrateFixture(t)
	workspace := acpAdapterWorkspace(t, "")
	workspace.Name = "acp-ws-link-conflict"
	workspace.UID = types.UID("acp-ws-link-conflict-uid")
	workspace.Spec.SessionRef = &workspacev1alpha1.ObjectIdentityReference{
		Name: acpTestSessionName, UID: types.UID("session-uid-1"),
	}
	workspace.Spec.Lifecycle.AllowedOnDetach = append(workspace.Spec.Lifecycle.AllowedOnDetach,
		workspacev1alpha1.WorkspaceOnDetachSuspend)
	workspace.Annotations[acpWorkspaceDetachActionAnnotation] = string(workspacev1alpha1.WorkspaceOnDetachDelete)
	dead := retentionSettlementTask(
		"link-conflict-dead", "link-conflict-dead-uid", workspace,
		workspacev1alpha1.WorkspaceOnDetachDelete,
	)
	waiter := retentionContinuationTask(
		"link-conflict-waiter", "link-conflict-waiter-uid",
		workspacev1alpha1.WorkspaceOnDetachSuspend,
	)
	r := acpClassTestReconciler(t, append(fixture.objects(), workspace, dead, waiter)...)
	dead = bindSuspendableSessionTaskForSettlement(t, r, dead)
	bindSuspendableSessionTaskForSettlement(t, r, waiter)
	baseClient := r.Client
	r.Client = &conflictSuccessorLinkClient{
		Client: baseClient,
		target: types.NamespacedName{Namespace: waiter.Namespace, Name: waiter.Name},
	}
	r.APIReader = baseClient

	deferred, retry, err := r.deferACPSettlementToSuccessor(ctx, workspace, dead)
	if err != nil || deferred || !retry {
		t.Fatalf("defer with link conflict = (deferred=%v retry=%v err=%v), want a retry", deferred, retry, err)
	}
	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := baseClient.Get(ctx, client.ObjectKeyFromObject(workspace), current); err != nil {
		t.Fatalf("read workspace after link conflict: %v", err)
	}
	if current.Annotations[acpWorkspaceDetachActionAnnotation] != string(workspacev1alpha1.WorkspaceOnDetachSuspend) {
		t.Fatalf("transferred action = %q, want the failed transfer residue", current.Annotations[acpWorkspaceDetachActionAnnotation])
	}
	if err := baseClient.Delete(ctx, waiter); err != nil {
		t.Fatalf("delete vanished successor: %v", err)
	}
	done, err := r.settleACPClassWorkspace(ctx, dead)
	if err != nil || !done {
		t.Fatalf("settle after successor vanished = (%v, %v), want predecessor completion", done, err)
	}
	if err := baseClient.Get(ctx, client.ObjectKeyFromObject(workspace), current); !apierrors.IsNotFound(err) {
		t.Fatalf("the predecessor Delete policy must be restored, got %v", err)
	}
}

// A conflicted quota-fallback delete did not apply the Delete disposition and
// must not emit a permanent Warning Event. The retry emits it after deletion.
func TestSettleACPClassWorkspaceRecordsQuotaFallbackAfterDelete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := suspendableSubstrateFixture(t)
	workspace := acpAdapterWorkspace(t, "")
	workspace.Name = "acp-ws-quota-event"
	workspace.UID = types.UID("acp-ws-quota-event-uid")
	workspace.Spec.SessionRef = &workspacev1alpha1.ObjectIdentityReference{
		Name: acpTestSessionName, UID: types.UID("session-uid-1"),
	}
	workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredReady
	workspace.Spec.Lifecycle.AllowedOnDetach = append(workspace.Spec.Lifecycle.AllowedOnDetach,
		workspacev1alpha1.WorkspaceOnDetachSuspend)
	workspace.Annotations[acpWorkspaceDetachActionAnnotation] = string(workspacev1alpha1.WorkspaceOnDetachSuspend)
	workspace.Annotations[acpWorkspaceMaxSuspendedAnnotation] = "0"
	workspace.Annotations[acpWorkspaceDurableAnnotation] = booleanTrueValue
	workspace.Annotations[acpWorkspaceDurableSessionCommittedAnnotation] = "1"
	task := retentionSettlementTask(
		"quota-event-task", "quota-event-task-uid", workspace,
		workspacev1alpha1.WorkspaceOnDetachSuspend,
	)
	r := acpClassTestReconciler(t, append(fixture.objects(), workspace, task)...)
	task = bindSuspendableSessionTaskForSettlement(t, r, task)
	baseClient := r.Client
	r.Client = &conflictWorkspaceDeleteClient{
		Client: baseClient,
		target: client.ObjectKeyFromObject(workspace),
	}
	r.APIReader = baseClient
	recorder := record.NewFakeRecorder(2)
	r.Recorder = recorder

	done, err := r.settleACPClassWorkspace(ctx, task)
	if err != nil || done {
		t.Fatalf("conflicted quota fallback = (%v, %v), want a pending retry", done, err)
	}
	select {
	case event := <-recorder.Events:
		t.Fatalf("conflicted delete emitted Event %q", event)
	default:
	}
	done, err = r.settleACPClassWorkspace(ctx, task)
	if err != nil || !done {
		t.Fatalf("successful quota fallback = (%v, %v), want completion", done, err)
	}
	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, "SuspendQuotaExhausted") {
			t.Fatalf("quota fallback Event = %q, want SuspendQuotaExhausted", event)
		}
	default:
		t.Fatal("successful quota fallback did not emit its Event")
	}
}

// The quota-fallback Delete honors a live queued continuation exactly like
// the ordinary Delete branch: a pre-attachment Suspend requester whose quota
// slot was consumed must not destroy the retained repository under a
// surviving waiter.
func TestSettleACPClassWorkspaceQuotaFallbackDefersToSuccessor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := suspendableSubstrateFixture(t)
	workspace := acpAdapterWorkspace(t, "")
	workspace.Name = "acp-ws-quota-deferred"
	workspace.UID = types.UID("acp-ws-quota-deferred-uid")
	workspace.Spec.SessionRef = &workspacev1alpha1.ObjectIdentityReference{
		Name: acpTestSessionName, UID: types.UID("session-uid-1"),
	}
	workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredReady
	workspace.Spec.Lifecycle.AllowedOnDetach = append(workspace.Spec.Lifecycle.AllowedOnDetach,
		workspacev1alpha1.WorkspaceOnDetachSuspend)
	workspace.Annotations[acpWorkspaceDetachActionAnnotation] = string(workspacev1alpha1.WorkspaceOnDetachSuspend)
	// A frozen cap of zero makes any suspension quota-exhausted.
	workspace.Annotations[acpWorkspaceMaxSuspendedAnnotation] = "0"
	workspace.Annotations[acpWorkspaceDurableAnnotation] = booleanTrueValue
	workspace.Annotations[acpWorkspaceDurableSessionCommittedAnnotation] = "1"
	dead := retentionSettlementTask(
		"lc-quota-dead", "lc-quota-dead-uid", workspace,
		workspacev1alpha1.WorkspaceOnDetachSuspend,
	)
	waiter := retentionSettlementTask(
		"lc-quota-waiter", "lc-quota-waiter-uid", workspace,
		workspacev1alpha1.WorkspaceOnDetachSuspend,
	)
	r := acpClassTestReconciler(t, append(fixture.objects(), workspace, dead, waiter)...)
	dead = bindSuspendableSessionTaskForSettlement(t, r, dead)
	bindSuspendableSessionTaskForSettlement(t, r, waiter)
	done, err := r.settleACPClassWorkspace(ctx, dead)
	if err != nil || !done {
		t.Fatalf("quota-exhausted settle with a queued continuation = (%v, %v), want deferred completion", done, err)
	}
	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, current); err != nil {
		t.Fatalf("the workspace must survive the quota fallback for the queued continuation: %v", err)
	}
}
