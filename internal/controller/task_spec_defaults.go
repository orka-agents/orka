package controller

import (
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

const (
	taskSpecDefaultPriority                = int32(500)
	taskSpecDefaultStartingDeadlineSeconds = int64(100)
	taskSpecDefaultSuccessfulRunsHistory   = int32(3)
	taskSpecDefaultFailedRunsHistory       = int32(1)
	taskSpecDefaultRetryBackoffMultiplier  = float64(2)
	taskSpecDefaultSessionMaxMessages      = int32(50)
	taskSpecDefaultCredentialKey           = "token"
	taskSpecDefaultWorkspaceReusePolicy    = corev1alpha1.WorkspaceReusePolicy("none")
	taskSpecDefaultWorkspaceSlot           = "default"
)

func taskSpecWithServerDefaults(spec corev1alpha1.TaskSpec) corev1alpha1.TaskSpec {
	out := *spec.DeepCopy()
	if out.Priority == nil {
		out.Priority = new(taskSpecDefaultPriority)
	}
	if out.ConcurrencyPolicy == "" {
		out.ConcurrencyPolicy = corev1alpha1.ForbidConcurrent
	}
	if out.StartingDeadlineSeconds == nil {
		out.StartingDeadlineSeconds = new(taskSpecDefaultStartingDeadlineSeconds)
	}
	if out.SuccessfulRunsHistoryLimit == nil {
		out.SuccessfulRunsHistoryLimit = new(taskSpecDefaultSuccessfulRunsHistory)
	}
	if out.FailedRunsHistoryLimit == nil {
		out.FailedRunsHistoryLimit = new(taskSpecDefaultFailedRunsHistory)
	}
	if out.RetryPolicy != nil && out.RetryPolicy.BackoffMultiplier == 0 {
		out.RetryPolicy.BackoffMultiplier = taskSpecDefaultRetryBackoffMultiplier
	}
	if out.SessionRef != nil && out.SessionRef.MaxMessages == 0 {
		out.SessionRef.MaxMessages = taskSpecDefaultSessionMaxMessages
	}
	if out.AI != nil && out.AI.MemoryConsolidation != nil && out.AI.MemoryConsolidation.Mode == "" {
		out.AI.MemoryConsolidation.Mode = corev1alpha1.MemoryConsolidationDryRun
	}
	if out.Workspace != nil {
		for _, ref := range []*corev1alpha1.WorkspaceCredentialReference{
			out.Workspace.ReadCredentialRef,
			out.Workspace.PublicationReadCredentialRef,
			out.Workspace.PublicationCredentialRef,
			out.Workspace.ForgeCredentialRef,
		} {
			if ref != nil && ref.Key == "" {
				ref.Key = taskSpecDefaultCredentialKey
			}
		}
	}
	if out.Execution != nil && out.Execution.Workspace != nil {
		if out.Execution.Workspace.ReusePolicy == "" {
			out.Execution.Workspace.ReusePolicy = taskSpecDefaultWorkspaceReusePolicy
		}
		if out.Execution.Workspace.WorkspaceSlot == "" {
			out.Execution.Workspace.WorkspaceSlot = taskSpecDefaultWorkspaceSlot
		}
	}
	return out
}
