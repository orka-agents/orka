package events

import (
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// The execution event vocabulary is part of the public storage contract in
// pkg/storage/v1, because providers validate event filters and terminal
// approval uniqueness against it. These aliases keep the established names.
type (
	ExecutionEventTruncation = storagev1.ExecutionEventTruncation
)

const (
	ExecutionEventTypeTaskCreated                   = storagev1.ExecutionEventTypeTaskCreated
	ExecutionEventTypeTaskPhaseChanged              = storagev1.ExecutionEventTypeTaskPhaseChanged
	ExecutionEventTypeTaskJobCreated                = storagev1.ExecutionEventTypeTaskJobCreated
	ExecutionEventTypeTaskStarted                   = storagev1.ExecutionEventTypeTaskStarted
	ExecutionEventTypeTaskSucceeded                 = storagev1.ExecutionEventTypeTaskSucceeded
	ExecutionEventTypeTaskFailed                    = storagev1.ExecutionEventTypeTaskFailed
	ExecutionEventTypeTaskCancelled                 = storagev1.ExecutionEventTypeTaskCancelled
	ExecutionEventTypeWorkerStarted                 = storagev1.ExecutionEventTypeWorkerStarted
	ExecutionEventTypeWorkerCompleted               = storagev1.ExecutionEventTypeWorkerCompleted
	ExecutionEventTypeWorkerFailed                  = storagev1.ExecutionEventTypeWorkerFailed
	ExecutionEventTypeModelRequestStarted           = storagev1.ExecutionEventTypeModelRequestStarted
	ExecutionEventTypeModelRequestCompleted         = storagev1.ExecutionEventTypeModelRequestCompleted
	ExecutionEventTypeModelRequestFailed            = storagev1.ExecutionEventTypeModelRequestFailed
	ExecutionEventTypeModelUsageUpdated             = storagev1.ExecutionEventTypeModelUsageUpdated
	ExecutionEventTypeModelContextUpdated           = storagev1.ExecutionEventTypeModelContextUpdated
	ExecutionEventTypeModelMessage                  = storagev1.ExecutionEventTypeModelMessage
	ExecutionEventTypeContextTruncated              = storagev1.ExecutionEventTypeContextTruncated
	ExecutionEventTypeNativeSessionCaptureSkipped   = storagev1.ExecutionEventTypeNativeSessionCaptureSkipped
	ExecutionEventTypeToolCallStarted               = storagev1.ExecutionEventTypeToolCallStarted
	ExecutionEventTypeToolCallCompleted             = storagev1.ExecutionEventTypeToolCallCompleted
	ExecutionEventTypeToolCallFailed                = storagev1.ExecutionEventTypeToolCallFailed
	ExecutionEventTypeWorkspacePreparationStarted   = storagev1.ExecutionEventTypeWorkspacePreparationStarted
	ExecutionEventTypeWorkspacePreparationCompleted = storagev1.ExecutionEventTypeWorkspacePreparationCompleted
	ExecutionEventTypeWorkspacePreparationFailed    = storagev1.ExecutionEventTypeWorkspacePreparationFailed
	ExecutionEventTypeAgentRuntimeStarted           = storagev1.ExecutionEventTypeAgentRuntimeStarted
	ExecutionEventTypeAgentRuntimeCommandStarted    = storagev1.ExecutionEventTypeAgentRuntimeCommandStarted
	ExecutionEventTypeAgentRuntimeCompleted         = storagev1.ExecutionEventTypeAgentRuntimeCompleted
	ExecutionEventTypeAgentRuntimeFailed            = storagev1.ExecutionEventTypeAgentRuntimeFailed
	ExecutionEventTypeAgentRuntimeCancelled         = storagev1.ExecutionEventTypeAgentRuntimeCancelled
	ExecutionEventTypeResultSubmitted               = storagev1.ExecutionEventTypeResultSubmitted
	ExecutionEventTypeGatewayDeliveryCompleted      = storagev1.ExecutionEventTypeGatewayDeliveryCompleted
	ExecutionEventTypeArtifactUploadCompleted       = storagev1.ExecutionEventTypeArtifactUploadCompleted
	ExecutionEventTypeArtifactUploadFailed          = storagev1.ExecutionEventTypeArtifactUploadFailed
	ExecutionEventTypeTaskForkRequested             = storagev1.ExecutionEventTypeTaskForkRequested
	ExecutionEventTypeTaskForkCreated               = storagev1.ExecutionEventTypeTaskForkCreated
	ExecutionEventTypeApprovalRequested             = storagev1.ExecutionEventTypeApprovalRequested
	ExecutionEventTypeApprovalApproved              = storagev1.ExecutionEventTypeApprovalApproved
	ExecutionEventTypeApprovalDeclined              = storagev1.ExecutionEventTypeApprovalDeclined
	ExecutionEventTypeApprovalExpired               = storagev1.ExecutionEventTypeApprovalExpired
	ExecutionEventTypeApprovalCancelled             = storagev1.ExecutionEventTypeApprovalCancelled
	ExecutionEventTypeApprovalExecutionUpdated      = storagev1.ExecutionEventTypeApprovalExecutionUpdated
	ExecutionEventTypePlanUpdated                   = storagev1.ExecutionEventTypePlanUpdated
	ExecutionEventSeverityDebug                     = storagev1.ExecutionEventSeverityDebug
	ExecutionEventSeverityInfo                      = storagev1.ExecutionEventSeverityInfo
	ExecutionEventSeverityWarning                   = storagev1.ExecutionEventSeverityWarning
	ExecutionEventSeverityError                     = storagev1.ExecutionEventSeverityError
	ExecutionEventStreamTypeTask                    = storagev1.ExecutionEventStreamTypeTask
	ExecutionEventStreamTypeSession                 = storagev1.ExecutionEventStreamTypeSession
)

func TerminalTaskEventTypes() []string { return storagev1.TerminalTaskEventTypes() }

func IsTerminalTaskEventType(value string) bool { return storagev1.IsTerminalTaskEventType(value) }

func IsTerminalApprovalEventType(value string) bool {
	return storagev1.IsTerminalApprovalEventType(value)
}

func ExecutionEventTypes() []string { return storagev1.ExecutionEventTypes() }

func CanonicalExecutionEventType(value string) string {
	return storagev1.CanonicalExecutionEventType(value)
}

func IsValidExecutionEventType(value string) bool { return storagev1.IsValidExecutionEventType(value) }

func NormalizeExecutionEventType(value string) string {
	return storagev1.NormalizeExecutionEventType(value)
}

func IsValidExecutionEventSeverity(value string) bool {
	return storagev1.IsValidExecutionEventSeverity(value)
}

func NormalizeExecutionEventSeverity(value string) string {
	return storagev1.NormalizeExecutionEventSeverity(value)
}

func IsValidExecutionEventStreamType(value string) bool {
	return storagev1.IsValidExecutionEventStreamType(value)
}
