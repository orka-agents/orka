package controller

import (
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestRepositoryMonitorReconcileScopesImplementationLabelOverlap(t *testing.T) {
	for _, tc := range []struct {
		name, label    string
		protect, pause []string
		issues, labels bool
		valid          bool
	}{
		{name: "PR-only default protected label", protect: []string{"orka:implement"}, labels: true, valid: true},
		{name: "PR-only custom protected label", label: "ship", protect: []string{"SHIP"}, labels: true, valid: true},
		{name: "labels disabled default protected label", protect: []string{"orka:implement"}, issues: true, valid: true},
		{name: "labels disabled custom pause label", label: "ship", pause: []string{"SHIP"}, issues: true, valid: true},
		{name: "active default protected label", protect: []string{" ORKA:IMPLEMENT "}, issues: true, labels: true},
		{name: "active custom protected label", label: "ship", protect: []string{" SHIP "}, issues: true, labels: true},
		{name: "active default pause label", pause: []string{"orka:implement"}, issues: true, labels: true},
		{name: "active custom pause label", label: "ship", pause: []string{"SHIP"}, issues: true, labels: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			monitor, secret := repositoryMonitorInventoryTestObjects("implementation-label-overlap")
			monitor.Spec.Targets.Issues.Enabled = tc.issues
			monitor.Spec.Triggers.GitHub.Labels.Enabled = tc.labels
			monitor.Spec.Triggers.GitHub.Labels.Issues.Implement = tc.label
			monitor.Spec.Policy.ProtectedLabels, monitor.Spec.Policy.PauseLabels = tc.protect, tc.pause
			scheme := runtime.NewScheme()
			require.NoError(t, corev1alpha1.AddToScheme(scheme))
			require.NoError(t, corev1.AddToScheme(scheme))
			db := setupControllerSQLiteStore(t)
			cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(monitor).WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).Build()
			r := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: db}
			key := types.NamespacedName{Namespace: monitor.Namespace, Name: monitor.Name}
			_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
			require.NoError(t, err)
			var current corev1alpha1.RepositoryMonitor
			require.NoError(t, cl.Get(t.Context(), key, &current))
			_, err = db.GetRepositoryMonitor(t.Context(), monitor.Namespace, monitor.Name)
			if tc.valid {
				require.NoError(t, err)
				require.Equal(t, repositoryMonitorPhaseReady, current.Status.Phase)
			} else {
				require.ErrorIs(t, err, store.ErrNotFound)
				require.Equal(t, repositoryMonitorPhaseError, current.Status.Phase)
				require.Len(t, current.Status.Conditions, 1)
				require.Equal(t, "InvalidCommandLabels", current.Status.Conditions[0].Reason)
				require.Contains(t, current.Status.Conditions[0].Message, "implementation label must not also be a pause or protected label")
			}
		})
	}
}
