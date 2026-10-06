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

func TestRepositoryMonitorReconcileValidatesConsumedEntryLabelScope(t *testing.T) {
	for _, tc := range []struct {
		name, label             string
		includes                []string
		consume, labels, issues bool
		valid                   bool
	}{
		{name: "default entry", includes: []string{" ORKA:IMPLEMENT "}, consume: true, labels: true, issues: true},
		{name: "custom entry", label: "ship", includes: []string{"ready", "SHIP"}, consume: true, labels: true, issues: true},
		{name: "retained entry", includes: []string{"orka:implement"}, labels: true, issues: true, valid: true},
		{name: "other scope label", includes: []string{"ready"}, consume: true, labels: true, issues: true, valid: true},
		{name: "command labels disabled", includes: []string{"orka:implement"}, consume: true, issues: true, valid: true},
		{name: "issues disabled", includes: []string{"orka:implement"}, consume: true, labels: true, valid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			monitor, secret := repositoryMonitorInventoryTestObjects("consumed-entry")
			monitor.Spec.Targets.Issues.Enabled, monitor.Spec.Targets.Issues.IncludeLabels = tc.issues, tc.includes
			monitor.Spec.Triggers.GitHub.Labels.Enabled = tc.labels
			monitor.Spec.Triggers.GitHub.Labels.ConsumeCommandLabels = tc.consume
			monitor.Spec.Triggers.GitHub.Labels.Issues.Implement = tc.label
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
				require.Contains(t, current.Status.Conditions[0].Message, "includeLabels must not contain the implementation label")
			}
		})
	}
}
