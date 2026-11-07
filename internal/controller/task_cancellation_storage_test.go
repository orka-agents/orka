package controller

import (
	"context"
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
	"github.com/orka-agents/orka/internal/storageclient"
	"github.com/orka-agents/orka/internal/storageclient/storageclienttest"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

type cancellationStorageFence struct {
	storageclient.FenceSource
	err error
}

func (s *cancellationStorageFence) WriteFence(ctx context.Context, contract string) (string, store.ControllerEpochFence, error) {
	if s.err != nil {
		return "", store.ControllerEpochFence{}, s.err
	}
	return s.FenceSource.WriteFence(ctx, contract)
}

func TestRequestedCancellationStopsJobWhenSharedStateWritesAreRefused(t *testing.T) {
	for _, requested := range []bool{true, false} {
		name := "cancellation requested"
		if !requested {
			name = "no cancellation requested"
		}
		t.Run(name, func(t *testing.T) {
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cancel-task", Namespace: "default", UID: "task-uid",
					Finalizers: []string{labels.TaskFinalizer},
				},
				Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
				Status: corev1alpha1.TaskStatus{
					Phase: corev1alpha1.TaskPhaseRunning, JobName: "cancel-job", JobUID: "job-uid",
				},
			}
			if requested {
				task.Annotations = map[string]string{labels.AnnotationCancelRequested: "stop"}
			}
			job := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: task.Status.JobName, Namespace: task.Namespace, UID: "job-uid"},
				Status:     batchv1.JobStatus{Active: 1},
			}
			namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: task.Namespace, UID: "namespace-uid"}}
			r := newUnitReconciler(newTestScheme(), task, job, namespace)
			backend := r.ResultStore.(*sqlite.Store)
			fences := &cancellationStorageFence{
				FenceSource: &storageclienttest.FollowDataset{Dataset: backend}, err: store.ErrUnavailable,
			}
			shared, err := storageclient.New(storageclient.Options{State: backend, Artifacts: backend, Embedded: backend, Fences: fences})
			require.NoError(t, err)
			// Production supplies one application client for usage and results.
			// Closing its state fence refuses the early RegisterUsageTask write.
			r.ResultStore, r.ExecutionEventStore = shared, shared
			r.MessageStore, r.PlanStore = shared, shared
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)}
			_, err = r.Reconcile(t.Context(), request)
			require.ErrorIs(t, err, store.ErrNotReady)
			lookupErr := r.Get(t.Context(), client.ObjectKeyFromObject(job), &batchv1.Job{})
			if requested {
				require.True(t, apierrors.IsNotFound(lookupErr), "an early usage failure must not keep cancelled work running")
			} else {
				require.NoError(t, lookupErr, "storage failure alone must not cancel work")
			}
			current := &corev1alpha1.Task{}
			require.NoError(t, r.Get(t.Context(), request.NamespacedName, current))
			require.Equal(t, corev1alpha1.TaskPhaseRunning, current.Status.Phase)
			require.Nil(t, current.Status.CompletionTime)
			if requested {
				fences.err = nil
				require.NoError(t, shared.SaveResult(t.Context(), task.Namespace, task.Name, []byte("retained result")))
				_, err = r.Reconcile(t.Context(), request)
				require.NoError(t, err)
				require.NoError(t, r.Get(t.Context(), request.NamespacedName, current))
				require.Equal(t, corev1alpha1.TaskPhaseCancelled, current.Status.Phase)
				require.NotNil(t, current.Status.ResultRef)
				require.True(t, current.Status.ResultRef.Available)
			}
		})
	}
}

func TestRequestedCancellationStopsJobDuringStorageOutage(t *testing.T) {
	for _, storageErr := range []error{store.ErrNotReady, store.ErrUnavailable} {
		t.Run(storageErr.Error(), func(t *testing.T) {
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cancel-task", Namespace: "default", UID: "task-uid",
					Finalizers:  []string{labels.TaskFinalizer},
					Annotations: map[string]string{labels.AnnotationCancelRequested: "stop"},
				},
				Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
				Status: corev1alpha1.TaskStatus{
					Phase: corev1alpha1.TaskPhaseRunning, JobName: "cancel-job", JobUID: "job-uid",
				},
			}
			job := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: task.Status.JobName, Namespace: task.Namespace, UID: "job-uid"},
				Status:     batchv1.JobStatus{Active: 1},
			}
			namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: task.Namespace, UID: "namespace-uid"}}
			r := newUnitReconciler(newTestScheme(), task, job, namespace)
			originalStore := r.ResultStore
			r.ResultStore = transientGetResultErrorStore{ResultStore: originalStore, err: storageErr}
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)}
			_, err := r.Reconcile(t.Context(), request)
			require.ErrorIs(t, err, storageErr)
			require.True(t, apierrors.IsNotFound(r.Get(t.Context(), client.ObjectKeyFromObject(job), &batchv1.Job{})),
				"cancellation must stop the workload while result finalization waits")
			current := &corev1alpha1.Task{}
			require.NoError(t, r.Get(t.Context(), request.NamespacedName, current))
			require.Equal(t, corev1alpha1.TaskPhaseRunning, current.Status.Phase)
			require.Nil(t, current.Status.CompletionTime)
			// Storage recovery must still collect the worker's result and finish
			// cancellation; stopping the Job must not mark finalization done.
			r.ResultStore = originalStore
			require.NoError(t, originalStore.SaveResult(t.Context(), task.Namespace, task.Name, []byte("retained result")))
			_, err = r.Reconcile(t.Context(), request)
			require.NoError(t, err)
			require.NoError(t, r.Get(t.Context(), request.NamespacedName, current))
			require.Equal(t, corev1alpha1.TaskPhaseCancelled, current.Status.Phase)
			require.NotNil(t, current.Status.CompletionTime)
			require.NotNil(t, current.Status.ResultRef)
			require.True(t, current.Status.ResultRef.Available)
		})
	}
}

func TestCancellationCollectsResultBeforeJobCleanup(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "cancel-task", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
		Status: corev1alpha1.TaskStatus{
			Phase: corev1alpha1.TaskPhaseRunning, JobName: "cancel-job",
		},
	}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: task.Status.JobName, Namespace: task.Namespace}}
	r := newUnitReconciler(newTestScheme(), task, job)
	require.NoError(t, r.ResultStore.SaveResult(t.Context(), task.Namespace, task.Name, []byte("retained result")))
	_, err := r.completeTask(t.Context(), task, corev1alpha1.TaskPhaseCancelled, "stop")
	require.NoError(t, err)
	require.NotNil(t, task.Status.ResultRef)
	require.True(t, task.Status.ResultRef.Available)
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(job), &batchv1.Job{}),
		"healthy finalization keeps the existing output collection and cleanup order")
	_, err = r.handleCompleted(t.Context(), task)
	require.NoError(t, err)
	require.True(t, apierrors.IsNotFound(r.Get(t.Context(), client.ObjectKeyFromObject(job), &batchv1.Job{})))
}
