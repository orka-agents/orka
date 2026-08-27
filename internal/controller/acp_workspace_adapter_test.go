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

	coordinationv1 "k8s.io/api/coordination/v1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	acpworkspacev1alpha1 "github.com/orka-agents/orka/api/acp.workspace/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
	"github.com/orka-agents/orka/pkg/workspaceprovider"
)

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

func TestACPWorkspaceProviderAdapterAdvertisesWithoutSuspend(t *testing.T) {
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
	for _, feature := range current.Status.SupportedFeatures {
		if feature == workspacev1alpha1.WorkspaceFeatureSuspend {
			t.Fatalf("suspend must not be advertised before data-only cold resume exists")
		}
	}
	// The core provider controller owns Compatible/Heartbeat/Ready; the
	// adapter must never write those conditions from its heartbeat snapshot.
	if workspaceprovider.FindCondition(current.Status.Conditions, string(workspacev1alpha1.ConditionProviderCompatible)) != nil {
		t.Fatalf("adapter must not write core-owned conditions, got %+v", current.Status.Conditions)
	}
}

// The config-identity pin lives in the controller-owned STATUS subresource:
// stripping the mirror annotation must not let a deleted-and-recreated
// same-name config be silently re-pinned and advertised.
func TestACPWorkspaceProviderAdapterPinSurvivesAnnotationStrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	provider := acpAdapterProvider()
	config := &acpworkspacev1alpha1.RuntimeProviderConfig{
		ObjectMeta: metav1.ObjectMeta{Name: acpTestConfigName, UID: types.UID("config-uid-original")},
		Spec:       acpworkspacev1alpha1.RuntimeProviderConfigSpec{Backend: acpworkspacev1alpha1.RuntimeProviderBackendAgentSandbox},
	}
	c := acpAdapterTestClient(t, provider, config)
	reconciler := &ACPWorkspaceProviderAdapterReconciler{
		Client: c, AgentSandboxEnabled: true, ACPWorkspaceDispatchEnabled: true, WorkspaceProviderAPIEnabled: true,
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: provider.Name}}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}
	current := &workspacev1alpha1.ExecutionWorkspaceProvider{}
	if err := c.Get(ctx, types.NamespacedName{Name: provider.Name}, current); err != nil {
		t.Fatalf("get provider: %v", err)
	}
	if current.Status.PinnedParametersUID != "config-uid-original" {
		t.Fatalf("status pin = %q, want the advertised config UID", current.Status.PinnedParametersUID)
	}

	// A metadata writer strips the mirror annotation; the config is deleted
	// and recreated under the same name with a different UID.
	base := current.DeepCopy()
	delete(current.Annotations, acpWorkspaceProviderConfigUIDAnnotation)
	if err := c.Patch(ctx, current, client.MergeFrom(base)); err != nil {
		t.Fatalf("strip annotation: %v", err)
	}
	if err := c.Delete(ctx, config); err != nil {
		t.Fatalf("delete config: %v", err)
	}
	replacement := &acpworkspacev1alpha1.RuntimeProviderConfig{
		ObjectMeta: metav1.ObjectMeta{Name: acpTestConfigName, UID: types.UID("config-uid-replacement")},
		Spec:       acpworkspacev1alpha1.RuntimeProviderConfigSpec{Backend: acpworkspacev1alpha1.RuntimeProviderBackendSubstrate},
	}
	if err := c.Create(ctx, replacement); err != nil {
		t.Fatalf("recreate config: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("post-replacement reconcile: %v", err)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: provider.Name}, current); err != nil {
		t.Fatalf("re-get provider: %v", err)
	}
	if current.Status.PinnedParametersUID != "config-uid-original" {
		t.Fatalf("status pin = %q; the replacement must never be re-pinned", current.Status.PinnedParametersUID)
	}
	ready := workspaceprovider.FindCondition(current.Status.Conditions, string(workspacev1alpha1.ConditionProviderReady))
	if ready != nil && ready.Status == metav1.ConditionTrue {
		t.Fatal("a replaced config must not stay advertised as Ready")
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
			Labels:      map[string]string{workspacev1alpha1.ProviderControllerLabel: acpWorkspaceProviderControllerName},
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

func acpAdapterLinkedPool(namespace, workspaceName string) *corev1alpha1.RuntimePool {
	const name = "acp-ws-pool"
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

func acpAdapterOwnedAttachmentCredentials(
	workspace *workspacev1alpha1.ExecutionWorkspace,
	epoch int64,
) (*corev1.Secret, *coordinationv1.Lease) {
	owner := *metav1.NewControllerRef(workspace, workspacev1alpha1.GroupVersion.WithKind("ExecutionWorkspace"))
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: workspace.Namespace,
		Name:      attachmentSecretName(workspace.Name, epoch),
		Labels:    map[string]string{workspaceAttachmentLabel: string(workspace.UID)},
		OwnerReferences: []metav1.OwnerReference{
			owner,
		},
	}}
	lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
		Namespace: workspace.Namespace,
		Name:      attachmentLeaseName(workspace.Name),
		OwnerReferences: []metav1.OwnerReference{
			owner,
		},
	}}
	return secret, lease
}

func TestACPExecutionWorkspaceAdapterReadyAndAttached(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	provider := acpAdapterProvider()
	workspace := acpAdapterWorkspace(t, "acp-ws-pool")
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

func TestACPExecutionWorkspaceAdapterSkipsStableStatusPatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	provider := acpAdapterProvider()
	workspace := acpAdapterWorkspace(t, "acp-ws-pool")
	base := acpAdapterTestClient(t, provider, workspace)
	statusPatches := 0
	c := interceptor.NewClient(base, interceptor.Funcs{
		SubResourcePatch: func(
			ctx context.Context,
			delegate client.Client,
			subresource string,
			object client.Object,
			patch client.Patch,
			options ...client.SubResourcePatchOption,
		) error {
			if _, isWorkspace := object.(*workspacev1alpha1.ExecutionWorkspace); isWorkspace {
				statusPatches++
			}
			return delegate.SubResource(subresource).Patch(ctx, object, patch, options...)
		},
	})
	reconciler := &ACPExecutionWorkspaceAdapterReconciler{Client: c, APIReader: c}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}}

	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if statusPatches != 1 {
		t.Fatalf("status patches after first reconcile = %d, want 1", statusPatches)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("stable reconcile: %v", err)
	}
	if statusPatches != 1 {
		t.Fatalf("status patches after stable reconcile = %d, want 1", statusPatches)
	}
}

func TestACPExecutionWorkspaceAdapterDeletionTearsDownLinkedPool(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	provider := acpAdapterProvider()
	workspace := acpAdapterWorkspace(t, "acp-ws-pool")
	workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredDeleted
	pool := acpAdapterLinkedPool(workspace.Namespace, workspace.Name)
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

// A directly deleted ATTACHED workspace must remove its bearer attachment
// Secret and Lease and prove their absence before publishing the Deleted
// disposition: Task settlement never ran, and asynchronous owner-reference GC
// is not proof.
func TestACPExecutionWorkspaceAdapterDeletionRevokesAttachmentCredentials(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	provider := acpAdapterProvider()
	workspace := acpAdapterWorkspace(t, "acp-ws-pool")
	workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredDeleted
	workspace.Spec.AttachmentEpoch = 2
	secret, lease := acpAdapterOwnedAttachmentCredentials(workspace, 2)
	c := acpAdapterTestClient(t, provider, workspace, secret, lease)

	for range 3 {
		reconcileACPWorkspaceAdapter(t, c, workspace)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: secret.Name}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatalf("attachment Secret must be deleted before finalization, got %v", err)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: lease.Name}, &coordinationv1.Lease{}); !apierrors.IsNotFound(err) {
		t.Fatalf("attachment Lease must be deleted before finalization, got %v", err)
	}
	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, current); err != nil {
		t.Fatalf("get workspace: %v", err)
	}
	if current.Status.State != workspacev1alpha1.ExecutionWorkspaceStateDeleted {
		t.Fatalf("state = %s, want Deleted after proven credential cleanup", current.Status.State)
	}
	if current.Status.Disposition == nil ||
		current.Status.Disposition.AccessCredentials != workspacev1alpha1.DispositionRevoked {
		t.Fatalf("disposition = %+v, want revoked access credentials", current.Status.Disposition)
	}
}

func TestACPExecutionWorkspaceAdapterRefusesForeignAttachmentCredentials(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		object func(*workspacev1alpha1.ExecutionWorkspace, metav1.OwnerReference) client.Object
		empty  func() client.Object
	}{
		{
			name: "Secret",
			object: func(workspace *workspacev1alpha1.ExecutionWorkspace, owner metav1.OwnerReference) client.Object {
				return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
					Namespace: workspace.Namespace,
					Name:      attachmentSecretName(workspace.Name, workspace.Spec.AttachmentEpoch),
					OwnerReferences: []metav1.OwnerReference{
						owner,
					},
				}}
			},
			empty: func() client.Object { return &corev1.Secret{} },
		},
		{
			name: "Lease",
			object: func(workspace *workspacev1alpha1.ExecutionWorkspace, owner metav1.OwnerReference) client.Object {
				return &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
					Namespace: workspace.Namespace,
					Name:      attachmentLeaseName(workspace.Name),
					OwnerReferences: []metav1.OwnerReference{
						owner,
					},
				}}
			},
			empty: func() client.Object { return &coordinationv1.Lease{} },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			provider := acpAdapterProvider()
			workspace := acpAdapterWorkspace(t, "acp-ws-pool")
			workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredDeleted
			workspace.Spec.AttachmentEpoch = 2
			foreignWorkspace := workspace.DeepCopy()
			foreignWorkspace.Name = "foreign-workspace"
			foreignWorkspace.UID = types.UID("foreign-workspace-uid")
			foreignOwner := *metav1.NewControllerRef(
				foreignWorkspace,
				workspacev1alpha1.GroupVersion.WithKind("ExecutionWorkspace"),
			)
			foreign := test.object(workspace, foreignOwner)
			c := acpAdapterTestClient(t, provider, workspace, foreign)
			reconciler := &ACPExecutionWorkspaceAdapterReconciler{Client: c, APIReader: c}

			_, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name},
			})
			if err == nil || !strings.Contains(err.Error(), "is not controlled by workspace") {
				t.Fatalf("reconcile error = %v, want foreign %s ownership rejection", err, test.name)
			}
			if err := c.Get(ctx, client.ObjectKeyFromObject(foreign), test.empty()); err != nil {
				t.Fatalf("foreign %s must be preserved: %v", test.name, err)
			}
			current := &workspacev1alpha1.ExecutionWorkspace{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), current); err != nil {
				t.Fatalf("get workspace: %v", err)
			}
			if current.Status.Disposition != nil || current.Status.State == workspacev1alpha1.ExecutionWorkspaceStateDeleted {
				t.Fatalf("terminal disposition must be withheld over a foreign %s: %+v", test.name, current.Status)
			}
		})
	}
}

// Rotation can leave the previous bearer behind if the controller exits after
// advancing the epoch but before deleting the superseded Secret. Quarantine
// preserves the workspace object, so terminal cleanup must find every exact
// workspace-owned attachment Secret rather than only the current epoch name.
func TestACPExecutionWorkspaceAdapterQuarantineDeletesRotatedAttachmentSecrets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	provider := acpAdapterProvider()
	workspace := acpAdapterWorkspace(t, "acp-ws-pool")
	workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined
	workspace.Spec.AttachmentEpoch = 3
	owner := *metav1.NewControllerRef(workspace, workspacev1alpha1.GroupVersion.WithKind("ExecutionWorkspace"))
	secretForEpoch := func(epoch int64) *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Namespace: workspace.Namespace,
			Name:      attachmentSecretName(workspace.Name, epoch),
			Labels:    map[string]string{workspaceAttachmentLabel: string(workspace.UID)},
			OwnerReferences: []metav1.OwnerReference{
				owner,
			},
		}}
	}
	stale := secretForEpoch(2)
	current := secretForEpoch(3)
	foreign := secretForEpoch(1)
	foreign.Name = "foreign-labeled-secret"
	foreign.OwnerReferences = nil
	c := acpAdapterTestClient(t, provider, workspace, stale, current, foreign)

	for range 3 {
		reconcileACPWorkspaceAdapter(t, c, workspace)
	}
	for _, secret := range []*corev1.Secret{stale, current} {
		if err := c.Get(ctx, client.ObjectKeyFromObject(secret), &corev1.Secret{}); !apierrors.IsNotFound(err) {
			t.Fatalf("workspace-owned attachment Secret %s must be deleted, got %v", secret.Name, err)
		}
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(foreign), &corev1.Secret{}); err != nil {
		t.Fatalf("a matching label without exact workspace ownership must be preserved: %v", err)
	}
	got := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), got); err != nil {
		t.Fatalf("get workspace: %v", err)
	}
	if got.Status.State != workspacev1alpha1.ExecutionWorkspaceStateQuarantined {
		t.Fatalf("state = %s, want Quarantined after all owned credentials are absent", got.Status.State)
	}
}

func TestACPExecutionWorkspaceAdapterRefusesForeignPool(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	provider := acpAdapterProvider()
	workspace := acpAdapterWorkspace(t, "acp-ws-pool")
	workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredDeleted
	foreign := acpAdapterLinkedPool(workspace.Namespace, "some-other-workspace")
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
	pool := acpAdapterLinkedPool(workspace.Namespace, workspace.Name)
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
	workspace.Spec.AttachmentEpoch = 4
	pool := acpAdapterLinkedPool(workspace.Namespace, workspace.Name)
	secret, lease := acpAdapterOwnedAttachmentCredentials(workspace, 4)
	c := acpAdapterTestClient(t, provider, workspace, pool, secret, lease)

	for range 3 {
		reconcileACPWorkspaceAdapter(t, c, workspace)
	}
	err := c.Get(ctx, types.NamespacedName{Namespace: pool.Namespace, Name: pool.Name}, &corev1alpha1.RuntimePool{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("quarantine must destroy the linked pool, got %v", err)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: secret.Namespace, Name: secret.Name}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatalf("quarantine must delete the attachment Secret, got %v", err)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: lease.Namespace, Name: lease.Name}, &coordinationv1.Lease{}); !apierrors.IsNotFound(err) {
		t.Fatalf("quarantine must delete the attachment Lease, got %v", err)
	}
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

// A workspace that binds the ACP provider UID but was not materialized by this
// controller (no controller label / pool-link annotation) must never be served
// a Ready projection: no RuntimePool exists for it, so advertising a usable
// physical environment would be a lie consumers can wait on forever.
func TestACPExecutionWorkspaceAdapterRequiresMaterializationMarkers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, mutate := range map[string]func(*workspacev1alpha1.ExecutionWorkspace){
		"missing controller label": func(workspace *workspacev1alpha1.ExecutionWorkspace) {
			delete(workspace.Labels, workspacev1alpha1.ProviderControllerLabel)
		},
		"missing pool link annotation": func(workspace *workspacev1alpha1.ExecutionWorkspace) {
			delete(workspace.Annotations, acpExecutionWorkspacePoolAnnotation)
		},
	} {
		t.Run(name, func(t *testing.T) {
			provider := acpAdapterProvider()
			workspace := acpAdapterWorkspace(t, "acp-ws-pool")
			mutate(workspace)
			c := acpAdapterTestClient(t, provider, workspace)
			reconcileACPWorkspaceAdapter(t, c, workspace)
			current := &workspacev1alpha1.ExecutionWorkspace{}
			if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, current); err != nil {
				t.Fatalf("get workspace: %v", err)
			}
			if current.Status.State != "" {
				t.Fatalf("unmaterialized workspace must stay unserved: %+v", current.Status)
			}
			if workspaceprovider.ConditionIsTrue(current.Status.Conditions, string(workspacev1alpha1.ConditionWorkspaceProvisioned)) {
				t.Fatal("unmaterialized workspace must never report Provisioned=True")
			}
		})
	}
}

// The class's frozen MaxLifetime is enforced by the adapter: an expired
// workspace tears down its linked RuntimePool and fails closed instead of
// executing indefinitely past the declared bound.
func TestACPExecutionWorkspaceAdapterEnforcesMaxLifetime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	provider := acpAdapterProvider()
	workspace := acpAdapterWorkspace(t, "acp-ws-pool")
	workspace.CreationTimestamp = metav1.NewTime(time.Now().Add(-2 * time.Hour))
	workspace.Spec.Lifecycle.MaxLifetime = &metav1.Duration{Duration: time.Hour}
	pool := acpAdapterLinkedPool(workspace.Namespace, workspace.Name)
	c := acpAdapterTestClient(t, provider, workspace, pool)

	reconciler := &ACPExecutionWorkspaceAdapterReconciler{Client: c, APIReader: c}
	for range 4 {
		if _, err := reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name},
		}); err != nil {
			t.Fatalf("adapter reconcile: %v", err)
		}
	}

	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, current); err != nil {
		t.Fatalf("get workspace: %v", err)
	}
	if current.Status.State != workspacev1alpha1.ExecutionWorkspaceStateFailed || current.Status.AttachedEpoch != 0 {
		t.Fatalf("expired workspace must fail closed with a revoked epoch: %+v", current.Status)
	}
	pools := &corev1alpha1.RuntimePoolList{}
	if err := c.List(ctx, pools, client.InNamespace(workspace.Namespace)); err != nil {
		t.Fatalf("list pools: %v", err)
	}
	if len(pools.Items) != 0 {
		t.Fatalf("the linked RuntimePool must be torn down on lifetime expiry, found %d", len(pools.Items))
	}
}

// A bounded, unexpired workspace keeps serving Ready but schedules its own
// enforcement wake-up so expiry fires without another triggering event.
func TestACPExecutionWorkspaceAdapterSchedulesMaxLifetimeEnforcement(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	provider := acpAdapterProvider()
	workspace := acpAdapterWorkspace(t, "acp-ws-pool")
	workspace.CreationTimestamp = metav1.NewTime(time.Now())
	workspace.Spec.Lifecycle.MaxLifetime = &metav1.Duration{Duration: time.Hour}
	c := acpAdapterTestClient(t, provider, workspace)

	reconciler := &ACPExecutionWorkspaceAdapterReconciler{Client: c, APIReader: c}
	result, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name},
	})
	if err != nil {
		t.Fatalf("adapter reconcile: %v", err)
	}
	if result.RequeueAfter <= 0 || result.RequeueAfter > time.Hour {
		t.Fatalf("a bounded workspace must requeue before its lifetime expiry, got %v", result.RequeueAfter)
	}
	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, current); err != nil {
		t.Fatalf("get workspace: %v", err)
	}
	if current.Status.State != workspacev1alpha1.ExecutionWorkspaceStateReady {
		t.Fatalf("unexpired workspace must stay Ready: %+v", current.Status)
	}
}

// Lifetime expiry is enforced BEFORE the admission gate: a workspace that
// lost current core admission (for example a Disabled provider) must not keep
// its linked RuntimePool executing past the frozen deadline.
func TestACPExecutionWorkspaceAdapterEnforcesMaxLifetimeWithoutAdmission(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	provider := acpAdapterProvider()
	workspace := acpAdapterWorkspace(t, "acp-ws-pool")
	workspace.CreationTimestamp = metav1.NewTime(time.Now().Add(-2 * time.Hour))
	workspace.Spec.Lifecycle.MaxLifetime = &metav1.Duration{Duration: time.Hour}
	// Core admission is not current for this generation.
	workspace.Spec.CoreAdmission = nil
	workspace.Status.Conditions = nil
	pool := acpAdapterLinkedPool(workspace.Namespace, workspace.Name)
	c := acpAdapterTestClient(t, provider, workspace, pool)

	reconciler := &ACPExecutionWorkspaceAdapterReconciler{Client: c, APIReader: c}
	for range 4 {
		if _, err := reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name},
		}); err != nil {
			t.Fatalf("adapter reconcile: %v", err)
		}
	}
	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, current); err != nil {
		t.Fatalf("get workspace: %v", err)
	}
	if current.Status.State != workspacev1alpha1.ExecutionWorkspaceStateFailed {
		t.Fatalf("expired unadmitted workspace must fail closed: %+v", current.Status)
	}
	pools := &corev1alpha1.RuntimePoolList{}
	if err := c.List(ctx, pools, client.InNamespace(workspace.Namespace)); err != nil {
		t.Fatalf("list pools: %v", err)
	}
	if len(pools.Items) != 0 {
		t.Fatalf("the linked RuntimePool must be torn down even without current admission, found %d", len(pools.Items))
	}

	// A bounded, unexpired, unadmitted workspace schedules its own expiry
	// wake-up instead of stranding the deadline on admission-denied passes.
	pending := acpAdapterWorkspace(t, "acp-ws-pool")
	pending.Name = "acp-ws-unadmitted-bounded"
	pending.CreationTimestamp = metav1.NewTime(time.Now())
	pending.Spec.Lifecycle.MaxLifetime = &metav1.Duration{Duration: time.Hour}
	pending.Spec.CoreAdmission = nil
	pending.Status.Conditions = nil
	c = acpAdapterTestClient(t, provider, pending)
	reconciler = &ACPExecutionWorkspaceAdapterReconciler{Client: c, APIReader: c}
	result, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: pending.Namespace, Name: pending.Name},
	})
	if err != nil {
		t.Fatalf("adapter reconcile: %v", err)
	}
	if result.RequeueAfter <= 0 || result.RequeueAfter > time.Hour {
		t.Fatalf("unadmitted bounded workspace must requeue before expiry, got %v", result.RequeueAfter)
	}
}

// The enforced attachment epoch is released only after the linked pool is
// proven absent: while the pool finalizer still drains, expiry reports Failed
// but must not clear the epoch, or FinalizeRevocation could delete the
// attachment authority before runtime quiescence is proven.
func TestACPExecutionWorkspaceAdapterExpiryHoldsEpochUntilPoolGone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	provider := acpAdapterProvider()
	workspace := acpAdapterWorkspace(t, "acp-ws-pool")
	workspace.CreationTimestamp = metav1.NewTime(time.Now().Add(-2 * time.Hour))
	workspace.Spec.Lifecycle.MaxLifetime = &metav1.Duration{Duration: time.Hour}
	// A live attachment past the deadline: expiry must not release the
	// enforced epoch before the pool is proven absent.
	workspace.Spec.AttachmentEpoch = 2
	workspace.Spec.Attachment = &workspacev1alpha1.ExecutionWorkspaceAttachment{
		TaskRef:        workspacev1alpha1.ObjectIdentityReference{Name: "expired-task", UID: types.UID("expired-task-uid")},
		Epoch:          2,
		TokenSHA256:    "sha256:" + strings.Repeat("e", 64),
		TokenSecretRef: workspacev1alpha1.SecretReference{Name: "expired-secret"},
		ExpiresAt:      metav1.Now(),
	}
	workspace.Status.AttachedEpoch = 2
	pool := acpAdapterLinkedPool(workspace.Namespace, workspace.Name)
	pool.Finalizers = append(pool.Finalizers, "orka.ai/test-drain-hold")
	c := acpAdapterTestClient(t, provider, workspace, pool)
	reconciler := &ACPExecutionWorkspaceAdapterReconciler{Client: c, APIReader: c}

	for range 2 {
		if _, err := reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name},
		}); err != nil {
			t.Fatalf("adapter reconcile: %v", err)
		}
	}
	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, current); err != nil {
		t.Fatalf("get workspace: %v", err)
	}
	if current.Status.State != workspacev1alpha1.ExecutionWorkspaceStateFailed {
		t.Fatalf("expired workspace must report Failed while the pool drains: %+v", current.Status)
	}
	if current.Status.AttachedEpoch != 2 {
		t.Fatalf("the enforced epoch must stay attached while the pool drains, got %d", current.Status.AttachedEpoch)
	}

	// The drain completes; the epoch is then revoked.
	heldPool := &corev1alpha1.RuntimePool{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: pool.Namespace, Name: pool.Name}, heldPool); err != nil {
		t.Fatalf("get pool: %v", err)
	}
	heldPool.Finalizers = nil
	if err := c.Update(ctx, heldPool); err != nil {
		t.Fatalf("release pool finalizer: %v", err)
	}
	for range 2 {
		if _, err := reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name},
		}); err != nil {
			t.Fatalf("adapter reconcile after drain: %v", err)
		}
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, current); err != nil {
		t.Fatalf("get settled workspace: %v", err)
	}
	if current.Status.AttachedEpoch != 0 {
		t.Fatalf("the epoch must be revoked once the pool is gone, got %d", current.Status.AttachedEpoch)
	}
}

// Lifetime expiry runs even when the live provider binding is unavailable
// (deleted or recreated provider): admission is withdrawn, but the linked
// RuntimePool must not run past the frozen hard deadline.
func TestACPExecutionWorkspaceAdapterEnforcesMaxLifetimeWithoutProvider(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	provider := acpAdapterProvider()
	// The provider was recreated: the workspace's frozen binding UID no
	// longer matches the live object.
	provider.UID = types.UID("recreated-provider-uid")
	workspace := acpAdapterWorkspace(t, "acp-ws-pool")
	workspace.CreationTimestamp = metav1.NewTime(time.Now().Add(-2 * time.Hour))
	workspace.Spec.Lifecycle.MaxLifetime = &metav1.Duration{Duration: time.Hour}
	pool := acpAdapterLinkedPool(workspace.Namespace, workspace.Name)
	c := acpAdapterTestClient(t, provider, workspace, pool)
	reconciler := &ACPExecutionWorkspaceAdapterReconciler{Client: c, APIReader: c}
	for range 4 {
		if _, err := reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name},
		}); err != nil {
			t.Fatalf("adapter reconcile: %v", err)
		}
	}
	pools := &corev1alpha1.RuntimePoolList{}
	if err := c.List(ctx, pools, client.InNamespace(workspace.Namespace)); err != nil {
		t.Fatalf("list pools: %v", err)
	}
	if len(pools.Items) != 0 {
		t.Fatalf("expiry must tear the pool down even without the exact provider binding, found %d", len(pools.Items))
	}
	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, current); err != nil {
		t.Fatalf("get workspace: %v", err)
	}
	if current.Status.State != workspacev1alpha1.ExecutionWorkspaceStateFailed {
		t.Fatalf("expired provider-unbound workspace must fail closed: %+v", current.Status)
	}
}

// Ownership rests on the admission-protected controller label: a live ACP
// provider must never claim a foreign workspace bound to a different
// provider UID with no ACP label - the maintenance path would report a
// terminal Deleted disposition for resources this adapter never managed.
func TestACPExecutionWorkspaceAdapterNeverClaimsUnlabeledWorkspaces(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	provider := acpAdapterProvider()
	workspace := acpAdapterWorkspace(t, "acp-ws-pool")
	workspace.Labels = nil
	// Deletion intent on the foreign workspace: the maintenance path must
	// not run for it even though a live same-name ACP provider exists.
	workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredDeleted
	c := acpAdapterTestClient(t, provider, workspace)
	reconcileACPWorkspaceAdapter(t, c, workspace)
	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, current); err != nil {
		t.Fatalf("get workspace: %v", err)
	}
	if current.Status.Disposition != nil || current.Status.State == workspacev1alpha1.ExecutionWorkspaceStateDeleted {
		t.Fatalf("an unlabeled workspace must never receive a terminal ACP disposition: %+v", current.Status)
	}
}

// Expiry is evaluated before the revocation bypass: settlement's
// BeginRevocation clears the attachment, and the bypass would otherwise
// publish AttachedEpoch=0 while the linked pool still drains - the premature
// release the expiry path exists to prevent.
func TestACPExecutionWorkspaceAdapterExpiryPrecedesRevocationBypass(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	provider := acpAdapterProvider()
	workspace := acpAdapterWorkspace(t, "acp-ws-pool")
	workspace.CreationTimestamp = metav1.NewTime(time.Now().Add(-2 * time.Hour))
	workspace.Spec.Lifecycle.MaxLifetime = &metav1.Duration{Duration: time.Hour}
	// Post-BeginRevocation shape: attachment cleared, enforced epoch standing.
	workspace.Spec.AttachmentEpoch = 3
	workspace.Status.AttachedEpoch = 3
	pool := acpAdapterLinkedPool(workspace.Namespace, workspace.Name)
	pool.Finalizers = append(pool.Finalizers, "orka.ai/test-drain-hold")
	c := acpAdapterTestClient(t, provider, workspace, pool)
	reconciler := &ACPExecutionWorkspaceAdapterReconciler{Client: c, APIReader: c}
	for range 2 {
		if _, err := reconciler.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name},
		}); err != nil {
			t.Fatalf("adapter reconcile: %v", err)
		}
	}
	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}, current); err != nil {
		t.Fatalf("get workspace: %v", err)
	}
	if current.Status.AttachedEpoch != 3 {
		t.Fatalf("the revocation bypass must not release the epoch of an expired workspace while its pool drains, got %d",
			current.Status.AttachedEpoch)
	}
	if current.Status.State != workspacev1alpha1.ExecutionWorkspaceStateFailed {
		t.Fatalf("the expired workspace must take the expiry path: %+v", current.Status)
	}
}
