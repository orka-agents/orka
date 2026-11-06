/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestTurnRequestedParkMatchesOnlyTheSettlingTurn(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "t", Namespace: "default", UID: "uid-t"},
		Status: corev1alpha1.TaskStatus{
			Execution: &corev1alpha1.TaskExecutionStatus{Attempt: 2, PromptID: "prompt-uid-t-2"},
			Wait: &corev1alpha1.TaskWaitStatus{
				State: corev1alpha1.TaskWaitStateRequested, Kind: corev1alpha1.TaskWaitKindTimer, Attempt: 2, PromptID: "prompt-uid-t-2",
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(newTestScheme()).WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task).Build()
	d := &ACPDispatcher{Client: c, APIReader: c}
	if !d.turnRequestedPark(context.Background(), task) {
		t.Fatal("the settling turn's own wait must release its runtime")
	}
	other := task.DeepCopy()
	other.Status.Execution.Attempt, other.Status.Execution.PromptID = 3, "prompt-uid-t-3"
	if d.turnRequestedPark(context.Background(), other) {
		t.Fatal("a wait from another attempt must not release this turn's runtime")
	}
	replaced := task.DeepCopy()
	replaced.UID = "uid-other"
	if d.turnRequestedPark(context.Background(), replaced) {
		t.Fatal("a replaced Task must not match")
	}
}
