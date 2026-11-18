package controller

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

func TestRepositoryMonitorReconcileControllerMutationsRequireForgeCredential(t *testing.T) {
	tests := []struct {
		name    string
		enabled bool
		repair  bool
		forge   *corev1.LocalObjectReference
		valid   bool
	}{
		{name: "missing reference", enabled: true},
		{name: "blank reference", enabled: true, forge: &corev1.LocalObjectReference{Name: " "}},
		{name: "valid reference", enabled: true, forge: &corev1.LocalObjectReference{Name: repositoryMonitorTestForgeCredential}, valid: true},
		{name: "agentless repair missing reference", repair: true},
		{name: "agentless repair blank reference", repair: true, forge: &corev1.LocalObjectReference{Name: " "}},
		{name: "agentless repair valid reference", repair: true, forge: &corev1.LocalObjectReference{Name: repositoryMonitorTestForgeCredential}, valid: true},
		{name: "read-only publication disabled", valid: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			monitorStore := setupControllerSQLiteStore(t)
			scheme := runtime.NewScheme()
			require.NoError(t, corev1alpha1.AddToScheme(scheme))
			require.NoError(t, corev1.AddToScheme(scheme))
			monitor := repositoryMonitorReviewIngestTestMonitor("publication-credentials")
			monitor.Spec.Review.Publish.Enabled = tt.enabled
			monitor.Spec.Repair.Enabled = tt.repair
			monitor.Spec.ForgeCredentialRef = tt.forge
			cl := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
				WithObjects(repositoryMonitorControllerObjects(monitor)...).
				Build()
			reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore}
			key := types.NamespacedName{Namespace: monitor.Namespace, Name: monitor.Name}
			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: key})
			require.NoError(t, err)
			var current corev1alpha1.RepositoryMonitor
			require.NoError(t, cl.Get(ctx, key, &current))
			_, err = monitorStore.GetRepositoryMonitor(ctx, monitor.Namespace, monitor.Name)
			if tt.valid {
				require.NoError(t, err)
				require.Equal(t, repositoryMonitorPhaseReady, current.Status.Phase)
			} else {
				require.ErrorIs(t, err, store.ErrNotFound)
				require.Equal(t, repositoryMonitorPhaseError, current.Status.Phase)
				require.Len(t, current.Status.Conditions, 1)
				require.Equal(t, repositoryMonitorReasonGitSecretInvalid, current.Status.Conditions[0].Reason)
				require.Contains(t, current.Status.Conditions[0].Message, "spec.forgeCredentialRef is required")
			}
		})
	}
}
