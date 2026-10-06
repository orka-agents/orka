package controller

import (
	"context"
	"testing"

	workspace "github.com/orka-agents/orka-workspace/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestWorkspaceCheckpointRoutingRequiresExactSourceAndProvider(t *testing.T) {
	for _, name := range []string{"valid", "replaced source", "replaced provider", "unsupported export"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := newExternalRuntimePoolFixture(t)
			provider := f.provider.DeepCopy()
			provider.Spec.ControllerName = f.workspace.Labels[workspace.ProviderControllerLabel]
			provider.Status.SupportedFeatures = append(provider.Status.SupportedFeatures, workspace.WorkspaceFeatureCheckpoint)
			if name == "replaced provider" {
				provider.UID = "new-provider"
			}
			if name == "unsupported export" {
				provider.Status.SupportedFeatures = nil
			}
			if err := f.r.Update(ctx, provider); err != nil {
				t.Fatal(err)
			}
			checkpoint := &workspace.ExecutionWorkspaceCheckpoint{ObjectMeta: metav1.ObjectMeta{Namespace: f.workspace.Namespace, Name: "export", UID: "checkpoint-uid"}, Spec: workspace.ExecutionWorkspaceCheckpointSpec{WorkspaceRef: workspace.ObjectIdentityReference{Name: f.workspace.Name, UID: f.workspace.UID}}}
			if name == "replaced source" {
				checkpoint.Spec.WorkspaceRef.UID = "previous-workspace"
			}
			if err := f.r.Create(ctx, checkpoint); err != nil {
				t.Fatal(err)
			}
			r := &WorkspaceCheckpointRoutingReconciler{Client: f.r.Client, APIReader: f.r.Client}
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(checkpoint)})
			if (err == nil) != (name == "valid") {
				t.Fatalf("routing error = %v", err)
			}
			if err := f.r.Get(ctx, client.ObjectKeyFromObject(checkpoint), checkpoint); err != nil {
				t.Fatal(err)
			}
			if name == "valid" {
				if checkpoint.Labels[workspaceCheckpointProviderNameLabel] != provider.Name || checkpoint.Labels[workspace.ProviderControllerLabel] != provider.Spec.ControllerName {
					t.Fatal("checkpoint did not pin the exact provider route")
				}
			} else if len(checkpoint.Labels) != 0 {
				t.Fatal("invalid source acquired provider authority")
			}
		})
	}
}
