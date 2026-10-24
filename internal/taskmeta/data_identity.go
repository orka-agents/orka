/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package taskmeta

import (
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// RestoreIdentityChangedReason is the Task execution reason the controller
// records when it settles a restored Task on its source execution.
const RestoreIdentityChangedReason = "RestoreIdentityChanged"

// DataUID returns the UID that keys a Task's durable records, such as its
// result, prompt result receipts and artifacts. Kubernetes UIDs are
// incarnation fences: a clean-cluster restore gives a Task a new UID, while
// the records of its source execution stay keyed by the UID frozen in its
// agent execution binding. Once the controller has settled a restored Task
// on that source execution (see UsesRestoredSourceIdentity), DataUID returns
// the source UID; otherwise it returns the Task's own UID.
func DataUID(task *corev1alpha1.Task) types.UID {
	if task == nil {
		return ""
	}
	if UsesRestoredSourceIdentity(task) {
		return task.Status.AgentExecutionBinding.Task.UID
	}
	return task.UID
}

// UsesRestoredSourceIdentity reports whether task is a restored incarnation
// that the controller settled on its source execution: a terminal agent Task
// whose harness-v2 execution binding names another Task UID, with the restore
// reason and a matching terminal outcome.
func UsesRestoredSourceIdentity(task *corev1alpha1.Task) bool {
	if task == nil || task.Status.Execution == nil {
		return false
	}
	switch task.Status.Phase {
	case corev1alpha1.TaskPhaseSucceeded, corev1alpha1.TaskPhaseFailed, corev1alpha1.TaskPhaseCancelled:
	default:
		return false
	}
	binding := agentExecutionBinding(task)
	if binding == nil || binding.Task.UID == "" || binding.Task.UID == task.UID ||
		task.Status.Execution.Reason != corev1alpha1.TaskExecutionReason(RestoreIdentityChangedReason) {
		return false
	}
	switch task.Status.Execution.State {
	case corev1alpha1.TaskExecutionStateSucceeded:
		return task.Status.Execution.Outcome == corev1alpha1.TaskExecutionOutcomeSucceeded
	case corev1alpha1.TaskExecutionStateFailed:
		return task.Status.Execution.Outcome == corev1alpha1.TaskExecutionOutcomeFailed
	case corev1alpha1.TaskExecutionStateCancelled:
		return task.Status.Execution.Outcome == corev1alpha1.TaskExecutionOutcomeCancelled
	case corev1alpha1.TaskExecutionStateOutcomeUnknown:
		return task.Status.Execution.Outcome == corev1alpha1.TaskExecutionOutcomeOutcomeUnknown
	default:
		return false
	}
}

func agentExecutionBinding(task *corev1alpha1.Task) *corev1alpha1.AgentExecutionBinding {
	if task.Spec.Type != corev1alpha1.TaskTypeAgent {
		return nil
	}
	binding := task.Status.AgentExecutionBinding
	if binding == nil || binding.ContractVersion != corev1alpha1.AgentRuntimeContractHarnessV2 {
		return nil
	}
	return binding
}
