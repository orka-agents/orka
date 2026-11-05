package controller

import (
	"testing"

	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
)

func TestFailedTaskStopsActiveJobDuringStorageOutage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		active int32
	}{
		{"active Job is stopped", 1},
		{"inactive Job is retained", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Name: "failed-task", Namespace: "default", UID: "task-uid", Finalizers: []string{labels.TaskFinalizer}},
				Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
				Status: corev1alpha1.TaskStatus{
					Phase: corev1alpha1.TaskPhaseRunning, JobName: "failed-job", JobUID: "job-uid",
				},
			}
			job := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: task.Status.JobName, Namespace: task.Namespace, UID: "job-uid"},
				Status:     batchv1.JobStatus{Active: tc.active},
			}
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pending-pod", Namespace: task.Namespace, Labels: map[string]string{labels.LabelTask: task.Name}},
				Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{
					Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: "no suitable node",
				}}},
			}
			namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: task.Namespace, UID: "namespace-uid"}}
			r := newUnitReconciler(newTestScheme(), task, job, pod, namespace)
			originalStore := r.ResultStore
			r.ResultStore = transientGetResultErrorStore{ResultStore: originalStore, err: store.ErrUnavailable}
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)}
			_, err := r.Reconcile(t.Context(), request)
			require.ErrorIs(t, err, store.ErrUnavailable)
			lookupErr := r.Get(t.Context(), client.ObjectKeyFromObject(job), &batchv1.Job{})
			if tc.active > 0 {
				require.True(t, apierrors.IsNotFound(lookupErr), "failed work must not start later while finalization waits")
			} else {
				require.NoError(t, lookupErr)
			}
			current := &corev1alpha1.Task{}
			require.NoError(t, r.Get(t.Context(), request.NamespacedName, current))
			require.Equal(t, corev1alpha1.TaskPhaseRunning, current.Status.Phase)
			require.Nil(t, current.Status.CompletionTime)
			r.ResultStore = originalStore
			require.NoError(t, originalStore.SaveResult(t.Context(), task.Namespace, task.Name, []byte("retained result")))
			_, err = r.Reconcile(t.Context(), request)
			require.NoError(t, err)
			require.NoError(t, r.Get(t.Context(), request.NamespacedName, current))
			require.Equal(t, corev1alpha1.TaskPhaseFailed, current.Status.Phase)
			require.NotNil(t, current.Status.ResultRef)
			require.True(t, current.Status.ResultRef.Available)
		})
	}
}
