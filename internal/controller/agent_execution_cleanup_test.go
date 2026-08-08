package controller

import (
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestTaskDispatchabilityFollowsImmutableContract(t *testing.T) {
	task := &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent}}
	if taskDispatchableByHarnessV1(task) || taskDispatchableByACP(task) {
		t.Fatal("unbound agent Task must not be dispatchable")
	}

	task.Status.AgentExecutionBinding = &corev1alpha1.AgentExecutionBinding{
		ContractVersion: corev1alpha1.AgentRuntimeContractHarnessV1,
	}
	if !taskDispatchableByHarnessV1(task) || taskDispatchableByACP(task) {
		t.Fatal("harness-v1 binding selected the wrong dispatcher")
	}

	task.Status.AgentExecutionBinding.ContractVersion = corev1alpha1.AgentRuntimeContractHarnessV2
	if taskDispatchableByHarnessV1(task) || !taskDispatchableByACP(task) {
		t.Fatal("harness-v2 binding selected the wrong dispatcher")
	}

	task.Spec.Type = corev1alpha1.TaskTypeContainer
	if taskDispatchableByHarnessV1(task) || taskDispatchableByACP(task) {
		t.Fatal("non-agent Task must not be dispatched by an agent harness")
	}
}
