package controller

import (
	"context"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestSeriesPrefixProtectedTaskCannotDispatch(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, batchv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "protected", Namespace: "test", UID: "exact-task"},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI,
			Enforcement: &corev1alpha1.EnforcementRequirement{
				Format: "example.policy/v1", Policy: []byte("complete policy"),
				RequiredCapabilities: []string{"network/egress-allow/v1"}, Mode: "enforce",
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task).Build()
	r := &TaskReconciler{Client: c, Scheme: scheme}
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: task.Name, Namespace: task.Namespace},
	})
	require.Error(t, err)
	var jobs batchv1.JobList
	var pods corev1.PodList
	require.NoError(t, c.List(context.Background(), &jobs))
	require.NoError(t, c.List(context.Background(), &pods))
	require.Empty(t, jobs.Items)
	require.Empty(t, pods.Items)
}
