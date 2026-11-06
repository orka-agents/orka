// Copyright (c) 2026. MIT License - see LICENSE file for details.

package controller

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"
	"time"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/operatoraccess"
)

func operatorTaintTestClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := workspacev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func operatorTaintTestTask(annotations map[string]string) *corev1alpha1.Task {
	all := map[string]string{acpExecutionWorkspaceUIDAnnotation: "ws-uid"}
	maps.Copy(all, annotations)
	return &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
		Namespace: "team", Name: "task", UID: "task-uid",
		Labels: map[string]string{acpExecutionWorkspaceLinkLabel: "ws"}, Annotations: all,
	}}
}

func TestACPTaskOperatorTaintReadsTaskAndExactWorkspace(t *testing.T) {
	tainted := map[string]string{operatoraccess.TaintAnnotation: operatoraccess.EncodeTaint(operatoraccess.Taint{AuditID: "op-1"})}
	for name, tc := range map[string]struct {
		task      *corev1alpha1.Task
		workspace *workspacev1alpha1.ExecutionWorkspace
		want      bool
	}{
		"clean":           {operatorTaintTestTask(nil), &workspacev1alpha1.ExecutionWorkspace{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "ws", UID: "ws-uid"}}, false},
		"task tainted":    {operatorTaintTestTask(tainted), nil, true},
		"workspace taint": {operatorTaintTestTask(nil), &workspacev1alpha1.ExecutionWorkspace{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "ws", UID: "ws-uid", Annotations: tainted}}, true},
		"replacement workspace taint is not this task's": {operatorTaintTestTask(nil),
			&workspacev1alpha1.ExecutionWorkspace{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "ws", UID: "other-uid", Annotations: tainted}}, false},
		"workspace gone": {operatorTaintTestTask(nil), nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			objects := []client.Object{tc.task}
			if tc.workspace != nil {
				objects = append(objects, tc.workspace)
			}
			c := operatorTaintTestClient(t, objects...)
			got, err := acpTaskOperatorTainted(context.Background(), c, tc.task)
			if err != nil || got != tc.want {
				t.Fatalf("acpTaskOperatorTainted = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
	// A Task that cannot be read fails closed instead of reporting clean.
	if _, err := acpTaskOperatorTainted(context.Background(), operatorTaintTestClient(t), operatorTaintTestTask(nil)); err == nil {
		t.Fatal("an unreadable Task must fail closed")
	}
}

func TestPublicationRefusedForOperatorTaintedTask(t *testing.T) {
	for name, tc := range map[string]struct {
		annotations map[string]string
		want        bool
	}{
		"clean":   {nil, false},
		"tainted": {map[string]string{operatoraccess.TaintAnnotation: "x"}, true},
	} {
		t.Run(name, func(t *testing.T) {
			task := operatorTaintTestTask(tc.annotations)
			c := operatorTaintTestClient(t, task)
			d := &ACPDispatcher{Client: c, APIReader: c}
			refused, err := d.taskPublicationRefused(context.Background(), task)
			if err != nil || refused != tc.want {
				t.Fatalf("taskPublicationRefused = %v, %v; want %v", refused, err, tc.want)
			}
		})
	}
}

func TestSuspensionRefusesOperatorTaintedWorkspace(t *testing.T) {
	workspace := &workspacev1alpha1.ExecutionWorkspace{ObjectMeta: metav1.ObjectMeta{
		Namespace: "team", Name: "ws", UID: "ws-uid",
		Annotations: map[string]string{operatoraccess.TaintAnnotation: "x"},
	}, Spec: workspacev1alpha1.ExecutionWorkspaceSpec{DesiredState: workspacev1alpha1.ExecutionWorkspaceDesiredReady}}
	c := operatorTaintTestClient(t, workspace)
	err := suspendACPWorkspaceWithinQuota(context.Background(), c, c, workspace.DeepCopy(), time.Now(), false, "task-uid", 1)
	if !errors.Is(err, operatoraccess.ErrTainted) {
		t.Fatalf("suspension of a tainted workspace = %v, want ErrTainted", err)
	}
	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(workspace), current); err != nil {
		t.Fatal(err)
	}
	if current.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady {
		t.Fatal("a tainted workspace was suspended into a data checkpoint")
	}
}

func TestCheckpointRoutingRefusesOperatorTaintedSource(t *testing.T) {
	ctx := context.Background()
	f := newExternalRuntimePoolFixture(t)
	provider := f.provider.DeepCopy()
	provider.Spec.ControllerName = f.workspace.Labels[workspacev1alpha1.ProviderControllerLabel]
	provider.Status.SupportedFeatures = append(provider.Status.SupportedFeatures, workspacev1alpha1.WorkspaceFeatureCheckpoint)
	if err := f.r.Update(ctx, provider); err != nil {
		t.Fatal(err)
	}
	source := &workspacev1alpha1.ExecutionWorkspace{}
	if err := f.r.Get(ctx, client.ObjectKeyFromObject(f.workspace), source); err != nil {
		t.Fatal(err)
	}
	source.Annotations[operatoraccess.TaintAnnotation] = "x"
	if err := f.r.Update(ctx, source); err != nil {
		t.Fatal(err)
	}
	checkpoint := &workspacev1alpha1.ExecutionWorkspaceCheckpoint{
		ObjectMeta: metav1.ObjectMeta{Namespace: f.workspace.Namespace, Name: "fork-export", UID: "checkpoint-uid"},
		Spec:       workspacev1alpha1.ExecutionWorkspaceCheckpointSpec{WorkspaceRef: workspacev1alpha1.ObjectIdentityReference{Name: f.workspace.Name, UID: f.workspace.UID}},
	}
	if err := f.r.Create(ctx, checkpoint); err != nil {
		t.Fatal(err)
	}
	r := &WorkspaceCheckpointRoutingReconciler{Client: f.r.Client, APIReader: f.r.Client}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(checkpoint)}); err != nil {
		t.Fatal(err)
	}
	if err := f.r.Get(ctx, client.ObjectKeyFromObject(checkpoint), checkpoint); err != nil {
		t.Fatal(err)
	}
	if checkpoint.Labels[workspaceCheckpointProviderNameLabel] != "" || checkpoint.Labels[workspacev1alpha1.ProviderControllerLabel] != "" {
		t.Fatal("a tainted source was routed to a provider for export")
	}
	if checkpoint.Annotations[workspaceCheckpointRoutingFailureAnnotation] != "SourceOperatorTainted" {
		t.Fatalf("routing diagnostic = %q", checkpoint.Annotations[workspaceCheckpointRoutingFailureAnnotation])
	}
}

func TestTaintedWorkspaceIsNeverReusedOrResumed(t *testing.T) {
	ctx := context.Background()
	r, task, plan, workspace := externalACPSessionLifecycleFixture(t)
	continuation := task.DeepCopy()
	continuation.Name, continuation.UID, continuation.ResourceVersion = "continuation", "continuation-uid", ""
	if err := r.Create(ctx, continuation); err != nil {
		t.Fatal(err)
	}
	// A suspended (retained) incarnation that operator access tainted must
	// not be cold-resumed for the continuation.
	workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
	workspace.Status.State = workspacev1alpha1.ExecutionWorkspaceStateSuspended
	workspace.Annotations[operatoraccess.TaintAnnotation] = "x"
	workspace.Annotations[acpWorkspaceLastDetachedAnnotation] = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	if err := r.Update(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	name, ready, err := r.ensureACPClassWorkspace(ctx, continuation, plan)
	if !errors.Is(err, operatoraccess.ErrTainted) || !errors.Is(err, errACPWorkspaceTerminalFailure) || ready || name != "" {
		t.Fatalf("continuation on a tainted workspace = (%q, %v, %v), want terminal ErrTainted", name, ready, err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(workspace), workspace); err != nil {
		t.Fatal(err)
	}
	if workspace.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredSuspended ||
		strings.Contains(workspace.Annotations[acpWorkspaceResumeRequestedAnnotation], string(continuation.UID)) {
		t.Fatalf("a tainted workspace was flipped toward cold resume: %s %q", workspace.Spec.DesiredState, workspace.Annotations[acpWorkspaceResumeRequestedAnnotation])
	}

	// While the tainting Task still holds the attachment, a continuation
	// queues for the fresh replacement instead of reusing the workspace warm.
	workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredReady
	workspace.Status.State = workspacev1alpha1.ExecutionWorkspaceStateReady
	workspace.Spec.Attachment = &workspacev1alpha1.ExecutionWorkspaceAttachment{
		TaskRef: workspacev1alpha1.ObjectIdentityReference{Name: task.Name, UID: task.UID}, Epoch: 4,
		TokenSHA256: "sha256:" + "0000000000000000000000000000000000000000000000000000000000000000",
		ExpiresAt:   metav1.NewTime(time.Now().Add(time.Hour)),
	}
	if err := r.Update(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	name, ready, err = r.ensureACPClassWorkspace(ctx, continuation, plan)
	if err != nil || ready || name != "" {
		t.Fatalf("continuation behind a tainted holder = (%q, %v, %v), want queued", name, ready, err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(workspace), workspace); err != nil {
		t.Fatal(err)
	}
	if workspace.Spec.Attachment == nil || workspace.Spec.Attachment.TaskRef.UID != task.UID {
		t.Fatal("the continuation took over a tainted workspace")
	}
}

func TestIdleRetentionDeletesTaintedSuspendableWorkspace(t *testing.T) {
	ctx := context.Background()
	workspace := retentionTestWorkspace(t, "acp-ws-retention-tainted", func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Spec.Lifecycle.DefaultOnDetach = workspacev1alpha1.WorkspaceOnDetachSuspend
		w.Spec.Lifecycle.AllowedOnDetach = []workspacev1alpha1.WorkspaceOnDetach{
			workspacev1alpha1.WorkspaceOnDetachSuspend, workspacev1alpha1.WorkspaceOnDetachDelete,
		}
		w.Annotations[acpWorkspaceDetachActionAnnotation] = string(workspacev1alpha1.WorkspaceOnDetachSuspend)
		w.Annotations[acpWorkspaceDurableSessionCommittedAnnotation] = "1"
		w.Annotations[acpWorkspaceLastDetachedAnnotation] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
		w.Annotations[operatoraccess.TaintAnnotation] = "x"
	})
	c := acpAdapterTestClient(t, workspace)
	reconcileRetention(t, c, workspace)
	current := &workspacev1alpha1.ExecutionWorkspace{}
	err := c.Get(ctx, client.ObjectKeyFromObject(workspace), current)
	if err == nil && current.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredSuspended {
		t.Fatal("idle retention suspended a tainted workspace into a data checkpoint")
	}
	if err == nil && current.DeletionTimestamp.IsZero() {
		t.Fatalf("idle retention must delete a tainted workspace, desired state %s", current.Spec.DesiredState)
	}
}

func TestSettlementDeletesTaintedSuspendWorkspace(t *testing.T) {
	ctx := context.Background()
	fixture := suspendableSubstrateFixture(t)
	workspace := acpAdapterWorkspace(t, "")
	workspace.Name = "acp-ws-tainted-suspend"
	workspace.UID = "acp-ws-tainted-suspend-uid"
	workspace.Spec.SessionRef = &workspacev1alpha1.ObjectIdentityReference{Name: acpTestSessionName, UID: suspendTestSessionUID}
	workspace.Spec.Lifecycle.AllowedOnDetach = append(workspace.Spec.Lifecycle.AllowedOnDetach, workspacev1alpha1.WorkspaceOnDetachSuspend)
	workspace.Annotations[acpWorkspaceDetachActionAnnotation] = string(workspacev1alpha1.WorkspaceOnDetachSuspend)
	workspace.Annotations[acpWorkspaceDurableAnnotation] = booleanTrueValue
	workspace.Annotations[acpWorkspaceDurableSessionCommittedAnnotation] = "1"
	workspace.Annotations[acpWorkspaceMaxSuspendedAnnotation] = "1"
	workspace.Annotations[acpWorkspaceLastDetachedAnnotation] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	workspace.Annotations[operatoraccess.TaintAnnotation] = "x"
	task := retentionSettlementTask("tainted-suspend-task", "tainted-suspend-task-uid", workspace, workspacev1alpha1.WorkspaceOnDetachSuspend)
	r := acpClassTestReconciler(t, append(fixture.objects(), workspace, task)...)
	task = bindSuspendableSessionTaskForSettlement(t, r, task)
	done, err := r.settleACPClassWorkspace(ctx, task)
	if err != nil || !done {
		t.Fatalf("settlement = (%v, %v), want completed", done, err)
	}
	current := &workspacev1alpha1.ExecutionWorkspace{}
	err = r.Get(ctx, client.ObjectKeyFromObject(workspace), current)
	if err == nil && current.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredSuspended {
		t.Fatal("settlement suspended a tainted workspace instead of deleting it")
	}
	if err == nil && current.DeletionTimestamp.IsZero() {
		t.Fatal("settlement must delete a tainted workspace")
	}
}

// The attachment manager revalidates the exact object version of every
// optimistic-lock attempt, so a taint orka-api commits after the reconciler's
// initial read must still refuse to hand the workspace to another Task.
func TestAttachmentAttemptRefusesAWorkspaceTaintedAfterTheInitialRead(t *testing.T) {
	ctx := context.Background()
	r, _, _, _ := externalACPFixture(t, true)
	r.Scheme = r.Client.Scheme()
	task := acpClassTestTask()
	resolved, err := r.resolveACPWorkspaceClass(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := resolveACPWorkspaceBindingWithClass(task, "", resolved)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := r.createACPClassWorkspace(ctx, task, binding, "pool", "workspace")
	if err != nil {
		t.Fatal(err)
	}
	if err := validateACPClassWorkspaceAttachmentAttempt(workspace, task, binding, "pool", time.Now()); errors.Is(err, operatoraccess.ErrTainted) {
		t.Fatalf("an untainted workspace was refused as tainted: %v", err)
	}
	tainted := workspace.DeepCopy()
	tainted.Annotations[operatoraccess.TaintAnnotation] = operatoraccess.EncodeTaint(operatoraccess.Taint{AuditID: "op-1"})
	if err := validateACPClassWorkspaceAttachmentAttempt(tainted, task, binding, "pool", time.Now()); !errors.Is(err, operatoraccess.ErrTainted) {
		t.Fatalf("attachment attempt on a tainted workspace = %v, want ErrTainted", err)
	}
}
