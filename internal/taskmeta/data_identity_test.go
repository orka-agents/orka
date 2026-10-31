/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package taskmeta

import (
	"testing"

	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func restoredTask() *corev1alpha1.Task {
	return &corev1alpha1.Task{
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
		Status: corev1alpha1.TaskStatus{
			Phase: corev1alpha1.TaskPhaseSucceeded,
			AgentExecutionBinding: &corev1alpha1.AgentExecutionBinding{
				ContractVersion: corev1alpha1.AgentRuntimeContractHarnessV2,
				Task:            corev1alpha1.AgentExecutionBindingTaskRef{UID: "source-uid"},
			},
			Execution: &corev1alpha1.TaskExecutionStatus{
				State:   corev1alpha1.TaskExecutionStateSucceeded,
				Outcome: corev1alpha1.TaskExecutionOutcomeSucceeded,
				Reason:  RestoreIdentityChangedReason,
			},
		},
	}
}

func TestDataUID(t *testing.T) {
	for name, test := range map[string]struct {
		mutate func(*corev1alpha1.Task)
		want   types.UID
	}{
		"settled restore keeps the source UID": {func(*corev1alpha1.Task) {}, "source-uid"},
		"own binding":                          {func(task *corev1alpha1.Task) { task.Status.AgentExecutionBinding.Task.UID = "restored-uid" }, "restored-uid"},
		"restore not settled yet": {func(task *corev1alpha1.Task) {
			task.Status.Execution.Reason = ""
		}, "restored-uid"},
		"not terminal": {func(task *corev1alpha1.Task) { task.Status.Phase = corev1alpha1.TaskPhaseRunning }, "restored-uid"},
		"outcome does not match the state": {func(task *corev1alpha1.Task) {
			task.Status.Execution.Outcome = corev1alpha1.TaskExecutionOutcomeFailed
		}, "restored-uid"},
		"not an agent Task": {func(task *corev1alpha1.Task) { task.Spec.Type = corev1alpha1.TaskTypeAI }, "restored-uid"},
		"no binding":        {func(task *corev1alpha1.Task) { task.Status.AgentExecutionBinding = nil }, "restored-uid"},
		"no execution":      {func(task *corev1alpha1.Task) { task.Status.Execution = nil }, "restored-uid"},
	} {
		t.Run(name, func(t *testing.T) {
			task := restoredTask()
			task.UID = "restored-uid"
			test.mutate(task)
			if got := DataUID(task); got != test.want {
				t.Fatalf("DataUID() = %q, want %q", got, test.want)
			}
		})
	}
	if got := DataUID(nil); got != "" {
		t.Fatalf("DataUID(nil) = %q, want empty", got)
	}
}
