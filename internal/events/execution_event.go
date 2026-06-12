package events

import "strings"

const (
	ExecutionEventTypeTaskCreated             = "TaskCreated"
	ExecutionEventTypeTaskPhaseChanged        = "TaskPhaseChanged"
	ExecutionEventTypeTaskJobCreated          = "TaskJobCreated"
	ExecutionEventTypeTaskStarted             = "TaskStarted"
	ExecutionEventTypeTaskSucceeded           = "TaskSucceeded"
	ExecutionEventTypeTaskFailed              = "TaskFailed"
	ExecutionEventTypeTaskCancelled           = "TaskCancelled"
	ExecutionEventTypeWorkerStarted           = "WorkerStarted"
	ExecutionEventTypeWorkerCompleted         = "WorkerCompleted"
	ExecutionEventTypeWorkerFailed            = "WorkerFailed"
	ExecutionEventTypeModelRequestStarted     = "ModelRequestStarted"
	ExecutionEventTypeModelRequestCompleted   = "ModelRequestCompleted"
	ExecutionEventTypeModelRequestFailed      = "ModelRequestFailed"
	ExecutionEventTypeModelMessage            = "ModelMessage"
	ExecutionEventTypeContextTruncated        = "ContextTruncated"
	ExecutionEventTypeToolCallStarted         = "ToolCallStarted"
	ExecutionEventTypeToolCallCompleted       = "ToolCallCompleted"
	ExecutionEventTypeToolCallFailed          = "ToolCallFailed"
	ExecutionEventTypeResultSubmitted         = "ResultSubmitted"
	ExecutionEventTypeArtifactUploadCompleted = "ArtifactUploadCompleted"
	ExecutionEventTypeArtifactUploadFailed    = "ArtifactUploadFailed"
)

const (
	ExecutionEventSeverityDebug   = "debug"
	ExecutionEventSeverityInfo    = "info"
	ExecutionEventSeverityWarning = "warning"
	ExecutionEventSeverityError   = "error"
)

const (
	// ExecutionEventStreamTypeTask is the only supported Wave 0 stream type.
	// Session streams are intentionally not defined in P0.
	ExecutionEventStreamTypeTask = "task"
)

var executionEventTypes = []string{
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
	ExecutionEventTypeResultSubmitted,
	ExecutionEventTypeArtifactUploadCompleted,
	ExecutionEventTypeArtifactUploadFailed,
}

var validExecutionEventTypes = newExecutionEventTypeSet(executionEventTypes)

func newExecutionEventTypeSet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	return set
}

// ExecutionEventTypes returns the stable Wave 0 execution event taxonomy.
func ExecutionEventTypes() []string {
	return append([]string(nil), executionEventTypes...)
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
	return isValidNormalizedExecutionEventSeverity(strings.ToLower(strings.TrimSpace(value)))
}

func isValidNormalizedExecutionEventSeverity(value string) bool {
	switch value {
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
	if !isValidNormalizedExecutionEventSeverity(value) {
		return ExecutionEventSeverityInfo
	}
	return value
}

// IsValidExecutionEventStreamType reports whether value is supported by the Wave 0 contract.
func IsValidExecutionEventStreamType(value string) bool {
	return strings.TrimSpace(value) == ExecutionEventStreamTypeTask
}
