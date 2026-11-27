package v1

import (
	"strings"
)

const (
	ExecutionEventTypeTaskCreated                   = "TaskCreated"
	ExecutionEventTypeTaskPhaseChanged              = "TaskPhaseChanged"
	ExecutionEventTypeTaskJobCreated                = "TaskJobCreated"
	ExecutionEventTypeTaskStarted                   = "TaskStarted"
	ExecutionEventTypeTaskSucceeded                 = "TaskSucceeded"
	ExecutionEventTypeTaskFailed                    = "TaskFailed"
	ExecutionEventTypeTaskCancellationRequested     = "TaskCancellationRequested"
	ExecutionEventTypeTaskCancelled                 = "TaskCancelled"
	ExecutionEventTypeWorkerStarted                 = "WorkerStarted"
	ExecutionEventTypeWorkerCompleted               = "WorkerCompleted"
	ExecutionEventTypeWorkerFailed                  = "WorkerFailed"
	ExecutionEventTypeModelRequestStarted           = "ModelRequestStarted"
	ExecutionEventTypeModelRequestCompleted         = "ModelRequestCompleted"
	ExecutionEventTypeModelRequestFailed            = "ModelRequestFailed"
	ExecutionEventTypeModelUsageUpdated             = "ModelUsageUpdated"
	ExecutionEventTypeModelContextUpdated           = "ModelContextUpdated"
	ExecutionEventTypeModelMessage                  = "ModelMessage"
	ExecutionEventTypeContextTruncated              = "ContextTruncated"
	ExecutionEventTypeNativeSessionCaptureSkipped   = "NativeSessionCaptureSkipped"
	ExecutionEventTypeToolCallStarted               = "ToolCallStarted"
	ExecutionEventTypeToolCallCompleted             = "ToolCallCompleted"
	ExecutionEventTypeToolCallFailed                = "ToolCallFailed"
	ExecutionEventTypeWorkspacePreparationStarted   = "WorkspacePreparationStarted"
	ExecutionEventTypeWorkspacePreparationCompleted = "WorkspacePreparationCompleted"
	ExecutionEventTypeWorkspacePreparationFailed    = "WorkspacePreparationFailed"
	ExecutionEventTypeAgentRuntimeStarted           = "AgentRuntimeStarted"
	ExecutionEventTypeAgentRuntimeCommandStarted    = "AgentRuntimeCommandStarted"
	ExecutionEventTypeAgentRuntimeCompleted         = "AgentRuntimeCompleted"
	ExecutionEventTypeAgentRuntimeFailed            = "AgentRuntimeFailed"
	ExecutionEventTypeAgentRuntimeCancelled         = "AgentRuntimeCancelled"
	ExecutionEventTypeResultSubmitted               = "ResultSubmitted"
	ExecutionEventTypeGatewayDeliveryCompleted      = "GatewayDeliveryCompleted"
	ExecutionEventTypeArtifactUploadCompleted       = "ArtifactUploadCompleted"
	ExecutionEventTypeArtifactUploadFailed          = "ArtifactUploadFailed"
	ExecutionEventTypeTaskForkRequested             = "TaskForkRequested"
	ExecutionEventTypeTaskForkCreated               = "TaskForkCreated"
	ExecutionEventTypeApprovalRequested             = "ApprovalRequested"
	ExecutionEventTypeApprovalApproved              = "ApprovalApproved"
	ExecutionEventTypeApprovalDeclined              = "ApprovalDeclined"
	ExecutionEventTypeApprovalExpired               = "ApprovalExpired"
	ExecutionEventTypeApprovalCancelled             = "ApprovalCancelled"
	ExecutionEventTypeApprovalExecutionUpdated      = "ApprovalExecutionUpdated"
	ExecutionEventTypePlanUpdated                   = "PlanUpdated"
)

const (
	ExecutionEventSeverityDebug   = "debug"
	ExecutionEventSeverityInfo    = "info"
	ExecutionEventSeverityWarning = "warning"
	ExecutionEventSeverityError   = "error"
)

const (
	ExecutionEventStreamTypeTask    = "task"
	ExecutionEventStreamTypeSession = "session"
)

var executionEventTypes = []string{
	ExecutionEventTypeTaskCreated,
	ExecutionEventTypeTaskPhaseChanged,
	ExecutionEventTypeTaskJobCreated,
	ExecutionEventTypeTaskStarted,
	ExecutionEventTypeTaskSucceeded,
	ExecutionEventTypeTaskFailed,
	ExecutionEventTypeTaskCancellationRequested,
	ExecutionEventTypeTaskCancelled,
	ExecutionEventTypeWorkerStarted,
	ExecutionEventTypeWorkerCompleted,
	ExecutionEventTypeWorkerFailed,
	ExecutionEventTypeModelRequestStarted,
	ExecutionEventTypeModelRequestCompleted,
	ExecutionEventTypeModelRequestFailed,
	ExecutionEventTypeModelUsageUpdated,
	ExecutionEventTypeModelContextUpdated,
	ExecutionEventTypeModelMessage,
	ExecutionEventTypeContextTruncated,
	ExecutionEventTypeNativeSessionCaptureSkipped,
	ExecutionEventTypeToolCallStarted,
	ExecutionEventTypeToolCallCompleted,
	ExecutionEventTypeToolCallFailed,
	ExecutionEventTypeWorkspacePreparationStarted,
	ExecutionEventTypeWorkspacePreparationCompleted,
	ExecutionEventTypeWorkspacePreparationFailed,
	ExecutionEventTypeAgentRuntimeStarted,
	ExecutionEventTypeAgentRuntimeCommandStarted,
	ExecutionEventTypeAgentRuntimeCompleted,
	ExecutionEventTypeAgentRuntimeFailed,
	ExecutionEventTypeAgentRuntimeCancelled,
	ExecutionEventTypeResultSubmitted,
	ExecutionEventTypeGatewayDeliveryCompleted,
	ExecutionEventTypeArtifactUploadCompleted,
	ExecutionEventTypeArtifactUploadFailed,
	ExecutionEventTypeTaskForkRequested,
	ExecutionEventTypeTaskForkCreated,
	ExecutionEventTypeApprovalRequested,
	ExecutionEventTypeApprovalApproved,
	ExecutionEventTypeApprovalDeclined,
	ExecutionEventTypeApprovalExpired,
	ExecutionEventTypeApprovalCancelled,
	ExecutionEventTypeApprovalExecutionUpdated,
	ExecutionEventTypePlanUpdated,
}

var validExecutionEventTypes = stringSet(executionEventTypes)

var terminalTaskEventTypes = []string{
	ExecutionEventTypeTaskSucceeded,
	ExecutionEventTypeTaskFailed,
	ExecutionEventTypeTaskCancelled,
}

var terminalTaskEventTypeSet = stringSet(terminalTaskEventTypes)

var terminalApprovalEventTypes = []string{
	ExecutionEventTypeApprovalApproved,
	ExecutionEventTypeApprovalDeclined,
	ExecutionEventTypeApprovalExpired,
	ExecutionEventTypeApprovalCancelled,
}

var terminalApprovalEventTypeSet = stringSet(terminalApprovalEventTypes)

func stringSet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	return set
}

func TerminalTaskEventTypes() []string {
	return append([]string(nil), terminalTaskEventTypes...)
}

func IsTerminalTaskEventType(value string) bool {
	_, ok := terminalTaskEventTypeSet[strings.TrimSpace(value)]
	return ok
}

func IsTerminalApprovalEventType(value string) bool {
	_, ok := terminalApprovalEventTypeSet[strings.TrimSpace(value)]
	return ok
}

func ExecutionEventTypes() []string {
	return append([]string(nil), executionEventTypes...)
}

func CanonicalExecutionEventType(value string) string {
	value = strings.TrimSpace(value)
	if IsValidExecutionEventType(value) {
		return value
	}
	for _, known := range executionEventTypes {
		if strings.EqualFold(known, value) {
			return known
		}
	}
	return ""
}

func IsValidExecutionEventType(value string) bool {
	_, ok := validExecutionEventTypes[strings.TrimSpace(value)]
	return ok
}

func NormalizeExecutionEventType(value string) string {
	value = strings.TrimSpace(value)
	if !IsValidExecutionEventType(value) {
		return ""
	}
	return value
}

func IsValidExecutionEventSeverity(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case ExecutionEventSeverityDebug, ExecutionEventSeverityInfo, ExecutionEventSeverityWarning, ExecutionEventSeverityError:
		return true
	default:
		return false
	}
}

func NormalizeExecutionEventSeverity(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if !IsValidExecutionEventSeverity(value) {
		return ExecutionEventSeverityInfo
	}
	return value
}

func IsValidExecutionEventStreamType(value string) bool {
	return strings.TrimSpace(value) == ExecutionEventStreamTypeTask
}
