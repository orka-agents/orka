package controller

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

func TestACPTerminalStatusPreservesCompletionTimeAfterConflict(t *testing.T) {
	for _, test := range []struct {
		name     string
		phase    corev1alpha1.TaskPhase
		delivery corev1alpha1.TaskDeliveryState
		settle   func(*ACPDispatcher, context.Context, *corev1alpha1.Task, corev1alpha1.TaskDeliveryStatus, string) error
	}{
		{
			name: "success", phase: corev1alpha1.TaskPhaseSucceeded,
			delivery: corev1alpha1.TaskDeliveryStateNotRequested, settle: (*ACPDispatcher).completeSuccessWithDelivery,
		},
		{
			name: "delivery-failure", phase: corev1alpha1.TaskPhaseFailed,
			delivery: corev1alpha1.TaskDeliveryStateDeliveryConflict, settle: (*ACPDispatcher).failTaskForDelivery,
		},
		{
			name: "cancellation", phase: corev1alpha1.TaskPhaseCancelled,
			delivery: corev1alpha1.TaskDeliveryStateCancelledBeforePublish, settle: (*ACPDispatcher).cancelTaskAfterExecution,
		},
	} {
		for _, writer := range []string{"direct", "outbox"} {
			t.Run(test.name+"/"+writer, func(t *testing.T) {
				ctx := context.Background()
				task := &corev1alpha1.Task{
					ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "completion-time", UID: types.UID("completion-time-uid")},
					Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent,
						SessionRef: &corev1alpha1.SessionReference{Name: "session"}},
					Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning,
						Execution: &corev1alpha1.TaskExecutionStatus{
							Attempt: 1, PromptID: "prompt-1", State: corev1alpha1.TaskExecutionStateSucceeded,
							Outcome: corev1alpha1.TaskExecutionOutcomeSucceeded,
						}},
				}
				kube := fake.NewClientBuilder().WithScheme(newTestScheme()).
					WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task).Build()
				key := client.ObjectKeyFromObject(task)
				if err := kube.Get(ctx, key, task); err != nil {
					t.Fatal(err)
				}
				stale := task.DeepCopy()
				historical := metav1.NewTime(time.Date(2024, 9, 17, 8, 0, 0, 0, time.UTC))
				task.Status.Phase = test.phase
				task.Status.CompletionTime = &historical
				if err := kube.Status().Update(ctx, task); err != nil {
					t.Fatal(err)
				}

				// The API has committed another writer's terminal status, but the
				// first cache read still returns the earlier incomplete Task.
				reads, conflicts := 0, 0
				cached := interceptor.NewClient(kube, interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, objectKey client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if current, ok := obj.(*corev1alpha1.Task); ok && objectKey == key {
							reads++
							if reads == 1 {
								stale.DeepCopyInto(current)
								return nil
							}
						}
						return c.Get(ctx, objectKey, obj, opts...)
					},
					SubResourcePatch: func(ctx context.Context, c client.Client, subresource string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
						err := c.SubResource(subresource).Patch(ctx, obj, patch, opts...)
						if apierrors.IsConflict(err) {
							conflicts++
						}
						return err
					},
				})
				delivery := corev1alpha1.TaskDeliveryStatus{
					State: test.delivery, Outcome: corev1alpha1.TaskDeliveryOutcome(test.delivery),
				}
				if writer == "direct" {
					dispatcher := &ACPDispatcher{Client: cached}
					if err := test.settle(dispatcher, ctx, task, delivery, "terminal delivery settled"); err != nil {
						t.Fatal(err)
					}
				} else {
					payload, err := json.Marshal(taskTerminalProjection{
						Namespace: task.Namespace, Task: task.Name, TaskUID: string(task.UID), Attempt: 1,
						Phase: test.phase, Execution: *task.Status.Execution, Delivery: &delivery,
					})
					if err != nil {
						t.Fatal(err)
					}
					projector := &ACPOutboxProjector{Client: cached}
					if _, err := projector.deliver(ctx, store.OutboxProjection{ProjectionKind: taskTerminalProjectionKind, Payload: payload}); err != nil {
						t.Fatal(err)
					}
				}

				latest := &corev1alpha1.Task{}
				if err := kube.Get(ctx, key, latest); err != nil {
					t.Fatal(err)
				}
				if latest.Status.CompletionTime == nil || !latest.Status.CompletionTime.Equal(&historical) {
					t.Fatalf("stale write replaced completionTime: got %v, want %v", latest.Status.CompletionTime, historical)
				}
				if latest.Status.Phase != test.phase || latest.Status.Delivery == nil || latest.Status.Delivery.State != test.delivery {
					t.Fatalf("status after retry = %#v, want phase %s and delivery %s", latest.Status, test.phase, test.delivery)
				}
				if conflicts != 1 || reads != 2 {
					t.Fatalf("conflicts=%d reads=%d, want one rejected stale write followed by a fresh read", conflicts, reads)
				}
			})
		}
	}
}
