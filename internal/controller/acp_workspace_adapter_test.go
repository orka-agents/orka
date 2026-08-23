/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	acpworkspacev1alpha1 "github.com/orka-agents/orka/api/acp.workspace/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
	"github.com/orka-agents/orka/pkg/workspaceprovider"
)

// The fake client never validates label values, so pin the API-server rule
// (RFC 1123-ish label value: no '/') for the ACP owner label directly.
func TestACPWorkspaceControllerLabelValueIsValid(t *testing.T) {
	t.Parallel()
	if errs := validation.IsValidLabelValue(acpWorkspaceControllerLabelValue); len(errs) != 0 {
		t.Fatalf("ACP workspace controller label value is invalid: %v", errs)
	}
}

func acpAdapterTestClient(t *testing.T, objects ...client.Object) client.WithWatch {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(testACPWorkspaceScheme(t)).
		WithStatusSubresource(
			&workspacev1alpha1.ExecutionWorkspace{},
			&workspacev1alpha1.ExecutionWorkspaceProvider{},
			&corev1alpha1.RuntimePool{},
		).
		WithObjects(objects...).
		Build()
}

func acpAdapterProvider() *workspacev1alpha1.ExecutionWorkspaceProvider {
	return &workspacev1alpha1.ExecutionWorkspaceProvider{
		ObjectMeta: metav1.ObjectMeta{Name: acpTestProviderName, UID: types.UID("acp-provider-uid"), Generation: 1},
		Spec: workspacev1alpha1.ExecutionWorkspaceProviderSpec{
			ControllerName: acpWorkspaceProviderControllerName,
			ParametersRef: workspacev1alpha1.TypedObjectReference{
				Group: acpworkspacev1alpha1.GroupVersion.Group, Kind: acpWorkspaceProviderConfigKind, Name: acpTestConfigName,
			},
			LifecycleState:    workspacev1alpha1.ExecutionWorkspaceProviderActive,
			RequiredContracts: []string{workspacev1alpha1.ContractVersionV1},
		},
	}
}

func TestACPWorkspaceProviderAdapterAdvertisesSuspend(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	provider := acpAdapterProvider()
	config := &acpworkspacev1alpha1.RuntimeProviderConfig{
		ObjectMeta: metav1.ObjectMeta{Name: acpTestConfigName},
		Spec:       acpworkspacev1alpha1.RuntimeProviderConfigSpec{Backend: acpworkspacev1alpha1.RuntimeProviderBackendAgentSandbox},
	}
	c := acpAdapterTestClient(t, provider, config)
	reconciler := &ACPWorkspaceProviderAdapterReconciler{
		Client: c, AgentSandboxEnabled: true, ACPWorkspaceDispatchEnabled: true, WorkspaceProviderAPIEnabled: true,
	}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: provider.Name}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	current := &workspacev1alpha1.ExecutionWorkspaceProvider{}
	if err := c.Get(ctx, types.NamespacedName{Name: provider.Name}, current); err != nil {
		t.Fatalf("get provider: %v", err)
	}
	if current.Status.Adapter == nil || current.Status.Adapter.Version == "" || current.Status.LastHeartbeat == nil {
		t.Fatalf("adapter identity/heartbeat missing: %+v", current.Status)
	}
	if len(current.Status.SupportedContracts) != 1 || current.Status.SupportedContracts[0] != workspacev1alpha1.ContractVersionV1 {
		t.Fatalf("contracts = %v", current.Status.SupportedContracts)
	}
	foundSuspend := false
	for _, feature := range current.Status.SupportedFeatures {
		if feature == workspacev1alpha1.WorkspaceFeatureSuspend {
			foundSuspend = true
		}
	}
	if !foundSuspend {
		t.Fatal("data-only cold suspension must be advertised; class profiles gate whether it is requestable")
	}
	// The core provider controller owns Compatible/Heartbeat/Ready; the
	// adapter must never write those conditions from its heartbeat snapshot.
	if workspaceprovider.FindCondition(current.Status.Conditions, string(workspacev1alpha1.ConditionProviderCompatible)) != nil {
		t.Fatalf("adapter must not write core-owned conditions, got %+v", current.Status.Conditions)
	}
}

func TestACPWorkspaceProviderAdapterFailsClosed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		reconciler func(client.Client) *ACPWorkspaceProviderAdapterReconciler
		config     *acpworkspacev1alpha1.RuntimeProviderConfig
	}{
		{
			name: "workspace provider API disabled",
			reconciler: func(c client.Client) *ACPWorkspaceProviderAdapterReconciler {
				return &ACPWorkspaceProviderAdapterReconciler{Client: c, AgentSandboxEnabled: true, ACPWorkspaceDispatchEnabled: true}
			},
			config: &acpworkspacev1alpha1.RuntimeProviderConfig{
				ObjectMeta: metav1.ObjectMeta{Name: acpTestConfigName},
				Spec:       acpworkspacev1alpha1.RuntimeProviderConfigSpec{Backend: acpworkspacev1alpha1.RuntimeProviderBackendAgentSandbox},
			},
		},
		{
			name: "dispatch disabled",
			reconciler: func(c client.Client) *ACPWorkspaceProviderAdapterReconciler {
				return &ACPWorkspaceProviderAdapterReconciler{Client: c, AgentSandboxEnabled: true, WorkspaceProviderAPIEnabled: true}
			},
			config: &acpworkspacev1alpha1.RuntimeProviderConfig{
				ObjectMeta: metav1.ObjectMeta{Name: acpTestConfigName},
				Spec:       acpworkspacev1alpha1.RuntimeProviderConfigSpec{Backend: acpworkspacev1alpha1.RuntimeProviderBackendAgentSandbox},
			},
		},
		{
			name: "backend flag disabled",
			reconciler: func(c client.Client) *ACPWorkspaceProviderAdapterReconciler {
				return &ACPWorkspaceProviderAdapterReconciler{Client: c, ACPWorkspaceDispatchEnabled: true, AgentSandboxEnabled: true, WorkspaceProviderAPIEnabled: true}
			},
			config: &acpworkspacev1alpha1.RuntimeProviderConfig{
				ObjectMeta: metav1.ObjectMeta{Name: acpTestConfigName},
				Spec:       acpworkspacev1alpha1.RuntimeProviderConfigSpec{Backend: acpworkspacev1alpha1.RuntimeProviderBackendSubstrate},
			},
		},
		{
			name: "missing config",
			reconciler: func(c client.Client) *ACPWorkspaceProviderAdapterReconciler {
				return &ACPWorkspaceProviderAdapterReconciler{Client: c, AgentSandboxEnabled: true, ACPWorkspaceDispatchEnabled: true, WorkspaceProviderAPIEnabled: true}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			provider := acpAdapterProvider()
			provider.Status.SupportedFeatures = []workspacev1alpha1.ExecutionWorkspaceFeature{workspacev1alpha1.WorkspaceFeatureExec}
			objects := []client.Object{provider}
			if tt.config != nil {
				objects = append(objects, tt.config)
			}
			c := acpAdapterTestClient(t, objects...)
			if _, err := tt.reconciler(c).Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: provider.Name}}); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			current := &workspacev1alpha1.ExecutionWorkspaceProvider{}
			if err := c.Get(ctx, types.NamespacedName{Name: provider.Name}, current); err != nil {
				t.Fatalf("get provider: %v", err)
			}
			if len(current.Status.SupportedFeatures) != 0 || current.Status.Adapter != nil || current.Status.LastHeartbeat != nil {
				t.Fatalf("advertisement must be cleared: %+v", current.Status)
			}
			if workspaceprovider.ConditionIsTrue(current.Status.Conditions, string(workspacev1alpha1.ConditionProviderCompatible)) {
				t.Fatal("Compatible must never be set true by the adapter; the core controller computes it from the cleared advertisement")
			}
		})
	}
}

func acpAdapterWorkspace(t *testing.T, poolName string) *workspacev1alpha1.ExecutionWorkspace {
	t.Helper()
	workspace := &workspacev1alpha1.ExecutionWorkspace{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: acpTestNamespace, Name: "acp-ws-test", UID: types.UID("acp-ws-test-uid"), Generation: 1,
			Labels:      map[string]string{workspacev1alpha1.ProviderControllerLabel: acpWorkspaceControllerLabelValue},
			Annotations: map[string]string{acpExecutionWorkspacePoolAnnotation: poolName},
		},
		Spec: workspacev1alpha1.ExecutionWorkspaceSpec{
			Mode: workspacev1alpha1.ExecutionWorkspaceModeInteractive,
			ClassBinding: workspacev1alpha1.ImmutableObjectBinding{
				Name: acpTestClassName, UID: types.UID("acp-class-uid"), Generation: 1,
				ProfileHash: "sha256:" + strings.Repeat("a", 64),
			},
			ProviderBinding: workspacev1alpha1.ImmutableObjectBinding{
				Name: acpTestProviderName, UID: types.UID("acp-provider-uid"), Generation: 1,
			},
			Slot:         defaultWorkspaceSlotName,
			DesiredState: workspacev1alpha1.ExecutionWorkspaceDesiredReady,
			Lifecycle: workspacev1alpha1.ExecutionWorkspaceLifecycle{
				DefaultOnDetach: workspacev1alpha1.WorkspaceOnDetachDelete,
				AllowedOnDetach: []workspacev1alpha1.WorkspaceOnDetach{workspacev1alpha1.WorkspaceOnDetachDelete},
				DetachTimeout:   metav1.Duration{Duration: 120000000000},
				DeletionPolicy: workspacev1alpha1.ExecutionWorkspaceDeletionPolicy{
					ProviderResources: workspacev1alpha1.WorkspaceDeletionActionDelete,
					PersistentVolumes: workspacev1alpha1.WorkspaceDeletionActionDelete,
					Checkpoints:       workspacev1alpha1.WorkspaceDeletionActionDelete,
				},
			},
		},
	}
	markWorkspaceAdmittedForPolicyReview(workspace, workspace.Generation)
	return workspace
}

func acpAdapterLinkedPool(namespace, name, workspaceName string) *corev1alpha1.RuntimePool {
	return &corev1alpha1.RuntimePool{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace, Name: name, UID: types.UID(name + "-uid"),
			Labels: map[string]string{acpExecutionWorkspaceLinkLabel: workspaceName},
			// Mirrors the controller-stamped workspace-incarnation pin the
			// adapter now requires before deleting a linked pool.
			Annotations: map[string]string{acpExecutionWorkspaceUIDAnnotation: workspaceName + "-uid"},
		},
		Spec: corev1alpha1.RuntimePoolSpec{
			ExecutionWorkspace: &corev1alpha1.RuntimePoolExecutionWorkspaceSpec{
				Provider:      corev1alpha1.WorkspaceProviderAgentSandbox,
				BindingDigest: "sha256:" + strings.Repeat("b", 64),
			},
		},
	}
}

func reconcileACPWorkspaceAdapter(t *testing.T, c client.Client, workspace *workspacev1alpha1.ExecutionWorkspace) {
	t.Helper()
	reconciler := &ACPExecutionWorkspaceAdapterReconciler{Client: c, APIReader: c}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name},
	}); err != nil {
		t.Fatalf("adapter reconcile: %v", err)
	}
}

func TestACPExecutionWorkspaceAdapterReadyAndAttached(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	provider := acpAdapterProvider()
	workspace := acpAdapterWorkspace(t, "")
	workspace.Spec.AttachmentEpoch = 3
	workspace.Spec.Attachment = &workspacev1alpha1.ExecutionWorkspaceAttachment{
		TaskRef:        workspacev1alpha1.ObjectIdentityReference{Name: "attached-task", UID: types.UID("task-uid")},
		Epoch:          3,
		TokenSHA256:    "sha256:" + strings.Repeat("c", 64),
		TokenSecretRef: workspacev1alpha1.SecretReference{Name: "attach-secret"},
		ExpiresAt:      metav1.Now(),
	}
	c := acpAdapterTestClient(t, provider, workspace)
	reconcileACPWorkspaceAdapter(t, c, workspace)

	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, current); err != nil {
		t.Fatalf("get workspace: %v", err)
	}
	if current.Status.State != workspacev1alpha1.ExecutionWorkspaceStateAttached || current.Status.AttachedEpoch != 3 {
		t.Fatalf("status = %+v", current.Status)
	}

	// Revocation clears the intent; the adapter releases the enforced epoch
	// even though re-admission for the bumped generation is still pending.
	current.Spec.Attachment = nil
	if err := c.Update(ctx, current); err != nil {
		t.Fatalf("revoke attachment: %v", err)
	}
	reconcileACPWorkspaceAdapter(t, c, current)
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, current); err != nil {
		t.Fatalf("get revoked workspace: %v", err)
	}
	if current.Status.AttachedEpoch != 0 || current.Status.State != workspacev1alpha1.ExecutionWorkspaceStateReady {
		t.Fatalf("revoked status = %+v", current.Status)
	}
}

func TestACPExecutionWorkspaceAdapterDeletionTearsDownLinkedPool(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	provider := acpAdapterProvider()
	workspace := acpAdapterWorkspace(t, "acp-ws-pool")
	workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredDeleted
	pool := acpAdapterLinkedPool(workspace.Namespace, "acp-ws-pool", workspace.Name)
	c := acpAdapterTestClient(t, provider, workspace, pool)

	reconcileACPWorkspaceAdapter(t, c, workspace)
	err := c.Get(ctx, types.NamespacedName{Namespace: pool.Namespace, Name: pool.Name}, &corev1alpha1.RuntimePool{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("linked pool must be deleted, got %v", err)
	}

	reconcileACPWorkspaceAdapter(t, c, workspace)
	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, current); err != nil {
		t.Fatalf("get workspace: %v", err)
	}
	if current.Status.State != workspacev1alpha1.ExecutionWorkspaceStateDeleted {
		t.Fatalf("state = %s", current.Status.State)
	}
	if err := workspaceprovider.ValidateInteractiveDeletedDisposition(
		current.Status.Disposition, current.Spec.Lifecycle.DeletionPolicy,
	); err != nil {
		t.Fatalf("terminal disposition: %v", err)
	}
}

func TestACPExecutionWorkspaceAdapterRefusesForeignPool(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	provider := acpAdapterProvider()
	workspace := acpAdapterWorkspace(t, "acp-ws-pool")
	workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredDeleted
	foreign := acpAdapterLinkedPool(workspace.Namespace, "acp-ws-pool", "some-other-workspace")
	c := acpAdapterTestClient(t, provider, workspace, foreign)

	reconcileACPWorkspaceAdapter(t, c, workspace)
	if err := c.Get(ctx, types.NamespacedName{Namespace: foreign.Namespace, Name: foreign.Name}, &corev1alpha1.RuntimePool{}); err != nil {
		t.Fatalf("foreign pool must be preserved: %v", err)
	}
	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, current); err != nil {
		t.Fatalf("get workspace: %v", err)
	}
	if current.Status.Disposition != nil || current.Status.State == workspacev1alpha1.ExecutionWorkspaceStateDeleted {
		t.Fatalf("terminal disposition must be withheld over a foreign pool: %+v", current.Status)
	}
	if workspaceprovider.ConditionIsTrue(current.Status.Conditions, string(workspacev1alpha1.ConditionWorkspaceFinalized)) {
		t.Fatalf("Finalized must not be true over a foreign pool")
	}
}

// A pool carrying the linked name but a different workspace-incarnation pin
// is foreign: the adapter must preserve it and hold the finalizer.
func TestACPExecutionWorkspaceAdapterRefusesUIDMismatchedPool(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	provider := acpAdapterProvider()
	workspace := acpAdapterWorkspace(t, "acp-ws-pool")
	workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredDeleted
	pool := acpAdapterLinkedPool(workspace.Namespace, "acp-ws-pool", workspace.Name)
	pool.Annotations[acpExecutionWorkspaceUIDAnnotation] = "different-incarnation-uid"
	c := acpAdapterTestClient(t, provider, workspace, pool)

	reconcileACPWorkspaceAdapter(t, c, workspace)
	if err := c.Get(ctx, types.NamespacedName{Namespace: pool.Namespace, Name: pool.Name}, &corev1alpha1.RuntimePool{}); err != nil {
		t.Fatalf("UID-mismatched pool must be preserved: %v", err)
	}
	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, current); err != nil {
		t.Fatalf("get workspace: %v", err)
	}
	if current.Status.Disposition != nil || current.Status.State == workspacev1alpha1.ExecutionWorkspaceStateDeleted {
		t.Fatalf("terminal disposition must be withheld over a UID-mismatched pool: %+v", current.Status)
	}
	if workspaceprovider.ConditionIsTrue(current.Status.Conditions, string(workspacev1alpha1.ConditionWorkspaceFinalized)) {
		t.Fatal("Finalized must not be true over a UID-mismatched pool")
	}
}

func TestACPExecutionWorkspaceAdapterQuarantineStopsCompute(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	provider := acpAdapterProvider()
	workspace := acpAdapterWorkspace(t, "acp-ws-pool")
	workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined
	pool := acpAdapterLinkedPool(workspace.Namespace, "acp-ws-pool", workspace.Name)
	c := acpAdapterTestClient(t, provider, workspace, pool)

	reconcileACPWorkspaceAdapter(t, c, workspace)
	err := c.Get(ctx, types.NamespacedName{Namespace: pool.Namespace, Name: pool.Name}, &corev1alpha1.RuntimePool{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("quarantine must destroy the linked pool, got %v", err)
	}
	reconcileACPWorkspaceAdapter(t, c, workspace)
	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, current); err != nil {
		t.Fatalf("get workspace: %v", err)
	}
	if current.Status.State != workspacev1alpha1.ExecutionWorkspaceStateQuarantined {
		t.Fatalf("state = %s", current.Status.State)
	}
	if current.Status.Disposition != nil {
		t.Fatalf("quarantine must not report a terminal deletion disposition")
	}
}

func TestACPExecutionWorkspaceAdapterIgnoresForeignWorkspaces(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	provider := acpAdapterProvider()
	provider.Spec.ControllerName = "someone.else/adapter"
	workspace := acpAdapterWorkspace(t, "")
	workspace.Labels = nil
	c := acpAdapterTestClient(t, provider, workspace)
	reconcileACPWorkspaceAdapter(t, c, workspace)
	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, current); err != nil {
		t.Fatalf("get workspace: %v", err)
	}
	if current.Status.State != "" || len(current.Status.Conditions) != len(workspace.Status.Conditions) {
		t.Fatalf("foreign workspace status must stay untouched: %+v", current.Status)
	}
}
