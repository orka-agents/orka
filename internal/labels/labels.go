/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package labels

const (
	// Finalizer
	TaskFinalizer = "orka.ai/cleanup"

	// Labels
	LabelTask           = "orka.ai/task"
	LabelTaskType       = "orka.ai/task-type"
	LabelParentTask     = "orka.ai/parent-task"
	LabelScheduledRun   = "orka.ai/scheduled-run"
	LabelCreatedBy      = "orka.ai/created-by"
	LabelAgentRole      = "orka.ai/agent-role"
	LabelCoordinator    = "orka.ai/coordinator"
	LabelDelegatedAgent = "orka.ai/delegated-agent"
	LabelIteration      = "orka.ai/iteration"
	LabelIterationGroup = "orka.ai/iteration-group"
	LabelPurpose        = "orka.ai/purpose"
	LabelManaged        = "orka.ai/managed"
	LabelChatSession    = "orka.ai/chat-session"

	// Annotations
	AnnotationCoordinationDepth = "orka.ai/coordination-depth"
	AnnotationAutoRetry         = "orka.ai/auto-retry"
	AnnotationMaxRetries        = "orka.ai/max-retries"
	AnnotationRetryCount        = "orka.ai/retry-count"
	AnnotationOriginalPrompt    = "orka.ai/original-prompt"
	AnnotationRetriedFrom       = "orka.ai/retried-from"
)
