// Copyright (c) 2026. MIT License - see LICENSE file for details.

package controller

import (
	"bytes"
	"context"
	"reflect"
	"testing"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func TestExternalWorkspaceCleanupOnlyNeverPublishesRuntime(t *testing.T) {
	ctx := context.Background()
	f := newExternalRuntimePoolFixture(t)
	f.r.WorkspaceCleanupOnly = true
	before := f.currentWorkspace(t).DeepCopy()
	for range 4 {
		runtimePoolReconcile(t, f.r, f.pool)
	}
	workspace := f.currentWorkspace(t)
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	if !reflect.DeepEqual(workspace.Spec, before.Spec) || pool.Status.ActiveInstance != nil ||
		pool.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed || f.seeds != 0 {
		t.Fatal("disabled workspace admission published compute intent or released runtime credentials")
	}
	for _, objects := range []client.ObjectList{&corev1.SecretList{}, &corev1.ServiceList{}, &appsv1.DeploymentList{}, &corev1.PodList{}, &networkingv1.NetworkPolicyList{}, &policyv1.PodDisruptionBudgetList{}} {
		if err := f.r.List(ctx, objects); err != nil {
			t.Fatal(err)
		}
		items, err := apimeta.ExtractList(objects)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 0 {
			t.Fatalf("cleanup-only admission created %T", objects)
		}
	}
}

func TestWorkspaceCleanupOnlyPreservesPlainRuntimePoolAdmission(t *testing.T) {
	ctx := context.Background()
	pool := runtimePoolTestObject(1)
	r := runtimePoolTestReconciler(t, runtimePoolTestScheme(t), nil, pool)
	r.WorkspaceCleanupOnly = true
	for range 3 {
		runtimePoolReconcile(t, r, pool)
	}
	got := runtimePoolTestGetPool(t, r, pool)
	if !controllerutil.ContainsFinalizer(&got, runtimePoolFinalizer) {
		t.Fatal("disabled workspace API stopped ordinary RuntimePool admission")
	}
	var deployments appsv1.DeploymentList
	if err := r.List(ctx, &deployments); err != nil {
		t.Fatal(err)
	}
	if len(deployments.Items) != 1 || deployments.Items[0].Spec.Replicas == nil || *deployments.Items[0].Spec.Replicas != 1 {
		t.Fatal("disabled workspace API prevented ordinary singleton materialization")
	}
}

func TestExternalWorkspaceCleanupOnlyRetiresExactExistingRuntime(t *testing.T) {
	ctx := context.Background()
	f := newExternalRuntimePoolFixture(t)
	workspace, pod := f.serve(t)
	request := workspace.Spec.Workload.DeepCopy()
	auth := runtimePoolTestPrivateAuthSecret(t, f.r, f.pool)
	seeds := f.seeds
	active := runtimePoolTestGetPool(t, f.r, f.pool).Status.ActiveInstance.DeepCopy()
	f.r.WorkspaceCleanupOnly = true
	runtimePoolReconcile(t, f.r, f.pool)
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	if pool.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed || !reflect.DeepEqual(pool.Status.ActiveInstance, active) {
		t.Fatal("cleanup-only mode lost the current exact runtime fence or kept admission open")
	}
	if err := f.r.Delete(ctx, &pool, deleteCurrentObjectPreconditions(&pool)...); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		runtimePoolReconcile(t, f.r, f.pool)
	}
	f.supervisor.probe = runtimePoolValidProbe(f.pool, &pod, "external-boot", true)
	runtimePoolReconcile(t, f.r, f.pool)
	workspace = f.currentWorkspace(t)
	if workspace.Spec.Retirement == nil || workspace.Spec.Retirement.Action != workspacev1alpha1.WorkloadRetirementDelete ||
		workspace.Spec.Retirement.Identity != workspace.Status.Allocation.Identity || workspace.Spec.Retirement.Sequence != request.Sequence {
		t.Fatal("cleanup-only mode did not authorize the exact drained runtime's retirement")
	}
	workspace.Status.Allocation.Startup = nil
	workspace.Status.Allocation.State = workspacev1alpha1.AllocationDeleted
	workspace.Status.ObservedGeneration = workspace.Generation
	workspace.Status.Allocation.Disposition = &workspacev1alpha1.ExecutionWorkspaceDisposition{
		Compute: workspacev1alpha1.DispositionDeleted, AccessCredentials: workspacev1alpha1.DispositionRevoked,
		EphemeralSecrets: workspacev1alpha1.DispositionDeleted, WorkspaceData: workspacev1alpha1.DispositionDeleted,
		PersistentVolumes: workspacev1alpha1.DispositionDeleted, Checkpoints: workspacev1alpha1.DispositionDeleted,
		ProviderResources: workspacev1alpha1.DispositionDeleted,
	}
	if err := f.r.Status().Update(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	runtimePoolReconcile(t, f.r, f.pool)
	kept := &corev1.Secret{}
	if err := f.r.Get(ctx, client.ObjectKeyFromObject(&auth), kept); err != nil || !bytes.Equal(kept.Data[runtimePoolControllerTokenKey], auth.Data[runtimePoolControllerTokenKey]) {
		t.Fatalf("provider assertion removed credentials before independent Pod absence: %v", err)
	}
	if err := f.r.Get(ctx, client.ObjectKeyFromObject(&pod), &corev1.Pod{}); err != nil {
		t.Fatalf("core deleted provider compute during cleanup-only retirement: %v", err)
	}
	// The provider removes its exact native runtime. Core only observes it.
	if err := f.r.Delete(ctx, &pod, deleteCurrentObjectPreconditions(&pod)...); err != nil {
		t.Fatal(err)
	}
	for range 6 {
		runtimePoolReconcile(t, f.r, f.pool)
	}
	if err := f.r.Get(ctx, client.ObjectKeyFromObject(f.pool), &corev1alpha1.RuntimePool{}); !apierrors.IsNotFound(err) {
		t.Fatalf("cleanup-only retirement retained the RuntimePool: %v", err)
	}
	var credentials corev1.SecretList
	if err := f.r.List(ctx, &credentials, client.MatchingLabels{runtimePoolUIDLabel: string(f.pool.UID)}); err != nil {
		t.Fatal(err)
	}
	if len(credentials.Items) != 0 || f.seeds != seeds || !reflect.DeepEqual(f.currentWorkspace(t).Spec.Workload, request) {
		t.Fatal("cleanup-only retirement retained credentials or published a replacement runtime")
	}
}

func TestExternalWorkspaceCleanupOnlyRecoversFinalizerBeforeDeletion(t *testing.T) {
	ctx := context.Background()
	f, secret, lease := preRequestDeletionFixture(t)
	workspace := f.currentWorkspace(t)
	workspace.Finalizers = nil
	if err := f.r.Update(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	before := workspace.DeepCopy()
	core := &ExecutionWorkspaceReconciler{Client: f.r.Client, APIReader: f.r.Client, CleanupOnly: true}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workspace)}
	if _, err := core.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	workspace = f.currentWorkspace(t)
	if !controllerutil.ContainsFinalizer(workspace, executionWorkspaceFinalizer) || workspace.UID != before.UID ||
		!reflect.DeepEqual(workspace.Spec, before.Spec) || !reflect.DeepEqual(workspace.Status, before.Status) {
		t.Fatal("cleanup-only restart did not recover the exact external ACP workspace finalizer without admission")
	}
	f.r.WorkspaceCleanupOnly = true
	if err := f.r.Delete(ctx, workspace, deleteCurrentObjectPreconditions(workspace)...); err != nil {
		t.Fatal(err)
	}
	if _, err := core.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	observePreRequestDeleted(t, f)
	for range 5 {
		if _, err := core.Reconcile(ctx, request); err != nil {
			t.Fatal(err)
		}
		runtimePoolReconcile(t, f.r, f.pool)
	}
	assertPreRequestResourcesGone(t, f, secret, lease)
}

func TestWorkspaceCleanupOnlyDoesNotRecoverGenericFinalizer(t *testing.T) {
	ctx := context.Background()
	f := newExternalRuntimePoolFixture(t)
	workspace := f.currentWorkspace(t)
	delete(workspace.Annotations, acpExecutionWorkspacePoolAnnotation)
	if err := f.r.Update(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	before := workspace.DeepCopy()
	core := &ExecutionWorkspaceReconciler{Client: f.r.Client, APIReader: f.r.Client, CleanupOnly: true}
	if _, err := core.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workspace)}); err != nil {
		t.Fatal(err)
	}
	workspace = f.currentWorkspace(t)
	if !reflect.DeepEqual(workspace, before) {
		t.Fatal("provider route alone authorized cleanup-only core ownership")
	}
}
