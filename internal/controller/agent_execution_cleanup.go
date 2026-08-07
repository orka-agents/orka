package controller

import corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"

// legacyCleanupBinding returns the migration-only binding for the requested
// executor. Cleanup authority is intentionally narrower than dispatch
// authority: it may terminalize and reclaim historical state, but it must
// never create new demand or cross a runtime request-write boundary.
func legacyCleanupBinding(
	task *corev1alpha1.Task,
	contract corev1alpha1.AgentRuntimeContractVersion,
) *corev1alpha1.AgentExecutionBinding {
	if task == nil || task.Spec.Type != corev1alpha1.TaskTypeAgent {
		return nil
	}
	binding := task.Status.AgentExecutionBinding
	if binding == nil || binding.ContractVersion != contract ||
		binding.Mode != corev1alpha1.AgentExecutionBindingModeCleanupOnly ||
		binding.Provenance != corev1alpha1.AgentExecutionProvenanceLegacyCleanupOnly {
		return nil
	}
	return binding
}

func executionBinding(
	task *corev1alpha1.Task,
	contract corev1alpha1.AgentRuntimeContractVersion,
) *corev1alpha1.AgentExecutionBinding {
	if task == nil || task.Spec.Type != corev1alpha1.TaskTypeAgent {
		return nil
	}
	binding := task.Status.AgentExecutionBinding
	if binding == nil || binding.ContractVersion != contract ||
		binding.Mode != corev1alpha1.AgentExecutionBindingModeExecute {
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
