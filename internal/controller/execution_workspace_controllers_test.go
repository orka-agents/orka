package controller

import (
	"context"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
	"github.com/orka-agents/orka/pkg/workspaceprovider"
)

func TestExecutionWorkspaceProviderReconcilerEvaluatesLifecycleAndHeartbeat(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	heartbeat := metav1.NewTime(now.Add(-time.Minute))
	provider := testGenericProvider("provider")
	provider.Status.SupportedContracts = []string{workspacev1alpha1.ContractVersionV1}
	provider.Status.ObservedGeneration = provider.Generation
	provider.Status.LastHeartbeat = &heartbeat

	c := fake.NewClientBuilder().WithScheme(testWorkspaceScheme(t)).WithStatusSubresource(provider).WithObjects(provider).Build()
	reconciler := &ExecutionWorkspaceProviderReconciler{
		Client: c, RESTMapper: testProviderParameterMapper(apimeta.RESTScopeRoot),
		Now: func() time.Time { return now },
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: provider.Name}}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("add provider finalizer: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("evaluate provider: %v", err)
	}
	got := &workspacev1alpha1.ExecutionWorkspaceProvider{}
	if err := c.Get(ctx, request.NamespacedName, got); err != nil {
		t.Fatalf("get provider: %v", err)
	}
	if !workspaceprovider.ConditionIsTrue(got.Status.Conditions, string(workspacev1alpha1.ConditionProviderReady)) {
		t.Fatalf("provider conditions = %#v, want Ready=True", got.Status.Conditions)
	}

	before := got.DeepCopy()
	got.Spec.LifecycleState = workspacev1alpha1.ExecutionWorkspaceProviderDraining
	if err := c.Patch(ctx, got, client.MergeFrom(before)); err != nil {
		t.Fatalf("drain provider: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("evaluate draining provider: %v", err)
	}
	if err := c.Get(ctx, request.NamespacedName, got); err != nil {
		t.Fatalf("get draining provider: %v", err)
	}
	if !workspaceprovider.ConditionIsFalse(got.Status.Conditions, string(workspacev1alpha1.ConditionProviderReady)) {
		t.Fatalf("draining provider conditions = %#v, want Ready=False", got.Status.Conditions)
	}

	before = got.DeepCopy()
	got.Spec.LifecycleState = workspacev1alpha1.ExecutionWorkspaceProviderActive
	if err := c.Patch(ctx, got, client.MergeFrom(before)); err != nil {
		t.Fatalf("reactivate provider: %v", err)
	}
	future := metav1.NewTime(now.Add(10 * time.Minute))
	got.Status.LastHeartbeat = &future
	if err := c.Status().Update(ctx, got); err != nil {
		t.Fatalf("set future heartbeat: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("evaluate future heartbeat: %v", err)
	}
	if err := c.Get(ctx, request.NamespacedName, got); err != nil {
		t.Fatalf("get future-heartbeat provider: %v", err)
	}
	if !workspaceprovider.ConditionIsFalse(
		got.Status.Conditions, string(workspacev1alpha1.ConditionProviderHeartbeat),
	) {
		t.Fatalf("future heartbeat conditions = %#v, want HeartbeatFresh=False", got.Status.Conditions)
	}
}

func TestExecutionWorkspaceProviderReconcilerRequiresAdapterIdentity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	heartbeat := metav1.NewTime(now)
	provider := testGenericProvider("provider-missing-adapter")
	provider.Status.Adapter = nil
	provider.Status.SupportedContracts = []string{workspacev1alpha1.ContractVersionV1}
	provider.Status.ObservedGeneration = provider.Generation
	provider.Status.LastHeartbeat = &heartbeat
	c := fake.NewClientBuilder().WithScheme(testWorkspaceScheme(t)).
		WithStatusSubresource(provider).
		WithObjects(provider).
		Build()
	reconciler := &ExecutionWorkspaceProviderReconciler{
		Client: c, RESTMapper: testProviderParameterMapper(apimeta.RESTScopeRoot),
		Now: func() time.Time { return now },
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: provider.Name}}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("add provider finalizer: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("evaluate provider: %v", err)
	}
	got := &workspacev1alpha1.ExecutionWorkspaceProvider{}
	if err := c.Get(ctx, request.NamespacedName, got); err != nil {
		t.Fatalf("get provider: %v", err)
	}
	condition := workspaceprovider.FindCondition(got.Status.Conditions, string(workspacev1alpha1.ConditionProviderReady))
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "AdapterIdentityMissing" {
		t.Fatalf("provider condition = %#v", condition)
	}
}

func TestExecutionWorkspaceProviderReconcilerRequiresCoreV1Contract(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	heartbeat := metav1.NewTime(now)
	provider := testGenericProvider("provider-v2-only")
	provider.Spec.RequiredContracts = []string{"workspace.orka.ai/v2"}
	provider.Status.SupportedContracts = []string{"workspace.orka.ai/v2"}
	provider.Status.ObservedGeneration = provider.Generation
	provider.Status.LastHeartbeat = &heartbeat
	c := fake.NewClientBuilder().WithScheme(testWorkspaceScheme(t)).
		WithStatusSubresource(provider).
		WithObjects(provider).
		Build()
	reconciler := &ExecutionWorkspaceProviderReconciler{
		Client: c, RESTMapper: testProviderParameterMapper(apimeta.RESTScopeRoot),
		Now: func() time.Time { return now },
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: provider.Name}}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("add provider finalizer: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("evaluate provider: %v", err)
	}
	got := &workspacev1alpha1.ExecutionWorkspaceProvider{}
	if err := c.Get(ctx, request.NamespacedName, got); err != nil {
		t.Fatalf("get provider: %v", err)
	}
	condition := workspaceprovider.FindCondition(got.Status.Conditions, string(workspacev1alpha1.ConditionProviderReady))
	if condition == nil || condition.Status != metav1.ConditionFalse ||
		condition.Reason != string(workspacev1alpha1.ReasonIncompatibleContract) {
		t.Fatalf("provider condition = %#v", condition)
	}
}

func TestExecutionWorkspaceProviderReconcilerRejectsNamespacedParameters(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	heartbeat := metav1.NewTime(now)
	provider := testGenericProvider("provider-namespaced-parameters")
	provider.Status.SupportedContracts = []string{workspacev1alpha1.ContractVersionV1}
	provider.Status.ObservedGeneration = provider.Generation
	provider.Status.LastHeartbeat = &heartbeat
	c := fake.NewClientBuilder().WithScheme(testWorkspaceScheme(t)).
		WithStatusSubresource(provider).
		WithObjects(provider).
		Build()
	reconciler := &ExecutionWorkspaceProviderReconciler{
		Client: c, RESTMapper: testProviderParameterMapper(apimeta.RESTScopeNamespace),
		Now: func() time.Time { return now },
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: provider.Name}}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("add provider finalizer: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("evaluate provider: %v", err)
	}
	got := &workspacev1alpha1.ExecutionWorkspaceProvider{}
	if err := c.Get(ctx, request.NamespacedName, got); err != nil {
		t.Fatalf("get provider: %v", err)
	}
	condition := workspaceprovider.FindCondition(got.Status.Conditions, string(workspacev1alpha1.ConditionProviderReady))
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != reasonParametersScopeInvalid {
		t.Fatalf("provider condition = %#v", condition)
	}
}

func TestExecutionWorkspaceProviderDeletionBlockedByReferences(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := metav1.Now()
	provider := testGenericProvider("provider-delete")
	provider.Finalizers = []string{executionWorkspaceProviderFinalizer}
	provider.DeletionTimestamp = &now
	pool := testGenericPool("default", "pool", provider.Name)
	c := fake.NewClientBuilder().WithScheme(testWorkspaceScheme(t)).WithStatusSubresource(provider).WithObjects(provider, pool).Build()
	reconciler := &ExecutionWorkspaceProviderReconciler{Client: c}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: provider.Name}}); err != nil {
		t.Fatalf("reconcile deleting provider: %v", err)
	}
	got := &workspacev1alpha1.ExecutionWorkspaceProvider{}
	if err := c.Get(ctx, types.NamespacedName{Name: provider.Name}, got); err != nil {
		t.Fatalf("get provider: %v", err)
	}
	if len(got.Finalizers) == 0 {
		t.Fatal("provider finalizer removed while pool reference remains")
	}
}

func TestExecutionWorkspaceClassReconcilerResolvesReadyProvider(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "workspace-class"}}
	provider := testGenericProvider("provider-class")
	provider.Status.SupportedFeatures = []workspacev1alpha1.ExecutionWorkspaceFeature{
		workspacev1alpha1.WorkspaceFeatureExec,
		workspacev1alpha1.WorkspaceFeatureReset,
		workspacev1alpha1.WorkspaceFeatureSuspend,
		workspacev1alpha1.WorkspaceFeatureTLS,
	}
	provider.Status.Conditions = []metav1.Condition{{Type: string(workspacev1alpha1.ConditionProviderReady), Status: metav1.ConditionTrue, Reason: "Ready"}}
	class := testGenericClass(ns.Name, "class", provider.Name)
	mapper, parameters := testParameterMapping(ns.Name, class.Spec.ParametersRef)
	c := fake.NewClientBuilder().WithScheme(testWorkspaceScheme(t)).
		WithStatusSubresource(class).
		WithObjects(ns, provider, class, parameters).
		Build()
	reconciler := &ExecutionWorkspaceClassReconciler{Client: c, RESTMapper: mapper}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: class.Namespace, Name: class.Name}}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("reconcile class: %v", err)
	}
	got := &workspacev1alpha1.ExecutionWorkspaceClass{}
	if err := c.Get(ctx, request.NamespacedName, got); err != nil {
		t.Fatalf("get class: %v", err)
	}
	if got.Status.ProviderRef == nil || got.Status.ProviderRef.Name != provider.Name ||
		!workspaceprovider.ConditionIsTrue(got.Status.Conditions, string(workspacev1alpha1.ConditionClassReady)) ||
		got.Status.ProfileHash == "" {
		t.Fatalf("class status = %#v", got.Status)
	}
	pinnedHash := got.Status.ProfileHash
	parameters.Object["spec"] = map[string]any{"image": "changed"}
	parameters.SetGeneration(2)
	if err := c.Update(ctx, parameters); err != nil {
		t.Fatalf("update provider parameters: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("reconcile drifted class: %v", err)
	}
	if err := c.Get(ctx, request.NamespacedName, got); err != nil {
		t.Fatalf("get drifted class: %v", err)
	}
	condition := workspaceprovider.FindCondition(got.Status.Conditions, string(workspacev1alpha1.ConditionClassReady))
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != reasonProfileDrift {
		t.Fatalf("drifted class condition = %#v", condition)
	}
	if got.Status.ProfileHash != pinnedHash {
		t.Fatalf("profile hash changed from %q to %q", pinnedHash, got.Status.ProfileHash)
	}
}

func TestExecutionWorkspaceClassReconcilerRequiresTLS(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "workspace-class-tls"}}
	provider := testGenericProvider("provider-class-tls")
	provider.Status.SupportedFeatures = []workspacev1alpha1.ExecutionWorkspaceFeature{
		workspacev1alpha1.WorkspaceFeatureExec,
		workspacev1alpha1.WorkspaceFeatureReset,
		workspacev1alpha1.WorkspaceFeatureSuspend,
	}
	provider.Status.Conditions = []metav1.Condition{{
		Type: string(workspacev1alpha1.ConditionProviderReady), Status: metav1.ConditionTrue, Reason: "Ready",
	}}
	class := testGenericClass(ns.Name, "class", provider.Name)
	mapper, parameters := testParameterMapping(ns.Name, class.Spec.ParametersRef)
	c := fake.NewClientBuilder().WithScheme(testWorkspaceScheme(t)).
		WithStatusSubresource(class).
		WithObjects(ns, provider, class, parameters).
		Build()
	reconciler := &ExecutionWorkspaceClassReconciler{Client: c, RESTMapper: mapper}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: class.Namespace, Name: class.Name}}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("reconcile class without TLS: %v", err)
	}
	got := &workspacev1alpha1.ExecutionWorkspaceClass{}
	if err := c.Get(ctx, request.NamespacedName, got); err != nil {
		t.Fatalf("get class: %v", err)
	}
	condition := workspaceprovider.FindCondition(got.Status.Conditions, string(workspacev1alpha1.ConditionClassReady))
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != reasonRequiredFeatures {
		t.Fatalf("class condition = %#v", condition)
	}
}

func TestExecutionWorkspaceClassReconcilerRequiresReset(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "workspace-class-reset"}}
	provider := testGenericProvider("provider-class-reset")
	provider.Status.SupportedFeatures = []workspacev1alpha1.ExecutionWorkspaceFeature{
		workspacev1alpha1.WorkspaceFeatureExec,
		workspacev1alpha1.WorkspaceFeatureSuspend,
		workspacev1alpha1.WorkspaceFeatureTLS,
	}
	provider.Status.Conditions = []metav1.Condition{{
		Type: string(workspacev1alpha1.ConditionProviderReady), Status: metav1.ConditionTrue, Reason: "Ready",
	}}
	class := testGenericClass(ns.Name, "class", provider.Name)
	mapper, parameters := testParameterMapping(ns.Name, class.Spec.ParametersRef)
	c := fake.NewClientBuilder().WithScheme(testWorkspaceScheme(t)).
		WithStatusSubresource(class).
		WithObjects(ns, provider, class, parameters).
		Build()
	reconciler := &ExecutionWorkspaceClassReconciler{Client: c, RESTMapper: mapper}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: class.Namespace, Name: class.Name}}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("reconcile class without reset: %v", err)
	}
	got := &workspacev1alpha1.ExecutionWorkspaceClass{}
	if err := c.Get(ctx, request.NamespacedName, got); err != nil {
		t.Fatalf("get class: %v", err)
	}
	condition := workspaceprovider.FindCondition(got.Status.Conditions, string(workspacev1alpha1.ConditionClassReady))
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != reasonRequiredFeatures {
		t.Fatalf("class condition = %#v", condition)
	}
}

func TestExecutionWorkspaceClassReconcilerFailsClosedOnInvalidNamespacePolicy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "workspace-policy"}}
	provider := testGenericProvider("provider-policy")
	provider.Status.SupportedFeatures = []workspacev1alpha1.ExecutionWorkspaceFeature{
		workspacev1alpha1.WorkspaceFeatureExec,
		workspacev1alpha1.WorkspaceFeatureReset,
		workspacev1alpha1.WorkspaceFeatureSuspend,
		workspacev1alpha1.WorkspaceFeatureTLS,
	}
	provider.Status.Conditions = []metav1.Condition{{
		Type: string(workspacev1alpha1.ConditionProviderReady), Status: metav1.ConditionTrue, Reason: "Ready",
	}}
	provider.Spec.UsagePolicy = &workspacev1alpha1.ExecutionWorkspaceProviderUsagePolicy{
		AllowedNamespaceSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
			Key: "team", Operator: metav1.LabelSelectorOperator("Invalid"), Values: []string{"a"},
		}}},
	}
	class := testGenericClass(ns.Name, "class", provider.Name)
	mapper, parameters := testParameterMapping(ns.Name, class.Spec.ParametersRef)
	c := fake.NewClientBuilder().WithScheme(testWorkspaceScheme(t)).
		WithStatusSubresource(class).
		WithObjects(ns, provider, class, parameters).
		Build()
	reconciler := &ExecutionWorkspaceClassReconciler{Client: c, RESTMapper: mapper}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: class.Namespace, Name: class.Name}}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("reconcile invalid namespace policy: %v", err)
	}
	got := &workspacev1alpha1.ExecutionWorkspaceClass{}
	if err := c.Get(ctx, request.NamespacedName, got); err != nil {
		t.Fatalf("get class: %v", err)
	}
	condition := workspaceprovider.FindCondition(got.Status.Conditions, string(workspacev1alpha1.ConditionClassReady))
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "NamespacePolicyInvalid" {
		t.Fatalf("class condition = %#v", condition)
	}
}

func TestExecutionWorkspaceClassReconcilerRequiresReadyPool(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "workspace-pool-class"}}
	provider := testGenericProvider("provider-pool-class")
	provider.Status.SupportedFeatures = []workspacev1alpha1.ExecutionWorkspaceFeature{
		workspacev1alpha1.WorkspaceFeatureExec,
		workspacev1alpha1.WorkspaceFeaturePools,
		workspacev1alpha1.WorkspaceFeatureReset,
		workspacev1alpha1.WorkspaceFeatureSuspend,
		workspacev1alpha1.WorkspaceFeatureTLS,
	}
	provider.Status.Conditions = []metav1.Condition{{
		Type: string(workspacev1alpha1.ConditionProviderReady), Status: metav1.ConditionTrue, Reason: "Ready",
	}}
	pool := testGenericPool(ns.Name, "pool", provider.Name)
	pool.Status.ObservedGeneration = pool.Generation
	pool.Status.Conditions = []metav1.Condition{{
		Type: string(workspacev1alpha1.ConditionPoolReady), Status: metav1.ConditionFalse, Reason: "BackendFailed",
	}}
	class := testGenericClass(ns.Name, "pooled-class", provider.Name)
	class.Spec.ProviderRef = nil
	class.Spec.ParametersRef = nil
	class.Spec.PoolRef = &corev1.LocalObjectReference{Name: pool.Name}
	mapper, parameters := testParameterMapping(ns.Name, &pool.Spec.ParametersRef)
	c := fake.NewClientBuilder().WithScheme(testWorkspaceScheme(t)).
		WithStatusSubresource(class, pool).
		WithObjects(ns, provider, pool, class, parameters).
		Build()
	reconciler := &ExecutionWorkspaceClassReconciler{Client: c, RESTMapper: mapper}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: class.Namespace, Name: class.Name}}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("reconcile class with failed pool: %v", err)
	}
	got := &workspacev1alpha1.ExecutionWorkspaceClass{}
	if err := c.Get(ctx, request.NamespacedName, got); err != nil {
		t.Fatalf("get class: %v", err)
	}
	if !workspaceprovider.ConditionIsFalse(got.Status.Conditions, string(workspacev1alpha1.ConditionClassReady)) {
		t.Fatalf("class conditions = %#v, want Ready=False", got.Status.Conditions)
	}

	if err := c.Get(ctx, types.NamespacedName{Namespace: pool.Namespace, Name: pool.Name}, pool); err != nil {
		t.Fatalf("get pool: %v", err)
	}
	pool.Status.Conditions[0].Status = metav1.ConditionTrue
	pool.Status.Conditions[0].Reason = "Ready"
	if err := c.Status().Update(ctx, pool); err != nil {
		t.Fatalf("mark pool ready: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("reconcile class with ready pool: %v", err)
	}
	if err := c.Get(ctx, request.NamespacedName, got); err != nil {
		t.Fatalf("get ready class: %v", err)
	}
	if !workspaceprovider.ConditionIsTrue(got.Status.Conditions, string(workspacev1alpha1.ConditionClassReady)) {
		t.Fatalf("class conditions = %#v, want Ready=True", got.Status.Conditions)
	}
	pinnedHash := got.Status.ProfileHash
	parameters.Object["spec"] = map[string]any{"profile": "changed"}
	parameters.SetGeneration(2)
	if err := c.Update(ctx, parameters); err != nil {
		t.Fatalf("update pool parameters: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("reconcile class with drifted pool parameters: %v", err)
	}
	if err := c.Get(ctx, request.NamespacedName, got); err != nil {
		t.Fatalf("get drifted pooled class: %v", err)
	}
	condition := workspaceprovider.FindCondition(got.Status.Conditions, string(workspacev1alpha1.ConditionClassReady))
	if condition == nil || condition.Reason != reasonProfileDrift || got.Status.ProfileHash != pinnedHash {
		t.Fatalf("drifted pooled class status = %#v", got.Status)
	}
}

func TestWorkspaceAttachmentManagerRotatesAndRevokesCredentials(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := testWorkspaceScheme(t)
	workspace := testBoundWorkspace(t, "default", "workspace", "class", "provider")
	workspace.Status.State = workspacev1alpha1.ExecutionWorkspaceStateReady
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: workspace.Namespace, UID: types.UID("task-uid")}}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(workspace).
		WithObjects(workspace, task).
		Build()
	manager := WorkspaceAttachmentManager{Client: c, LeaseTTL: time.Minute, Now: func() time.Time {
		return time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	}}
	result, err := manager.Attach(ctx, workspace, task)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if result.Epoch != 1 || result.AttachmentRef.Name == "" {
		t.Fatalf("attachment result = %#v", result)
	}
	secret := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: result.AttachmentRef.Name}, secret); err != nil {
		t.Fatalf("get attachment Secret: %v", err)
	}
	if len(secret.Data[workspaceAttachmentTokenKey]) != 32 {
		t.Fatalf("token bytes = %d, want 32", len(secret.Data[workspaceAttachmentTokenKey]))
	}
	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, current); err != nil {
		t.Fatalf("get attached workspace: %v", err)
	}
	if current.Spec.Attachment == nil || current.Spec.Attachment.TokenSHA256 == string(secret.Data[workspaceAttachmentTokenKey]) {
		t.Fatalf("workspace attachment = %#v, raw token must not be stored", current.Spec.Attachment)
	}
	lease := &coordinationv1.Lease{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: attachmentLeaseName(workspace.Name)}, lease); err != nil {
		t.Fatalf("get attachment Lease: %v", err)
	}

	if err := manager.BeginRevocation(ctx, current, result.Epoch); err != nil {
		t.Fatalf("BeginRevocation: %v", err)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, current); err != nil {
		t.Fatalf("refetch workspace after begin revocation: %v", err)
	}
	current.Status.AttachedEpoch = result.Epoch
	current.Status.Conditions = []metav1.Condition{{Type: string(workspacev1alpha1.ConditionWorkspaceAttached), Status: metav1.ConditionTrue, Reason: "Attached"}}
	if err := c.Status().Update(ctx, current); err != nil {
		t.Fatalf("set attached status: %v", err)
	}
	if err := manager.FinalizeRevocation(ctx, current, result.Epoch, result.AttachmentRef.Name); err == nil {
		t.Fatal("FinalizeRevocation succeeded before provider revocation")
	}
	current.Status.AttachedEpoch = 0
	current.Status.Conditions = []metav1.Condition{{Type: string(workspacev1alpha1.ConditionWorkspaceAttached), Status: metav1.ConditionFalse, Reason: "Revoked"}}
	if err := c.Status().Update(ctx, current); err != nil {
		t.Fatalf("set revoked status: %v", err)
	}
	if err := manager.FinalizeRevocation(ctx, current, result.Epoch, result.AttachmentRef.Name); err != nil {
		t.Fatalf("FinalizeRevocation: %v", err)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: result.AttachmentRef.Name}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatalf("attachment Secret still exists: %v", err)
	}
}

func TestWorkspaceClassAuthorizerUsesUseVerb(t *testing.T) {
	t.Parallel()
	scheme := testWorkspaceScheme(t)
	base := fake.NewClientBuilder().WithScheme(scheme).Build()
	capture := &subjectAccessReviewClient{Client: base, allowed: true}
	authorizer := WorkspaceClassAuthorizer{Client: capture}
	err := authorizer.Authorize(context.Background(), defaultNS, "coding-v1", authenticationv1.UserInfo{
		Username: "alice",
		Groups:   []string{"developers"},
	})
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if capture.review == nil || capture.review.Spec.ResourceAttributes == nil {
		t.Fatal("SubjectAccessReview was not captured")
	}
	attrs := capture.review.Spec.ResourceAttributes
	if attrs.Verb != "use" || attrs.Group != "workspace.orka.ai" || attrs.Resource != "executionworkspaceclasses" || attrs.Name != "coding-v1" || attrs.Namespace != defaultNS {
		t.Fatalf("resource attributes = %#v", attrs)
	}

	capture.allowed = false
	capture.reason = "policy denied"
	if err := authorizer.Authorize(
		context.Background(),
		defaultNS,
		"coding-v1",
		authenticationv1.UserInfo{Username: "alice"},
	); err == nil {
		t.Fatal("Authorize succeeded for denied review")
	}
}

func TestFakeWorkspaceProviderReconcilesProviderPoolAndWorkspace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := testWorkspaceScheme(t)
	provider := testGenericProvider("fake-provider")
	class := testGenericClass("default", "class", provider.Name)
	pool := testGenericPool("default", "pool", provider.Name)
	class.Spec.ProviderRef = nil
	class.Spec.ParametersRef = nil
	class.Spec.PoolRef = &corev1.LocalObjectReference{Name: pool.Name}
	workspace := testBoundWorkspace(t, "default", "workspace", class.Name, provider.Name)
	workspace.Spec.Attachment = workspaceAttachmentForTest(
		"attachment", 3, metav1.NewTime(time.Now().Add(time.Minute)),
	)
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(provider, pool, workspace).
		WithObjects(provider, class, pool, workspace).
		Build()
	providerReconciler := &FakeExecutionWorkspaceProviderReconciler{Client: c}
	if _, err := providerReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: provider.Name}}); err != nil {
		t.Fatalf("reconcile fake provider: %v", err)
	}
	workspaceReconciler := &FakeExecutionWorkspaceReconciler{Client: c}
	if _, err := workspaceReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}}); err != nil {
		t.Fatalf("reconcile fake workspace: %v", err)
	}
	poolReconciler := &FakeExecutionWorkspacePoolReconciler{Client: c}
	if _, err := poolReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: pool.Namespace, Name: pool.Name}}); err != nil {
		t.Fatalf("reconcile fake pool: %v", err)
	}
	gotWorkspace := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, gotWorkspace); err != nil {
		t.Fatalf("get fake workspace: %v", err)
	}
	if gotWorkspace.Status.State != workspacev1alpha1.ExecutionWorkspaceStateAttached || gotWorkspace.Status.AttachedEpoch != 3 {
		t.Fatalf("fake workspace status = %#v", gotWorkspace.Status)
	}
	gotPool := &workspacev1alpha1.ExecutionWorkspacePool{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: pool.Namespace, Name: pool.Name}, gotPool); err != nil {
		t.Fatalf("get fake pool: %v", err)
	}
	if gotPool.Status.Allocated != 1 || gotPool.Status.Total < 1 {
		t.Fatalf("fake pool status = %#v", gotPool.Status)
	}
}

type subjectAccessReviewClient struct {
	client.Client
	allowed bool
	reason  string
	review  *authorizationv1.SubjectAccessReview
}

func (c *subjectAccessReviewClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	review, ok := obj.(*authorizationv1.SubjectAccessReview)
	if !ok {
		return c.Client.Create(ctx, obj, opts...)
	}
	c.review = review.DeepCopy()
	review.Status.Allowed = c.allowed
	review.Status.Reason = c.reason
	return nil
}

func workspaceAttachmentForTest(
	name string,
	epoch int64,
	expiresAt metav1.Time,
) *workspacev1alpha1.ExecutionWorkspaceAttachment {
	attachment := &workspacev1alpha1.ExecutionWorkspaceAttachment{
		TaskRef:     workspacev1alpha1.ObjectIdentityReference{Name: "task", UID: types.UID("task-uid")},
		Epoch:       epoch,
		TokenSHA256: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		ExpiresAt:   expiresAt,
	}
	attachment.TokenSecretRef.Name = name
	return attachment
}

func testProviderParameterMapper(scope apimeta.RESTScope) apimeta.RESTMapper {
	groupVersion := schema.GroupVersion{Group: "fake.workspace.orka.ai", Version: "v1"}
	mapper := apimeta.NewDefaultRESTMapper([]schema.GroupVersion{groupVersion})
	mapper.Add(groupVersion.WithKind("FakeProviderConfig"), scope)
	return mapper
}

func testParameterMapping(
	namespace string,
	ref *workspacev1alpha1.TypedObjectReference,
) (apimeta.RESTMapper, *unstructured.Unstructured) {
	groupVersion := schema.GroupVersion{Group: ref.Group, Version: "v1"}
	kind := schema.GroupVersionKind{Group: ref.Group, Version: groupVersion.Version, Kind: ref.Kind}
	mapper := apimeta.NewDefaultRESTMapper([]schema.GroupVersion{groupVersion})
	mapper.Add(kind, apimeta.RESTScopeNamespace)
	parameters := &unstructured.Unstructured{}
	parameters.SetGroupVersionKind(kind)
	parameters.SetNamespace(namespace)
	parameters.SetName(ref.Name)
	parameters.SetUID(types.UID(ref.Name + "-uid"))
	parameters.SetGeneration(1)
	parameters.Object["spec"] = map[string]any{"profile": "initial"}
	return mapper, parameters
}

func testWorkspaceScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme,
		corev1alpha1.AddToScheme,
		workspacev1alpha1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("add scheme: %v", err)
		}
	}
	return scheme
}

func testGenericProvider(name string) *workspacev1alpha1.ExecutionWorkspaceProvider {
	return &workspacev1alpha1.ExecutionWorkspaceProvider{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(name + "-uid"), Generation: 1},
		Spec: workspacev1alpha1.ExecutionWorkspaceProviderSpec{
			ControllerName:    FakeWorkspaceControllerName,
			ParametersRef:     workspacev1alpha1.TypedObjectReference{Group: "fake.workspace.orka.ai", Kind: "FakeProviderConfig", Name: "default"},
			LifecycleState:    workspacev1alpha1.ExecutionWorkspaceProviderActive,
			RequiredContracts: []string{workspacev1alpha1.ContractVersionV1},
		},
		Status: workspacev1alpha1.ExecutionWorkspaceProviderStatus{
			Adapter: &workspacev1alpha1.ExecutionWorkspaceAdapterStatus{Version: "1.0.0"},
		},
	}
}

func testGenericPool(namespace, name, provider string) *workspacev1alpha1.ExecutionWorkspacePool {
	return &workspacev1alpha1.ExecutionWorkspacePool{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: types.UID(name + "-uid"), Generation: 1},
		Spec: workspacev1alpha1.ExecutionWorkspacePoolSpec{
			ProviderRef:   workspacev1alpha1.ClusterObjectReference{Name: provider},
			ParametersRef: workspacev1alpha1.TypedObjectReference{Group: "fake.workspace.orka.ai", Kind: "FakePoolParameters", Name: "default"},
			Capacity:      workspacev1alpha1.ExecutionWorkspacePoolCapacity{MinReady: 2, MaxSize: 10},
		},
	}
}

func testGenericClass(namespace, name, provider string) *workspacev1alpha1.ExecutionWorkspaceClass {
	return &workspacev1alpha1.ExecutionWorkspaceClass{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: types.UID(name + "-uid"), Generation: 1},
		Spec: workspacev1alpha1.ExecutionWorkspaceClassSpec{
			ProviderRef:        &workspacev1alpha1.ClusterObjectReference{Name: provider},
			ParametersRef:      &workspacev1alpha1.TypedObjectReference{Group: "fake.workspace.orka.ai", Kind: "FakeWorkspaceProfile", Name: "default"},
			Mode:               workspacev1alpha1.ExecutionWorkspaceModeInteractive,
			RequiredFeatures:   []workspacev1alpha1.ExecutionWorkspaceFeature{workspacev1alpha1.WorkspaceFeatureExec},
			AllowedReuseScopes: []workspacev1alpha1.WorkspaceReuseScope{workspacev1alpha1.WorkspaceReuseScopeNone, workspacev1alpha1.WorkspaceReuseScopeSession},
			Lifecycle:          validWorkspaceLifecycle(),
		},
	}
}

func testBoundWorkspace(t *testing.T, namespace, name, className, providerName string) *workspacev1alpha1.ExecutionWorkspace {
	t.Helper()
	class := testGenericClass(namespace, className, providerName)
	hash, err := workspaceprovider.ClassProfileHash(class.Spec)
	if err != nil {
		t.Fatalf("hash class: %v", err)
	}
	return &workspacev1alpha1.ExecutionWorkspace{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: types.UID(name + "-uid"), Generation: 1},
		Spec: workspacev1alpha1.ExecutionWorkspaceSpec{
			Mode:            workspacev1alpha1.ExecutionWorkspaceModeInteractive,
			ClassBinding:    workspacev1alpha1.ImmutableObjectBinding{Name: className, UID: types.UID(className + "-uid"), Generation: 1, ProfileHash: hash},
			ProviderBinding: workspacev1alpha1.ImmutableObjectBinding{Name: providerName, UID: types.UID(providerName + "-uid"), Generation: 1},
			Slot:            "default",
			DesiredState:    workspacev1alpha1.ExecutionWorkspaceDesiredReady,
			Lifecycle:       validWorkspaceLifecycle(),
		},
	}
}
