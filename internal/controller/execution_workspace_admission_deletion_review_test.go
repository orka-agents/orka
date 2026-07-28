package controller

import (
	"context"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
	"github.com/orka-agents/orka/pkg/workspaceprovider"
)

func TestExecutionWorkspaceClassDeletionProtection(t *testing.T) {
	t.Parallel()
	class, _, workspace := workspacePolicyReviewFixture(t)
	class.Finalizers = []string{executionWorkspaceClassFinalizer}
	deletionTimestamp := metav1.Now()
	class.DeletionTimestamp = &deletionTimestamp
	workspace.Status.State = workspacev1alpha1.ExecutionWorkspaceStateAttached
	markWorkspaceAdmittedForPolicyReview(workspace, workspace.Generation)

	c := fake.NewClientBuilder().
		WithScheme(testWorkspaceScheme(t)).
		WithStatusSubresource(class, workspace).
		WithObjects(class, workspace).
		Build()
	reconciler := &ExecutionWorkspaceClassReconciler{Client: c, APIReader: c}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: class.Namespace, Name: class.Name}}
	result, err := reconciler.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := &workspacev1alpha1.ExecutionWorkspaceClass{}
	if err := c.Get(context.Background(), request.NamespacedName, got); err != nil {
		t.Fatalf("get protected class: %v", err)
	}
	if !controllerutil.ContainsFinalizer(got, executionWorkspaceClassFinalizer) {
		t.Fatal("class finalizer was removed while a core-admitted workspace remained bound")
	}
	condition := workspaceprovider.FindCondition(got.Status.Conditions, string(workspacev1alpha1.ConditionClassReady))
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "ReferencesRemain" {
		t.Fatalf("class condition = %#v, want Ready=False/ReferencesRemain", condition)
	}
	if result.RequeueAfter != classReadinessRequeue {
		t.Fatalf("RequeueAfter = %s, want %s", result.RequeueAfter, classReadinessRequeue)
	}
}

func TestExecutionWorkspaceClassDeletionDoesNotAdmitNewWorkspace(t *testing.T) {
	t.Parallel()
	class, provider, workspace := workspacePolicyReviewFixture(t)
	class.Finalizers = []string{executionWorkspaceClassFinalizer}
	deletionTimestamp := metav1.Now()
	class.DeletionTimestamp = &deletionTimestamp
	workspace.Status.State = workspacev1alpha1.ExecutionWorkspaceStateReady

	reason := validateWorkspacePolicyReviewBindings(t, class, provider, workspace)
	if reason != "ClassDeleting" {
		t.Fatalf("new workspace reason = %q, want ClassDeleting", reason)
	}

	markWorkspaceAdmittedForPolicyReview(workspace, workspace.Generation)
	reason = validateWorkspacePolicyReviewBindings(t, class, provider, workspace)
	if reason != string(workspacev1alpha1.ReasonReady) {
		t.Fatalf("existing workspace reason = %q, want Ready", reason)
	}
}

func TestExecutionWorkspaceCoreAdmissionPrecedesAdapterStatus(t *testing.T) {
	t.Parallel()
	class, provider, workspace := workspacePolicyReviewFixture(t)
	workspace.Finalizers = []string{executionWorkspaceFinalizer}
	mapper, parameters := testParameterMapping(class.Namespace, class.Spec.ParametersRef)

	c := fake.NewClientBuilder().
		WithScheme(testWorkspaceScheme(t)).
		WithStatusSubresource(workspace).
		WithObjects(class, provider, workspace, parameters).
		Build()
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}}
	core := &ExecutionWorkspaceReconciler{Client: c, APIReader: c, RESTMapper: mapper}
	adapter := &FakeExecutionWorkspaceReconciler{Client: c, APIReader: c}

	if _, err := core.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("prepublish core admission condition: %v", err)
	}
	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(context.Background(), request.NamespacedName, current); err != nil {
		t.Fatalf("get workspace after prepublished condition: %v", err)
	}
	condition := workspaceprovider.FindCondition(current.Status.Conditions, string(workspacev1alpha1.ConditionWorkspaceAdmitted))
	if current.Spec.CoreAdmission != nil || condition == nil || condition.Status != metav1.ConditionTrue ||
		condition.ObservedGeneration != current.Generation+1 {
		t.Fatalf("pre-admission state = marker %#v, condition %#v", current.Spec.CoreAdmission, condition)
	}
	if result, err := adapter.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("adapter before spec marker: %v", err)
	} else if result.RequeueAfter != workspaceRequeueInterval {
		t.Fatalf("adapter RequeueAfter = %s, want %s", result.RequeueAfter, workspaceRequeueInterval)
	}

	if _, err := core.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("publish core admission marker: %v", err)
	}
	if err := c.Get(context.Background(), request.NamespacedName, current); err != nil {
		t.Fatalf("get workspace after core marker: %v", err)
	}
	if !workspaceHasCoreAdmission(current) {
		t.Fatalf("core admission marker = %#v, want matching immutable bindings", current.Spec.CoreAdmission)
	}
	// controller-runtime's fake client does not simulate API-server generation
	// increments for spec patches. Advance it to the generation core predicted.
	current.Generation = current.Spec.CoreAdmission.AdmittedGeneration
	if err := c.Update(context.Background(), current); err != nil {
		t.Fatalf("simulate API generation increment: %v", err)
	}
	if !workspaceCurrentlyAdmittedByCore(current) {
		t.Fatalf("workspace core admission = %#v at generation %d, want current", current.Spec.CoreAdmission, current.Generation)
	}

	if _, err := adapter.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("adapter after dual core admission: %v", err)
	}
	if err := c.Get(context.Background(), request.NamespacedName, current); err != nil {
		t.Fatalf("get admitted workspace: %v", err)
	}
	if current.Status.State != workspacev1alpha1.ExecutionWorkspaceStateReady {
		t.Fatalf("workspace state = %q, want Ready", current.Status.State)
	}
}

func TestExecutionWorkspaceCoreAdmissionRevalidatesProviderBeforeMarker(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	class, provider, workspace := workspacePolicyReviewFixture(t)
	mapper, parameters := testParameterMapping(class.Namespace, class.Spec.ParametersRef)
	c := fake.NewClientBuilder().
		WithScheme(testWorkspaceScheme(t)).
		WithStatusSubresource(workspace).
		WithObjects(class, provider, workspace, parameters).
		Build()
	reconciler := &ExecutionWorkspaceReconciler{Client: c, APIReader: c, RESTMapper: mapper}

	reason, _, err := reconciler.validateWorkspaceBindings(ctx, workspace)
	if err != nil || reason != string(workspacev1alpha1.ReasonReady) {
		t.Fatalf("initial validation = %q, %v, want Ready", reason, err)
	}
	currentProvider := &workspacev1alpha1.ExecutionWorkspaceProvider{}
	if err := c.Get(ctx, types.NamespacedName{Name: provider.Name}, currentProvider); err != nil {
		t.Fatalf("get provider before lifecycle change: %v", err)
	}
	currentProvider.Spec.LifecycleState = workspacev1alpha1.ExecutionWorkspaceProviderDraining
	if err := c.Update(ctx, currentProvider); err != nil {
		t.Fatalf("set provider draining after initial validation: %v", err)
	}
	currentWorkspace := &workspacev1alpha1.ExecutionWorkspace{}
	key := types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}
	if err := c.Get(ctx, key, currentWorkspace); err != nil {
		t.Fatalf("get workspace before marker publication: %v", err)
	}

	reason, _, err = reconciler.patchWorkspaceCoreAdmission(ctx, currentWorkspace, currentWorkspace.Generation+1)
	if err != nil {
		t.Fatalf("patchWorkspaceCoreAdmission: %v", err)
	}
	if reason != string(workspacev1alpha1.ReasonProviderDraining) {
		t.Fatalf("marker revalidation reason = %q, want %q", reason, workspacev1alpha1.ReasonProviderDraining)
	}
	if err := c.Get(ctx, key, currentWorkspace); err != nil {
		t.Fatalf("get workspace after rejected marker publication: %v", err)
	}
	if currentWorkspace.Spec.CoreAdmission != nil {
		t.Fatalf("core admission was minted after provider became Draining: %#v", currentWorkspace.Spec.CoreAdmission)
	}
}

func TestExecutionWorkspaceDrainingProviderQuarantinesPendingWorkspace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	class, provider, workspace := workspacePolicyReviewFixture(t)
	provider.Spec.LifecycleState = workspacev1alpha1.ExecutionWorkspaceProviderDraining
	workspace.Finalizers = []string{executionWorkspaceFinalizer}
	mapper, parameters := testParameterMapping(class.Namespace, class.Spec.ParametersRef)
	c := fake.NewClientBuilder().
		WithScheme(testWorkspaceScheme(t)).
		WithStatusSubresource(workspace).
		WithObjects(class, provider, workspace, parameters).
		Build()
	reconciler := &ExecutionWorkspaceReconciler{Client: c, APIReader: c, RESTMapper: mapper}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}}
	for range 2 {
		if _, err := reconciler.Reconcile(ctx, request); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}

	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, request.NamespacedName, current); err != nil {
		t.Fatalf("get quarantined workspace: %v", err)
	}
	if current.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined ||
		current.Labels[workspacev1alpha1.QuarantinedLabel] != scheduledRunLabelValue {
		t.Fatalf("draining provider left workspace pending: spec=%#v labels=%#v", current.Spec, current.Labels)
	}
	if current.Spec.CoreAdmission != nil {
		t.Fatalf("draining provider minted core admission: %#v", current.Spec.CoreAdmission)
	}
	condition := workspaceprovider.FindCondition(current.Status.Conditions, string(workspacev1alpha1.ConditionWorkspaceAdmitted))
	if condition == nil || condition.Status != metav1.ConditionFalse ||
		condition.Reason != string(workspacev1alpha1.ReasonProviderDraining) {
		t.Fatalf("draining provider admission condition = %#v", condition)
	}
}

func TestExecutionWorkspacePoolAdmissionLeaseContentionDoesNotPublishCapacityDenial(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	provider := testGenericProvider("provider")
	provider.Status.ObservedGeneration = provider.Generation
	provider.Status.SupportedContracts = []string{workspacev1alpha1.ContractVersionV1}
	provider.Status.SupportedFeatures = []workspacev1alpha1.ExecutionWorkspaceFeature{
		workspacev1alpha1.WorkspaceFeatureExec,
		workspacev1alpha1.WorkspaceFeaturePools,
		workspacev1alpha1.WorkspaceFeatureReset,
		workspacev1alpha1.WorkspaceFeatureSuspend,
		workspacev1alpha1.WorkspaceFeatureTLS,
	}
	workspaceprovider.SetCondition(&provider.Status.Conditions, metav1.Condition{
		Type:               string(workspacev1alpha1.ConditionProviderReady),
		Status:             metav1.ConditionTrue,
		Reason:             string(workspacev1alpha1.ReasonReady),
		ObservedGeneration: provider.Generation,
	})
	pool := testGenericPool("default", "pool", provider.Name)
	pool.Status.ObservedGeneration = pool.Generation
	workspaceprovider.SetCondition(&pool.Status.Conditions, metav1.Condition{
		Type:               string(workspacev1alpha1.ConditionPoolReady),
		Status:             metav1.ConditionTrue,
		Reason:             string(workspacev1alpha1.ReasonReady),
		ObservedGeneration: pool.Generation,
	})
	workspaceprovider.SetCondition(&pool.Status.Conditions, metav1.Condition{
		Type:               string(workspacev1alpha1.ConditionPoolAdmitted),
		Status:             metav1.ConditionTrue,
		Reason:             string(workspacev1alpha1.ReasonReady),
		ObservedGeneration: pool.Generation,
	})
	class := testGenericClass(pool.Namespace, "class", provider.Name)
	class.Spec.ProviderRef = nil
	class.Spec.ParametersRef = nil
	class.Spec.PoolRef = &corev1.LocalObjectReference{Name: pool.Name}
	workspace := testBoundWorkspace(t, class.Namespace, "workspace", class.Name, provider.Name)
	workspace.Finalizers = []string{executionWorkspaceFinalizer}
	mapper, parameters := preparePooledClassProfileForTest(t, provider, pool, class, workspace)
	class.Status.ObservedGeneration = class.Generation
	workspaceprovider.SetCondition(&class.Status.Conditions, metav1.Condition{
		Type:               string(workspacev1alpha1.ConditionClassReady),
		Status:             metav1.ConditionTrue,
		Reason:             string(workspacev1alpha1.ReasonReady),
		ObservedGeneration: class.Generation,
	})

	foreignHolder := "another-workspace"
	leaseDuration := int32(workspacePoolAdmissionLeaseDuration / time.Second)
	now := metav1.NewMicroTime(time.Now().UTC())
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      workspacePoolAdmissionLeaseName(pool),
			Namespace: pool.Namespace,
		},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       &foreignHolder,
			LeaseDurationSeconds: &leaseDuration,
			AcquireTime:          &now,
			RenewTime:            &now,
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(testWorkspaceScheme(t)).
		WithStatusSubresource(workspace).
		WithObjects(provider, pool, class, workspace, parameters, lease).
		Build()
	reconciler := &ExecutionWorkspaceReconciler{Client: c, APIReader: c, RESTMapper: mapper}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("prepublish admission condition: %v", err)
	}
	result, err := reconciler.Reconcile(ctx, request)
	if err != nil {
		t.Fatalf("reconcile contended pool admission: %v", err)
	}
	if result.RequeueAfter != workspacePoolAdmissionContentionRequeue {
		t.Fatalf("RequeueAfter = %s, want %s", result.RequeueAfter, workspacePoolAdmissionContentionRequeue)
	}

	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, request.NamespacedName, current); err != nil {
		t.Fatalf("get workspace after contention: %v", err)
	}
	if current.Spec.CoreAdmission != nil {
		t.Fatalf("contended admission minted marker: %#v", current.Spec.CoreAdmission)
	}
	condition := workspaceprovider.FindCondition(current.Status.Conditions, string(workspacev1alpha1.ConditionWorkspaceAdmitted))
	if condition == nil || condition.Status != metav1.ConditionTrue ||
		condition.Reason != string(workspacev1alpha1.ReasonReady) {
		t.Fatalf("contended admission condition = %#v, want pending Ready prepublication", condition)
	}
}

func TestExecutionWorkspacePoolCapacityDenialIsStableAndReleasesLease(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	provider := testGenericProvider("provider")
	provider.Status.ObservedGeneration = provider.Generation
	provider.Status.SupportedContracts = []string{workspacev1alpha1.ContractVersionV1}
	provider.Status.SupportedFeatures = []workspacev1alpha1.ExecutionWorkspaceFeature{
		workspacev1alpha1.WorkspaceFeatureExec,
		workspacev1alpha1.WorkspaceFeaturePools,
		workspacev1alpha1.WorkspaceFeatureReset,
		workspacev1alpha1.WorkspaceFeatureSuspend,
		workspacev1alpha1.WorkspaceFeatureTLS,
	}
	workspaceprovider.SetCondition(&provider.Status.Conditions, metav1.Condition{
		Type:               string(workspacev1alpha1.ConditionProviderReady),
		Status:             metav1.ConditionTrue,
		Reason:             string(workspacev1alpha1.ReasonReady),
		ObservedGeneration: provider.Generation,
	})
	pool := testGenericPool("default", "pool", provider.Name)
	pool.Spec.Capacity.MaxSize = 1
	pool.Status.ObservedGeneration = pool.Generation
	workspaceprovider.SetCondition(&pool.Status.Conditions, metav1.Condition{
		Type:               string(workspacev1alpha1.ConditionPoolReady),
		Status:             metav1.ConditionTrue,
		Reason:             string(workspacev1alpha1.ReasonReady),
		ObservedGeneration: pool.Generation,
	})
	workspaceprovider.SetCondition(&pool.Status.Conditions, metav1.Condition{
		Type:               string(workspacev1alpha1.ConditionPoolAdmitted),
		Status:             metav1.ConditionTrue,
		Reason:             string(workspacev1alpha1.ReasonReady),
		ObservedGeneration: pool.Generation,
	})
	class := testGenericClass(pool.Namespace, "class", provider.Name)
	class.Spec.ProviderRef = nil
	class.Spec.ParametersRef = nil
	class.Spec.PoolRef = &corev1.LocalObjectReference{Name: pool.Name}
	workspace := testBoundWorkspace(t, class.Namespace, "waiting-workspace", class.Name, provider.Name)
	workspace.Finalizers = []string{executionWorkspaceFinalizer}
	reserved := testBoundWorkspace(t, class.Namespace, "reserved-workspace", class.Name, provider.Name)
	mapper, parameters := preparePooledClassProfileForTest(t, provider, pool, class, workspace, reserved)
	class.Status.ObservedGeneration = class.Generation
	workspaceprovider.SetCondition(&class.Status.Conditions, metav1.Condition{
		Type:               string(workspacev1alpha1.ConditionClassReady),
		Status:             metav1.ConditionTrue,
		Reason:             string(workspacev1alpha1.ReasonReady),
		ObservedGeneration: class.Generation,
	})
	markWorkspaceAdmittedForPolicyReview(reserved, reserved.Generation)
	reserved.Spec.CoreAdmission.PoolBinding = &workspacev1alpha1.ImmutableObjectBinding{
		Name: pool.Name, UID: pool.UID, Generation: pool.Generation,
	}

	c := fake.NewClientBuilder().
		WithScheme(testWorkspaceScheme(t)).
		WithStatusSubresource(workspace, reserved).
		WithObjects(provider, pool, class, workspace, reserved, parameters).
		Build()
	reconciler := &ExecutionWorkspaceReconciler{Client: c, APIReader: c, RESTMapper: mapper}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("prepublish admission condition: %v", err)
	}
	result, err := reconciler.Reconcile(ctx, request)
	if err != nil {
		t.Fatalf("reconcile full pool admission: %v", err)
	}
	if result.RequeueAfter != workspaceRequeueInterval {
		t.Fatalf("capacity denial RequeueAfter = %s, want %s", result.RequeueAfter, workspaceRequeueInterval)
	}

	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, request.NamespacedName, current); err != nil {
		t.Fatalf("get capacity-denied workspace: %v", err)
	}
	if current.Spec.CoreAdmission != nil {
		t.Fatalf("full pool minted admission marker: %#v", current.Spec.CoreAdmission)
	}
	condition := workspaceprovider.FindCondition(current.Status.Conditions, string(workspacev1alpha1.ConditionWorkspaceAdmitted))
	if condition == nil || condition.Status != metav1.ConditionFalse ||
		condition.Reason != string(workspacev1alpha1.ReasonCapacityUnavailable) {
		t.Fatalf("capacity denial condition = %#v", condition)
	}
	deniedResourceVersion := current.ResourceVersion

	lease := &coordinationv1.Lease{}
	leaseKey := types.NamespacedName{Namespace: pool.Namespace, Name: workspacePoolAdmissionLeaseName(pool)}
	if err := c.Get(ctx, leaseKey, lease); err != nil {
		t.Fatalf("get released admission Lease: %v", err)
	}
	if lease.Spec.HolderIdentity != nil {
		t.Fatalf("capacity denial left Lease held by %q", *lease.Spec.HolderIdentity)
	}

	result, err = reconciler.Reconcile(ctx, request)
	if err != nil {
		t.Fatalf("reconcile stable capacity denial: %v", err)
	}
	if result.RequeueAfter <= 0 || result.RequeueAfter > workspaceRequeueInterval {
		t.Fatalf("stable denial RequeueAfter = %s, want a bounded delayed retry", result.RequeueAfter)
	}
	after := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, request.NamespacedName, after); err != nil {
		t.Fatalf("get workspace after stable denial: %v", err)
	}
	if after.ResourceVersion != deniedResourceVersion {
		t.Fatalf("stable capacity denial rewrote workspace: resourceVersion %s -> %s", deniedResourceVersion, after.ResourceVersion)
	}
}

func TestExecutionWorkspaceOwnerProjectionReadsFreshAdmissionStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := testWorkspaceScheme(t)
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
		Name: "task", Namespace: "default", UID: types.UID("task-uid"),
	}}
	workspace := testBoundWorkspace(t, task.Namespace, "workspace", "class", "provider")
	if err := controllerutil.SetControllerReference(task, workspace, scheme); err != nil {
		t.Fatalf("set workspace owner: %v", err)
	}
	workspace.Status.Conditions = []metav1.Condition{{
		Type:               string(workspacev1alpha1.ConditionWorkspaceAdmitted),
		Status:             metav1.ConditionFalse,
		Reason:             string(workspacev1alpha1.ReasonCapacityUnavailable),
		ObservedGeneration: workspace.Generation,
		LastTransitionTime: metav1.Now(),
	}}
	staleWorkspace := workspace.DeepCopy()
	staleWorkspace.Status.Conditions = []metav1.Condition{{
		Type:               string(workspacev1alpha1.ConditionWorkspaceAdmitted),
		Status:             metav1.ConditionTrue,
		Reason:             string(workspacev1alpha1.ReasonReady),
		ObservedGeneration: workspace.Generation + 1,
	}}
	baseClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(task, workspace).
		WithObjects(task, workspace).
		Build()
	staleClient := &staleExecutionWorkspaceGetClient{Client: baseClient, workspace: staleWorkspace}
	reconciler := &ExecutionWorkspaceReconciler{Client: staleClient, APIReader: baseClient}
	key := types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}
	if err := reconciler.projectLatestWorkspaceToOwner(ctx, key); err != nil {
		t.Fatalf("projectLatestWorkspaceToOwner: %v", err)
	}

	currentTask := &corev1alpha1.Task{}
	if err := baseClient.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, currentTask); err != nil {
		t.Fatalf("get projected task: %v", err)
	}
	if currentTask.Status.ExecutionWorkspace == nil {
		t.Fatal("task workspace projection is missing")
	}
	condition := workspaceprovider.FindCondition(
		currentTask.Status.ExecutionWorkspace.Conditions,
		string(workspacev1alpha1.ConditionWorkspaceAdmitted),
	)
	if condition == nil || condition.Status != metav1.ConditionFalse ||
		condition.Reason != string(workspacev1alpha1.ReasonCapacityUnavailable) {
		t.Fatalf("owner received stale admission projection: %#v", currentTask.Status.ExecutionWorkspace)
	}
}

type staleExecutionWorkspaceGetClient struct {
	client.Client
	workspace *workspacev1alpha1.ExecutionWorkspace
}

func (c *staleExecutionWorkspaceGetClient) Get(
	ctx context.Context,
	key client.ObjectKey,
	obj client.Object,
	opts ...client.GetOption,
) error {
	if workspace, ok := obj.(*workspacev1alpha1.ExecutionWorkspace); ok &&
		c.workspace != nil && key.Namespace == c.workspace.Namespace && key.Name == c.workspace.Name {
		c.workspace.DeepCopyInto(workspace)
		return nil
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func TestExecutionWorkspaceClassCleanupOnlyRemovesUnreferencedFinalizer(t *testing.T) {
	t.Parallel()
	class, _, _ := workspacePolicyReviewFixture(t)
	class.Finalizers = []string{executionWorkspaceClassFinalizer}
	deletionTimestamp := metav1.Now()
	class.DeletionTimestamp = &deletionTimestamp

	c := fake.NewClientBuilder().
		WithScheme(testWorkspaceScheme(t)).
		WithStatusSubresource(class).
		WithObjects(class).
		Build()
	reconciler := &ExecutionWorkspaceClassReconciler{Client: c, APIReader: c, CleanupOnly: true}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: class.Namespace, Name: class.Name}}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := &workspacev1alpha1.ExecutionWorkspaceClass{}
	err := c.Get(context.Background(), request.NamespacedName, got)
	if err == nil && controllerutil.ContainsFinalizer(got, executionWorkspaceClassFinalizer) {
		t.Fatal("cleanup-only reconciliation left the unreferenced class finalizer in place")
	}
}

func TestExecutionWorkspaceClassCleanupOnlySkipsActiveClass(t *testing.T) {
	t.Parallel()
	class, _, _ := workspacePolicyReviewFixture(t)
	class.Finalizers = []string{executionWorkspaceClassFinalizer}
	original := class.DeepCopy()

	c := fake.NewClientBuilder().
		WithScheme(testWorkspaceScheme(t)).
		WithStatusSubresource(class).
		WithObjects(class).
		Build()
	reconciler := &ExecutionWorkspaceClassReconciler{Client: c, APIReader: c, CleanupOnly: true}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: class.Namespace, Name: class.Name}}
	if result, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("Reconcile: %v", err)
	} else if result != (ctrl.Result{}) {
		t.Fatalf("cleanup-only active class result = %#v, want zero result", result)
	}

	got := &workspacev1alpha1.ExecutionWorkspaceClass{}
	if err := c.Get(context.Background(), request.NamespacedName, got); err != nil {
		t.Fatalf("get class: %v", err)
	}
	if got.Generation != original.Generation || got.Status.ObservedGeneration != original.Status.ObservedGeneration {
		t.Fatalf("cleanup-only reconciliation mutated active class: before=%#v after=%#v", original, got)
	}
}

func TestWorkspaceProjectionTrustedForMarkerBackedCleanup(t *testing.T) {
	t.Parallel()
	base := testBoundWorkspace(t, "default", "projection-workspace", "class", "provider")
	markWorkspaceAdmittedForPolicyReview(base, base.Generation)

	tests := []struct {
		name   string
		mutate func(*workspacev1alpha1.ExecutionWorkspace)
		want   bool
	}{
		{name: "current admission", want: true},
		{
			name: "stale normal work is blocked",
			mutate: func(workspace *workspacev1alpha1.ExecutionWorkspace) {
				workspace.Generation++
			},
		},
		{
			name: "stale delete intent remains projectable",
			mutate: func(workspace *workspacev1alpha1.ExecutionWorkspace) {
				workspace.Generation++
				workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredDeleted
			},
			want: true,
		},
		{
			name: "stale quarantine intent remains projectable",
			mutate: func(workspace *workspacev1alpha1.ExecutionWorkspace) {
				workspace.Generation++
				workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined
			},
			want: true,
		},
		{
			name: "deletion timestamp remains projectable",
			mutate: func(workspace *workspacev1alpha1.ExecutionWorkspace) {
				workspace.Generation++
				now := metav1.Now()
				workspace.DeletionTimestamp = &now
			},
			want: true,
		},
		{
			name: "unmarked maintenance receives sanitized projection",
			mutate: func(workspace *workspacev1alpha1.ExecutionWorkspace) {
				workspace.Spec.CoreAdmission = nil
				workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredDeleted
			},
			want: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workspace := base.DeepCopy()
			if test.mutate != nil {
				test.mutate(workspace)
			}
			if got := workspaceProjectionTrusted(workspace); got != test.want {
				t.Fatalf("workspaceProjectionTrusted() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestExecutionWorkspaceClassDeletionProtectionIncludesPendingWorkspace(t *testing.T) {
	t.Parallel()
	class, _, workspace := workspacePolicyReviewFixture(t)
	class.Finalizers = []string{executionWorkspaceClassFinalizer}
	deletionTimestamp := metav1.Now()
	class.DeletionTimestamp = &deletionTimestamp
	workspace.Spec.CoreAdmission = nil

	c := fake.NewClientBuilder().
		WithScheme(testWorkspaceScheme(t)).
		WithStatusSubresource(class, workspace).
		WithObjects(class, workspace).
		Build()
	reconciler := &ExecutionWorkspaceClassReconciler{Client: c, APIReader: c}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: class.Namespace, Name: class.Name}}
	result, err := reconciler.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got := &workspacev1alpha1.ExecutionWorkspaceClass{}
	if err := c.Get(context.Background(), request.NamespacedName, got); err != nil {
		t.Fatalf("get protected class: %v", err)
	}
	if !controllerutil.ContainsFinalizer(got, executionWorkspaceClassFinalizer) {
		t.Fatal("class finalizer was removed while a pending workspace remained bound")
	}
	if result.RequeueAfter != classReadinessRequeue {
		t.Fatalf("RequeueAfter = %s, want %s", result.RequeueAfter, classReadinessRequeue)
	}
}
