package controller

import corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"

func executionBinding(
	task *corev1alpha1.Task,
	contract corev1alpha1.AgentRuntimeContractVersion,
) *corev1alpha1.AgentExecutionBinding {
	if task == nil || task.Spec.Type != corev1alpha1.TaskTypeAgent {
		return nil
	}
	binding := task.Status.AgentExecutionBinding
	if binding == nil || binding.ContractVersion != contract {
		return nil
	}
	return binding
}

func taskDispatchableByHarnessV1(task *corev1alpha1.Task) bool {
	return executionBinding(task, corev1alpha1.AgentRuntimeContractHarnessV1) != nil
}

func taskDispatchableByACP(task *corev1alpha1.Task) bool {
	return executionBinding(task, corev1alpha1.AgentRuntimeContractHarnessV2) != nil
}
