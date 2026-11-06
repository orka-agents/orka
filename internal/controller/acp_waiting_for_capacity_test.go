/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/metrics"
)

func capacityTestTask() *corev1alpha1.Task {
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "queued", Namespace: "default", UID: "uid-queued"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
		Status: corev1alpha1.TaskStatus{
			Phase: corev1alpha1.TaskPhasePending,
			Execution: &corev1alpha1.TaskExecutionStatus{
				State: corev1alpha1.TaskExecutionStateQueued, Reason: corev1alpha1.TaskExecutionReasonWaitingForCapacity,
				Message: "RuntimePool is at capacity: RuntimePool has no unclaimed resident or prompt slot",
			},
		},
	}
}

func TestDesiredWaitingForCapacityConditionMapsCapacityReasons(t *testing.T) {
	pool := desiredWaitingForCapacityCondition(capacityTestTask())
	if pool == nil || pool.Status != metav1.ConditionTrue || pool.Reason != corev1alpha1.TaskWaitingForCapacityReasonRuntimeCapacityUnavailable {
		t.Fatalf("RuntimePool capacity must be reported, got %+v", pool)
	}

	generic := capacityTestTask()
	generic.Status.Execution.Reason = corev1alpha1.TaskExecutionReasonAtCapacity
	if got := desiredWaitingForCapacityCondition(generic); got != nil {
		t.Fatalf("a generic admission retry is not a capacity wait, got %+v", got)
	}

	workspace := capacityTestTask()
	workspace.Status.Execution = nil
	workspace.Status.ExecutionWorkspace = &corev1alpha1.ExecutionWorkspaceStatus{Conditions: []metav1.Condition{{
		Type: "Admitted", Status: metav1.ConditionFalse, Reason: string(workspacev1alpha1.ReasonCapacityUnavailable),
		Message: "bound workspace pool has no admission capacity",
	}}}
	got := desiredWaitingForCapacityCondition(workspace)
	if got == nil || got.Status != metav1.ConditionTrue || got.Reason != corev1alpha1.TaskWaitingForCapacityReasonWorkspaceCapacityUnavailable {
		t.Fatalf("workspace capacity must be reported, got %+v", got)
	}

	running := capacityTestTask()
	running.Status.Phase = corev1alpha1.TaskPhaseRunning
	if got := desiredWaitingForCapacityCondition(running); got != nil {
		t.Fatal("a Task that never waited records nothing")
	}
	meta.SetStatusCondition(&running.Status.Conditions, metav1.Condition{
		Type: corev1alpha1.TaskConditionWaitingForCapacity, Status: metav1.ConditionTrue, Reason: "RuntimeCapacityUnavailable",
	})
	if got := desiredWaitingForCapacityCondition(running); got == nil || got.Status != metav1.ConditionFalse ||
		got.Reason != corev1alpha1.TaskWaitingForCapacityReasonCapacityAvailable {
		t.Fatalf("admission must clear the condition, got %+v", got)
	}
}

func TestSyncACPWaitingForCapacityRecordsTransitionsOnce(t *testing.T) {
	task := capacityTestTask()
	c := fake.NewClientBuilder().WithScheme(newTestScheme()).WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task).Build()
	r := &TaskReconciler{Client: c, APIReader: c}
	before := metrics.CounterVecValue(metrics.WaitingForCapacityTotal, "task", corev1alpha1.TaskWaitingForCapacityReasonRuntimeCapacityUnavailable)
	for range 2 {
		current := &corev1alpha1.Task{}
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(task), current); err != nil {
			t.Fatal(err)
		}
		if err := r.syncACPWaitingForCapacity(context.Background(), current); err != nil {
			t.Fatal(err)
		}
	}
	if got := metrics.CounterVecValue(metrics.WaitingForCapacityTotal, "task", corev1alpha1.TaskWaitingForCapacityReasonRuntimeCapacityUnavailable); got != before+1 {
		t.Fatalf("metric = %v, want one transition", got-before)
	}
	current := &corev1alpha1.Task{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(task), current); err != nil {
		t.Fatal(err)
	}
	condition := meta.FindStatusCondition(current.Status.Conditions, corev1alpha1.TaskConditionWaitingForCapacity)
	if condition == nil || condition.Status != metav1.ConditionTrue {
		t.Fatalf("condition = %+v", condition)
	}

	current.Status.Phase = corev1alpha1.TaskPhaseRunning
	current.Status.Execution.State = corev1alpha1.TaskExecutionStateRunning
	current.Status.Execution.Reason = ""
	if err := c.Status().Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	if err := r.syncACPWaitingForCapacity(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	if condition := meta.FindStatusCondition(current.Status.Conditions, corev1alpha1.TaskConditionWaitingForCapacity); condition == nil ||
		condition.Status != metav1.ConditionFalse {
		t.Fatalf("admission must clear the condition, got %+v", condition)
	}
}

func TestACPAdmissionRetryReasonTreatsANonAdmittingPoolAsACapacityWait(t *testing.T) {
	for _, tc := range []struct {
		cause   error
		reason  corev1alpha1.TaskExecutionReason
		message string
	}{
		{fmt.Errorf("%w: full", errACPRuntimePoolAtCapacity), corev1alpha1.TaskExecutionReasonWaitingForCapacity, "RuntimePool is at capacity; runtime admission will be retried"},
		{fmt.Errorf("%w: draining", errACPRuntimePoolNotAdmitting), corev1alpha1.TaskExecutionReasonWaitingForCapacity, "RuntimePool is not admitting new sessions; runtime admission will be retried"},
		{errors.New("lease contention"), corev1alpha1.TaskExecutionReasonAtCapacity, "runtime admission will be retried"},
	} {
		if got := acpAdmissionRetryReason(tc.cause); got != tc.reason {
			t.Errorf("acpAdmissionRetryReason(%v) = %q, want %q", tc.cause, got, tc.reason)
		}
		if got := acpAdmissionRetryMessage(tc.cause); got != tc.message {
			t.Errorf("acpAdmissionRetryMessage(%v) = %q, want %q", tc.cause, got, tc.message)
		}
	}
}

// promptRateLimitedTestTask is the status the built-in RuntimePool prompt-429
// path in executeReservedTask writes: still Submitting and Pending, since the
// runtime proved it did not accept the prompt.
func promptRateLimitedTestTask() *corev1alpha1.Task {
	task := capacityTestTask()
	task.Status.Execution.State = corev1alpha1.TaskExecutionStateSubmitting
	task.Status.Execution.Message = "RuntimePool prompt admission was rate limited and will be reconciled"
	return task
}

func TestPromptRateLimitedSubmittingTaskIsWaitingForCapacity(t *testing.T) {
	if got := desiredWaitingForCapacityCondition(promptRateLimitedTestTask()); got == nil || got.Status != metav1.ConditionTrue ||
		got.Reason != corev1alpha1.TaskWaitingForCapacityReasonRuntimeCapacityUnavailable {
		t.Fatalf("a prompt-429 Submitting Task must wait for capacity, got %+v", got)
	}
	submitting := promptRateLimitedTestTask()
	submitting.Status.Execution.Reason = ""
	if got := desiredWaitingForCapacityCondition(submitting); got != nil {
		t.Fatalf("a Submitting Task without a capacity reason must not wait for capacity, got %+v", got)
	}

	task := promptRateLimitedTestTask()
	task.Status.Conditions = []metav1.Condition{{
		Type: corev1alpha1.TaskConditionWaitingForCapacity, Status: metav1.ConditionTrue,
		Reason: corev1alpha1.TaskWaitingForCapacityReasonRuntimeCapacityUnavailable, Message: "RuntimePool is at capacity",
		LastTransitionTime: metav1.Now(),
	}}
	c := fake.NewClientBuilder().WithScheme(newTestScheme()).WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task).Build()
	r := &TaskReconciler{Client: c, APIReader: c}
	current := &corev1alpha1.Task{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(task), current); err != nil {
		t.Fatal(err)
	}
	if err := r.syncACPWaitingForCapacity(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	if condition := meta.FindStatusCondition(current.Status.Conditions, corev1alpha1.TaskConditionWaitingForCapacity); condition == nil ||
		condition.Status != metav1.ConditionTrue {
		t.Fatalf("a prompt 429 must not clear the capacity wait, got %+v", condition)
	}
}
