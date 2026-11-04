package v1

import "testing"

func TestExecutionEventTypeConstantsCoverTaxonomy(t *testing.T) {
	want := []string{
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
	got := ExecutionEventTypes()
	if len(got) != len(want) {
		t.Fatalf("executionEventTypes length = %d, want %d", len(got), len(want))
	}
	seen := map[string]bool{}
	for _, typ := range got {
		if seen[typ] {
			t.Fatalf("executionEventTypes contains duplicate %q", typ)
		}
		seen[typ] = true
		if !IsValidExecutionEventType(typ) {
			t.Fatalf("IsValidExecutionEventType(%q) = false", typ)
		}
	}
	for _, typ := range want {
		if !seen[typ] {
			t.Fatalf("executionEventTypes missing %q", typ)
		}
	}
}

func TestExecutionEventTypeValidationRejectsEmptyAndUnknown(t *testing.T) {
	for _, value := range []string{"", " ", "TaskUnknown", "taskCreated"} {
		if IsValidExecutionEventType(value) {
			t.Fatalf("IsValidExecutionEventType(%q) = true, want false", value)
		}
		if got := NormalizeExecutionEventType(value); got != "" {
			t.Fatalf("NormalizeExecutionEventType(%q) = %q, want empty", value, got)
		}
	}
	if got := NormalizeExecutionEventType("  " + ExecutionEventTypeTaskCreated + " "); got != ExecutionEventTypeTaskCreated {
		t.Fatalf("NormalizeExecutionEventType() = %q", got)
	}
}

func TestTerminalTaskEventTaxonomy(t *testing.T) {
	want := []string{
		ExecutionEventTypeTaskSucceeded,
		ExecutionEventTypeTaskFailed,
		ExecutionEventTypeTaskCancelled,
	}
	got := TerminalTaskEventTypes()
	assertSameEventSet(t, got, want)
	for _, typ := range got {
		if !IsValidExecutionEventType(typ) {
			t.Fatalf("terminal task event %q is not a valid execution event type", typ)
		}
		if !IsTerminalTaskEventType("  " + typ + " ") {
			t.Fatalf("IsTerminalTaskEventType(%q) = false, want true", typ)
		}
	}
	for _, typ := range []string{ExecutionEventTypeTaskStarted, ExecutionEventTypeApprovalApproved, ExecutionEventTypeAgentRuntimeCompleted, ""} {
		if IsTerminalTaskEventType(typ) {
			t.Fatalf("IsTerminalTaskEventType(%q) = true, want false", typ)
		}
	}
	got[0] = ExecutionEventTypeTaskStarted
	if TerminalTaskEventTypes()[0] != ExecutionEventTypeTaskSucceeded {
		t.Fatalf("TerminalTaskEventTypes returned mutable package storage")
	}
}

func TestTerminalApprovalEventTaxonomy(t *testing.T) {
	want := []string{
		ExecutionEventTypeApprovalApproved,
		ExecutionEventTypeApprovalDeclined,
		ExecutionEventTypeApprovalExpired,
		ExecutionEventTypeApprovalCancelled,
	}
	got := terminalApprovalEventTypes
	assertSameEventSet(t, got, want)
	for _, typ := range got {
		if !IsValidExecutionEventType(typ) {
			t.Fatalf("terminal approval event %q is not a valid execution event type", typ)
		}
		if !IsTerminalApprovalEventType("  " + typ + " ") {
			t.Fatalf("IsTerminalApprovalEventType(%q) = false, want true", typ)
		}
	}
	for _, typ := range []string{ExecutionEventTypeApprovalRequested, ExecutionEventTypeTaskSucceeded, ExecutionEventTypeAgentRuntimeCompleted, ""} {
		if IsTerminalApprovalEventType(typ) {
			t.Fatalf("IsTerminalApprovalEventType(%q) = true, want false", typ)
		}
	}
}

func assertSameEventSet(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("event set length = %d, want %d (got=%v want=%v)", len(got), len(want), got, want)
	}
	seen := map[string]bool{}
	for _, typ := range got {
		if seen[typ] {
			t.Fatalf("event set contains duplicate %q", typ)
		}
		seen[typ] = true
	}
	for _, typ := range want {
		if !seen[typ] {
			t.Fatalf("event set missing %q (got=%v)", typ, got)
		}
	}
}

func TestEventSeverityNormalization(t *testing.T) {
	tests := map[string]string{
		"":         ExecutionEventSeverityInfo,
		" ":        ExecutionEventSeverityInfo,
		"INFO":     ExecutionEventSeverityInfo,
		"warning":  ExecutionEventSeverityWarning,
		"Error":    ExecutionEventSeverityError,
		"debug":    ExecutionEventSeverityDebug,
		"critical": ExecutionEventSeverityInfo,
	}
	for input, want := range tests {
		if got := NormalizeExecutionEventSeverity(input); got != want {
			t.Fatalf("NormalizeExecutionEventSeverity(%q) = %q, want %q", input, got, want)
		}
	}
	for _, valid := range []string{ExecutionEventSeverityDebug, ExecutionEventSeverityInfo, ExecutionEventSeverityWarning, ExecutionEventSeverityError} {
		if !IsValidExecutionEventSeverity(valid) {
			t.Fatalf("IsValidExecutionEventSeverity(%q) = false", valid)
		}
	}
	if IsValidExecutionEventSeverity("critical") {
		t.Fatalf("IsValidExecutionEventSeverity(critical) = true, want false")
	}
}
