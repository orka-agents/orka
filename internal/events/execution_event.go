package events

import "strings"

const (
	ExecutionEventTypeTaskCreated                   = "TaskCreated"
	ExecutionEventTypeTaskPhaseChanged              = "TaskPhaseChanged"
	ExecutionEventTypeTaskJobCreated                = "TaskJobCreated"
	ExecutionEventTypeTaskStarted                   = "TaskStarted"
	ExecutionEventTypeTaskSucceeded                 = "TaskSucceeded"
	ExecutionEventTypeTaskFailed                    = "TaskFailed"
	ExecutionEventTypeTaskCancelled                 = "TaskCancelled"
	ExecutionEventTypeWorkerStarted                 = "WorkerStarted"
	ExecutionEventTypeWorkerCompleted               = "WorkerCompleted"
	ExecutionEventTypeWorkerFailed                  = "WorkerFailed"
	ExecutionEventTypeModelRequestStarted           = "ModelRequestStarted"
	ExecutionEventTypeModelRequestCompleted         = "ModelRequestCompleted"
	ExecutionEventTypeModelRequestFailed            = "ModelRequestFailed"
	ExecutionEventTypeModelMessage                  = "ModelMessage"
	ExecutionEventTypeContextTruncated              = "ContextTruncated"
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
	ExecutionEventTypeResultSubmitted               = "ResultSubmitted"
	ExecutionEventTypeArtifactUploadCompleted       = "ArtifactUploadCompleted"
	ExecutionEventTypeArtifactUploadFailed          = "ArtifactUploadFailed"
	ExecutionEventTypeTaskForkRequested             = "TaskForkRequested"
	ExecutionEventTypeTaskForkCreated               = "TaskForkCreated"
	ExecutionEventTypeApprovalRequested             = "ApprovalRequested"
	ExecutionEventTypeApprovalApproved              = "ApprovalApproved"
	ExecutionEventTypeApprovalDeclined              = "ApprovalDeclined"
	ExecutionEventTypeApprovalExpired               = "ApprovalExpired"
	ExecutionEventTypeApprovalCancelled             = "ApprovalCancelled"
)

const (
	ExecutionEventSeverityDebug   = "debug"
	ExecutionEventSeverityInfo    = "info"
	ExecutionEventSeverityWarning = "warning"
	ExecutionEventSeverityError   = "error"
)

const (
	// ExecutionEventStreamTypeTask is the persisted per-task stream type.
	ExecutionEventStreamTypeTask = "task"
	// ExecutionEventStreamTypeSession names the public aggregated session stream read model.
	ExecutionEventStreamTypeSession = "session"
)

var validExecutionEventTypes = map[string]struct{}{
	ExecutionEventTypeTaskCreated:                   {},
	ExecutionEventTypeTaskPhaseChanged:              {},
	ExecutionEventTypeTaskJobCreated:                {},
	ExecutionEventTypeTaskStarted:                   {},
	ExecutionEventTypeTaskSucceeded:                 {},
	ExecutionEventTypeTaskFailed:                    {},
	ExecutionEventTypeTaskCancelled:                 {},
	ExecutionEventTypeWorkerStarted:                 {},
	ExecutionEventTypeWorkerCompleted:               {},
	ExecutionEventTypeWorkerFailed:                  {},
	ExecutionEventTypeModelRequestStarted:           {},
	ExecutionEventTypeModelRequestCompleted:         {},
	ExecutionEventTypeModelRequestFailed:            {},
	ExecutionEventTypeModelMessage:                  {},
	ExecutionEventTypeContextTruncated:              {},
	ExecutionEventTypeToolCallStarted:               {},
	ExecutionEventTypeToolCallCompleted:             {},
	ExecutionEventTypeToolCallFailed:                {},
	ExecutionEventTypeWorkspacePreparationStarted:   {},
	ExecutionEventTypeWorkspacePreparationCompleted: {},
	ExecutionEventTypeWorkspacePreparationFailed:    {},
	ExecutionEventTypeAgentRuntimeStarted:           {},
	ExecutionEventTypeAgentRuntimeCommandStarted:    {},
	ExecutionEventTypeAgentRuntimeCompleted:         {},
	ExecutionEventTypeAgentRuntimeFailed:            {},
	ExecutionEventTypeResultSubmitted:               {},
	ExecutionEventTypeArtifactUploadCompleted:       {},
	ExecutionEventTypeArtifactUploadFailed:          {},
	ExecutionEventTypeTaskForkRequested:             {},
	ExecutionEventTypeTaskForkCreated:               {},
	ExecutionEventTypeApprovalRequested:             {},
	ExecutionEventTypeApprovalApproved:              {},
	ExecutionEventTypeApprovalDeclined:              {},
	ExecutionEventTypeApprovalExpired:               {},
	ExecutionEventTypeApprovalCancelled:             {},
}

// ExecutionEventTypes returns the stable Wave 0 execution event taxonomy.
func ExecutionEventTypes() []string {
	return []string{
		ExecutionEventTypeTaskCreated,
		ExecutionEventTypeTaskPhaseChanged,
		ExecutionEventTypeTaskJobCreated,
		ExecutionEventTypeTaskStarted,
		ExecutionEventTypeTaskSucceeded,
		ExecutionEventTypeTaskFailed,
		ExecutionEventTypeTaskCancelled,
		ExecutionEventTypeWorkerStarted,
		ExecutionEventTypeWorkerCompleted,
		ExecutionEventTypeWorkerFailed,
		ExecutionEventTypeModelRequestStarted,
		ExecutionEventTypeModelRequestCompleted,
		ExecutionEventTypeModelRequestFailed,
		ExecutionEventTypeModelMessage,
		ExecutionEventTypeContextTruncated,
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
		ExecutionEventTypeResultSubmitted,
		ExecutionEventTypeArtifactUploadCompleted,
		ExecutionEventTypeArtifactUploadFailed,
		ExecutionEventTypeTaskForkRequested,
		ExecutionEventTypeTaskForkCreated,
		ExecutionEventTypeApprovalRequested,
		ExecutionEventTypeApprovalApproved,
		ExecutionEventTypeApprovalDeclined,
		ExecutionEventTypeApprovalExpired,
		ExecutionEventTypeApprovalCancelled,
	}
}

// IsValidExecutionEventType reports whether value is one of the Wave 0 event types.
func IsValidExecutionEventType(value string) bool {
	_, ok := validExecutionEventTypes[strings.TrimSpace(value)]
	return ok
}

// NormalizeExecutionEventType trims a known event type. Unknown values normalize to empty.
func NormalizeExecutionEventType(value string) string {
	value = strings.TrimSpace(value)
	if !IsValidExecutionEventType(value) {
		return ""
	}
	return value
}

// IsValidExecutionEventSeverity reports whether value is a known severity after normalization.
func IsValidExecutionEventSeverity(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case ExecutionEventSeverityDebug, ExecutionEventSeverityInfo, ExecutionEventSeverityWarning, ExecutionEventSeverityError:
		return true
	default:
		return false
	}
}

// NormalizeExecutionEventSeverity normalizes severity to lowercase.
// Empty or unsupported values intentionally coerce to info so event producers can be permissive
// while stores and APIs persist a stable severity value.
func NormalizeExecutionEventSeverity(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if !IsValidExecutionEventSeverity(value) {
		return ExecutionEventSeverityInfo
	}
	return value
}

// IsValidExecutionEventStreamType reports whether value is a supported persisted stream type.
// Session timelines are an aggregated read model over task streams, not a direct P1 append target.
func IsValidExecutionEventStreamType(value string) bool {
	return strings.TrimSpace(value) == ExecutionEventStreamTypeTask
}
