package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

// unacceptedResultStore refuses results, as the storage client does while
// the controller's fence is not open or an earlier write's outcome is unknown.
type unacceptedResultStore struct {
	store.ResultStore
	err error
}

func (s *unacceptedResultStore) SaveResult(context.Context, store.ResultKey, []byte) error {
	return s.err
}

// A Task whose result the store has not accepted yet, or may have saved, is
// not completed without its result: the completion is retried.
func TestTaskCompletionRetriesAResultTheStoreHasNotAccepted(t *testing.T) {
	for name, storageErr := range map[string]error{
		"not ready":       fmt.Errorf("save: %w", store.ErrNotReady),
		"outcome unknown": fmt.Errorf("save: %w", store.ErrOutcomeUnknown),
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Name: "container-task", Namespace: "default", UID: types.UID("container-task-uid")},
				Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
				Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning, Attempts: 1, JobName: "container-task-job"},
			}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: "container-task-job-pod", Namespace: "default", Labels: map[string]string{"job-name": "container-task-job"},
			}}
			reconciler := newUnitReconciler(newTestScheme(), task, pod)
			reconciler.KubeClient = k8sfake.NewSimpleClientset(pod)
			results := reconciler.ResultStore
			reconciler.ResultStore = &unacceptedResultStore{ResultStore: results, err: storageErr}

			if _, err := reconciler.completeExecutedTask(ctx, task.DeepCopy(), corev1alpha1.TaskPhaseSucceeded, "completed"); !errors.Is(err, storageErr) {
				t.Fatalf("completion error = %v, want the storage error so the Task is retried", err)
			}
			current := &corev1alpha1.Task{}
			if err := reconciler.Get(ctx, client.ObjectKeyFromObject(task), current); err != nil {
				t.Fatal(err)
			}
			if current.Status.Phase != corev1alpha1.TaskPhaseRunning || current.Status.ResultRef != nil {
				t.Fatalf("Task after a refused result = phase %q result %#v, want Running without a result", current.Status.Phase, current.Status.ResultRef)
			}

			reconciler.ResultStore = results
			if _, err := reconciler.completeExecutedTask(ctx, current, corev1alpha1.TaskPhaseSucceeded, "completed"); err != nil {
				t.Fatalf("completion after the store accepts results: %v", err)
			}
			if err := reconciler.Get(ctx, client.ObjectKeyFromObject(task), current); err != nil {
				t.Fatal(err)
			}
			if current.Status.Phase != corev1alpha1.TaskPhaseSucceeded || current.Status.ResultRef == nil || !current.Status.ResultRef.Available {
				t.Fatalf("Task after retry = phase %q result %#v, want Succeeded with its result", current.Status.Phase, current.Status.ResultRef)
			}
		})
	}
}
