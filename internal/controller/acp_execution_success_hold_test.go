package controller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

// No ACP write newly reports execution success while storage is held for
// recovery, including one whose Task phase reports a failed delivery or a
// cancellation, one that leaves the Task running, and a restored Task's
// settlement; once the hold is released the same write goes through.
func TestACPExecutionSuccessWaitsWhileStorageIsHeld(t *testing.T) {
	ctx := context.Background()
	succeeded := corev1alpha1.TaskExecutionStatus{
		Attempt: 1, PromptID: "prompt-1", State: corev1alpha1.TaskExecutionStateSucceeded,
		Outcome: corev1alpha1.TaskExecutionOutcomeSucceeded,
	}
	delivery := corev1alpha1.TaskDeliveryStatus{
		State:   corev1alpha1.TaskDeliveryStateNotRequested,
		Outcome: corev1alpha1.TaskDeliveryOutcome(corev1alpha1.TaskDeliveryStateNotRequested),
	}
	project := func(phase corev1alpha1.TaskPhase) func(*ACPDispatcher, *corev1alpha1.Task) error {
		return func(d *ACPDispatcher, task *corev1alpha1.Task) error {
			payload, err := json.Marshal(taskTerminalProjection{
				Namespace: task.Namespace, Task: task.Name, TaskUID: string(task.UID), Attempt: 1,
				Phase: phase, Execution: succeeded, Delivery: &delivery,
			})
			if err != nil {
				return err
			}
			projector := &ACPOutboxProjector{Client: d.Client, Epochs: d.Epochs}
			_, err = projector.deliver(ctx, store.OutboxProjection{ProjectionKind: taskTerminalProjectionKind, Payload: payload})
			return err
		}
	}
	for name, write := range map[string]func(*ACPDispatcher, *corev1alpha1.Task) error{
		"outbox failed delivery":           project(corev1alpha1.TaskPhaseFailed),
		"outbox cancelled after execution": project(corev1alpha1.TaskPhaseCancelled),
		"direct failed delivery": func(d *ACPDispatcher, task *corev1alpha1.Task) error {
			return d.failTaskForDelivery(ctx, task, delivery, "ACP delivery failed")
		},
		"direct cancelled after execution": func(d *ACPDispatcher, task *corev1alpha1.Task) error {
			return d.cancelTaskAfterExecution(ctx, task, delivery, "publication cancelled before push")
		},
		"execution patch": func(d *ACPDispatcher, task *corev1alpha1.Task) error {
			return d.patchExecution(ctx, task, func(status *corev1alpha1.TaskExecutionStatus) {
				status.State, status.Outcome = corev1alpha1.TaskExecutionStateSucceeded, corev1alpha1.TaskExecutionOutcomeSucceeded
			})
		},
		"execution state alone": func(d *ACPDispatcher, task *corev1alpha1.Task) error {
			return d.patchExecution(ctx, task, func(status *corev1alpha1.TaskExecutionStatus) {
				status.State = corev1alpha1.TaskExecutionStateSucceeded
			})
		},
		"restored terminal": func(d *ACPDispatcher, task *corev1alpha1.Task) error {
			return d.patchRestoredTaskTerminal(ctx, task, 7, corev1alpha1.TaskExecutionStateSucceeded,
				corev1alpha1.TaskExecutionOutcomeSucceeded, "restored", &delivery)
		},
	} {
		t.Run(name, func(t *testing.T) {
			// A binding whose Task UID differs from the Task's is a restored
			// incarnation's, which the restored settlement requires.
			binding := testACPExecuteBindingForDispatcher()
			binding.Task.UID = types.UID("source-uid")
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "task", UID: types.UID("task-uid")},
				Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent,
					SessionRef: &corev1alpha1.SessionReference{Name: "session"}},
				Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning,
					Execution: &corev1alpha1.TaskExecutionStatus{
						Attempt: 1, PromptID: "prompt-1", State: corev1alpha1.TaskExecutionStateSettling,
					},
					AgentExecutionBinding: binding,
				},
			}
			kube := fake.NewClientBuilder().WithScheme(newTestScheme()).
				WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task).Build()
			manager := &ControllerEpochManager{ready: make(chan struct{})}
			manager.setHold(store.StorageRecoveryHold{ID: "hold-1", Reason: store.StorageRecoveryUnsafeSwitch})
			dispatcher := &ACPDispatcher{Client: kube, Epochs: manager}
			current := func() corev1alpha1.TaskStatus {
				t.Helper()
				var latest corev1alpha1.Task
				if err := kube.Get(ctx, client.ObjectKeyFromObject(task), &latest); err != nil {
					t.Fatal(err)
				}
				return latest.Status
			}

			if err := write(dispatcher, task.DeepCopy()); !errors.Is(err, store.ErrStorageRecoveryRequired) {
				t.Fatalf("execution success written while held = %v; want ErrStorageRecoveryRequired", err)
			}
			if status := current(); taskSuccessOf(&corev1alpha1.Task{Status: status}) != (taskSuccess{}) {
				t.Fatalf("Task status while held = %#v; want no success", status)
			}
			manager.clearHold()
			if err := write(dispatcher, task.DeepCopy()); err != nil {
				t.Fatalf("execution success written after the hold was released = %v", err)
			}
			if status := current(); status.Execution == nil || status.Execution.State != corev1alpha1.TaskExecutionStateSucceeded {
				t.Fatalf("Task execution after the hold was released = %#v; want Succeeded", status.Execution)
			}
		})
	}
}
