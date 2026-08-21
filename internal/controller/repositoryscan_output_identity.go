package controller

import (
	"fmt"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// securityTaskOutputAttempt returns the attempt used to bind security task
// outputs. ACP-executed tasks report a durable one-based attempt in
// status.execution; tasks that have not started an attempt yet fall back to the
// projected attempt derived from status.attempts and phase.
func securityTaskOutputAttempt(task *corev1alpha1.Task) int64 {
	if task == nil {
		return 0
	}
	if task.Status.Execution != nil && task.Status.Execution.Attempt > 0 {
		return int64(task.Status.Execution.Attempt)
	}
	attempt := int64(task.Status.Attempts)
	if task.Status.Phase == corev1alpha1.TaskPhasePending || task.Status.Phase == corev1alpha1.TaskPhaseScheduled {
		attempt++
	}
	return attempt
}

// securityTaskRuntimeSessionID returns the controller-owned runtime session
// identity for the current attempt, when one exists.
func securityTaskRuntimeSessionID(task *corev1alpha1.Task) string {
	if task == nil || task.Status.Execution == nil {
		return ""
	}
	return task.Status.Execution.RuntimeSessionUID
}

// securityTaskTurnID returns the durable prompt identity for the current
// attempt, when one exists.
func securityTaskTurnID(task *corev1alpha1.Task) string {
	if task == nil || task.Status.Execution == nil {
		return ""
	}
	return task.Status.Execution.PromptID
}

// securityTaskExecutionCorrelationID derives a deterministic correlation value
// from the controller-owned ACP attempt identity so stage receipts keep a
// harness execution identity now that the legacy wrapper correlation
// annotation no longer exists.
func securityTaskExecutionCorrelationID(task *corev1alpha1.Task) string {
	if task == nil || task.Status.Execution == nil {
		return ""
	}
	execution := task.Status.Execution
	if execution.RuntimeSessionUID == "" || execution.PromptID == "" || execution.Attempt <= 0 {
		return ""
	}
	return fmt.Sprintf("acp:%s:%d:%s", execution.RuntimeSessionUID, execution.Attempt, execution.PromptID)
}
