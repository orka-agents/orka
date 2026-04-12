/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package labels

const (
	// Finalizer
	TaskFinalizer = "orka.ai/cleanup"

	// Labels
	LabelTask              = "orka.ai/task"
	LabelTaskType          = "orka.ai/task-type"
	LabelParentTask        = "orka.ai/parent-task"
	LabelScheduledRun      = "orka.ai/scheduled-run"
	LabelCreatedBy         = "orka.ai/created-by"
	LabelAgentRole         = "orka.ai/agent-role"
	LabelCoordinator       = "orka.ai/coordinator"
	LabelDelegatedAgent    = "orka.ai/delegated-agent"
	LabelIteration         = "orka.ai/iteration"
	LabelIterationGroup    = "orka.ai/iteration-group"
	LabelPurpose           = "orka.ai/purpose"
	LabelManaged           = "orka.ai/managed"
	LabelChatSession       = "orka.ai/chat-session"
	LabelSecurityTarget    = "orka.ai/security-target"
	LabelSecurityScanID    = "orka.ai/security-scan-id"
	LabelSecurityMode      = "orka.ai/security-scan-mode"
	LabelSecurityStage     = "orka.ai/security-stage"
	LabelSecurityScope     = "orka.ai/security-scope"
	LabelSecurityFindingID = "orka.ai/security-finding-id"

	// Annotations
	AnnotationCoordinationDepth = "orka.ai/coordination-depth"
	AnnotationAutoRetry         = "orka.ai/auto-retry"
	AnnotationMaxRetries        = "orka.ai/max-retries"
	AnnotationRetryCount        = "orka.ai/retry-count"
	AnnotationOriginalPrompt    = "orka.ai/original-prompt"
)
