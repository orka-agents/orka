package controller

import corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"

func executionBinding(task *corev1alpha1.Task) *corev1alpha1.AgentExecutionBinding {
	if task == nil || task.Spec.Type != corev1alpha1.TaskTypeAgent {
		return nil
	}
	binding := task.Status.AgentExecutionBinding
	if binding == nil || binding.ContractVersion != corev1alpha1.AgentRuntimeContractHarnessV2 {
		return nil
	}
	return binding
}

func taskDispatchableByACP(task *corev1alpha1.Task) bool {
	return executionBinding(task) != nil
}
