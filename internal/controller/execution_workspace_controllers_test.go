package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
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

	acpworkspacev1alpha1 "github.com/orka-agents/orka/api/acp.workspace/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
	"github.com/orka-agents/orka/pkg/workspaceprovider"
)

const testSubstrateTemplateName = "substrate-template"

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
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != reasonNamespacePolicyInvalid {
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
	markWorkspaceAdmittedForPolicyReview(workspace, workspace.Generation)
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: workspace.Namespace, UID: types.UID("task-uid")}}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(workspace).
		WithObjects(workspace, task).
		Build()
	manager := WorkspaceAttachmentManager{Client: c, LeaseTTL: time.Minute, Now: func() time.Time {
		return time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	}}
	result, err := manager.Attach(ctx, workspace, task, nil)
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
	suspended := testBoundWorkspace(t, "default", "suspended-workspace", class.Name, provider.Name)
	suspended.Status.State = workspacev1alpha1.ExecutionWorkspaceStateSuspended
	mapper, parameters := preparePooledClassProfileForTest(t, provider, pool, class, workspace, suspended)
	markWorkspaceAdmittedForPolicyReview(workspace, workspace.Generation)
	workspace.Spec.CoreAdmission.PoolBinding = &workspacev1alpha1.ImmutableObjectBinding{
		Name: pool.Name, UID: pool.UID, Generation: pool.Generation,
	}
	markWorkspaceAdmittedForPolicyReview(suspended, suspended.Generation)
	suspended.Spec.CoreAdmission.PoolBinding = &workspacev1alpha1.ImmutableObjectBinding{
		Name: pool.Name, UID: pool.UID, Generation: pool.Generation,
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(provider, pool, workspace, suspended).
		WithObjects(provider, class, pool, workspace, suspended, parameters).
		Build()
	providerReconciler := &FakeExecutionWorkspaceProviderReconciler{Client: c}
	if _, err := providerReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: provider.Name}}); err != nil {
		t.Fatalf("reconcile fake provider: %v", err)
	}
	workspaceReconciler := &FakeExecutionWorkspaceReconciler{Client: c, APIReader: c, RESTMapper: mapper}
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
	if gotPool.Status.Allocated != 1 || gotPool.Status.Suspended != 1 || gotPool.Status.Total < 2 {
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

func testGenericPool(namespace, _ string, provider string) *workspacev1alpha1.ExecutionWorkspacePool {
	const name = "pool"

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

func preparePooledClassProfileForTest(
	t *testing.T,
	provider *workspacev1alpha1.ExecutionWorkspaceProvider,
	pool *workspacev1alpha1.ExecutionWorkspacePool,
	class *workspacev1alpha1.ExecutionWorkspaceClass,
	workspaces ...*workspacev1alpha1.ExecutionWorkspace,
) (apimeta.RESTMapper, *unstructured.Unstructured) {
	t.Helper()
	mapper, parameters := testParameterMapping(pool.Namespace, &pool.Spec.ParametersRef)
	c := fake.NewClientBuilder().
		WithScheme(testWorkspaceScheme(t)).
		WithObjects(provider, pool, parameters).
		Build()
	reconciler := &ExecutionWorkspaceClassReconciler{Client: c, APIReader: c, RESTMapper: mapper}
	profileHash, err := reconciler.resolvedClassProfileHash(context.Background(), class)
	if err != nil {
		t.Fatalf("resolve pooled class profile: %v", err)
	}
	class.Status.ProfileHash = profileHash
	for _, workspace := range workspaces {
		workspace.Spec.ClassBinding.ProfileHash = profileHash
	}
	return mapper, parameters
}

// Cleanup-only recovery installs the core finalizer only on ACP-owned
// workspaces: adapters registered solely under the enabled provider API (the
// development fake provider) are not running in cleanup-only mode, and a
// finalizer no adapter can settle with StateDeleted would make the object
// undeletable.
func TestExecutionWorkspaceCleanupOnlyFinalizerIsACPScoped(t *testing.T) {
	t.Parallel()
	const cleanupTestNamespace = "cleanup-ns"
	ctx := context.Background()
	scheme := testWorkspaceScheme(t)
	acpOwned := &workspacev1alpha1.ExecutionWorkspace{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: cleanupTestNamespace, Name: "acp-owned", UID: types.UID("acp-owned-uid"),
			Labels: map[string]string{workspacev1alpha1.ProviderControllerLabel: acpWorkspaceProviderControllerName},
		},
	}
	foreign := &workspacev1alpha1.ExecutionWorkspace{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: cleanupTestNamespace, Name: "fake-owned", UID: types.UID("fake-owned-uid"),
			Labels: map[string]string{workspacev1alpha1.ProviderControllerLabel: FakeWorkspaceControllerName},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(acpOwned, foreign).Build()
	reconciler := &ExecutionWorkspaceReconciler{Client: c, APIReader: c, CleanupOnly: true}
	for _, name := range []string{acpOwned.Name, foreign.Name} {
		if _, err := reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: cleanupTestNamespace, Name: name},
		}); err != nil {
			t.Fatalf("cleanup-only reconcile %s: %v", name, err)
		}
	}
	got := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: cleanupTestNamespace, Name: acpOwned.Name}, got); err != nil {
		t.Fatalf("read ACP workspace: %v", err)
	}
	if len(got.Finalizers) == 0 {
		t.Fatal("an ACP-owned workspace must gain the cleanup finalizer so retention can reclaim it")
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: cleanupTestNamespace, Name: foreign.Name}, got); err != nil {
		t.Fatalf("read fake workspace: %v", err)
	}
	if len(got.Finalizers) != 0 {
		t.Fatal("a non-ACP workspace must not gain a finalizer no running adapter can ever settle")
	}
}

// A substrate-backend Suspend class must not advertise readiness when its
// profile also carries agent-sandbox inputs: resolution rejects that profile,
// so every Task selecting the class would fail after admission.
func TestExecutionWorkspaceClassReconcilerRejectsCrossBackendSubstrateProfile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const profileName = "acp-cross-profile"
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "acp-cross-backend"}}
	provider := testGenericProvider("acp-provider-cross")
	provider.Spec.ControllerName = acpWorkspaceProviderControllerName
	provider.Status.SupportedFeatures = []workspacev1alpha1.ExecutionWorkspaceFeature{
		workspacev1alpha1.WorkspaceFeatureExec,
		workspacev1alpha1.WorkspaceFeatureReset,
		workspacev1alpha1.WorkspaceFeatureSuspend,
		workspacev1alpha1.WorkspaceFeatureTLS,
	}
	provider.Status.Conditions = []metav1.Condition{{
		Type: string(workspacev1alpha1.ConditionProviderReady), Status: metav1.ConditionTrue, Reason: "Ready",
	}}
	provider.Spec.ParametersRef = workspacev1alpha1.TypedObjectReference{
		Group: acpworkspacev1alpha1.GroupVersion.Group, Kind: acpWorkspaceProviderConfigKind, Name: "acp-config-cross",
	}
	providerConfig := &acpworkspacev1alpha1.RuntimeProviderConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "acp-config-cross"},
		Spec:       acpworkspacev1alpha1.RuntimeProviderConfigSpec{Backend: acpworkspacev1alpha1.RuntimeProviderBackendSubstrate},
	}
	class := testGenericClass(ns.Name, "class", provider.Name)
	class.Spec.ParametersRef = &workspacev1alpha1.TypedObjectReference{
		Group: acpworkspacev1alpha1.GroupVersion.Group, Kind: acpWorkspaceProviderProfileKind, Name: profileName,
	}
	class.Spec.Lifecycle.AllowedOnDetach = append(class.Spec.Lifecycle.AllowedOnDetach,
		workspacev1alpha1.WorkspaceOnDetachSuspend)
	mapper, parameters := testParameterMapping(ns.Name, class.Spec.ParametersRef)
	profile := &acpworkspacev1alpha1.RuntimeWorkspaceProfile{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns.Name, Name: profileName, UID: profileName + "-uid", Generation: 1},
		Spec: acpworkspacev1alpha1.RuntimeWorkspaceProfileSpec{
			Substrate: &acpworkspacev1alpha1.SubstrateProfileSpec{
				TemplateRef: acpworkspacev1alpha1.SubstrateTemplateReference{Name: testSubstrateTemplateName},
				Suspend:     &acpworkspacev1alpha1.SubstrateSuspendPolicy{Mode: acpworkspacev1alpha1.SubstrateSuspendModeDataOnly},
			},
			AgentSandbox: &acpworkspacev1alpha1.AgentSandboxProfileSpec{},
		},
	}
	scheme := testWorkspaceScheme(t)
	if err := acpworkspacev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add acp scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(class).
		WithObjects(ns, provider, providerConfig, class, parameters, profile).
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
	condition := workspaceprovider.FindCondition(got.Status.Conditions, string(workspacev1alpha1.ConditionClassReady))
	if condition == nil || condition.Status == metav1.ConditionTrue {
		t.Fatalf("a substrate profile with agent-sandbox inputs must not be Ready, got %+v", condition)
	}
}

// A StorageClass change re-enqueues every class so a created or corrected
// storage class lifts a stale NotReady without an unrelated class edit.
func TestExecutionWorkspaceClassReconcilerWatchesStorageClasses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scheme := testWorkspaceScheme(t)
	classA := testGenericClass("ns-a", "class-a", "provider")
	classB := testGenericClass("ns-b", "class-b", "provider")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(classA, classB).Build()
	reconciler := &ExecutionWorkspaceClassReconciler{Client: c}
	requests := reconciler.classesForStorageChange(ctx, &storagev1.StorageClass{})
	if len(requests) != 2 {
		t.Fatalf("storage-class change enqueued %d classes, want every class (2)", len(requests))
	}
}

// The reserved ACP adapter advertises suspend per provider, but a class that
// permits Suspend goes Ready only when it allows Session reuse and its
// RuntimeWorkspaceProfile opts into a backend DataOnly suspend policy;
// otherwise every Task relying on the advertised lifecycle would fail later
// at binding or detach-action resolution.
func TestExecutionWorkspaceClassReconcilerRequiresACPSuspendPolicy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const acpProfileName = "acp-profile"
	shape := func(
		nsName string,
		withPolicy, withSessionReuse, withIdleTimeout, withMaxLifetime, zeroCapSuspendOnly bool,
	) (bool, string) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}
		provider := testGenericProvider("acp-provider-" + nsName)
		provider.Spec.ControllerName = acpWorkspaceProviderControllerName
		provider.Status.SupportedFeatures = []workspacev1alpha1.ExecutionWorkspaceFeature{
			workspacev1alpha1.WorkspaceFeatureExec,
			workspacev1alpha1.WorkspaceFeatureReset,
			workspacev1alpha1.WorkspaceFeatureSuspend,
			workspacev1alpha1.WorkspaceFeatureTLS,
		}
		provider.Status.Conditions = []metav1.Condition{{
			Type: string(workspacev1alpha1.ConditionProviderReady), Status: metav1.ConditionTrue, Reason: "Ready",
		}}
		provider.Spec.ParametersRef = workspacev1alpha1.TypedObjectReference{
			Group: acpworkspacev1alpha1.GroupVersion.Group, Kind: acpWorkspaceProviderConfigKind, Name: "acp-config-" + nsName,
		}
		providerConfig := &acpworkspacev1alpha1.RuntimeProviderConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "acp-config-" + nsName},
			Spec:       acpworkspacev1alpha1.RuntimeProviderConfigSpec{Backend: acpworkspacev1alpha1.RuntimeProviderBackendSubstrate},
		}
		class := testGenericClass(ns.Name, "class", provider.Name)
		class.Spec.ParametersRef = &workspacev1alpha1.TypedObjectReference{
			Group: acpworkspacev1alpha1.GroupVersion.Group, Kind: acpWorkspaceProviderProfileKind, Name: acpProfileName,
		}
		class.Spec.Lifecycle.AllowedOnDetach = append(class.Spec.Lifecycle.AllowedOnDetach,
			workspacev1alpha1.WorkspaceOnDetachSuspend)
		if zeroCapSuspendOnly {
			class.Spec.Lifecycle.DefaultOnDetach = workspacev1alpha1.WorkspaceOnDetachSuspend
			class.Spec.Lifecycle.AllowedOnDetach = []workspacev1alpha1.WorkspaceOnDetach{
				workspacev1alpha1.WorkspaceOnDetachSuspend,
			}
		}
		if withIdleTimeout {
			class.Spec.Lifecycle.IdleTimeout = &metav1.Duration{Duration: time.Hour}
		}
		if withMaxLifetime {
			class.Spec.Lifecycle.MaxLifetime = &metav1.Duration{Duration: 24 * time.Hour}
		}
		if !withSessionReuse {
			class.Spec.AllowedReuseScopes = []workspacev1alpha1.WorkspaceReuseScope{
				workspacev1alpha1.WorkspaceReuseScopeNone,
			}
		}
		mapper, parameters := testParameterMapping(ns.Name, class.Spec.ParametersRef)
		profile := &acpworkspacev1alpha1.RuntimeWorkspaceProfile{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns.Name, Name: acpProfileName, UID: acpProfileName + "-uid", Generation: 1},
			Spec: acpworkspacev1alpha1.RuntimeWorkspaceProfileSpec{
				Substrate: &acpworkspacev1alpha1.SubstrateProfileSpec{
					TemplateRef: acpworkspacev1alpha1.SubstrateTemplateReference{Name: testSubstrateTemplateName},
				},
			},
		}
		if withPolicy {
			profile.Spec.Substrate.Suspend = &acpworkspacev1alpha1.SubstrateSuspendPolicy{Mode: acpworkspacev1alpha1.SubstrateSuspendModeDataOnly}
		}
		limit := int32(1)
		if zeroCapSuspendOnly {
			limit = 0
		}
		profile.Spec.Retention = &acpworkspacev1alpha1.RetentionPolicy{MaxSuspendedWorkspaces: &limit}
		scheme := testWorkspaceScheme(t)
		if err := acpworkspacev1alpha1.AddToScheme(scheme); err != nil {
			t.Fatalf("add acp scheme: %v", err)
		}
		c := fake.NewClientBuilder().WithScheme(scheme).
			WithStatusSubresource(class).
			WithObjects(ns, provider, providerConfig, class, parameters, profile).
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
		condition := workspaceprovider.FindCondition(got.Status.Conditions, string(workspacev1alpha1.ConditionClassReady))
		if condition == nil {
			t.Fatal("class readiness condition missing")
		}
		return condition.Status == metav1.ConditionTrue, condition.Message
	}

	ready, message := shape("acp-suspend-nopolicy", false, true, false, true, false)
	if ready || !strings.Contains(message, "DataOnly suspend policy") {
		t.Fatalf("a Suspend class without a profile policy must not be Ready (ready=%v message=%q)", ready, message)
	}
	ready, message = shape("acp-suspend-no-session-reuse", true, false, false, true, false)
	if ready || !strings.Contains(message, "Session reuse scope") {
		t.Fatalf("a Suspend class without Session reuse must not be Ready (ready=%v message=%q)", ready, message)
	}
	if ready, message = shape("acp-suspend-policy", true, true, false, true, false); !ready {
		t.Fatalf("a Suspend class with a DataOnly profile policy must be Ready (message=%q)", message)
	}
	if ready, message = shape("acp-suspend-quota-only", true, true, false, false, false); ready ||
		!strings.Contains(message, "RuntimeWorkspaceProfile is invalid") {
		t.Fatalf("a quota-only Suspend class must not be Ready (ready=%v message=%q)", ready, message)
	}
	if ready, message = shape("acp-suspend-capped-idle-only", true, true, true, false, false); ready ||
		!strings.Contains(message, "RuntimeWorkspaceProfile is invalid") {
		t.Fatalf("a capped idle-only Suspend class must not be Ready (ready=%v message=%q)", ready, message)
	}
	if ready, message = shape("acp-suspend-zero-cap-only", true, true, false, true, true); ready ||
		!strings.Contains(message, "RuntimeWorkspaceProfile is invalid") {
		t.Fatalf("a zero-cap suspend-only class must not be Ready (ready=%v message=%q)", ready, message)
	}
}

// A sandbox-backend Suspend class goes Ready only when its profile passes the
// SAME validators Task resolution runs: the frozen durable-volume shape and
// the pinned Delete-reclaim storage class - and never while the profile also
// carries substrate inputs.
func TestExecutionWorkspaceClassReconcilerValidatesSandboxSuspendProfile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const sandboxProfileName = "acp-sandbox-profile"
	retain := corev1.PersistentVolumeReclaimRetain
	shape := func(
		nsName string,
		mutate func(
			profile *acpworkspacev1alpha1.RuntimeWorkspaceProfile,
			storageClass *storagev1.StorageClass,
			class *workspacev1alpha1.ExecutionWorkspaceClass,
		),
	) (bool, string) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}
		provider := testGenericProvider("acp-provider-" + nsName)
		provider.Spec.ControllerName = acpWorkspaceProviderControllerName
		provider.Status.SupportedFeatures = []workspacev1alpha1.ExecutionWorkspaceFeature{
			workspacev1alpha1.WorkspaceFeatureExec,
			workspacev1alpha1.WorkspaceFeatureReset,
			workspacev1alpha1.WorkspaceFeatureSuspend,
			workspacev1alpha1.WorkspaceFeatureTLS,
		}
		provider.Status.Conditions = []metav1.Condition{{
			Type: string(workspacev1alpha1.ConditionProviderReady), Status: metav1.ConditionTrue, Reason: "Ready",
		}}
		provider.Spec.ParametersRef = workspacev1alpha1.TypedObjectReference{
			Group: acpworkspacev1alpha1.GroupVersion.Group, Kind: acpWorkspaceProviderConfigKind, Name: "acp-config-" + nsName,
		}
		providerConfig := &acpworkspacev1alpha1.RuntimeProviderConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "acp-config-" + nsName},
			Spec:       acpworkspacev1alpha1.RuntimeProviderConfigSpec{Backend: acpworkspacev1alpha1.RuntimeProviderBackendAgentSandbox},
		}
		class := testGenericClass(ns.Name, "class", provider.Name)
		class.Spec.ParametersRef = &workspacev1alpha1.TypedObjectReference{
			Group: acpworkspacev1alpha1.GroupVersion.Group, Kind: acpWorkspaceProviderProfileKind, Name: sandboxProfileName,
		}
		class.Spec.Lifecycle.AllowedOnDetach = append(class.Spec.Lifecycle.AllowedOnDetach,
			workspacev1alpha1.WorkspaceOnDetachSuspend)
		class.Spec.Lifecycle.MaxLifetime = &metav1.Duration{Duration: 24 * time.Hour}
		mapper, parameters := testParameterMapping(ns.Name, class.Spec.ParametersRef)
		profile := &acpworkspacev1alpha1.RuntimeWorkspaceProfile{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns.Name, Name: sandboxProfileName, UID: sandboxProfileName + "-uid", Generation: 1},
			Spec: acpworkspacev1alpha1.RuntimeWorkspaceProfileSpec{
				Retention: &acpworkspacev1alpha1.RetentionPolicy{
					MaxSuspendedWorkspaces: func() *int32 { limit := int32(1); return &limit }(),
				},
				AgentSandbox: &acpworkspacev1alpha1.AgentSandboxProfileSpec{
					Suspend: &acpworkspacev1alpha1.AgentSandboxSuspendPolicy{
						Mode: acpworkspacev1alpha1.SubstrateSuspendModeDataOnly,
						Volume: acpworkspacev1alpha1.AgentSandboxDurableVolume{
							Capacity:         "1Gi",
							StorageClassName: "durable-" + nsName,
						},
					},
				},
			},
		}
		storageClass := &storagev1.StorageClass{
			ObjectMeta:  metav1.ObjectMeta{Name: "durable-" + nsName},
			Provisioner: "durable.csi.example",
		}
		if mutate != nil {
			mutate(profile, storageClass, class)
		}
		scheme := testWorkspaceScheme(t)
		if err := acpworkspacev1alpha1.AddToScheme(scheme); err != nil {
			t.Fatalf("add acp scheme: %v", err)
		}
		c := fake.NewClientBuilder().WithScheme(scheme).
			WithStatusSubresource(class).
			WithObjects(ns, provider, providerConfig, class, parameters, profile, storageClass).
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
		condition := workspaceprovider.FindCondition(got.Status.Conditions, string(workspacev1alpha1.ConditionClassReady))
		if condition == nil {
			t.Fatal("class readiness condition missing")
		}
		return condition.Status == metav1.ConditionTrue, condition.Message
	}

	if ready, message := shape("acp-sandbox-valid", nil); !ready {
		t.Fatalf("a valid sandbox suspend profile must be Ready (message=%q)", message)
	}
	if ready, message := shape("acp-sandbox-quota-only", func(_ *acpworkspacev1alpha1.RuntimeWorkspaceProfile, _ *storagev1.StorageClass, class *workspacev1alpha1.ExecutionWorkspaceClass) {
		class.Spec.Lifecycle.MaxLifetime = nil
	}); ready || !strings.Contains(message, "RuntimeWorkspaceProfile is invalid") {
		t.Fatalf("a quota-only sandbox Suspend class must not be Ready (ready=%v message=%q)", ready, message)
	}
	if ready, message := shape("acp-sandbox-capped-idle-only", func(_ *acpworkspacev1alpha1.RuntimeWorkspaceProfile, _ *storagev1.StorageClass, class *workspacev1alpha1.ExecutionWorkspaceClass) {
		class.Spec.Lifecycle.MaxLifetime = nil
		class.Spec.Lifecycle.IdleTimeout = &metav1.Duration{Duration: time.Hour}
	}); ready || !strings.Contains(message, "RuntimeWorkspaceProfile is invalid") {
		t.Fatalf("a capped idle-only sandbox Suspend class must not be Ready (ready=%v message=%q)", ready, message)
	}
	if ready, message := shape("acp-sandbox-mode", func(profile *acpworkspacev1alpha1.RuntimeWorkspaceProfile, _ *storagev1.StorageClass, _ *workspacev1alpha1.ExecutionWorkspaceClass) {
		profile.Spec.AgentSandbox.Suspend.Volume.AccessModes = []string{string(corev1.ReadOnlyMany)}
	}); ready || !strings.Contains(message, "RuntimeWorkspaceProfile is invalid") {
		t.Fatalf("a read-only durable volume must not be Ready (ready=%v message=%q)", ready, message)
	}
	if ready, message := shape("acp-sandbox-capacity", func(profile *acpworkspacev1alpha1.RuntimeWorkspaceProfile, _ *storagev1.StorageClass, _ *workspacev1alpha1.ExecutionWorkspaceClass) {
		profile.Spec.AgentSandbox.Suspend.Volume.Capacity = "-1Gi"
	}); ready || !strings.Contains(message, "RuntimeWorkspaceProfile is invalid") {
		t.Fatalf("a non-positive durable capacity must not be Ready (ready=%v message=%q)", ready, message)
	}
	if ready, message := shape("acp-sandbox-reclaim", func(_ *acpworkspacev1alpha1.RuntimeWorkspaceProfile, storageClass *storagev1.StorageClass, _ *workspacev1alpha1.ExecutionWorkspaceClass) {
		storageClass.ReclaimPolicy = &retain
	}); ready || !strings.Contains(message, "RuntimeWorkspaceProfile is invalid") {
		t.Fatalf("a Retain-reclaim storage class must not be Ready (ready=%v message=%q)", ready, message)
	}
	if ready, message := shape("acp-sandbox-missing-class", func(profile *acpworkspacev1alpha1.RuntimeWorkspaceProfile, _ *storagev1.StorageClass, _ *workspacev1alpha1.ExecutionWorkspaceClass) {
		profile.Spec.AgentSandbox.Suspend.Volume.StorageClassName = "absent-storage-class"
	}); ready || !strings.Contains(message, "RuntimeWorkspaceProfile is invalid") {
		t.Fatalf("a missing storage class must not be Ready (ready=%v message=%q)", ready, message)
	}
	if ready, message := shape("acp-sandbox-substrate", func(profile *acpworkspacev1alpha1.RuntimeWorkspaceProfile, _ *storagev1.StorageClass, _ *workspacev1alpha1.ExecutionWorkspaceClass) {
		profile.Spec.Substrate = &acpworkspacev1alpha1.SubstrateProfileSpec{
			Suspend: &acpworkspacev1alpha1.SubstrateSuspendPolicy{Mode: acpworkspacev1alpha1.SubstrateSuspendModeDataOnly},
		}
	}); ready || !strings.Contains(message, "RuntimeWorkspaceProfile is invalid") {
		t.Fatalf("simultaneous substrate inputs on a sandbox backend must not be Ready (ready=%v message=%q)", ready, message)
	}
	if ready, message := shape("acp-sandbox-delete-only-invalid", func(
		profile *acpworkspacev1alpha1.RuntimeWorkspaceProfile,
		_ *storagev1.StorageClass,
		class *workspacev1alpha1.ExecutionWorkspaceClass,
	) {
		class.Spec.Lifecycle.DefaultOnDetach = workspacev1alpha1.WorkspaceOnDetachDelete
		class.Spec.Lifecycle.AllowedOnDetach = []workspacev1alpha1.WorkspaceOnDetach{workspacev1alpha1.WorkspaceOnDetachDelete}
		profile.Spec.AgentSandbox.Suspend.Volume.Capacity = "-1Gi"
	}); ready || !strings.Contains(message, "RuntimeWorkspaceProfile is invalid") {
		t.Fatalf("a Delete-only class with an invalid durable profile must not be Ready (ready=%v message=%q)", ready, message)
	}
}

func TestExecutionWorkspaceClassReconcilerReadsACPSuspendPolicyFromAPIReader(t *testing.T) {
	t.Parallel()
	const (
		namespace   = "acp-suspend-reader"
		profileName = "acp-profile"
	)
	profile := func(withPolicy bool) *acpworkspacev1alpha1.RuntimeWorkspaceProfile {
		result := &acpworkspacev1alpha1.RuntimeWorkspaceProfile{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:  namespace,
				Name:       profileName,
				UID:        profileName + "-uid",
				Generation: 1,
			},
			Spec: acpworkspacev1alpha1.RuntimeWorkspaceProfileSpec{
				Substrate: &acpworkspacev1alpha1.SubstrateProfileSpec{
					TemplateRef: acpworkspacev1alpha1.SubstrateTemplateReference{Name: testSubstrateTemplateName},
				},
			},
		}
		if withPolicy {
			result.Spec.Substrate.Suspend = &acpworkspacev1alpha1.SubstrateSuspendPolicy{
				Mode: acpworkspacev1alpha1.SubstrateSuspendModeDataOnly,
			}
		}
		return result
	}
	for _, test := range []struct {
		name                string
		cachedPolicy        bool
		authoritativePolicy bool
		want                bool
	}{
		{
			name:                "current policy enables suspension",
			cachedPolicy:        false,
			authoritativePolicy: true,
			want:                true,
		},
		{
			name:                "current policy disables suspension",
			cachedPolicy:        true,
			authoritativePolicy: false,
			want:                false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			const configName = "acp-config"
			scheme := testWorkspaceScheme(t)
			if err := acpworkspacev1alpha1.AddToScheme(scheme); err != nil {
				t.Fatalf("add acp scheme: %v", err)
			}
			providerConfig := &acpworkspacev1alpha1.RuntimeProviderConfig{
				ObjectMeta: metav1.ObjectMeta{Name: configName},
				Spec: acpworkspacev1alpha1.RuntimeProviderConfigSpec{
					Backend: acpworkspacev1alpha1.RuntimeProviderBackendSubstrate,
				},
			}
			cached := fake.NewClientBuilder().WithScheme(scheme).
				WithObjects(profile(test.cachedPolicy), providerConfig.DeepCopy()).
				Build()
			authoritative := fake.NewClientBuilder().WithScheme(scheme).
				WithObjects(profile(test.authoritativePolicy), providerConfig.DeepCopy()).
				Build()
			class := testGenericClass(namespace, "class", "provider")
			class.Spec.Lifecycle.MaxLifetime = &metav1.Duration{Duration: time.Hour}
			class.Spec.ParametersRef = &workspacev1alpha1.TypedObjectReference{
				Group: acpworkspacev1alpha1.GroupVersion.Group,
				Kind:  acpWorkspaceProviderProfileKind,
				Name:  profileName,
			}
			provider := testGenericProvider("provider")
			provider.Spec.ParametersRef = workspacev1alpha1.TypedObjectReference{
				Group: acpworkspacev1alpha1.GroupVersion.Group,
				Kind:  acpWorkspaceProviderConfigKind,
				Name:  configName,
			}

			valid, got, err := (&ExecutionWorkspaceClassReconciler{
				Client: cached, APIReader: authoritative,
			}).validateACPClassProfile(context.Background(), class, provider)
			if err != nil {
				t.Fatalf("read suspend policy: %v", err)
			}
			if !valid {
				t.Fatal("authoritative profile must be valid")
			}
			if got != test.want {
				t.Fatalf("permits suspend = %v, want %v", got, test.want)
			}
		})
	}
}
