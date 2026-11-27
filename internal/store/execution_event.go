package store

import (
	"strings"

	"github.com/orka-agents/orka/internal/events"
)

const (
	ExecutionEventStreamTypeSession = events.ExecutionEventStreamTypeSession
)

func SanitizeExecutionEventPayloadFields(event *ExecutionEvent) error {
	if event == nil {
		return nil
	}
	payload, err := events.SanitizeExecutionEventPayload(event.Summary, event.Content, event.ContentText)
	if err != nil {
		return err
	}
	event.Summary = payload.Summary
	event.Content = payload.Content
	event.ContentText = payload.ContentText
	event.Truncation = MergeExecutionEventTruncation(event.Truncation, payload.Truncation)
	return nil
}

func ExecutionEventPayloadSanitizationSignals(event *ExecutionEvent) (redacted, truncated bool) {
	if event == nil {
		return false, false
	}
	redacted = executionEventPayloadContainsRedactionMarker(event)
	truncated = event.Truncation != nil && !event.Truncation.Empty()
	return redacted, truncated
}

func MergeExecutionEventTruncation(values ...*events.ExecutionEventTruncation) *events.ExecutionEventTruncation {
	var merged events.ExecutionEventTruncation
	for _, value := range values {
		if value == nil {
			continue
		}
		merged.SummaryTruncated = merged.SummaryTruncated || value.SummaryTruncated
		merged.SummaryOriginalChars = max(merged.SummaryOriginalChars, value.SummaryOriginalChars)
		merged.ContentTextTruncated = merged.ContentTextTruncated || value.ContentTextTruncated
		merged.ContentTextOriginalChars = max(merged.ContentTextOriginalChars, value.ContentTextOriginalChars)
		merged.ContentJSONTruncated = merged.ContentJSONTruncated || value.ContentJSONTruncated
		merged.ContentJSONOriginalBytes = max(merged.ContentJSONOriginalBytes, value.ContentJSONOriginalBytes)
	}
	if merged.Empty() {
		return nil
	}
	return &merged
}

func executionEventPayloadContainsRedactionMarker(event *ExecutionEvent) bool {
	if event == nil {
		return false
	}
	return strings.Contains(event.Summary, events.ExecutionEventRedactedValue) ||
		strings.Contains(event.ContentText, events.ExecutionEventRedactedValue) ||
		strings.Contains(string(event.Content), events.ExecutionEventRedactedValue)
}
