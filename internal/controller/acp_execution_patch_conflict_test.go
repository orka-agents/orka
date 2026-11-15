package controller

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// newExecutionPatchFixture stores a Pending agent Task whose prompt is being
// submitted, and returns the Task as last read from the API server.
func newExecutionPatchFixture(t *testing.T) (client.WithWatch, *corev1alpha1.Task) {
	t.Helper()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "execution-patch", UID: types.UID("execution-patch-uid")},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending, Execution: &corev1alpha1.TaskExecutionStatus{
			Attempt: 1, PromptID: "prompt-1", State: corev1alpha1.TaskExecutionStateSubmitting,
		}},
	}
	kube := fake.NewClientBuilder().WithScheme(newTestScheme()).
		WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task).Build()
	if err := kube.Get(t.Context(), client.ObjectKeyFromObject(task), task); err != nil {
		t.Fatal(err)
	}
	return kube, task
}

// markPromptRunning is the execution mutation the dispatcher applies when the
// runtime accepts a prompt.
func markPromptRunning(status *corev1alpha1.TaskExecutionStatus) {
	status.State = corev1alpha1.TaskExecutionStateRunning
	status.Reason = ""
	status.Message = ""
	status.LastTransitionTime = nowMeta()
}

func countStatusPatchConflicts(conflicts *int) func(context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
	return func(ctx context.Context, c client.Client, subresource string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
		err := c.SubResource(subresource).Patch(ctx, obj, patch, opts...)
		if apierrors.IsConflict(err) {
			*conflicts++
		}
		return err
	}
}

func TestACPPatchExecutionKeepsConcurrentTerminalPhase(t *testing.T) {
	for _, test := range []struct {
		name       string
		concurrent corev1alpha1.TaskPhase
		wantPhase  corev1alpha1.TaskPhase
		wantReads  int
		wantRetry  int
	}{
		{name: "uncontended", wantPhase: corev1alpha1.TaskPhaseRunning, wantReads: 1},
		{name: "cancelled", concurrent: corev1alpha1.TaskPhaseCancelled, wantPhase: corev1alpha1.TaskPhaseCancelled, wantReads: 2, wantRetry: 1},
		{name: "failed", concurrent: corev1alpha1.TaskPhaseFailed, wantPhase: corev1alpha1.TaskPhaseFailed, wantReads: 2, wantRetry: 1},
		{name: "succeeded", concurrent: corev1alpha1.TaskPhaseSucceeded, wantPhase: corev1alpha1.TaskPhaseSucceeded, wantReads: 2, wantRetry: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			kube, task := newExecutionPatchFixture(t)
			key := client.ObjectKeyFromObject(task)
			completed := metav1.NewTime(time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC))
			const message = "finished by another writer"

			// Another writer finishes the Task after the dispatcher has read it
			// as Pending but before the dispatcher's Running transition lands.
			reads, conflicts := 0, 0
			racing := interceptor.NewClient(kube, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, objectKey client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if err := c.Get(ctx, objectKey, obj, opts...); err != nil {
						return err
					}
					if _, ok := obj.(*corev1alpha1.Task); !ok || objectKey != key {
						return nil
					}
					reads++
					if reads > 1 || test.concurrent == "" {
						return nil
					}
					finished := &corev1alpha1.Task{}
					if err := c.Get(ctx, key, finished); err != nil {
						return err
					}
					finished.Status.Phase = test.concurrent
					finished.Status.CompletionTime = &completed
					finished.Status.Message = message
					return c.Status().Update(ctx, finished)
				},
				SubResourcePatch: countStatusPatchConflicts(&conflicts),
			})

			dispatcher := &ACPDispatcher{Client: racing, APIReader: racing}
			if err := dispatcher.patchExecution(ctx, task, markPromptRunning); err != nil {
				t.Fatal(err)
			}

			latest := &corev1alpha1.Task{}
			if err := kube.Get(ctx, key, latest); err != nil {
				t.Fatal(err)
			}
			if latest.Status.Phase != test.wantPhase {
				t.Fatalf("phase = %s, want %s", latest.Status.Phase, test.wantPhase)
			}
			if latest.Status.Execution == nil || latest.Status.Execution.State != corev1alpha1.TaskExecutionStateRunning {
				t.Fatalf("execution status = %#v, want state Running", latest.Status.Execution)
			}
			if test.concurrent == "" {
				if latest.Status.StartTime == nil {
					t.Fatal("startTime was not recorded when the Task started running")
				}
			} else {
				if latest.Status.StartTime != nil {
					t.Fatalf("startTime = %v, want none for a Task that never started running", latest.Status.StartTime)
				}
				if latest.Status.CompletionTime == nil || !latest.Status.CompletionTime.Equal(&completed) || latest.Status.Message != message {
					t.Fatalf("concurrent terminal status was changed: completionTime=%v message=%q", latest.Status.CompletionTime, latest.Status.Message)
				}
			}
			if reads != test.wantReads || conflicts != test.wantRetry {
				t.Fatalf("reads=%d conflicts=%d, want reads=%d conflicts=%d", reads, conflicts, test.wantReads, test.wantRetry)
			}
			if task.Status.Phase != test.wantPhase {
				t.Fatalf("caller status phase = %s, want the persisted %s", task.Status.Phase, test.wantPhase)
			}
		})
	}
}

func TestACPPatchExecutionStopsAfterRepeatedConflicts(t *testing.T) {
	ctx := t.Context()
	kube, task := newExecutionPatchFixture(t)
	key := client.ObjectKeyFromObject(task)
	before := task.DeepCopy()

	// Another writer updates the Task after every dispatcher read, so every
	// dispatcher write is stale.
	reads, conflicts := 0, 0
	busy := interceptor.NewClient(kube, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, objectKey client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := c.Get(ctx, objectKey, obj, opts...); err != nil {
				return err
			}
			if _, ok := obj.(*corev1alpha1.Task); !ok || objectKey != key {
				return nil
			}
			reads++
			other := &corev1alpha1.Task{}
			if err := c.Get(ctx, key, other); err != nil {
				return err
			}
			other.Status.Message = fmt.Sprintf("concurrent write %d", reads)
			return c.Status().Update(ctx, other)
		},
		SubResourcePatch: countStatusPatchConflicts(&conflicts),
	})

	dispatcher := &ACPDispatcher{Client: busy, APIReader: busy}
	done := make(chan error, 1)
	go func() { done <- dispatcher.patchExecution(ctx, task, markPromptRunning) }()
	var err error
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("patchExecution kept retrying a write that always conflicts")
	}

	if !apierrors.IsConflict(err) {
		t.Fatalf("err = %v, want the conflict once retries are exhausted", err)
	}
	if reads != retry.DefaultRetry.Steps || conflicts != retry.DefaultRetry.Steps {
		t.Fatalf("reads=%d conflicts=%d, want %d of each", reads, conflicts, retry.DefaultRetry.Steps)
	}
	latest := &corev1alpha1.Task{}
	if err := kube.Get(ctx, key, latest); err != nil {
		t.Fatal(err)
	}
	if latest.Status.Phase != corev1alpha1.TaskPhasePending || latest.Status.StartTime != nil ||
		latest.Status.Execution == nil || latest.Status.Execution.State != corev1alpha1.TaskExecutionStateSubmitting {
		t.Fatalf("a rejected write changed the stored status: %#v", latest.Status)
	}
	if !reflect.DeepEqual(task.Status, before.Status) {
		t.Fatalf("caller status changed after a failed write: %#v", task.Status)
	}
}

func TestACPPatchExecutionReadsPastStaleCache(t *testing.T) {
	ctx := t.Context()
	kube, task := newExecutionPatchFixture(t)
	key := client.ObjectKeyFromObject(task)

	// The informer cache still holds the Task from before the dispatcher's
	// previous status write.
	stale := task.DeepCopy()
	task.Status.Execution.RuntimeSessionSupervisorBootID = "boot-1"
	if err := kube.Status().Update(ctx, task); err != nil {
		t.Fatal(err)
	}

	cachedReads, apiReads, conflicts := 0, 0, 0
	cached := interceptor.NewClient(kube, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, objectKey client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if current, ok := obj.(*corev1alpha1.Task); ok && objectKey == key {
				cachedReads++
				stale.DeepCopyInto(current)
				return nil
			}
			return c.Get(ctx, objectKey, obj, opts...)
		},
		SubResourcePatch: countStatusPatchConflicts(&conflicts),
	})
	api := interceptor.NewClient(kube, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, objectKey client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1alpha1.Task); ok && objectKey == key {
				apiReads++
			}
			return c.Get(ctx, objectKey, obj, opts...)
		},
	})

	dispatcher := &ACPDispatcher{Client: cached, APIReader: api}
	if err := dispatcher.patchExecution(ctx, task, markPromptRunning); err != nil {
		t.Fatal(err)
	}
	if cachedReads != 0 || apiReads != 1 || conflicts != 0 {
		t.Fatalf("cachedReads=%d apiReads=%d conflicts=%d, want one API read and no conflict", cachedReads, apiReads, conflicts)
	}
	latest := &corev1alpha1.Task{}
	if err := kube.Get(ctx, key, latest); err != nil {
		t.Fatal(err)
	}
	if latest.Status.Phase != corev1alpha1.TaskPhaseRunning || latest.Status.Execution == nil ||
		latest.Status.Execution.State != corev1alpha1.TaskExecutionStateRunning ||
		latest.Status.Execution.RuntimeSessionSupervisorBootID != "boot-1" {
		t.Fatalf("status = %#v, want Running with the earlier write preserved", latest.Status)
	}
}
